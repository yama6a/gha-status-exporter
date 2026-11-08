// Package config reads the GHA_STATUS_* environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"
)

// Config holds every setting of the exporter.
type Config struct {
	Owner            string
	Token            string
	ScanInterval     time.Duration
	RateLimitReserve int
	MaxRunAge        time.Duration
	Listen           string
	LogLevel         zapcore.Level
}

// Load reads the environment and applies the defaults.
func Load() (Config, error) {
	c := Config{
		Owner:            os.Getenv("GHA_STATUS_OWNER"),
		Token:            os.Getenv("GHA_STATUS_TOKEN"),
		ScanInterval:     5 * time.Minute,
		RateLimitReserve: 1000,
		Listen:           env("GHA_STATUS_LISTEN", ":8080"),
	}
	if err := c.required(); err != nil {
		return c, err
	}

	if v := os.Getenv("GHA_STATUS_SCAN_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("GHA_STATUS_SCAN_INTERVAL must be a positive duration, got %q", v)
		}
		c.ScanInterval = d
	}
	if v := os.Getenv("GHA_STATUS_RATE_LIMIT_RESERVE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("GHA_STATUS_RATE_LIMIT_RESERVE must be a non-negative integer, got %q", v)
		}
		c.RateLimitReserve = n
	}
	age, err := parseAge(os.Getenv("GHA_STATUS_MAX_RUN_AGE"))
	if err != nil {
		return c, err
	}
	c.MaxRunAge = age
	if err := c.LogLevel.UnmarshalText([]byte(env("GHA_STATUS_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("GHA_STATUS_LOG_LEVEL: %w", err)
	}
	return c, nil
}

func (c Config) required() error {
	if c.Owner == "" {
		return errors.New("GHA_STATUS_OWNER is required")
	}
	if c.Token == "" {
		return errors.New("GHA_STATUS_TOKEN is required")
	}
	return nil
}

// parseAge reads GHA_STATUS_MAX_RUN_AGE: a Go duration or whole days such as "8d". Empty means off.
func parseAge(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(v, "d"); ok {
		var n int
		n, err = strconv.Atoi(days)
		d = time.Duration(n) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(v)
	}
	if err != nil || d < 0 {
		return 0, fmt.Errorf("GHA_STATUS_MAX_RUN_AGE must be a non-negative duration such as 192h or 8d, got %q", v)
	}
	return d, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
