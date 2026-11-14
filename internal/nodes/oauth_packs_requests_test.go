package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

func TestHubSpotModule(t *testing.T) {
	var got captured
	hs := shippedModule(t, "hubspot.yaml", `{"id":"101","createdAt":"c","updatedAt":"u","archived":false,"properties":{"email":"ada@example.com","firstname":"Ada"}}`, &got)
	res := runShipped(t, hs["hubspot.getContact"], map[string]any{"contactId": "ada@example.com", "idProperty": "email"}, Item{})
	if got.method != "GET" || got.path != "/api/crm/v3/objects/contacts/ada@example.com" || !strings.Contains(got.query, "idProperty=email") ||
		res["id"] != "101" || res["email"] != "ada@example.com" || res["updatedAt"] != "u" {
		t.Fatalf("get: %+v %v", got, res)
	}
	runShipped(t, hs["hubspot.getContact"], map[string]any{"contactId": "{{ json.id }}"}, Item{"id": "7"})
	if got.path != "/api/crm/v3/objects/contacts/7" || strings.Contains(got.query, "idProperty") {
		t.Fatalf("get by id: %+v", got)
	}

	runShipped(t, hs["hubspot.createContact"], map[string]any{"properties": map[string]any{"email": "{{ json.email }}"}}, Item{"email": "g@example.com"})
	if got.method != "POST" || got.path != "/api/crm/v3/objects/contacts" || got.body["properties"].(map[string]any)["email"] != "g@example.com" {
		t.Fatalf("create: %+v", got)
	}
	runShipped(t, hs["hubspot.updateContact"], map[string]any{"contactId": "9", "properties": map[string]any{"phone": "1"}}, Item{})
	if got.method != "PATCH" || got.path != "/api/crm/v3/objects/contacts/9" {
		t.Fatalf("update: %+v", got)
	}

	list := `{"total":1,"results":[{"id":"5","createdAt":"c","updatedAt":"u","properties":{"email":"x@example.com"}}]}`
	hs = shippedModule(t, "hubspot.yaml", list, &got)
	res = runShipped(t, hs["hubspot.searchContacts"], map[string]any{"filterProperty": "email", "operator": "CONTAINS_TOKEN", "filterValue": "example.com", "limit": 5}, Item{})
	filters := got.body["filterGroups"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	props := got.body["properties"].([]any)
	if got.path != "/api/crm/v3/objects/contacts/search" || filters["propertyName"] != "email" || filters["operator"] != "CONTAINS_TOKEN" ||
		props[0] != "email" || props[1] != "firstname" || got.body["limit"] != float64(5) || got.body["query"] != nil ||
		res["id"] != "5" || res["email"] != "x@example.com" {
		t.Fatalf("search: %+v %v", got.body, res)
	}
	runShipped(t, hs["hubspot.searchDeals"], map[string]any{"query": "renewal"}, Item{})
	if got.path != "/api/crm/v3/objects/deals/search" || got.body["query"] != "renewal" || got.body["filterGroups"] != nil {
		t.Fatalf("search deals: %+v", got.body)
	}

	runShipped(t, hs["hubspot.createDeal"], map[string]any{"properties": map[string]any{"dealname": "D"}, "contactId": "101"}, Item{})
	assoc := got.body["associations"].([]any)[0].(map[string]any)
	if got.path != "/api/crm/v3/objects/deals" || assoc["to"].(map[string]any)["id"] != "101" ||
		assoc["types"].([]any)[0].(map[string]any)["associationTypeId"] != float64(3) {
		t.Fatalf("create deal: %+v", got.body)
	}
	runShipped(t, hs["hubspot.createDeal"], map[string]any{"properties": map[string]any{"dealname": "D"}}, Item{})
	if _, ok := got.body["associations"]; ok {
		t.Fatalf("empty association sent: %+v", got.body)
	}

	hs = shippedModule(t, "hubspot.yaml", `{"results":[{"id":"1","email":"o@example.com","firstName":"O","userId":3}]}`, &got)
	res = runShipped(t, hs["hubspot.listOwners"], map[string]any{}, Item{})
	if got.path != "/api/crm/v3/owners" || res["email"] != "o@example.com" {
		t.Fatalf("owners: %+v %v", got, res)
	}
}

func TestSalesforceModule(t *testing.T) {
	var got captured
	sf := shippedModule(t, "salesforce.yaml", `{"totalSize":1,"done":true,"records":[{"attributes":{"type":"Contact"},"Id":"003x","Name":"Ada"}]}`, &got)
	res := runShipped(t, sf["salesforce.query"], map[string]any{"soql": "SELECT Id, Name FROM Contact WHERE Email = 'a@b.c'"}, Item{})
	if got.path != "/api/services/data/v62.0/query" || !strings.Contains(got.query, "q=SELECT+Id%2C+Name+FROM+Contact") ||
		res["Id"] != "003x" || res["Name"] != "Ada" || res["attributes"] != nil {
		t.Fatalf("query: %+v %v", got, res)
	}
	runShipped(t, sf["salesforce.query"], map[string]any{"soql": "SELECT Id FROM Lead", "includeDeleted": true}, Item{})
	if got.path != "/api/services/data/v62.0/queryAll" {
		t.Fatalf("queryAll: %s", got.path)
	}

	sf = shippedModule(t, "salesforce.yaml", `{"id":"001x","success":true,"errors":[]}`, &got)
	res = runShipped(t, sf["salesforce.create"], map[string]any{"sobject": "Account", "fields": map[string]any{"Name": "{{ json.name }}"}}, Item{"name": "Acme"})
	if got.method != "POST" || got.path != "/api/services/data/v62.0/sobjects/Account" || got.body["Name"] != "Acme" || res["id"] != "001x" || res["success"] != true {
		t.Fatalf("create: %+v %v", got, res)
	}

	sf = shippedModule(t, "salesforce.yaml", "", &got)
	res = runShipped(t, sf["salesforce.update"], map[string]any{"sobject": "Account", "recordId": "001x", "fields": map[string]any{"Phone": "1"}}, Item{})
	if got.method != "PATCH" || got.path != "/api/services/data/v62.0/sobjects/Account/001x" || got.body["Phone"] != "1" || res["id"] != "001x" {
		t.Fatalf("update: %+v %v", got, res)
	}
	runShipped(t, sf["salesforce.delete"], map[string]any{"sobject": "Account", "recordId": "001x"}, Item{})
	if got.method != "DELETE" || got.path != "/api/services/data/v62.0/sobjects/Account/001x" {
		t.Fatalf("delete: %+v", got)
	}
	runShipped(t, sf["salesforce.upsert"], map[string]any{"sobject": "Contact", "externalIdField": "Ext__c", "externalId": "e-1", "fields": map[string]any{"LastName": "L"}}, Item{})
	if got.method != "PATCH" || got.path != "/api/services/data/v62.0/sobjects/Contact/Ext__c/e-1" {
		t.Fatalf("upsert: %+v", got)
	}

	// baseUrl and apiHosts must name the same host, so that editing both keeps the token safe.
	raw, err := os.ReadFile("../../modules/salesforce.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.BaseURL != "https://"+m.OAuth2.APIHosts[0] {
		t.Fatalf("baseUrl %q and apiHosts %v must name the same host", m.BaseURL, m.OAuth2.APIHosts)
	}
}

func TestGoogleCalendarModule(t *testing.T) {
	var got captured
	ev := `{"id":"e1","status":"confirmed","summary":"Sync","start":{"dateTime":"2026-03-01T10:00:00+01:00","timeZone":"Europe/Berlin"},"end":{"dateTime":"2026-03-01T11:00:00+01:00"},"attendees":[{"email":"a@example.com"}],"updated":"2026-02-01T00:00:00Z"}`
	gc := shippedModule(t, "google-calendar.yaml", ev, &got)
	res := runShipped(t, gc["google-calendar.createEvent"], map[string]any{
		"calendarId": "primary", "summary": "Sync", "start": "2026-03-01T10:00:00+01:00", "end": "2026-03-01T11:00:00+01:00",
		"timeZone": "Europe/Berlin", "attendees": "a@example.com, Bo <b@example.com>", "addMeet": true, "sendUpdates": "all",
	}, Item{})
	start := got.body["start"].(map[string]any)
	att := got.body["attendees"].([]any)
	conf := got.body["conferenceData"].(map[string]any)["createRequest"].(map[string]any)
	if got.method != "POST" || got.path != "/api/calendars/primary/events" || start["dateTime"] != "2026-03-01T10:00:00+01:00" || start["timeZone"] != "Europe/Berlin" ||
		len(att) != 2 || att[1].(map[string]any)["email"] != "b@example.com" || conf["requestId"] == "" || conf["conferenceSolutionKey"].(map[string]any)["type"] != "hangoutsMeet" ||
		!strings.Contains(got.query, "conferenceDataVersion=1") || !strings.Contains(got.query, "sendUpdates=all") ||
		got.body["description"] != nil || res["id"] != "e1" || res["start"] != "2026-03-01T10:00:00+01:00" || res["allDay"] != false {
		t.Fatalf("create: %+v %v", got, res)
	}
	runShipped(t, gc["google-calendar.createEvent"], map[string]any{
		"calendarId": "primary", "summary": "Holiday", "start": "2026-03-01", "end": "2026-03-02", "allDay": true,
	}, Item{})
	if got.body["start"].(map[string]any)["date"] != "2026-03-01" || got.body["end"].(map[string]any)["date"] != "2026-03-02" ||
		got.body["attendees"] != nil || strings.Contains(got.query, "conferenceDataVersion") {
		t.Fatalf("all-day: %+v %s", got.body, got.query)
	}

	runShipped(t, gc["google-calendar.updateEvent"], map[string]any{"calendarId": "primary", "eventId": "e1", "summary": "Renamed"}, Item{})
	if got.method != "PATCH" || got.path != "/api/calendars/primary/events/e1" || len(got.body) != 1 || got.body["summary"] != "Renamed" {
		t.Fatalf("update: %+v", got)
	}
	runShipped(t, gc["google-calendar.getEvent"], map[string]any{"calendarId": "primary", "eventId": "{{ json.id }}"}, Item{"id": "e1"})
	if got.path != "/api/calendars/primary/events/e1" {
		t.Fatalf("get: %+v", got)
	}

	list := `{"items":[` + ev + `]}`
	gc = shippedModule(t, "google-calendar.yaml", list, &got)
	res = runShipped(t, gc["google-calendar.listEvents"], map[string]any{"calendarId": "primary", "timeMax": "2026-04-01T00:00:00Z", "query": "sync"}, Item{})
	if got.path != "/api/calendars/primary/events" || !strings.Contains(got.query, "singleEvents=true") || !strings.Contains(got.query, "orderBy=startTime") ||
		!strings.Contains(got.query, "timeMax=2026-04-01") || !strings.Contains(got.query, "q=sync") || !strings.Contains(got.query, "timeMin=20") || res["summary"] != "Sync" {
		t.Fatalf("list: %+v %v", got, res)
	}
	gc = shippedModule(t, "google-calendar.yaml", `{"items":[{"id":"c1","summary":"Work","primary":true}]}`, &got)
	if res = runShipped(t, gc["google-calendar.listCalendars"], map[string]any{}, Item{}); got.path != "/api/users/me/calendarList" || res["primary"] != true {
		t.Fatalf("calendars: %+v %v", got, res)
	}
	gc = shippedModule(t, "google-calendar.yaml", "", &got)
	runShipped(t, gc["google-calendar.deleteEvent"], map[string]any{"calendarId": "primary", "eventId": "e1"}, Item{})
	if got.method != "DELETE" || got.path != "/api/calendars/primary/events/e1" {
		t.Fatalf("delete: %+v", got)
	}

	// The trigger emits an event again when it changes (key: id and updated), not on every check.
	gc = shippedModule(t, "google-calendar.yaml", list, &got)
	n := gc["google-calendar.newEvent"]
	def := n.Definition()
	state := NewMemoryState()
	check := func(trigger string) []Item {
		t.Helper()
		out, err := n.Execute(context.Background(), &ExecContext{
			Node: &workflow.Node{ID: "c", Name: def.Name, Params: map[string]any{"calendarId": "primary"}}, Def: &def, Items: []Item{},
			OAuth: &OAuthToken{AccessToken: "tok"}, State: state, TriggerType: trigger,
			Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out["main"]
	}
	if def.Trigger != TriggerPoll || len(check("poll")) != 0 || !strings.Contains(got.query, "updatedMin=20") || !strings.Contains(got.query, "orderBy=updated") {
		t.Fatalf("first check: %s", got.query)
	}
	if len(check("poll")) != 0 {
		t.Fatal("unchanged event emitted again")
	}
	gc = shippedModule(t, "google-calendar.yaml", strings.Replace(list, "2026-02-01T00:00:00Z", "2026-02-02T00:00:00Z", 1), &got)
	n = gc["google-calendar.newEvent"]
	if items := check("poll"); len(items) != 1 || items[0]["id"] != "e1" {
		t.Fatalf("changed event: %v", items)
	}
}

func TestGoogleDriveModule(t *testing.T) {
	var got captured
	file := `{"id":"f1","name":"Plan","mimeType":"application/vnd.google-apps.document","parents":["p1"],"webViewLink":"https://docs/f1","owners":[{"emailAddress":"o@example.com"}]}`
	gd := shippedModule(t, "google-drive.yaml", `{"files":[`+file+`]}`, &got)
	res := runShipped(t, gd["google-drive.list"], map[string]any{"nameContains": "O'Neil", "folderId": "root", "mimeType": "application/pdf", "query": "starred = true"}, Item{})
	q, _ := url.ParseQuery(got.query)
	wantQ := `name contains 'O\'Neil' and 'root' in parents and mimeType = 'application/pdf' and trashed = false and starred = true`
	if got.path != "/api/files" || q.Get("q") != wantQ || q.Get("supportsAllDrives") != "true" || q.Get("orderBy") != "modifiedTime desc" ||
		res["id"] != "f1" || res["isFolder"] != false || res["owner"] != "o@example.com" || res["parents"].([]any)[0] != "p1" {
		t.Fatalf("list: %+v %q %v", got, q.Get("q"), res)
	}
	runShipped(t, gd["google-drive.list"], map[string]any{"includeTrashed": true}, Item{})
	if q, _ = url.ParseQuery(got.query); q.Get("q") != "" {
		t.Fatalf("unfiltered list: %q", q.Get("q"))
	}

	gd = shippedModule(t, "google-drive.yaml", file, &got)
	runShipped(t, gd["google-drive.createFolder"], map[string]any{"name": "Reports", "parentId": "p1"}, Item{})
	if got.method != "POST" || got.path != "/api/files" || got.body["mimeType"] != "application/vnd.google-apps.folder" || got.body["parents"].([]any)[0] != "p1" {
		t.Fatalf("folder: %+v", got)
	}
	runShipped(t, gd["google-drive.copy"], map[string]any{"fileId": "f1", "name": "Copy"}, Item{})
	if got.path != "/api/files/f1/copy" || got.body["name"] != "Copy" || got.body["parents"] != nil {
		t.Fatalf("copy: %+v", got)
	}
	runShipped(t, gd["google-drive.update"], map[string]any{"fileId": "f1", "name": "New", "addParent": "p2", "removeParent": "p1"}, Item{})
	if got.method != "PATCH" || got.path != "/api/files/f1" || !strings.Contains(got.query, "addParents=p2") || !strings.Contains(got.query, "removeParents=p1") || got.body["name"] != "New" {
		t.Fatalf("update: %+v", got)
	}
	runShipped(t, gd["google-drive.trash"], map[string]any{"fileId": "f1"}, Item{})
	if got.method != "PATCH" || got.body["trashed"] != true {
		t.Fatalf("trash: %+v", got)
	}
	runShipped(t, gd["google-drive.get"], map[string]any{"fileId": "{{ json.id }}"}, Item{"id": "f1"})
	if got.path != "/api/files/f1" {
		t.Fatalf("get: %+v", got)
	}

	gd = shippedModule(t, "google-drive.yaml", `{"id":"perm1","type":"user","role":"writer"}`, &got)
	res = runShipped(t, gd["google-drive.share"], map[string]any{"fileId": "f1", "type": "user", "target": "a@example.com", "role": "writer", "sendNotification": false}, Item{})
	if got.path != "/api/files/f1/permissions" || got.body["emailAddress"] != "a@example.com" || got.body["role"] != "writer" || got.body["type"] != "user" ||
		!strings.Contains(got.query, "sendNotificationEmail=false") || res["permissionId"] != "perm1" {
		t.Fatalf("share: %+v %v", got, res)
	}
	runShipped(t, gd["google-drive.share"], map[string]any{"fileId": "f1", "type": "anyone", "role": "reader", "sendNotification": true}, Item{})
	if got.body["emailAddress"] != nil || got.body["type"] != "anyone" || strings.Contains(got.query, "sendNotificationEmail") {
		t.Fatalf("share anyone: %+v %s", got.body, got.query)
	}

	gd = shippedModule(t, "google-drive.yaml", "", &got)
	runShipped(t, gd["google-drive.delete"], map[string]any{"fileId": "f1"}, Item{})
	if got.method != "DELETE" || got.path != "/api/files/f1" {
		t.Fatalf("delete: %+v", got)
	}

	// Text exports come back as a plain string body.
	gd = shippedModule(t, "google-drive.yaml", "Hello\nWorld", &got)
	res = runShipped(t, gd["google-drive.exportText"], map[string]any{"fileId": "f1"}, Item{})
	if got.path != "/api/files/f1/export" || !strings.Contains(got.query, "mimeType=text%2Fplain") || res["text"] != "Hello\nWorld" {
		t.Fatalf("export: %+v %v", got, res)
	}
}

func TestSharePointModule(t *testing.T) {
	var got captured
	sp := shippedModule(t, "sharepoint.yaml", `{"value":[{"id":"s1","name":"team","displayName":"Team","webUrl":"https://c.sharepoint.com/sites/team"}]}`, &got)
	res := runShipped(t, sp["sharepoint.searchSites"], map[string]any{"query": "team"}, Item{})
	if got.path != "/api/sites" || got.query != "search=team" || res["displayName"] != "Team" {
		t.Fatalf("sites: %+v %v", got, res)
	}

	sp = shippedModule(t, "sharepoint.yaml", `{"value":[{"id":"l1","displayName":"Tasks","list":{"template":"genericList","hidden":false}},{"id":"l2","displayName":"Hidden","list":{"hidden":true}}]}`, &got)
	res = runShipped(t, sp["sharepoint.listLists"], map[string]any{"siteId": "c.sharepoint.com:/sites/team"}, Item{})
	if got.path != "/api/sites/c.sharepoint.com:/sites/team/lists" || res["id"] != "l1" || res["template"] != "genericList" {
		t.Fatalf("lists: %+v %v", got, res)
	}
	def := sp["sharepoint.listLists"].Definition()
	out, err := sp["sharepoint.listLists"].Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{Name: def.Name, Params: map[string]any{"siteId": "s", "includeHidden": true}}, Def: &def, Items: []Item{{}},
		OAuth: &OAuthToken{AccessToken: "tok"}, Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || len(out["main"]) != 2 {
		t.Fatalf("hidden lists: %v %v", out, err)
	}

	sp = shippedModule(t, "sharepoint.yaml", `{"value":[{"id":"1","webUrl":"w","createdBy":{"user":{"email":"a@example.com"}},"fields":{"Title":"First","Status":"Open"}}]}`, &got)
	res = runShipped(t, sp["sharepoint.listItems"], map[string]any{"siteId": "s", "listId": "l1", "filter": "fields/Status eq 'Open'", "top": 10}, Item{})
	q, _ := url.ParseQuery(got.query)
	if got.path != "/api/sites/s/lists/l1/items" || q.Get("$expand") != "fields" || q.Get("$filter") != "fields/Status eq 'Open'" || q.Get("$top") != "10" ||
		got.prefer != "HonorNonIndexedQueriesWarningMayFailRandomly" || res["createdBy"] != "a@example.com" || res["fields"].(map[string]any)["Title"] != "First" {
		t.Fatalf("items: %+v %v", got, res)
	}
	runShipped(t, sp["sharepoint.listItems"], map[string]any{"siteId": "s", "listId": "l1"}, Item{})
	if got.prefer != "" || strings.Contains(got.query, "filter") {
		t.Fatalf("unfiltered items: %+v", got)
	}

	sp = shippedModule(t, "sharepoint.yaml", `{"id":"9","fields":{"Title":"New"}}`, &got)
	res = runShipped(t, sp["sharepoint.createItem"], map[string]any{"siteId": "s", "listId": "l1", "fields": map[string]any{"Title": "{{ json.t }}"}}, Item{"t": "New"})
	if got.method != "POST" || got.path != "/api/sites/s/lists/l1/items" || got.body["fields"].(map[string]any)["Title"] != "New" || res["id"] != "9" {
		t.Fatalf("create: %+v %v", got, res)
	}
	res = runShipped(t, sp["sharepoint.updateItem"], map[string]any{"siteId": "s", "listId": "l1", "itemId": "9", "fields": map[string]any{"Status": "Done"}}, Item{})
	if got.method != "PATCH" || got.path != "/api/sites/s/lists/l1/items/9/fields" || got.body["Status"] != "Done" || res["id"] != "9" {
		t.Fatalf("update: %+v %v", got, res)
	}
	runShipped(t, sp["sharepoint.getItem"], map[string]any{"siteId": "s", "listId": "l1", "itemId": "{{ json.id }}"}, Item{"id": "9"})
	if got.path != "/api/sites/s/lists/l1/items/9" || got.query != "%24expand=fields" {
		t.Fatalf("get: %+v", got)
	}
	sp = shippedModule(t, "sharepoint.yaml", "", &got)
	runShipped(t, sp["sharepoint.deleteItem"], map[string]any{"siteId": "s", "listId": "l1", "itemId": "9"}, Item{})
	if got.method != "DELETE" || got.path != "/api/sites/s/lists/l1/items/9" {
		t.Fatalf("delete: %+v", got)
	}

	sp = shippedModule(t, "sharepoint.yaml", `{"value":[{"id":"d1","name":"Docs","size":12,"file":{"mimeType":"text/plain"},"parentReference":{"driveId":"dr1"}},{"id":"d2","name":"Sub","folder":{"childCount":1}}]}`, &got)
	res = runShipped(t, sp["sharepoint.listDriveItems"], map[string]any{"siteId": "s"}, Item{})
	if got.path != "/api/sites/s/drive/root/children" || res["mimeType"] != "text/plain" || res["isFolder"] != false || res["driveId"] != "dr1" {
		t.Fatalf("drive items: %+v %v", got, res)
	}
	runShipped(t, sp["sharepoint.listDriveItems"], map[string]any{"driveId": "dr1", "folderId": "d2"}, Item{})
	if got.path != "/api/drives/dr1/items/d2/children" {
		t.Fatalf("drive folder: %+v", got)
	}
	runShipped(t, sp["sharepoint.listDrives"], map[string]any{"siteId": "s"}, Item{})
	if got.path != "/api/sites/s/drives" {
		t.Fatalf("drives: %+v", got)
	}
}
