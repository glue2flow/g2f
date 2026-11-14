package nodes

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

// Push delivery: a polling trigger may also be woken up by its source. A
// wake-up only runs the trigger's normal check right away; the check stays
// the single source of truth and periodic checks continue as a safety net.

// Push endpoints: who notifies the server.
const (
	// PushListen: the module keeps a connection open (e.g. IMAP IDLE) and
	// wakes the trigger itself. Go modules only, see RegisterListener.
	PushListen = "listen"
	// PushGmail: Gmail watches publish to Google Cloud Pub/Sub, whose push
	// subscription calls /push/gmail/<token>.
	PushGmail = "gmail"
	// PushGraph: Microsoft Graph change notifications call /push/graph.
	PushGraph = "graph"
)

// DeliveryParam lets users turn push off for one trigger.
const DeliveryParam = "delivery"

// DeliveryPollOnly is the DeliveryParam value that disables push.
const DeliveryPollOnly = "poll"

func deliveryParam() Param {
	return Param{
		Name: DeliveryParam, Label: "Delivery", Type: ParamSelect, Default: "auto", NoExpression: true,
		Options: []Option{{Label: "Automatic (push when available)", Value: "auto"}, {Label: "Polling only", Value: DeliveryPollOnly}},
		Description: "With push, new mail is checked for as soon as the server reports it; the interval above then " +
			"only applies while push is unavailable (otherwise at least every 15 minutes, as a safety net).",
	}
}

// PushAllowed reports whether the user left push on for a trigger node.
func PushAllowed(params map[string]any) bool {
	v, _ := params[DeliveryParam].(string)
	return v != DeliveryPollOnly
}

// PushSpec declares push delivery for a polling trigger (manifest "push").
//
// For the http runtime, subscribe/renew/unsubscribe are request templates
// like a node's request. Besides params, config and oauth they see
// push.notificationUrl (the server's public endpoint), push.clientState (a
// random secret per subscription), push.expires (now + lifetime, RFC 3339)
// and push.id (the provider's subscription id). id and expires are
// expressions over `response` that read the subscription from the subscribe
// (and renew) response; expires may be RFC 3339 or epoch milliseconds.
type PushSpec struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Per is "trigger" (default) or "connection": one subscription per
	// connected account shared by all its triggers (Gmail watches a mailbox).
	Per string `yaml:"per" json:"per"`
	// Requires lists config entries that must be set for push to be possible
	// (e.g. the Pub/Sub topic). Without them the trigger polls.
	Requires []string `yaml:"requires" json:"requires,omitempty"`
	// Lifetime is the expiry asked for; RenewEvery how often a subscription
	// is renewed (default 24h, and always before it expires).
	Lifetime    string           `yaml:"lifetime" json:"-"`
	RenewEvery  string           `yaml:"renewEvery" json:"-"`
	Subscribe   *ManifestRequest `yaml:"subscribe" json:"-"`
	Renew       *ManifestRequest `yaml:"renew" json:"-"` // default: subscribe again
	Unsubscribe *ManifestRequest `yaml:"unsubscribe" json:"-"`
	ID          string           `yaml:"id" json:"-"`
	Expires     string           `yaml:"expires" json:"-"`

	lifetime, renewEvery time.Duration
}

// LifetimeDuration is the parsed Lifetime (0: the provider decides).
func (p *PushSpec) LifetimeDuration() time.Duration { return p.lifetime }

// RenewInterval is the parsed RenewEvery.
func (p *PushSpec) RenewInterval() time.Duration { return p.renewEvery }

// PerConnection reports whether one subscription serves all triggers of a connection.
func (p *PushSpec) PerConnection() bool { return p.Per == "connection" }

func (p *PushSpec) validate(m *Manifest) error {
	switch p.Per {
	case "":
		p.Per = "trigger"
	case "trigger", "connection":
	default:
		return fmt.Errorf("per must be trigger or connection")
	}
	declared := map[string]bool{}
	for _, c := range m.Config {
		declared[c.Name] = true
	}
	for _, r := range p.Requires {
		if !declared[r] {
			return fmt.Errorf("requires: config %q is not declared", r)
		}
	}
	var err error
	if p.Lifetime != "" {
		if p.lifetime, err = time.ParseDuration(p.Lifetime); err != nil || p.lifetime <= 0 {
			return fmt.Errorf("lifetime: invalid duration %q", p.Lifetime)
		}
	}
	p.renewEvery = 24 * time.Hour
	if p.RenewEvery != "" {
		if p.renewEvery, err = time.ParseDuration(p.RenewEvery); err != nil || p.renewEvery < time.Minute {
			return fmt.Errorf("renewEvery: must be a duration of at least 1m")
		}
	}
	switch p.Endpoint {
	case PushListen:
		if m.Runtime != "go" {
			return fmt.Errorf("endpoint listen needs runtime go")
		}
		if p.Subscribe != nil || p.Renew != nil || p.Unsubscribe != nil {
			return fmt.Errorf("endpoint listen has no requests")
		}
	case PushGmail, PushGraph:
		if m.Runtime != "http" || m.OAuth2 == nil {
			return fmt.Errorf("endpoint %s needs runtime http with oauth2", p.Endpoint)
		}
		if p.Subscribe == nil || p.Subscribe.Method == "" || (p.Subscribe.Path == "" && p.Subscribe.URL == "") {
			return fmt.Errorf("subscribe needs a method and a path or url")
		}
		if p.Subscribe.URL == "" && m.BaseURL == "" {
			return fmt.Errorf("subscribe: url or module baseUrl required")
		}
		if p.Expires == "" {
			return fmt.Errorf("expires is required")
		}
	default:
		return fmt.Errorf("unknown endpoint %q (supported: listen, gmail, graph)", p.Endpoint)
	}
	return nil
}

// --- listeners (go runtime) -------------------------------------------------

// ListenFunc keeps a connection to a trigger's source open until ctx ends
// and calls wake whenever the source reports a change. It calls connected
// once it listens. It returns ErrPushUnsupported (wrapped) when the source
// cannot push, any other error when the connection fails; the caller
// reconnects with backoff. ec is the trigger node's context (config,
// credential, params) as a check would see it.
type ListenFunc func(ctx context.Context, ec *ExecContext, wake func(), connected func()) error

// ErrPushUnsupported: the source cannot notify (e.g. no IMAP IDLE).
var ErrPushUnsupported = errors.New("push not supported by the server")

var (
	listenMu  sync.RWMutex
	listeners = map[string]ListenFunc{}
)

// RegisterListener provides the listener of a native polling trigger whose
// manifest declares push.endpoint: listen. nodeType includes the module.
func RegisterListener(nodeType string, fn ListenFunc) {
	listenMu.Lock()
	defer listenMu.Unlock()
	if _, dup := listeners[nodeType]; dup {
		panic("listener for " + nodeType + " registered twice")
	}
	listeners[nodeType] = fn
}

// Listener returns the listener of a node type.
func Listener(nodeType string) (ListenFunc, bool) {
	listenMu.RLock()
	defer listenMu.RUnlock()
	fn, ok := listeners[nodeType]
	return fn, ok
}

// --- provider subscriptions (http runtime) ----------------------------------

// Pusher is implemented by nodes whose manifest declares push requests.
type Pusher interface {
	PushSpec() *PushSpec
	// PushCall sends the subscribe, renew or unsubscribe request.
	PushCall(ctx context.Context, ec *ExecContext, which string, vars PushVars) (*PushResult, error)
}

// PushVars is the push scope of the request templates.
type PushVars struct {
	NotificationURL string
	ClientState     string
	ID              string
	Expires         time.Time
}

// PushResult is what subscribe and renew return.
type PushResult struct {
	ID      string
	Expires time.Time
}

// PushHTTPError is a provider's refusal; Status 404/410 means the
// subscription does not exist (any more).
type PushHTTPError struct {
	Status int
	Body   string
}

func (e *PushHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func (d *declarativeNode) PushSpec() *PushSpec { return d.spec.Push }

func (d *declarativeNode) PushCall(ctx context.Context, ec *ExecContext, which string, vars PushVars) (*PushResult, error) {
	p := d.spec.Push
	if p == nil || p.Endpoint == PushListen {
		return nil, errors.New("node has no push requests")
	}
	var req *ManifestRequest
	switch which {
	case "subscribe":
		req = p.Subscribe
	case "renew":
		req = p.Renew
		if req == nil {
			req = p.Subscribe
		}
	case "unsubscribe":
		req = p.Unsubscribe
		if req == nil {
			return &PushResult{}, nil // the subscription simply expires
		}
	default:
		return nil, fmt.Errorf("unknown push request %q", which)
	}
	if ec.OAuth == nil {
		return nil, errors.New("no account")
	}
	params, err := ResolveParams(ec, Item{}, 0)
	if err != nil {
		return nil, err
	}
	env := ec.ItemEnv(Item{}, 0)
	env["params"] = params
	env["config"] = ec.Config
	env["oauth"] = map[string]any{"accessToken": ec.OAuth.AccessToken, "tokenType": ec.OAuth.TokenType, "instanceUrl": ec.OAuth.InstanceURL}
	expires := ""
	if !vars.Expires.IsZero() {
		expires = vars.Expires.UTC().Format(time.RFC3339)
	}
	env["push"] = map[string]any{
		"notificationUrl": vars.NotificationURL, "clientState": vars.ClientState, "id": vars.ID, "expires": expires,
	}
	reqURL, err := d.requestURLFor(*req, env)
	if err != nil {
		return nil, err
	}
	// The token only goes to the module's API hosts, as for node requests.
	if !d.tokenHostAllowed(reqURL) {
		return nil, fmt.Errorf("refusing to send the %s access token to %s (not in oauth2.apiHosts)", d.module.Name, hostOf(reqURL))
	}
	resolve := func(v any) (any, error) { return expression.Resolve(v, env) }
	headers := map[string]any{}
	for _, h := range []map[string]any{d.module.Headers, req.Headers} {
		for k, v := range h {
			if headers[k], err = resolve(v); err != nil {
				return nil, err
			}
		}
	}
	if !hasHeader(headers, "Authorization") {
		headers["Authorization"] = ec.OAuth.AuthorizationHeader()
	}
	queryV, err := resolve(anyMap(req.Query))
	if err != nil {
		return nil, err
	}
	bodyV, err := resolve(normalizeYAML(req.Body))
	if err != nil {
		return nil, err
	}
	query, _ := queryV.(map[string]any)
	res, err := doHTTP(ctx, httpRequest{
		Method: req.Method, URL: reqURL, Headers: headers, Query: query, Body: bodyV, Timeout: 30 * time.Second, SameHostOnly: true,
	})
	if err != nil {
		return nil, err
	}
	if status := res["status"].(int); status >= 400 {
		return nil, &PushHTTPError{Status: status, Body: truncate(expression.Stringify(res["body"]), 300)}
	}
	if which == "unsubscribe" {
		return &PushResult{}, nil
	}
	env["response"] = res
	out := &PushResult{ID: vars.ID}
	if p.ID != "" {
		v, err := expression.Render(p.ID, env)
		if err != nil {
			return nil, fmt.Errorf("push id: %w", err)
		}
		if s := expression.Stringify(v); s != "" {
			out.ID = s
		}
	}
	v, err := expression.Render(p.Expires, env)
	if err != nil {
		return nil, fmt.Errorf("push expires: %w", err)
	}
	if out.Expires, err = parseExpiry(v); err != nil {
		return nil, err
	}
	return out, nil
}

// parseExpiry reads an RFC 3339 time or epoch milliseconds (Gmail).
func parseExpiry(v any) (time.Time, error) {
	s := strings.TrimSpace(expression.Stringify(v))
	if ms, err := strconv.ParseFloat(s, 64); err == nil && ms > 0 {
		return time.UnixMilli(int64(ms)).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("push expires: %q is neither RFC 3339 nor epoch milliseconds", s)
}

// HTTPSURL reports whether raw is an https URL on a host other than
// localhost or a loopback address: providers can only notify such URLs.
func HTTPSURL(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h != "localhost" && !strings.HasSuffix(h, ".localhost") && h != "127.0.0.1" && h != "::1"
}
