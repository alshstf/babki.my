// Package config loads the application configuration from environment variables.
package config

import "github.com/caarlos0/env/v11"

// Config is the whole process configuration, shared by all roles.
type Config struct {
	HTTPAddr    string `env:"BABKI_HTTP_ADDR" envDefault:":8080"`
	DatabaseURL string `env:"BABKI_DATABASE_URL"`
	LogLevel    string `env:"BABKI_LOG_LEVEL" envDefault:"info"`
	LogFormat   string `env:"BABKI_LOG_FORMAT" envDefault:"json"` // json|text
	AutoMigrate bool   `env:"BABKI_AUTO_MIGRATE" envDefault:"true"`
	// EncryptionKey seals secrets stored at rest, such as broker tokens: 64 hex
	// characters (see secretbox.ParseKey). cmd/babki requires it for the roles
	// that use it.
	EncryptionKey string `env:"BABKI_ENCRYPTION_KEY"`
	// EncryptionKeyPrevious is the key being rotated out: it only opens secrets
	// sealed before the rotation.
	EncryptionKeyPrevious string `env:"BABKI_ENCRYPTION_KEY_PREVIOUS"`
	// CookieSecure marks the session cookie HTTPS-only (on by default, decision
	// Р-15). An install served over plain http turns it off.
	CookieSecure bool `env:"BABKI_COOKIE_SECURE" envDefault:"true"`
	// SetupCode is the one-time code first-run setup asks for. When empty the
	// server generates one and logs it.
	SetupCode string `env:"BABKI_SETUP_CODE"`
}

// Load reads the configuration from the environment. Role-specific
// requirements are checked by the command, not here.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}
