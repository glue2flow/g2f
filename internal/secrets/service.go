package secrets

import (
	"context"
	"fmt"
	"strconv"

	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

// Store is the persistence the service needs.
type Store interface {
	NodeSecrets(ctx context.Context, workflowID string) (map[string]map[string]string, error)
	PluginConfig(ctx context.Context, org, module string) (map[string]string, error)
	SetPluginConfig(ctx context.Context, org, module, by string, set map[string]string, del []string) error
}

// Service manages per-node secrets and per-organization plugin configuration.
//
// Per-node secrets are parameters of type "secret". They never stay in the
// workflow graph: on save they are moved into node_secrets (encrypted), the
// API returns nodes.SecretRef in their place, and the engine decrypts them
// right before a node runs.
type Service struct {
	box   *Box
	store Store
	reg   *nodes.Registry
}

func NewService(box *Box, st Store, reg *nodes.Registry) *Service {
	return &Service{box: box, store: st, reg: reg}
}

// ExtractNodeSecrets removes secret parameters from the graph of w and returns
// the changes to persist. A nodes.SecretRef value keeps the stored secret, a
// non-empty string replaces it and an empty value deletes it.
func (s *Service) ExtractNodeSecrets(w *workflow.Workflow) ([]store.SecretOp, error) {
	var ops []store.SecretOp
	for i := range w.Graph.Nodes {
		n := &w.Graph.Nodes[i]
		impl, ok := s.reg.Get(n.Type)
		if !ok {
			continue
		}
		def := impl.Definition()
		for _, name := range def.SecretParams() {
			v, present := n.Params[name]
			delete(n.Params, name)
			switch {
			case nodes.IsSecretRef(v):
				// keep stored value
			case present && v != nil && v != "":
				str, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("node %q: secret %q must be a string", n.Name, name)
				}
				sealed, err := s.box.Seal(str, NodeAAD(w.ID, n.ID, name))
				if err != nil {
					return nil, err
				}
				ops = append(ops, store.SecretOp{NodeID: n.ID, Param: name, Sealed: sealed})
			default:
				ops = append(ops, store.SecretOp{NodeID: n.ID, Param: name, Delete: true})
			}
		}
	}
	return ops, nil
}

// MarkStored sets nodes.SecretRef for every stored secret so the editor can show
// that a value exists without revealing it.
func (s *Service) MarkStored(ctx context.Context, w *workflow.Workflow) error {
	stored, err := s.store.NodeSecrets(ctx, w.ID)
	if err != nil {
		return err
	}
	for i := range w.Graph.Nodes {
		n := &w.Graph.Nodes[i]
		for param := range stored[n.ID] {
			if n.Params == nil {
				n.Params = map[string]any{}
			}
			n.Params[param] = nodes.SecretRef
		}
	}
	return nil
}

// NodeSecrets returns decrypted secrets: node id -> param -> value.
func (s *Service) NodeSecrets(ctx context.Context, workflowID string) (map[string]map[string]string, error) {
	sealed, err := s.store.NodeSecrets(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(sealed))
	for node, params := range sealed {
		out[node] = make(map[string]string, len(params))
		for param, v := range params {
			pt, err := s.box.Open(v, NodeAAD(workflowID, node, param))
			if err != nil {
				return nil, fmt.Errorf("secret %s of node %s: %w", param, node, err)
			}
			out[node][param] = pt
		}
	}
	return out, nil
}

// ModuleSettings returns the decrypted settings of a module for an
// organization, split into plugin-only config and editor-visible vars, with
// defaults applied and numbers/booleans converted.
func (s *Service) ModuleSettings(ctx context.Context, org, module string) (config, vars map[string]any, err error) {
	config, vars = map[string]any{}, map[string]any{}
	info, ok := s.reg.Module(module)
	if !ok || len(info.Config)+len(info.Vars) == 0 {
		return config, vars, nil
	}
	sealed, err := s.store.PluginConfig(ctx, org, module)
	if err != nil {
		return nil, nil, err
	}
	fill := func(params []nodes.Param, into map[string]any) error {
		for _, p := range params {
			v, ok := sealed[p.Name]
			if !ok {
				if p.Default != nil {
					into[p.Name] = p.Default
				}
				continue
			}
			pt, err := s.box.Open(v, ConfigAAD(org, module, p.Name))
			if err != nil {
				return fmt.Errorf("setting %s.%s: %w", module, p.Name, err)
			}
			into[p.Name] = typed(p.Type, pt)
		}
		return nil
	}
	if err := fill(info.Config, config); err != nil {
		return nil, nil, err
	}
	if err := fill(info.Vars, vars); err != nil {
		return nil, nil, err
	}
	return config, vars, nil
}

func typed(t nodes.ParamType, s string) any {
	switch t {
	case nodes.ParamNumber:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	case nodes.ParamBoolean:
		return s == "true"
	}
	return s
}

// Setting kinds.
const (
	KindConfig = "config" // plugin-only
	KindVar    = "var"    // usable in node fields as vars.<name>
)

// ConfigValue is how the API presents one setting. Secret values are never returned.
type ConfigValue struct {
	nodes.Param
	Kind  string `json:"kind"` // config | var
	Set   bool   `json:"set"`
	Value any    `json:"value,omitempty"`
}

type kindedParam struct {
	nodes.Param
	kind string
}

func settingsOf(info *nodes.ModuleInfo) []kindedParam {
	out := make([]kindedParam, 0, len(info.Config)+len(info.Vars))
	for _, p := range info.Config {
		out = append(out, kindedParam{p, KindConfig})
	}
	for _, p := range info.Vars {
		out = append(out, kindedParam{p, KindVar})
	}
	return out
}

// ConfigStatus lists the module's settings (config and vars) with current
// non-secret values.
func (s *Service) ConfigStatus(ctx context.Context, org, module string) ([]ConfigValue, error) {
	info, ok := s.reg.Module(module)
	if !ok {
		return nil, fmt.Errorf("unknown module %q", module)
	}
	sealed, err := s.store.PluginConfig(ctx, org, module)
	if err != nil {
		return nil, err
	}
	all := settingsOf(info)
	out := make([]ConfigValue, 0, len(all))
	for _, kp := range all {
		p := kp.Param
		cv := ConfigValue{Param: p, Kind: kp.kind}
		if v, ok := sealed[p.Name]; ok {
			cv.Set = true
			if p.Type != nodes.ParamSecret {
				if pt, err := s.box.Open(v, ConfigAAD(org, module, p.Name)); err == nil {
					cv.Value = typed(p.Type, pt)
				}
			}
		}
		out = append(out, cv)
	}
	return out, nil
}

// UpdateConfig applies changes to config and vars: a value of nil or "" deletes the key,
// nodes.SecretRef keeps it, anything else is stored encrypted.
func (s *Service) UpdateConfig(ctx context.Context, org, module, by string, values map[string]any) error {
	info, ok := s.reg.Module(module)
	if !ok {
		return fmt.Errorf("unknown module %q", module)
	}
	known := map[string]nodes.Param{}
	for _, kp := range settingsOf(info) {
		known[kp.Name] = kp.Param
	}
	set := map[string]string{}
	var del []string
	for k, v := range values {
		if _, ok := known[k]; !ok {
			return fmt.Errorf("unknown config key %q", k)
		}
		switch {
		case nodes.IsSecretRef(v):
		case v == nil || v == "":
			del = append(del, k)
		default:
			sealed, err := s.box.Seal(fmt.Sprint(v), ConfigAAD(org, module, k))
			if err != nil {
				return err
			}
			set[k] = sealed
		}
	}
	return s.store.SetPluginConfig(ctx, org, module, by, set, del)
}
