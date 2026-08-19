package platform

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/karmamgmt/complydesk/internal/config"
)

// slogRedisLogger routes go-redis's internal logging into the application's
// structured logger, so a Redis problem appears in the same stream and format
// as everything else rather than as bare lines on stderr.
type slogRedisLogger struct{}

func (slogRedisLogger) Printf(ctx context.Context, format string, v ...any) {
	slog.WarnContext(ctx, "redis", "detail", fmt.Sprintf(format, v...))
}

// discardRedisLogger drops go-redis output. Installed only around the startup
// probe, whose failure is an expected, handled outcome that OpenRedis already
// reports to the caller.
type discardRedisLogger struct{}

func (discardRedisLogger) Printf(context.Context, string, ...any) {}

func init() {
	redis.SetLogger(slogRedisLogger{})
}

// OpenRedis connects and verifies the connection.
//
// Redis is optional: the caller degrades to no rate limiting and no caching when
// this returns an error. Because a failure here is a normal outcome rather than
// a fault, the driver's own retry chatter is suppressed for the duration of the
// probe — otherwise a routine "Redis is not running" prints five lines saying
// the same thing, four of them unstructured. Logging is restored immediately
// afterwards so a genuine outage *during* operation is still reported.
func OpenRedis(cfg config.Redis) (*redis.Client, error) {
	redis.SetLogger(discardRedisLogger{})
	defer redis.SetLogger(slogRedisLogger{})

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		// The probe is a yes/no question. Retrying it only multiplies the delay
		// before the app can report that Redis is absent and carry on.
		MaxRetries: -1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}
