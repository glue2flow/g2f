package api

import (
	"net/http"
	"strings"
	"testing"
)

const tinyManifest = `module: tiny
name: Tiny
version: 1.0.0
baseUrl: https://api.example.com
nodes:
  - { type: ping, name: Ping, request: { path: /ping } }
`

func nodeTypes(t *testing.T, c *client) []string {
	t.Helper()
	var out []string
	for _, d := range list(t, c.must(http.StatusOK, "GET", "/api/nodes", nil)) {
		out = append(out, obj(t, d)["type"].(string))
	}
	return out
}

func moduleNames(t *testing.T, c *client) []string {
	t.Helper()
	var out []string
	for _, m := range list(t, c.must(http.StatusOK, "GET", "/api/plugins", nil)) {
		out = append(out, obj(t, m)["module"].(string))
	}
	return out
}

func TestPlugins(t *testing.T) {
	s := newTestServer(t)
	pa := s.as(pluginAdmin)

	t.Run("everyone signed in can list plugins", func(t *testing.T) {
		for _, u := range []testUser{alice, carol} {
			got := moduleNames(t, s.as(u))
			for _, want := range []string{"github", "gmail", "email", "slack"} {
				if !contains(got, want) {
					t.Errorf("%s: %s missing from %v", u.Sub, want, got)
				}
			}
		}
	})
	t.Run("import, delete and catalog need the plugin admin role", func(t *testing.T) {
		for _, u := range []testUser{alice, orgAdmin} {
			c := s.as(u)
			rec := c.upload("/api/plugins", "tiny.yaml", tinyManifest, nil)
			if rec.Code != http.StatusForbidden || errorOf(t, decodeBody(t, rec)) != "requires the plugin admin role" {
				t.Errorf("%s import: %d %s", u.Sub, rec.Code, rec.Body)
			}
			if code, _ := c.do("DELETE", "/api/plugins/github", nil); code != http.StatusForbidden {
				t.Errorf("%s delete: %d", u.Sub, code)
			}
		}
		if contains(nodeTypes(t, s.as(alice)), "tiny.ping") {
			t.Fatal("refused import installed the plugin")
		}
	})
	t.Run("plugin admin imports a manifest and its nodes appear", func(t *testing.T) {
		info := s.importPlugin("tiny.yaml", tinyManifest)
		if info["module"] != "tiny" || info["version"] != "1.0.0" || info["installedBy"] != "Plugin Admin" {
			t.Fatalf("import result %v", info)
		}
		if !contains(nodeTypes(t, s.as(alice)), "tiny.ping") || !contains(moduleNames(t, s.as(carol)), "tiny") {
			t.Fatal("imported plugin not visible")
		}
	})
	t.Run("replacing needs confirmation", func(t *testing.T) {
		v2 := strings.Replace(tinyManifest, "1.0.0", "2.0.0", 1)
		rec := pa.upload("/api/plugins", "tiny.yaml", v2, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("replace without flag: %d %s", rec.Code, rec.Body)
		}
		body := obj(t, decodeBody(t, rec))
		conflict := obj(t, body["conflict"])
		if !strings.Contains(errorOf(t, body), "already installed") || obj(t, conflict["current"])["version"] != "1.0.0" ||
			obj(t, conflict["incoming"])["version"] != "2.0.0" {
			t.Fatalf("conflict body %v", body)
		}
		rec = pa.upload("/api/plugins", "tiny.yaml", v2, map[string]string{"replace": "true"})
		if rec.Code != http.StatusCreated || obj(t, decodeBody(t, rec))["version"] != "2.0.0" {
			t.Fatalf("replace: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("invalid uploads are 400, bundled module names 409", func(t *testing.T) {
		cases := []struct {
			name, file, content string
			want                int
		}{
			{"invalid manifest", "bad.yaml", "module: bad\nnodez: []", http.StatusBadRequest},
			{"unsupported file type", "tiny.txt", tinyManifest, http.StatusBadRequest},
			{"no file", "", "", http.StatusBadRequest},
			{"name of a bundled module", "github.yaml", strings.Replace(tinyManifest, "module: tiny", "module: github", 1), http.StatusConflict},
		}
		for _, c := range cases {
			rec := pa.upload("/api/plugins", c.file, c.content, nil)
			if rec.Code != c.want {
				t.Errorf("%s: %d, want %d: %s", c.name, rec.Code, c.want, rec.Body)
			}
			errorOf(t, decodeBody(t, rec))
		}
		rec := pa.raw("POST", "/api/plugins", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("not multipart: %d", rec.Code)
		}
	})
	t.Run("delete", func(t *testing.T) {
		for module, want := range map[string]int{"github": http.StatusConflict, "builtin": http.StatusConflict, "nope": http.StatusNotFound} {
			if code, body := pa.do("DELETE", "/api/plugins/"+module, nil); code != want {
				t.Errorf("delete %s: %d, want %d (%v)", module, code, want, body)
			}
		}
		pa.must(http.StatusNoContent, "DELETE", "/api/plugins/tiny", nil)
		if contains(nodeTypes(t, pa), "tiny.ping") {
			t.Fatal("deleted plugin's nodes still listed")
		}
		pa.must(http.StatusNotFound, "DELETE", "/api/plugins/tiny", nil)
	})
}

func TestPluginImportRequiresSignature(t *testing.T) {
	s := newTestServerWith(t, serverOptions{requireSigned: true})
	if cfg := obj(t, s.anon().must(http.StatusOK, "GET", "/api/config", nil)); cfg["pluginRequireSigned"] != true {
		t.Fatalf("config %v", cfg)
	}
	rec := s.as(pluginAdmin).upload("/api/plugins", "tiny.yaml", tinyManifest, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, decodeBody(t, rec)), "not signed") {
		t.Fatalf("unsigned import: %d %s", rec.Code, rec.Body)
	}
	rec = s.as(pluginAdmin).upload("/api/plugins", "tiny.yaml", tinyManifest, map[string]string{"signature": "garbage"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad signature: %d %s", rec.Code, rec.Body)
	}
	if contains(moduleNames(t, s.as(alice)), "tiny") {
		t.Fatal("unsigned plugin installed")
	}
}

func TestPluginCatalogEndpoints(t *testing.T) {
	// Nothing listens on port 1: fetching the index fails fast.
	const index = "http://127.0.0.1:1/index.json"
	s := newTestServerWith(t, serverOptions{catalogs: []string{index}})
	if cfg := obj(t, s.anon().must(http.StatusOK, "GET", "/api/config", nil)); cfg["pluginCatalog"] != true {
		t.Fatalf("config %v", cfg)
	}
	for _, u := range []testUser{alice, orgAdmin} {
		c := s.as(u)
		for _, r := range []struct{ method, path string }{
			{"GET", "/api/plugins/catalog"}, {"POST", "/api/plugins/catalog/refresh"}, {"POST", "/api/plugins/catalog/install"},
		} {
			if code, _ := c.do(r.method, r.path, map[string]any{"catalog": index, "module": "x"}); code != http.StatusForbidden {
				t.Errorf("%s %s %s: %d, want 403", u.Sub, r.method, r.path, code)
			}
		}
	}
	pa := s.as(pluginAdmin)
	got := obj(t, pa.must(http.StatusOK, "GET", "/api/plugins/catalog", nil))
	cats := list(t, got["catalogs"])
	if len(cats) != 1 || obj(t, cats[0])["url"] != index || obj(t, cats[0])["error"] == nil {
		t.Fatalf("catalogs %v", got)
	}
	code, body := pa.do("POST", "/api/plugins/catalog/install", map[string]any{"catalog": "https://elsewhere.example.com/index.json", "module": "x"})
	if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "unknown catalog") {
		t.Fatalf("install from an unconfigured catalog: %d %v", code, body)
	}
}
