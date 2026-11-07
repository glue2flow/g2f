package expression

import (
	"reflect"
	"testing"
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
