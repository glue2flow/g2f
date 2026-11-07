// Package plugin installs node modules ("plugins") from YAML manifests and
// WebAssembly bundles, either from the modules directory or imported at runtime.
package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
)

const (
	maxBundleSize   = 50 << 20 // 50 MiB upload
	maxManifestSize = 1 << 20
	maxWasmSize     = 40 << 20
)

var (
	ErrReadOnly = errors.New("plugin is installed from the modules directory and cannot be changed through the API")
	ErrConflict = errors.New("module name conflicts with a built-in or file-based module")
)

// Store is the persistence the manager needs.
type Store interface {
	ListPlugins(ctx context.Context) ([]*store.PluginRecord, error)
	PluginDigests(ctx context.Context) (map[string]string, error)
	SavePlugin(ctx context.Context, p *store.PluginRecord) error
	DeletePlugin(ctx context.Context, module string) error
}

// Bundle is a parsed plugin upload.
type Bundle struct {
	Manifest    *nodes.Manifest
	RawManifest []byte
	Wasm        []byte
}

func (b *Bundle) digest() string {
	h := sha256.New()
	h.Write(b.RawManifest)
	h.Write([]byte{0})
	h.Write(b.Wasm)
	return hex.EncodeToString(h.Sum(nil))
}

// ParseBundle accepts a YAML manifest (http runtime) or a .zip containing
// plugin.yaml (or manifest.yaml) plus the WebAssembly file it names.
func ParseBundle(filename string, data []byte) (*Bundle, error) {
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
		return &Bundle{Manifest: m, RawManifest: data}, nil
	}
	if ext != ".zip" {
		return nil, errors.New("upload a .yaml manifest or a .zip bundle")
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		// Bundles may be zipped with or without a top-level folder.
		files[filepath.Base(f.Name)] = f
	}
	mf := files["plugin.yaml"]
	if mf == nil {
		mf = files["manifest.yaml"]
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
	if m.Runtime == "wasm" {
		wf := files[filepath.Base(m.Wasm)]
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

// Manager keeps the registry in sync with installed plugins.
type Manager struct {
	reg   *nodes.Registry
	store Store
	log   *slog.Logger

	mu       sync.Mutex
	files    map[string]bool        // modules loaded from the modules directory
	imported map[string]string      // module -> digest of imported plugins
	wasm     map[string]*wasmModule // module -> compiled wasm
}

func NewManager(reg *nodes.Registry, st Store, log *slog.Logger) *Manager {
	return &Manager{
		reg: reg, store: st, log: log.With("component", "plugins"),
		files: map[string]bool{}, imported: map[string]string{}, wasm: map[string]*wasmModule{},
	}
}

// LoadDir installs plugins from dir: *.yaml manifests, *.zip bundles and
// sub-directories containing plugin.yaml (+ wasm file).
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
				b, err = ParseBundle(e.Name(), data)
			}
		default:
			continue
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		if err := m.install(ctx, b, "file:"+e.Name(), "", nil); err != nil {
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
	if mf.Runtime == "wasm" {
		if b.Wasm, err = os.ReadFile(filepath.Join(dir, filepath.Base(mf.Wasm))); err != nil {
			return nil, fmt.Errorf("read wasm (build the plugin first?): %w", err)
		}
	}
	return b, nil
}

// install builds the nodes of b and registers them, replacing an older version.
func (m *Manager) install(ctx context.Context, b *Bundle, source, by string, at *time.Time) error {
	mf := b.Manifest
	var list []nodes.Node
	var mod *wasmModule
	var err error
	switch mf.Runtime {
	case "http":
		list, err = nodes.HTTPModuleNodes(mf, source)
	case "wasm":
		if mod, err = compileWasm(ctx, mf, b.Wasm, m.log); err == nil {
			list = wasmNodes(mf, mod, source)
		}
	}
	if err != nil {
		return err
	}
	info := mf.Info(source)
	info.InstalledBy, info.InstalledAt = by, at
	if err := m.reg.RegisterModule(info, list); err != nil {
		if mod != nil {
			mod.close()
		}
		return err
	}
	m.mu.Lock()
	old := m.wasm[mf.Module]
	if mod != nil {
		m.wasm[mf.Module] = mod
	} else {
		delete(m.wasm, mf.Module)
	}
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

// Import validates, stores and installs an uploaded plugin.
func (m *Manager) Import(ctx context.Context, filename string, data []byte, by string) (*nodes.ModuleInfo, error) {
	b, err := ParseBundle(filename, data)
	if err != nil {
		return nil, err
	}
	module := b.Manifest.Module
	m.mu.Lock()
	isFile := m.files[module]
	m.mu.Unlock()
	if isFile {
		return nil, ErrConflict
	}
	now := time.Now().UTC()
	if err := m.install(ctx, b, "imported", by, &now); err != nil {
		return nil, err
	}
	rec := &store.PluginRecord{
		Module: module, Version: b.Manifest.Version, Manifest: b.RawManifest,
		Wasm: b.Wasm, SHA256: b.digest(), InstalledBy: by,
	}
	if err := m.store.SavePlugin(ctx, rec); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.imported[module] = rec.SHA256
	m.mu.Unlock()
	info, _ := m.reg.Module(module)
	m.log.Info("plugin imported", "module", module, "version", rec.Version, "by", by)
	return info, nil
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
// Running it periodically propagates imports between replicas.
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
		skip := m.imported[rec.Module] == rec.SHA256 || m.files[rec.Module]
		m.mu.Unlock()
		if skip {
			if m.files[rec.Module] {
				m.log.Warn("imported plugin shadowed by modules directory", "module", rec.Module)
			}
			continue
		}
		mf, err := nodes.ParseManifest(rec.Manifest)
		if err == nil {
			at := rec.InstalledAt
			err = m.install(ctx, &Bundle{Manifest: mf, RawManifest: rec.Manifest, Wasm: rec.Wasm}, "imported", rec.InstalledBy, &at)
		}
		if err != nil {
			m.log.Error("install plugin", "module", rec.Module, "err", err)
			continue
		}
		m.mu.Lock()
		m.imported[rec.Module] = rec.SHA256
		m.mu.Unlock()
	}
	return nil
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
