// Package plugin installs node modules ("plugins") from YAML manifests and
// WebAssembly bundles, either from the modules directory or imported at runtime.
package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/plugin/signing"
	"github.com/glue2flow/g2f/internal/store"
)

const (
	maxBundleSize   = 50 << 20 // 50 MiB upload
	maxManifestSize = 1 << 20
	maxWasmSize     = 40 << 20
	maxZipEntries   = 256
)

var (
	ErrReadOnly = errors.New("plugin is installed from the modules directory and cannot be changed through the API")
	ErrConflict = errors.New("module name conflicts with a built-in or file-based module")
)

// Store is the persistence the manager needs.
type Store interface {
	ListPlugins(ctx context.Context) ([]*store.PluginRecord, error)
	PluginDigests(ctx context.Context) (map[string]string, error)
	SavePlugin(ctx context.Context, p *store.PluginRecord, replace bool) error
	DeletePlugin(ctx context.Context, module string) error
}

// Bundle is a parsed plugin upload.
type Bundle struct {
	Manifest    *nodes.Manifest
	RawManifest []byte
	Wasm        []byte
	// Icons holds the icon files the manifest references (path -> data URI).
	Icons map[string]string
}

// resolveIcons validates the manifest's icons, reading icon files with read.
func (b *Bundle) resolveIcons(read nodes.IconFiles) error {
	icons, err := b.Manifest.ResolveIcons(read)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	b.Icons = icons
	return nil
}

// dirFiles reads icon files from a directory on disk.
func dirFiles(dir string) nodes.IconFiles {
	return func(name string) ([]byte, error) {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, nodes.MaxIconSize+1))
	}
}

// storedIcons serves icon files saved with an imported plugin.
func storedIcons(icons map[string]string) nodes.IconFiles {
	return func(name string) ([]byte, error) {
		uri, ok := icons[name]
		if !ok {
			return nil, fmt.Errorf("file not stored with the plugin")
		}
		_, payload, _ := strings.Cut(uri, ",")
		return base64.StdEncoding.DecodeString(payload)
	}
}

// ParseBundle accepts a YAML manifest (http runtime) or a .zip containing
// plugin.yaml (or manifest.yaml) plus the WebAssembly file it names and the
// icon files it references. A bare YAML upload can only have inline icons.
func ParseBundle(filename string, data []byte) (*Bundle, error) {
	return parseBundle(filename, data, nil)
}

// parseBundle is ParseBundle; files reads icon files next to a YAML manifest.
func parseBundle(filename string, data []byte, files nodes.IconFiles) (*Bundle, error) {
	if len(data) > maxBundleSize {
		return nil, fmt.Errorf("plugin exceeds %d MiB", maxBundleSize>>20)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".yaml" || ext == ".yml" {
		m, err := nodes.ParseManifest(data)
		if err != nil {
			return nil, err
		}
		if m.Runtime == "wasm" {
			return nil, errors.New("wasm plugins must be uploaded as a .zip containing the manifest and the .wasm file")
		}
		b := &Bundle{Manifest: m, RawManifest: data}
		if err := b.resolveIcons(files); err != nil {
			return nil, err
		}
		return b, nil
	}
	if ext != ".zip" {
		return nil, errors.New("upload a .yaml manifest or a .zip bundle")
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("zip has more than %d entries", maxZipEntries)
	}
	// Every name must be unambiguous: with two entries of the same name,
	// which one is used would depend on the reader.
	entries := map[string]*zip.File{}
	byPath := map[string]*zip.File{}
	ambiguous := map[string]bool{}
	for _, f := range zr.File {
		p := path.Clean(strings.ReplaceAll(f.Name, `\`, "/"))
		if byPath[p] != nil {
			return nil, fmt.Errorf("zip contains %s twice", p)
		}
		byPath[p] = f
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		// Bundles may be zipped with or without a top-level folder.
		base := path.Base(p)
		if entries[base] != nil {
			ambiguous[base] = true
		}
		entries[base] = f
	}
	for _, name := range []string{"plugin.yaml", "manifest.yaml"} {
		if ambiguous[name] {
			return nil, fmt.Errorf("zip contains more than one %s", name)
		}
	}
	if entries["plugin.yaml"] != nil && entries["manifest.yaml"] != nil {
		return nil, errors.New("zip contains both plugin.yaml and manifest.yaml")
	}
	mf := entries["plugin.yaml"]
	if mf == nil {
		mf = entries["manifest.yaml"]
	}
	if mf == nil {
		return nil, errors.New("zip must contain plugin.yaml")
	}
	raw, err := readZip(mf, maxManifestSize)
	if err != nil {
		return nil, err
	}
	m, err := nodes.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Manifest: m, RawManifest: raw}
	// Icon paths are relative to the manifest inside the zip.
	root := path.Dir(path.Clean(strings.ReplaceAll(mf.Name, `\`, "/")))
	err = b.resolveIcons(func(name string) ([]byte, error) {
		f := byPath[path.Join(root, name)]
		if f == nil {
			return nil, fmt.Errorf("not in the zip")
		}
		return readZip(f, nodes.MaxIconSize+1)
	})
	if err != nil {
		return nil, err
	}
	if m.Runtime == "wasm" {
		wf := entries[path.Base(m.Wasm)]
		if ambiguous[path.Base(m.Wasm)] {
			return nil, fmt.Errorf("zip contains more than one %s", path.Base(m.Wasm))
		}
		if wf == nil {
			return nil, fmt.Errorf("zip does not contain %s", m.Wasm)
		}
		if b.Wasm, err = readZip(wf, maxWasmSize); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func readZip(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%s is too large", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is too large", f.Name)
	}
	return data, nil
}

// Trust is the signature policy for imported plugins.
type Trust struct {
	// Keys are the publishers whose signatures are accepted.
	Keys signing.Keys
	// RequireSigned refuses (and stops loading) plugins without a valid
	// signature by one of Keys.
	RequireSigned bool
}

// ImportOptions are the optional parts of an import.
type ImportOptions struct {
	// Signature is the detached signature envelope (empty: unsigned).
	Signature []byte
	// Replace allows replacing an imported plugin of the same module.
	Replace bool
	// Module, if set, is the module the bundle must contain (catalog installs).
	Module string
}

// Summary identifies an installed or uploaded version of a plugin.
type Summary struct {
	Module  string `json:"module"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Signer  string `json:"signer,omitempty"`
	Blocked string `json:"blocked,omitempty"`
}

// ExistsError is returned when an import would replace an imported plugin
// without ImportOptions.Replace: plugins are shared by all organizations, so
// swapping one out must be a deliberate decision.
type ExistsError struct {
	Current  Summary `json:"current"`
	Incoming Summary `json:"incoming"`
}

func (e *ExistsError) Error() string {
	v := e.Current.Version
	if v == "" {
		v = "(no version)"
	}
	return fmt.Sprintf("plugin %q is already installed (version %s); confirm to replace it", e.Current.Module, v)
}

// Is makes errors.Is(err, ErrExists) work.
func (e *ExistsError) Is(target error) bool { return target == ErrExists }

// ErrExists matches *ExistsError.
var ErrExists = errors.New("plugin already installed")

// ErrUnsigned is returned for unsigned plugins when signatures are required.
var ErrUnsigned = errors.New("plugin is not signed, and this installation only accepts plugins signed by a trusted key (G2F_PLUGIN_REQUIRE_SIGNED)")

// Manager keeps the registry in sync with installed plugins.
type Manager struct {
	reg   *nodes.Registry
	store Store
	log   *slog.Logger
	trust Trust

	importMu sync.Mutex // serializes imports on this replica

	mu       sync.Mutex
	files    map[string]bool              // modules loaded from the modules directory
	imported map[string]string            // module -> digest of the stored row last processed
	blocked  map[string]*nodes.ModuleInfo // stored plugins that are not loaded, and why
	wasm     map[string]*wasmModule       // module -> compiled wasm
}

func NewManager(reg *nodes.Registry, st Store, log *slog.Logger) *Manager {
	return &Manager{
		reg: reg, store: st, log: log.With("component", "plugins"),
		files: map[string]bool{}, imported: map[string]string{}, blocked: map[string]*nodes.ModuleInfo{},
		wasm: map[string]*wasmModule{},
	}
}

// SetTrust sets the signature policy. Call it before LoadDir and Sync.
func (m *Manager) SetTrust(t Trust) { m.trust = t }

// RequireSigned reports whether only signed plugins are accepted.
func (m *Manager) RequireSigned() bool { return m.trust.RequireSigned }

// TrustsKey reports whether keyID is a trusted publisher key.
func (m *Manager) TrustsKey(keyID string) bool { _, ok := m.trust.Keys[keyID]; return ok }

// LoadDir installs plugins from dir: *.yaml manifests, *.zip bundles and
// sub-directories containing plugin.yaml (+ wasm file). They are trusted
// because the operator placed them there, so signatures are not checked.
func (m *Manager) LoadDir(ctx context.Context, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		var b *Bundle
		switch {
		case e.IsDir():
			b, err = bundleFromDir(path)
			if b == nil && err == nil {
				continue
			}
		case strings.HasSuffix(e.Name(), ".yaml"), strings.HasSuffix(e.Name(), ".yml"), strings.HasSuffix(e.Name(), ".zip"):
			var data []byte
			if data, err = os.ReadFile(path); err == nil {
				if strings.HasSuffix(e.Name(), ".zip") {
					b, err = ParseBundle(e.Name(), data)
				} else {
					b, err = parseBundle(e.Name(), data, dirFiles(dir))
				}
			}
		default:
			continue
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		p, err := m.prepare(ctx, b, "file:"+e.Name())
		if err == nil {
			err = m.activate(p)
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		m.mu.Lock()
		m.files[b.Manifest.Module] = true
		m.mu.Unlock()
		count++
	}
	return count, nil
}

func bundleFromDir(dir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	mf, err := nodes.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Manifest: mf, RawManifest: raw}
	if err := b.resolveIcons(dirFiles(dir)); err != nil {
		return nil, err
	}
	if mf.Runtime == "wasm" {
		if b.Wasm, err = os.ReadFile(filepath.Join(dir, filepath.Base(mf.Wasm))); err != nil {
			return nil, fmt.Errorf("read wasm (build the plugin first?): %w", err)
		}
	}
	return b, nil
}

// prepared is a built plugin that is not registered yet.
type prepared struct {
	info nodes.ModuleInfo
	list []nodes.Node
	mod  *wasmModule
}

func (p *prepared) discard() {
	if p.mod != nil {
		p.mod.close()
	}
}

// prepare builds (and for wasm compiles) the nodes of b without touching
// the registry.
func (m *Manager) prepare(ctx context.Context, b *Bundle, source string) (*prepared, error) {
	mf := b.Manifest
	p := &prepared{info: mf.Info(source)}
	var err error
	switch mf.Runtime {
	case "http":
		p.list, err = nodes.HTTPModuleNodes(mf, source)
	case "go":
		p.list, err = nodes.NativeModuleNodes(mf, source)
	case "wasm":
		if p.mod, err = compileWasm(ctx, mf, b.Wasm, m.log); err == nil {
			p.list = wasmNodes(mf, p.mod, source)
		}
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// activate registers a prepared plugin, replacing an older version.
func (m *Manager) activate(p *prepared) error {
	module := p.info.Module
	if err := m.reg.RegisterModule(p.info, p.list); err != nil {
		p.discard()
		return err
	}
	m.mu.Lock()
	old := m.wasm[module]
	if p.mod != nil {
		m.wasm[module] = p.mod
	} else {
		delete(m.wasm, module)
	}
	delete(m.blocked, module)
	m.mu.Unlock()
	m.retire(old)
	return nil
}

// retire closes a replaced wasm module once running executions are done with it.
func (m *Manager) retire(mod *wasmModule) {
	if mod != nil {
		time.AfterFunc(15*time.Minute, mod.close)
	}
}

// verify applies the signature policy to an uploaded or stored bundle and
// returns the signer ("" for unsigned) and the normalized envelope. A
// signature that is present but does not verify is always an error, also
// when unsigned plugins are allowed: it means the bundle or its signature was
// changed, or the publisher's key is no longer trusted.
func (m *Manager) verify(filename string, data, sig []byte) (signer, envelope string, err error) {
	if len(bytes.TrimSpace(sig)) == 0 {
		if m.trust.RequireSigned {
			return "", "", ErrUnsigned
		}
		return "", "", nil
	}
	s, err := signing.Parse(sig)
	if err != nil {
		return "", "", err
	}
	if signer, err = m.trust.Keys.Verify(filename, data, s); err != nil {
		if errors.Is(err, signing.ErrUnknownKey) {
			err = fmt.Errorf("%w; add it to G2F_PLUGIN_TRUSTED_KEYS if you trust its publisher", err)
		}
		return "", "", err
	}
	return signer, string(s.Marshal()), nil
}

// Import verifies, stores and installs an uploaded plugin. The plugin is
// saved to the database first and only registered when that succeeded, so a
// failed import changes nothing on any replica.
func (m *Manager) Import(ctx context.Context, filename string, data []byte, by string, opt ImportOptions) (*nodes.ModuleInfo, error) {
	if len(data) > maxBundleSize {
		return nil, fmt.Errorf("plugin exceeds %d MiB", maxBundleSize>>20)
	}
	if _, err := signing.Kind(filename); err != nil {
		return nil, errors.New("upload a .yaml manifest or a .zip bundle")
	}
	// The signature is checked on the raw bytes before anything is parsed.
	signer, envelope, err := m.verify(filename, data, opt.Signature)
	if err != nil {
		return nil, err
	}
	b, err := ParseBundle(filename, data)
	if err != nil {
		return nil, err
	}
	if b.Manifest.Runtime == "go" {
		return nil, errors.New("go modules are compiled into glue2flow and load from the modules directory; they cannot be imported")
	}
	module := b.Manifest.Module
	if opt.Module != "" && module != opt.Module {
		return nil, fmt.Errorf("bundle contains module %q, expected %q", module, opt.Module)
	}
	incoming := Summary{Module: module, Name: b.Manifest.Name, Version: b.Manifest.Version, Signer: signer}

	m.importMu.Lock()
	defer m.importMu.Unlock()
	m.mu.Lock()
	isFile := m.files[module]
	_, known := m.imported[module]
	m.mu.Unlock()
	if isFile {
		return nil, ErrConflict
	}
	if known && !opt.Replace {
		return nil, &ExistsError{Current: m.summary(module), Incoming: incoming}
	}

	p, err := m.prepare(ctx, b, "imported")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	rec := &store.PluginRecord{
		Module: module, Version: b.Manifest.Version, Manifest: b.RawManifest,
		Bundle: data, Filename: path.Base(strings.ReplaceAll(filename, `\`, "/")),
		Signature: envelope, Signer: signer, SHA256: hex.EncodeToString(sum[:]), InstalledBy: by,
	}
	if err := m.store.SavePlugin(ctx, rec, opt.Replace); err != nil {
		p.discard()
		if errors.Is(err, store.ErrPluginExists) {
			// Imported on another replica in the meantime.
			return nil, &ExistsError{Current: m.summary(module), Incoming: incoming}
		}
		return nil, err
	}
	p.info.InstalledBy, p.info.InstalledAt, p.info.Signer = by, &rec.InstalledAt, signer
	if err := m.activate(p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.imported[module] = rec.SHA256
	m.mu.Unlock()
	info, _ := m.reg.Module(module)
	signedBy := signer
	if signedBy == "" {
		signedBy = "(unsigned)"
	}
	m.log.Info("plugin imported", "module", module, "version", rec.Version, "by", by, "signer", signedBy, "replace", opt.Replace)
	return info, nil
}

// summary describes the installed (or blocked) imported version of module.
func (m *Manager) summary(module string) Summary {
	m.mu.Lock()
	b := m.blocked[module]
	m.mu.Unlock()
	if b != nil {
		return Summary{Module: module, Name: b.Name, Version: b.Version, Signer: b.Signer, Blocked: b.Blocked}
	}
	if info, ok := m.reg.Module(module); ok {
		return Summary{Module: module, Name: info.Name, Version: info.Version, Signer: info.Signer}
	}
	return Summary{Module: module, Name: module}
}

// Installed returns the imported version of module, if any.
func (m *Manager) Installed(module string) (Summary, bool) {
	m.mu.Lock()
	_, ok := m.imported[module]
	m.mu.Unlock()
	if !ok {
		return Summary{}, false
	}
	return m.summary(module), true
}

// Modules lists the registered modules plus stored plugins that are blocked.
func (m *Manager) Modules() []nodes.ModuleInfo {
	out := m.reg.Modules()
	m.mu.Lock()
	for _, b := range m.blocked {
		out = append(out, *b)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}

// Delete uninstalls an imported plugin.
func (m *Manager) Delete(ctx context.Context, module string) error {
	m.mu.Lock()
	isFile := m.files[module]
	m.mu.Unlock()
	if isFile || module == nodes.BuiltinModule {
		return ErrReadOnly
	}
	if err := m.store.DeletePlugin(ctx, module); err != nil {
		return err
	}
	m.uninstall(module)
	m.log.Info("plugin deleted", "module", module)
	return nil
}

func (m *Manager) uninstall(module string) {
	m.reg.UnregisterModule(module)
	m.mu.Lock()
	mod := m.wasm[module]
	delete(m.wasm, module)
	delete(m.imported, module)
	delete(m.blocked, module)
	m.mu.Unlock()
	m.retire(mod)
}

// IsFileModule reports whether a module comes from the modules directory.
func (m *Manager) IsFileModule(module string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[module]
}

// Sync installs, updates and removes imported plugins to match the database.
// Running it periodically propagates imports between replicas. Every stored
// bundle is verified again before it is loaded, so a row changed in the
// database, or one that the current policy no longer accepts, is blocked.
func (m *Manager) Sync(ctx context.Context) error {
	digests, err := m.store.PluginDigests(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	changed := false
	for mod, d := range digests {
		if m.imported[mod] != d && !m.files[mod] {
			changed = true
		}
	}
	var removed []string
	for mod := range m.imported {
		if _, ok := digests[mod]; !ok {
			removed = append(removed, mod)
		}
	}
	m.mu.Unlock()
	for _, mod := range removed {
		m.uninstall(mod)
	}
	if !changed {
		return nil
	}

	recs, err := m.store.ListPlugins(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		m.mu.Lock()
		isFile := m.files[rec.Module]
		skip := m.imported[rec.Module] == rec.SHA256 || isFile
		m.mu.Unlock()
		if skip {
			if isFile {
				m.log.Warn("imported plugin shadowed by modules directory", "module", rec.Module)
			}
			continue
		}
		if err := m.load(ctx, rec); err != nil {
			m.block(rec, err.Error())
		}
		m.mu.Lock()
		m.imported[rec.Module] = rec.SHA256
		m.mu.Unlock()
	}
	return nil
}

// load verifies and installs a stored plugin.
func (m *Manager) load(ctx context.Context, rec *store.PluginRecord) error {
	if rec.Bundle == nil {
		return errors.New("stored without its original bundle (imported before plugin signing); import it again")
	}
	if sum := sha256.Sum256(rec.Bundle); hex.EncodeToString(sum[:]) != rec.SHA256 {
		return errors.New("stored bundle does not match its digest")
	}
	signer, _, err := m.verify(rec.Filename, rec.Bundle, []byte(rec.Signature))
	if err != nil {
		return err
	}
	b, err := ParseBundle(rec.Filename, rec.Bundle)
	if err != nil {
		return err
	}
	if b.Manifest.Module != rec.Module {
		return fmt.Errorf("stored bundle contains module %q", b.Manifest.Module)
	}
	p, err := m.prepare(ctx, b, "imported")
	if err != nil {
		return err
	}
	at := rec.InstalledAt
	p.info.InstalledBy, p.info.InstalledAt, p.info.Signer = rec.InstalledBy, &at, signer
	return m.activate(p)
}

// block unloads a stored plugin and lists it with the reason.
func (m *Manager) block(rec *store.PluginRecord, reason string) {
	m.reg.UnregisterModule(rec.Module)
	info := nodes.ModuleInfo{Module: rec.Module, Name: rec.Module, Version: rec.Version, Runtime: "http", Source: "imported"}
	// The manifest is only read for display; it is not trusted here.
	if mf, err := nodes.ParseManifest(rec.Manifest); err == nil && mf.Module == rec.Module {
		info = mf.Info("imported")
	}
	at := rec.InstalledAt
	info.InstalledBy, info.InstalledAt, info.Blocked = rec.InstalledBy, &at, reason
	info.Config, info.Vars, info.Nodes, info.Icon = []nodes.Param{}, []nodes.Param{}, []string{}, ""
	m.mu.Lock()
	old := m.wasm[rec.Module]
	delete(m.wasm, rec.Module)
	m.blocked[rec.Module] = &info
	m.mu.Unlock()
	m.retire(old)
	m.log.Error("plugin blocked, not loading it", "module", rec.Module, "version", rec.Version, "reason", reason)
}

// Run syncs periodically until ctx is done.
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Sync(ctx); err != nil {
				m.log.Error("plugin sync", "err", err)
			}
		}
	}
}
