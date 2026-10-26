// Package config loads and validates all runtime configuration from
// environment variables (12-factor). Every problem is collected and reported
// at once, so a misconfigured deployment fails fast with a complete list
// instead of one error per restart.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	// Embed the IANA timezone database so BUSINESS_TIMEZONE works even in
	// minimal container images that ship without /usr/share/zoneinfo.
	_ "time/tzdata"
)

const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"

	minSecretLength = 32
)

type Config struct {
	Env         string
	HTTPPort    int
	MetricsPort int // 0 disables the internal metrics listener

	DatabaseURL        string
	DBMaxConns         int32
	DBMinConns         int32
	DBStatementTimeout time.Duration
	AutoMigrate        bool

	RedisURL string

	LogLevel  slog.Level
	LogFormat string // "json" | "text"

	CORSAllowedOrigins []string
	TrustedProxyHops   int
	RateLimitEnabled   bool
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	IdempotencyTTL     time.Duration

	Auth     AuthConfig
	Money    MoneyConfig
	Payments PaymentsConfig
}

type AuthConfig struct {
	AccessSecret         string
	PreviousAccessSecret string // still accepted for verification during key rotation
	Issuer               string
	Audience             string
	AccessTTL            time.Duration
	RefreshTTL           time.Duration
	RefreshReuseGrace    time.Duration
	RefreshCookieName    string
	RefreshCookiePath    string
	CookieSecure         bool
	CookieDomain         string
	LoginMaxAttempts     int
	LoginLockout         time.Duration
	MaxConcurrentHashes  int
}

type MoneyConfig struct {
	DefaultCurrency     string
	SupportedCurrencies []string
	// Upper bound for any single posting, in minor units.
	MaxTransactionAmount int64
	// Daily outgoing-transfer cap for users without VERIFIED KYC (0 = no cap).
	UnverifiedDailyTransferLimit int64
	// Calendar days for daily limits and dashboard "today" stats.
	BusinessLocation *time.Location
}

type PaymentsConfig struct {
	Provider             string // "sandbox" | "paystack": used for new deposits
	SandboxWebhookSecret string
	PublicBaseURL        string
	// Exposes POST /payments/sandbox/deposits/{id}/simulate. Never in production.
	SandboxSimulationEnabled bool

	PaystackSecretKey   string
	PaystackBaseURL     string
	PaystackCallbackURL string // where Paystack sends the customer after checkout
}

// SandboxEnabled reports whether the sandbox provider is registered: always
// outside production (local testing), and in production only if it is the
// active provider.
func (p PaymentsConfig) SandboxEnabled(prod bool) bool {
	return !prod || p.Provider == "sandbox"
}

func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// SupportsCurrency reports whether wallets may be opened in currency.
func (m MoneyConfig) SupportsCurrency(currency string) bool {
	return slices.Contains(m.SupportedCurrencies, currency)
}

// Load reads the environment. Warnings are non-fatal notes (e.g. insecure
// development defaults) that the caller should log.
func Load() (Config, []string, error) {
	l := &loader{}

	env := l.str("APP_ENV", EnvDevelopment)
	if !slices.Contains([]string{EnvDevelopment, EnvTest, EnvProduction}, env) {
		l.fail("APP_ENV must be one of development, test, production (got %q)", env)
	}
	prod := env == EnvProduction

	// The sandbox is only registered outside production or when it is the
	// active provider; only then is its webhook secret read (and, in
	// production, required).
	sandboxSecret := ""
	if provider := l.str("PAYMENTS_PROVIDER", "sandbox"); !prod || provider == "sandbox" {
		sandboxSecret = l.secret("SANDBOX_WEBHOOK_SECRET", prod, "dev-only-insecure-sandbox-webhook-secret!!")
	}

	cfg := Config{
		Env:                env,
		HTTPPort:           l.int("PORT", 8000),
		MetricsPort:        l.int("METRICS_PORT", 9090),
		DatabaseURL:        l.required("DATABASE_URL"),
		DBMaxConns:         int32(l.int("DB_MAX_CONNS", 20)),
		DBMinConns:         int32(l.int("DB_MIN_CONNS", 2)),
		DBStatementTimeout: l.duration("DB_STATEMENT_TIMEOUT", 10*time.Second),
		AutoMigrate:        l.bool("AUTO_MIGRATE", false),
		RedisURL:           l.str("REDIS_URL", "redis://127.0.0.1:6379/0"),
		LogLevel:           l.level("LOG_LEVEL", ifElse(prod, slog.LevelInfo, slog.LevelDebug)),
		LogFormat:          l.str("LOG_FORMAT", ifElse(prod, "json", "text")),
		CORSAllowedOrigins: l.list("CORS_ALLOWED_ORIGINS", []string{"http://localhost:3000", "http://localhost:9000"}),
		TrustedProxyHops:   l.int("TRUSTED_PROXY_HOPS", 0),
		RateLimitEnabled:   l.bool("RATE_LIMIT_ENABLED", true),
		RequestTimeout:     l.duration("REQUEST_TIMEOUT", 30*time.Second),
		ShutdownTimeout:    l.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		IdempotencyTTL:     l.duration("IDEMPOTENCY_TTL", 24*time.Hour),
		Auth: AuthConfig{
			AccessSecret:         l.secret("JWT_ACCESS_SECRET", prod, "dev-only-insecure-access-secret-change-me!!"),
			PreviousAccessSecret: l.str("JWT_ACCESS_SECRET_PREVIOUS", ""),
			Issuer:               l.str("JWT_ISSUER", "irate-core"),
			Audience:             l.str("JWT_AUDIENCE", "irate-api"),
			AccessTTL:            l.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTTL:           l.duration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
			RefreshReuseGrace:    l.duration("REFRESH_REUSE_GRACE", 20*time.Second),
			RefreshCookieName:    l.str("REFRESH_COOKIE_NAME", "refreshToken"),
			RefreshCookiePath:    l.str("REFRESH_COOKIE_PATH", "/api/v1/auth"),
			CookieSecure:         l.bool("COOKIE_SECURE", prod),
			CookieDomain:         l.str("COOKIE_DOMAIN", ""),
			LoginMaxAttempts:     l.int("LOGIN_MAX_ATTEMPTS", 5),
			LoginLockout:         l.duration("LOGIN_LOCKOUT", 15*time.Minute),
			MaxConcurrentHashes:  l.int("MAX_CONCURRENT_PASSWORD_HASHES", 8),
		},
		Money: MoneyConfig{
			DefaultCurrency:              strings.ToUpper(l.str("DEFAULT_CURRENCY", "NGN")),
			SupportedCurrencies:          upper(l.list("SUPPORTED_CURRENCIES", []string{"NGN"})),
			MaxTransactionAmount:         l.int64("MAX_TRANSACTION_AMOUNT_MINOR", 1_000_000_000),      // ₦10,000,000.00
			UnverifiedDailyTransferLimit: l.int64("UNVERIFIED_DAILY_TRANSFER_LIMIT_MINOR", 5_000_000), // ₦50,000.00
			BusinessLocation:             l.location("BUSINESS_TIMEZONE", "Africa/Lagos"),
		},
		Payments: PaymentsConfig{
			Provider:                 l.str("PAYMENTS_PROVIDER", "sandbox"),
			PublicBaseURL:            strings.TrimRight(l.str("PUBLIC_BASE_URL", "http://localhost:8000"), "/"),
			SandboxWebhookSecret:     sandboxSecret,
			SandboxSimulationEnabled: l.bool("PAYMENTS_SANDBOX_SIMULATION", !prod),
			PaystackSecretKey:        l.str("PAYSTACK_SECRET_KEY", ""),
			PaystackBaseURL:          strings.TrimRight(l.str("PAYSTACK_BASE_URL", "https://api.paystack.co"), "/"),
			PaystackCallbackURL:      l.str("PAYSTACK_CALLBACK_URL", ""),
		},
	}

	cfg.validate(l)

	if len(l.errs) > 0 {
		return Config{}, l.warnings, fmt.Errorf("invalid configuration:\n  - %w", errors.Join(l.errs...))
	}
	return cfg, l.warnings, nil
}

func (c *Config) validate(l *loader) {
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		l.fail("PORT must be a valid TCP port")
	}
	if c.MetricsPort < 0 || c.MetricsPort > 65535 || (c.MetricsPort != 0 && c.MetricsPort == c.HTTPPort) {
		l.fail("METRICS_PORT must be 0 (disabled) or a TCP port different from PORT")
	}
	if c.DBMaxConns < 1 || c.DBMinConns < 0 || c.DBMinConns > c.DBMaxConns {
		l.fail("DB_MAX_CONNS must be >= 1 and DB_MIN_CONNS between 0 and DB_MAX_CONNS")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		l.fail("LOG_FORMAT must be json or text")
	}
	if c.TrustedProxyHops < 0 {
		l.fail("TRUSTED_PROXY_HOPS must be >= 0")
	}
	if c.Auth.AccessTTL < time.Minute || c.Auth.AccessTTL > time.Hour {
		l.fail("ACCESS_TOKEN_TTL must be between 1m and 1h")
	}
	if c.Auth.RefreshTTL < c.Auth.AccessTTL {
		l.fail("REFRESH_TOKEN_TTL must be longer than ACCESS_TOKEN_TTL")
	}
	if c.Auth.LoginMaxAttempts < 1 || c.Auth.MaxConcurrentHashes < 1 {
		l.fail("LOGIN_MAX_ATTEMPTS and MAX_CONCURRENT_PASSWORD_HASHES must be >= 1")
	}
	if p := c.Auth.PreviousAccessSecret; p != "" && len(p) < minSecretLength {
		l.fail("JWT_ACCESS_SECRET_PREVIOUS must be at least %d characters", minSecretLength)
	}
	if !c.Money.SupportsCurrency(c.Money.DefaultCurrency) {
		l.fail("DEFAULT_CURRENCY %q must be listed in SUPPORTED_CURRENCIES", c.Money.DefaultCurrency)
	}
	for _, cur := range c.Money.SupportedCurrencies {
		if len(cur) != 3 {
			l.fail("SUPPORTED_CURRENCIES entries must be 3-letter ISO codes (got %q)", cur)
		}
	}
	if c.Money.MaxTransactionAmount <= 0 {
		l.fail("MAX_TRANSACTION_AMOUNT_MINOR must be > 0")
	}
	if c.Money.UnverifiedDailyTransferLimit < 0 {
		l.fail("UNVERIFIED_DAILY_TRANSFER_LIMIT_MINOR must be >= 0")
	}
	switch c.Payments.Provider {
	case "sandbox":
	case "paystack":
		key := c.Payments.PaystackSecretKey
		if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "sk_live_") {
			l.fail("PAYSTACK_SECRET_KEY must be a Paystack secret key (sk_test_… or sk_live_…) when PAYMENTS_PROVIDER=paystack")
		}
		if c.IsProduction() && strings.HasPrefix(key, "sk_test_") {
			l.warnings = append(l.warnings, "PAYSTACK_SECRET_KEY is a TEST key in production: no real money will be collected")
		}
	default:
		l.fail("PAYMENTS_PROVIDER %q is not supported (available: sandbox, paystack)", c.Payments.Provider)
	}

	if c.IsProduction() {
		if !c.Auth.CookieSecure {
			l.fail("COOKIE_SECURE cannot be disabled in production")
		}
		if c.Payments.SandboxSimulationEnabled {
			l.fail("PAYMENTS_SANDBOX_SIMULATION cannot be enabled in production")
		}
		for _, o := range c.CORSAllowedOrigins {
			if o == "*" {
				l.fail("CORS_ALLOWED_ORIGINS cannot contain * in production")
			}
		}
	}
}

// ---------------------------------------------------------------- loader

type loader struct {
	errs     []error
	warnings []string
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (l *loader) required(key string) string {
	v := l.str(key, "")
	if v == "" {
		l.fail("%s is required", key)
	}
	return v
}

// secret is required and must be strong in production. Outside production a
// clearly-labelled insecure default is used, with a warning.
func (l *loader) secret(key string, prod bool, devDefault string) string {
	v := l.str(key, "")
	switch {
	case v == "" && prod:
		l.fail("%s is required in production", key)
	case v == "":
		l.warnings = append(l.warnings, key+" is not set; using an INSECURE development default")
		return devDefault
	case len(v) < minSecretLength:
		l.fail("%s must be at least %d characters (generate one with: openssl rand -hex 32)", key, minSecretLength)
	}
	return v
}

func (l *loader) int(key string, def int) int {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail("%s must be an integer (got %q)", key, v)
		return def
	}
	return n
}

func (l *loader) int64(key string, def int64) int64 {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail("%s must be an integer (got %q)", key, v)
		return def
	}
	return n
}

func (l *loader) bool(key string, def bool) bool {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s must be a boolean (got %q)", key, v)
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail("%s must be a positive duration like 30s or 15m (got %q)", key, v)
		return def
	}
	return d
}

func (l *loader) list(key string, def []string) []string {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (l *loader) level(key string, def slog.Level) slog.Level {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail("%s must be one of debug, info, warn, error (got %q)", key, v)
		return def
	}
	return lvl
}

func (l *loader) location(key, def string) *time.Location {
	name := l.str(key, def)
	loc, err := time.LoadLocation(name)
	if err != nil {
		l.fail("%s must be an IANA timezone like Africa/Lagos (got %q)", key, name)
		return time.UTC
	}
	return loc
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func ifElse[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
