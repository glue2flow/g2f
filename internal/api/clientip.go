package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP returns the address of the client that sent r. The peer address is
// used unless it belongs to a trusted proxy; then X-Forwarded-For is walked
// from right to left and the first address that is not a trusted proxy wins.
// Forwarding headers from other peers are ignored, as clients can forge them.
func clientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	peer = peer.Unmap().WithZone("")
	if !isTrusted(peer, trusted) {
		return peer, true
	}
	values := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer, true // garbage: do not guess
		}
		ip = ip.Unmap().WithZone("")
		if !isTrusted(ip, trusted) {
			return ip, true
		}
	}
	return peer, true
}

func isTrusted(ip netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIPMiddleware sets r.RemoteAddr to the client address (see clientIP),
// keeping the peer's port so that net.SplitHostPort keeps working downstream.
func clientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip, ok := clientIP(r, trusted); ok {
				_, port, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil {
					port = "0"
				}
				r.RemoteAddr = net.JoinHostPort(ip.String(), port)
			}
			next.ServeHTTP(w, r)
		})
	}
}
