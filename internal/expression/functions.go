package expression

import (
	"encoding/base64"
	"fmt"

	"github.com/expr-lang/expr"

	"github.com/glue2flow/g2f/internal/mailmsg"
)

// functions are helpers available to every expression in addition to the
// expr builtins (toBase64, fromBase64, …).
var functions = []expr.Option{
	// toBase64Url(s): URL-safe base64 without padding (JWT, Gmail raw messages).
	expr.Function("toBase64Url", func(params ...any) (any, error) {
		return base64.RawURLEncoding.EncodeToString([]byte(Stringify(params[0]))), nil
	}, new(func(any) string)),

	// fromBase64Url(s): decodes URL-safe or standard base64, padded or not
	// (e.g. Gmail message bodies). nil and "" yield "".
	expr.Function("fromBase64Url", func(params ...any) (any, error) {
		s := Stringify(params[0])
		if s == "" {
			return "", nil
		}
		b, err := mailmsg.DecodeBase64(s)
		if err != nil {
			return nil, fmt.Errorf("fromBase64Url: %w", err)
		}
		return string(b), nil
	}, new(func(any) string)),

	// emailAddresses(v): parses "a@x.com, Jane <j@x.com>" (or an array) into
	// [{name, address}], e.g. for Microsoft Graph recipients.
	expr.Function("emailAddresses", func(params ...any) (any, error) {
		list, err := mailmsg.Addresses(params[0])
		if err != nil {
			return nil, fmt.Errorf("emailAddresses: %w", err)
		}
		out := make([]any, len(list))
		for i, a := range list {
			out[i] = map[string]any{"name": a.Name, "address": a.Address}
		}
		return out, nil
	}, new(func(any) []any)),

	// mimeMessage({from, to, cc, bcc, replyTo, subject, text, html, headers,
	// attachments}) builds a complete RFC 5322 e-mail (multipart when needed,
	// UTF-8 safe). The Bcc header is included, as APIs like Gmail expect.
	expr.Function("mimeMessage", func(params ...any) (any, error) {
		m, ok := params[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mimeMessage: expected an object, got %T", params[0])
		}
		msg, err := mailmsg.FromMap(m)
		if err != nil {
			return nil, fmt.Errorf("mimeMessage: %w", err)
		}
		raw, err := mailmsg.Build(msg, true)
		if err != nil {
			return nil, fmt.Errorf("mimeMessage: %w", err)
		}
		return string(raw), nil
	}, new(func(any) string)),
}
