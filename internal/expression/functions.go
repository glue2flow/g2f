package expression

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"

	"github.com/expr-lang/expr"

	"github.com/glue2flow/g2f/internal/mailmsg"
)

// functions are helpers available to every expression in addition to the
// expr builtins (toBase64, fromBase64, …).
var functions = []expr.Option{
	// uniq(a) replaces the expr builtin, which compares every element with
	// every kept one (quadratic, unbounded on large arrays and not covered by
	// the step budget). Same result, linear time.
	expr.DisableBuiltin("uniq"),
	expr.Function("uniq", func(params ...any) (any, error) {
		if len(params) != 1 {
			return nil, fmt.Errorf("invalid number of arguments (expected 1, got %d)", len(params))
		}
		v := reflect.ValueOf(params[0])
		if v.Kind() != reflect.Array && v.Kind() != reflect.Slice {
			return nil, fmt.Errorf("cannot uniq %s", v.Kind())
		}
		seen := make(map[any]struct{}, v.Len())
		out := []any{}
		for i := 0; i < v.Len(); i++ {
			e := v.Index(i).Interface()
			k := uniqKey(e)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, e)
		}
		return out, nil
	}, new(func(any) []any)),

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

	// urlEncode(s): query-component escaping (space becomes +), for text that
	// is built into a URL query or form body.
	expr.Function("urlEncode", func(params ...any) (any, error) {
		return url.QueryEscape(Stringify(params[0])), nil
	}, new(func(any) string)),

	// pathEscape(s): escapes s as one URL path segment (space becomes %20, "/"
	// becomes %2F). Path templates do this for every {{ }} block already; the
	// function is for values composed inside rawPath(...).
	expr.Function("pathEscape", func(params ...any) (any, error) {
		return url.PathEscape(Stringify(params[0])), nil
	}, new(func(any) string)),

	// rawPath(s): in a manifest path template, inserts s unescaped (it may hold
	// slashes or be pre-encoded). ".." segments, "?" and "#" are still refused.
	expr.Function("rawPath", func(params ...any) (any, error) {
		return RawPath(Stringify(params[0])), nil
	}, new(func(any) any)),

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

// RawPath is the value of rawPath(x): text that a path template inserts as is
// instead of percent-encoding it as one segment.
type RawPath string

// uniqKey maps a value to a comparable key: equal numbers (1 and 1.0) share a
// key, maps and slices are keyed by their JSON form.
func uniqKey(v any) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := rv.Uint(); u > math.MaxInt64 {
			return u
		}
		return int64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if f == math.Trunc(f) && math.Abs(f) < 1<<62 {
			return int64(f)
		}
		return f
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		b, err := json.Marshal(v)
		if err != nil {
			return new(int) // not comparable: never a duplicate
		}
		return jsonKey(b)
	}
	return v
}

// jsonKey keys maps and slices apart from plain strings.
type jsonKey string
