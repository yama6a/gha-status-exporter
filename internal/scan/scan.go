// Package scan turns GitHub workflow runs into the failure state the exporter publishes.
package scan

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/yama6a/gha-status-exporter/internal/github"
	"github.com/yama6a/gha-status-exporter/internal/metrics"
)

const (
	maxConcurrent  = 4
	workflowPrefix = ".github/workflows/"
)

// Scanner polls one owner's repos and keeps the last good result of each.
type Scanner struct {
	Client   *github.Client
	Metrics  *metrics.Metrics
	Log      *zap.Logger
	Owner    string
	Reserve  int
	Interval time.Duration
	// MaxRunAge drops a workflow whose deciding run was created longer ago than this. Zero keeps every workflow.
	MaxRunAge time.Duration

	state map[string][]metrics.Workflow
}

// Run scans at start and then every Interval until ctx ends.
// After a rate limit it also waits for the quota to reset.
func (s *Scanner) Run(ctx context.Context) {
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()

	for {
		var ge *github.Error
		if err := s.Scan(ctx); errors.As(err, &ge) && ge.Reason == github.ReasonRateLimited {
			s.Log.Warn("rate limited, waiting for the quota", zap.Time("reset", ge.Reset))
			select {
			case <-ctx.Done():
			case <-time.After(time.Until(ge.Reset)):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Scan reads every repo once and publishes the result. It returns the first error it met.
// Repos that fail keep their previous workflows.
func (s *Scanner) Scan(ctx context.Context) error {
	start := time.Now()
	err := s.scan(ctx)
	s.Metrics.ScanDone(start, time.Now(), err == nil)
	if remaining, _ := s.Client.Budget(); remaining >= 0 {
		s.Metrics.SetRemaining(remaining)
	}
	if err != nil && ctx.Err() == nil {
		s.Log.Warn("scan failed", zap.Error(err))
	}
	return err
}

func (s *Scanner) scan(ctx context.Context) error {
	if remaining, reset := s.Client.Budget(); remaining >= 0 && remaining < s.Reserve && time.Now().Before(reset) {
		s.Metrics.CountError(github.ReasonRateLimited)
		return &github.Error{Reason: github.ReasonRateLimited, URL: "quota", Reset: reset}
	}

	repos, err := s.repos(ctx)
	if err != nil {
		s.count(err)
		return err
	}
	s.Metrics.SetRepos(len(repos))
	if len(repos) == 0 {
		s.Log.Warn("no repos left after filtering", zap.String("owner", s.Owner))
	}

	next, err := s.readRepos(ctx, repos)
	s.state = next
	s.publish()
	return err
}

// readRepos reads every repo. A repo that fails keeps its previous workflows, and a rate limit stops the rest.
// It returns the first error that it counted.
func (s *Scanner) readRepos(ctx context.Context, repos []github.Repo) (map[string][]metrics.Workflow, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		next     = make(map[string][]metrics.Workflow, len(repos))
		firstErr error
		g        errgroup.Group
	)
	g.SetLimit(maxConcurrent)
	for _, r := range repos {
		g.Go(func() error {
			ws, err := s.workflows(ctx, r)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				next[r.Name] = ws
				return nil
			}
			next[r.Name] = s.state[r.Name]
			if s.count(err) && firstErr == nil {
				firstErr = err
			}
			if isRateLimited(err) {
				cancel()
			}
			return nil
		})
	}
	_ = g.Wait()

	if firstErr == nil && ctx.Err() != nil {
		firstErr = fmt.Errorf("scan stopped: %w", ctx.Err())
	}
	return next, firstErr
}

func (s *Scanner) repos(ctx context.Context) ([]github.Repo, error) {
	ownerType, err := s.Client.OwnerType(ctx, s.Owner)
	if err != nil {
		return nil, fmt.Errorf("owner %s: %w", s.Owner, err)
	}
	all, err := s.Client.Repos(ctx, s.Owner, ownerType)
	if err != nil {
		return nil, fmt.Errorf("list repos of %s: %w", s.Owner, err)
	}
	return slices.DeleteFunc(all, func(r github.Repo) bool {
		return !strings.EqualFold(r.Owner.Login, s.Owner) || r.Fork || r.Archived || r.Disabled
	}), nil
}

func (s *Scanner) workflows(ctx context.Context, r github.Repo) ([]metrics.Workflow, error) {
	wfs, err := s.Client.Workflows(ctx, s.Owner, r.Name)
	if err != nil {
		return nil, fmt.Errorf("list workflows of %s: %w", r.Name, err)
	}

	var out []metrics.Workflow
	for _, wf := range wfs {
		if wf.State != "active" || strings.HasPrefix(wf.Path, "dynamic/") {
			continue
		}
		runs, err := s.Client.Runs(ctx, s.Owner, r.Name, wf.ID, r.DefaultBranch)
		if err != nil {
			return nil, fmt.Errorf("list runs of %s/%s: %w", r.Name, wf.Path, err)
		}
		failed, deciding, ok := decide(runs, r)
		if !ok || s.MaxRunAge > 0 && time.Since(deciding.CreatedAt) > s.MaxRunAge {
			continue
		}
		out = append(out, metrics.Workflow{
			Owner: s.Owner, Repo: r.Name, Branch: r.DefaultBranch,
			Workflow: strings.TrimPrefix(wf.Path, workflowPrefix), Name: wf.Name,
			Failed: failed, Updated: deciding.UpdatedAt,
		})
	}
	return out, nil
}

// decide returns the verdict of the newest run that succeeded or failed.
// Cancelled, skipped and similar runs say nothing, so the run before decides.
func decide(runs []github.Run, r github.Repo) (failed bool, deciding github.Run, ok bool) {
	for _, run := range runs {
		if run.Status != "completed" || run.HeadBranch != r.DefaultBranch || run.HeadRepository.ID != r.ID {
			continue
		}
		switch run.Conclusion {
		case "success":
			return false, run, true
		case "failure", "timed_out", "startup_failure":
			return true, run, true
		}
	}
	return false, github.Run{}, false
}

func (s *Scanner) publish() {
	var all []metrics.Workflow
	for _, name := range slices.Sorted(maps.Keys(s.state)) {
		all = append(all, s.state[name]...)
	}
	s.Metrics.SetWorkflows(all)
}

// count adds err to the error counter. It skips a cancelled context and reports whether it counted.
func (s *Scanner) count(err error) bool {
	var ge *github.Error
	if !errors.As(err, &ge) {
		return false
	}
	s.Metrics.CountError(ge.Reason)
	return true
}

func isRateLimited(err error) bool {
	var ge *github.Error
	return errors.As(err, &ge) && ge.Reason == github.ReasonRateLimited
}
