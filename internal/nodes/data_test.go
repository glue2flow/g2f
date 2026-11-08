package nodes

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

func field(items []Item, name string) []any {
	out := []any{}
	for _, it := range items {
		out = append(out, it[name])
	}
	return out
}

func TestFilterSortLimit(t *testing.T) {
	items := []Item{{"n": "b", "v": 10.0}, {"n": "A", "v": 2.0}, {"n": "c", "v": "30"}, {"n": "d"}}
	out := runNode(t, filterNode{}, map[string]any{"condition": "{{ json.v != nil }}"}, items, nil)
	if len(out["main"]) != 3 || len(out["discarded"]) != 1 {
		t.Fatalf("filter: %v", out)
	}
	out = runNode(t, sortNode{}, map[string]any{"by": "{{ json.v }}"}, items, nil)
	if got := field(out["main"], "n"); !reflect.DeepEqual(got, []any{"A", "b", "c", "d"}) {
		t.Fatalf("numeric sort (nil last): %v", got)
	}
	out = runNode(t, sortNode{}, map[string]any{"by": "{{ json.n }}", "order": "desc"}, items, nil)
	if got := field(out["main"], "n"); !reflect.DeepEqual(got, []any{"d", "c", "b", "A"}) {
		t.Fatalf("text sort desc, case-insensitive: %v", got)
	}
	dates := []Item{{"d": "2026-03-01T10:00:00+01:00"}, {"d": "2026-03-01T08:30:00Z"}}
	out = runNode(t, sortNode{}, map[string]any{"by": "{{ json.d }}"}, dates, nil)
	if out["main"][0]["d"] != "2026-03-01T08:30:00Z" {
		t.Fatalf("date sort: %v", out["main"])
	}
	multi := []Item{{"a": "x", "b": 2.0}, {"a": "x", "b": 1.0}, {"a": "w", "b": 3.0}}
	out = runNode(t, sortNode{}, map[string]any{"by": "{{ [json.a, json.b] }}"}, multi, nil)
	if got := field(out["main"], "b"); !reflect.DeepEqual(got, []any{3.0, 1.0, 2.0}) {
		t.Fatalf("multi-key sort: %v", got)
	}
	out = runNode(t, limitNode{}, map[string]any{"max": 2.0, "keep": "last"}, items, nil)
	if got := field(out["main"], "n"); !reflect.DeepEqual(got, []any{"c", "d"}) {
		t.Fatalf("limit last: %v", got)
	}
}

func TestSplitOutAggregate(t *testing.T) {
	orders := []Item{
		{"id": 1, "customer": "acme", "lines": []any{map[string]any{"sku": "a", "qty": 2.0}, map[string]any{"sku": "b", "qty": 1.0}}},
		{"id": 2, "customer": "zeta", "lines": []any{map[string]any{"sku": "a", "qty": 5.0}}},
		{"id": 3, "customer": "acme", "lines": []any{}},
	}
	out := runNode(t, splitOutNode{}, map[string]any{"list": "{{ json.lines }}"}, orders, nil)
	if len(out["main"]) != 3 || out["main"][2]["sku"] != "a" {
		t.Fatalf("split: %v", out["main"])
	}
	out = runNode(t, splitOutNode{}, map[string]any{"list": "{{ json.lines }}", "include": "withParent", "field": "line"}, orders, nil)
	if out["main"][1]["id"] != 1 || out["main"][1]["line"].(map[string]any)["sku"] != "b" {
		t.Fatalf("split with parent: %v", out["main"])
	}
	out = runNode(t, splitOutNode{}, map[string]any{"list": "{{ ['x', 'y'] }}"}, []Item{{}}, nil)
	if out["main"][1]["item"] != "y" {
		t.Fatalf("split scalars: %v", out["main"])
	}

	lines := runNode(t, splitOutNode{}, map[string]any{"list": "{{ json.lines }}", "include": "withParent", "field": "line"}, orders, nil)["main"]
	out = runNode(t, aggregateNode{}, map[string]any{"operation": "sum", "value": "{{ json.line.qty }}", "groupBy": "{{ json.customer }}", "field": "qty"}, lines, nil)
	if len(out["main"]) != 2 || out["main"][0]["group"] != "acme" || out["main"][0]["qty"] != 3.0 || out["main"][0]["count"] != 2 {
		t.Fatalf("sum by group: %v", out["main"])
	}
	out = runNode(t, aggregateNode{}, nil, orders, nil)
	if len(out["main"]) != 1 || len(out["main"][0]["result"].([]any)) != 3 {
		t.Fatalf("collect items: %v", out["main"])
	}
	for op, want := range map[string]any{"join": "acme, zeta, acme", "unique": []any{"acme", "zeta"}, "min": "acme", "max": "zeta", "count": 3, "first": "acme"} {
		out = runNode(t, aggregateNode{}, map[string]any{"operation": op, "value": "{{ json.customer }}"}, orders, nil)
		if got := out["main"][0]["result"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", op, got, want)
		}
	}
	out = runNode(t, aggregateNode{}, map[string]any{"operation": "average", "value": "{{ json.id }}"}, orders, nil)
	if out["main"][0]["result"] != 2.0 {
		t.Fatalf("average: %v", out["main"])
	}
	if _, err := runNodeErr(aggregateNode{}, map[string]any{"operation": "sum", "value": "{{ json.customer }}"}, orders, nil); err == nil {
		t.Fatal("sum of text accepted")
	}
}

func TestMapAndEditFields(t *testing.T) {
	items := []Item{{"first": "Ada", "last": "L", "skip": true}, {"first": "Bob", "last": "M"}}
	out := runNode(t, mapNode{}, map[string]any{"expression": "{{ json.skip == true ? nil : {name: json.first + ' ' + json.last} }}"}, items, nil)
	if len(out["main"]) != 1 || out["main"][0]["name"] != "Bob M" {
		t.Fatalf("map: %v", out["main"])
	}
	in := []Item{{"mail": "a@x", "password": "p", "addr": map[string]any{"zip": "1", "city": "B"}}}
	out = runNode(t, editFieldsNode{}, map[string]any{
		"rename": map[string]any{"mail": "email", "addr.zip": "postal.code"}, "remove": "password",
	}, in, nil)
	want := Item{"email": "a@x", "addr": map[string]any{"city": "B"}, "postal": map[string]any{"code": "1"}}
	if !reflect.DeepEqual(out["main"][0], want) {
		t.Fatalf("edit: %v", out["main"][0])
	}
	if _, ok := in[0]["mail"]; !ok {
		t.Fatal("input item was modified")
	}
	out = runNode(t, editFieldsNode{}, map[string]any{"keepOnly": "mail, addr.city"}, in, nil)
	if !reflect.DeepEqual(out["main"][0], Item{"mail": "a@x", "addr": map[string]any{"city": "B"}}) {
		t.Fatalf("keep only: %v", out["main"][0])
	}
}

func TestDateTime(t *testing.T) {
	run := func(params map[string]any) any {
		t.Helper()
		return runNode(t, dateTimeNode{}, params, []Item{{"d": "2026-03-28T22:30:00Z", "n": 1}}, nil)["main"][0]["date"]
	}
	cases := []struct {
		params map[string]any
		want   any
	}{
		{map[string]any{"date": "{{ json.d }}", "format": "custom", "customFormat": "EEE, d. MMMM yyyy HH:mm 'Uhr' (Z)", "timezone": "Europe/Berlin"},
			"Sat, 28. March 2026 23:30 Uhr (+01:00)"},
		// Adding a calendar day across the DST change keeps the wall clock.
		{map[string]any{"operation": "add", "date": "{{ json.d }}", "amount": 1, "unit": "days", "timezone": "Europe/Berlin"}, "2026-03-29T23:30:00+02:00"},
		{map[string]any{"operation": "subtract", "date": "2026-03-31", "amount": 1, "unit": "months", "format": "date"}, "2026-03-03"},
		{map[string]any{"operation": "startOf", "date": "{{ json.d }}", "period": "week", "format": "date"}, "2026-03-23"},
		{map[string]any{"operation": "endOf", "date": "{{ json.d }}", "period": "month"}, "2026-03-31T23:59:59.999Z"},
		{map[string]any{"operation": "startOf", "date": "2026-08-15", "period": "quarter", "format": "date"}, "2026-07-01"},
		{map[string]any{"operation": "difference", "date": "2026-01-31", "endDate": "2026-03-30", "unit": "months"}, 1.0},
		{map[string]any{"operation": "difference", "date": "{{ json.d }}", "endDate": "2026-03-30T22:30:00Z", "unit": "hours"}, 48.0},
		{map[string]any{"operation": "parse", "date": "28.03.2026 9:05", "inputFormat": "dd.MM.yyyy H:mm", "timezone": "Europe/Berlin"}, "2026-03-28T09:05:00+01:00"},
		{map[string]any{"date": "Sat, 28 Mar 2026 22:30:00 +0000", "format": "unix"}, int64(1774737000)},
		{map[string]any{"date": 1774737000000.0, "format": "iso"}, "2026-03-28T22:30:00Z"},
		{map[string]any{"date": "2026-03-28 10:00", "timezone": "America/New_York", "format": "custom", "customFormat": "h:mm a z"}, "10:00 AM EDT"},
	}
	for _, c := range cases {
		if got := run(c.params); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %#v, want %#v", c.params, got, c.want)
		}
	}
	// now() (a time value) works as input.
	if got := run(map[string]any{"format": "date"}).(string); got != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("now: %v", got)
	}
	if _, err := runNodeErr(dateTimeNode{}, map[string]any{"date": "not a date"}, []Item{{}}, nil); err == nil {
		t.Error("invalid date accepted")
	}
}

func TestCSV(t *testing.T) {
	text := "\ufeffname;city;note\nAda;Berlin;\"a;b\"\nBob;Paris\n"
	out := runNode(t, csvNode{}, map[string]any{"text": "{{ json.body }}"}, []Item{{"body": text}}, nil)
	if len(out["main"]) != 2 || out["main"][0]["note"] != "a;b" || out["main"][1]["city"] != "Paris" {
		t.Fatalf("parse: %v", out["main"])
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("a,b\n1,2\n"))
	out = runNode(t, csvNode{}, map[string]any{"text": b64}, []Item{{}}, nil)
	if out["main"][0]["b"] != "2" {
		t.Fatalf("base64: %v", out["main"])
	}
	out = runNode(t, csvNode{}, map[string]any{"text": "1,2\n3,4", "header": false}, []Item{{}}, nil)
	if out["main"][1]["column2"] != "4" {
		t.Fatalf("no header: %v", out["main"])
	}

	items := []Item{{"id": 1, "name": "Ada, L.", "c": map[string]any{"x": 1}}, {"id": 2, "extra": true}}
	out = runNode(t, csvNode{}, map[string]any{"operation": "generate"}, items, nil)
	res := out["main"][0]
	if res["csv"] != "c,id,name,extra\n\"{\"\"x\"\":1}\",1,\"Ada, L.\",\n,2,,true\n" || res["rows"] != 2 || res["filename"] != "export.csv" {
		t.Fatalf("generate: %q", res["csv"])
	}
	out = runNode(t, csvNode{}, map[string]any{"operation": "generate", "columns": "id, c.x", "delimiter": ";", "filename": "r-{{ json.id }}.csv"}, items, nil)
	res = out["main"][0]
	raw, _ := base64.StdEncoding.DecodeString(res["content"].(string))
	if string(raw) != "id;c.x\n1;1\n2;\n" || res["filename"] != "r-1.csv" || !strings.HasPrefix(res["contentType"].(string), "text/csv") {
		t.Fatalf("generate columns: %q %v", raw, res)
	}
}
