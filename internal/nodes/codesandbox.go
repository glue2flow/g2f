package nodes

// Code node sandbox process.
//
// goja allocates on the Go heap of the process it runs in, so a script's memory
// cannot be measured (or limited) inside the server. Each Code node run
// therefore starts the server's own binary again as a short-lived child
// (about 10 ms), which runs exactly one script:
//
//   - The child gets an empty environment: no G2F_SECRET_KEY, no database URL.
//   - Inside the child, a watchdog measures the heap (only the script lives
//     there) and interrupts the script when it exceeds the limit.
//   - The parent kills the child when its resident memory grows far beyond the
//     limit anyway (Linux), and when the timeout passes. On Linux the child
//     also dies with the parent.
//
// Any binary that imports this package can act as the sandbox: init() takes
// over when the marker variable is set, before main runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

// sandboxMarker is set in the environment of sandbox processes.
const sandboxMarker = "G2F_INTERNAL_CODE_SANDBOX"

// rssSlack is how far the child's resident memory may exceed the script limit
// (Go runtime, goja, the request data) before the parent kills it.
const rssSlack = 96 << 20

func init() {
	if os.Getenv(sandboxMarker) == "1" {
		os.Exit(sandboxMain(os.Stdin, os.Stdout))
	}
}

// sandboxMain runs one script request from in and writes the result to out.
func sandboxMain(in io.Reader, out io.Writer) int {
	var req scriptRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: bad request:", err)
		return 2
	}
	if req.MemoryBytes > 0 {
		// Make the GC work hard before the limit is reached, so the watchdog
		// only fires for memory the script really holds.
		debug.SetMemoryLimit(req.MemoryBytes)
	}
	var interrupt atomic.Pointer[func(reason string)]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchMemory(ctx, req.MemoryBytes, func() {
		if stop := interrupt.Load(); stop != nil {
			(*stop)("memory")
		}
	})
	res := runScript(ctx, &req, &interrupt)
	if err := json.NewEncoder(out).Encode(res); err != nil {
		// Typically a result that is not JSON-serializable.
		_ = json.NewEncoder(out).Encode(scriptResult{Error: "result is not JSON-serializable: " + err.Error()})
	}
	return 0
}

// watchMemory calls exceeded when the live heap stays above limit.
func watchMemory(ctx context.Context, limit int64, exceeded func()) {
	if limit <= 0 {
		return
	}
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		metrics.Read(sample)
		if int64(sample[0].Value.Uint64()) <= limit {
			continue
		}
		// Part of it may be garbage: collect and look again.
		runtime.GC()
		metrics.Read(sample)
		if int64(sample[0].Value.Uint64()) > limit {
			exceeded()
			return
		}
	}
}

// runSandboxed runs req in a child process.
func runSandboxed(ctx context.Context, req *scriptRequest) (*scriptResult, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("code sandbox: %w", err)
	}
	input, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// The child enforces the script timeout itself; this is the hard stop.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond+5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self)
	cmd.Env = []string{sandboxMarker + "=1", "GOMAXPROCS=2"}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	sandboxAttr(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("code sandbox: %w", err)
	}

	killedForMemory := make(chan struct{})
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
			}
			if rss := processRSS(cmd.Process.Pid); rss > req.MemoryBytes+rssSlack {
				close(killedForMemory)
				_ = cmd.Process.Kill()
				return
			}
		}
	}()

	err = cmd.Wait()
	stopWatch()
	select {
	case <-killedForMemory:
		return &scriptResult{Error: memoryError(req.MemoryBytes)}, nil
	default:
	}
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &scriptResult{Error: fmt.Sprintf("code timed out after %s", time.Duration(req.TimeoutMs)*time.Millisecond)}, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var res scriptResult
	if jerr := json.Unmarshal(stdout.Bytes(), &res); jerr != nil {
		// No result: the process died (e.g. the Go runtime ran out of memory).
		if bytes.Contains(stderr.Bytes(), []byte("out of memory")) {
			return &scriptResult{Error: memoryError(req.MemoryBytes)}, nil
		}
		msg := "code sandbox crashed"
		if err != nil {
			msg += ": " + err.Error()
		}
		return nil, errors.New(msg)
	}
	return &res, nil
}

// limitedWriter keeps the first n bytes.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
