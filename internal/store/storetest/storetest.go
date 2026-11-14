// Package storetest gives tests their own PostgreSQL database. Set
// G2F_TEST_DATABASE_URL to a server where the user may create databases,
// e.g. postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable; without it the
// tests are skipped.
package storetest

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/glue2flow/g2f/internal/store"
)

// DB creates a migrated database that is dropped when the test ends.
func DB(t testing.TB) *store.Store {
	t.Helper()
	base := os.Getenv("G2F_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("G2F_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "g2f_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}
