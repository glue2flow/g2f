// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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

	// Issuer is the expected "iss" claim, e.g. http://localhost:8081/realms/g8g.
	Issuer string
	// JWKSURL optionally overrides where signing keys are fetched from. Useful in
	// docker-compose where the browser and the backend reach Keycloak via different hosts.
	JWKSURL     string
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
		Listen:        env("G8G_LISTEN", ":8080"),
		DatabaseURL:   env("G8G_DATABASE_URL", "postgres://g8g:g8g@localhost:5432/g8g?sslmode=disable"),
		PublicURL:     strings.TrimRight(env("G8G_PUBLIC_URL", "http://localhost:8080"), "/"),
		StaticDir:     env("G8G_STATIC_DIR", "./web/dist"),
		ModulesDir:    env("G8G_MODULES_DIR", "./modules"),
		SecretKey:     env("G8G_SECRET_KEY", ""),
		SecretKeysOld: splitList(env("G8G_SECRET_KEYS_OLD", "")),
		Auth: AuthConfig{
			Mode:            AuthMode(env("G8G_AUTH_MODE", string(AuthOIDC))),
			Issuer:          strings.TrimRight(env("G8G_OIDC_ISSUER", ""), "/"),
			JWKSURL:         env("G8G_OIDC_JWKS_URL", ""),
			ClientID:        env("G8G_OIDC_CLIENT_ID", "g8g"),
			GroupsClaim:     env("G8G_OIDC_GROUPS_CLAIM", "groups"),
			OrgClaim:        env("G8G_OIDC_ORG_CLAIM", "org"),
			AdminRole:       env("G8G_OIDC_ADMIN_ROLE", "g8g-admin"),
			PluginAdminRole: env("G8G_OIDC_PLUGIN_ADMIN_ROLE", "g8g-plugin-admin"),
			DefaultOrg:      env("G8G_DEFAULT_ORG", "default"),
			DevUser:         env("G8G_DEV_USER", "dev"),
			DevGroups:       splitList(env("G8G_DEV_GROUPS", "/engineering")),
		},
	}

	var err error
	if c.Workers, err = envInt("G8G_WORKERS", 16); err != nil {
		return nil, err
	}
	if c.MaxStoredItems, err = envInt("G8G_MAX_STORED_ITEMS", 100); err != nil {
		return nil, err
	}
	if c.ExecutionTimeout, err = time.ParseDuration(env("G8G_EXECUTION_TIMEOUT", "5m")); err != nil {
		return nil, fmt.Errorf("G8G_EXECUTION_TIMEOUT: %w", err)
	}

	if c.ScheduleCatchUp, err = time.ParseDuration(env("G8G_SCHEDULE_CATCHUP", "1h")); err != nil {
		return nil, fmt.Errorf("G8G_SCHEDULE_CATCHUP: %w", err)
	}
	if c.WebhookMaxWait, err = time.ParseDuration(env("G8G_WEBHOOK_MAX_WAIT", "60s")); err != nil {
		return nil, fmt.Errorf("G8G_WEBHOOK_MAX_WAIT: %w", err)
	}

	if c.PluginSync, err = time.ParseDuration(env("G8G_PLUGIN_SYNC", "30s")); err != nil {
		return nil, fmt.Errorf("G8G_PLUGIN_SYNC: %w", err)
	}
	if c.SecretKey == "" {
		return nil, fmt.Errorf("G8G_SECRET_KEY is required (generate one with: openssl rand -base64 32)")
	}

	switch c.Auth.Mode {
	case AuthOIDC:
		if c.Auth.Issuer == "" {
			return nil, fmt.Errorf("G8G_OIDC_ISSUER is required when G8G_AUTH_MODE=oidc")
		}
	case AuthNone:
	default:
		return nil, fmt.Errorf("G8G_AUTH_MODE must be 'oidc' or 'none', got %q", c.Auth.Mode)
	}
	return c, nil
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
