// Telegram plugin for g8g, written in Go and compiled to WebAssembly.
//
// Build with the standard Go toolchain (1.24+):
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ../plugin.wasm .
//
// or with TinyGo (much smaller binary):
//
//	tinygo build -target wasip1 -buildmode=c-shared -o ../plugin.wasm .
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/extism/go-pdk"
)

// request is the JSON the g8g host sends to `execute` (see internal/plugin/wasm.go).
type request struct {
	Node   string         `json:"node"`
	Items  []item         `json:"items"`
	Config map[string]any `json:"config"`
}

type item struct {
	JSON   map[string]any `json:"json"`   // the incoming data item
	Params map[string]any `json:"params"` // node parameters, resolved for this item
}

type response struct {
	Items []map[string]any `json:"items"`
}

//go:wasmexport execute
func execute() int32 {
	var req request
	if err := pdk.InputJSON(&req); err != nil {
		return fail(err)
	}
	out := make([]map[string]any, 0, len(req.Items))
	for i, it := range req.Items {
		res, err := handle(req, it)
		if err != nil {
			return fail(fmt.Errorf("item %d: %w", i, err))
		}
		out = append(out, res)
	}
	if err := pdk.OutputJSON(response{Items: out}); err != nil {
		return fail(err)
	}
	return 0
}

func handle(req request, it item) (map[string]any, error) {
	switch req.Node {
	case "sendMessage":
		payload := map[string]any{
			"chat_id":              str(it.Params["chatId"]),
			"text":                 str(it.Params["text"]),
			"disable_notification": it.Params["silent"] == true,
		}
		if pm := str(it.Params["parseMode"]); pm != "" && pm != "none" {
			payload["parse_mode"] = pm
		}
		return call(req, it, "sendMessage", payload)
	case "getMe":
		return call(req, it, "getMe", map[string]any{})
	default:
		return nil, fmt.Errorf("unknown node %q", req.Node)
	}
}

// call invokes a Bot API method. The token comes from the node (per-user
// secret) or falls back to the organization's default (global config).
func call(req request, it item, method string, payload map[string]any) (map[string]any, error) {
	token := str(it.Params["botToken"])
	if token == "" {
		token = str(req.Config["botToken"])
	}
	if token == "" {
		return nil, errors.New("no bot token: set one on the node or a default on the Plugins page")
	}
	base := strings.TrimRight(str(req.Config["apiBaseUrl"]), "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	r := pdk.NewHTTPRequest(pdk.MethodPost, base+"/bot"+token+"/"+method)
	r.SetHeader("Content-Type", "application/json")
	r.SetBody(body)
	resp := r.Send()

	var tg struct {
		OK          bool           `json:"ok"`
		Result      map[string]any `json:"result"`
		Description string         `json:"description"`
		ErrorCode   int            `json:"error_code"`
	}
	// Never put the URL (it contains the token) into error messages.
	if err := json.Unmarshal(resp.Body(), &tg); err != nil {
		return nil, fmt.Errorf("telegram: unexpected response (HTTP %d)", resp.Status())
	}
	if !tg.OK {
		return nil, fmt.Errorf("telegram: %s (code %d)", tg.Description, tg.ErrorCode)
	}
	return tg.Result, nil
}

func fail(err error) int32 {
	pdk.SetError(err)
	return 1
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// main is required by the Go toolchain but unused: the host calls `execute`.
func main() {}
