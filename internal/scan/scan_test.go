package scan_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // an ETag is a cache key, not a secret
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/yama6a/gha-status-exporter/internal/github"
	"github.com/yama6a/gha-status-exporter/internal/metrics"
	"github.com/yama6a/gha-status-exporter/internal/scan"
)

const token = "ghp_secret_value"

type route struct {
	status int
	body   any
	header http.Header
}

type request struct {
	key         string
	ifNoneMatch string
	auth        string
}

type fake struct {
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string]route
	reqs   []request
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{routes: map[string]route{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	if p := r.URL.Query().Get("page"); p != "" {
		key += "?page=" + p
	}

	f.mu.Lock()
	rt, ok := f.routes[key]
	f.reqs = append(f.reqs, request{key, r.Header.Get("If-None-Match"), r.Header.Get("Authorization")})
	f.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}
	for k, v := range rt.header {
		w.Header()[k] = v
	}
	if rt.status != 0 {
		w.WriteHeader(rt.status)
		return
	}
	b, err := json.Marshal(rt.body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	etag := fmt.Sprintf(`"%x"`, sha1.Sum(b)) //nolint:gosec // see import
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (f *fake) set(key string, body any) { f.setRoute(key, route{body: body}) }

func (f *fake) fail(key string, status int, hdr http.Header) {
	f.setRoute(key, route{status: status, header: hdr})
}

func (f *fake) setRoute(key string, rt route) {
	f.mu.Lock()
	f.routes[key] = rt
	f.mu.Unlock()
}

func (f *fake) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

func (f *fake) count(key string) int {
	n := 0
	for _, r := range f.requests() {
		if r.key == key {
			n++
		}
	}
	return n
}

func repo(id int64, name string) github.Repo {
	r := github.Repo{ID: id, Name: name, DefaultBranch: "main"}
	r.Owner.Login = "yama6a"
	return r
}

func run(repoID int64, conclusion string) github.Run {
	r := github.Run{
		Status: "completed", Conclusion: conclusion, HeadBranch: "main",
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
	r.HeadRepository.ID = repoID
	return r
}

func workflows(ws ...github.Workflow) map[string]any { return map[string]any{"workflows": ws} }
func runs(rs ...github.Run) map[string]any           { return map[string]any{"workflow_runs": rs} }

func wf(state string) github.Workflow {
	return github.Workflow{ID: 10, Name: "CI", Path: ".github/workflows/ci.yaml", State: state}
}

// listOne serves one repo with a single active workflow whose runs are rs.
func listOne(f *fake, rs ...github.Run) {
	f.set("/users/yama6a", map[string]string{"type": "User"})
	f.set("/user/repos", []github.Repo{repo(1, "a")})
	f.set("/repos/yama6a/a/actions/workflows", workflows(wf("active")))
	f.set("/repos/yama6a/a/actions/workflows/10/runs", runs(rs...))
}

type harness struct {
	f    *fake
	m    *metrics.Metrics
	s    *scan.Scanner
	logs *observer.ObservedLogs
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := newFake(t)
	core, logs := observer.New(zapcore.DebugLevel)
	c := github.New(f.srv.URL, token)
	c.Retry.Attempts = 1
	m := metrics.New("test")
	return &harness{f: f, m: m, logs: logs, s: &scan.Scanner{
		Client: c, Metrics: m, Log: zap.New(core), Owner: "yama6a", Reserve: 1000, Interval: time.Minute,
	}}
}

// series returns the exposition lines that start with name.
func (h *harness) series(t *testing.T, name string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	require.NoError(t, err)

	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, name+" ") || strings.HasPrefix(l, name+"{") {
			out = append(out, l)
		}
	}
	return out
}

func (h *harness) failed(t *testing.T, repo string) string {
	t.Helper()
	prefix := fmt.Sprintf(`gha_workflow_last_run_failed{branch="main",owner="yama6a",repo=%q,workflow="ci.yaml",`, repo)
	for _, l := range h.series(t, "gha_workflow_last_run_failed") {
		if strings.HasPrefix(l, prefix) {
			return l[strings.LastIndex(l, " ")+1:]
		}
	}
	return ""
}

func (h *harness) value(t *testing.T, name string) string {
	t.Helper()
	s := h.series(t, name)
	require.Len(t, s, 1, name)
	return s[0][strings.LastIndex(s[0], " ")+1:]
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var ge *github.Error
	require.ErrorAs(t, err, &ge)
	return ge.Reason
}

func TestOwnerTypeSelectsListing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, ownerType, listing string
	}{
		{"user", "User", "/user/repos"},
		{"organization", "Organization", "/orgs/yama6a/repos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			other := repo(2, "b")
			other.Owner.Login = "someone-else"
			h.f.set("/users/yama6a", map[string]string{"type": tt.ownerType})
			h.f.set(tt.listing, []github.Repo{repo(1, "a"), other})
			h.f.set("/repos/yama6a/a/actions/workflows", workflows(wf("active")))
			h.f.set("/repos/yama6a/a/actions/workflows/10/runs", runs(run(1, "success")))

			require.NoError(t, h.s.Scan(t.Context()))

			require.Equal(t, "0", h.failed(t, "a"))
			require.Empty(t, h.failed(t, "b"))
			require.Equal(t, "1", h.value(t, "gha_exporter_repos"))
			require.Equal(t, "1", h.value(t, "gha_exporter_scan_success"))
			require.Equal(t, 1, h.f.count(tt.listing))
		})
	}
}

func TestUnknownOwner(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.f.fail("/users/yama6a", http.StatusNotFound, nil)

	err := h.s.Scan(t.Context())

	require.Equal(t, github.ReasonNotFound, reason(t, err))
	require.Equal(t, "0", h.value(t, "gha_exporter_scan_success"))
	require.Equal(t, `gha_exporter_scan_errors_total{reason="not_found"} 1`, h.series(t, "gha_exporter_scan_errors_total")[2])
}

func TestRepoFilterAndPagination(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	fork, archived, disabled := repo(2, "fork"), repo(3, "old"), repo(4, "off")
	fork.Fork, archived.Archived, disabled.Disabled = true, true, true
	h.f.set("/users/yama6a", map[string]string{"type": "User"})
	h.f.setRoute("/user/repos", route{
		body:   []github.Repo{repo(1, "a"), fork, archived},
		header: http.Header{"Link": {fmt.Sprintf(`<%s/user/repos?page=2>; rel="next"`, h.f.srv.URL)}},
	})
	h.f.set("/user/repos?page=2", []github.Repo{disabled, repo(5, "b")})
	for _, n := range []string{"a", "b"} {
		h.f.set("/repos/yama6a/"+n+"/actions/workflows", workflows(wf("active")))
		h.f.set("/repos/yama6a/"+n+"/actions/workflows/10/runs", runs(run(map[string]int64{"a": 1, "b": 5}[n], "success")))
	}

	require.NoError(t, h.s.Scan(t.Context()))

	require.Equal(t, "2", h.value(t, "gha_exporter_repos"))
	require.Len(t, h.series(t, "gha_workflow_last_run_failed"), 2)
	require.Equal(t, "0", h.failed(t, "b"))
}

func TestConclusions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		runs []github.Run
		want string
	}{
		{"success", []github.Run{run(1, "success")}, "0"},
		{"failure", []github.Run{run(1, "failure")}, "1"},
		{"timed out", []github.Run{run(1, "timed_out")}, "1"},
		{"startup failure", []github.Run{run(1, "startup_failure")}, "1"},
		{"cancelled over success", []github.Run{run(1, "cancelled"), run(1, "success")}, "0"},
		{"skipped over failure", []github.Run{run(1, "skipped"), run(1, "failure")}, "1"},
		{"neutral, stale and action required skip", []github.Run{
			run(1, "neutral"), run(1, "stale"), run(1, "action_required"), run(1, "failure"),
		}, "1"},
		{"only cancelled has no series", []github.Run{run(1, "cancelled")}, ""},
		{"no runs has no series", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			listOne(h.f, tt.runs...)

			require.NoError(t, h.s.Scan(t.Context()))

			require.Equal(t, tt.want, h.failed(t, "a"))
		})
	}
}

func TestUnfinishedRunsNeverCount(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	queued, inProgress := run(1, "failure"), run(1, "failure")
	queued.Status, inProgress.Status = "queued", "in_progress"
	listOne(h.f, queued, inProgress, run(1, "success"))

	require.NoError(t, h.s.Scan(t.Context()))

	require.Equal(t, "0", h.failed(t, "a"))
}

func TestForeignRunsAreIgnored(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	fromFork := run(99, "failure")
	otherBranch := run(1, "failure")
	otherBranch.HeadBranch = "feature"
	listOne(h.f, fromFork, otherBranch, run(1, "success"))

	require.NoError(t, h.s.Scan(t.Context()))

	require.Equal(t, "0", h.failed(t, "a"))
}

func TestWorkflowSelection(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dynamic := github.Workflow{ID: 11, Name: "Copilot", Path: "dynamic/copilot-swe-agent/copilot", State: "active"}
	listOne(h.f, run(1, "failure"))
	h.f.set("/repos/yama6a/a/actions/workflows", workflows(wf("active"), dynamic))
	h.f.set("/repos/yama6a/a/actions/workflows/11/runs", runs(run(1, "failure")))

	require.NoError(t, h.s.Scan(t.Context()))

	require.Len(t, h.series(t, "gha_workflow_last_run_failed"), 1)
	require.Zero(t, h.f.count("/repos/yama6a/a/actions/workflows/11/runs"))

	for _, state := range []string{"disabled_manually", "deleted"} {
		h.f.set("/repos/yama6a/a/actions/workflows", workflows(wf(state)))
		require.NoError(t, h.s.Scan(t.Context()))
		require.Empty(t, h.series(t, "gha_workflow_last_run_failed"), state)
	}
}

func TestSeriesLabels(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "failure"))

	require.NoError(t, h.s.Scan(t.Context()))

	require.Equal(t, []string{
		`gha_workflow_last_run_failed{branch="main",owner="yama6a",repo="a",workflow="ci.yaml",workflow_name="CI"} 1`,
	}, h.series(t, "gha_workflow_last_run_failed"))
	require.Equal(t, []string{
		`gha_workflow_last_run_timestamp_seconds{branch="main",owner="yama6a",repo="a",workflow="ci.yaml",workflow_name="CI"} 1.7e+09`,
	}, h.series(t, "gha_workflow_last_run_timestamp_seconds"))
}

func TestNotModifiedReusesTheCache(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "failure"))

	require.NoError(t, h.s.Scan(t.Context()))
	require.NoError(t, h.s.Scan(t.Context()))

	require.Equal(t, "1", h.failed(t, "a"))
	reqs := h.f.requests()
	require.Len(t, reqs, 8)
	for _, r := range reqs[:4] {
		require.Empty(t, r.ifNoneMatch)
		require.Equal(t, "Bearer "+token, r.auth)
	}
	for _, r := range reqs[4:] {
		require.NotEmpty(t, r.ifNoneMatch, r.key)
	}
}

func TestUnauthorizedKeepsTheOldSeries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "failure"))
	require.NoError(t, h.s.Scan(t.Context()))

	h.f.fail("/users/yama6a", http.StatusUnauthorized, nil)
	err := h.s.Scan(t.Context())

	require.Equal(t, github.ReasonUnauthorized, reason(t, err))
	require.Equal(t, "1", h.failed(t, "a"))
	require.Equal(t, "0", h.value(t, "gha_exporter_scan_success"))
	require.Contains(t, h.series(t, "gha_exporter_scan_errors_total"), `gha_exporter_scan_errors_total{reason="unauthorized"} 1`)
}

func TestServerErrorKeepsOnlyThatRepo(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.f.set("/users/yama6a", map[string]string{"type": "User"})
	h.f.set("/user/repos", []github.Repo{repo(1, "a"), repo(2, "b")})
	for _, n := range []string{"a", "b"} {
		h.f.set("/repos/yama6a/"+n+"/actions/workflows", workflows(wf("active")))
	}
	h.f.set("/repos/yama6a/a/actions/workflows/10/runs", runs(run(1, "failure")))
	h.f.set("/repos/yama6a/b/actions/workflows/10/runs", runs(run(2, "failure")))
	require.NoError(t, h.s.Scan(t.Context()))

	h.f.set("/repos/yama6a/a/actions/workflows/10/runs", runs(run(1, "success")))
	h.f.fail("/repos/yama6a/b/actions/workflows/10/runs", http.StatusBadGateway, nil)
	err := h.s.Scan(t.Context())

	require.Equal(t, github.ReasonServer, reason(t, err))
	require.Equal(t, "0", h.failed(t, "a"))
	require.Equal(t, "1", h.failed(t, "b"))
	require.Equal(t, "0", h.value(t, "gha_exporter_scan_success"))
}

func TestRepoMissingFromTheListLosesItsSeries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "failure"))
	require.NoError(t, h.s.Scan(t.Context()))

	h.f.set("/user/repos", []github.Repo{})
	require.NoError(t, h.s.Scan(t.Context()))

	require.Empty(t, h.series(t, "gha_workflow_last_run_failed"))
	require.Equal(t, "1", h.value(t, "gha_exporter_scan_success"))
}

func TestRateLimitStopsTheScan(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.f.set("/users/yama6a", map[string]string{"type": "User"})
	h.f.set("/user/repos", []github.Repo{repo(1, "a")})
	reset := time.Now().Add(time.Hour)
	h.f.fail("/repos/yama6a/a/actions/workflows", http.StatusForbidden, http.Header{
		"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())},
	})

	err := h.s.Scan(t.Context())

	var ge *github.Error
	require.ErrorAs(t, err, &ge)
	require.Equal(t, github.ReasonRateLimited, ge.Reason)
	require.WithinDuration(t, reset, ge.Reset, 2*time.Second)
	require.Equal(t, "0", h.value(t, "gha_exporter_rate_limit_remaining"))

	before := len(h.f.requests())
	err = h.s.Scan(t.Context())
	require.Equal(t, github.ReasonRateLimited, reason(t, err))
	require.Len(t, h.f.requests(), before, "a scan below the reserve sends nothing")
}

func TestScanBelowReserveIsSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "success"))
	h.f.setRoute("/users/yama6a", route{
		body: map[string]string{"type": "User"},
		header: http.Header{
			"X-Ratelimit-Remaining": {"10"}, "X-Ratelimit-Reset": {fmt.Sprint(time.Now().Add(time.Hour).Unix())},
		},
	})
	require.NoError(t, h.s.Scan(t.Context()))
	before := len(h.f.requests())

	err := h.s.Scan(t.Context())

	require.Equal(t, github.ReasonRateLimited, reason(t, err))
	require.Len(t, h.f.requests(), before)
	require.Equal(t, "0", h.value(t, "gha_exporter_scan_success"))
	require.Contains(t, h.series(t, "gha_exporter_scan_errors_total"), `gha_exporter_scan_errors_total{reason="rate_limited"} 1`)
}

func TestTokenNeverReachesTheLogs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	listOne(h.f, run(1, "failure"))
	require.NoError(t, h.s.Scan(t.Context()))
	h.f.fail("/users/yama6a", http.StatusUnauthorized, nil)
	require.Error(t, h.s.Scan(t.Context()))
	h.f.srv.Close()
	require.Error(t, h.s.Scan(context.Background()))

	require.NotEmpty(t, h.logs.All())
	for _, e := range h.logs.All() {
		require.NotContains(t, fmt.Sprint(e.Message, e.ContextMap()), token)
	}
}

func TestMaxRunAge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		maxAge time.Duration
		age    time.Duration
		want   string
	}{
		{"22 days old, 8d", 8 * 24 * time.Hour, 22 * 24 * time.Hour, ""},
		{"22 days old, off", 0, 22 * 24 * time.Hour, "1"},
		{"2 days old, 8d", 8 * 24 * time.Hour, 2 * 24 * time.Hour, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.s.MaxRunAge = tt.maxAge
			old := run(1, "failure")
			old.UpdatedAt = time.Now().Add(-tt.age)
			listOne(h.f, old, run(1, "success"))

			require.NoError(t, h.s.Scan(t.Context()))

			require.Equal(t, tt.want, h.failed(t, "a"))
			if tt.want == "" {
				require.Empty(t, h.series(t, "gha_workflow_last_run_timestamp_seconds"))
			}
		})
	}
}
