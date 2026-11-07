// Package nodes contains the node registry, the built-in nodes and the loader
// for declarative (YAML) API modules.
package nodes

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"sync"

	"github.com/wolfposd/g8g/internal/expression"
	"github.com/wolfposd/g8g/internal/workflow"
)

// Item is one unit of data flowing between nodes (a JSON object).
type Item = map[string]any

// Output maps an output handle (e.g. "main", "true", "false") to its items.
type Output map[string][]Item

// TriggerKind identifies how a trigger node starts a workflow.
type TriggerKind string

const (
	TriggerManual   TriggerKind = "manual"
	TriggerSchedule TriggerKind = "schedule"
	TriggerWebhook  TriggerKind = "webhook"
)

type ParamType string

const (
	ParamString  ParamType = "string"
	ParamText    ParamType = "text"
	ParamNumber  ParamType = "number"
	ParamBoolean ParamType = "boolean"
	ParamSelect  ParamType = "select"
	ParamJSON    ParamType = "json"
	ParamSecret  ParamType = "secret"
)

type Option struct {
	Label string `json:"label" yaml:"label"`
	Value string `json:"value" yaml:"value"`
}

// Param describes one configurable field; the editor renders forms from these.
type Param struct {
	Name        string    `json:"name" yaml:"name"`
	Label       string    `json:"label" yaml:"label"`
	Type        ParamType `json:"type" yaml:"type"`
	Description string    `json:"description,omitempty" yaml:"description"`
	Placeholder string    `json:"placeholder,omitempty" yaml:"placeholder"`
	Default     any       `json:"default,omitempty" yaml:"default"`
	Required    bool      `json:"required,omitempty" yaml:"required"`
	Options     []Option  `json:"options,omitempty" yaml:"options"`
	// NoExpression disables {{ }} evaluation (used for trigger settings).
	NoExpression bool `json:"noExpression,omitempty" yaml:"noExpression"`
}

// Definition is the public description of a node type served to the editor.
type Definition struct {
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	Category    string      `json:"category"`
	Description string      `json:"description"`
	Color       string      `json:"color,omitempty"`
	Trigger     TriggerKind `json:"trigger,omitempty"`
	Inputs      []string    `json:"inputs"`
	Outputs     []string    `json:"outputs"`
	Params      []Param     `json:"params"`
	Source      string      `json:"source"` // builtin | module:<file>
}

func (d *Definition) Param(name string) *Param {
	for i := range d.Params {
		if d.Params[i].Name == name {
			return &d.Params[i]
		}
	}
	return nil
}

// Node is implemented by every node type.
type Node interface {
	Definition() Definition
	Execute(ctx context.Context, ec *ExecContext) (Output, error)
}

// ExecContext is what a node sees while it runs.
type ExecContext struct {
	Node   *workflow.Node
	Def    *Definition
	Items  []Item
	Env    expression.Env // shared scope: nodes, trigger, execution, workflow
	Logger *slog.Logger
}

// ItemEnv builds the expression scope for a single item.
func (c *ExecContext) ItemEnv(item Item, index int) expression.Env {
	env := make(expression.Env, len(c.Env)+3)
	maps.Copy(env, c.Env)
	env["json"] = item
	env["index"] = index
	env["items"] = c.Items
	return env
}

// Raw returns the unevaluated parameter value, falling back to the default.
func (c *ExecContext) Raw(name string) any {
	if v, ok := c.Node.Params[name]; ok && v != nil && v != "" {
		return v
	}
	if p := c.Def.Param(name); p != nil {
		return p.Default
	}
	return nil
}

// Param evaluates a parameter for the given item.
func (c *ExecContext) Param(name string, item Item, index int) (any, error) {
	v := c.Raw(name)
	if p := c.Def.Param(name); p != nil && p.NoExpression {
		return v, nil
	}
	r, err := expression.Resolve(v, c.ItemEnv(item, index))
	if err != nil {
		return nil, fmt.Errorf("parameter %q: %w", name, err)
	}
	return r, nil
}

func (c *ExecContext) String(name string, item Item, index int) (string, error) {
	v, err := c.Param(name, item, index)
	if err != nil {
		return "", err
	}
	return expression.Stringify(v), nil
}

func (c *ExecContext) Bool(name string, item Item, index int) (bool, error) {
	v, err := c.Param(name, item, index)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func (c *ExecContext) Number(name string, item Item, index int) (float64, error) {
	v, err := c.Param(name, item, index)
	if err != nil {
		return 0, err
	}
	switch t := v.(type) {
	case nil:
		return 0, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case string:
		if t == "" {
			return 0, nil
		}
		return strconv.ParseFloat(t, 64)
	}
	return 0, fmt.Errorf("parameter %q: expected a number, got %T", name, v)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "false" && t != "0"
	case int:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Registry holds all known node types.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]Node
}

func NewRegistry() *Registry { return &Registry{nodes: map[string]Node{}} }

func (r *Registry) Register(n Node) error {
	d := n.Definition()
	if d.Type == "" {
		return fmt.Errorf("node definition without type")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.nodes[d.Type]; dup {
		return fmt.Errorf("node type %q registered twice", d.Type)
	}
	r.nodes[d.Type] = n
	return nil
}

func (r *Registry) Get(typ string) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[typ]
	return n, ok
}

func (r *Registry) Exists(typ string) bool { _, ok := r.Get(typ); return ok }

// TriggerKind returns the trigger kind of a node type, or "" if it is not a trigger.
func (r *Registry) TriggerKind(typ string) TriggerKind {
	if n, ok := r.Get(typ); ok {
		return n.Definition().Trigger
	}
	return ""
}

func (r *Registry) IsTrigger(typ string) bool { return r.TriggerKind(typ) != "" }

func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]Definition, 0, len(r.nodes))
	for _, n := range r.nodes {
		defs = append(defs, n.Definition())
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].Category != defs[j].Category {
			return categoryRank(defs[i].Category) < categoryRank(defs[j].Category)
		}
		return defs[i].Name < defs[j].Name
	})
	return defs
}

func categoryRank(c string) string {
	order := []string{"Triggers", "Core"}
	if i := slices.Index(order, c); i >= 0 {
		return strconv.Itoa(i)
	}
	return "9" + c
}

// Validator is optionally implemented by nodes that check their parameters
// when a workflow is saved.
type Validator interface {
	Validate(n *workflow.Node) error
}
