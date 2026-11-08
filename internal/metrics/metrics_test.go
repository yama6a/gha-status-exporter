package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yama6a/gha-status-exporter/internal/github"
	"github.com/yama6a/gha-status-exporter/internal/metrics"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(b)
}

func TestStartState(t *testing.T) {
	t.Parallel()

	out := scrape(t, metrics.New("v1.2.3"))

	require.Contains(t, out, `gha_exporter_build_info{version="v1.2.3"} 1`)
	require.Contains(t, out, "gha_exporter_scan_success 0")
	require.NotContains(t, out, "gha_exporter_last_success_timestamp_seconds ")
	for _, r := range github.Reasons() {
		require.Contains(t, out, `gha_exporter_scan_errors_total{reason="`+r+`"} 0`)
	}
}

func TestLastSuccessAppearsAfterAGoodScan(t *testing.T) {
	t.Parallel()

	m := metrics.New("dev")
	start := time.Unix(1700000000, 0)

	m.ScanDone(start, start.Add(3*time.Second), false)
	require.NotContains(t, scrape(t, m), "gha_exporter_last_success_timestamp_seconds ")

	m.ScanDone(start, start.Add(3*time.Second), true)
	out := scrape(t, m)
	require.Contains(t, out, "gha_exporter_last_success_timestamp_seconds 1.700000003e+09")
	require.Contains(t, out, "gha_exporter_scan_duration_seconds 3")
	require.Contains(t, out, "gha_exporter_scan_success 1")
}
