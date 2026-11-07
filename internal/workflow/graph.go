package workflow

import (
	"fmt"
)

// DefaultHandle is the handle name used when an edge does not specify one.
const DefaultHandle = "main"

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Node is one box in the editor.
type Node struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Name     string         `json:"name"`
	Position Position       `json:"position"`
	Params   map[string]any `json:"params"`
	Disabled bool           `json:"disabled,omitempty"`
}

// Edge is an arrow from an output handle of Source to an input handle of Target.
type Edge struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	Target       string `json:"target"`
	TargetHandle string `json:"targetHandle,omitempty"`
}

func (e Edge) Handle() string {
	if e.SourceHandle == "" {
		return DefaultHandle
	}
	return e.SourceHandle
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

func (g *Graph) Node(id string) *Node {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// Validate checks structural integrity: unique ids and names, edges pointing at
// existing nodes, and no cycles. isTrigger reports whether a node type is a trigger.
func (g *Graph) Validate(typeExists func(string) bool, isTrigger func(string) bool) error {
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.ID == "" {
			return fmt.Errorf("node without id")
		}
		if ids[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		ids[n.ID] = true
		if n.Name == "" {
			n.Name = n.ID
		}
		if names[n.Name] {
			return fmt.Errorf("duplicate node name %q", n.Name)
		}
		names[n.Name] = true
		if !typeExists(n.Type) {
			return fmt.Errorf("node %q has unknown type %q", n.Name, n.Type)
		}
		if n.Params == nil {
			n.Params = map[string]any{}
		}
	}
	for _, e := range g.Edges {
		if !ids[e.Source] || !ids[e.Target] {
			return fmt.Errorf("edge %q references unknown node", e.ID)
		}
		if e.Source == e.Target {
			return fmt.Errorf("edge %q connects a node to itself", e.ID)
		}
		if t := g.Node(e.Target); isTrigger(t.Type) {
			return fmt.Errorf("trigger node %q cannot have incoming connections", t.Name)
		}
	}
	if _, err := g.TopoOrder(nil); err != nil {
		return err
	}
	return nil
}

// TopoOrder returns node ids in topological order. If only is non-nil, the
// order is restricted to those node ids.
func (g *Graph) TopoOrder(only map[string]bool) ([]string, error) {
	indeg := map[string]int{}
	out := map[string][]string{}
	for _, n := range g.Nodes {
		if only == nil || only[n.ID] {
			indeg[n.ID] = 0
		}
	}
	for _, e := range g.Edges {
		_, s := indeg[e.Source]
		_, t := indeg[e.Target]
		if s && t {
			out[e.Source] = append(out[e.Source], e.Target)
			indeg[e.Target]++
		}
	}
	// Seed in graph order for deterministic output.
	var queue []string
	for _, n := range g.Nodes {
		if d, ok := indeg[n.ID]; ok && d == 0 {
			queue = append(queue, n.ID)
		}
	}
	order := make([]string, 0, len(indeg))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, t := range out[id] {
			indeg[t]--
			if indeg[t] == 0 {
				queue = append(queue, t)
			}
		}
	}
	if len(order) != len(indeg) {
		return nil, fmt.Errorf("workflow graph contains a cycle")
	}
	return order, nil
}

// Reachable returns all node ids reachable from start (inclusive).
func (g *Graph) Reachable(start string) map[string]bool {
	seen := map[string]bool{start: true}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range g.Edges {
			if e.Source == id && !seen[e.Target] {
				seen[e.Target] = true
				stack = append(stack, e.Target)
			}
		}
	}
	return seen
}
