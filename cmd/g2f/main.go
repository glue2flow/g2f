// Command g2f runs the glue2flow workflow automation server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/glue2flow/g2f/internal/api"
	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/config"
	"github.com/glue2flow/g2f/internal/credentials"
	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/oauth"
	"github.com/glue2flow/g2f/internal/plugin"
	"github.com/glue2flow/g2f/internal/push"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/trigger"

	// Native (Go) modules. Each is active only while its manifest is in the
	// modules directory.
	"github.com/glue2flow/g2f/modules/email"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// `g2f healthcheck` lets distroless containers run a HEALTHCHECK without curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	// `g2f plugin keygen|sign|verify|catalog` are tools for plugin publishers.
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		os.Exit(pluginCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Workflow HTTP requests and WASM plugins must not reach private networks
	// unless configured; installed before any plugin is loaded.
	netguard.SetPolicy(netguard.Policy{AllowPrivate: cfg.HTTPAllowPrivate, Allow: cfg.HTTPAllowed})
	netguard.InstallDefaults()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	box, err := secrets.NewBox(cfg.SecretKey, cfg.SecretKeysOld)
	if err != nil {
		return err
	}

	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		return err
	}
	plugins := plugin.NewManager(reg, st, log)
	plugins.SetTrust(plugin.Trust{Keys: cfg.PluginTrustedKeys, RequireSigned: cfg.PluginRequireSigned})
	log.Info("plugin signatures", "trustedKeys", len(cfg.PluginTrustedKeys), "requireSigned", cfg.PluginRequireSigned,
		"catalogs", len(cfg.PluginCatalogs))
	count, err := plugins.LoadDir(ctx, cfg.ModulesDir)
	if err != nil {
		return fmt.Errorf("load modules: %w", err)
	}
	if err := plugins.Sync(ctx); err != nil {
		return fmt.Errorf("load imported plugins: %w", err)
	}
	log.Info("node registry ready", "modulesFromDir", count, "modules", len(reg.Modules()), "nodes", len(reg.Definitions()))
	sec := secrets.NewService(box, st, reg)

	authn, err := auth.New(ctx, cfg.Auth)
	if err != nil {
		return err
	}
	if cfg.Auth.Mode == config.AuthNone {
		log.Warn("AUTHENTICATION DISABLED (G2F_AUTH_MODE=none) - do not use in production")
	}

	oauthMgr := oauth.NewManager(st, box, reg, sec, cfg.PublicURL)
	creds := credentials.NewService(st, box, reg)
	nodes.CodeNodeEnabled = !cfg.DisableCodeNode
	nodes.CodeMemoryLimit = int64(cfg.CodeMemoryMB) << 20
	expression.MaxSteps = int64(cfg.ExpressionMaxSteps)
	signer := trigger.NewResumeSigner(cfg.SecretKey, cfg.PublicURL)
	errDispatch := trigger.NewErrorDispatcher(st, nil, reg, log, cfg.PublicURL)
	eng := engine.New(reg, st, log, engine.Options{
		Secrets:     sec,
		Tokens:      oauthMgr,
		Credentials: creds,
		State:       st,
		Workflows:   st,
		ResumeURL:   signer.URL,
		OnFailure:   errDispatch.WorkflowFailed,
		Workers:     cfg.Workers, Timeout: cfg.ExecutionTimeout, MaxStoredItems: cfg.MaxStoredItems,
	})

	errDispatch.SetEngine(eng)
	// Executions of stopped instances (this one before a restart, or other
	// replicas) are taken over by RunRecovery.
	if err := eng.Register(ctx); err != nil {
		return fmt.Errorf("register instance: %w", err)
	}
	log.Info("engine instance registered", "instance", eng.InstanceID())
	recoveryCtx, stopRecovery := context.WithCancel(context.Background())
	defer stopRecovery()
	go eng.RunRecovery(recoveryCtx, cfg.InstanceTimeout)

	sched := trigger.NewScheduler(st, eng, reg, log, trigger.SchedulerOptions{
		CatchUp: cfg.ScheduleCatchUp, ExecutionRetention: cfg.ExecutionRetention,
		Instance: eng.InstanceID(), CheckWorkers: cfg.PollWorkers, Health: st,
	})
	// Push wakes polling mail triggers (IMAP IDLE, Gmail, Microsoft Graph).
	pushSvc := push.New(push.Options{
		Store: st, Registry: reg, Box: box, Waker: sched.Checks(), Contexts: eng, Tokens: oauthMgr, Settings: sec, Log: log,
		Instance: eng.InstanceID(), PublicURL: cfg.PublicURL, Enabled: cfg.MailPush, IdleMax: cfg.IMAPIdleMax,
	})
	log.Info("mail push", "enabled", cfg.MailPush, "imapIdleMax", cfg.IMAPIdleMax, "pollWorkers", cfg.PollWorkers)
	schedCtx, stopSched := context.WithCancel(context.Background())
	defer stopSched()
	schedDone := make(chan struct{})
	go func() { defer close(schedDone); sched.Run(schedCtx) }()
	pushDone := make(chan struct{})
	go func() { defer close(pushDone); pushSvc.Run(schedCtx) }()

	srv := api.New(api.Deps{
		Config: cfg, Store: st, Registry: reg, Engine: eng, Auth: authn, Logger: log,
		Listener: api.Listeners{sched, pushSvc},
		Webhooks: trigger.NewWebhookHandler(st, eng, reg, sec, log, cfg.WebhookMaxWait),
		Forms: trigger.NewFormHandler(st, eng, reg, log, trigger.FormOptions{
			SecretKey: cfg.SecretKey, PublicURL: cfg.PublicURL,
			State: st, Auth: authn, Mailer: email.OrgMailer{Settings: sec.ModuleSettings},
		}),
		Resume:      trigger.NewResumeHandler(signer, eng, log),
		Secrets:     sec,
		Plugins:     plugins,
		Catalogs:    plugin.NewCatalogs(cfg.PluginCatalogs, plugins, version),
		OAuth:       oauthMgr,
		Credentials: creds,
		Push:        pushSvc.Handler(),
		Delivery:    pushSvc,
	})
	go plugins.Run(schedCtx, cfg.PluginSync)
	go eng.RunResumer(schedCtx, 2*time.Second)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "publicUrl", cfg.PublicURL, "version", version)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	stopSched()
	<-schedDone
	<-pushDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	// Heartbeats continue while running executions finish or are handed over.
	eng.Shutdown(cfg.ShutdownGrace, 20*time.Second)
	stopRecovery()
	return nil
}

func healthcheck() int {
	addr := os.Getenv("G2F_LISTEN")
	if addr == "" || addr[0] == ':' {
		addr = "127.0.0.1" + addr
		if addr == "127.0.0.1" {
			addr += ":8080"
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
