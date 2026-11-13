package api

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/oauth"
	"github.com/glue2flow/g2f/internal/store"
)

type connectionView struct {
	*store.Connection
	CanManage bool `json:"canManage"`
}

// listConnections returns the connected accounts the caller may use,
// optionally filtered by ?module=.
func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	all, err := s.store.ListConnections(r.Context(), u.Org)
	if err != nil {
		s.storeError(w, err)
		return
	}
	module := r.URL.Query().Get("module")
	out := []connectionView{}
	for _, c := range all {
		if (module == "" || c.Module == module) && c.UsableBy(u) {
			out = append(out, connectionView{c, c.ManageableBy(u)})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// authorizeConnection starts the OAuth2 flow: {"module": "...", "name": "..."} -> {"url": "..."}.
func (s *Server) authorizeConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Module string `json:"module"`
		Name   string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	url, nonce, err := s.oauth.AuthorizeURL(r.Context(), auth.FromContext(r.Context()), in.Module, in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setOAuthCookie(w, nonce, int(oauth.StateTTL/time.Second))
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// setOAuthCookie binds the OAuth flow to this browser; maxAge < 0 clears it.
func (s *Server) setOAuthCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: oauth.CookieName, Value: value, Path: "/oauth/callback", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"),
	})
}

func (s *Server) loadManagedConnection(w http.ResponseWriter, r *http.Request) (*store.Connection, bool) {
	u := auth.FromContext(r.Context())
	c, err := s.store.GetConnection(r.Context(), chi.URLParam(r, "id"))
	if err != nil || !c.UsableBy(u) {
		writeError(w, http.StatusNotFound, "connection not found")
		return nil, false
	}
	if !c.ManageableBy(u) {
		writeError(w, http.StatusForbidden, "only the owner can change this connection")
		return nil, false
	}
	return c, true
}

// updateConnection renames or re-shares: {"name", "scope", "groups"}.
func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadManagedConnection(w, r)
	if !ok {
		return
	}
	var in struct {
		Name   *string  `json:"name"`
		Scope  *string  `json:"scope"`
		Groups []string `json:"groups"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if in.Scope != nil {
		switch *in.Scope {
		case "private", "group", "org":
			c.Scope = *in.Scope
		default:
			writeError(w, http.StatusBadRequest, "scope must be private, group or org")
			return
		}
	}
	if in.Groups != nil {
		c.Groups = []string{}
		for _, g := range in.Groups {
			if g = auth.NormalizeGroup(g); g != "" {
				c.Groups = append(c.Groups, g)
			}
		}
	}
	if err := s.store.UpdateConnectionMeta(r.Context(), c); err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, connectionView{c, true})
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadManagedConnection(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteConnection(r.Context(), c.ID); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var callbackPage = template.Must(template.New("cb").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>glue2flow – {{if .OK}}Account connected{{else}}Connection failed{{end}}</title>
<style>
 body{font-family:system-ui,sans-serif;background:#f7f6f4;color:#1f1a14;display:grid;place-items:center;min-height:100vh;margin:0;padding:16px}
 .card{background:#fff;border:1px solid #e8e4de;border-radius:12px;padding:24px 28px;max-width:420px;text-align:center}
 h1{font-size:18px;margin:12px 0 6px} p{color:#6f675d;margin:0}
 img{width:48px;height:48px;border-radius:10px}
 @media (prefers-color-scheme: dark){body{background:#121110;color:#ece8e3}.card{background:#1b1917;border-color:#302c27}p{color:#a59c91}}
</style></head><body><div class="card">
<img src="/appicon-64.png" alt="">
<h1>{{if .OK}}Account connected{{else}}Connection failed{{end}}</h1>
<p>{{if .OK}}{{.Name}}{{if .Account}} ({{.Account}}){{end}} is ready to use. You can close this window.{{else}}{{.Error}}{{end}}</p>
</div>
<script>
 (function(){
  var msg = {type: "g2f-oauth", ok: {{.OK}}, connectionId: {{.ID}}, error: {{.Error}}};
  if (window.opener) { window.opener.postMessage(msg, {{.Origin}}); {{if .OK}}setTimeout(function(){ window.close(); }, 1200);{{end}} }
 })();
</script></body></html>`))

// oauthCallback receives the provider redirect. It is not behind the API auth:
// the encrypted state binds the flow to the user who started it, and the
// nonce cookie to the browser that started it.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	var nonce string
	if ck, err := r.Cookie(oauth.CookieName); err == nil {
		nonce = ck.Value
	}
	s.setOAuthCookie(w, "", -1)
	q := r.URL.Query()
	data := map[string]any{"OK": false, "ID": "", "Name": "", "Account": "", "Error": "", "Origin": s.cfg.PublicURL}
	var err error
	if e := q.Get("error"); e != "" {
		err = fmt.Errorf("the provider reported: %s %s", e, q.Get("error_description"))
	} else if q.Get("code") == "" || q.Get("state") == "" {
		err = errors.New("missing code or state")
	} else {
		var c *store.Connection
		if c, err = s.oauth.Callback(r.Context(), q.Get("code"), q.Get("state"), nonce); err == nil {
			data["OK"], data["ID"], data["Name"], data["Account"] = true, c.ID, c.Name, c.Account
		}
	}
	if err != nil {
		s.log.Warn("oauth callback failed", "err", err)
		data["Error"] = err.Error()
		w.WriteHeader(http.StatusBadRequest)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = callbackPage.Execute(w, data)
}
