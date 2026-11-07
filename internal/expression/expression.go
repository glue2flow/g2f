// Package expression evaluates {{ ... }} templates inside node parameters.
//
// Expressions use the expr language (https://expr-lang.org). A parameter that is
// exactly one expression ("{{ json.count + 1 }}") keeps the expression's type;
// mixed text ("Hello {{ json.name }}") always yields a string.
package expression

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
)

// Env is the variable scope available to expressions.
type Env map[string]any

var cache sync.Map // string -> *vm.Program

func compile(code string) (*vm.Program, error) {
	if p, ok := cache.Load(code); ok {
		return p.(*vm.Program), nil
	}
	p, err := expr.Compile(code, expr.AllowUndefinedVariables(), expr.Patch(nilSafe{}))
	if err != nil {
		return nil, err
	}
	cache.Store(code, p)
	return p, nil
}

// nilSafe makes every member access optional, so `trigger.body.name` yields nil
// instead of an error when body is missing (like `trigger.body?.name`).
// Workflow data is loosely shaped; failing on absent fields is rarely useful.
type nilSafe struct{}

func (nilSafe) Visit(n *ast.Node) {
	switch t := (*n).(type) {
	case *ast.MemberNode:
		t.Optional = true
		ast.Patch(n, &ast.ChainNode{Node: t})
	case *ast.CallNode:
		// Method calls need a plain member callee.
		if ch, ok := t.Callee.(*ast.ChainNode); ok {
			if m, ok := ch.Node.(*ast.MemberNode); ok {
				m.Optional = false
				t.Callee = m
			}
		}
	}
}

// Eval runs a bare expression (without braces).
func Eval(code string, env Env) (any, error) {
	p, err := compile(code)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", code, err)
	}
	v, err := expr.Run(p, map[string]any(env))
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", code, err)
	}
	return v, nil
}

// Render evaluates all {{ }} blocks in s.
func Render(s string, env Env) (any, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") && strings.Count(trimmed, "{{") == 1 {
		return Eval(strings.TrimSpace(trimmed[2:len(trimmed)-2]), env)
	}

	var b strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			return nil, fmt.Errorf("unterminated expression in %q", s)
		}
		b.WriteString(rest[:start])
		v, err := Eval(strings.TrimSpace(rest[start+2:start+end]), env)
		if err != nil {
			return nil, err
		}
		b.WriteString(Stringify(v))
		rest = rest[start+end+2:]
	}
	return b.String(), nil
}

// Resolve renders expressions recursively inside strings, maps and slices.
func Resolve(v any, env Env) (any, error) {
	switch t := v.(type) {
	case string:
		return Render(t, env)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			r, err := Resolve(e, env)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := Resolve(e, env)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// Stringify converts a value to its textual form; maps and slices become JSON.
func Stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any, []any, []map[string]any:
		b, _ := json.Marshal(t)
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}

// Identifiers returns the top-level variable names used by the {{ }} blocks in
// v (strings, maps and slices are searched recursively), e.g. "json", "vars".
func Identifiers(v any) ([]string, error) {
	seen := map[string]bool{}
	var walk func(any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case string:
			for _, code := range blocks(t) {
				tree, err := parser.Parse(code)
				if err != nil {
					return fmt.Errorf("expression %q: %w", code, err)
				}
				ast.Walk(&tree.Node, identCollector(seen))
			}
		case map[string]any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(v); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out, nil
}

type identCollector map[string]bool

func (c identCollector) Visit(n *ast.Node) {
	if id, ok := (*n).(*ast.IdentifierNode); ok {
		c[id.Value] = true
	}
}

// blocks returns the code inside each {{ }} of s.
func blocks(s string) []string {
	var out []string
	for {
		start := strings.Index(s, "{{")
		if start < 0 {
			return out
		}
		end := strings.Index(s[start:], "}}")
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(s[start+2:start+end]))
		s = s[start+end+2:]
	}
}
