package nodes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

type captured struct {
	method, path, query, prefer string
	body                        map[string]any
}

// shippedModule loads modules/<file> with its API redirected to a test server
// that answers every request with reply.
func shippedModule(t *testing.T, file string, reply string, got *captured) map[string]Node {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query, got.prefer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Prefer")
		got.body = nil
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		if reply == "" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(api.Close)
	raw, err := os.ReadFile("../../modules/" + file)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.BaseURL = api.URL + "/api"
	m.OAuth2.APIHosts = []string{"127.0.0.1"}
	list, err := HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Node{}
	for _, n := range list {
		out[n.Definition().Type] = n
	}
	return out
}

func runShipped(t *testing.T, n Node, params map[string]any, item Item) Item {
	t.Helper()
	def := n.Definition()
	out, err := n.Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{Name: def.Name, Params: params}, Def: &def, Items: []Item{item},
		OAuth: &OAuthToken{AccessToken: "tok"}, Vars: map[string]any{"defaultTeamId": "team-1", "defaultChannelId": "19:chan@thread.tacv2"},
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("%s: %v", def.Type, err)
	}
	if len(out["main"]) == 0 {
		return nil
	}
	return out["main"][0]
}

func TestGmailModule(t *testing.T) {
	var got captured
	gm := shippedModule(t, "gmail.yaml", `{"id":"m1","threadId":"t1","labelIds":["SENT"]}`, &got)
	res := runShipped(t, gm["gmail.send"], map[string]any{
		"to": "{{ json.email }}", "bcc": "audit@example.com", "subject": "Grüße", "bodyType": "html", "body": "<p>Hi</p>",
		"threadId": "t1", "inReplyTo": "<orig@example.com>",
	}, Item{"email": "Ada <ada@example.com>"})
	if got.path != "/api/messages/send" || got.body["threadId"] != "t1" || res["id"] != "m1" {
		t.Fatalf("send: %+v %v", got, res)
	}
	mime, err := base64.RawURLEncoding.DecodeString(got.body["raw"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`To: "Ada" <ada@example.com>`, "Bcc: <audit@example.com>", "Subject: =?utf-8?q?Gr=C3=BC=C3=9Fe?=",
		"In-Reply-To: <orig@example.com>", "Content-Type: text/html; charset=utf-8", "<p>Hi</p>"} {
		if !strings.Contains(string(mime), want) {
			t.Errorf("raw message lacks %q:\n%s", want, mime)
		}
	}

	// Without a thread, threadId is not sent at all.
	runShipped(t, gm["gmail.send"], map[string]any{"to": "a@example.com", "subject": "s", "body": "b"}, Item{})
	if _, ok := got.body["threadId"]; ok {
		t.Fatalf("empty threadId sent: %v", got.body)
	}

	// Get Message flattens a nested multipart message.
	b64 := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	msg, _ := json.Marshal(map[string]any{
		"id": "m1", "threadId": "t1", "labelIds": []string{"INBOX", "UNREAD"}, "snippet": "Hello",
		"payload": map[string]any{
			"mimeType": "multipart/mixed",
			"headers":  []any{map[string]any{"name": "Subject", "value": "Report"}, map[string]any{"name": "From", "value": "Grace <g@example.com>"}, map[string]any{"name": "Message-ID", "value": "<x@y>"}},
			"parts": []any{
				map[string]any{"mimeType": "multipart/alternative", "parts": []any{
					map[string]any{"mimeType": "text/plain", "filename": "", "body": map[string]any{"data": b64("Hällo text")}},
					map[string]any{"mimeType": "text/html", "filename": "", "body": map[string]any{"data": b64("<b>Hällo</b>")}},
				}},
				map[string]any{"mimeType": "application/pdf", "filename": "q3.pdf", "body": map[string]any{"attachmentId": "att1", "size": 1234}},
			},
		},
	})
	gm = shippedModule(t, "gmail.yaml", string(msg), &got)
	res = runShipped(t, gm["gmail.get"], map[string]any{}, Item{"id": "m1"})
	atts := res["attachments"].([]any)
	if got.path != "/api/messages/m1" || res["subject"] != "Report" || res["from"] != "Grace <g@example.com>" || res["unread"] != true ||
		res["text"] != "Hällo text" || res["html"] != "<b>Hällo</b>" || res["messageId"] != "<x@y>" ||
		len(atts) != 1 || atts[0].(map[string]any)["attachmentId"] != "att1" {
		t.Fatalf("get: %v", res)
	}

	runShipped(t, gm["gmail.modify"], map[string]any{"addLabels": "STARRED, Label_1"}, Item{"id": "m1"})
	if got.path != "/api/messages/m1/modify" || len(got.body["addLabelIds"].([]any)) != 2 || got.body["removeLabelIds"].([]any)[0] != "UNREAD" {
		t.Fatalf("modify: %+v", got)
	}
}

func TestOutlookModule(t *testing.T) {
	var got captured
	ol := shippedModule(t, "outlook.yaml", "", &got)
	res := runShipped(t, ol["outlook.send"], map[string]any{
		"to": `"Doe, Jane" <jane@example.com>, bob@example.com`, "subject": "Hi", "body": "Text",
		"attachments": []any{map[string]any{"filename": "a.txt", "content": "ZmlsZQ=="}},
	}, Item{})
	m := got.body["message"].(map[string]any)
	to := m["toRecipients"].([]any)
	att := m["attachments"].([]any)[0].(map[string]any)
	if got.path != "/api/sendMail" || res["sent"] != true || len(to) != 2 ||
		to[0].(map[string]any)["emailAddress"].(map[string]any)["name"] != "Doe, Jane" ||
		len(m["ccRecipients"].([]any)) != 0 || att["@odata.type"] != "#microsoft.graph.fileAttachment" || att["contentBytes"] != "ZmlsZQ==" {
		t.Fatalf("send: %+v", got.body)
	}

	list := `{"value":[{"id":"a1","subject":"S","isRead":false,"from":{"emailAddress":{"address":"x@example.com","name":"X"}},"toRecipients":[{"emailAddress":{"address":"me@example.com"}}],"body":{"content":"hello"}}]}`
	ol = shippedModule(t, "outlook.yaml", list, &got)
	res = runShipped(t, ol["outlook.list"], map[string]any{}, Item{})
	if got.path != "/api/mailFolders/inbox/messages" || !strings.Contains(got.query, "isRead+eq+false") || got.prefer != `outlook.body-content-type="text"` ||
		res["from"] != "x@example.com" || res["body"] != "hello" || res["to"].([]any)[0] != "me@example.com" {
		t.Fatalf("list: %+v %v", got, res)
	}
	runShipped(t, ol["outlook.list"], map[string]any{"search": "invoice", "filter": "hasAttachments eq true"}, Item{})
	if strings.Contains(got.query, "isRead") || !strings.Contains(got.query, "%24search=%22invoice%22") || !strings.Contains(got.query, "hasAttachments") {
		t.Fatalf("search query: %s", got.query)
	}

	runShipped(t, ol["outlook.reply"], map[string]any{"comment": "Thanks", "replyAll": true}, Item{"id": "a1"})
	if got.path != "/api/messages/a1/replyAll" || got.body["comment"] != "Thanks" {
		t.Fatalf("reply: %+v", got)
	}
	runShipped(t, ol["outlook.markRead"], map[string]any{}, Item{"id": "a1"})
	if got.method != "PATCH" || got.body["isRead"] != true {
		t.Fatalf("markRead: %+v", got)
	}
}

func TestTeamsModule(t *testing.T) {
	var got captured
	tm := shippedModule(t, "ms-teams.yaml", `{"id":"msg1","webUrl":"https://teams/x"}`, &got)
	res := runShipped(t, tm["ms-teams.sendChannelMessage"], map[string]any{"message": "<b>Deploy done</b>"}, Item{})
	body := got.body["body"].(map[string]any)
	if got.path != "/api/teams/team-1/channels/19:chan@thread.tacv2/messages" || body["contentType"] != "html" ||
		body["content"] != "<b>Deploy done</b>" || got.body["subject"] != nil || res["id"] != "msg1" {
		t.Fatalf("channel message: %+v %v", got, res)
	}
	runShipped(t, tm["ms-teams.sendChannelMessage"], map[string]any{"message": "m", "subject": "Release"}, Item{})
	if got.body["subject"] != "Release" {
		t.Fatalf("subject: %+v", got.body)
	}
	runShipped(t, tm["ms-teams.replyToChannelMessage"], map[string]any{"message": "ok"}, Item{"id": "msg1"})
	if got.path != "/api/teams/team-1/channels/19:chan@thread.tacv2/messages/msg1/replies" {
		t.Fatalf("reply: %+v", got)
	}
	runShipped(t, tm["ms-teams.sendChatMessage"], map[string]any{"chatId": "19:c@unq.gbl.spaces", "contentType": "text", "message": "hi"}, Item{})
	if got.path != "/api/chats/19:c@unq.gbl.spaces/messages" || got.body["body"].(map[string]any)["contentType"] != "text" {
		t.Fatalf("chat: %+v", got)
	}
}

func TestMailPollTriggers(t *testing.T) {
	var got captured
	reply := `{"messages":[{"id":"m2","threadId":"t"},{"id":"m1","threadId":"t"}]}`
	gm := shippedModule(t, "gmail.yaml", reply, &got)
	n := gm["gmail.newEmail"]
	def := n.Definition()
	if def.Trigger != TriggerPoll || def.Param(PollParam) == nil || len(def.Inputs) != 0 {
		t.Fatalf("definition: %+v", def)
	}
	state := NewMemoryState()
	check := func(trigger string) []Item {
		t.Helper()
		out, err := n.Execute(context.Background(), &ExecContext{
			Node: &workflow.Node{ID: "g", Name: def.Name, Params: map[string]any{}}, Def: &def, Items: []Item{},
			OAuth: &OAuthToken{AccessToken: "tok"}, State: state, TriggerType: trigger,
			Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out["main"]
	}
	if items := check("manual"); len(items) != 2 || items[0]["id"] != "m1" {
		t.Fatalf("test run (oldest first): %v", items)
	}
	if items := check("poll"); len(items) != 0 {
		t.Fatalf("first check: %v", items)
	}
	if got.query != "maxResults=25&q=in%3Ainbox" {
		t.Fatalf("query %q", got.query)
	}
	// m3 arrives.
	gm = shippedModule(t, "gmail.yaml", `{"messages":[{"id":"m3"},{"id":"m2"},{"id":"m1"}]}`, &got)
	n = gm["gmail.newEmail"]
	if items := check("poll"); len(items) != 1 || items[0]["id"] != "m3" {
		t.Fatalf("new mail: %v", items)
	}
	if items := check("poll"); len(items) != 0 {
		t.Fatalf("repeated: %v", items)
	}

	ol := shippedModule(t, "outlook.yaml", `{"value":[{"id":"b"},{"id":"a"}]}`, &got)
	odef := ol["outlook.newEmail"].Definition()
	out, err := ol["outlook.newEmail"].Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{ID: "o", Params: map[string]any{"unreadOnly": true}}, Def: &odef, Items: []Item{},
		OAuth: &OAuthToken{AccessToken: "tok"}, State: state, TriggerType: "manual",
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || len(out["main"]) != 2 || out["main"][0]["id"] != "a" || !strings.Contains(got.query, "isRead+eq+false") {
		t.Fatalf("outlook: %v %v %s", out, err, got.query)
	}
}
