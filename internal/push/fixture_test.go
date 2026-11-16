package push

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/store/storetest"
	"github.com/glue2flow/g2f/internal/workflow"

	_ "github.com/glue2flow/g2f/modules/email" // IMAP IDLE listener
)

// Tests call httptest and IMAP servers on 127.0.0.1 over plain connections.
func TestMain(m *testing.M) {
	netguard.SetPolicy(netguard.Policy{AllowPrivate: true})
	nodes.AllowInsecureAPIHosts()
	os.Exit(m.Run())
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// --- fakes ------------------------------------------------------------------

type fakeWaker struct {
	mu    sync.Mutex
	wakes map[store.TriggerRef]int
	ch    chan store.TriggerRef
}

func newWaker() *fakeWaker {
	return &fakeWaker{wakes: map[store.TriggerRef]int{}, ch: make(chan store.TriggerRef, 1000)}
}

func (w *fakeWaker) Wake(_ context.Context, wf, node string) error {
	ref := store.TriggerRef{WorkflowID: wf, NodeID: node}
	w.mu.Lock()
	w.wakes[ref]++
	w.mu.Unlock()
	w.ch <- ref
	return nil
}

func (w *fakeWaker) count(wf, node string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wakes[store.TriggerRef{WorkflowID: wf, NodeID: node}]
}

func (w *fakeWaker) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.wakes {
		n += c
	}
	return n
}

type fakeSettings struct {
	mu  sync.Mutex
	cfg map[string]map[string]any // org/module -> config
}

func (f *fakeSettings) set(org, module string, cfg map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg[org+"/"+module] = cfg
}

func (f *fakeSettings) ModuleSettings(_ context.Context, org, module string) (map[string]any, map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.cfg[org+"/"+module] {
		out[k] = v
	}
	return out, map[string]any{}, nil
}

type fakeTokens struct{}

func (fakeTokens) Token(_ context.Context, org, module, id string) (*nodes.OAuthToken, error) {
	return &nodes.OAuthToken{AccessToken: "tok-" + id, TokenType: "Bearer"}, nil
}

// fakeContexts builds a trigger's context from the registry and settings,
// like the engine does without secrets and credentials.
type fakeContexts struct {
	reg      *nodes.Registry
	settings *fakeSettings
}

func (f fakeContexts) TriggerContext(ctx context.Context, wf *workflow.Workflow, nodeID string) (*nodes.ExecContext, error) {
	n := wf.Graph.Node(nodeID)
	impl, _ := f.reg.Get(n.Type)
	def := impl.Definition()
	cfg, _, _ := f.settings.ModuleSettings(ctx, wf.OrgID, def.Module)
	return &nodes.ExecContext{Node: n, Def: &def, Config: cfg, Env: expression.Env{}, Logger: discard}, nil
}

type fixture struct {
	st       *store.Store
	reg      *nodes.Registry
	box      *secrets.Box
	waker    *fakeWaker
	settings *fakeSettings
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{st: storetest.DB(t), reg: nodes.NewRegistry(), box: box, waker: newWaker(),
		settings: &fakeSettings{cfg: map[string]map[string]any{}}}
}

// loadModule registers a shipped module, with its API hosts replaced.
func (f *fixture) loadModule(t *testing.T, file string, replace ...string) {
	t.Helper()
	raw, err := os.ReadFile("../../modules/" + file)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer(replace...).Replace(string(raw))
	m, err := nodes.ParseManifest([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	var list []nodes.Node
	if m.Runtime == "go" {
		list, err = nodes.NativeModuleNodes(m, "test")
	} else {
		m.OAuth2.APIHosts = []string{"127.0.0.1"}
		list, err = nodes.HTTPModuleNodes(m, "test")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RegisterModule(m.Info("test"), list); err != nil {
		t.Fatal(err)
	}
}

// service starts a push service (one "replica") that stops with the test
// or when the returned function is called.
func (f *fixture) service(t *testing.T, mod func(*Options)) (*Service, func()) {
	t.Helper()
	o := Options{
		Store: f.st, Registry: f.reg, Box: f.box, Waker: f.waker, Contexts: fakeContexts{f.reg, f.settings},
		Tokens: fakeTokens{}, Settings: f.settings, Log: discard, Instance: uuid.NewString(),
		PublicURL: "https://g2f.example.com", Enabled: true, IdleMax: 10,
		ListenTick: 50 * time.Millisecond, Resync: time.Hour, ReconcileEvery: time.Hour,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, UnsupportedRetry: time.Hour,
	}
	if mod != nil {
		mod(&o)
	}
	s := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return s, stop
}

// workflow stores an active workflow with one trigger node.
func (f *fixture) workflow(t *testing.T, nodeType string, params map[string]any) *workflow.Workflow {
	t.Helper()
	wf := &workflow.Workflow{ID: uuid.NewString(), OrgID: "acme", Name: "Mail", OwnerSub: "alice", Active: true,
		Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
			{ID: "t", Name: "Trigger", Type: nodeType, Params: params},
		}}}
	if err := f.st.CreateWorkflow(context.Background(), wf, nil); err != nil {
		t.Fatal(err)
	}
	return wf
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- a fake IMAP server that pushes ------------------------------------------

// pushBackend is go-imap's in-memory backend whose mailboxes report new
// messages to selected (and idling) connections, like a real server.
type pushBackend struct {
	*memory.Backend
	updates chan backend.Update
}

func (b *pushBackend) Updates() <-chan backend.Update { return b.updates }

func (b *pushBackend) Login(ci *imap.ConnInfo, user, pass string) (backend.User, error) {
	u, err := b.Backend.Login(ci, user, pass)
	if err != nil {
		return nil, err
	}
	return &pushUser{User: u, b: b}, nil
}

type pushUser struct {
	backend.User
	b *pushBackend
}

func (u *pushUser) GetMailbox(name string) (backend.Mailbox, error) {
	m, err := u.User.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return &pushMailbox{Mailbox: m, u: u}, nil
}

type pushMailbox struct {
	backend.Mailbox
	u *pushUser
}

func (m *pushMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if err := m.Mailbox.CreateMessage(flags, date, body); err != nil {
		return err
	}
	st, err := m.Mailbox.Status([]imap.StatusItem{imap.StatusMessages})
	if err != nil {
		return err
	}
	upd := &backend.MailboxUpdate{Update: backend.NewUpdate(m.u.Username(), m.Name()), MailboxStatus: st}
	go func() { m.u.b.updates <- upd }()
	return nil
}

// imapProxy sits in front of the server to misbehave: hide the IDLE
// capability, drop connections, refuse them, or stall without a greeting.
type imapProxy struct {
	ln        net.Listener
	target    string
	stripIdle atomic.Bool
	refuse    atomic.Bool
	stall     atomic.Bool
	accepted  atomic.Int32

	mu      sync.Mutex
	conns   []net.Conn
	accepts []time.Time
}

func startIMAP(t *testing.T) (*imapProxy, func(subject string)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(&pushBackend{Backend: memory.New(), updates: make(chan backend.Update, 100)})
	srv.AllowInsecureAuth = true
	srv.ErrorLog = log0{}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &imapProxy{ln: pl, target: ln.Addr().String()}
	go p.serve()
	t.Cleanup(func() { pl.Close(); p.drop() })

	deliver := func(subject string) {
		c, err := client.Dial(ln.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Logout()
		if err := c.Login("username", "password"); err != nil {
			t.Error(err)
			return
		}
		msg := "From: a@example.com\r\nTo: b@example.com\r\nSubject: " + subject + "\r\n\r\nbody\r\n"
		if err := c.Append("INBOX", nil, time.Now(), strings.NewReader(msg)); err != nil {
			t.Error(err)
		}
	}
	return p, deliver
}

type log0 struct{}

func (log0) Printf(string, ...any) {}
func (log0) Println(...any)        {}

func (p *imapProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *imapProxy) config() map[string]any {
	return map[string]any{"imapHost": "127.0.0.1", "imapPort": strconv.Itoa(p.port()), "imapSecurity": "none",
		"imapUsername": "username", "imapPassword": "password"}
}

func (p *imapProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.accepts = append(p.accepts, time.Now())
		p.mu.Unlock()
		switch {
		case p.refuse.Load():
			c.Close()
			continue
		case p.stall.Load():
			continue // never greets; closed by drop
		}
		go p.pipe(c)
	}
}

func (p *imapProxy) pipe(c net.Conn) {
	s, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, s)
	p.mu.Unlock()
	go func() { _, _ = io.Copy(s, c); s.Close() }()
	r := bufio.NewReader(s)
	for {
		line, err := r.ReadString('\n')
		if p.stripIdle.Load() {
			line = strings.ReplaceAll(line, " IDLE", "")
		}
		if _, werr := io.WriteString(c, line); werr != nil || err != nil {
			c.Close()
			return
		}
	}
}

// drop closes every open connection.
func (p *imapProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *imapProxy) acceptTimes() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.accepts...)
}
