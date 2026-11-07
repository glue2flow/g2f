package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wolfposd/g8g/internal/expression"
)

const maxResponseBody = 10 << 20 // 10 MiB

var httpClient = &http.Client{Timeout: 60 * time.Second}

// httpRequest is shared by the HTTP Request node and declarative modules.
type httpRequest struct {
	Method  string
	URL     string
	Headers map[string]any
	Query   map[string]any
	Body    any
	Timeout time.Duration
}

func doHTTP(ctx context.Context, r httpRequest) (Item, error) {
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid URL %q", r.URL)
	}
	if len(r.Query) > 0 {
		q := u.Query()
		for k, v := range r.Query {
			if s := expression.Stringify(v); s != "" {
				q.Set(k, s)
			}
		}
		u.RawQuery = q.Encode()
	}

	var body io.Reader
	contentType := ""
	switch b := r.Body.(type) {
	case nil:
	case string:
		if b != "" {
			body = strings.NewReader(b)
			if json.Valid([]byte(b)) {
				contentType = "application/json"
			}
		}
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		body = bytes.NewReader(buf)
		contentType = "application/json"
	}

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(r.Method), u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "g8g-workflow/1.0")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range r.Headers {
		// Empty headers are dropped so manifests can make headers conditional.
		if s := expression.Stringify(v); s != "" {
			req.Header.Set(k, s)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	headers := map[string]any{}
	for k := range resp.Header {
		headers[strings.ToLower(k)] = resp.Header.Get(k)
	}
	var parsed any = string(raw)
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || json.Valid(raw) {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			parsed = v
		}
	}
	return Item{"status": resp.StatusCode, "headers": headers, "body": parsed}, nil
}

func toMap(v any) (map[string]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return t, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err != nil {
			return nil, fmt.Errorf("expected a JSON object: %w", err)
		}
		return m, nil
	}
	return nil, fmt.Errorf("expected an object, got %T", v)
}
