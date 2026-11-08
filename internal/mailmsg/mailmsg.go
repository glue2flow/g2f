// Package mailmsg builds RFC 5322 / MIME e-mail messages. It is used by the
// SMTP module and by the mimeMessage() expression function (e.g. for the Gmail
// API, which expects a complete raw message).
package mailmsg

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

// Message is an e-mail to be encoded.
type Message struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     []string
	Subject     string
	Text        string
	HTML        string
	Headers     map[string]string // extra headers, e.g. In-Reply-To, References
	Attachments []Attachment
	Date        time.Time // default: now
}

// Attachment is a file attached to a message.
type Attachment struct {
	Filename    string
	ContentType string // default: derived from the file name
	Content     []byte
}

// Recipients returns all envelope recipients (To, Cc and Bcc) as bare addresses.
func (m *Message) Recipients() ([]string, error) {
	var out []string
	for _, list := range [][]string{m.To, m.Cc, m.Bcc} {
		addrs, err := parseList(list)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			out = append(out, a.Address)
		}
	}
	return out, nil
}

// FromAddress returns the bare address of From.
func (m *Message) FromAddress() (string, error) {
	a, err := mail.ParseAddress(m.From)
	if err != nil {
		return "", fmt.Errorf("from: %w", err)
	}
	return a.Address, nil
}

// protected headers are set by Build and cannot be overridden through Headers.
var protected = map[string]bool{
	"From": true, "To": true, "Cc": true, "Bcc": true, "Reply-To": true, "Subject": true,
	"Mime-Version": true, "Content-Type": true, "Content-Transfer-Encoding": true,
}

// Build encodes the message. With includeBcc the Bcc header is written (APIs
// like Gmail read recipients from it and strip it); SMTP senders pass false
// and hand the Bcc addresses to the server as envelope recipients only.
func Build(m *Message, includeBcc bool) ([]byte, error) {
	if m.Text == "" && m.HTML == "" && len(m.Attachments) == 0 {
		m.Text = " "
	}
	var h bytes.Buffer
	write := func(k, v string) error {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %s must not contain line breaks", k)
		}
		fmt.Fprintf(&h, "%s: %s\r\n", k, v)
		return nil
	}
	addrHeader := func(k string, list []string) error {
		addrs, err := parseList(list)
		if err != nil {
			return fmt.Errorf("%s: %w", strings.ToLower(k), err)
		}
		if len(addrs) == 0 {
			return nil
		}
		parts := make([]string, len(addrs))
		for i, a := range addrs {
			parts[i] = a.String()
		}
		return write(k, strings.Join(parts, ", "))
	}

	if m.From != "" {
		if err := addrHeader("From", []string{m.From}); err != nil {
			return nil, err
		}
	}
	if err := addrHeader("To", m.To); err != nil {
		return nil, err
	}
	if err := addrHeader("Cc", m.Cc); err != nil {
		return nil, err
	}
	if includeBcc {
		if err := addrHeader("Bcc", m.Bcc); err != nil {
			return nil, err
		}
	}
	if err := addrHeader("Reply-To", m.ReplyTo); err != nil {
		return nil, err
	}
	if err := write("Subject", mime.QEncoding.Encode("utf-8", m.Subject)); err != nil {
		return nil, err
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	_ = write("Date", date.Format(time.RFC1123Z))

	keys := make([]string, 0, len(m.Headers))
	hasMsgID := false
	for k := range m.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ck := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(k))
		if ck == "" || strings.ContainsAny(ck, " :\r\n") {
			return nil, fmt.Errorf("invalid header name %q", k)
		}
		if protected[ck] || ck == "Date" {
			continue
		}
		if m.Headers[k] == "" {
			continue
		}
		if ck == "Message-Id" {
			hasMsgID = true
			ck = "Message-ID"
		}
		if err := write(ck, mime.QEncoding.Encode("utf-8", m.Headers[k])); err != nil {
			return nil, err
		}
	}
	if !hasMsgID {
		_ = write("Message-ID", messageID(m.From))
	}
	_ = write("MIME-Version", "1.0")

	var body bytes.Buffer
	switch {
	case len(m.Attachments) > 0:
		mw := multipart.NewWriter(&body)
		_ = write("Content-Type", `multipart/mixed; boundary="`+mw.Boundary()+`"`)
		if m.Text != "" || m.HTML != "" {
			hdr, content, err := alternative(m)
			if err != nil {
				return nil, err
			}
			pw, err := mw.CreatePart(hdr)
			if err != nil {
				return nil, err
			}
			pw.Write(content)
		}
		for _, a := range m.Attachments {
			if err := attach(mw, a); err != nil {
				return nil, err
			}
		}
		mw.Close()
	default:
		hdr, content, err := alternative(m)
		if err != nil {
			return nil, err
		}
		for _, k := range []string{"Content-Type", "Content-Transfer-Encoding"} {
			_ = write(k, hdr.Get(k))
		}
		body.Write(content)
	}
	h.WriteString("\r\n")
	h.Write(body.Bytes())
	return h.Bytes(), nil
}

// alternative encodes the text and/or HTML body as one part.
func alternative(m *Message) (textproto.MIMEHeader, []byte, error) {
	if m.Text != "" && m.HTML != "" {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for _, p := range []struct{ typ, s string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
			pw, err := mw.CreatePart(textHeader(p.typ))
			if err != nil {
				return nil, nil, err
			}
			pw.Write(qp(p.s))
		}
		mw.Close()
		return textproto.MIMEHeader{"Content-Type": {`multipart/alternative; boundary="` + mw.Boundary() + `"`}}, b.Bytes(), nil
	}
	if m.HTML != "" {
		return textHeader("text/html"), qp(m.HTML), nil
	}
	return textHeader("text/plain"), qp(m.Text), nil
}

func textHeader(typ string) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Type":              {typ + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	}
}

func qp(s string) []byte {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func attach(mw *multipart.Writer, a Attachment) error {
	name := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '"' || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, a.Filename)
	if name == "" {
		name = "attachment"
	}
	ct := a.ContentType
	if ct == "" {
		if i := strings.LastIndex(name, "."); i >= 0 {
			ct = mime.TypeByExtension(name[i:])
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
	}
	if _, _, err := mime.ParseMediaType(ct); err != nil || strings.ContainsAny(ct, "\r\n") {
		return fmt.Errorf("attachment %s: invalid content type %q", name, ct)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType(strings.Split(ct, ";")[0], map[string]string{"name": name}))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	h.Set("Content-Transfer-Encoding", "base64")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(a.Content)
	for len(enc) > 76 {
		pw.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	pw.Write([]byte(enc + "\r\n"))
	return nil
}

func messageID(from string) string {
	domain := "glue2flow.local"
	if a, err := mail.ParseAddress(from); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			domain = a.Address[i+1:]
		}
	}
	b := make([]byte, 12)
	rand.Read(b)
	return "<" + hex.EncodeToString(b) + "@" + domain + ">"
}

func parseList(list []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(s)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", s, err)
		}
		out = append(out, addrs...)
	}
	return out, nil
}

// --- conversion from loosely typed workflow data ---------------------------

// FromMap builds a message from a JSON object such as
//
//	{from, to, cc, bcc, replyTo, subject, text, html, headers: {...},
//	 attachments: [{filename, content (base64), contentType}]}
//
// Address fields take a comma separated string or an array.
func FromMap(v map[string]any) (*Message, error) {
	m := &Message{
		From:    str(v["from"]),
		To:      List(v["to"]),
		Cc:      List(v["cc"]),
		Bcc:     List(v["bcc"]),
		ReplyTo: List(v["replyTo"]),
		Subject: str(v["subject"]),
		Text:    str(v["text"]),
		HTML:    str(v["html"]),
	}
	if h, ok := v["headers"].(map[string]any); ok {
		m.Headers = map[string]string{}
		for k, e := range h {
			m.Headers[k] = str(e)
		}
	}
	atts, err := Attachments(v["attachments"])
	if err != nil {
		return nil, err
	}
	m.Attachments = atts
	return m, nil
}

// Attachments reads [{filename, content, contentType}] where content is base64
// (standard or URL alphabet). A single object is accepted as well.
func Attachments(v any) ([]Attachment, error) {
	var list []any
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return nil, errors.New("attachments: expected a list of {filename, content, contentType}")
	case []any:
		list = t
	case map[string]any:
		list = []any{t}
	default:
		return nil, fmt.Errorf("attachments: unexpected %T", v)
	}
	out := make([]Attachment, 0, len(list))
	for i, e := range list {
		o, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("attachment %d: expected an object", i)
		}
		data, err := DecodeBase64(str(o["content"]))
		if err != nil {
			return nil, fmt.Errorf("attachment %d: content must be base64: %w", i, err)
		}
		out = append(out, Attachment{Filename: str(o["filename"]), ContentType: str(o["contentType"]), Content: data})
	}
	return out, nil
}

// DecodeBase64 accepts standard and URL-safe base64, padded or not, and
// ignores line breaks.
func DecodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', ' ', '\t':
			return -1
		case '-':
			return '+'
		case '_':
			return '/'
		}
		return r
	}, s)
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Addresses parses addresses given as a comma separated string, an array, or
// {name, address} objects.
func Addresses(v any) ([]*mail.Address, error) {
	return parseList(List(v))
}

// List reads a comma separated string or an array of strings. Commas inside
// quoted display names ("Doe, Jane" <jane@example.com>) are kept.
func List(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, List(e)...)
		}
		return out
	case []string:
		return t
	case map[string]any: // {name, address} / {name, email}
		addr := str(t["address"])
		if addr == "" {
			addr = str(t["email"])
		}
		if addr == "" {
			return nil
		}
		return []string{(&mail.Address{Name: str(t["name"]), Address: addr}).String()}
	}
	return []string{fmt.Sprint(v)}
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v)
}
