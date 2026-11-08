package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/yama6a/gha-status-exporter/internal/config"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{
		"GHA_STATUS_OWNER", "GHA_STATUS_TOKEN", "GHA_STATUS_SCAN_INTERVAL",
		"GHA_STATUS_RATE_LIMIT_RESERVE", "GHA_STATUS_MAX_RUN_AGE", "GHA_STATUS_LISTEN", "GHA_STATUS_LOG_LEVEL",
	} {
		t.Setenv(k, kv[k])
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"GHA_STATUS_OWNER": "yama6a", "GHA_STATUS_TOKEN": "t"})

	c, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.Config{
		Owner: "yama6a", Token: "t", ScanInterval: 5 * time.Minute, RateLimitReserve: 1000,
		Listen: ":8080", LogLevel: zapcore.InfoLevel,
	}, c)
}

func TestLoadOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_SCAN_INTERVAL": "30s",
		"GHA_STATUS_RATE_LIMIT_RESERVE": "0", "GHA_STATUS_LISTEN": ":9", "GHA_STATUS_LOG_LEVEL": "debug",
	})

	c, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, c.ScanInterval)
	require.Zero(t, c.RateLimitReserve)
	require.Equal(t, ":9", c.Listen)
	require.Equal(t, zapcore.DebugLevel, c.LogLevel)
}

func TestLoadMaxRunAge(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"8d", 8 * 24 * time.Hour},
		{"192h", 192 * time.Hour},
		{"30m", 30 * time.Minute},
		{"0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			setEnv(t, map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_MAX_RUN_AGE": tt.in})
			c, err := config.Load()
			require.NoError(t, err)
			require.Equal(t, tt.want, c.MaxRunAge)
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"empty owner", map[string]string{"GHA_STATUS_TOKEN": "t"}},
		{"empty token", map[string]string{"GHA_STATUS_OWNER": "o"}},
		{"bad interval", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_SCAN_INTERVAL": "soon"}},
		{"zero interval", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_SCAN_INTERVAL": "0s"}},
		{"bad reserve", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_RATE_LIMIT_RESERVE": "-1"}},
		{"negative max age", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_MAX_RUN_AGE": "-1h"}},
		{"unknown max age unit", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_MAX_RUN_AGE": "8x"}},
		{"negative max age days", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_MAX_RUN_AGE": "-8d"}},
		{"bad level", map[string]string{"GHA_STATUS_OWNER": "o", "GHA_STATUS_TOKEN": "t", "GHA_STATUS_LOG_LEVEL": "loud"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			_, err := config.Load()
			require.Error(t, err)
		})
	}
}
