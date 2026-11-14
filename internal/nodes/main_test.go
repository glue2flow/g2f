package nodes

import (
	"os"
	"testing"

	"github.com/glue2flow/g2f/internal/netguard"
)

// Most tests call httptest servers on 127.0.0.1 over plain http: allow local
// addresses and plain-http token hosts. Tests of the guard set their own
// policy and restore this one.
func TestMain(m *testing.M) {
	netguard.SetPolicy(netguard.Policy{AllowPrivate: true})
	allowInsecureAPIHosts = true
	os.Exit(m.Run())
}

// setPolicy sets the SSRF policy for one test.
func setPolicy(t *testing.T, p netguard.Policy) {
	t.Helper()
	old := netguard.CurrentPolicy()
	netguard.SetPolicy(p)
	t.Cleanup(func() { netguard.SetPolicy(old) })
}
