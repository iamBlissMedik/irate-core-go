package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDevelopmentDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/irate")
	cfg, warnings, err := Load()
	require.NoError(t, err)
	assert.Equal(t, EnvDevelopment, cfg.Env)
	assert.Equal(t, 8000, cfg.HTTPPort)
	assert.Equal(t, "NGN", cfg.Money.DefaultCurrency)
	assert.False(t, cfg.Auth.CookieSecure)
	assert.True(t, cfg.Payments.SandboxSimulationEnabled)
	assert.Len(t, warnings, 2, "insecure dev secrets are flagged")
}

func TestLoadProductionRequiresSecrets(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://db/irate")
	_, _, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_ACCESS_SECRET is required in production")
	assert.Contains(t, err.Error(), "SANDBOX_WEBHOOK_SECRET is required in production")
}

func TestLoadProductionValid(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://db/irate")
	t.Setenv("JWT_ACCESS_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SANDBOX_WEBHOOK_SECRET", "fedcba9876543210fedcba9876543210")
	cfg, warnings, err := Load()
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.True(t, cfg.Auth.CookieSecure, "secure cookies by default in production")
	assert.False(t, cfg.Payments.SandboxSimulationEnabled)
	assert.Equal(t, "json", cfg.LogFormat)
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("PORT", "abc")
	t.Setenv("JWT_ACCESS_SECRET", "too-short")
	t.Setenv("ACCESS_TOKEN_TTL", "forever")
	t.Setenv("BUSINESS_TIMEZONE", "Mars/Olympus")
	t.Setenv("DEFAULT_CURRENCY", "USD")
	_, _, err := Load()
	require.Error(t, err)
	for _, want := range []string{"DATABASE_URL is required", "PORT must be an integer", "JWT_ACCESS_SECRET must be at least",
		"ACCESS_TOKEN_TTL must be a positive duration", "BUSINESS_TIMEZONE", "DEFAULT_CURRENCY"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestLoadProductionForbidsUnsafeOverrides(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://db/irate")
	t.Setenv("JWT_ACCESS_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SANDBOX_WEBHOOK_SECRET", "fedcba9876543210fedcba9876543210")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("PAYMENTS_SANDBOX_SIMULATION", "true")
	t.Setenv("CORS_ALLOWED_ORIGINS", "*")
	_, _, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "COOKIE_SECURE")
	assert.Contains(t, err.Error(), "PAYMENTS_SANDBOX_SIMULATION")
	assert.Contains(t, err.Error(), "CORS_ALLOWED_ORIGINS")
}

func TestLoadPaystack(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/irate")
	t.Setenv("PAYMENTS_PROVIDER", "paystack")
	_, _, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAYSTACK_SECRET_KEY must be a Paystack secret key")

	t.Setenv("PAYSTACK_SECRET_KEY", "sk_test_abc")
	cfg, _, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://api.paystack.co", cfg.Payments.PaystackBaseURL)
	assert.True(t, cfg.Payments.SandboxEnabled(false), "sandbox stays available outside production")
}

func TestLoadProductionWithPaystackDoesNotNeedSandboxSecret(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://db/irate")
	t.Setenv("JWT_ACCESS_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PAYMENTS_PROVIDER", "paystack")
	t.Setenv("PAYSTACK_SECRET_KEY", "sk_live_abc")
	cfg, warnings, err := Load()
	require.NoError(t, err)
	assert.Empty(t, warnings, "no noise about an unused sandbox secret")
	assert.False(t, cfg.Payments.SandboxEnabled(true), "sandbox is not registered in production")

	t.Setenv("PAYSTACK_SECRET_KEY", "sk_test_abc")
	_, warnings, err = Load()
	require.NoError(t, err)
	assert.Contains(t, warnings[0], "TEST key in production")
}
