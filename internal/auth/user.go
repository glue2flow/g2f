// Package auth validates Keycloak (OIDC) access tokens and exposes the caller's
// identity, groups and organization to the rest of the application.
package auth

import (
	"context"
	"strings"
)

// User is the authenticated caller.
type User struct {
	Sub      string   `json:"sub"`
	Username string   `json:"username"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Org      string   `json:"org"`
	Groups   []string `json:"groups"`
	Roles    []string `json:"roles"`
	Admin    bool     `json:"admin"`

	// memberOf contains Groups plus all of their ancestor paths, so that a
	// member of /engineering/backend also matches a share for /engineering.
	memberOf map[string]struct{}
}

// InGroup reports whether the user is a member of group or one of its subgroups.
func (u *User) InGroup(group string) bool {
	if u == nil {
		return false
	}
	if u.memberOf == nil {
		u.memberOf = expandGroups(u.Groups)
	}
	_, ok := u.memberOf[normalizeGroup(group)]
	return ok
}

// GroupPaths returns the user's groups including all ancestor paths.
func (u *User) GroupPaths() []string {
	if u.memberOf == nil {
		u.memberOf = expandGroups(u.Groups)
	}
	out := make([]string, 0, len(u.memberOf))
	for g := range u.memberOf {
		out = append(out, g)
	}
	return out
}

func expandGroups(groups []string) map[string]struct{} {
	m := make(map[string]struct{})
	for _, g := range groups {
		g = normalizeGroup(g)
		for g != "" && g != "/" {
			m[g] = struct{}{}
			i := strings.LastIndex(g, "/")
			if i <= 0 {
				break
			}
			g = g[:i]
		}
	}
	return m
}

// normalizeGroup turns "engineering" or "/engineering/" into "/engineering".
func normalizeGroup(g string) string {
	g = strings.TrimSpace(g)
	g = strings.TrimRight(g, "/")
	if g != "" && !strings.HasPrefix(g, "/") {
		g = "/" + g
	}
	return g
}

// NormalizeGroup is the exported form of normalizeGroup for validating shares.
func NormalizeGroup(g string) string { return normalizeGroup(g) }

type ctxKey struct{}

func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the authenticated user or nil.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}
