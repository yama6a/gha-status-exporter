// Package metrics holds the Prometheus registry of the exporter.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yama6a/gha-status-exporter/internal/github"
)

// Workflow is the state of one workflow on its repo's default branch.
type Workflow struct {
	Owner    string
	Repo     string
	Branch   string
	Workflow string
	Name     string
	Failed   bool
	Updated  time.Time
}

// Metrics owns the registry and every series.
type Metrics struct {
	reg         *prometheus.Registry
	workflows   *workflowCollector
	scanSuccess prometheus.Gauge
	repos       prometheus.Gauge
	remaining   prometheus.Gauge
	duration    prometheus.Gauge
	errors      *prometheus.CounterVec
}

// New registers every series. The build_info gauge carries version.
func New(version string) *Metrics {
	m := &Metrics{
		reg:       prometheus.NewRegistry(),
		workflows: &workflowCollector{},
		scanSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gha_exporter_scan_success",
			Help: "1 if the last scan read every repo, else 0.",
		}),
		repos: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gha_exporter_repos",
			Help: "Repos in the last scan, after filtering.",
		}),
		remaining: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gha_exporter_rate_limit_remaining",
			Help: "Last x-ratelimit-remaining.",
		}),
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gha_exporter_scan_duration_seconds",
			Help: "Length of the last scan.",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_exporter_scan_errors_total",
			Help: "Failed requests during scans, by reason.",
		}, []string{"reason"}),
	}
	for _, r := range github.Reasons() {
		m.errors.WithLabelValues(r)
	}

	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "gha_exporter_build_info",
		Help:        "Always 1. The version label holds the build version.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	info.Set(1)

	m.reg.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.workflows, info, m.scanSuccess, m.repos, m.remaining, m.duration, m.errors,
	)
	return m
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// SetWorkflows replaces the workflow series. The caller must not change ws afterwards.
func (m *Metrics) SetWorkflows(ws []Workflow) {
	m.workflows.mu.Lock()
	m.workflows.snapshot = ws
	m.workflows.mu.Unlock()
}

// SetRepos sets the repo count of the last scan.
func (m *Metrics) SetRepos(n int) { m.repos.Set(float64(n)) }

// SetRemaining sets the last known API quota.
func (m *Metrics) SetRemaining(n int) { m.remaining.Set(float64(n)) }

// CountError adds one failed request with the given reason.
func (m *Metrics) CountError(reason string) { m.errors.WithLabelValues(reason).Inc() }

// ScanDone records the end of a scan that started at start.
func (m *Metrics) ScanDone(start, end time.Time, ok bool) {
	m.duration.Set(end.Sub(start).Seconds())
	if !ok {
		m.scanSuccess.Set(0)
		return
	}
	m.scanSuccess.Set(1)
	m.workflows.mu.Lock()
	m.workflows.lastSuccess = end
	m.workflows.mu.Unlock()
}

var (
	workflowLabels = []string{"owner", "repo", "branch", "workflow", "workflow_name"}
	failedDesc     = prometheus.NewDesc("gha_workflow_last_run_failed",
		"1 if the latest decisive completed run on the default branch has an alerting conclusion, else 0.",
		workflowLabels, nil)
	timestampDesc = prometheus.NewDesc("gha_workflow_last_run_timestamp_seconds",
		"updated_at of the latest decisive completed run.", workflowLabels, nil)
	lastSuccessDesc = prometheus.NewDesc("gha_exporter_last_success_timestamp_seconds",
		"End of the last scan that read every repo.", nil, nil)
)

type workflowCollector struct {
	mu          sync.Mutex
	snapshot    []Workflow
	lastSuccess time.Time
}

func (c *workflowCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- failedDesc
	ch <- timestampDesc
	ch <- lastSuccessDesc
}

func (c *workflowCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	snapshot, lastSuccess := c.snapshot, c.lastSuccess
	c.mu.Unlock()

	for _, w := range snapshot {
		labels := []string{w.Owner, w.Repo, w.Branch, w.Workflow, w.Name}
		failed := 0.0
		if w.Failed {
			failed = 1
		}
		ch <- prometheus.MustNewConstMetric(failedDesc, prometheus.GaugeValue, failed, labels...)
		ch <- prometheus.MustNewConstMetric(timestampDesc, prometheus.GaugeValue, float64(w.Updated.Unix()), labels...)
	}
	if !lastSuccess.IsZero() {
		ch <- prometheus.MustNewConstMetric(lastSuccessDesc, prometheus.GaugeValue, float64(lastSuccess.Unix()))
	}
}
