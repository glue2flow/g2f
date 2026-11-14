package store

import (
	"context"
	"errors"
	"testing"
)

func TestSavePluginReplace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const module = "zz-test-plugin-save"
	_ = s.DeletePlugin(ctx, module)
	t.Cleanup(func() { _ = s.DeletePlugin(context.Background(), module) })

	v1 := &PluginRecord{Module: module, Version: "1.0.0", Manifest: []byte("m1"), Bundle: []byte("b1"),
		Filename: "x.yaml", Signature: `{"keyId":"acme"}`, Signer: "acme", SHA256: "d1", InstalledBy: "alice"}
	if err := s.SavePlugin(ctx, v1, false); err != nil {
		t.Fatal(err)
	}
	v2 := &PluginRecord{Module: module, Version: "2.0.0", Manifest: []byte("m2"), Bundle: []byte("b2"), Filename: "x.zip", SHA256: "d2"}
	if err := s.SavePlugin(ctx, v2, false); !errors.Is(err, ErrPluginExists) {
		t.Fatalf("want ErrPluginExists, got %v", err)
	}
	get := func() *PluginRecord {
		recs, err := s.ListPlugins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Module == module {
				return r
			}
		}
		t.Fatal("not found")
		return nil
	}
	if r := get(); r.Version != "1.0.0" || string(r.Bundle) != "b1" || r.Signer != "acme" || r.Signature == "" || r.Filename != "x.yaml" {
		t.Fatalf("%+v", r)
	}
	if err := s.SavePlugin(ctx, v2, true); err != nil {
		t.Fatal(err)
	}
	if r := get(); r.Version != "2.0.0" || string(r.Bundle) != "b2" || r.Signer != "" || r.Signature != "" || r.SHA256 != "d2" {
		t.Fatalf("%+v", r)
	}
}
