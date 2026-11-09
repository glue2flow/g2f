package nodes

import (
	"strings"
	"testing"
)

func formParams() map[string]any {
	return map[string]any{
		"title": "Survey",
		"fields": []any{
			map[string]any{"label": "Your name", "type": "text", "required": true},
			map[string]any{"key": "team", "label": "Team", "type": "choice", "options": "Sales\nSupport\n\nSales"},
			map[string]any{"key": "topics", "label": "Topics", "type": "multichoice", "required": true,
				"options": []any{"A", "B", "C"}},
			map[string]any{"key": "note", "label": "Note", "type": "textarea"},
		},
	}
}

func TestParseForm(t *testing.T) {
	f, err := ParseForm(formParams())
	if err != nil {
		t.Fatal(err)
	}
	if f.Fields[0].Key != "your_name" {
		t.Fatalf("derived key %q", f.Fields[0].Key)
	}
	if got := strings.Join(f.Fields[1].Options, "|"); got != "Sales|Support" {
		t.Fatalf("options %q", got)
	}
	if f.SubmitLabel != "Submit" {
		t.Fatalf("default submit label %q", f.SubmitLabel)
	}
	if _, err := ParseForm(map[string]any{}); err != nil {
		t.Fatalf("default questions should be valid: %v", err)
	}

	bad := []struct {
		name   string
		fields []any
		want   string
	}{
		{"empty", []any{}, "at least one"},
		{"no label", []any{map[string]any{"type": "text"}}, "label"},
		{"bad key", []any{map[string]any{"key": "1x", "label": "X"}}, "may only contain"},
		{"reserved", []any{map[string]any{"key": "submittedAt", "label": "X"}}, "reserved"},
		{"dup", []any{map[string]any{"key": "a", "label": "A"}, map[string]any{"key": "a", "label": "B"}}, "duplicate"},
		{"no options", []any{map[string]any{"label": "X", "type": "choice"}}, "option"},
		{"type", []any{map[string]any{"label": "X", "type": "date"}}, "unknown type"},
	}
	for _, c := range bad {
		_, err := ParseForm(map[string]any{"fields": c.fields})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
}

func TestFormSubmit(t *testing.T) {
	f, err := ParseForm(formParams())
	if err != nil {
		t.Fatal(err)
	}
	item, errs := f.Submit(map[string][]string{
		"your_name": {"  Ada   Lovelace "},
		"team":      {"Support"},
		"topics":    {"C", "A"},
		"note":      {"line 1\r\nline 2"},
		"extra":     {"ignored"},
	})
	if errs != nil {
		t.Fatal(errs)
	}
	if item["your_name"] != "Ada Lovelace" || item["team"] != "Support" || item["note"] != "line 1\nline 2" {
		t.Fatalf("unexpected item %v", item)
	}
	if topics := item["topics"].([]any); len(topics) != 2 || topics[0] != "A" || topics[1] != "C" {
		t.Fatalf("topics %v", topics)
	}
	if _, ok := item["extra"]; ok {
		t.Fatal("unknown field accepted")
	}
	if item[FormSubmittedAt] == "" {
		t.Fatal("missing submittedAt")
	}

	_, errs = f.Submit(map[string][]string{"team": {"Hacker"}, "topics": {"Z"}, "note": {strings.Repeat("x", 10001)}})
	for _, k := range []string{"your_name", "team", "topics", "note"} {
		if errs[k] == "" {
			t.Errorf("expected an error for %s, got %v", k, errs)
		}
	}
}

func TestFormKey(t *testing.T) {
	for in, want := range map[string]string{"E-mail address": "e_mail_address", "  Größe? ": "groesse", "1st choice": "q_1st_choice", "!!!": "field"} {
		if got := FormKey(in); got != want {
			t.Errorf("FormKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormProtection(t *testing.T) {
	f, err := ParseForm(map[string]any{"protection": "email", "emailDomains": "Example.com, @acme.org"})
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"a@example.com": true, "a@mail.acme.org": true, "a@notexample.com": false, "a@example.com.evil": false,
	} {
		if got := f.EmailAllowed(addr); got != want {
			t.Errorf("EmailAllowed(%q) = %v", addr, got)
		}
	}
	if _, err := ParseForm(map[string]any{"protection": "magic"}); err == nil {
		t.Fatal("unknown protection accepted")
	}
	if _, err := ParseForm(map[string]any{"emailDomains": "localhost"}); err == nil {
		t.Fatal("invalid domain accepted")
	}
	if _, err := ParseForm(map[string]any{"fields": []any{map[string]any{"key": "submittedBy", "label": "X"}}}); err == nil {
		t.Fatal("reserved key accepted")
	}
	if f, _ := ParseForm(nil); f.Protection != FormProtectNone {
		t.Fatalf("default protection %q", f.Protection)
	}
}
