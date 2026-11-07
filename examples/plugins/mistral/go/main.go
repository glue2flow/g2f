// Mistral AI plugin for glue2flow, written in Go and compiled to WebAssembly.
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
	"strconv"
	"strings"

	"github.com/extism/go-pdk"
)

const defaultBaseURL = "https://api.mistral.ai"

// request is the JSON the glue2flow host sends to `execute` (see internal/plugin/wasm.go).
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

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	case "chat":
		return chat(req, it)
	default:
		return nil, fmt.Errorf("unknown node %q", req.Node)
	}
}

// chat sends the prompt to the chat completions endpoint and returns the answer.
func chat(req request, it item) (map[string]any, error) {
	key := str(it.Params["apiKey"])
	if key == "" {
		key = str(req.Config["apiKey"])
	}
	if key == "" {
		return nil, errors.New("no API key: set one on the node or a default on the Plugins page")
	}
	model := strings.TrimSpace(str(it.Params["model"]))
	if model == "" {
		return nil, errors.New("model is required")
	}
	prompt := str(it.Params["prompt"])
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}

	var msgs []message
	if sys := str(it.Params["systemPrompt"]); sys != "" {
		msgs = append(msgs, message{Role: "system", Content: sys})
	}
	msgs = append(msgs, message{Role: "user", Content: prompt})

	payload := map[string]any{"model": model, "messages": msgs}
	if s := strings.TrimSpace(str(it.Params["temperature"])); s != "" {
		t, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid temperature %q", s)
		}
		payload["temperature"] = t
	}
	if s := strings.TrimSpace(str(it.Params["maxTokens"])); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid max tokens %q", s)
		}
		payload["max_tokens"] = n
	}
	if it.Params["jsonMode"] == true {
		payload["response_format"] = map[string]any{"type": "json_object"}
	}

	base := strings.TrimRight(str(req.Config["apiBaseUrl"]), "/")
	if base == "" {
		base = defaultBaseURL
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	r := pdk.NewHTTPRequest(pdk.MethodPost, base+"/v1/chat/completions")
	r.SetHeader("Content-Type", "application/json")
	r.SetHeader("Accept", "application/json")
	r.SetHeader("Authorization", "Bearer "+key)
	r.SetBody(body)
	resp := r.Send()

	var mr struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		// Error shapes: {"message": "..."} or {"error": {"message": "..."}}.
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body(), &mr); err != nil {
		return nil, fmt.Errorf("mistral: unexpected response (HTTP %d)", resp.Status())
	}
	if resp.Status() >= 300 {
		msg := mr.Message
		if mr.Error != nil && mr.Error.Message != "" {
			msg = mr.Error.Message
		}
		return nil, fmt.Errorf("mistral: %s (HTTP %d)", msg, resp.Status())
	}
	if len(mr.Choices) == 0 {
		return nil, errors.New("mistral: response contained no choices")
	}
	c := mr.Choices[0]
	text := content(c.Message.Content)

	out := map[string]any{
		"text":         text,
		"model":        mr.Model,
		"finishReason": c.FinishReason,
		"usage": map[string]any{
			"promptTokens":     mr.Usage.PromptTokens,
			"completionTokens": mr.Usage.CompletionTokens,
			"totalTokens":      mr.Usage.TotalTokens,
		},
	}
	if it.Params["jsonMode"] == true {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			out["data"] = parsed
		}
	}
	return out, nil
}

// content flattens message content, which is a string or a list of chunks.
func content(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
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
