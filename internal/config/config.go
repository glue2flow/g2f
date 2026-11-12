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
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed when determining client addresses (rate limits, logs).
	TrustedProxies []netip.Prefix
	// DisableCodeNode turns off the JavaScript Code node.
	DisableCodeNode bool
	// CodeMemoryMB limits the memory of Code node scripts (0: no limit,
	// scripts run inside the server process).
	CodeMemoryMB int

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
	// WebhookMaxWait bounds webhooks that respond with the workflow result.
	WebhookMaxWait time.Duration
	// MaxStoredItems caps how many items per node output are persisted with an execution.
	MaxStoredItems int
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
	if c.WebhookMaxWait, err = time.ParseDuration(env("G2F_WEBHOOK_MAX_WAIT", "60s")); err != nil {
		return nil, fmt.Errorf("G2F_WEBHOOK_MAX_WAIT: %w", err)
	}

	if c.TrustedProxies, err = parseProxies(env("G2F_TRUSTED_PROXIES", "")); err != nil {
		return nil, fmt.Errorf("G2F_TRUSTED_PROXIES: %w", err)
	}
	if c.PluginSync, err = time.ParseDuration(env("G2F_PLUGIN_SYNC", "30s")); err != nil {
		return nil, fmt.Errorf("G2F_PLUGIN_SYNC: %w", err)
	}
	c.DisableCodeNode = env("G2F_DISABLE_CODE_NODE", "false") == "true"
	if c.CodeMemoryMB, err = envInt("G2F_CODE_MEMORY_MB", 128); err != nil {
		return nil, err
	}
	if c.CodeMemoryMB < 0 {
		return nil, fmt.Errorf("G2F_CODE_MEMORY_MB must not be negative")
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
