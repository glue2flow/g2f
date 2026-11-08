package nodes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

func runCode(t *testing.T, code string, timeout float64) (Output, error, []string) {
	t.Helper()
	var logs []string
	def := codeNode{}.Definition()
	ec := &ExecContext{
		Node:  &workflow.Node{ID: "c", Name: "Code", Params: map[string]any{"code": code, "timeoutSeconds": timeout}},
		Def:   &def,
		Items: []Item{{"n": 1}},
		Env:   expression.Env{"workflow": map[string]any{"name": "wf"}},
		LogSink: func(level, msg string) {
			logs = append(logs, level+":"+msg)
		},
	}
	out, err := codeNode{}.Execute(context.Background(), ec)
	return out, err, logs
}

func withMemoryLimit(t *testing.T, mb int64) {
	old := CodeMemoryLimit
	CodeMemoryLimit = mb << 20
	t.Cleanup(func() { CodeMemoryLimit = old })
}

func TestCodeSandboxRunsScript(t *testing.T) {
	withMemoryLimit(t, 64)
	out, err, logs := runCode(t, "console.log('hi', workflow.name); return items.map(i => ({n: i.n + 1}))", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["main"][0]["n"]; got != float64(2) {
		t.Fatalf("output %v", out)
	}
	if len(logs) != 1 || logs[0] != "info:hi wf" {
		t.Fatalf("logs %v", logs)
	}
}

func TestCodeSandboxMemoryLimit(t *testing.T) {
	withMemoryLimit(t, 32)
	start := time.Now()
	_, err, _ := runCode(t, "const a = []; while (true) a.push('x'.repeat(1 << 16) + a.length);", 20)
	if err == nil || !strings.Contains(err.Error(), "memory limit of 32 MB") {
		t.Fatalf("expected memory error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("memory limit took %s", time.Since(start))
	}
}

func TestCodeSandboxTimeout(t *testing.T) {
	withMemoryLimit(t, 32)
	_, err, _ := runCode(t, "while (true) {}", 0.5)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestCodeSandboxScriptError(t *testing.T) {
	withMemoryLimit(t, 32)
	_, err, _ := runCode(t, "throw new Error('boom')", 5)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected script error, got %v", err)
	}
}

func TestCodeInProcessWithoutLimit(t *testing.T) {
	withMemoryLimit(t, 0)
	out, err, _ := runCode(t, "return {ok: true}", 5)
	if err != nil || out["main"][0]["ok"] != true {
		t.Fatalf("%v %v", out, err)
	}
}
