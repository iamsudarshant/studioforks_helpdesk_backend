// Package app is the composition root. It owns construction order and the
// dependency wiring between packages, so no domain package has to know how the
// others are built.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/karmamgmt/complydesk/internal/audit"
	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/catalogue"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/document"
	"github.com/karmamgmt/complydesk/internal/help"
	"github.com/karmamgmt/complydesk/internal/httpx/middleware"
	"github.com/karmamgmt/complydesk/internal/notification"
	"github.com/karmamgmt/complydesk/internal/org"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/storage"
	"github.com/karmamgmt/complydesk/internal/tenant"
	"github.com/karmamgmt/complydesk/internal/ticket"
	"github.com/karmamgmt/complydesk/internal/user"
)

// App holds every long-lived dependency.
type App struct {
	Cfg    *config.Config
	DB     *platform.DB
	Redis  *redis.Client
	Sealer *platform.Sealer
	Store  storage.Storage
	Audit  *audit.Writer

	TenantRepo *tenant.Repository
	UserRepo   *user.Repository
	OrgRepo    *org.Repository
	AuthRepo   *auth.Repository

	DocRepo       *document.Repository
	CatalogueRepo *catalogue.Repository
	HelpRepo      *help.Repository
	NotifyRepo    *notification.Repository
	TicketRepo    *ticket.Repository
	Tickets       *ticket.Service
	Tenants       *tenant.Service
	Auth          *auth.Service
	Documents     *document.Service
	Publisher     *notification.Publisher

	Limiter       *middleware.Limiter
	Authenticator *middleware.Authenticator
	TenantMW      *middleware.TenantMiddleware
}

// New builds the application graph. It returns a cleanup function that must be
// called on shutdown.
func New(ctx context.Context, cfg *config.Config) (*App, func(context.Context), error) {
	configureLogging(cfg)

	db, err := platform.OpenDB(cfg.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to database: %w", err)
	}

	// Redis is required in production but optional in development, so a
	// developer without it can still run the API. Rate limiting fails open and
	// caches are simply skipped.
	rdb, err := platform.OpenRedis(cfg.Redis)
	if err != nil {
		if cfg.App.IsProduction() {
			_ = db.Close()
			return nil, nil, fmt.Errorf("connecting to redis: %w", err)
		}
		slog.Warn("redis unavailable; rate limiting and caching are disabled", "error", err)
		rdb = nil
	}

	kek, err := cfg.Storage.KEK()
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	sealer, err := platform.NewSealer(kek)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	store, err := storage.NewLocal(cfg.Storage, sealer)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("initialising storage: %w", err)
	}

	auditor := audit.NewWriter(db.Primary)
	auditor.Start()

	tenantRepo := tenant.NewRepository(db)
	userRepo := user.NewRepository(db)
	orgRepo := org.NewRepository(db)
	authRepo := auth.NewRepository(db)
	docRepo := document.NewRepository(db)
	catalogueRepo := catalogue.NewRepository(db)
	ticketRepo := ticket.NewRepository(db)
	helpRepo := help.NewRepository(db)
	notifyRepo := notification.NewRepository(db)

	tenants := tenant.NewService(tenantRepo, rdb, cfg)
	documents := document.NewService(docRepo, store, cfg)
	publisher := notification.NewPublisher(db)
	tickets := ticket.NewService(ticketRepo, userRepo, tenants, publisher, auditor)

	tokens := auth.NewTokenService(cfg.Auth)
	hasher := auth.NewHasher(cfg.Auth)

	authSvc := auth.NewService(authRepo, userRepo, tokens, hasher, sealer,
		tenants, publisher, auditor, db, cfg)
	authSvc.SetProfileLookup(newLookup(orgRepo, tenantRepo, db))

	limiter := middleware.NewLimiter(rdb, cfg.Rate.Enabled && rdb != nil)

	app := &App{
		Cfg: cfg, DB: db, Redis: rdb, Sealer: sealer, Store: store, Audit: auditor,
		TenantRepo: tenantRepo, UserRepo: userRepo, OrgRepo: orgRepo, AuthRepo: authRepo,
		DocRepo: docRepo, CatalogueRepo: catalogueRepo,
		HelpRepo: helpRepo, NotifyRepo: notifyRepo,
		TicketRepo: ticketRepo, Tickets: tickets,
		Tenants: tenants, Auth: authSvc, Documents: documents, Publisher: publisher,
		Limiter:       limiter,
		Authenticator: middleware.NewAuthenticator(authSvc),
		TenantMW:      middleware.NewTenant(tenants, cfg.Tenancy, cfg.App.IsProduction()),
	}

	cleanup := func(shutdownCtx context.Context) {
		auditor.Stop(shutdownCtx)
		if rdb != nil {
			_ = rdb.Close()
		}
		if err := db.Close(); err != nil {
			slog.Error("closing database", "error", err)
		}
	}

	return app, cleanup, nil
}

// Health reports dependency status for the readiness probe.
func (a *App) Health(ctx context.Context) map[string]string {
	out := map[string]string{}

	if err := a.DB.Health(ctx); err != nil {
		out["database"] = "down: " + err.Error()
	} else {
		out["database"] = "ok"
	}

	if a.Redis == nil {
		out["redis"] = "not configured"
	} else {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := a.Redis.Ping(pingCtx).Err(); err != nil {
			out["redis"] = "down: " + err.Error()
		} else {
			out["redis"] = "ok"
		}
	}

	if err := a.Store.Health(); err != nil {
		out["storage"] = "down: " + err.Error()
	} else {
		out["storage"] = "ok"
	}

	return out
}

// Ready reports whether every hard dependency is usable.
func (a *App) Ready(ctx context.Context) bool {
	if err := a.DB.Health(ctx); err != nil {
		return false
	}
	if err := a.Store.Health(); err != nil {
		return false
	}
	if a.Cfg.App.IsProduction() && a.Redis != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := a.Redis.Ping(pingCtx).Err(); err != nil {
			return false
		}
	}
	return true
}

func configureLogging(cfg *config.Config) {
	var level slog.Level
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactSensitive}

	var handler slog.Handler
	if cfg.Log.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}

// sensitiveKeys never reach the log, whatever a caller passes.
var sensitiveKeys = map[string]struct{}{
	"password": {}, "new_password": {}, "current_password": {}, "confirm_password": {},
	"token": {}, "access_token": {}, "refresh_token": {}, "mfa_token": {},
	"code": {}, "otp": {}, "secret": {}, "api_key": {}, "authorization": {},
	"pan_number": {}, "uan_number": {}, "pf_number": {}, "esic_number": {},
	"temporary_password": {}, "recovery_code": {}, "recovery_codes": {},
}

func redactSensitive(_ []string, a slog.Attr) slog.Attr {
	if _, sensitive := sensitiveKeys[a.Key]; sensitive {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

// noCacheHeaders is used by handlers that stream sensitive content.
func noCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
}
