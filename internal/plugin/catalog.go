package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/plugin/signing"
)

const (
	maxIndexSize     = 1 << 20
	maxCatalogItems  = 1000
	catalogTTL       = 5 * time.Minute
	catalogErrorTTL  = time.Minute
	indexTimeout     = 20 * time.Second
	downloadTimeout  = 2 * time.Minute
	maxCatalogRedirs = 5
)

// CatalogIndex is the JSON document served by a plugin catalog.
type CatalogIndex struct {
	Name    string         `json:"name"`
	Plugins []CatalogEntry `json:"plugins"`
}

// CatalogEntry is one downloadable plugin of a catalog.
type CatalogEntry struct {
	Module      string `json:"module"`
	Name        string `json:"name,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	// URL of the bundle; relative URLs resolve against the index URL.
	URL string `json:"url"`
	// SHA256 of the bundle, hex.
	SHA256       string             `json:"sha256"`
	Signature    *signing.Signature `json:"signature,omitempty"`
	SignatureURL string             `json:"signatureUrl,omitempty"`
	Homepage     string             `json:"homepage,omitempty"`
	MinG2F       string             `json:"minG2F,omitempty"`
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// catalogItem is a validated entry.
type catalogItem struct {
	CatalogEntry
	bundleURL *url.URL
	err       string // why the entry cannot be installed
}

type catalogCache struct {
	at    time.Time
	name  string
	items []*catalogItem
	err   error
}

// Catalogs fetches plugin catalogs and installs plugins from them through
// the same verification and import path as uploads.
//
// The index URLs are set by the operator and fetched without the SSRF guard.
// Bundle and signature URLs come from the index, i.e. from a third party:
// they must be https and are fetched through the guarded transport, unless
// they are on the index's own origin (same scheme and host).
type Catalogs struct {
	urls    []string
	mgr     *Manager
	version string // of this server, for minG2F

	// Transports; tests replace them.
	unguarded http.RoundTripper
	guarded   http.RoundTripper

	mu    sync.Mutex
	cache map[string]*catalogCache
}

// NewCatalogs serves the catalogs at urls; version is this server's version
// ("dev" or empty skips minG2F checks).
func NewCatalogs(urls []string, mgr *Manager, version string) *Catalogs {
	return &Catalogs{
		urls: urls, mgr: mgr, version: version,
		unguarded: netguard.Unguarded(), guarded: netguard.NewTransport(),
		cache: map[string]*catalogCache{},
	}
}

// Enabled reports whether any catalog is configured.
func (c *Catalogs) Enabled() bool { return c != nil && len(c.urls) > 0 }

// CatalogView is a catalog as shown to plugin admins.
type CatalogView struct {
	URL       string          `json:"url"`
	Name      string          `json:"name"`
	Error     string          `json:"error,omitempty"`
	FetchedAt *time.Time      `json:"fetchedAt,omitempty"`
	Plugins   []CatalogPlugin `json:"plugins"`
}

// CatalogPlugin is a catalog entry merged with the installed state.
type CatalogPlugin struct {
	Module      string `json:"module"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// KeyID is the key the entry claims to be signed with; the signature is
	// verified on install, against the downloaded bundle.
	KeyID string `json:"keyId,omitempty"`
	// Verification: trusted (key is trusted), unknown-key, unsigned.
	Verification string `json:"verification"`
	// State: available, installed, update, older (installed is newer),
	// blocked (installed but not loaded), bundled (from the modules directory).
	State            string `json:"state"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	InstalledSigner  string `json:"installedSigner,omitempty"`
	Installable      bool   `json:"installable"`
	Reason           string `json:"reason,omitempty"`
}

// List returns all catalogs, from the cache when fresh. An unreachable
// catalog is reported in its view, never as an error.
func (c *Catalogs) List(ctx context.Context) []CatalogView {
	out := make([]CatalogView, 0, len(c.urls))
	for _, u := range c.urls {
		out = append(out, c.view(u, c.get(ctx, u, false)))
	}
	return out
}

// Refresh drops the cache and fetches every catalog again.
func (c *Catalogs) Refresh(ctx context.Context) []CatalogView {
	c.mu.Lock()
	c.cache = map[string]*catalogCache{}
	c.mu.Unlock()
	return c.List(ctx)
}

func (c *Catalogs) get(ctx context.Context, indexURL string, force bool) *catalogCache {
	c.mu.Lock()
	cached := c.cache[indexURL]
	c.mu.Unlock()
	if cached != nil && !force {
		ttl := catalogTTL
		if cached.err != nil {
			ttl = catalogErrorTTL
		}
		if time.Since(cached.at) < ttl {
			return cached
		}
	}
	fresh := &catalogCache{at: time.Now().UTC()}
	fresh.name, fresh.items, fresh.err = c.fetchIndex(ctx, indexURL)
	c.mu.Lock()
	c.cache[indexURL] = fresh
	c.mu.Unlock()
	return fresh
}

func (c *Catalogs) fetchIndex(ctx context.Context, indexURL string) (string, []*catalogItem, error) {
	base, err := url.Parse(indexURL)
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, indexTimeout)
	defer cancel()
	data, err := c.fetch(ctx, base, base, maxIndexSize)
	if err != nil {
		return "", nil, err
	}
	idx, items, err := parseIndex(base, data)
	if err != nil {
		return "", nil, err
	}
	for _, it := range items {
		if it.err != "" || it.SignatureURL == "" {
			continue
		}
		// Detached signature files are fetched with the index, so the list
		// can show the signer before anything is downloaded.
		su, err := resolveCatalogURL(base, it.SignatureURL)
		if err == nil {
			var raw []byte
			if raw, err = c.fetch(ctx, base, su, signing.MaxSignatureSize); err == nil {
				it.Signature, err = signing.Parse(raw)
			}
		}
		if err != nil {
			it.err = "signatureUrl: " + err.Error()
		}
	}
	return idx.Name, items, nil
}

// parseIndex decodes and validates a catalog index fetched from base. Errors
// in single entries are kept with the entry; a document that is not a
// catalog at all is an error.
func parseIndex(base *url.URL, data []byte) (*CatalogIndex, []*catalogItem, error) {
	var idx CatalogIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, nil, fmt.Errorf("invalid catalog index: %w", err)
	}
	if idx.Plugins == nil {
		return nil, nil, errors.New(`invalid catalog index: "plugins" is missing`)
	}
	if len(idx.Plugins) > maxCatalogItems {
		return nil, nil, fmt.Errorf("catalog index lists more than %d plugins", maxCatalogItems)
	}
	if idx.Name == "" {
		idx.Name = base.Host
	}
	seen := map[string]bool{}
	items := make([]*catalogItem, 0, len(idx.Plugins))
	for _, e := range idx.Plugins {
		it := &catalogItem{CatalogEntry: e}
		if it.Name == "" {
			it.Name = it.Module
		}
		it.SHA256 = strings.ToLower(it.SHA256)
		var err error
		switch {
		case !nodes.ValidModuleName(e.Module):
			err = fmt.Errorf("invalid module %q", e.Module)
		case seen[e.Module]:
			err = errors.New("module listed twice")
		case e.Version == "":
			err = errors.New("version is missing")
		case !sha256Re.MatchString(it.SHA256):
			err = errors.New("sha256 must be 64 hex characters")
		case e.Signature != nil && e.SignatureURL != "":
			err = errors.New("use either signature or signatureUrl")
		case e.URL == "":
			err = errors.New("url is missing")
		}
		if err == nil {
			if it.bundleURL, err = resolveCatalogURL(base, e.URL); err == nil {
				_, err = signing.Kind(it.bundleURL.Path)
			}
		}
		if err == nil && e.Signature != nil {
			_, err = signing.Parse(mustJSON(e.Signature))
		}
		if err != nil {
			it.err = err.Error()
		}
		seen[e.Module] = true
		items = append(items, it)
	}
	return &idx, items, nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// resolveCatalogURL resolves ref against the index URL. Third-party content
// must come over https; plain http is accepted only from the index's own
// origin, which the operator chose.
func resolveCatalogURL(base *url.URL, ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q", ref)
	}
	u := base.ResolveReference(r)
	if u.User != nil {
		return nil, fmt.Errorf("url %q must not contain credentials", ref)
	}
	if u.Scheme != "https" && !sameOrigin(base, u) {
		return nil, fmt.Errorf("url %q must use https", ref)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid url %q", ref)
	}
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// fetch GETs target (found via the index at base) with at most limit bytes.
// Redirects must stay on the same host.
func (c *Catalogs) fetch(ctx context.Context, base, target *url.URL, limit int64) ([]byte, error) {
	rt := c.guarded
	if sameOrigin(base, target) {
		rt = c.unguarded
	}
	client := &http.Client{Transport: rt, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxCatalogRedirs {
			return errors.New("too many redirects")
		}
		if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return fmt.Errorf("redirect to another host (%s) refused", req.URL.Host)
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != via[0].URL.Scheme {
			return errors.New("redirect from https to http refused")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "glue2flow-plugin-catalog")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", target.Redacted(), resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", target.Redacted(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", target.Redacted(), limit)
	}
	return data, nil
}

func (c *Catalogs) view(indexURL string, cc *catalogCache) CatalogView {
	v := CatalogView{URL: indexURL, Name: cc.name, Plugins: []CatalogPlugin{}}
	at := cc.at
	v.FetchedAt = &at
	if cc.err != nil {
		v.Error = cc.err.Error()
		return v
	}
	for _, it := range cc.items {
		v.Plugins = append(v.Plugins, c.describe(it))
	}
	return v
}

// describe merges an entry with what is installed and the signature policy.
func (c *Catalogs) describe(it *catalogItem) CatalogPlugin {
	p := CatalogPlugin{
		Module: it.Module, Name: it.Name, Version: it.Version, Description: it.Description,
		Runtime: it.Runtime, Homepage: it.Homepage, State: "available", Installable: true,
	}
	switch {
	case it.Signature == nil:
		p.Verification = "unsigned"
	case c.mgr.TrustsKey(it.Signature.KeyID):
		p.Verification, p.KeyID = "trusted", it.Signature.KeyID
	default:
		p.Verification, p.KeyID = "unknown-key", it.Signature.KeyID
	}
	if c.mgr.IsFileModule(it.Module) {
		p.State, p.Installable, p.Reason = "bundled", false, "installed from the modules directory"
		return p
	}
	if cur, ok := c.mgr.Installed(it.Module); ok {
		p.InstalledVersion, p.InstalledSigner = cur.Version, cur.Signer
		switch cmp := CompareVersions(it.Version, cur.Version); {
		case cur.Blocked != "":
			p.State = "blocked"
		case cmp > 0:
			p.State = "update"
		case cmp == 0:
			p.State = "installed"
		default:
			p.State = "older"
		}
	}
	if reason := c.refusal(it); reason != "" {
		p.Installable, p.Reason = false, reason
	}
	return p
}

// refusal says why an entry cannot be installed, as far as is known before
// downloading it.
func (c *Catalogs) refusal(it *catalogItem) string {
	switch {
	case it.err != "":
		return it.err
	case it.MinG2F != "" && c.version != "" && c.version != "dev" && CompareVersions(c.version, it.MinG2F) < 0:
		return "requires glue2flow " + it.MinG2F + " or later"
	case it.Signature != nil && !c.mgr.TrustsKey(it.Signature.KeyID):
		return fmt.Sprintf("signed by key %q, which is not in G2F_PLUGIN_TRUSTED_KEYS", it.Signature.KeyID)
	case it.Signature == nil && c.mgr.RequireSigned():
		return "not signed; this installation only accepts signed plugins"
	}
	return ""
}

// Install downloads a catalog entry, checks its sha256 and imports it like an
// upload (signature policy, explicit replace).
func (c *Catalogs) Install(ctx context.Context, indexURL, module, by string, replace bool) (*nodes.ModuleInfo, error) {
	known := false
	for _, u := range c.urls {
		known = known || u == indexURL
	}
	if !known {
		return nil, errors.New("unknown catalog")
	}
	cc := c.get(ctx, indexURL, false)
	if cc.err != nil {
		return nil, fmt.Errorf("catalog unavailable: %w", cc.err)
	}
	var it *catalogItem
	for _, x := range cc.items {
		if x.Module == module {
			it = x
		}
	}
	if it == nil {
		return nil, fmt.Errorf("catalog does not list %q", module)
	}
	if reason := c.refusal(it); reason != "" {
		return nil, errors.New(reason)
	}
	base, _ := url.Parse(indexURL)
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, err := c.fetch(dctx, base, it.bundleURL, maxBundleSize)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != it.SHA256 {
		return nil, errors.New("download does not match the sha256 in the catalog; not installed")
	}
	var sig []byte
	if it.Signature != nil {
		sig = it.Signature.Marshal()
	}
	return c.mgr.Import(ctx, path.Base(it.bundleURL.Path), data, by, ImportOptions{Signature: sig, Replace: replace, Module: module})
}

// CompareVersions compares dotted versions ("1.2.10" > "1.2.9"); a leading
// "v" and build metadata are ignored, and a pre-release ("1.0.0-rc1") sorts
// before its release. Non-numeric parts compare as strings.
func CompareVersions(a, b string) int {
	norm := func(v string) (core []string, pre string) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		v, _, _ = strings.Cut(v, "+")
		v, pre, _ = strings.Cut(v, "-")
		return strings.Split(v, "."), pre
	}
	ac, ap := norm(a)
	bc, bp := norm(b)
	for i := 0; i < len(ac) || i < len(bc); i++ {
		x, y := "0", "0"
		if i < len(ac) {
			x = ac[i]
		}
		if i < len(bc) {
			y = bc[i]
		}
		if c := comparePart(x, y); c != 0 {
			return c
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return comparePart(ap, bp)
}

func comparePart(x, y string) int {
	xn, errx := strconv.Atoi(x)
	yn, erry := strconv.Atoi(y)
	if errx == nil && erry == nil {
		switch {
		case xn < yn:
			return -1
		case xn > yn:
			return 1
		}
		return 0
	}
	return strings.Compare(x, y)
}
