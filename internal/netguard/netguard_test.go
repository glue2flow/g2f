package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBlocked(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::1", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fe80::1%eth0", "fd00::1", "fc00::1", "100.64.0.1", "100.127.255.255",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1", "192.0.0.8", "198.18.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.5", "::ffff:169.254.169.254", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe",
		"2002:7f00:1::", "2001:db8::1", "::7f00:1",
	}
	for _, s := range blocked {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.32.0.1", "100.128.0.1", "2606:4700:4700::1111", "64:ff9b::808:808", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

func TestParseAllowList(t *testing.T) {
	l, err := ParseAllowList([]string{"10.1.2.3", "192.168.0.0/16", "Intranet.Example", "*.corp.internal", "::ffff:10.9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !l.allowsIP(netip.MustParseAddr("10.1.2.3")) || !l.allowsIP(netip.MustParseAddr("192.168.9.9")) || !l.allowsIP(netip.MustParseAddr("10.9.9.9")) {
		t.Error("ip entries")
	}
	if l.allowsIP(netip.MustParseAddr("10.1.2.4")) {
		t.Error("unlisted ip")
	}
	for host, want := range map[string]bool{
		"intranet.example": true, "INTRANET.example.": true, "a.corp.internal": true, "a.b.corp.internal": true,
		"corp.internal": false, "evilcorp.internal": false, "x.intranet.example": false,
	} {
		if got := l.allowsHost(host); got != want {
			t.Errorf("allowsHost(%q) = %v, want %v", host, got, want)
		}
	}
	for _, bad := range []string{"http://x.example", "a/b", "10.0.0.0/33", "foo.*", "host:8080", "*.", "a..b"} {
		if _, err := ParseAllowList([]string{bad}); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func setTestPolicy(t *testing.T, p Policy) {
	t.Helper()
	old := CurrentPolicy()
	SetPolicy(p)
	t.Cleanup(func() { SetPolicy(old) })
}

func fakeDNS(t *testing.T, answers map[string]string) {
	t.Helper()
	old := lookupIP
	lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := answers[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { lookupIP = old })
}

func get(t *testing.T, url string) error {
	t.Helper()
	c := &http.Client{Transport: NewTransport()}
	resp, err := c.Get(url)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestDialBlocksAfterResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	fakeDNS(t, map[string]string{"localhost": "127.0.0.1", "public.example": "127.0.0.1", "meta.example": "169.254.169.254", "internal.example": "10.0.0.5"})

	setTestPolicy(t, Policy{})
	for _, u := range []string{srv.URL, "http://localhost" + port, "http://public.example" + port, "http://meta.example/", "http://[::1]" + port, "http://0.0.0.0" + port} {
		err := get(t, u)
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("%s: want BlockedError, got %v", u, err)
		}
	}
	err := get(t, "http://internal.example/x")
	if err == nil || !strings.Contains(err.Error(), "request to 10.0.0.5 (internal.example) blocked: private and local addresses are not allowed (see G2F_HTTP_ALLOW_PRIVATE / G2F_HTTP_ALLOWED_HOSTS)") {
		t.Errorf("message: %v", err)
	}

	// A hostname allow-list entry applies to the requested name, not the IP.
	allow, _ := ParseAllowList([]string{"public.example"})
	setTestPolicy(t, Policy{Allow: allow})
	if err := get(t, "http://public.example"+port); err != nil {
		t.Errorf("allowed by name: %v", err)
	}
	if err := get(t, srv.URL); err == nil {
		t.Error("the IP itself stays blocked")
	}

	allow, _ = ParseAllowList([]string{"127.0.0.0/8"})
	setTestPolicy(t, Policy{Allow: allow})
	if err := get(t, srv.URL); err != nil {
		t.Errorf("allowed by CIDR: %v", err)
	}
	if err := get(t, "http://public.example"+port); err != nil {
		t.Errorf("name resolving into an allowed CIDR: %v", err)
	}

	allow, _ = ParseAllowList([]string{"*.example"})
	setTestPolicy(t, Policy{Allow: allow})
	if err := get(t, "http://public.example"+port); err != nil {
		t.Errorf("allowed by wildcard: %v", err)
	}

	setTestPolicy(t, Policy{AllowPrivate: true})
	if err := get(t, srv.URL); err != nil {
		t.Errorf("allow private: %v", err)
	}
}

// A name that resolves to a public address first and a private one later
// (DNS rebinding) is checked on every connection.
func TestDialChecksEveryConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	setTestPolicy(t, Policy{})
	calls := 0
	old := lookupIP
	lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		calls++
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupIP = old })
	c := &http.Client{Transport: NewTransport()}
	for range 2 {
		if resp, err := c.Get("http://rebind.example" + port); err == nil {
			resp.Body.Close()
			t.Fatal("must be blocked")
		}
	}
	if calls != 2 {
		t.Fatalf("lookups = %d", calls)
	}
}

func TestDefaultCheckRedirect(t *testing.T) {
	req := func(ctx context.Context, u string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		return r
	}
	ctx := WithAllowedHosts(context.Background(), []string{"api.example.com", "*.cdn.example"})
	if err := defaultCheckRedirect(req(ctx, "https://api.example.com/x"), nil); err != nil {
		t.Error(err)
	}
	if err := defaultCheckRedirect(req(ctx, "https://img.cdn.example/x"), nil); err != nil {
		t.Error(err)
	}
	if err := defaultCheckRedirect(req(ctx, "https://evil.example/x"), nil); err == nil {
		t.Error("redirect outside allowedHosts must fail")
	}
	if err := defaultCheckRedirect(req(context.Background(), "https://anything.example/x"), nil); err != nil {
		t.Errorf("requests that are not plugin requests are unrestricted: %v", err)
	}
	if err := defaultCheckRedirect(req(ctx, "https://api.example.com/x"), make([]*http.Request, 10)); err == nil {
		t.Error("redirect limit")
	}
}
