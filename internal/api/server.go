// Package api exposes the REST API, webhook endpoints and the editor SPA.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/config"
	"github.com/wolfposd/g8g/internal/engine"
	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/plugin"
	"github.com/wolfposd/g8g/internal/secrets"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

// ChangeListener is notified when workflows are saved or deleted, so that
// schedules and webhooks can be (re)registered.
type ChangeListener interface {
	WorkflowChanged(w *workflow.Workflow)
	WorkflowDeleted(id string)
}

type Server struct {
	cfg      *config.Config
	store    *store.Store
	reg      *nodes.Registry
	engine   *engine.Engine
	auth     *auth.Authenticator
	log      *slog.Logger
	listener ChangeListener
	webhooks http.Handler
	secrets  *secrets.Service
	plugins  *plugin.Manager
}

type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Registry *nodes.Registry
	Engine   *engine.Engine
	Auth     *auth.Authenticator
	Logger   *slog.Logger
	// Listener is optional.
	Listener ChangeListener
	// Webhooks handles /webhook/*; optional.
	Webhooks http.Handler
	Secrets  *secrets.Service
	Plugins  *plugin.Manager
}

func New(d Deps) *Server {
	return &Server{
		cfg: d.Config, store: d.Store, reg: d.Registry, engine: d.Engine,
		auth: d.Auth, log: d.Logger, listener: d.Listener, webhooks: d.Webhooks,
		secrets: d.Secrets, plugins: d.Plugins,
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, s.requestLogger, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Pool().Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api", func(r chi.Router) {
		r.Use(middleware.NoCache)
		r.Get("/config", s.getConfig)
		r.Group(func(r chi.Router) {
			r.Use(s.auth.Middleware)
			r.Get("/me", s.getMe)
			r.Get("/nodes", s.listNodes)
			r.Get("/workflows", s.listWorkflows)
			r.Post("/workflows", s.createWorkflow)
			r.Get("/workflows/{id}", s.getWorkflow)
			r.Put("/workflows/{id}", s.updateWorkflow)
			r.Delete("/workflows/{id}", s.deleteWorkflow)
			r.Post("/workflows/{id}/run", s.runWorkflow)
			r.Get("/workflows/{id}/executions", s.listExecutions)
			r.Get("/workflows/{id}/triggers", s.listTriggers)
			r.Get("/executions/{id}", s.getExecution)

			r.Get("/plugins", s.listPlugins)
			r.Post("/plugins", s.importPlugin)
			r.Delete("/plugins/{module}", s.deletePlugin)
			r.Get("/plugins/{module}/config", s.getPluginConfig)
			r.Put("/plugins/{module}/config", s.putPluginConfig)
		})
	})

	if s.webhooks != nil {
		r.Handle("/webhook/*", s.webhooks)
	}

	r.NotFound(s.spa)
	return r
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhook/") {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"duration", time.Since(start).Round(time.Millisecond))
		}
	})
}

// spa serves the built editor and falls back to index.html for client routes.
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	dir := s.cfg.StaticDir
	path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if strings.Contains(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, path)
		return
	}
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		http.Error(w, "editor not built: run `npm run build` in web/ or set G8G_STATIC_DIR", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

type clientConfig struct {
	AuthMode    config.AuthMode `json:"authMode"`
	KeycloakURL string          `json:"keycloakUrl,omitempty"`
	Realm       string          `json:"realm,omitempty"`
	ClientID    string          `json:"clientId,omitempty"`
	PublicURL   string          `json:"publicUrl"`
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	c := clientConfig{AuthMode: s.cfg.Auth.Mode, PublicURL: s.cfg.PublicURL}
	if s.cfg.Auth.Mode == config.AuthOIDC {
		// Keycloak issuers look like https://host/realms/<realm>.
		if base, realm, ok := strings.Cut(s.cfg.Auth.Issuer, "/realms/"); ok {
			c.KeycloakURL, c.Realm = base, realm
		}
		c.ClientID = s.cfg.Auth.ClientID
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, auth.FromContext(r.Context()))
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.Definitions())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "workflow was modified by someone else; reload and try again")
	default:
		s.log.Error("store", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 5<<20))
	return dec.Decode(v)
}
