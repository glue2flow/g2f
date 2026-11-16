package nodes

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ExecFunc implements one node of a native (Go) module.
type ExecFunc func(ctx context.Context, ec *ExecContext) (Output, error)

var (
	nativeMu      sync.RWMutex
	nativeModules = map[string]map[string]ExecFunc{}
)

// RegisterNative makes a module implemented in Go available. The module's
// manifest (runtime: go) declares its nodes, parameters and config like any
// other plugin; impls maps each node type (without module prefix) to its code.
// Modules call this from init(); cmd/g2f imports them. The module is only
// installed when its manifest is found in the modules directory, so admins
// decide which native modules are active.
func RegisterNative(module string, impls map[string]ExecFunc) {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if _, dup := nativeModules[module]; dup {
		panic("native module " + module + " registered twice")
	}
	nativeModules[module] = impls
}

// NativeModules lists the compiled-in native modules.
func NativeModules() []string {
	nativeMu.RLock()
	defer nativeMu.RUnlock()
	out := make([]string, 0, len(nativeModules))
	for m := range nativeModules {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// NativeModuleNodes builds the nodes of a "go" runtime manifest from the
// implementation registered with RegisterNative.
func NativeModuleNodes(m *Manifest, source string) ([]Node, error) {
	if m.Runtime != "go" {
		return nil, fmt.Errorf("module %s is not a go module", m.Module)
	}
	nativeMu.RLock()
	impls, ok := nativeModules[m.Module]
	nativeMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("module %s has runtime go, but no Go implementation is compiled into this server", m.Module)
	}
	out := make([]Node, 0, len(m.Nodes))
	declared := map[string]bool{}
	for _, n := range m.Nodes {
		fn, ok := impls[n.Type]
		if !ok {
			return nil, fmt.Errorf("node %s: no Go implementation", n.Type)
		}
		declared[n.Type] = true
		if n.Push != nil && n.Push.Endpoint == PushListen {
			if _, ok := Listener(m.Module + "." + n.Type); !ok {
				return nil, fmt.Errorf("node %s: push endpoint listen, but no listener is registered", n.Type)
			}
		}
		out = append(out, &nativeNode{def: m.NodeDefinition(n, source), fn: fn})
	}
	for t := range impls {
		if !declared[t] {
			return nil, fmt.Errorf("Go implementation of node %s is missing in the manifest", t)
		}
	}
	return out, nil
}

type nativeNode struct {
	def Definition
	fn  ExecFunc
}

func (n *nativeNode) Definition() Definition { return n.def }

// Execute runs the node. For polling triggers fn is the check: it runs when
// the engine polls (or in a manual test run) and returns the new items;
// TriggerType tells which ("poll" or "manual", see ExecContext.IsTestRun).
func (n *nativeNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	if n.def.Trigger == TriggerPoll {
		return pollTrigger(ctx, ec, func(ctx context.Context, ec *ExecContext) ([]Item, error) {
			out, err := n.fn(ctx, ec)
			if err != nil {
				return nil, err
			}
			return out["main"], nil
		})
	}
	return n.fn(ctx, ec)
}
