// Package github is a read-only client for the few GitHub REST endpoints the exporter uses.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/yama6a/gokit/retryhttp"
)

// Failure reasons, as exported in gha_exporter_scan_errors_total.
const (
	ReasonUnauthorized = "unauthorized"
	ReasonForbidden    = "forbidden"
	ReasonNotFound     = "not_found"
	ReasonRateLimited  = "rate_limited"
	ReasonServer       = "server"
	ReasonTransport    = "transport"
	ReasonDecode       = "decode"
)

// Reasons lists every failure reason.
func Reasons() []string {
	return []string{
		ReasonUnauthorized, ReasonForbidden, ReasonNotFound, ReasonRateLimited,
		ReasonServer, ReasonTransport, ReasonDecode,
	}
}

// Error is a failed request. Reset is set for ReasonRateLimited and says when the quota returns.
type Error struct {
	Reason string
	URL    string
	Reset  time.Time
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.URL, e.Reason)
	}
	return fmt.Sprintf("%s: %s: %v", e.URL, e.Reason, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

type cached struct {
	etag   string
	value  any
	header http.Header
}

// Client sends authenticated GET requests and keeps an ETag cache.
// A 304 answer reuses the cached result and costs no rate limit quota.
type Client struct {
	// Retry applies to every request. It retries transport errors and 5xx answers only.
	Retry retryhttp.Policy

	base  string
	token string
	http  *http.Client

	mu        sync.Mutex
	cache     map[string]cached
	remaining int
	reset     time.Time
}

// New returns a Client for the API at base.
func New(base, token string) *Client {
	return &Client{
		Retry: retryhttp.Policy{
			Attempts: 3, Base: time.Second, Max: 30 * time.Second,
			RetryOn: func(resp *http.Response, err error) bool {
				if err != nil {
					return retryhttp.DefaultRetryOn(resp, err)
				}
				return resp.StatusCode >= 500
			},
		},
		base:      base,
		token:     token,
		http:      &http.Client{Timeout: 30 * time.Second, Transport: retryhttp.New(nil)},
		cache:     make(map[string]cached),
		remaining: -1,
	}
}

// Budget returns the last x-ratelimit-remaining and when the quota resets. Remaining is -1 before any answer.
func (c *Client) Budget() (remaining int, reset time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remaining, c.reset
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func get[T any](ctx context.Context, c *Client, url string) (T, http.Header, error) {
	var zero T
	if remaining, reset := c.Budget(); remaining == 0 && time.Now().Before(reset) {
		return zero, nil, &Error{Reason: ReasonRateLimited, URL: url, Reset: reset}
	}

	resp, prev, hit, err := c.do(ctx, url)
	if err != nil {
		return zero, nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		v, ok := prev.value.(T)
		if !hit || !ok {
			return zero, nil, &Error{Reason: ReasonDecode, URL: url}
		}
		return v, prev.header, nil
	default:
		return zero, nil, statusError(resp, url)
	}

	return decode[T](ctx, c, url, resp)
}

func decode[T any](ctx context.Context, c *Client, url string, resp *http.Response) (T, http.Header, error) {
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		if ctx.Err() != nil {
			return v, nil, fmt.Errorf("get %s: %w", url, ctx.Err())
		}
		return v, nil, &Error{Reason: ReasonDecode, URL: url, Err: err}
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.mu.Lock()
		c.cache[url] = cached{etag: etag, value: v, header: resp.Header}
		c.mu.Unlock()
	}
	return v, resp.Header, nil
}

// do sends the request and records the rate limit headers. The caller closes the body.
func (c *Client) do(ctx context.Context, url string) (resp *http.Response, prev cached, hit bool, err error) {
	req, err := http.NewRequestWithContext(retryhttp.WithPolicy(ctx, c.Retry), http.MethodGet, url, nil)
	if err != nil {
		return nil, prev, false, &Error{Reason: ReasonTransport, URL: url, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gha-status-exporter")

	c.mu.Lock()
	prev, hit = c.cache[url]
	c.mu.Unlock()
	if hit {
		req.Header.Set("If-None-Match", prev.etag)
	}

	resp, err = c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, prev, false, fmt.Errorf("get %s: %w", url, ctx.Err())
		}
		return nil, prev, false, &Error{Reason: ReasonTransport, URL: url, Err: err}
	}
	c.recordBudget(resp.Header)
	return resp, prev, hit, nil
}

func (c *Client) recordBudget(h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-Ratelimit-Remaining"))
	if err != nil {
		return
	}
	var reset time.Time
	if secs, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		reset = time.Unix(secs, 0)
	}
	c.mu.Lock()
	c.remaining, c.reset = remaining, reset
	c.mu.Unlock()
}

func statusError(resp *http.Response, url string) *Error {
	e := &Error{URL: url, Err: fmt.Errorf("status %d", resp.StatusCode)}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.Reason = ReasonUnauthorized
	case isRateLimit(resp):
		e.Reason = ReasonRateLimited
		e.Reset = rateLimitReset(resp)
	case resp.StatusCode == http.StatusForbidden:
		e.Reason = ReasonForbidden
	case resp.StatusCode == http.StatusNotFound:
		e.Reason = ReasonNotFound
	default:
		e.Reason = ReasonServer
	}
	return e
}

// isRateLimit tells a quota or abuse limit from a plain 403.
func isRateLimit(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("X-Ratelimit-Remaining") == "0"
	default:
		return false
	}
}

func rateLimitReset(resp *http.Response) time.Time {
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	if secs, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		return time.Unix(secs, 0)
	}
	return time.Now().Add(time.Minute)
}
