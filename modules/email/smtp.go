package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/mailmsg"
	"github.com/glue2flow/g2f/internal/nodes"
)

// send delivers one message per input item over a single SMTP connection.
func send(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	if len(ec.Items) == 0 {
		return nodes.Output{"main": {}}, nil
	}
	srv, err := settings(ec, "smtp", 587, "starttls")
	if err != nil {
		return nil, err
	}
	// Default sender: a personal account (the node's credential) sends as itself,
	// the organization's account as the configured sender.
	orgUser := strings.TrimSpace(expression.Stringify(ec.Config["smtpUsername"]))
	defaultFrom := strings.TrimSpace(expression.Stringify(ec.Config["fromAddress"]))
	if (srv.username != orgUser || defaultFrom == "") && strings.Contains(srv.username, "@") {
		defaultFrom = srv.username
	}

	// Build all messages first, so a bad item does not leave a half-sent batch.
	msgs := make([]*mailmsg.Message, len(ec.Items))
	for i, item := range ec.Items {
		m, err := buildMessage(ec, item, i, defaultFrom)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		msgs[i] = m
	}

	c, err := connectSMTP(ctx, srv)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer c.Close()

	out := make([]nodes.Item, 0, len(msgs))
	for i, m := range msgs {
		res, err := deliver(c, m)
		if err != nil {
			err = ctxErr(ctx, err)
			if i > 0 {
				return nil, fmt.Errorf("item %d (after %d sent): %w", i, i, err)
			}
			return nil, err
		}
		ec.Log(nodes.LogInfo, fmt.Sprintf("sent %q to %s", m.Subject, recipientsText(res)))
		out = append(out, res)
	}
	_ = c.Quit()
	return nodes.Output{"main": out}, nil
}

func recipientsText(res nodes.Item) string {
	var parts []string
	for _, r := range res["recipients"].([]any) {
		parts = append(parts, r.(string))
	}
	return strings.Join(parts, ", ")
}

func buildMessage(ec *nodes.ExecContext, item nodes.Item, i int, defaultFrom string) (*mailmsg.Message, error) {
	p, err := nodes.ResolveParams(ec, item, i)
	if err != nil {
		return nil, err
	}
	str := func(k string) string { return strings.TrimSpace(expression.Stringify(p[k])) }
	m := &mailmsg.Message{
		From:    str("from"),
		To:      mailmsg.List(p["to"]),
		Cc:      mailmsg.List(p["cc"]),
		Bcc:     mailmsg.List(p["bcc"]),
		ReplyTo: mailmsg.List(p["replyTo"]),
		Subject: expression.Stringify(p["subject"]),
		Text:    expression.Stringify(p["text"]),
		HTML:    expression.Stringify(p["html"]),
	}
	if m.From == "" {
		m.From = defaultFrom
	}
	if m.From == "" {
		return nil, errors.New("no sender: set From, or the default sender in the module settings")
	}
	if m.Attachments, err = mailmsg.Attachments(jsonValue(p["attachments"])); err != nil {
		return nil, err
	}
	switch h := jsonValue(p["headers"]).(type) {
	case nil:
	case map[string]any:
		m.Headers = map[string]string{}
		for k, v := range h {
			m.Headers[k] = expression.Stringify(v)
		}
	default:
		return nil, errors.New("extra headers must be a JSON object")
	}
	return m, nil
}

// jsonValue reads a JSON field: empty strings are unset, and text (e.g. a
// field mixing JSON and expressions) is parsed.
func jsonValue(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err == nil {
		return parsed
	}
	return v
}

func connectSMTP(ctx context.Context, srv server) (*smtp.Client, error) {
	conn, err := dial(ctx, srv)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", srv.addr(), err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(commandTimeout)
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, srv.host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp %s: %w", srv.addr(), err)
	}
	fail := func(err error) (*smtp.Client, error) {
		c.Close()
		return nil, err
	}
	if err := c.Hello(heloName()); err != nil {
		return fail(fmt.Errorf("smtp hello: %w", err))
	}
	if srv.security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fail(fmt.Errorf("%s does not offer STARTTLS", srv.addr()))
		}
		if err := c.StartTLS(srv.tlsConfig()); err != nil {
			return fail(fmt.Errorf("smtp starttls: %w", err))
		}
	}
	if srv.username != "" {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return fail(fmt.Errorf("%s does not accept authentication", srv.addr()))
		}
		var a smtp.Auth
		if hasMech(mechs, "PLAIN") || !hasMech(mechs, "LOGIN") {
			a = &plainAuth{username: srv.username, password: srv.password}
		} else {
			a = &loginAuth{username: srv.username, password: srv.password}
		}
		if err := c.Auth(a); err != nil {
			return fail(fmt.Errorf("smtp authentication as %s failed: %w", srv.username, err))
		}
	}
	return c, nil
}

func deliver(c *smtp.Client, m *mailmsg.Message) (nodes.Item, error) {
	from, err := m.FromAddress()
	if err != nil {
		return nil, err
	}
	rcpts, err := m.Recipients()
	if err != nil {
		return nil, err
	}
	if len(rcpts) == 0 {
		return nil, errors.New("no recipients")
	}
	raw, err := mailmsg.Build(m, false)
	if err != nil {
		return nil, err
	}
	if err := c.Mail(from); err != nil {
		return nil, fmt.Errorf("MAIL FROM %s: %w", from, err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			_ = c.Reset()
			return nil, fmt.Errorf("RCPT TO %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return nil, fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("message rejected: %w", err)
	}
	list := make([]any, len(rcpts))
	for i, r := range rcpts {
		list[i] = r
	}
	return nodes.Item{
		"messageId":  headerValue(raw, "Message-ID"),
		"from":       from,
		"recipients": list,
		"subject":    m.Subject,
	}, nil
}

func headerValue(raw []byte, name string) string {
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func hasMech(mechs, m string) bool {
	for _, f := range strings.Fields(mechs) {
		if strings.EqualFold(f, m) {
			return true
		}
	}
	return false
}

func heloName() string {
	if h, err := os.Hostname(); err == nil && h != "" && !strings.ContainsAny(h, " \r\n") {
		return h
	}
	return "localhost"
}

// plainAuth is AUTH PLAIN. Unlike smtp.PlainAuth it trusts the caller's check
// (server.checkCredentials) that credentials only travel encrypted or locally.
type plainAuth struct{ username, password string }

func (a *plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// loginAuth is AUTH LOGIN, still the only password mechanism of some servers
// (e.g. Microsoft 365).
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:", "user name", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", challenge)
}
