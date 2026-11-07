// Package workflow defines workflows, their graphs and the sharing model.
package workflow

import (
	"fmt"
	"time"

	"github.com/glue2flow/g2f/internal/auth"
)

// Scope controls who can see a workflow.
type Scope string

const (
	// ScopePrivate: only the owner (and org admins).
	ScopePrivate Scope = "private"
	// ScopeGroup: the owner plus members of the groups listed in Shares.
	ScopeGroup Scope = "group"
	// ScopeOrg: everyone in the owner's organization, with OrgRole; Shares can grant more.
	ScopeOrg Scope = "org"
)

// Role is a permission level on a workflow. Higher roles include lower ones.
type Role string

const (
	RoleNone   Role = ""
	RoleViewer Role = "viewer" // see workflow and executions
	RoleRunner Role = "runner" // + run manually
	RoleEditor Role = "editor" // + edit graph, activate/deactivate
	RoleOwner  Role = "owner"  // + delete, change scope and shares
)

var roleRank = map[Role]int{RoleNone: 0, RoleViewer: 1, RoleRunner: 2, RoleEditor: 3, RoleOwner: 4}

func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] }

func (r Role) Valid() bool {
	_, ok := roleRank[r]
	return ok && r != RoleNone && r != RoleOwner
}

func maxRole(a, b Role) Role {
	if roleRank[b] > roleRank[a] {
		return b
	}
	return a
}

// Share grants a Keycloak group (full path, e.g. /engineering/backend) a role.
type Share struct {
	Group string `json:"group"`
	Role  Role   `json:"role"`
}

type Workflow struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"orgId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	OwnerSub    string    `json:"ownerSub"`
	OwnerName   string    `json:"ownerName"`
	Scope       Scope     `json:"scope"`
	OrgRole     Role      `json:"orgRole"`
	Shares      []Share   `json:"shares"`
	Active      bool      `json:"active"`
	Graph       Graph     `json:"graph"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	// Access is the caller's effective role, filled in by the API layer.
	Access Role `json:"access,omitempty"`
}

// RoleFor computes the effective role of u on w.
func (w *Workflow) RoleFor(u *auth.User) Role {
	if u == nil || u.Org != w.OrgID {
		return RoleNone
	}
	if w.OwnerSub == u.Sub || u.Admin {
		return RoleOwner
	}
	role := RoleNone
	switch w.Scope {
	case ScopeOrg:
		role = w.OrgRole
		if !role.Valid() {
			role = RoleViewer
		}
		fallthrough
	case ScopeGroup:
		for _, s := range w.Shares {
			if u.InGroup(s.Group) {
				role = maxRole(role, s.Role)
			}
		}
	}
	return role
}

// ValidateSharing normalises and checks scope, org role and shares.
func (w *Workflow) ValidateSharing() error {
	switch w.Scope {
	case ScopePrivate, ScopeGroup, ScopeOrg:
	case "":
		w.Scope = ScopePrivate
	default:
		return fmt.Errorf("invalid scope %q", w.Scope)
	}
	if w.OrgRole == "" {
		w.OrgRole = RoleViewer
	}
	if !w.OrgRole.Valid() {
		return fmt.Errorf("invalid orgRole %q", w.OrgRole)
	}
	seen := map[string]bool{}
	shares := w.Shares[:0]
	for _, s := range w.Shares {
		s.Group = auth.NormalizeGroup(s.Group)
		if s.Group == "" {
			return fmt.Errorf("share group must not be empty")
		}
		if !s.Role.Valid() {
			return fmt.Errorf("invalid role %q for group %s", s.Role, s.Group)
		}
		if seen[s.Group] {
			return fmt.Errorf("duplicate share for group %s", s.Group)
		}
		seen[s.Group] = true
		shares = append(shares, s)
	}
	w.Shares = shares
	if w.Shares == nil {
		w.Shares = []Share{}
	}
	return nil
}
