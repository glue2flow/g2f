package push

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// reconciler returns a service that is not running but holds the
// reconciler lease, so tests call reconcile step by step.
func (f *fixture) reconciler(t *testing.T, mod func(*Options)) *Service {
	t.Helper()
	o := Options{
		Store: f.st, Registry: f.reg, Box: f.box, Waker: f.waker, Contexts: fakeContexts{f.reg, f.settings},
		Tokens: fakeTokens{}, Settings: f.settings, Log: discard, Instance: "11111111-1111-1111-1111-111111111111",
		PublicURL: "https://g2f.example.com", Enabled: true, Resync: time.Hour, ReconcileEvery: time.Hour,
	}
	if mod != nil {
		mod(&o)
	}
	s := New(o)
	if ok, err := f.st.AcquireLease(context.Background(), reconcilerLease, o.Instance, time.Hour); err != nil || !ok {
		t.Fatal("reconciler lease", ok, err)
	}
	return s
}

func (f *fixture) connection(t *testing.T, module, account string) string {
	t.Helper()
	c := &store.Connection{OrgID: "acme", Module: module, Kind: store.KindOAuth2, Name: account, Account: account,
		OwnerSub: "alice", Scope: "private"}
	if err := f.st.CreateConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func reconcile(t *testing.T, s *Service) {
	t.Helper()
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// fakeProvider records API calls and answers like Gmail or Graph.
type fakeProvider struct {
	mu     sync.Mutex
	calls  []string // "METHOD /path"
	bodies []map[string]any
	auth   []string
	fail   int // answer this many calls with HTTP 500
	gone   map[string]bool
	n      int
}

func (p *fakeProvider) count(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (p *fakeProvider) last() (string, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1], p.bodies[len(p.bodies)-1]
}

func (p *fakeProvider) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		p.calls = append(p.calls, r.Method+" "+r.URL.Path)
		p.bodies = append(p.bodies, body)
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		if p.fail > 0 {
			p.fail--
			http.Error(w, `{"error":"backend down"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/gmail/v1/users/me/watch":
			exp := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
			fmt.Fprintf(w, `{"historyId":"1234","expiration":"%d"}`, exp)
		case r.URL.Path == "/gmail/v1/users/me/stop":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.0/subscriptions":
			p.n++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("sub-%d", p.n), "expirationDateTime": body["expirationDateTime"]})
		case strings.HasPrefix(r.URL.Path, "/v1.0/subscriptions/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1.0/subscriptions/")
			if p.gone[id] {
				http.Error(w, `{"error":{"code":"ResourceNotFound"}}`, http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "expirationDateTime": body["expirationDateTime"]})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gmailNote(address string) string {
	data := base64.StdEncoding.EncodeToString([]byte(`{"emailAddress":"` + address + `","historyId":99}`))
	return `{"message":{"data":"` + data + `","messageId":"1"},"subscription":"projects/p/subscriptions/s"}`
}

func post(h http.Handler, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func setActive(t *testing.T, f *fixture, s *Service, wf *workflow.Workflow, active bool) {
	t.Helper()
	wf.Active = active
	if err := f.st.UpdateWorkflow(context.Background(), wf, nil); err != nil {
		t.Fatal(err)
	}
	s.WorkflowChanged(wf)
}

func TestGmailPush(t *testing.T) {
	f := newFixture(t)
	api := &fakeProvider{}
	srv := api.server(t)
	f.loadModule(t, "gmail.yaml", "https://gmail.googleapis.com", srv.URL)
	f.settings.set("acme", "gmail", map[string]any{"pubsubTopic": "projects/p/topics/gmail"})
	c1, c2 := f.connection(t, "gmail", "User@Example.com"), f.connection(t, "gmail", "other@example.com")
	wf1 := f.workflow(t, "gmail.newEmail", map[string]any{"connection": c1})
	wf2 := f.workflow(t, "gmail.newEmail", map[string]any{"connection": c1, "query": "from:boss"})
	wf3 := f.workflow(t, "gmail.newEmail", map[string]any{"connection": c2})
	ctx := context.Background()
	s := f.reconciler(t, nil)

	// One watch per account, shared by its triggers.
	reconcile(t, s)
	if n := api.count("POST /gmail/v1/users/me/watch"); n != 2 {
		t.Fatalf("%d watch calls, want 2", n)
	}
	call, body := api.last()
	if !strings.HasSuffix(call, "/watch") || body["topicName"] != "projects/p/topics/gmail" {
		t.Fatalf("watch: %s %v", call, body)
	}
	if a := api.auth[0]; !strings.HasPrefix(a, "Bearer tok-") {
		t.Fatalf("watch without the account's token: %q", a)
	}
	sub, err := f.st.GetPushSubscription(ctx, "gmail:"+c1)
	if err != nil || len(sub.Triggers) != 2 || sub.ExpiresAt == nil || sub.ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("subscription: %+v %v", sub, err)
	}
	d := s.Delivery(ctx, wf1, &wf1.Graph.Nodes[0], true)
	if !d.Push || d.Via != "Gmail" || d.Renews == nil || !strings.HasPrefix(d.Endpoint, "https://g2f.example.com/push/gmail/") {
		t.Fatalf("delivery: %+v", d)
	}
	if again := s.Delivery(ctx, wf1, &wf1.Graph.Nodes[0], false); again.Endpoint != "" {
		t.Fatal("endpoint URL shown to a non-admin")
	}
	// Converged: nothing to do on the next pass.
	reconcile(t, s)
	if n := len(api.calls); n != 2 {
		t.Fatalf("second pass called the provider (%d calls)", n)
	}

	h := s.Handler()
	path := strings.TrimPrefix(d.Endpoint, "https://g2f.example.com")
	if rec := post(h, "/push/gmail/not-the-token", gmailNote("user@example.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("bad token: %d", rec.Code)
	}
	if f.waker.total() != 0 {
		t.Fatal("woken by a bad token")
	}
	if rec := post(h, path, gmailNote("user@example.com")); rec.Code != http.StatusNoContent {
		t.Fatalf("valid push: %d %s", rec.Code, rec.Body)
	}
	if f.waker.count(wf1.ID, "t") != 1 || f.waker.count(wf2.ID, "t") != 1 || f.waker.count(wf3.ID, "t") != 0 {
		t.Fatalf("wakes: %v", f.waker.wakes)
	}
	if rec := post(h, path, gmailNote("stranger@example.com")); rec.Code != http.StatusNoContent || f.waker.total() != 2 {
		t.Fatalf("unknown address: %d, wakes %d", rec.Code, f.waker.total())
	}
	for _, bad := range []string{`not json`, `{"message":{}}`, `{"message":{"data":"%%%"}}`,
		`{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(`{"historyId":1}`)) + `"}}`} {
		if rec := post(h, path, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed %q: %d", bad, rec.Code)
		}
	}
	if f.waker.total() != 2 {
		t.Fatal("woken by malformed messages")
	}

	// Renewed daily.
	sub, _ = f.st.GetPushSubscription(ctx, "gmail:"+c1)
	old := time.Now().Add(-25 * time.Hour)
	sub.RenewedAt = &old
	if err := f.st.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	if n := api.count("POST /gmail/v1/users/me/watch"); n != 3 {
		t.Fatalf("watch not renewed: %d calls", n)
	}

	// The last trigger of an account goes away: users.stop.
	setActive(t, f, s, wf3, false)
	reconcile(t, s)
	if n := api.count("POST /gmail/v1/users/me/stop"); n != 1 {
		t.Fatalf("%d stop calls", n)
	}
	if _, err := f.st.GetPushSubscription(ctx, "gmail:"+c2); err == nil {
		t.Fatal("subscription kept after stop")
	}
	// One of two triggers of an account goes away: the watch stays.
	setActive(t, f, s, wf2, false)
	reconcile(t, s)
	if n := api.count("POST /gmail/v1/users/me/stop"); n != 1 {
		t.Fatal("watch stopped while a trigger still uses it")
	}
	sub, _ = f.st.GetPushSubscription(ctx, "gmail:"+c1)
	if len(sub.Triggers) != 1 || sub.Triggers[0].WorkflowID != wf1.ID {
		t.Fatalf("triggers: %v", sub.Triggers)
	}

	// Another replica does not reconcile while this one holds the lease.
	other := New(Options{Store: f.st, Registry: f.reg, Box: f.box, Waker: f.waker, Tokens: fakeTokens{}, Settings: f.settings,
		Log: discard, Instance: "22222222-2222-2222-2222-222222222222", PublicURL: "https://g2f.example.com", Enabled: true})
	sub.RenewedAt = &old
	_ = f.st.SavePushSubscription(ctx, sub)
	if err := other.reconcile(ctx); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("second reconciler ran: %v", err)
	}
	if n := api.count("POST /gmail/v1/users/me/watch"); n != 3 {
		t.Fatal("second reconciler called the provider")
	}
}

// With the push service account set, Pub/Sub's OIDC token is required.
func TestGmailPushOIDC(t *testing.T) {
	f := newFixture(t)
	api := &fakeProvider{}
	srv := api.server(t)
	f.loadModule(t, "gmail.yaml", "https://gmail.googleapis.com", srv.URL)
	const account = "pubsub-push@p.iam.gserviceaccount.com"
	f.settings.set("acme", "gmail", map[string]any{"pubsubTopic": "projects/p/topics/gmail", "pushServiceAccount": account})
	c1 := f.connection(t, "gmail", "user@example.com")
	wf := f.workflow(t, "gmail.newEmail", map[string]any{"connection": c1})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := f.reconciler(t, func(o *Options) {
		o.GoogleKeys = &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	})
	reconcile(t, s)
	endpoint, err := s.GmailEndpoint(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	token := func(aud, email string) string {
		raw, err := jwt.Signed(signer).Claims(map[string]any{
			"iss": "https://accounts.google.com", "aud": aud, "email": email, "email_verified": true,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "sub": "123",
		}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + raw
	}
	path := strings.TrimPrefix(endpoint, "https://g2f.example.com")
	h := s.Handler()
	for name, auth := range map[string]string{
		"none": "", "garbage": "Bearer x.y.z", "other account": token(endpoint, "evil@p.iam.gserviceaccount.com"),
		"other audience": token("https://elsewhere.example.com/push", account),
	} {
		if rec := post(h, path, gmailNote("user@example.com"), "Authorization", auth); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
	if f.waker.total() != 0 {
		t.Fatal("woken without a valid token")
	}
	if rec := post(h, path, gmailNote("user@example.com"), "Authorization", token(endpoint, account)); rec.Code != http.StatusNoContent {
		t.Fatalf("valid token: %d %s", rec.Code, rec.Body)
	}
	if f.waker.count(wf.ID, "t") != 1 {
		t.Fatal("not woken")
	}
}

// Without a public https URL providers cannot notify: nothing is
// subscribed and the trigger says why.
func TestPushNeedsPublicHTTPS(t *testing.T) {
	f := newFixture(t)
	api := &fakeProvider{}
	srv := api.server(t)
	f.loadModule(t, "outlook.yaml", "https://graph.microsoft.com", srv.URL)
	c := f.connection(t, "outlook", "user@example.com")
	wf := f.workflow(t, "outlook.newEmail", map[string]any{"connection": c})
	for _, u := range []string{"http://localhost:8080", "https://localhost:8443", "http://g2f.example.com"} {
		s := f.reconciler(t, func(o *Options) { o.PublicURL = u })
		reconcile(t, s)
		if len(api.calls) != 0 {
			t.Fatalf("%s: subscribed anyway", u)
		}
		d := s.Delivery(context.Background(), wf, &wf.Graph.Nodes[0], false)
		if d.Push || !strings.Contains(d.Reason, "not a public https URL") {
			t.Fatalf("%s: delivery %+v", u, d)
		}
	}
	// Gmail without the topic: not set up.
	f.loadModule(t, "gmail.yaml", "https://gmail.googleapis.com", srv.URL)
	gc := f.connection(t, "gmail", "user@example.com")
	gwf := f.workflow(t, "gmail.newEmail", map[string]any{"connection": gc})
	s := f.reconciler(t, nil)
	if d := s.Delivery(context.Background(), gwf, &gwf.Graph.Nodes[0], false); d.Push || !strings.Contains(d.Reason, "Pub/Sub topic") {
		t.Fatalf("delivery without topic: %+v", d)
	}
}

func graphNote(id, state, lifecycle string) string {
	n := map[string]any{"subscriptionId": id, "clientState": state, "changeType": "created", "resource": "x"}
	if lifecycle != "" {
		n["lifecycleEvent"] = lifecycle
	}
	b, _ := json.Marshal(map[string]any{"value": []any{n}})
	return string(b)
}

func TestGraphPush(t *testing.T) {
	f := newFixture(t)
	api := &fakeProvider{gone: map[string]bool{}}
	srv := api.server(t)
	f.loadModule(t, "outlook.yaml", "https://graph.microsoft.com", srv.URL)
	c := f.connection(t, "outlook", "user@example.com")
	wf := f.workflow(t, "outlook.newEmail", map[string]any{"connection": c, "folder": "inbox"})
	ctx := context.Background()
	s := f.reconciler(t, nil)
	h := s.Handler()
	key := "outlook:" + wf.ID + "/t"

	// Validation handshake: the token comes back as plain text.
	req := httptest.NewRequest(http.MethodPost, "/push/graph?validationToken=Validation%3A+Testing+client+application", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "Validation: Testing client application" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("handshake: %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}

	reconcile(t, s)
	call, body := api.last()
	exp, _ := time.Parse(time.RFC3339, fmt.Sprint(body["expirationDateTime"]))
	if call != "POST /v1.0/subscriptions" || body["changeType"] != "created" || body["resource"] != "me/mailFolders('inbox')/messages" ||
		body["notificationUrl"] != "https://g2f.example.com/push/graph" || body["lifecycleNotificationUrl"] != "https://g2f.example.com/push/graph" ||
		body["clientState"] == "" || exp.Sub(time.Now()) > 4230*time.Minute || exp.Sub(time.Now()) < 4100*time.Minute {
		t.Fatalf("subscribe: %s %v", call, body)
	}
	state := body["clientState"].(string)
	sub, err := f.st.GetPushSubscription(ctx, key)
	if err != nil || sub.ExternalID != "sub-1" || sub.Secret == state {
		t.Fatalf("stored: %+v %v (the clientState must be sealed)", sub, err)
	}
	if d := s.Delivery(ctx, wf, &wf.Graph.Nodes[0], false); !d.Push || d.Via != "Microsoft Graph" {
		t.Fatalf("delivery: %+v", d)
	}

	// Notifications: right clientState wakes, anything else is dropped.
	if rec := post(h, "/push/graph", graphNote("sub-1", "wrong", "")); rec.Code != http.StatusAccepted || f.waker.total() != 0 {
		t.Fatalf("wrong clientState: %d wakes %d", rec.Code, f.waker.total())
	}
	if rec := post(h, "/push/graph", graphNote("sub-9", state, "")); rec.Code != http.StatusAccepted || f.waker.total() != 0 {
		t.Fatal("unknown subscription woke something")
	}
	if rec := post(h, "/push/graph", `{"value":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", rec.Code)
	}
	if rec := post(h, "/push/graph", graphNote("sub-1", state, "")); rec.Code != http.StatusAccepted || f.waker.count(wf.ID, "t") != 1 {
		t.Fatalf("notification: %d wakes %d", rec.Code, f.waker.count(wf.ID, "t"))
	}

	// Renewed before it expires.
	soon := time.Now().Add(30 * time.Minute)
	sub, _ = f.st.GetPushSubscription(ctx, key)
	sub.ExpiresAt = &soon
	_ = f.st.SavePushSubscription(ctx, sub)
	reconcile(t, s)
	if call, body := api.last(); call != "PATCH /v1.0/subscriptions/sub-1" || body["expirationDateTime"] == nil {
		t.Fatalf("renewal: %s %v", call, body)
	}
	sub, _ = f.st.GetPushSubscription(ctx, key)
	if sub.ExpiresAt.Before(time.Now().Add(60 * time.Hour)) {
		t.Fatalf("expiry not extended: %v", sub.ExpiresAt)
	}

	// Removed by Microsoft: check now and subscribe again.
	if rec := post(h, "/push/graph", graphNote("sub-1", state, "subscriptionRemoved")); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	if f.waker.count(wf.ID, "t") != 2 {
		t.Fatal("removed subscription: no check")
	}
	reconcile(t, s)
	sub, _ = f.st.GetPushSubscription(ctx, key)
	if call, _ := api.last(); call != "POST /v1.0/subscriptions" || sub.ExternalID != "sub-2" {
		t.Fatalf("not recreated: %s %+v", call, sub)
	}
	// reauthorizationRequired: renewed at the next pass.
	_, body = api.last()
	state = body["clientState"].(string)
	post(h, "/push/graph", graphNote("sub-2", state, "reauthorizationRequired"))
	reconcile(t, s)
	if call, _ := api.last(); call != "PATCH /v1.0/subscriptions/sub-2" {
		t.Fatalf("not reauthorized: %s", call)
	}
	// A renewal of a subscription Graph no longer knows: subscribe again.
	api.mu.Lock()
	api.gone["sub-2"] = true
	api.mu.Unlock()
	sub, _ = f.st.GetPushSubscription(ctx, key)
	sub.ExpiresAt = &soon
	_ = f.st.SavePushSubscription(ctx, sub)
	reconcile(t, s)
	if sub, _ = f.st.GetPushSubscription(ctx, key); sub.ExternalID != "sub-3" {
		t.Fatalf("404 on renewal: %+v", sub)
	}

	// Another folder replaces the subscription.
	wf.Graph.Nodes[0].Params["folder"] = "archive"
	setActive(t, f, s, wf, true)
	reconcile(t, s)
	if api.count("DELETE /v1.0/subscriptions/sub-3") != 1 {
		t.Fatal("old subscription not deleted")
	}
	if _, body := api.last(); body["resource"] != "me/mailFolders('archive')/messages" {
		t.Fatalf("new resource: %v", body)
	}

	// Deactivated: deleted at Graph and forgotten.
	setActive(t, f, s, wf, false)
	reconcile(t, s)
	if call, _ := api.last(); call != "DELETE /v1.0/subscriptions/sub-4" {
		t.Fatalf("not deleted: %s", call)
	}
	if _, err := f.st.GetPushSubscription(ctx, key); err == nil {
		t.Fatal("row kept")
	}
}

// Provider errors back off per subscription instead of hammering on every pass.
func TestReconcileBackoff(t *testing.T) {
	f := newFixture(t)
	api := &fakeProvider{fail: 1}
	srv := api.server(t)
	f.loadModule(t, "outlook.yaml", "https://graph.microsoft.com", srv.URL)
	c := f.connection(t, "outlook", "user@example.com")
	wf := f.workflow(t, "outlook.newEmail", map[string]any{"connection": c})
	ctx := context.Background()
	s := f.reconciler(t, nil)
	reconcile(t, s)
	key := "outlook:" + wf.ID + "/t"
	sub, err := f.st.GetPushSubscription(ctx, key)
	if err != nil || sub.Failures != 1 || sub.LastError == "" || sub.RetryAt == nil ||
		sub.RetryAt.Sub(time.Now()) < 50*time.Second || sub.RetryAt.Sub(time.Now()) > 70*time.Second {
		t.Fatalf("after failure: %+v %v", sub, err)
	}
	if d := s.Delivery(ctx, wf, &wf.Graph.Nodes[0], false); d.Push || !strings.Contains(d.Reason, "subscription failed") {
		t.Fatalf("delivery: %+v", d)
	}
	reconcile(t, s)
	reconcile(t, s)
	if n := len(api.calls); n != 1 {
		t.Fatalf("%d calls during backoff", n)
	}
	if h, _ := f.st.HealthyPushTriggers(ctx); len(h) != 0 {
		t.Fatal("failed subscription counted as healthy")
	}
	// Due again: succeeds and resets the failure count.
	past := time.Now().Add(-time.Second)
	sub.RetryAt = &past
	_ = f.st.SavePushSubscription(ctx, sub)
	reconcile(t, s)
	sub, _ = f.st.GetPushSubscription(ctx, key)
	if sub.Failures != 0 || sub.LastError != "" || sub.ExternalID == "" {
		t.Fatalf("after recovery: %+v", sub)
	}
	if h, _ := f.st.HealthyPushTriggers(ctx); len(h) != 1 || h[0] != (store.TriggerRef{WorkflowID: wf.ID, NodeID: "t"}) {
		t.Fatalf("healthy: %v", h)
	}
	if backoff(1) != time.Minute || backoff(3) != 4*time.Minute || backoff(20) != time.Hour {
		t.Fatal("backoff schedule")
	}
}

// The manifests' push blocks parse and point at the right endpoints.
func TestShippedPushSpecs(t *testing.T) {
	f := newFixture(t)
	f.loadModule(t, "gmail.yaml")
	f.loadModule(t, "outlook.yaml")
	f.loadModuleOnce(t)
	for typ, want := range map[string]string{"gmail.newEmail": nodes.PushGmail, "outlook.newEmail": nodes.PushGraph, "email.received": nodes.PushListen} {
		impl, ok := f.reg.Get(typ)
		if !ok {
			t.Fatalf("%s missing", typ)
		}
		def := impl.Definition()
		if def.Push != want || def.Param(nodes.DeliveryParam) == nil {
			t.Fatalf("%s: push %q", typ, def.Push)
		}
	}
}
