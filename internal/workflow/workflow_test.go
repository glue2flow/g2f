package workflow

import (
	"testing"

	"github.com/glue2flow/g2f/internal/auth"
)

func TestRoleFor(t *testing.T) {
	owner := &auth.User{Sub: "o", Org: "acme"}
	backend := &auth.User{Sub: "b", Org: "acme", Groups: []string{"/engineering/backend"}}
	sales := &auth.User{Sub: "s", Org: "acme", Groups: []string{"/sales"}}
	admin := &auth.User{Sub: "a", Org: "acme", Admin: true}
	other := &auth.User{Sub: "x", Org: "globex", Groups: []string{"/engineering"}, Admin: true}

	cases := []struct {
		name string
		wf   Workflow
		user *auth.User
		want Role
	}{
		{"owner private", Workflow{Scope: ScopePrivate}, owner, RoleOwner},
		{"other private", Workflow{Scope: ScopePrivate}, backend, RoleNone},
		{"admin private", Workflow{Scope: ScopePrivate}, admin, RoleOwner},
		{"other org admin", Workflow{Scope: ScopeOrg}, other, RoleNone},
		{"group share via parent", Workflow{Scope: ScopeGroup, Shares: []Share{{"/engineering", RoleEditor}}}, backend, RoleEditor},
		{"group not shared", Workflow{Scope: ScopeGroup, Shares: []Share{{"/engineering", RoleEditor}}}, sales, RoleNone},
		{"org default", Workflow{Scope: ScopeOrg, OrgRole: RoleRunner}, sales, RoleRunner},
		{"org plus share", Workflow{Scope: ScopeOrg, OrgRole: RoleViewer, Shares: []Share{{"/engineering/backend", RoleEditor}}}, backend, RoleEditor},
		{"private ignores shares", Workflow{Scope: ScopePrivate, Shares: []Share{{"/engineering", RoleEditor}}}, backend, RoleNone},
	}
	for _, c := range cases {
		c.wf.OwnerSub, c.wf.OrgID = "o", "acme"
		if got := c.wf.RoleFor(c.user); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestValidateSharing(t *testing.T) {
	w := Workflow{Scope: ScopeGroup, Shares: []Share{{"engineering/", RoleViewer}}}
	if err := w.ValidateSharing(); err != nil {
		t.Fatal(err)
	}
	if w.Shares[0].Group != "/engineering" || w.OrgRole != RoleViewer {
		t.Fatalf("not normalised: %+v", w)
	}
	bad := Workflow{Scope: ScopeGroup, Shares: []Share{{"/x", RoleOwner}}}
	if bad.ValidateSharing() == nil {
		t.Fatal("owner role must not be grantable via shares")
	}
}

func TestGraphCycle(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a", Type: "t"}, {ID: "b", Type: "t"}},
		Edges: []Edge{{ID: "1", Source: "a", Target: "b"}, {ID: "2", Source: "b", Target: "a"}},
	}
	err := g.Validate(func(string) bool { return true }, func(string) bool { return false })
	if err == nil {
		t.Fatal("expected cycle error")
	}
}
