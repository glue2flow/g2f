package nodes

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// MaxIconSize limits plugin icons (raw file size).
const MaxIconSize = 32 << 10

// IconFiles reads a file referenced by a manifest's icon field, relative to
// the manifest (plugin folder, zip bundle or modules directory). nil means
// that only inline icons are possible.
type IconFiles func(name string) ([]byte, error)

// ResolveIcons validates the icons of the module and its nodes and turns them
// into data URIs (Manifest.IconURI, ManifestNode.IconURI). It returns the
// data URI of every referenced file by path, so imported plugins can store
// them next to the manifest.
func (m *Manifest) ResolveIcons(read IconFiles) (map[string]string, error) {
	files := map[string]string{}
	resolve := func(ref, where string) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return "", nil
		}
		if strings.HasPrefix(ref, "<") || strings.HasPrefix(ref, "data:") {
			uri, err := inlineIcon(ref)
			if err != nil {
				return "", fmt.Errorf("%s icon: %w", where, err)
			}
			return uri, nil
		}
		name, err := iconPath(ref)
		if err != nil {
			return "", fmt.Errorf("%s icon: %w", where, err)
		}
		if uri, ok := files[name]; ok {
			return uri, nil
		}
		if read == nil {
			return "", fmt.Errorf("%s icon %q: upload a .zip bundle that contains the file, or put the SVG inline", where, ref)
		}
		data, err := read(name)
		if err != nil {
			return "", fmt.Errorf("%s icon %q: %w", where, ref, err)
		}
		uri, err := IconDataURI(data)
		if err != nil {
			return "", fmt.Errorf("%s icon %q: %w", where, ref, err)
		}
		files[name] = uri
		return uri, nil
	}
	var err error
	if m.IconURI, err = resolve(m.Icon, "module"); err != nil {
		return nil, err
	}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		if n.IconURI, err = resolve(n.Icon, "node "+n.Type); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// iconPath checks a relative file reference: no absolute paths, no "..".
func iconPath(ref string) (string, error) {
	if strings.Contains(ref, `\`) || path.IsAbs(ref) {
		return "", fmt.Errorf("%q must be a relative path", ref)
	}
	clean := path.Clean(ref)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q must not leave the plugin directory", ref)
	}
	return clean, nil
}

func inlineIcon(ref string) (string, error) {
	if strings.HasPrefix(ref, "data:") {
		meta, payload, ok := strings.Cut(strings.TrimPrefix(ref, "data:"), ",")
		if !ok || !strings.HasSuffix(meta, ";base64") {
			return "", errors.New("data URIs must be base64 encoded")
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", fmt.Errorf("invalid base64: %w", err)
		}
		return IconDataURI(data)
	}
	return IconDataURI([]byte(ref))
}

var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	svgEvents = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	svgHrefs  = regexp.MustCompile(`(?i)(?:xlink:)?href\s*=\s*["']\s*([^"'\s]*)`)
	svgURLs   = regexp.MustCompile(`(?i)url\(\s*['"]?\s*([^)'"\s]*)`)
	svgBanned = []string{"<script", "<foreignobject", "<iframe", "<embed", "<object", "<!entity", "<!doctype",
		"javascript:", "@import"}
)

// IconDataURI validates an icon file (SVG, PNG or WebP, detected from its
// content, at most MaxIconSize) and returns it as a base64 data URI.
//
// The editor only shows icons with <img>, where browsers never run scripts,
// but SVGs are checked anyway: no scripts, event handlers, embedded documents,
// style sheets or references to anything outside the file.
func IconDataURI(data []byte) (string, error) {
	if len(data) > MaxIconSize {
		return "", fmt.Errorf("icon is %d KB, at most %d KB are allowed", (len(data)+1023)>>10, MaxIconSize>>10)
	}
	var typ string
	switch {
	case bytes.HasPrefix(data, pngMagic):
		typ = "image/png"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		typ = "image/webp"
	default:
		s := strings.ToLower(string(data))
		if !strings.Contains(s, "<svg") {
			return "", errors.New("icon must be an SVG, PNG or WebP image")
		}
		for _, b := range svgBanned {
			if strings.Contains(s, b) {
				return "", fmt.Errorf("SVG icons must not contain %s", b)
			}
		}
		if svgEvents.MatchString(s) {
			return "", errors.New("SVG icons must not contain event handlers")
		}
		for _, re := range []*regexp.Regexp{svgHrefs, svgURLs} {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				if !strings.HasPrefix(m[1], "#") {
					return "", fmt.Errorf("SVG icons may only reference parts of themselves, not %q", m[1])
				}
			}
		}
		typ = "image/svg+xml"
	}
	return "data:" + typ + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
