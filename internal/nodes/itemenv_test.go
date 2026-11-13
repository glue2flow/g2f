package nodes

import (
	"context"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
)

func TestItemEnvKeepsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ec := &ExecContext{Env: expression.WithContext(expression.Env{}, ctx)}
	if _, err := expression.Render("{{ json.a }}", ec.ItemEnv(Item{"a": 1}, 0)); err == nil {
		t.Fatal("item scope lost the cancellation context")
	}
}
