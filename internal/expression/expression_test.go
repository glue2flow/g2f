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
