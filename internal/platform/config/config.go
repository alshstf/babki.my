// Package config загружает конфигурацию приложения из переменных окружения.
package config

import "github.com/caarlos0/env/v11"

// Config — вся конфигурация процесса babki. Одна структура на все роли.
type Config struct {
	HTTPAddr    string `env:"BABKI_HTTP_ADDR" envDefault:":8080"`
	DatabaseURL string `env:"BABKI_DATABASE_URL"`
	LogLevel    string `env:"BABKI_LOG_LEVEL" envDefault:"info"`
	LogFormat   string `env:"BABKI_LOG_FORMAT" envDefault:"json"` // json|text
	AutoMigrate bool   `env:"BABKI_AUTO_MIGRATE" envDefault:"true"`
}

// Load читает конфигурацию из env. Не валидирует DatabaseURL:
// обязательность зависит от команды и проверяется в cmd.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}
