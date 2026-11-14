package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

const mistralDir = "../../examples/plugins/mistral"

type mistralSecrets struct{ fakeSecrets }

func (m mistralSecrets) ModuleSettings(_ context.Context, _, module string) (map[string]any, map[string]any, error) {
	if module == "mistral" {
		return m.config, m.vars, nil
	}
	return map[string]any{}, map[string]any{}, nil
}

// fakeCredentials resolves credential ids to field values (any org, module).
type fakeCredentials map[string]map[string]any

func (f fakeCredentials) Credential(_ context.Context, _, _, id string) (map[string]any, []string, error) {
	c, ok := f[id]
	if !ok {
		return nil, nil, errors.New("credential not found")
	}
	var secrets []string
	for _, v := range c {
		secrets = append(secrets, v.(string))
	}
	return c, secrets, nil
}

func mistralZip(t *testing.T, host string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a wasm plugin")
	}
	out := filepath.Join(t.TempDir(), "mistral.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = filepath.Join(mistralDir, "go")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	wasm, _ := os.ReadFile(out)
	manifest, err := os.ReadFile(filepath.Join(mistralDir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.Replace(manifest, []byte("  - api.mistral.ai"), []byte("  - "+host), 1)
	icon, err := os.ReadFile(filepath.Join(mistralDir, "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{"mistral/plugin.yaml": manifest, "mistral/plugin.wasm": wasm, "mistral/icon.svg": icon} {
		w, _ := zw.Create(name)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestMistralWasmPlugin(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		_ = json.NewDecoder(r.Body).Decode(&last)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		msgs := last["messages"].([]any)
		prompt := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		if prompt == "bad" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   last["model"],
			"choices": []any{map[string]any{"message": map[string]any{"content": "echo: " + prompt}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	}))
	defer srv.Close()

	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(reg, newMemStore(), discard)
	info, err := mgr.Import(context.Background(), "mistral.zip", mistralZip(t, "127.0.0.1"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Icon, "data:image/svg+xml;base64,") {
		t.Fatalf("mistral icon: %q", info.Icon)
	}

	run := func(sec mistralSecrets, params map[string]any) *engine.Result {
		eng := engine.New(reg, newMemStore(), discard, engine.Options{Secrets: sec, Credentials: fakeCredentials{"cred-1": {"apiKey": "own"}}})
		wf := &workflow.Workflow{ID: "wf", Name: "Ask", OrgID: "acme", Graph: workflow.Graph{
			Nodes: []workflow.Node{
				{ID: "t", Name: "Start", Type: "trigger.manual", Params: map[string]any{"payload": map[string]any{"text": "hello"}}},
				{ID: "m", Name: "Mistral", Type: "mistral.chat", Params: params},
			},
			Edges: []workflow.Edge{{ID: "e", Source: "t", Target: "m"}},
		}}
		res, err := eng.Run(context.Background(), engine.Request{Workflow: wf, TriggerType: nodes.TriggerManual})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	cfg := map[string]any{"apiKey": "org-key", "apiBaseUrl": srv.URL}

	res := run(mistralSecrets{fakeSecrets{config: cfg}}, map[string]any{
		"model": "mistral-small-latest", "systemPrompt": "Be brief", "prompt": "{{ json.text }}",
		"temperature": "0.2", "maxTokens": "50",
	})
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("run failed: %s", res.Execution.Error)
	}
	out := res.LastOutput[0]
	if out["text"] != "echo: hello" || out["finishReason"] != "stop" {
		t.Fatalf("unexpected output %v", out)
	}
	if last["temperature"] != 0.2 || last["max_tokens"] != float64(50) || len(last["messages"].([]any)) != 2 {
		t.Fatalf("unexpected request %v", last)
	}

	// The node's credential overrides the org default.
	if def := info.Credentials; def == nil || !def.Optional || def.APIHosts[0] != "127.0.0.1" {
		t.Fatalf("credentials spec: %+v", def)
	}
	res = run(mistralSecrets{fakeSecrets{config: cfg}},
		map[string]any{"model": "m", "prompt": "x", nodes.ConnectionParam: "cred-1"})
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("run failed: %s", res.Execution.Error)
	}
	if got := strings.Join(auths, ","); got != "Bearer org-key,Bearer own" {
		t.Fatalf("auth headers: %s", got)
	}

	// API errors don't leak the key.
	res = run(mistralSecrets{fakeSecrets{config: cfg}}, map[string]any{"model": "m", "prompt": "bad"})
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "Unauthorized") ||
		strings.Contains(res.Execution.Error, "org-key") {
		t.Fatalf("unexpected error %q", res.Execution.Error)
	}

	// Missing key.
	res = run(mistralSecrets{fakeSecrets{config: map[string]any{"apiBaseUrl": srv.URL}}}, map[string]any{"model": "m", "prompt": "x"})
	if !strings.Contains(res.Execution.Error, "no API key") {
		t.Fatalf("expected missing key error, got %q", res.Execution.Error)
	}
}
