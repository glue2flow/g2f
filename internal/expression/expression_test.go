package expression

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	env := Env{"json": map[string]any{"name": "Ada", "n": 41, "tags": []any{"a", "b"}}}
	cases := []struct {
		in   string
		want any
	}{
		{"plain", "plain"},
		{"{{ json.n + 1 }}", 42},
		{"  {{ json.name }}  ", "Ada"},
		{"Hello {{ json.name }}!", "Hello Ada!"},
		{"{{ json.tags }}", []any{"a", "b"}},
		{"tags={{ json.tags }}", `tags=["a","b"]`},
		{"{{ json.missing ?? 'x' }}", "x"},
		{"{{ json.missing.deep.field }}", nil},
		{"{{ json.missing.deep ?? 'fallback' }}", "fallback"},
		{"{{ upper(json.name) }}", "ADA"},
		{"{{ filter(json.tags, # == 'b') }}", []any{"b"}},
	}
	for _, c := range cases {
		got, err := Render(c.in, env)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %#v want %#v", c.in, got, c.want)
		}
	}
}

func TestResolveNested(t *testing.T) {
	got, err := Resolve(map[string]any{"a": []any{"{{ 1 + 1 }}", "x"}}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": []any{2, "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestIdentifiers(t *testing.T) {
	got, err := Identifiers(map[string]any{
		"a": "Hi {{ json.name }} {{ vars.footer ?? '' }}",
		"b": []any{"{{ map(items, .x) }}", "plain"},
		"c": "{{ json.config.token }}", // a field named config, not the variable
	})
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	if !set["json"] || !set["vars"] || !set["items"] || set["config"] {
		t.Fatalf("unexpected identifiers %v", got)
	}
	if got, _ := Identifiers("{{ config.botToken }}"); len(got) != 1 || got[0] != "config" {
		t.Fatalf("config not detected: %v", got)
	}
}

func TestNormalizeInterfaceMaps(t *testing.T) {
	v, err := Render("{{ fromPairs([['a', 1], ['b', [fromPairs([['c', 2]])]]]) }}", Env{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatalf("not JSON-serializable: %v (%T)", err, v)
	}
	if v.(map[string]any)["b"].([]any)[0].(map[string]any)["c"] != 2 {
		t.Fatalf("got %#v", v)
	}
}

func TestMailFunctions(t *testing.T) {
	v, err := Render("{{ fromBase64Url(toBase64Url('hä?>')) }}", Env{})
	if err != nil || v != "hä?>" {
		t.Fatalf("base64url roundtrip: %v %v", v, err)
	}
	v, err = Render(`{{ map(emailAddresses('"Doe, Jane" <j@x.com>, b@x.com'), .address) }}`, Env{})
	if err != nil || len(v.([]any)) != 2 || v.([]any)[0] != "j@x.com" {
		t.Fatalf("emailAddresses: %v %v", v, err)
	}
	v, err = Render("{{ mimeMessage({to: 'a@x.com', subject: 'Hi', html: '<b>x</b>'}) }}", Env{})
	if err != nil || !strings.Contains(v.(string), "Content-Type: text/html") {
		t.Fatalf("mimeMessage: %v %v", v, err)
	}
}

func items(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"a": i, "g": i % 3, "s": "x"}
	}
	return out
}

func TestPredicateSemantics(t *testing.T) {
	env := Env{
		"nums":  []any{3, 1, 2, 5, 4},
		"items": []any{map[string]any{"a": 1, "g": "x"}, map[string]any{"a": 2, "g": "y"}, map[string]any{"a": 3, "g": "x"}},
		"deep":  map[string]any{"list": []any{map[string]any{"v": 1}, map[string]any{}}},
	}
	cases := []struct {
		code string
		want any
	}{
		{`filter(nums, # > 2)`, []any{3, 5, 4}},
		{`map(nums, # * 2)`, []any{6, 2, 4, 10, 8}},
		{`map(items, .a)`, []any{1, 2, 3}},
		{`reduce(nums, #acc + #, 0)`, 15},
		{`reduce(nums, #acc + #index, 10)`, 20},
		{`sortBy(items, .a, "desc")[0].a`, 3},
		{`sortBy(nums, -#)`, []any{5, 4, 3, 2, 1}},
		{`groupBy(items, .g)["x"][1].a`, 3},
		{`count(nums, # > 2)`, 3},
		{`sum(nums, # * 2)`, 30},
		{`find(nums, # > 3)`, 5},
		{`findIndex(nums, # > 3)`, 3},
		{`findLast(nums, # < 3)`, 2},
		{`findLastIndex(nums, # < 3)`, 2},
		{`all(nums, # > 0)`, true},
		{`all(nums, # > 1)`, false},
		{`any(nums, # > 4)`, true},
		{`none(nums, # > 5)`, true},
		{`one(nums, # == 3)`, true},
		{`map(filter(items, .a > 1), .a + 1)`, []any{3, 4}},
		{`map(nums, map(nums, # + 1)[0])`, []any{4, 4, 4, 4, 4}},
		{`all(items, any(nums, # >= 1))`, true},
		{`map(nums, filter(nums, # == 3)[0])`, []any{3, 3, 3, 3, 3}},
		{`map(items, let a = .a; filter(nums, # == a)[0])`, []any{1, 2, 3}},
		{`filter(deep.list, .v == nil)`, []any{map[string]any{}}},
		{`map(deep.list, .v)`, []any{1, nil}},
		{`map(nums, "a" + string(#))[1]`, "a1"},
		{`1 + 2 * 3`, 7},
		{`"a" + "b"`, "ab"},
	}
	for _, c := range cases {
		got, err := Eval(c.code, env)
		if err != nil {
			t.Errorf("%s: %v", c.code, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v want %#v", c.code, got, c.want)
		}
	}
}

func TestStepBudget(t *testing.T) {
	env := Env{"items": items(2000)}
	start := time.Now()
	_, err := Eval(`all(items, all(items, all(items, .a >= 0)))`, env)
	elapsed := time.Since(start)
	t.Logf("pathological expression stopped after %v", elapsed)
	if err == nil || !strings.Contains(err.Error(), "too expensive") {
		t.Fatalf("want too expensive error, got %v", err)
	}
	if elapsed > 30*time.Second { // ~0.5s normally, ~6s under -race
		t.Fatalf("took %v", elapsed)
	}
}

func TestLargeButLegitimate(t *testing.T) {
	env := Env{"items": items(100_000)}
	v, err := Eval(`len(filter(items, .a > 5))`, env)
	if err != nil || v != 99_994 {
		t.Fatalf("filter: %v %v", v, err)
	}
	if v, err = Eval(`sum(map(items, .a))`, env); err != nil || v != 4_999_950_000 {
		t.Fatalf("sum: %v %v", v, err)
	}
	env = Env{"items": items(300)}
	if v, err = Eval(`all(items, all(items, .a >= 0))`, env); err != nil || v != true {
		t.Fatalf("nested: %v %v", v, err)
	}
	if v, err = Eval(`len(uniq(map(items, .g)))`, env); err != nil || v != 3 {
		t.Fatalf("uniq: %v %v", v, err)
	}
}

func TestMaxStepsConfigurable(t *testing.T) {
	old := MaxSteps
	MaxSteps = 10
	defer func() { MaxSteps = old }()
	if _, err := Eval(`all(1..10, # > 0)`, Env{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Eval(`all(1..11, # > 0)`, Env{}); err == nil {
		t.Fatal("want error")
	}
}

func TestCancellation(t *testing.T) {
	old := MaxSteps
	MaxSteps = 1 << 60
	defer func() { MaxSteps = old }()
	env := Env{"items": items(2000)}
	const code = `all(items, all(items, all(items, .a >= 0)))`

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Eval(code, WithContext(env, ctx)); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled ctx: %v", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Eval(code, WithContext(env, ctx))
	if err == nil || !strings.Contains(err.Error(), "cancelled") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stopped after %v", d)
	}
}

func TestContextSurvivesCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := WithContext(Env{"a": 1}, ctx)
	cp := Env{}
	maps.Copy(cp, env)
	if _, err := Render("{{ a }}", cp); err == nil {
		t.Fatal("copied env lost the context")
	}
	if _, ok := Env(nil)[ctxKey]; ok || WithContext(Env{"a": 1}, nil)["a"] != 1 {
		t.Fatal("nil context must keep env")
	}
}

func TestHiddenKeysNotExposed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := WithContext(Env{"json": map[string]any{"a": 1}}, ctx)
	v, err := Eval(`$env`, env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Normalize(v))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "steps") || strings.Contains(string(b), "Err") {
		t.Fatalf("leaked: %s", b)
	}
	if Stringify(v) == "" {
		t.Fatal("stringify")
	}
	if _, ok := env[budgetKey]; ok {
		t.Fatal("Eval wrote into the caller's env")
	}
}

func TestReservedNames(t *testing.T) {
	for _, code := range []string{
		`__budget`, `__tick(1, 2)`, `let __budget = 1; all(1..3, # > 0)`,
		`map(1..3, __budget)`, `__ctx`, `all(1..3, let __x = 1; true)`,
	} {
		_, err := Eval(code, Env{})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: want reserved error, got %v", code, err)
		}
	}
	// A field called __typename is data, not a name.
	if v, err := Eval(`json.__typename`, Env{"json": map[string]any{"__typename": "T"}}); err != nil || v != "T" {
		t.Fatalf("%v %v", v, err)
	}
	// Even if a user env entry shadows the budget, tick refuses it.
	if _, err := Eval(`all(1..3, # > 0)`, Env{"__budget": 1}); err != nil {
		t.Fatalf("budget must be replaced by Eval: %v", err)
	}
	ids, err := Identifiers("{{ config.x }}")
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
}

func TestStringBounds(t *testing.T) {
	for _, code := range []string{
		`len(reduce(1..40, #acc + #acc, "x"))`,
		`let a = "xxxxxxxx"; let b = a + a; let c = b + b; let d = c + c; let e = d + d; let f = e + e; let g = f + f; let h = g + g; let i = h + h; let j = i + i; let k = j + j; let l = k + k; let m = l + l; let n = m + m; let o = n + n; let p = o + o; let q = p + p; let r = q + q; let s = r + r; let t = s + s; let u = t + t; let v = u + u; let w = v + v; let x = w + w; len(x)`,
		`len(repeat("x", 1000000000))`,
		`len(1..100000000)`,
	} {
		if _, err := Eval(code, Env{}); err == nil {
			t.Errorf("%.40s...: want error", code)
		}
	}
}

func TestUniq(t *testing.T) {
	v, err := Eval(`uniq([1, 15, "a", 2, 3, 5, 2, "a", 2, "b", 1.0, [1, 2], [1, 2], "x", nil, nil])`, Env{})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{1, 15, "a", 2, 3, 5, "b", []any{1, 2}, "x", nil}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v", v)
	}
}

func TestConcurrentEval(t *testing.T) {
	env := Env{"items": items(50)}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				v, err := Eval(fmt.Sprintf(`len(filter(items, .a > %d)) + 0`, i%10), env)
				if err != nil || v != 49-i%10 {
					t.Errorf("got %v %v", v, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(env) != 1 {
		t.Fatalf("env was modified: %v", env)
	}
}

func TestCacheBounded(t *testing.T) {
	for i := 0; i < maxCacheEntries+50; i++ {
		if _, err := compile(fmt.Sprintf("%d + 1", i)); err != nil {
			t.Fatal(err)
		}
	}
	cacheMu.RLock()
	n := len(cache)
	cacheMu.RUnlock()
	if n > maxCacheEntries {
		t.Fatalf("cache has %d entries", n)
	}
}

func BenchmarkSimple(b *testing.B) {
	env := Env{"json": map[string]any{"count": 1}}
	for i := 0; i < b.N; i++ {
		if _, err := Eval("json.count + 1", env); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilter1000(b *testing.B) {
	env := Env{"items": items(1000)}
	for i := 0; i < b.N; i++ {
		if _, err := Eval("filter(items, .a > 5)", env); err != nil {
			b.Fatal(err)
		}
	}
}

func TestURLFunctions(t *testing.T) {
	env := Env{"v": "a b/c?d=e&f#g+é"}
	for in, want := range map[string]any{
		"{{ urlEncode(v) }}":    "a+b%2Fc%3Fd%3De%26f%23g%2B%C3%A9",
		"{{ urlEncode(nil) }}":  "",
		"{{ urlEncode(12.5) }}": "12.5",
		"{{ pathEscape(v) }}":   "a%20b%2Fc%3Fd=e&f%23g+%C3%A9",
		"q={{ urlEncode(v) }}":  "q=a+b%2Fc%3Fd%3De%26f%23g%2B%C3%A9",
		"{{ rawPath('a/b') }}":  RawPath("a/b"),
	} {
		got, err := Render(in, env)
		if err != nil || got != want {
			t.Errorf("%s: got %#v, %v; want %#v", in, got, err, want)
		}
	}
	// Mixed with text, rawPath is plain text.
	if got, _ := Render("/x/{{ rawPath('a/b') }}", env); got != "/x/a/b" {
		t.Errorf("mixed: %#v", got)
	}
}

func TestRenderWith(t *testing.T) {
	env := Env{"a": "x/y", "n": 3}
	var prefixes []string
	got, err := RenderWith("/p/{{ a }}/q/{{ n }}", env, func(prefix string, v any) (string, error) {
		prefixes = append(prefixes, prefix)
		return "<" + Stringify(v) + ">", nil
	})
	if err != nil || got != "/p/<x/y>/q/<3>" {
		t.Fatalf("%q %v", got, err)
	}
	if len(prefixes) != 2 || prefixes[0] != "/p/" || prefixes[1] != "/p/{{ a }}/q/" {
		t.Fatalf("prefixes %q", prefixes)
	}
	if _, err := RenderWith("{{ a }", env, nil); err == nil {
		t.Fatal("unterminated block accepted")
	}
	if got, err := RenderWith("no blocks", env, nil); err != nil || got != "no blocks" {
		t.Fatalf("%q %v", got, err)
	}
	wantErr := errors.New("nope")
	if _, err := RenderWith("{{ a }}", env, func(string, any) (string, error) { return "", wantErr }); err != wantErr {
		t.Fatalf("callback error: %v", err)
	}
}
