// Command g8g runs the workflow automation server.
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

	"github.com/wolfposd/g8g/internal/api"
	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/config"
	"github.com/wolfposd/g8g/internal/engine"
	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// `g8g healthcheck` lets distroless containers run a HEALTHCHECK without curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
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
	if n, err := st.FailInterrupted(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("marked interrupted executions as failed", "count", n)
	}

	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		return err
	}
	count, err := nodes.LoadModules(reg, cfg.ModulesDir)
	if err != nil {
		return fmt.Errorf("load modules: %w", err)
	}
	log.Info("node registry ready", "declarativeNodes", count, "total", len(reg.Definitions()))

	authn, err := auth.New(ctx, cfg.Auth)
	if err != nil {
		return err
	}
	if cfg.Auth.Mode == config.AuthNone {
		log.Warn("AUTHENTICATION DISABLED (G8G_AUTH_MODE=none) - do not use in production")
	}

	eng := engine.New(reg, st, log, engine.Options{
		Workers: cfg.Workers, Timeout: cfg.ExecutionTimeout, MaxStoredItems: cfg.MaxStoredItems,
	})

	srv := api.New(api.Deps{
		Config: cfg, Store: st, Registry: reg, Engine: eng, Auth: authn, Logger: log,
	})
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	eng.Shutdown(20 * time.Second)
	return nil
}

func healthcheck() int {
	addr := os.Getenv("G8G_LISTEN")
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
