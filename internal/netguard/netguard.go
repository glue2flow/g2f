// Package netguard keeps workflow-directed HTTP requests away from private
// networks (SSRF protection). The check runs in the dial path, after DNS
// resolution, so it covers every connection: redirect hops and DNS rebinding
// included.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// Policy decides which destinations workflow HTTP requests may reach.
type Policy struct {
	// AllowPrivate turns the block off entirely.
	AllowPrivate bool
	// Allow lists exceptions that stay reachable while the block is on.
	Allow AllowList
}

var policy atomic.Pointer[Policy]

// SetPolicy replaces the process-wide policy. The zero Policy blocks all
// private and local destinations.
func SetPolicy(p Policy) { policy.Store(&p) }

// CurrentPolicy returns the active policy.
func CurrentPolicy() Policy {
	if p := policy.Load(); p != nil {
		return *p
	}
	return Policy{}
}

// AllowList holds IP/CIDR and host name exceptions.
type AllowList struct {
	prefixes []netip.Prefix
	hosts    []string // lower case; "*.suffix" entries match subdomains
}

// ParseAllowList parses entries that are IPs, CIDR ranges, host names or
// "*.example.internal" wildcards.
func ParseAllowList(entries []string) (AllowList, error) {
	var l AllowList
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if pre, err := netip.ParsePrefix(e); err == nil {
			l.prefixes = append(l.prefixes, pre.Masked())
			continue
		}
		if ip, err := netip.ParseAddr(e); err == nil {
			ip = ip.Unmap()
			l.prefixes = append(l.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		name := strings.TrimPrefix(e, "*.")
		if !validHostName(name) {
			return AllowList{}, fmt.Errorf("%q is not an IP address, CIDR range or host name (wildcards: *.example.internal)", e)
		}
		l.hosts = append(l.hosts, strings.TrimSuffix(e, "."))
	}
	return l, nil
}

func validHostName(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

func (l AllowList) allowsHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range l.hosts {
		if suffix, ok := strings.CutPrefix(h, "*"); ok {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}

func (l AllowList) allowsIP(ip netip.Addr) bool {
	for _, p := range l.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4",
	"240.0.0.0/4", // includes the broadcast address
	// IPv6
	"::/96", // unspecified, loopback, deprecated IPv4-compatible
	"64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// Blocked reports whether ip is a private, local or otherwise special-purpose
// address. IPv4 addresses embedded in IPv4-mapped, NAT64 and 6to4 forms are
// checked as IPv4.
func Blocked(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case netip.MustParsePrefix("64:ff9b::/96").Contains(ip):
			return Blocked(netip.AddrFrom4([4]byte(b[12:16])))
		case netip.MustParsePrefix("2002::/16").Contains(ip):
			return Blocked(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// lookupIP resolves a host name; tests replace it.
var lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// BlockedError is returned when a request is refused by the policy.
type BlockedError struct{ IP, Host string }

func (e *BlockedError) Error() string {
	to := e.IP
	if e.Host != "" && e.Host != e.IP {
		to += " (" + e.Host + ")"
	}
	return fmt.Sprintf("request to %s blocked: private and local addresses are not allowed (see G2F_HTTP_ALLOW_PRIVATE / G2F_HTTP_ALLOWED_HOSTS)", to)
}

// dialContext resolves, filters and then dials the vetted IP itself, so a
// second DNS answer cannot change the destination.
func dialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		p := CurrentPolicy()
		if p.AllowPrivate {
			return d.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else if ips, err = lookupIP(ctx, host); err != nil {
			return nil, err
		}
		byName := p.Allow.allowsHost(host)
		var ok []netip.Addr
		var blocked error
		for _, ip := range ips {
			if byName || p.Allow.allowsIP(ip.WithZone("").Unmap()) || !Blocked(ip) {
				ok = append(ok, ip)
			} else if blocked == nil {
				blocked = &BlockedError{IP: ip.WithZone("").Unmap().String(), Host: host}
			}
		}
		if len(ok) == 0 {
			if blocked == nil {
				blocked = fmt.Errorf("no addresses found for %s", host)
			}
			return nil, blocked
		}
		var last error
		for _, ip := range ok {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// NewTransport returns an HTTP transport whose connections obey the policy.
// Proxy settings from the environment still apply: with a proxy only the
// connection to the proxy is checked, because the proxy resolves the target.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContext(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// unguarded is the standard transport, captured before InstallDefaults
// replaces http.DefaultTransport.
var unguarded http.RoundTripper = http.DefaultTransport

// Unguarded is the transport for the server's own clients (identity provider,
// OAuth token endpoints): their URLs are set by administrators and may be
// internal.
func Unguarded() http.RoundTripper { return unguarded }

type hostsKey struct{}

// WithAllowedHosts marks ctx as a plugin request: redirects made by
// http.DefaultClient under this context must stay within the plugin's
// allowedHosts (exact names or glob patterns, like Extism's own check).
func WithAllowedHosts(ctx context.Context, hosts []string) context.Context {
	return context.WithValue(ctx, hostsKey{}, hosts)
}

// MatchHost reports whether host matches one of the allowedHosts patterns.
func MatchHost(patterns []string, host string) bool {
	for _, p := range patterns {
		if p == host {
			return true
		}
		if ok, _ := path.Match(p, host); ok {
			return true
		}
	}
	return false
}

func defaultCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if hosts, ok := req.Context().Value(hostsKey{}).([]string); ok && !MatchHost(hosts, req.URL.Hostname()) {
		return fmt.Errorf("redirect to '%s' is not allowed by the plugin's allowedHosts", req.URL.Hostname())
	}
	return nil
}

// InstallDefaults guards http.DefaultTransport and checks plugin redirects on
// http.DefaultClient. WASM plugins send their requests through them (Extism
// offers no other hook). Code that must reach internal hosts needs its own
// client on Unguarded().
func InstallDefaults() {
	http.DefaultTransport = NewTransport()
	http.DefaultClient.CheckRedirect = defaultCheckRedirect
}
