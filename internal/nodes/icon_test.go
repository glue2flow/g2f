package nodes

import (
	"errors"
	"strings"
	"testing"
)

const testSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><defs><linearGradient id="g"/></defs><path fill="url(#g)" d="M0 0h24v24H0z"/><use href="#g"/></svg>`

func TestIconDataURI(t *testing.T) {
	uri, err := IconDataURI([]byte(testSVG))
	if err != nil || !strings.HasPrefix(uri, "data:image/svg+xml;base64,") {
		t.Fatalf("svg: %q %v", uri, err)
	}
	if uri, err := IconDataURI(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20)...)); err != nil || !strings.HasPrefix(uri, "data:image/png;") {
		t.Fatalf("png: %q %v", uri, err)
	}
	for name, bad := range map[string]string{
		"script":        `<svg><script>alert(1)</script></svg>`,
		"event handler": `<svg onload="alert(1)"></svg>`,
		"external href": `<svg><image href="https://evil.example/x.png"/></svg>`,
		"xlink js":      `<svg><a xlink:href="javascript:alert(1)"><path/></a></svg>`,
		"css url":       `<svg><path style="fill:url(https://evil.example/t)"/></svg>`,
		"foreignObject": `<svg><foreignObject><div/></foreignObject></svg>`,
		"entity":        `<!DOCTYPE svg [<!ENTITY x "y">]><svg/>`,
		"not an image":  `GIF89a....`,
		"too large":     `<svg>` + strings.Repeat(" ", MaxIconSize) + `</svg>`,
	} {
		if _, err := IconDataURI([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResolveIcons(t *testing.T) {
	m, err := ParseManifest([]byte(`
module: demo
baseUrl: https://example.com
icon: icons/demo.svg
nodes:
  - { type: a, name: A, request: { path: /a } }
  - { type: b, name: B, icon: "` + strings.ReplaceAll(testSVG, `"`, `\"`) + `", request: { path: /b } }
`))
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	files, err := m.ResolveIcons(func(name string) ([]byte, error) {
		asked = append(asked, name)
		return []byte(testSVG), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.IconURI == "" || m.Nodes[0].IconURI != "" || m.Nodes[1].IconURI == "" || len(files) != 1 || asked[0] != "icons/demo.svg" {
		t.Fatalf("module %q nodes %q/%q files %v asked %v", m.IconURI, m.Nodes[0].IconURI, m.Nodes[1].IconURI, files, asked)
	}
	info := m.Info("test")
	def := m.NodeDefinition(m.Nodes[1], "test")
	if info.Icon != m.IconURI || def.Icon == "" || m.NodeDefinition(m.Nodes[0], "test").Icon != "" {
		t.Fatal("icons not exposed on module info / node override")
	}

	// File references need a reader, and stay inside the plugin.
	if _, err := m.ResolveIcons(nil); err == nil || !strings.Contains(err.Error(), ".zip") {
		t.Fatalf("no reader: %v", err)
	}
	for _, ref := range []string{"../secret.svg", "/etc/x.svg", `a\b.svg`, "icons/../../x.svg"} {
		m.Icon = ref
		if _, err := m.ResolveIcons(func(string) ([]byte, error) { return []byte(testSVG), nil }); err == nil {
			t.Errorf("%s accepted", ref)
		}
	}
	m.Icon = "missing.svg"
	if _, err := m.ResolveIcons(func(string) ([]byte, error) { return nil, errors.New("no such file") }); err == nil {
		t.Error("missing file accepted")
	}
}
