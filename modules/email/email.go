// Package email implements the "email" module: SMTP sending and IMAP reading.
// Its nodes and settings are declared in plugin.yaml (runtime: go); this
// package registers the Go code behind them. Import it for side effects:
//
//	import _ "github.com/glue2flow/g2f/modules/email"
package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/nodes"
)

func init() {
	nodes.RegisterNative("email", map[string]nodes.ExecFunc{
		"send":     send,
		"read":     read,
		"move":     move,
		"setFlags": setFlags,
	})
}

const (
	dialTimeout    = 30 * time.Second
	commandTimeout = 2 * time.Minute
)

// server holds the connection settings of one protocol.
type server struct {
	host     string
	port     int
	security string // tls | starttls | none
	username string
	password string
}

func (s server) addr() string { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }

func (s server) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
}

// checkCredentials refuses to send a password in clear text to anything but
// the local machine.
func (s server) checkCredentials() error {
	if s.username != "" && s.security == "none" && !isLocal(s.host) {
		return fmt.Errorf("refusing to send credentials to %s over an unencrypted connection; use tls or starttls", s.host)
	}
	return nil
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// settings reads the server of a protocol ("smtp" or "imap") from the module
// config. A Username/Password on the node replaces the organization's account
// (the host stays the one configured by the admin).
func settings(ec *nodes.ExecContext, proto string, defPort int, defSecurity string) (server, error) {
	cfg := func(k string) string { return strings.TrimSpace(expression.Stringify(ec.Config[proto+k])) }
	s := server{
		host:     cfg("Host"),
		security: strings.ToLower(cfg("Security")),
		username: cfg("Username"),
		password: expression.Stringify(ec.Config[proto+"Password"]),
	}
	if s.host == "" {
		return s, fmt.Errorf("%s is not configured: an admin must set the %s host in Plugins → Email → Settings", strings.ToUpper(proto), strings.ToUpper(proto))
	}
	s.port = defPort
	if p := cfg("Port"); p != "" {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 1 || f > 65535 {
			return s, fmt.Errorf("invalid %s port %q", proto, p)
		}
		s.port = int(f)
	}
	if s.security == "" {
		s.security = defSecurity
	}
	switch s.security {
	case "tls", "starttls", "none":
	default:
		return s, fmt.Errorf("invalid %s security %q", proto, s.security)
	}
	var first nodes.Item
	if len(ec.Items) > 0 {
		first = ec.Items[0]
	}
	user, err := ec.String("username", first, 0)
	if err != nil {
		return s, err
	}
	if user = strings.TrimSpace(user); user != "" {
		pass, err := ec.String("password", first, 0)
		if err != nil {
			return s, err
		}
		s.username, s.password = user, pass
	}
	return s, s.checkCredentials()
}

// dial opens a TCP (or TLS) connection that is closed when ctx ends.
func dial(ctx context.Context, s server) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if s.security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: s.tlsConfig()}).DialContext(ctx, "tcp", s.addr())
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.addr())
	}
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	return &ctxConn{Conn: conn, stop: stop}, nil
}

type ctxConn struct {
	net.Conn
	stop func() bool
}

func (c *ctxConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// ctxErr prefers the context's error (timeout, cancel) over the I/O error it caused.
func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// --- parameter helpers ------------------------------------------------------

func param(ec *nodes.ExecContext, name string, item nodes.Item, i int) (string, error) {
	s, err := ec.String(name, item, i)
	return strings.TrimSpace(s), err
}

func required(ec *nodes.ExecContext, name string, item nodes.Item, i int) (string, error) {
	s, err := param(ec, name, item, i)
	if err == nil && s == "" {
		label := name
		if p := ec.Def.Param(name); p != nil {
			label = p.Label
		}
		err = fmt.Errorf("parameter %q is required", label)
	}
	return s, err
}

func uidParam(ec *nodes.ExecContext, item nodes.Item, i int) (uint32, error) {
	s, err := required(ec, "uid", item, i)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 1 || f > float64(^uint32(0)) || f != float64(uint32(f)) {
		return 0, fmt.Errorf("item %d: invalid UID %q", i, s)
	}
	return uint32(f), nil
}

func copyItem(item nodes.Item) nodes.Item {
	out := make(nodes.Item, len(item)+1)
	for k, v := range item {
		out[k] = v
	}
	return out
}
