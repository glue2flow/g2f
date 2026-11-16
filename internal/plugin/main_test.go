package plugin

import (
	"os"
	"testing"

	"github.com/glue2flow/g2f/internal/netguard"
)

// WASM plugins send HTTP through the guarded defaults, as in the server. The
// tests call httptest servers on 127.0.0.1.
func TestMain(m *testing.M) {
	netguard.SetPolicy(netguard.Policy{AllowPrivate: true})
	netguard.InstallDefaults()
	os.Exit(m.Run())
}
