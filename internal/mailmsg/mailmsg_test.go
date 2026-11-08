package mailmsg

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	m := &Message{From: "Bot <bot@example.com>", To: []string{`"Doe, Jane" <jane@example.com>, bob@example.com`}, Bcc: []string{"x@example.com"},
		Subject: "Grüße", Text: "Zeile 1\nZeile 2 ä"}
	raw, err := Build(m, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`To: "Doe, Jane" <jane@example.com>, <bob@example.com>`, "Subject: =?utf-8?q?Gr=C3=BC=C3=9Fe?=",
		"Message-ID: <", "@example.com>", "text/plain; charset=utf-8", "Zeile 1\r\nZeile 2 =C3=A4"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "Bcc") {
		t.Error("Bcc header written for SMTP")
	}
	rcpts, _ := m.Recipients()
	if strings.Join(rcpts, ",") != "jane@example.com,bob@example.com,x@example.com" {
		t.Errorf("recipients %v", rcpts)
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	for _, m := range []*Message{
		{To: []string{"a@example.com"}, Subject: "x\r\nBcc: evil@example.com"},
		{To: []string{"a@example.com"}, Headers: map[string]string{"X-A": "1\nBcc: evil@example.com"}},
		{To: []string{"a@example.com\r\nBcc: evil@example.com"}},
	} {
		// Either refused, or encoded so that no extra header line appears.
		if raw, err := Build(m, true); err == nil && strings.Contains(string(raw), "\r\nBcc:") {
			t.Errorf("header injected:\n%s", raw)
		}
	}
}

func TestDecodeBase64(t *testing.T) {
	for _, in := range []string{"aGk_Pz8-", "aGk/Pz8+", "aGk_Pz8-\r\n", "aGk/Pz8+"} {
		if b, err := DecodeBase64(in); err != nil || string(b) != "hi???>" {
			t.Errorf("%q: %q %v", in, b, err)
		}
	}
}
