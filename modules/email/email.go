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
	"github.com/glue2flow/g2f/internal/mailmsg"
	"github.com/glue2flow/g2f/internal/nodes"
)

func init() {
	nodes.RegisterNative("email", map[string]nodes.ExecFunc{
		"send":     send,
		"read":     read,
		"move":     move,
		"setFlags": setFlags,
		"received": received,
	})
	nodes.RegisterListener("email.received", listen)
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
// config. The node's credential, if one is selected, replaces the
// organization's account (the host stays the one configured by the admin).
func settings(ec *nodes.ExecContext, proto string, defPort int, defSecurity string) (server, error) {
	s, err := serverConfig(ec.Config, proto, defPort, defSecurity)
	if err != nil {
		return s, err
	}
	if user := strings.TrimSpace(expression.Stringify(ec.Credential["username"])); user != "" {
		s.username, s.password = user, expression.Stringify(ec.Credential["password"])
	}
	return s, s.checkCredentials()
}

// serverConfig reads the organization's server of a protocol from the module config.
func serverConfig(config map[string]any, proto string, defPort int, defSecurity string) (server, error) {
	cfg := func(k string) string { return strings.TrimSpace(expression.Stringify(config[proto+k])) }
	s := server{
		host:     cfg("Host"),
		security: strings.ToLower(cfg("Security")),
		username: cfg("Username"),
		password: expression.Stringify(config[proto+"Password"]),
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
	return s, nil
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

// Configured reports whether the module config (Plugins → Email → Settings)
// has an SMTP server, so SendText can be used.
func Configured(config map[string]any) bool {
	return strings.TrimSpace(expression.Stringify(config["smtpHost"])) != ""
}

// SendText sends a plain-text message with the organization's SMTP account
// and default sender. It is used outside workflows, e.g. for the access codes
// of form triggers.
func SendText(ctx context.Context, config map[string]any, to, subject, text string) error {
	srv, err := serverConfig(config, "smtp", 587, "starttls")
	if err != nil {
		return err
	}
	if err := srv.checkCredentials(); err != nil {
		return err
	}
	from := strings.TrimSpace(expression.Stringify(config["fromAddress"]))
	if from == "" {
		from = srv.username
	}
	if !strings.Contains(from, "@") {
		return fmt.Errorf("no sender address: set the default sender in Plugins → Email → Settings")
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	c, err := connectSMTP(ctx, srv)
	if err != nil {
		return ctxErr(ctx, err)
	}
	defer c.Close()
	if _, err := deliver(c, &mailmsg.Message{From: from, To: []string{to}, Subject: subject, Text: text}); err != nil {
		return ctxErr(ctx, err)
	}
	return c.Quit()
}

// OrgMailer sends plain-text messages with an organization's e-mail settings
// (e.g. access codes of form triggers). Settings is secrets.Service.ModuleSettings.
type OrgMailer struct {
	Settings func(ctx context.Context, org, module string) (config, vars map[string]any, err error)
}

func (m OrgMailer) Configured(ctx context.Context, org string) (bool, error) {
	cfg, _, err := m.Settings(ctx, org, "email")
	if err != nil {
		return false, err
	}
	return Configured(cfg), nil
}

func (m OrgMailer) SendText(ctx context.Context, org, to, subject, text string) error {
	cfg, _, err := m.Settings(ctx, org, "email")
	if err != nil {
		return err
	}
	if !Configured(cfg) {
		return fmt.Errorf("e-mail is not configured for organization %q", org)
	}
	return SendText(ctx, cfg, to, subject, text)
}
