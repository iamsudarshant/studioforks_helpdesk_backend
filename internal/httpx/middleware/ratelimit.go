package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
)

// Limiter is a fixed-window counter in Redis. A fixed window is deliberate:
// it costs one round trip, and the burst-at-boundary weakness is irrelevant for
// abuse control at these thresholds.
type Limiter struct {
	rdb     *redis.Client
	enabled bool
}

func NewLimiter(rdb *redis.Client, enabled bool) *Limiter {
	return &Limiter{rdb: rdb, enabled: enabled}
}

// Allow increments the counter for key and reports whether the request may
// proceed, along with the seconds until the window resets.
func (l *Limiter) Allow(ctx context.Context, key string, limit config.Limit) (bool, int) {
	if !l.enabled || l.rdb == nil {
		return true, 0
	}

	rkey := "ratelimit:" + key

	pipe := l.rdb.TxPipeline()
	incr := pipe.Incr(ctx, rkey)
	// ExpireNX only sets the TTL on the first increment, so the window is
	// anchored to the first request rather than sliding with every hit.
	pipe.ExpireNX(ctx, rkey, limit.Window)
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail open: Redis being down must not lock every user out of login.
		slog.WarnContext(ctx, "rate limiter unavailable, allowing request", "error", err, "key", key)
		return true, 0
	}

	count := incr.Val()
	if count <= int64(limit.Count) {
		return true, 0
	}

	ttl, err := l.rdb.TTL(ctx, rkey).Result()
	if err != nil || ttl <= 0 {
		ttl = limit.Window
	}
	return false, int(ttl.Seconds()) + 1
}

// Reset clears a counter, used after a successful login so a user who finally
// remembers their password is not still throttled.
func (l *Limiter) Reset(ctx context.Context, key string) {
	if !l.enabled || l.rdb == nil {
		return
	}
	_ = l.rdb.Del(ctx, "ratelimit:"+key).Err()
}

// PerIP limits by client address. Use for unauthenticated endpoints.
func (l *Limiter) PerIP(name string, limit config.Limit) func(http.Handler) http.Handler {
	return l.by(name, limit, func(r *http.Request) string {
		return "ip:" + appctx.ClientIP(r.Context())
	})
}

// PerActor limits by authenticated user, falling back to IP when anonymous.
func (l *Limiter) PerActor(name string, limit config.Limit) func(http.Handler) http.Handler {
	return l.by(name, limit, func(r *http.Request) string {
		if a := appctx.ActorFrom(r.Context()); a != nil {
			return "user:" + a.PublicID
		}
		return "ip:" + appctx.ClientIP(r.Context())
	})
}

// PerTenantIP limits by tenant + IP so one noisy client cannot exhaust another
// tenant's allowance.
func (l *Limiter) PerTenantIP(name string, limit config.Limit) func(http.Handler) http.Handler {
	return l.by(name, limit, func(r *http.Request) string {
		tenant := "-"
		if t := appctx.TenantFrom(r.Context()); t != nil {
			tenant = t.Slug
		}
		return "t:" + tenant + ":ip:" + appctx.ClientIP(r.Context())
	})
}

func (l *Limiter) by(name string, limit config.Limit, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := name + ":" + keyFn(r)
			ok, retryAfter := l.Allow(r.Context(), key, limit)
			if !ok {
				slog.WarnContext(r.Context(), "rate limit exceeded",
					"bucket", name, "path", r.URL.Path, "ip", appctx.ClientIP(r.Context()))
				httpx.Fail(w, r, httpx.ErrRateLimited(retryAfter))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AllowIdentifier throttles an authentication attempt by the identifier the
// caller typed, so spraying one password across many accounts from one IP and
// hammering one account from many IPs are both limited. Returns an error ready
// to return to the client.
func (l *Limiter) AllowIdentifier(ctx context.Context, bucket, identifier string, limit config.Limit) error {
	key := fmt.Sprintf("%s:id:%s", bucket, strings.ToLower(strings.TrimSpace(identifier)))
	ok, retryAfter := l.Allow(ctx, key, limit)
	if !ok {
		return httpx.ErrRateLimited(retryAfter)
	}
	return nil
}

// Backoff returns an increasing delay used to slow repeated failures without
// fully locking an account.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	d := time.Duration(attempt*attempt) * 100 * time.Millisecond
	if d > 3*time.Second {
		return 3 * time.Second
	}
	return d
}
