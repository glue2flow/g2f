package store

import (
	"context"
	"testing"
)

func TestConnectionInstanceURL(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := &Connection{OrgID: "o-inst", Module: "m", Name: "n", OwnerSub: "u", Scope: "private", InstanceURL: "https://acme.my.salesforce.com"}
	plain := &Connection{OrgID: "o-inst", Module: "m", Name: "plain", OwnerSub: "u", Scope: "private"}
	for _, x := range []*Connection{c, plain} {
		if err := s.CreateConnection(ctx, x); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.DeleteConnection(ctx, x.ID) })
	}
	if got, err := s.GetConnection(ctx, c.ID); err != nil || got.InstanceURL != "https://acme.my.salesforce.com" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := s.GetConnection(ctx, plain.ID); err != nil || got.InstanceURL != "" {
		t.Fatalf("%+v %v", got, err)
	}
	got, err := s.RefreshConnection(ctx, c.ID, func(c *Connection) (bool, error) {
		c.InstanceURL = "https://acme2.my.salesforce.com"
		return true, nil
	})
	if err != nil || got.InstanceURL != "https://acme2.my.salesforce.com" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ = s.GetConnection(ctx, c.ID); got.InstanceURL != "https://acme2.my.salesforce.com" {
		t.Fatalf("not persisted: %+v", got)
	}
}
