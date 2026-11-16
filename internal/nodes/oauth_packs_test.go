package nodes

import (
	"os"
	"strings"
	"testing"
)

// TestBundledOAuthModules loads every bundled OAuth2 module and checks what an admin
// and the editor rely on: valid manifest, app credentials in config, a token host,
// a schema for every node and the account picker.
func TestBundledOAuthModules(t *testing.T) {
	want := map[string]struct {
		nodes   []string
		host    string
		trigger string
	}{
		"hubspot":         {[]string{"getContact", "searchContacts", "createContact", "updateContact", "getDeal", "searchDeals", "createDeal", "updateDeal", "createCompany", "listOwners"}, "api.hubapi.com", ""},
		"salesforce":      {[]string{"query", "get", "create", "update", "upsert", "delete"}, "*.my.salesforce.com", ""},
		"google-calendar": {[]string{"listCalendars", "listEvents", "getEvent", "createEvent", "updateEvent", "deleteEvent", "newEvent"}, "www.googleapis.com", "newEvent"},
		"google-drive":    {[]string{"list", "get", "createFolder", "copy", "update", "trash", "delete", "share", "exportText"}, "www.googleapis.com", ""},
		"sharepoint":      {[]string{"searchSites", "getSite", "listLists", "listItems", "getItem", "createItem", "updateItem", "deleteItem", "listDrives", "listDriveItems"}, "graph.microsoft.com", ""},
	}
	for module, w := range want {
		t.Run(module, func(t *testing.T) {
			raw, err := os.ReadFile("../../modules/" + module + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			m, err := ParseManifest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if m.Module != module || m.Runtime != "http" || m.Name == "" || m.Description == "" || m.Color == "" || m.OAuth2 == nil {
				t.Fatalf("manifest: %+v", m)
			}
			if len(m.OAuth2.APIHosts) != 1 || m.OAuth2.APIHosts[0] != w.host || m.OAuth2.ClientIDConfig != "clientId" {
				t.Fatalf("oauth2: %+v", m.OAuth2)
			}
			if module == "salesforce" {
				if m.BaseURL != "{{ oauth.instanceUrl }}" || m.OAuth2.InstanceURLField != "instance_url" {
					t.Fatalf("baseUrl %q, instanceUrlField %q", m.BaseURL, m.OAuth2.InstanceURLField)
				}
			} else if !strings.HasPrefix(m.BaseURL, "https://") || !strings.Contains(m.BaseURL, w.host) && module != "google-calendar" && module != "google-drive" && module != "sharepoint" {
				t.Fatalf("baseUrl %q", m.BaseURL)
			}
			list, err := HTTPModuleNodes(m, "test")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]Definition{}
			for _, n := range list {
				got[n.Definition().Type] = n.Definition()
			}
			if len(got) != len(w.nodes) {
				t.Fatalf("nodes: %d, want %d", len(got), len(w.nodes))
			}
			for _, name := range w.nodes {
				def, ok := got[module+"."+name]
				if !ok {
					t.Fatalf("missing node %s", name)
				}
				if def.OutputSchema == nil || def.Params[0].Name != ConnectionParam || def.Description == "" {
					t.Errorf("%s: schema/account/description missing", name)
				}
				if (def.Trigger == TriggerPoll) != (name == w.trigger) {
					t.Errorf("%s: trigger %q", name, def.Trigger)
				}
			}
		})
	}
}
