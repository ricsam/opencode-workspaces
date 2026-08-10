package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/auth"
	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/httpserver"
	"github.com/ricsam/opencode-workspaces/internal/kube"
	"github.com/ricsam/opencode-workspaces/internal/oidc"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := database.Open(ctx, cfg.DatabaseURL, cfg.EncryptionKey)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		logger.Error("migrate database", "error", err)
		os.Exit(1)
	}
	authManager := auth.New(store, cfg.SessionKey, cfg.CookieSecure)
	oidcManager := &oidc.Manager{Store: store, Auth: authManager, PublicURL: cfg.PublicURL, SigningKey: cfg.SessionKey, CookieSecure: cfg.CookieSecure}
	controller, err := kube.New(ctx, store, cfg, logger)
	if err != nil {
		logger.Error("initialize Kubernetes controller", "error", err)
		os.Exit(1)
	}
	go controller.Run(ctx)
	app := httpserver.New(cfg, store, authManager, oidcManager, controller, logger)
	server := &http.Server{Addr: cfg.Address, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 90 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()
	logger.Info("server listening", "address", cfg.Address, "public_url", cfg.PublicURL.String())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
