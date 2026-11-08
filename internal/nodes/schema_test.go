package nodes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsDeclareOutputSchema(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltins(r); err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Definitions() {
		if d.OutputSchema == nil {
			t.Errorf("%s has no output schema", d.Type)
			continue
		}
		if err := d.OutputSchema.Validate(d.Params); err != nil {
			t.Errorf("%s: %v", d.Type, err)
		}
	}
}

func TestManifestOutputSchema(t *testing.T) {
	base := "module: demo\nnodes:\n  - type: get\n    name: Get\n    request: {url: 'https://example.com'}\n"
	m, err := ParseManifest([]byte(base + "    params: [{name: q}]\n    outputSchema:\n      fromParam: q\n      fields:\n        - {path: user.id, type: number}\n        - path: name\n"))
	if err != nil {
		t.Fatal(err)
	}
	def := m.NodeDefinition(m.Nodes[0], "file:demo")
	if def.OutputSchema == nil || len(def.OutputSchema.Fields) != 2 || def.OutputSchema.Fields[0].Path != "user.id" {
		t.Fatalf("schema not carried into definition: %+v", def.OutputSchema)
	}
	for in, want := range map[string]string{
		"    outputSchema: {fields: [{path: 'a..b'}]}\n":               "invalid path",
		"    outputSchema: {fields: [{path: a, type: date}]}\n":        "unknown type",
		"    outputSchema: {fromParam: nope}\n":                        "unknown parameter",
		"    outputSchema: {passthrough: true, passthroughIf: nope}\n": "passthroughIf",
		"    outputSchema: {fields: [{path: a}, {path: a}]}\n":         "duplicate field",
	} {
		if _, err := ParseManifest([]byte(base + in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
}

// The shipped modules and sample plugins declare output schemas for every node.
func TestShippedManifestsDeclareOutputSchema(t *testing.T) {
	files, _ := filepath.Glob("../../modules/*.yaml")
	plugins, _ := filepath.Glob("../../examples/plugins/*/plugin.yaml")
	files = append(files, plugins...)
	if len(files) < 5 {
		t.Fatalf("found only %v", files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m, err := ParseManifest(raw)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, n := range m.Nodes {
			if n.OutputSchema == nil {
				t.Errorf("%s: node %s has no outputSchema", f, n.Type)
			}
		}
	}
}
