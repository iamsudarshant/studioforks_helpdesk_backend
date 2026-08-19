// Command api runs the ComplyDesk HTTP server.
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

	"github.com/karmamgmt/complydesk/internal/app"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/notification"
	"github.com/karmamgmt/complydesk/internal/ticket"
)

func main() {
	if err := run(); err != nil {
		// slog may not be configured yet if config loading failed.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, cleanup, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.App.Port),
		Handler:           application.Router(),
		ReadTimeout:       cfg.App.ReadTimeout,
		WriteTimeout:      cfg.App.WriteTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Background workers. Both are tied to the same context as the server, so a
	// shutdown signal stops them with it rather than leaving them mid-batch.
	//
	// They run in-process deliberately: at this scale a separate worker
	// deployment would add operational surface without buying anything, and
	// both are safe to run in more than one replica — the outbox claims rows
	// row-by-row and the SLA sweep is guarded by ticket_sla_events.
	go notification.NewWorker(application.DB, slog.Default()).Run(ctx)
	go ticket.NewSLASweeper(application.DB, application.Publisher, slog.Default()).Run(ctx)

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("api listening",
			"port", cfg.App.Port,
			"env", cfg.App.Env,
			"base_url", cfg.App.BaseURL,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		cleanup(context.Background())
		return fmt.Errorf("http server: %w", err)

	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	}

	// Stop accepting new work, let in-flight requests finish, then release
	// resources — the audit writer flushes its buffer during cleanup.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.App.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed, closing forcibly", "error", err)
		_ = server.Close()
	}

	cleanup(shutdownCtx)
	slog.Info("shutdown complete")
	return nil
}
