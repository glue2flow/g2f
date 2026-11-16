package engine

import (
	"os"
	"testing"

	"github.com/glue2flow/g2f/internal/netguard"
)

// Tests call httptest servers on 127.0.0.1.
func TestMain(m *testing.M) {
	netguard.SetPolicy(netguard.Policy{AllowPrivate: true})
	os.Exit(m.Run())
}
