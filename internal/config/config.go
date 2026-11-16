// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/plugin/signing"
)

// demoSecretKeys are published in this repository (docker-compose.yml and the
// Makefile). Anyone can read them, so they must not protect a real deployment.
var demoSecretKeys = []string{
	"ZzhnLWRlbW8ta2V5LU5PVC1mb3ItcHJvZHVjdGlvbiE=",
	"ZGV2LWtleS1kZXYta2V5LWRldi1rZXktZGV2LWtleS0=",
}

type AuthMode string

const (
	AuthOIDC AuthMode = "oidc"
	// AuthNone disables authentication and uses a static development user.
	// Never use it in production.
	AuthNone AuthMode = "none"
)

type Config struct {
	Listen      string
	DatabaseURL string
	PublicURL   string
	StaticDir   string
	ModulesDir  string

	Auth AuthConfig

	// SecretKey (base64, 32 bytes) encrypts node secrets and plugin config.
	SecretKey string
	// SecretKeysOld are previous keys, still accepted for decryption (rotation).
	SecretKeysOld []string
	// PluginSync is how often imported plugins are re-read from the database.
	PluginSync time.Duration
	// PluginTrustedKeys are the publishers whose plugin signatures are accepted.
	PluginTrustedKeys signing.Keys
	// PluginRequireSigned refuses plugins without a valid trusted signature.
	PluginRequireSigned bool
	// PluginCatalogs are the index URLs of plugin catalogs.
	PluginCatalogs []string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed when determining client addresses (rate limits, logs).
	TrustedProxies []netip.Prefix
	// DisableCodeNode turns off the JavaScript Code node.
	DisableCodeNode bool
	// CodeMemoryMB limits the memory of Code node scripts (0: no limit,
	// scripts run inside the server process).
	CodeMemoryMB int
	// ExpressionMaxSteps is the number of predicate iterations (all, filter,
	// map, ...) one expression evaluation may run.
	ExpressionMaxSteps int
	// HTTPAllowPrivate lets workflow HTTP requests reach private and local
	// addresses (SSRF protection off).
	HTTPAllowPrivate bool
	// HTTPAllowed are exceptions to the private network block: IPs, CIDRs and
	// host names.
	HTTPAllowed netguard.AllowList

	// InstanceTimeout: how long an instance may miss heartbeats before other
	// instances take over its running executions.
	InstanceTimeout time.Duration
	// ShutdownGrace: how long running executions may continue on shutdown
	// before they are handed over to another instance.
	ShutdownGrace time.Duration

	Workers          int
	ExecutionTimeout time.Duration
	// ScheduleCatchUp: how late a missed one-time schedule may still fire.
	ScheduleCatchUp time.Duration
	// ExecutionRetention: finished executions older than this are deleted; 0 keeps them forever.
	ExecutionRetention time.Duration
	// WebhookMaxWait bounds webhooks that respond with the workflow result.
	WebhookMaxWait time.Duration
	// MaxStoredItems caps how many items per node output are persisted with an execution.
	MaxStoredItems int

	// MailPush lets mail triggers be woken by IMAP IDLE, Gmail push and
	// Microsoft Graph notifications instead of only polling.
	MailPush bool
	// IMAPIdleMax caps the IMAP IDLE connections one replica keeps open.
	IMAPIdleMax int
	// PollWorkers bounds the polling checks running at once per replica.
	PollWorkers int
}

type AuthConfig struct {
	Mode AuthMode

	// Issuer is the expected "iss" claim, e.g. http://localhost:8081/realms/g2f.
	Issuer string
	// JWKSURL optionally overrides where signing keys are fetched from. Useful in
	// docker-compose where the browser and the backend reach Keycloak via different hosts.
	JWKSURL string
	// TokenURL optionally overrides the token endpoint used by server-side
	// logins (form triggers). Default: derived from JWKSURL, else the issuer.
	TokenURL    string
	ClientID    string
	GroupsClaim string
	OrgClaim    string
	AdminRole   string
	// PluginAdminRole may install plugins for the whole installation.
	PluginAdminRole string
	DefaultOrg      string

	// Development user used when Mode == AuthNone.
	DevUser   string
	DevGroups []string
}

func Load() (*Config, error) {
	c := &Config{
		Listen:        env("G2F_LISTEN", ":8080"),
		DatabaseURL:   env("G2F_DATABASE_URL", "postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable"),
		PublicURL:     strings.TrimRight(env("G2F_PUBLIC_URL", "http://localhost:8080"), "/"),
		StaticDir:     env("G2F_STATIC_DIR", "./web/dist"),
		ModulesDir:    env("G2F_MODULES_DIR", "./modules"),
		SecretKey:     env("G2F_SECRET_KEY", ""),
		SecretKeysOld: splitList(env("G2F_SECRET_KEYS_OLD", "")),
		Auth: AuthConfig{
			Mode:            AuthMode(env("G2F_AUTH_MODE", string(AuthOIDC))),
			Issuer:          strings.TrimRight(env("G2F_OIDC_ISSUER", ""), "/"),
			JWKSURL:         env("G2F_OIDC_JWKS_URL", ""),
			TokenURL:        env("G2F_OIDC_TOKEN_URL", ""),
			ClientID:        env("G2F_OIDC_CLIENT_ID", "g2f"),
			GroupsClaim:     env("G2F_OIDC_GROUPS_CLAIM", "groups"),
			OrgClaim:        env("G2F_OIDC_ORG_CLAIM", "org"),
			AdminRole:       env("G2F_OIDC_ADMIN_ROLE", "g2f-admin"),
			PluginAdminRole: env("G2F_OIDC_PLUGIN_ADMIN_ROLE", "g2f-plugin-admin"),
			DefaultOrg:      env("G2F_DEFAULT_ORG", "default"),
			DevUser:         env("G2F_DEV_USER", "dev"),
			DevGroups:       splitList(env("G2F_DEV_GROUPS", "/engineering")),
		},
	}

	var err error
	if c.Workers, err = envInt("G2F_WORKERS", 16); err != nil {
		return nil, err
	}
	if c.MaxStoredItems, err = envInt("G2F_MAX_STORED_ITEMS", 100); err != nil {
		return nil, err
	}
	if c.ExecutionTimeout, err = time.ParseDuration(env("G2F_EXECUTION_TIMEOUT", "5m")); err != nil {
		return nil, fmt.Errorf("G2F_EXECUTION_TIMEOUT: %w", err)
	}

	if c.ScheduleCatchUp, err = time.ParseDuration(env("G2F_SCHEDULE_CATCHUP", "1h")); err != nil {
		return nil, fmt.Errorf("G2F_SCHEDULE_CATCHUP: %w", err)
	}
	if c.ExecutionRetention, err = parseRetention(env("G2F_EXECUTION_RETENTION", "30d")); err != nil {
		return nil, fmt.Errorf("G2F_EXECUTION_RETENTION: %w", err)
	}
	if c.WebhookMaxWait, err = time.ParseDuration(env("G2F_WEBHOOK_MAX_WAIT", "60s")); err != nil {
		return nil, fmt.Errorf("G2F_WEBHOOK_MAX_WAIT: %w", err)
	}

	if c.TrustedProxies, err = parseProxies(env("G2F_TRUSTED_PROXIES", "")); err != nil {
		return nil, fmt.Errorf("G2F_TRUSTED_PROXIES: %w", err)
	}
	if c.PluginSync, err = time.ParseDuration(env("G2F_PLUGIN_SYNC", "30s")); err != nil {
		return nil, fmt.Errorf("G2F_PLUGIN_SYNC: %w", err)
	}
	if c.PluginTrustedKeys, err = signing.ParseKeys(env("G2F_PLUGIN_TRUSTED_KEYS", "")); err != nil {
		return nil, fmt.Errorf("G2F_PLUGIN_TRUSTED_KEYS: %w", err)
	}
	if c.PluginRequireSigned, err = strconv.ParseBool(env("G2F_PLUGIN_REQUIRE_SIGNED", "false")); err != nil {
		return nil, fmt.Errorf("G2F_PLUGIN_REQUIRE_SIGNED must be true or false")
	}
	if c.PluginRequireSigned && len(c.PluginTrustedKeys) == 0 {
		return nil, fmt.Errorf("G2F_PLUGIN_REQUIRE_SIGNED=true needs at least one key in G2F_PLUGIN_TRUSTED_KEYS")
	}
	if c.PluginCatalogs, err = parseCatalogs(env("G2F_PLUGIN_CATALOGS", "")); err != nil {
		return nil, fmt.Errorf("G2F_PLUGIN_CATALOGS: %w", err)
	}
	c.DisableCodeNode = env("G2F_DISABLE_CODE_NODE", "false") == "true"
	if c.CodeMemoryMB, err = envInt("G2F_CODE_MEMORY_MB", 128); err != nil {
		return nil, err
	}
	if c.CodeMemoryMB < 0 {
		return nil, fmt.Errorf("G2F_CODE_MEMORY_MB must not be negative")
	}
	if c.ExpressionMaxSteps, err = envInt("G2F_EXPRESSION_MAX_STEPS", 2_000_000); err != nil {
		return nil, err
	}
	if c.ExpressionMaxSteps <= 0 {
		return nil, fmt.Errorf("G2F_EXPRESSION_MAX_STEPS must be greater than 0")
	}
	c.HTTPAllowPrivate = env("G2F_HTTP_ALLOW_PRIVATE", "false") == "true"
	if c.HTTPAllowed, err = netguard.ParseAllowList(splitList(env("G2F_HTTP_ALLOWED_HOSTS", ""))); err != nil {
		return nil, fmt.Errorf("G2F_HTTP_ALLOWED_HOSTS: %w", err)
	}
	if c.MailPush, err = strconv.ParseBool(env("G2F_MAIL_PUSH", "true")); err != nil {
		return nil, fmt.Errorf("G2F_MAIL_PUSH must be true or false")
	}
	if c.IMAPIdleMax, err = envInt("G2F_IMAP_IDLE_MAX", 50); err != nil {
		return nil, err
	}
	if c.IMAPIdleMax < 0 {
		return nil, fmt.Errorf("G2F_IMAP_IDLE_MAX must not be negative")
	}
	if c.PollWorkers, err = envInt("G2F_POLL_WORKERS", 8); err != nil {
		return nil, err
	}
	if c.PollWorkers < 1 {
		return nil, fmt.Errorf("G2F_POLL_WORKERS must be at least 1")
	}
	if c.InstanceTimeout, err = time.ParseDuration(env("G2F_INSTANCE_TIMEOUT", "30s")); err != nil {
		return nil, fmt.Errorf("G2F_INSTANCE_TIMEOUT: %w", err)
	}
	if c.InstanceTimeout < 5*time.Second {
		return nil, fmt.Errorf("G2F_INSTANCE_TIMEOUT must be at least 5s")
	}
	if c.ShutdownGrace, err = time.ParseDuration(env("G2F_SHUTDOWN_GRACE", "15s")); err != nil {
		return nil, fmt.Errorf("G2F_SHUTDOWN_GRACE: %w", err)
	}
	if c.SecretKey == "" {
		return nil, fmt.Errorf("G2F_SECRET_KEY is required (generate one with: openssl rand -base64 32)")
	}
	if err = checkDemoKeys(c); err != nil {
		return nil, err
	}

	switch c.Auth.Mode {
	case AuthOIDC:
		if c.Auth.Issuer == "" {
			return nil, fmt.Errorf("G2F_OIDC_ISSUER is required when G2F_AUTH_MODE=oidc")
		}
	case AuthNone:
	default:
		return nil, fmt.Errorf("G2F_AUTH_MODE must be 'oidc' or 'none', got %q", c.Auth.Mode)
	}
	return c, nil
}

// checkDemoKeys refuses the published demo keys unless the public URL is local.
func checkDemoKeys(c *Config) error {
	usesDemo := isDemoKey(c.SecretKey)
	for _, k := range c.SecretKeysOld {
		usesDemo = usesDemo || isDemoKey(k)
	}
	if usesDemo && !isLocalURL(c.PublicURL) {
		return fmt.Errorf("G2F_SECRET_KEY is the public demo key; generate your own with: openssl rand -base64 32 (the demo key is only accepted when G2F_PUBLIC_URL is on localhost)")
	}
	return nil
}

func isDemoKey(key string) bool {
	for _, k := range demoSecretKeys {
		if key == k {
			return true
		}
	}
	return false
}

// isLocalURL reports whether the URL host is localhost or a loopback address.
// URLs that cannot be parsed are not local.
func isLocalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseCatalogs parses catalog index URLs. They must be https; plain http is
// accepted for localhost, where nobody in between can change the index.
func parseCatalogs(s string) ([]string, error) {
	var out []string
	for _, raw := range splitList(s) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("%q is not an http(s) URL", raw)
		}
		if u.Scheme == "http" && !isLocalURL(raw) {
			return nil, fmt.Errorf("%q must use https", raw)
		}
		if u.User != nil {
			return nil, fmt.Errorf("%q must not contain credentials", raw)
		}
		out = append(out, raw)
	}
	return out, nil
}

// parseProxies parses a comma separated list of CIDRs and single IPs.
func parseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range splitList(s) {
		if pre, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pre.Masked())
			continue
		}
		ip, err := netip.ParseAddr(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", p)
		}
		ip = ip.Unmap()
		out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return out, nil
}

// parseRetention parses a Go duration or a number of days with a "d" suffix
// ("30d"). Zero is allowed (disabled); negative values are rejected.
func parseRetention(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	var d time.Duration
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", v)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, err
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return d, nil
}
