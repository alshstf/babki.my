// Package config loads the application configuration from environment variables.
package config

import "github.com/caarlos0/env/v11"

// Config is the entire babki process configuration. One struct for all roles.
type Config struct {
	HTTPAddr    string `env:"BABKI_HTTP_ADDR" envDefault:":8080"`
	DatabaseURL string `env:"BABKI_DATABASE_URL"`
	LogLevel    string `env:"BABKI_LOG_LEVEL" envDefault:"info"`
	LogFormat   string `env:"BABKI_LOG_FORMAT" envDefault:"json"` // json|text
	AutoMigrate bool   `env:"BABKI_AUTO_MIGRATE" envDefault:"true"`
	// EncryptionKey decrypts secrets stored at rest — today the T-Invest
	// broker token, and any future importer's token besides (hence the
	// generic name rather than one naming a single broker). Expected to be
	// 64 hex characters; see secretbox.ParseKey for the exact contract.
	// Required only by the roles that decrypt something at runtime (all,
	// api, worker) — that requirement is enforced in cmd/babki, not here,
	// same as DatabaseURL above.
	EncryptionKey string `env:"BABKI_ENCRYPTION_KEY"`
	// EncryptionKeyPrevious is the key BABKI_ENCRYPTION_KEY replaced, while
	// secrets it sealed are still stored: it opens them, never seals. Empty
	// when no key is being replaced.
	EncryptionKeyPrevious string `env:"BABKI_ENCRYPTION_KEY_PREVIOUS"`
	// CookieSecure marks the session cookie HTTPS-only. Off by default: a home
	// install often runs plain http on the local network, where a cookie marked
	// so would never be sent back. Turn it on behind an HTTPS proxy.
	CookieSecure bool `env:"BABKI_COOKIE_SECURE" envDefault:"false"`
}

// Load reads configuration from env. Does not validate DatabaseURL or
// EncryptionKey: both are required by some roles and not others, so
// validation is command-dependent and checked in cmd.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}
