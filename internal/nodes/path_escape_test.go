package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

func TestPathSegment(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     "plain",
		"a/b":       "a%2Fb",
		"x?y=1#z":   "x%3Fy=1%23z",
		"a b":       "a%20b",
		"%2e%2e":    "%252e%252e",
		"..x":       "..x",
		"Sheet1!A1": "Sheet1%21A1",
	} {
		if got, err := pathSegment(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{".", ".."} {
		if _, err := pathSegment(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string]string{
		"a/b/c":                   "a/b/c",
		"host.example:/sites/x":   "host.example:/sites/x",
		"contoso.com,guid1,guid2": "contoso.com,guid1,guid2",
		"root:/folder/file.txt:":  "root:/folder/file.txt:",
		"Sheet%201":               "Sheet%201",
	} {
		if got, err := pathSegment(expression.RawPath(in)); err != nil || got != want {
			t.Errorf("raw %q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"..", "a/../b", "a/./b", "a/%2e%2E/b", "a?b=1", "a#b", "a\\b", "a\nb", "%zz"} {
		if _, err := pathSegment(expression.RawPath(bad)); err == nil {
			t.Errorf("raw %q accepted", bad)
		}
	}
}

func TestInURLPath(t *testing.T) {
	for prefix, want := range map[string]bool{
		"":                           false,
		"https://":                   false,
		"https://api.example.com":    false,
		"https://api.example.com/":   true,
		"https://api.example.com/a/": true,
		"https://api.example.com/a?": false,
		"https://api.example.com/#":  false,
	} {
		if got := inURLPath(prefix); got != want {
			t.Errorf("%q: got %v", prefix, got)
		}
	}
}

// TestPathEscapingRequests sends requests through a test server and checks the
// path as it arrives on the wire.
func TestPathEscapingRequests(t *testing.T) {
	var raw, query string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, query = r.URL.EscapedPath(), r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer api.Close()
	m, err := ParseManifest([]byte(`
module: demo
baseUrl: ` + api.URL + `/api
nodes:
  - type: seg
    name: Seg
    params: [{ name: id }]
    request: { path: "/things/{{ params.id }}/x" }
  - type: raw
    name: Raw
    params: [{ name: id }]
    request: { path: "/things/{{ rawPath(params.id) }}/x" }
  - type: url
    name: Url
    params: [{ name: id }]
    request: { url: "` + api.URL + `/v1/{{ params.id }}?q={{ urlEncode(params.id) }}&r={{ params.id }}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	list, err := HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	run := func(node, id string) error {
		for _, n := range list {
			if n.Definition().Type == "demo."+node {
				def := n.Definition()
				_, err := n.Execute(context.Background(), &ExecContext{
					Node: &workflow.Node{Name: "N", Params: map[string]any{"id": id}}, Def: &def, Items: []Item{{}},
					Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				return err
			}
		}
		t.Fatalf("no node %s", node)
		return nil
	}

	if err := run("seg", "a/b"); err != nil || raw != "/api/things/a%2Fb/x" {
		t.Fatalf("a/b: %q %v", raw, err)
	}
	if err := run("seg", "x?y=1#z"); err != nil || raw != "/api/things/x%3Fy=1%23z/x" || query != "" {
		t.Fatalf("x?y=1#z: %q %q %v", raw, query, err)
	}
	for _, bad := range []string{"..", "."} {
		raw = ""
		if err := run("seg", bad); err == nil || raw != "" {
			t.Fatalf("%q: err %v, request path %q", bad, err, raw)
		}
	}
	if err := run("raw", "a/b"); err != nil || raw != "/api/things/a/b/x" {
		t.Fatalf("raw a/b: %q %v", raw, err)
	}
	for _, bad := range []string{"../admin", "a?b", "a#b"} {
		raw = ""
		if err := run("raw", bad); err == nil || raw != "" {
			t.Fatalf("raw %q: err %v, request path %q", bad, err, raw)
		}
	}
	// In a url template only the path part is segment-encoded; the query part is as written.
	if err := run("url", "a/b"); err != nil || raw != "/v1/a%2Fb" || query != "q=a%2Fb&r=a/b" {
		t.Fatalf("url: %q %q %v", raw, query, err)
	}
	if err := run("url", ".."); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("url ..: %v", err)
	}
}

// rawPath is for module authors: written in a node field it must not survive
// into the module's path template.
func TestRawPathFromNodeFieldIsEscaped(t *testing.T) {
	v, err := expression.Render(`{{ rawPath("a/b") }}`, expression.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(expression.RawPath); !ok {
		t.Fatalf("expected a RawPath from the expression, got %T", v)
	}
	nested := plainPaths(map[string]any{"x": []any{v}}).(map[string]any)["x"].([]any)[0]
	for _, got := range []any{plainPaths(v), nested} {
		seg, err := pathSegment(got)
		if err != nil || seg != "a%2Fb" {
			t.Fatalf("segment %q, err %v", seg, err)
		}
	}
}
