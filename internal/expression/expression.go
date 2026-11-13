// Package expression evaluates {{ ... }} templates inside node parameters.
//
// Expressions use the expr language (https://expr-lang.org). A parameter that is
// exactly one expression ("{{ json.count + 1 }}") keeps the expression's type;
// mixed text ("Hello {{ json.name }}") always yields a string.
package expression

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
)

// Env is the variable scope available to expressions.
type Env map[string]any

// MaxSteps is the number of predicate iterations (all, filter, map, ...) one
// expression evaluation may run before it is stopped (G2F_EXPRESSION_MAX_STEPS).
var MaxSteps int64 = 2_000_000

// maxCacheEntries bounds the compiled-program cache.
const maxCacheEntries = 10_000

var (
	cacheMu sync.RWMutex
	cache   = map[string]*vm.Program{}
)

func compile(code string) (*vm.Program, error) {
	cacheMu.RLock()
	p, ok := cache[code]
	cacheMu.RUnlock()
	if ok {
		return p, nil
	}
	if err := checkReserved(code); err != nil {
		return nil, err
	}
	opts := append([]expr.Option{expr.AllowUndefinedVariables(), expr.Patch(nilSafe{}), expr.Patch(ticker{})}, functions...)
	opts = append(opts, tickFunction, catFunction)
	p, err := expr.Compile(code, opts...)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	if len(cache) >= maxCacheEntries {
		clear(cache) // simple bound: start over instead of tracking recency
	}
	cache[code] = p
	cacheMu.Unlock()
	return p, nil
}

// checkReserved rejects user code that uses names starting with "__", which are
// reserved for the step budget and the cancellation context.
func checkReserved(code string) error {
	tree, err := parser.Parse(code)
	if err != nil {
		return nil // reported by the compiler with better context
	}
	var bad string
	ast.Walk(&tree.Node, reservedFinder{&bad})
	if bad != "" {
		return fmt.Errorf("names starting with \"__\" are reserved (%s)", bad)
	}
	return nil
}

type reservedFinder struct{ bad *string }

func (f reservedFinder) Visit(n *ast.Node) {
	switch t := (*n).(type) {
	case *ast.IdentifierNode:
		if strings.HasPrefix(t.Value, "__") && *f.bad == "" {
			*f.bad = t.Value
		}
	case *ast.VariableDeclaratorNode:
		if strings.HasPrefix(t.Name, "__") && *f.bad == "" {
			*f.bad = t.Name
		}
	}
}

// Hidden env keys. Eval puts the budget of the current evaluation under
// budgetKey; WithContext stores the run's context under ctxKey.
const (
	budgetKey = "__budget"
	ctxKey    = "__ctx"
	tickName  = "__tick"
	catName   = "__cat"

	// maxStringLen caps strings built with "+", which would otherwise double
	// per let/reduce step (2^40 bytes) and kill the process.
	maxStringLen = 32 << 20
)

// budget limits one evaluation: the number of predicate iterations and, every
// 1024 iterations, whether the run's context is still alive.
type budget struct {
	steps int64
	max   int64
	n     uint32
	ctx   context.Context
}

// MarshalJSON hides the budget if an expression returns the whole env ($env).
func (*budget) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func (b *budget) tick() error {
	b.steps--
	if b.steps < 0 {
		return fmt.Errorf("expression is too expensive: more than %d iterations", b.max)
	}
	b.n++
	if b.n&1023 == 0 && b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return fmt.Errorf("expression cancelled: %w", err)
		}
	}
	return nil
}

// ctxHolder carries the run's context inside an Env.
type ctxHolder struct{ ctx context.Context }

// MarshalJSON hides the context if an expression returns the whole env ($env).
func (*ctxHolder) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// WithContext returns a copy of env whose expression evaluations stop when ctx
// is done. Scopes derived from the copy (e.g. per-item scopes) keep it.
func WithContext(env Env, ctx context.Context) Env {
	if ctx == nil {
		return env
	}
	out := make(Env, len(env)+1)
	maps.Copy(out, env)
	out[ctxKey] = &ctxHolder{ctx}
	return out
}

// tickFunction is __tick(budget, body): spends one step and returns body.
var tickFunction = expr.Function(tickName, func(params ...any) (any, error) {
	b, ok := params[0].(*budget)
	if !ok {
		return nil, fmt.Errorf("names starting with \"__\" are reserved")
	}
	if err := b.tick(); err != nil {
		return nil, err
	}
	return params[1], nil
}, new(func(any, any) any))

// catFunction is __cat(budget, a + b): it returns the sum, rejects huge
// strings and charges one step per KiB, so repeated appends stay metered.
var catFunction = expr.Function(catName, func(params ...any) (any, error) {
	b, ok := params[0].(*budget)
	if !ok {
		return nil, fmt.Errorf("names starting with \"__\" are reserved")
	}
	if s, ok := params[1].(string); ok {
		if len(s) > maxStringLen {
			return nil, fmt.Errorf("string is too long: more than %d bytes", maxStringLen)
		}
		b.steps -= int64(len(s) >> 10)
		if b.steps < 0 {
			return nil, fmt.Errorf("expression is too expensive: more than %d iterations", b.max)
		}
	}
	return params[1], nil
}, new(func(any, any) any))

// ticker wraps the body of every predicate (all, filter, map, ...) into
// __tick(__budget, body), so each iteration spends one step, and routes
// non-numeric "+" through __cat. It runs on the already parsed tree, after user
// code was checked for reserved names.
type ticker struct{}

func (ticker) Visit(n *ast.Node) {
	switch t := (*n).(type) {
	case *ast.PredicateNode:
		t.Node = guard(tickName, t.Node)
	case *ast.BinaryNode:
		if t.Operator == "+" && !isNumber(t.Left) && !isNumber(t.Right) {
			ast.Patch(n, guard(catName, t))
		}
	}
}

// guard returns fn(__budget, body).
func guard(fn string, body ast.Node) ast.Node {
	call := &ast.CallNode{
		Callee:    &ast.IdentifierNode{Value: fn},
		Arguments: []ast.Node{&ast.IdentifierNode{Value: budgetKey}, body},
	}
	call.SetLocation(body.Location())
	return call
}

// isNumber reports whether n is statically a number ("+" then cannot build a string).
func isNumber(n ast.Node) bool {
	t := n.Type()
	if t == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
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
	// The env may be shared between goroutines: evaluate in a copy that holds
	// this evaluation's budget.
	scope := make(map[string]any, len(env)+1)
	maps.Copy(scope, env)
	b := &budget{steps: MaxSteps, max: MaxSteps}
	if h, ok := env[ctxKey].(*ctxHolder); ok {
		b.ctx = h.ctx
		if err := b.ctx.Err(); err != nil {
			return nil, fmt.Errorf("expression %q: expression cancelled: %w", code, err)
		}
	}
	scope[budgetKey] = b
	v, err := expr.Run(p, scope)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", code, err)
	}
	return Normalize(v), nil
}

// Normalize converts values produced by expr into JSON-compatible types:
// some builtins (e.g. fromPairs, groupBy) return maps with interface keys,
// which encoding/json cannot store.
func Normalize(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = Normalize(e)
		}
		return out
	case map[string]any:
		for k, e := range t {
			t[k] = Normalize(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = Normalize(e)
		}
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = Normalize(e)
		}
		return out
	}
	return v
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
