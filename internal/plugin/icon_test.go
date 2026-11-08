package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
)

const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#f00"/></svg>`

func iconZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportedPluginIcons(t *testing.T) {
	manifest := "module: iconic\nbaseUrl: https://example.com\nicon: img/logo.svg\nnodes: [{type: a, name: A, request: {path: /a}, outputSchema: {dynamic: true}}]\n"
	st := newMemStore()
	mgr := NewManager(nodes.NewRegistry(), st, discard)
	ctx := context.Background()

	// Bundled with a top-level folder; the icon path is relative to plugin.yaml.
	data := iconZip(t, map[string]string{"iconic/plugin.yaml": manifest, "iconic/img/logo.svg": iconSVG})
	info, err := mgr.Import(ctx, "iconic.zip", data, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Icon, "data:image/svg+xml;base64,") {
		t.Fatalf("icon %q", info.Icon)
	}

	// Another replica installs it from the database, icon included.
	reg2 := nodes.NewRegistry()
	if err := NewManager(reg2, st, discard).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if m, ok := reg2.Module("iconic"); !ok || m.Icon != info.Icon {
		t.Fatalf("synced module icon %+v", m)
	}

	// Missing and malicious icons are refused; a bare YAML upload can only use inline icons.
	for name, files := range map[string]map[string]string{
		"missing": {"plugin.yaml": manifest},
		"script":  {"plugin.yaml": manifest, "img/logo.svg": `<svg><script>alert(1)</script></svg>`},
	} {
		if _, err := mgr.Import(ctx, "x.zip", iconZip(t, files), "alice"); err == nil {
			t.Errorf("%s icon accepted", name)
		}
	}
	if _, err := mgr.Import(ctx, "x.yaml", []byte(manifest), "alice"); err == nil || !strings.Contains(err.Error(), ".zip") {
		t.Errorf("yaml upload with icon file: %v", err)
	}
}
