package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	_ "github.com/glue2flow/g2f/modules/email"
)

// The shipped modules directory loads, including the Go (runtime: go) module.
func TestLoadShippedModules(t *testing.T) {
	reg := nodes.NewRegistry()
	mgr := NewManager(reg, newMemStore(), discard)
	n, err := mgr.LoadDir(context.Background(), "../../modules")
	if err != nil {
		t.Fatal(err)
	}
	if n < 7 {
		t.Fatalf("loaded only %d modules", n)
	}
	info, ok := reg.Module("email")
	if !ok || info.Runtime != "go" || len(info.Nodes) != 4 || !mgr.IsFileModule("email") {
		t.Fatalf("email module: %+v", info)
	}
	for _, typ := range []string{"email.send", "email.read", "gmail.send", "outlook.send", "ms-teams.sendChannelMessage"} {
		if !reg.Exists(typ) {
			t.Errorf("node %s missing", typ)
		}
	}
}

func TestNativeModulesCannotBeImported(t *testing.T) {
	mgr := NewManager(nodes.NewRegistry(), newMemStore(), discard)
	_, err := mgr.Import(context.Background(), "x.yaml", []byte("module: email\nruntime: go\nnodes: [{type: send, name: Send}]\n"), "alice")
	if err == nil || !strings.Contains(err.Error(), "cannot be imported") {
		t.Fatalf("expected refusal, got %v", err)
	}
	// A go manifest without a compiled-in implementation fails to load.
	if _, err := nodes.NativeModuleNodes(&nodes.Manifest{Module: "nope", Runtime: "go"}, "test"); err == nil {
		t.Fatal("unknown native module accepted")
	}
}
