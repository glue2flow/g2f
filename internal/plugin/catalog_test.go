package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/plugin/signing"
)

// testCatalog serves an index and bundles on 127.0.0.1. The bundles are on
// the index's own origin, which is what lets them use plain http here; the
// same rule applies in production.
type testCatalog struct {
	srv     *httptest.Server
	files   map[string][]byte
	index   func() any
	fetches atomic.Int32
}

func newTestCatalog(t *testing.T) *testCatalog {
	c := &testCatalog{files: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugins/index.json" {
			c.fetches.Add(1)
			_ = json.NewEncoder(w).Encode(c.index())
			return
		}
		data, ok := c.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *testCatalog) url() string { return c.srv.URL + "/plugins/index.json" }

func entryFor(module, version, file string, data []byte, sig *signing.Signature) map[string]any {
	e := map[string]any{"module": module, "version": version, "url": file, "sha256": digestOf(data), "description": "test"}
	if sig != nil {
		e["signature"] = sig
	}
	return e
}

func catalogFixture(t *testing.T, trust Trust) (*testCatalog, *Catalogs, *nodes.Registry, *memStore) {
	t.Helper()
	c := newTestCatalog(t)
	reg := nodes.NewRegistry()
	st := newMemStore()
	mgr := NewManager(reg, st, discard)
	mgr.SetTrust(trust)
	return c, NewCatalogs([]string{c.url()}, mgr, "dev"), reg, st
}

func pluginByModule(t *testing.T, v CatalogView, module string) CatalogPlugin {
	t.Helper()
	for _, p := range v.Plugins {
		if p.Module == module {
			return p
		}
	}
	t.Fatalf("%s not listed in %+v", module, v)
	return CatalogPlugin{}
}

func TestCatalogInstallAndStates(t *testing.T) {
	acmePub, acme := newKey(t)
	_, stranger := newKey(t)
	trust := Trust{Keys: signing.Keys{"acme": acmePub}}
	c, cats, reg, _ := catalogFixture(t, trust)
	ctx := context.Background()

	v1 := yamlPlugin("demo", "1.0.0")
	v2 := yamlPlugin("demo", "1.2.0")
	plain := yamlPlugin("plain", "0.1.0")
	strange := yamlPlugin("strange", "1.0.0")
	c.files["/plugins/demo-1.0.0.yaml"] = v1
	c.files["/plugins/files/demo-1.2.0.yaml"] = v2
	c.files["/plugins/plain.yaml"] = plain
	c.files["/plugins/strange.yaml"] = strange
	sigV1, _ := signing.Sign("demo-1.0.0.yaml", v1, "acme", acme)
	sigV2, _ := signing.Sign("demo-1.2.0.yaml", v2, "acme", acme)
	sigStrange, _ := signing.Sign("strange.yaml", strange, "stranger", stranger)
	current := entryFor("demo", "1.0.0", "demo-1.0.0.yaml", v1, sigV1)
	c.index = func() any {
		return map[string]any{"name": "ACME plugins", "plugins": []any{
			current,
			entryFor("plain", "0.1.0", "plain.yaml", plain, nil),
			entryFor("strange", "1.0.0", "strange.yaml", strange, sigStrange),
		}}
	}

	views := cats.List(ctx)
	if len(views) != 1 || views[0].Error != "" || views[0].Name != "ACME plugins" {
		t.Fatalf("%+v", views)
	}
	if p := pluginByModule(t, views[0], "demo"); p.State != "available" || p.Verification != "trusted" || p.KeyID != "acme" || !p.Installable {
		t.Fatalf("demo %+v", p)
	}
	if p := pluginByModule(t, views[0], "strange"); p.Verification != "unknown-key" || p.Installable {
		t.Fatalf("strange %+v", p)
	}
	if p := pluginByModule(t, views[0], "plain"); p.Verification != "unsigned" || !p.Installable {
		t.Fatalf("plain %+v", p)
	}

	info, err := cats.Install(ctx, c.url(), "demo", "alice", false)
	if err != nil || info.Signer != "acme" || !reg.Exists("demo.get") {
		t.Fatalf("install: %+v %v", info, err)
	}
	if _, err := cats.Install(ctx, c.url(), "strange", "alice", false); err == nil || !strings.Contains(err.Error(), "not in G2F_PLUGIN_TRUSTED_KEYS") {
		t.Fatalf("unknown key installed: %v", err)
	}
	if _, err := cats.Install(ctx, c.url(), "plain", "alice", false); err != nil {
		t.Fatalf("unsigned without require-signed: %v", err)
	}
	if p := pluginByModule(t, cats.List(ctx)[0], "demo"); p.State != "installed" || p.InstalledVersion != "1.0.0" {
		t.Fatalf("after install %+v", p)
	}

	// The catalog publishes 1.2.0 under a sub-folder (relative URL).
	current = entryFor("demo", "1.2.0", "files/demo-1.2.0.yaml", v2, sigV2)
	if c.fetches.Load() != 1 {
		t.Fatalf("index fetched %d times, want 1 (cached)", c.fetches.Load())
	}
	views = cats.Refresh(ctx)
	if p := pluginByModule(t, views[0], "demo"); p.State != "update" || p.InstalledVersion != "1.0.0" || p.Version != "1.2.0" {
		t.Fatalf("update state %+v", p)
	}
	if _, err := cats.Install(ctx, c.url(), "demo", "alice", false); !errors.Is(err, ErrExists) {
		t.Fatalf("update without replace: %v", err)
	}
	if info, err := cats.Install(ctx, c.url(), "demo", "alice", true); err != nil || info.Version != "1.2.0" {
		t.Fatalf("update: %+v %v", info, err)
	}

	// Tampered download: the sha256 no longer matches.
	c.files["/plugins/files/demo-1.2.0.yaml"] = append(v2, '#')
	if _, err := cats.Install(ctx, c.url(), "demo", "alice", true); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha mismatch: %v", err)
	}

	// With require-signed the unsigned entry cannot be installed.
	cats.mgr.SetTrust(Trust{Keys: trust.Keys, RequireSigned: true})
	if p := pluginByModule(t, cats.List(ctx)[0], "plain"); p.Installable || !strings.Contains(p.Reason, "signed") {
		t.Fatalf("plain with require-signed %+v", p)
	}
	if _, err := cats.Install(ctx, c.url(), "plain", "alice", true); err == nil {
		t.Fatal("unsigned catalog entry installed with require-signed")
	}
	if _, err := cats.Install(ctx, "https://elsewhere.example/index.json", "demo", "alice", true); err == nil {
		t.Fatal("unconfigured catalog accepted")
	}
}

// A wasm bundle from a catalog with a separate signature file, verified again
// on another replica that requires signatures.
func TestCatalogWasmWithSignatureURL(t *testing.T) {
	acmePub, acme := newKey(t)
	trust := Trust{Keys: signing.Keys{"acme": acmePub}, RequireSigned: true}
	c, cats, reg, st := catalogFixture(t, trust)
	zip := sampleZip(t, "127.0.0.1")
	sig, _ := signing.Sign("telegram.zip", zip, "acme", acme)
	c.files["/plugins/telegram.zip"] = zip
	c.files["/plugins/telegram.zip.sig"] = sig.Marshal()
	c.index = func() any {
		e := entryFor("telegram", "1.0.0", "telegram.zip", zip, nil)
		e["signatureUrl"] = "telegram.zip.sig"
		return map[string]any{"plugins": []any{e}}
	}
	if p := pluginByModule(t, cats.List(context.Background())[0], "telegram"); p.KeyID != "acme" || !p.Installable {
		t.Fatalf("%+v", p)
	}
	info, err := cats.Install(context.Background(), c.url(), "telegram", "alice", false)
	if err != nil || info.Signer != "acme" || !reg.Exists("telegram.sendMessage") {
		t.Fatalf("%+v %v", info, err)
	}
	reg2 := nodes.NewRegistry()
	m2 := NewManager(reg2, st, discard)
	m2.SetTrust(trust)
	if err := m2.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := reg2.Module("telegram"); !ok || got.Signer != "acme" {
		t.Fatalf("replica: %+v", got)
	}
}

func TestCatalogFetchRules(t *testing.T) {
	ctx := context.Background()
	c, cats, _, _ := catalogFixture(t, Trust{})
	data := yamlPlugin("demo", "1.0.0")

	// Redirects to another host are refused (localhost != 127.0.0.1).
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write(data)
	}))
	defer other.Close()
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	c.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plugins/index.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"plugins": []any{
				entryFor("demo", "1.0.0", "demo.yaml", data, nil),
				entryFor("big", "1.0.0", "big.yaml", data, nil),
			}})
		case "/plugins/demo.yaml":
			http.Redirect(w, r, otherURL+"/demo.yaml", http.StatusFound)
		case "/plugins/big.yaml":
			_, _ = w.Write(make([]byte, maxBundleSize+1))
		}
	})
	if _, err := cats.Install(ctx, c.url(), "demo", "alice", false); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("redirect: %v", err)
	}
	if reached.Load() != 0 {
		t.Fatal("redirect target was reached")
	}
	if _, err := cats.Install(ctx, c.url(), "big", "alice", false); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized: %v", err)
	}

	// Bundles on other hosts go through the guarded transport.
	var guarded atomic.Int32
	cats.guarded = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		guarded.Add(1)
		return nil, errors.New("blocked by test")
	})
	c.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"plugins": []any{
			entryFor("cdn", "1.0.0", "https://cdn.example.net/cdn.yaml", data, nil),
		}})
	})
	cats.Refresh(ctx)
	if _, err := cats.Install(ctx, c.url(), "cdn", "alice", false); err == nil || guarded.Load() != 1 {
		t.Fatalf("third-party host not fetched through the guard: %v (%d)", err, guarded.Load())
	}

	// An unreachable catalog is reported, not fatal.
	down := NewCatalogs([]string{"http://127.0.0.1:1/index.json", c.url()}, cats.mgr, "dev")
	views := down.List(ctx)
	if len(views) != 2 || views[0].Error == "" || views[1].Error != "" || len(views[1].Plugins) != 1 {
		t.Fatalf("%+v", views)
	}
	if _, err := down.Install(ctx, "http://127.0.0.1:1/index.json", "demo", "alice", false); err == nil {
		t.Fatal("install from unreachable catalog")
	}
}

func TestParseIndex(t *testing.T) {
	base, _ := url.Parse("https://plugins.example.com/acme/index.json")
	sha := strings.Repeat("ab", 32)
	for name, doc := range map[string]string{
		"bad json":      `{"plugins": [`,
		"no plugins":    `{"name": "x"}`,
		"plugins type":  `{"plugins": {}}`,
		"not an object": `[]`,
	} {
		if _, _, err := parseIndex(base, []byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	doc := `{"plugins": [
		{"module": "ok", "version": "1.0.0", "url": "bundles/ok-1.0.0.zip", "sha256": "` + sha + `"},
		{"module": "abs", "version": "1.0.0", "url": "https://cdn.example.net/abs.yaml", "sha256": "` + strings.ToUpper(sha) + `"},
		{"module": "plainhttp", "version": "1", "url": "http://cdn.example.net/x.zip", "sha256": "` + sha + `"},
		{"module": "nover", "url": "x.zip", "sha256": "` + sha + `"},
		{"module": "nosha", "version": "1", "url": "x.zip"},
		{"module": "nourl", "version": "1", "sha256": "` + sha + `"},
		{"module": "Bad Name", "version": "1", "url": "x.zip", "sha256": "` + sha + `"},
		{"module": "ok", "version": "2", "url": "x.zip", "sha256": "` + sha + `"},
		{"module": "exe", "version": "1", "url": "x.exe", "sha256": "` + sha + `"},
		{"module": "creds", "version": "1", "url": "https://u:p@cdn.example.net/x.zip", "sha256": "` + sha + `"},
		{"module": "badsig", "version": "1", "url": "x.zip", "sha256": "` + sha + `", "signature": {"keyId": "a", "alg": "rsa", "signature": "x"}},
		{"module": "bothsig", "version": "1", "url": "x.zip", "sha256": "` + sha + `", "signatureUrl": "x.sig", "signature": {"keyId": "a", "alg": "ed25519", "signature": "x"}}
	]}`
	idx, items, err := parseIndex(base, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if idx.Name != "plugins.example.com" {
		t.Errorf("default name %q", idx.Name)
	}
	errs := map[string]string{}
	for _, it := range items {
		if _, dup := errs[it.Module]; !dup {
			errs[it.Module] = it.err
		} else {
			errs[it.Module+"#2"] = it.err
		}
	}
	if errs["ok"] != "" || errs["abs"] != "" {
		t.Fatalf("valid entries refused: %v", errs)
	}
	if got := items[0].bundleURL.String(); got != "https://plugins.example.com/acme/bundles/ok-1.0.0.zip" {
		t.Errorf("relative url resolved to %s", got)
	}
	if items[1].SHA256 != sha {
		t.Error("sha256 not normalized to lower case")
	}
	for _, m := range []string{"plainhttp", "nover", "nosha", "nourl", "Bad Name", "ok#2", "exe", "creds", "badsig", "bothsig"} {
		if errs[m] == "" {
			t.Errorf("%s accepted", m)
		}
	}
	if !strings.Contains(errs["plainhttp"], "https") {
		t.Errorf("plainhttp: %s", errs["plainhttp"])
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.2.10", "1.2.9", 1}, {"v1.2", "1.2.0", 0}, {"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0-rc2", "1.0.0-rc1", 1}, {"2", "10", -1}, {"1.0.0+build5", "1.0.0", 0}, {"", "0.1", -1},
	} {
		if got := CompareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("%q vs %q: %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
