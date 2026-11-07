package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/plugin"
	"github.com/wolfposd/g8g/internal/store"
)

const maxUpload = 50 << 20

// listPlugins lists installed modules (built-in, file-based and imported).
func (s *Server) listPlugins(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.Modules())
}

// importPlugin installs a plugin from a multipart upload (field "file"):
// a .yaml manifest or a .zip bundle with plugin.yaml and a .wasm module.
// Plugins are installation-wide, so this requires the plugin admin role.
func (s *Server) importPlugin(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if !u.PluginAdmin {
		writeError(w, http.StatusForbidden, "requires the plugin admin role")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart upload with field 'file': "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := s.plugins.Import(r.Context(), hdr.Filename, data, u.Name)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, plugin.ErrConflict) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) deletePlugin(w http.ResponseWriter, r *http.Request) {
	if !auth.FromContext(r.Context()).PluginAdmin {
		writeError(w, http.StatusForbidden, "requires the plugin admin role")
		return
	}
	err := s.plugins.Delete(r.Context(), chi.URLParam(r, "module"))
	switch {
	case errors.Is(err, plugin.ErrReadOnly):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "plugin not found")
	case err != nil:
		s.storeError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// getPluginConfig shows the organization's global config of a module. Secret
// values are never returned, only whether they are set. Org admins only.
func (s *Server) getPluginConfig(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if !u.Admin {
		writeError(w, http.StatusForbidden, "requires the organization admin role")
		return
	}
	module := chi.URLParam(r, "module")
	if _, ok := s.reg.Module(module); !ok {
		writeError(w, http.StatusNotFound, "plugin not found")
		return
	}
	values, err := s.secrets.ConfigStatus(r.Context(), u.Org, module)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, values)
}

// putPluginConfig updates config values: {"values": {"key": "value"}}.
// An empty value deletes a key, {"$secret": "stored"} keeps a secret unchanged.
func (s *Server) putPluginConfig(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if !u.Admin {
		writeError(w, http.StatusForbidden, "requires the organization admin role")
		return
	}
	module := chi.URLParam(r, "module")
	if _, ok := s.reg.Module(module); !ok {
		writeError(w, http.StatusNotFound, "plugin not found")
		return
	}
	var in struct {
		Values map[string]any `json:"values"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.secrets.UpdateConfig(r.Context(), u.Org, module, u.Name, in.Values); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.getPluginConfig(w, r)
}
