// Package config loads and validates all runtime configuration from the
// environment. Nothing in the codebase may read os.Getenv directly; everything
// flows through the Config struct so that required values fail fast at boot.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	App     App
	Log     Log
	DB      DB
	Redis   Redis
	Auth    Auth
	Storage Storage
	Tenancy Tenancy
	Rate    Rate
	SMTP    SMTP
	SMS     SMS
	Worker  Worker
	Obs     Observability
}

type App struct {
	Env             string        `env:"APP_ENV" envDefault:"development"`
	Name            string        `env:"APP_NAME" envDefault:"ComplyDesk"`
	Port            int           `env:"APP_PORT" envDefault:"8080"`
	BaseURL         string        `env:"APP_BASE_URL" envDefault:"http://localhost:8080"`
	FrontendURL     string        `env:"APP_FRONTEND_URL" envDefault:"http://localhost:5173"`
	ShutdownTimeout time.Duration `env:"APP_SHUTDOWN_TIMEOUT" envDefault:"20s"`
	ReadTimeout     time.Duration `env:"APP_READ_TIMEOUT" envDefault:"30s"`
	WriteTimeout    time.Duration `env:"APP_WRITE_TIMEOUT" envDefault:"60s"`
	RequestTimeout  time.Duration `env:"APP_REQUEST_TIMEOUT" envDefault:"30s"`
	MaxBodyBytes    int64         `env:"APP_MAX_BODY_BYTES" envDefault:"10485760"`
}

func (a App) IsProduction() bool  { return a.Env == "production" }
func (a App) IsDevelopment() bool { return a.Env == "development" }

type Log struct {
	Level  string `env:"LOG_LEVEL" envDefault:"info"`
	Format string `env:"LOG_FORMAT" envDefault:"json"`
}

type DB struct {
	Host            string        `env:"DB_HOST" envDefault:"127.0.0.1"`
	Port            int           `env:"DB_PORT" envDefault:"3306"`
	Name            string        `env:"DB_NAME" envDefault:"complydesk"`
	User            string        `env:"DB_USER" envDefault:"root"`
	Password        string        `env:"DB_PASSWORD"`
	Params          string        `env:"DB_PARAMS" envDefault:"parseTime=true&charset=utf8mb4&loc=UTC"`
	MaxOpenConns    int           `env:"DB_MAX_OPEN_CONNS" envDefault:"50"`
	MaxIdleConns    int           `env:"DB_MAX_IDLE_CONNS" envDefault:"25"`
	ConnMaxLifetime time.Duration `env:"DB_CONN_MAX_LIFETIME" envDefault:"30m"`
	ConnMaxIdleTime time.Duration `env:"DB_CONN_MAX_IDLE_TIME" envDefault:"5m"`
	ReplicaHost     string        `env:"DB_REPLICA_HOST"`
	ReplicaPort     int           `env:"DB_REPLICA_PORT" envDefault:"3306"`
}

// DSN builds the go-sql-driver connection string for the primary.
func (d DB) DSN() string {
	return d.dsnFor(d.Host, d.Port)
}

// withClientFoundRows forces MySQL to report MATCHED rows rather than CHANGED
// rows from an UPDATE.
//
// Without it, re-saving a record with identical values reports zero affected
// rows, and every repository that uses affected-rows to detect "does this row
// exist?" answers NOT_FOUND — so saving a form without editing anything looks
// like the record vanished. Matched-row semantics are what that code assumes.
func withClientFoundRows(params string) string {
	if strings.Contains(params, "clientFoundRows=") {
		return params
	}
	if strings.TrimSpace(params) == "" {
		return "clientFoundRows=true"
	}
	return params + "&clientFoundRows=true"
}

// withUTCSession pins the connection's session time zone to UTC.
//
// `loc=UTC` only tells the Go driver how to interpret the bytes it receives; it
// says nothing about what the server thinks "now" is. Without this, every
// DEFAULT CURRENT_TIMESTAMP column is written in the server's local zone while
// the queries that read them compare against UTC_TIMESTAMP() — so on a machine
// at +05:30 a freshly inserted outbox row was stamped five and a half hours in
// the future and never became eligible, and "raised this week" counted the
// wrong window.
//
// Pinning the session makes CURRENT_TIMESTAMP, NOW() and UTC_TIMESTAMP() the
// same clock, which is what the schema and every query already assumed.
func withUTCSession(params string) string {
	if strings.Contains(params, "time_zone=") {
		return params
	}
	// The value has to arrive at the server as the literal '+00:00', so the
	// quotes and colon are percent-encoded for the DSN.
	const tz = "time_zone=%27%2B00%3A00%27"
	if strings.TrimSpace(params) == "" {
		return tz
	}
	return params + "&" + tz
}

// ReplicaDSN returns the replica DSN, or "" when no replica is configured.
func (d DB) ReplicaDSN() string {
	if strings.TrimSpace(d.ReplicaHost) == "" {
		return ""
	}
	return d.dsnFor(d.ReplicaHost, d.ReplicaPort)
}

func (d DB) dsnFor(host string, port int) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		d.User, d.Password, host, port, d.Name, withUTCSession(withClientFoundRows(d.Params)))
}

// AdminDSN connects without selecting a database (used by the CLI to create it).
func (d DB) AdminDSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/?%s", d.User, d.Password, d.Host, d.Port, d.Params)
}

type Redis struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"127.0.0.1:6379"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
	PoolSize int    `env:"REDIS_POOL_SIZE" envDefault:"50"`
}

type Auth struct {
	JWTSecret             string        `env:"JWT_SECRET,required"`
	JWTIssuer             string        `env:"JWT_ISSUER" envDefault:"complydesk"`
	AccessTokenTTL        time.Duration `env:"ACCESS_TOKEN_TTL" envDefault:"15m"`
	RefreshTokenTTL       time.Duration `env:"REFRESH_TOKEN_TTL" envDefault:"168h"`
	ResetTokenTTL         time.Duration `env:"RESET_TOKEN_TTL" envDefault:"30m"`
	ActivationTokenTTL    time.Duration `env:"ACTIVATION_TOKEN_TTL" envDefault:"72h"`
	OTPTTL                time.Duration `env:"OTP_TTL" envDefault:"5m"`
	OTPMaxAttempts        int           `env:"OTP_MAX_ATTEMPTS" envDefault:"5"`
	MaxConcurrentSessions int           `env:"MAX_CONCURRENT_SESSIONS" envDefault:"5"`
	IdleTimeout           time.Duration `env:"IDLE_TIMEOUT" envDefault:"30m"`

	ArgonMemory      uint32 `env:"ARGON_MEMORY" envDefault:"65536"`
	ArgonIterations  uint32 `env:"ARGON_ITERATIONS" envDefault:"3"`
	ArgonParallelism uint8  `env:"ARGON_PARALLELISM" envDefault:"2"`
	ArgonSaltLength  uint32 `env:"ARGON_SALT_LENGTH" envDefault:"16"`
	ArgonKeyLength   uint32 `env:"ARGON_KEY_LENGTH" envDefault:"32"`
}

type Storage struct {
	Driver         string        `env:"STORAGE_DRIVER" envDefault:"local"`
	Root           string        `env:"STORAGE_ROOT" envDefault:"./storage"`
	MaxUploadBytes int64         `env:"STORAGE_MAX_UPLOAD_BYTES" envDefault:"26214400"`
	AllowedExt     []string      `env:"STORAGE_ALLOWED_EXT" envSeparator:"," envDefault:"pdf,jpg,jpeg,png,gif,webp,bmp,tif,tiff,heic,xls,xlsx,doc,docx,ppt,pptx,odt,ods,odp,rtf,txt,csv,zip"`
	MasterKEK      string        `env:"MASTER_KEK,required"`
	SignedURLTTL   time.Duration `env:"SIGNED_URL_TTL" envDefault:"5m"`
}

// kekHowTo tells the operator how to produce a valid key. AES-256 needs exactly
// 32 bytes, and the most common mistake is base64-encoding a 32-character
// passphrase that is not 32 bytes once decoded.
const kekHowTo = "Generate one with:\n" +
	"    openssl rand -base64 32\n" +
	"  or, on Windows without openssl:\n" +
	"    powershell -Command \"[Convert]::ToBase64String((1..32|%{Get-Random -Max 256}))\""

// isPlaceholderKEK reports whether the configured key is one of the values
// shipped in .env.example. The check has to run on the DECODED bytes: the
// encoded form of "change-me..." is "Y2hhbmdl...", so testing the base64 string
// never matched and this guard silently passed for every published example key.
func isPlaceholderKEK(encoded string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		// Undecodable keys are rejected by KEK() with a clearer message.
		return false
	}
	lowered := strings.ToLower(string(raw))
	for _, marker := range []string{"change-me", "changeme", "example", "placeholder", "dev-only"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// KEK decodes the base64 master key-encryption key and validates its length.
func (s Storage) KEK() ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s.MasterKEK))
	if err != nil {
		return nil, fmt.Errorf("MASTER_KEK is not valid base64: %w.\n  %s", err, kekHowTo)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf(
			"MASTER_KEK must decode to exactly 32 bytes for AES-256, got %d.\n  %s",
			len(raw), kekHowTo)
	}
	return raw, nil
}

type Tenancy struct {
	BaseDomain     string   `env:"TENANT_BASE_DOMAIN" envDefault:"complydesk.local"`
	DefaultSlug    string   `env:"TENANT_DEFAULT_SLUG"`
	AllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`
}

// Limit is a parsed "count/window" rate-limit rule.
type Limit struct {
	Count  int
	Window time.Duration
}

type Rate struct {
	Enabled bool   `env:"RATE_LIMIT_ENABLED" envDefault:"true"`
	Login   string `env:"RATE_LIMIT_LOGIN" envDefault:"10/1m"`
	OTP     string `env:"RATE_LIMIT_OTP" envDefault:"5/5m"`
	Forgot  string `env:"RATE_LIMIT_FORGOT" envDefault:"5/15m"`
	Upload  string `env:"RATE_LIMIT_UPLOAD" envDefault:"60/1m"`
	Export  string `env:"RATE_LIMIT_EXPORT" envDefault:"10/5m"`
	Search  string `env:"RATE_LIMIT_SEARCH" envDefault:"120/1m"`
	Default string `env:"RATE_LIMIT_DEFAULT" envDefault:"600/1m"`
}

// Rule parses one of the "N/duration" strings. Unparsable values fall back to
// a conservative 60/1m rather than disabling the limiter.
func (r Rate) Rule(spec string) Limit {
	parts := strings.SplitN(spec, "/", 2)
	if len(parts) != 2 {
		return Limit{Count: 60, Window: time.Minute}
	}
	count, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || count <= 0 {
		return Limit{Count: 60, Window: time.Minute}
	}
	window, err := time.ParseDuration(strings.TrimSpace(parts[1]))
	if err != nil || window <= 0 {
		return Limit{Count: count, Window: time.Minute}
	}
	return Limit{Count: count, Window: window}
}

type SMTP struct {
	Host       string        `env:"SMTP_HOST" envDefault:"127.0.0.1"`
	Port       int           `env:"SMTP_PORT" envDefault:"1025"`
	Username   string        `env:"SMTP_USERNAME"`
	Password   string        `env:"SMTP_PASSWORD"`
	Encryption string        `env:"SMTP_ENCRYPTION" envDefault:"none"`
	FromEmail  string        `env:"SMTP_FROM_EMAIL" envDefault:"no-reply@complydesk.local"`
	FromName   string        `env:"SMTP_FROM_NAME" envDefault:"ComplyDesk"`
	Timeout    time.Duration `env:"SMTP_TIMEOUT" envDefault:"15s"`
}

type SMS struct {
	Enabled  bool   `env:"SMS_ENABLED" envDefault:"false"`
	Provider string `env:"SMS_PROVIDER" envDefault:"custom"`
	Endpoint string `env:"SMS_ENDPOINT"`
	APIKey   string `env:"SMS_API_KEY"`
	SenderID string `env:"SMS_SENDER_ID" envDefault:"CMPLYD"`
}

type Worker struct {
	Concurrency int    `env:"WORKER_CONCURRENCY" envDefault:"10"`
	Queues      string `env:"WORKER_QUEUES" envDefault:"critical:6,default:3,low:1"`
}

// QueueMap turns "critical:6,default:3" into asynq's priority map.
func (w Worker) QueueMap() map[string]int {
	out := map[string]int{}
	for _, pair := range strings.Split(w.Queues, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(kv) != 2 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(kv[1])); err == nil && n > 0 {
			out[strings.TrimSpace(kv[0])] = n
		}
	}
	if len(out) == 0 {
		out["default"] = 1
	}
	return out
}

type Observability struct {
	MetricsEnabled bool   `env:"METRICS_ENABLED" envDefault:"true"`
	MetricsPath    string `env:"METRICS_PATH" envDefault:"/metrics"`
	SwaggerEnabled bool   `env:"SWAGGER_ENABLED" envDefault:"true"`
}

// Load reads .env (when present) then the process environment, and validates
// the result. It returns an error rather than exiting so callers control
// shutdown behaviour.
func Load() (*Config, error) {
	// .env is a developer convenience; a missing file is never an error.
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(); err != nil {
			return nil, fmt.Errorf("loading .env: %w", err)
		}
	}

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parsing environment: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var problems []string

	if len(c.Auth.JWTSecret) < 32 {
		problems = append(problems, "JWT_SECRET must be at least 32 characters")
	}
	if _, err := c.Storage.KEK(); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Storage.Driver != "local" && c.Storage.Driver != "s3" {
		problems = append(problems, "STORAGE_DRIVER must be 'local' or 's3'")
	}

	if c.App.IsProduction() {
		// Refuse to boot production with development placeholders.
		if strings.Contains(c.Auth.JWTSecret, "change-me") {
			problems = append(problems, "JWT_SECRET still holds the development placeholder")
		}
		// The KEK is base64, so the placeholder has to be recognised after
		// decoding — testing the encoded form never matches, which is why this
		// guard silently passed for every shipped example value.
		if isPlaceholderKEK(c.Storage.MasterKEK) {
			problems = append(problems, "MASTER_KEK still holds the development placeholder; "+
				"generate a real key with: openssl rand -base64 32")
		}
		if len(c.Tenancy.AllowedOrigins) == 0 {
			problems = append(problems, "CORS_ALLOWED_ORIGINS must be set in production")
		}
		if c.Tenancy.DefaultSlug != "" {
			problems = append(problems, "TENANT_DEFAULT_SLUG must be empty in production (tenants must resolve explicitly)")
		}
		if !strings.HasPrefix(c.App.BaseURL, "https://") {
			problems = append(problems, "APP_BASE_URL must use https in production")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
