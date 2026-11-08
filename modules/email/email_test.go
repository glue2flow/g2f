package email

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	imapserver "github.com/emersion/go-imap/server"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/workflow"
)

// loadNodes builds the module from its real manifest.
func loadNodes(t *testing.T) map[string]nodes.Node {
	t.Helper()
	raw, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := nodes.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	list, err := nodes.NativeModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]nodes.Node{}
	for _, n := range list {
		out[n.Definition().Type] = n
	}
	return out
}

func run(t *testing.T, n nodes.Node, config, params map[string]any, items []nodes.Item) (nodes.Output, error) {
	t.Helper()
	def := n.Definition()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return n.Execute(ctx, &nodes.ExecContext{
		Node: &workflow.Node{Name: def.Name, Params: params}, Def: &def, Items: items, Config: config,
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// --- SMTP -------------------------------------------------------------------

type fakeSMTP struct {
	mu   sync.Mutex
	auth string
	from string
	rcpt []string
	data string
}

func startSMTP(t *testing.T) (*fakeSMTP, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f, ln.Addr().(*net.TCPAddr).Port
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(s string) { conn.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		f.mu.Lock()
		switch cmd {
		case "EHLO":
			say("250-fake")
			say("250 AUTH PLAIN LOGIN")
		case "AUTH":
			f.auth = line
			say("235 ok")
		case "MAIL":
			f.from = line
			say("250 ok")
		case "RCPT":
			f.rcpt = append(f.rcpt, line)
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.data = b.String()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			f.mu.Unlock()
			return
		default:
			say("250 ok")
		}
		f.mu.Unlock()
	}
}

func TestSend(t *testing.T) {
	f, port := startSMTP(t)
	n := loadNodes(t)["email.send"]
	cfg := map[string]any{
		"smtpHost": "127.0.0.1", "smtpPort": float64(port), "smtpSecurity": "none",
		"smtpUsername": "bot@example.com", "smtpPassword": "pw", "fromAddress": "Bot <bot@example.com>",
	}
	out, err := run(t, n, cfg, map[string]any{
		"to":          "{{ json.email }}",
		"bcc":         "audit@example.com",
		"subject":     "Hällo {{ json.name }}",
		"text":        "Hi {{ json.name }}",
		"html":        "<b>Hi</b>",
		"attachments": `[{"filename": "a.txt", "content": "` + base64.StdEncoding.EncodeToString([]byte("file")) + `"}]`,
	}, []nodes.Item{{"email": "ada@example.com", "name": "Ada"}})
	if err != nil {
		t.Fatal(err)
	}
	res := out["main"][0]
	if res["from"] != "bot@example.com" || len(res["recipients"].([]any)) != 2 || res["messageId"] == "" {
		t.Fatalf("result: %v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00bot@example.com\x00pw"))
	if f.auth != wantAuth || f.from != "MAIL FROM:<bot@example.com>" || len(f.rcpt) != 2 {
		t.Fatalf("auth=%q from=%q rcpt=%v", f.auth, f.from, f.rcpt)
	}
	for _, want := range []string{"To: <ada@example.com>", "Subject: =?utf-8?q?H=C3=A4llo_Ada?=", "multipart/mixed", "multipart/alternative", "filename=a.txt", "ZmlsZQ=="} {
		if !strings.Contains(f.data, want) {
			t.Errorf("message lacks %q:\n%s", want, f.data)
		}
	}
	if strings.Contains(f.data, "audit@example.com") {
		t.Error("Bcc leaked into the message headers")
	}
}

func TestSendRefusesCleartextCredentials(t *testing.T) {
	n := loadNodes(t)["email.send"]
	_, err := run(t, n, map[string]any{"smtpHost": "mail.example.com", "smtpSecurity": "none", "smtpUsername": "u", "smtpPassword": "p"},
		map[string]any{"to": "a@example.com", "subject": "x", "from": "b@example.com"}, []nodes.Item{{}})
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("expected refusal, got %v", err)
	}
	_, err = run(t, n, map[string]any{}, map[string]any{"to": "a@example.com", "subject": "x"}, []nodes.Item{{}})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected not configured, got %v", err)
	}
}

// --- IMAP -------------------------------------------------------------------

func startIMAP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := imapserver.New(memory.New())
	s.AllowInsecureAuth = true
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	// Add a second, unread message with an attachment and an Archive folder.
	c, err := client.Dial("127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	msg := "From: Grace <grace@example.com>\r\nTo: team@example.com\r\nSubject: =?utf-8?q?Report_f=C3=BCr_Q3?=\r\n" +
		"Message-ID: <r1@example.com>\r\nDate: Mon, 02 Nov 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nNumbers attached.\r\n" +
		"--XX\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=q3.csv\r\n\r\na,b\r\n--XX--\r\n"
	if err := c.Append("INBOX", nil, time.Now(), strings.NewReader(msg)); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestIMAPReadMoveFlags(t *testing.T) {
	port := startIMAP(t)
	all := loadNodes(t)
	cfg := map[string]any{"imapHost": "127.0.0.1", "imapPort": float64(port), "imapSecurity": "none", "imapUsername": "username", "imapPassword": "password"}

	// Only unread: the appended message.
	out, err := run(t, all["email.read"], cfg, map[string]any{"includeAttachments": true, "markSeen": true}, []nodes.Item{{}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := out["main"]
	if len(msgs) != 1 {
		t.Fatalf("want 1 unread message, got %d: %v", len(msgs), msgs)
	}
	m := msgs[0]
	atts := m["attachments"].([]any)
	if m["subject"] != "Report für Q3" || m["from"] != "grace@example.com" || m["fromName"] != "Grace" ||
		!strings.Contains(m["text"].(string), "Numbers attached.") || m["messageId"] != "<r1@example.com>" ||
		len(atts) != 1 || atts[0].(nodes.Item)["filename"] != "q3.csv" || m["seen"] != true {
		t.Fatalf("message: %v", m)
	}

	// Marked as read: nothing unread is left; all messages when unseenOnly is off.
	if out, _ = run(t, all["email.read"], cfg, map[string]any{}, []nodes.Item{{}}); len(out["main"]) != 0 {
		t.Fatalf("message not marked as read: %v", out["main"])
	}
	if out, _ = run(t, all["email.read"], cfg, map[string]any{"unseenOnly": false, "subjectFilter": "little"}, []nodes.Item{{}}); len(out["main"]) != 1 {
		t.Fatalf("subject filter: %v", out["main"])
	}

	// Flag and move, using the read output as input.
	if _, err := run(t, all["email.setFlags"], cfg, map[string]any{"flags": `\Flagged`}, msgs); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, all["email.move"], cfg, map[string]any{"target": "Archive"}, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if out["main"][0]["movedTo"] != "Archive" {
		t.Fatalf("move: %v", out["main"])
	}
	out, err = run(t, all["email.read"], cfg, map[string]any{"mailbox": "Archive", "unseenOnly": false}, []nodes.Item{{}})
	if err != nil || len(out["main"]) != 1 {
		t.Fatalf("archive: %v %v", out, err)
	}
	if flags := out["main"][0]["flags"].([]any); !containsAny(flags, `\Flagged`) {
		t.Fatalf("flags: %v", flags)
	}
}

func containsAny(list []any, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func TestEmailReceivedTrigger(t *testing.T) {
	port := startIMAP(t)
	n := loadNodes(t)["email.received"]
	if n.Definition().Trigger != nodes.TriggerPoll || len(n.Definition().Inputs) != 0 {
		t.Fatalf("not a polling trigger: %+v", n.Definition())
	}
	cfg := map[string]any{"imapHost": "127.0.0.1", "imapPort": float64(port), "imapSecurity": "none", "imapUsername": "username", "imapPassword": "password"}
	state := nodes.NewMemoryState()
	check := func(trigger string, params map[string]any) []nodes.Item {
		t.Helper()
		def := n.Definition()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := n.Execute(ctx, &nodes.ExecContext{
			Node: &workflow.Node{ID: "trig", Name: def.Name, Params: params}, Def: &def, Items: []nodes.Item{}, Config: cfg,
			Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), State: state, TriggerType: trigger,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out["main"]
	}
	add := func(subject string) {
		c, err := client.Dial("127.0.0.1:" + strconv.Itoa(port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Logout()
		if err := c.Login("username", "password"); err != nil {
			t.Fatal(err)
		}
		msg := "From: a@example.com\r\nTo: b@example.com\r\nSubject: " + subject + "\r\n\r\nbody\r\n"
		if err := c.Append("INBOX", nil, time.Now(), strings.NewReader(msg)); err != nil {
			t.Fatal(err)
		}
	}

	// ▶ Run in the editor shows existing mail and remembers nothing.
	if got := check("manual", nil); len(got) != 2 {
		t.Fatalf("test run: %d messages", len(got))
	}
	// First real check: existing mail is ignored.
	if got := check("poll", nil); len(got) != 0 {
		t.Fatalf("first check emitted %d", len(got))
	}
	add("New one")
	got := check("poll", nil)
	if len(got) != 1 || got[0]["subject"] != "New one" {
		t.Fatalf("new mail: %v", got)
	}
	if got := check("poll", nil); len(got) != 0 {
		t.Fatalf("mail emitted twice: %v", got)
	}
	// More new mail than the limit: the rest comes with the next check.
	add("A")
	add("B")
	add("C")
	params := map[string]any{"limit": 2}
	if got = check("poll", params); len(got) != 2 || got[0]["subject"] != "A" {
		t.Fatalf("limited check: %v", got)
	}
	if got = check("poll", params); len(got) != 1 || got[0]["subject"] != "C" {
		t.Fatalf("remaining mail: %v", got)
	}
	// Filters: mail not matching is skipped for good.
	add("ignore me")
	add("Invoice 7")
	if got = check("poll", map[string]any{"subjectFilter": "Invoice"}); len(got) != 1 || got[0]["subject"] != "Invoice 7" {
		t.Fatalf("filtered: %v", got)
	}
}
