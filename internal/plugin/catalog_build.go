package plugin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/glue2flow/g2f/internal/plugin/signing"
)

// BuildCatalog writes the index of the bundles (*.zip, *.yaml, *.yml) in dir.
// baseURL is where the files will be served ("" for URLs relative to the
// index). With a key every bundle is signed; otherwise an existing
// "<bundle>.sig" next to a bundle is included.
func BuildCatalog(dir, name, baseURL, keyID string, key ed25519.PrivateKey) (*CatalogIndex, error) {
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("--base-url %q is not an absolute URL", baseURL)
		}
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	idx := &CatalogIndex{Name: name, Plugins: []CatalogEntry{}}
	seen := map[string]string{}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if _, err := signing.Kind(f.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		b, err := ParseBundle(f.Name(), data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name(), err)
		}
		mf := b.Manifest
		if other, dup := seen[mf.Module]; dup {
			return nil, fmt.Errorf("%s and %s both contain module %q", other, f.Name(), mf.Module)
		}
		seen[mf.Module] = f.Name()
		sum := sha256.Sum256(data)
		e := CatalogEntry{
			Module: mf.Module, Name: mf.Name, Version: mf.Version, Description: mf.Description,
			Runtime: mf.Runtime, URL: baseURL + url.PathEscape(f.Name()), SHA256: hex.EncodeToString(sum[:]),
		}
		if key != nil {
			if e.Signature, err = signing.Sign(f.Name(), data, keyID, key); err != nil {
				return nil, err
			}
		} else if raw, err := os.ReadFile(filepath.Join(dir, f.Name()+".sig")); err == nil {
			if e.Signature, err = signing.Parse(raw); err != nil {
				return nil, fmt.Errorf("%s.sig: %w", f.Name(), err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		idx.Plugins = append(idx.Plugins, e)
	}
	sort.Slice(idx.Plugins, func(i, j int) bool { return idx.Plugins[i].Module < idx.Plugins[j].Module })
	return idx, nil
}
