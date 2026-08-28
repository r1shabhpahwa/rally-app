// Package config loads and validates runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every runtime setting. Everything comes from the environment so
// the container stays stateless apart from the SQLite file.
type Config struct {
	BaseURL  string
	Timezone *time.Location
	DBPath   string
	Addr     string

	OrganizerName     string
	OrganizerEmail    string
	OrganizerPassword string // seeded on first boot only, then ignored

	SMTP SMTPConfig

	BackupDir     string
	BackupKeepFor time.Duration
}

// SMTPConfig describes how to reach the outbound mail server.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
	TLS      string // starttls | tls | none
	RatePerS float64
}

// Enabled reports whether enough SMTP settings are present to attempt delivery.
func (s SMTPConfig) Enabled() bool { return s.Host != "" && s.From != "" }

// Load reads configuration from the environment, applying defaults and
// rejecting values that would fail silently at runtime.
func Load() (*Config, error) {
	c := &Config{
		BaseURL:           strings.TrimRight(env("APP_BASE_URL", ""), "/"),
		DBPath:            env("DB_PATH", "./data/badminton.db"),
		Addr:              ":" + env("PORT", "8080"),
		OrganizerName:     env("ORGANIZER_NAME", "Organizer"),
		OrganizerEmail:    strings.TrimSpace(env("ORGANIZER_EMAIL", "")),
		OrganizerPassword: env("ORGANIZER_PASSWORD", ""),
		BackupDir:         env("BACKUP_DIR", ""),
		BackupKeepFor:     7 * 24 * time.Hour,
	}

	// Every link in every email is built from this. A wrong or missing value is
	// invisible until 32 people click a dead button, so fail at boot instead.
	if c.BaseURL == "" {
		return nil, fmt.Errorf("APP_BASE_URL is required (e.g. https://badminton.example.com)")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return nil, fmt.Errorf("APP_BASE_URL must start with http:// or https://, got %q", c.BaseURL)
	}
	if c.OrganizerEmail == "" {
		return nil, fmt.Errorf("ORGANIZER_EMAIL is required")
	}

	tzName := env("APP_TIMEZONE", "UTC")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("APP_TIMEZONE %q: %w", tzName, err)
	}
	c.Timezone = loc

	port, err := strconv.Atoi(env("SMTP_PORT", "587"))
	if err != nil {
		return nil, fmt.Errorf("SMTP_PORT: %w", err)
	}
	rate, err := strconv.ParseFloat(env("SMTP_RATE_PER_SEC", "1"), 64)
	if err != nil || rate <= 0 {
		return nil, fmt.Errorf("SMTP_RATE_PER_SEC must be a positive number")
	}
	tlsMode := strings.ToLower(env("SMTP_TLS", "starttls"))
	switch tlsMode {
	case "starttls", "tls", "none":
	default:
		return nil, fmt.Errorf("SMTP_TLS must be starttls, tls or none, got %q", tlsMode)
	}

	from := strings.TrimSpace(env("SMTP_FROM", c.OrganizerEmail))
	c.SMTP = SMTPConfig{
		Host:     strings.TrimSpace(env("SMTP_HOST", "")),
		Port:     port,
		Username: env("SMTP_USER", ""),
		Password: env("SMTP_PASS", ""),
		From:     from,
		FromName: env("SMTP_FROM_NAME", c.OrganizerName),
		TLS:      tlsMode,
		RatePerS: rate,
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// TrustProxy reports whether X-Forwarded-For should be believed. It is only
// safe when the app really does sit behind a reverse proxy; otherwise any
// caller could spoof their address and reset their own rate limit.
func TrustProxy() bool {
	v := strings.ToLower(env("TRUST_PROXY", ""))
	return v == "1" || v == "true" || v == "yes"
}
