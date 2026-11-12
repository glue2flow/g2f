package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("fd00::/8")}
	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer, spoofed XFF", "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted peer, chain", "10.0.0.1:80", []string{"6.6.6.6, 198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"trusted peer, several headers", "10.0.0.1:80", []string{"6.6.6.6", "198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"trusted peer, no XFF", "10.0.0.1:80", nil, "10.0.0.1"},
		{"trusted peer, all trusted", "10.0.0.1:80", []string{"10.0.0.5, 127.0.0.1"}, "10.0.0.1"},
		{"trusted peer, garbage", "10.0.0.1:80", []string{"nonsense"}, "10.0.0.1"},
		{"trusted peer, garbage after real", "10.0.0.1:80", []string{"198.51.100.7, nonsense"}, "10.0.0.1"},
		{"trusted peer, empty", "10.0.0.1:80", []string{""}, "10.0.0.1"},
		{"ipv6 untrusted peer", "[2001:db8::1]:443", []string{"1.2.3.4"}, "2001:db8::1"},
		{"ipv6 trusted peer", "[fd00::1]:443", []string{"2001:db8::5, fd00::2"}, "2001:db8::5"},
		{"ipv4-mapped peer", "[::ffff:10.0.0.1]:80", []string{"198.51.100.7"}, "198.51.100.7"},
		{"no trusted proxies configured", "10.0.0.1:80", []string{"1.2.3.4"}, "10.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := trusted
			if tc.name == "no trusted proxies configured" {
				tr = nil
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			r.Header.Set("X-Real-IP", "9.9.9.9")
			r.Header.Set("True-Client-IP", "9.9.9.9")
			got, ok := clientIP(r, tr)
			if !ok || got.String() != tc.want {
				t.Fatalf("got %v (%v), want %s", got, ok, tc.want)
			}
		})
	}
}

func TestClientIPMiddleware(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	var seen string
	h := clientIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	for _, tc := range []struct{ remote, xff, want string }{
		{"127.0.0.1:5000", "198.51.100.7", "198.51.100.7:5000"},
		{"203.0.113.9:5000", "198.51.100.7", "203.0.113.9:5000"},
		{"127.0.0.1:5000", "2001:db8::1", "[2001:db8::1]:5000"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.xff)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if seen != tc.want {
			t.Fatalf("remote %s: got %s, want %s", tc.remote, seen, tc.want)
		}
	}
}
