// Package proxy observes management credentials and quota without modifying routing.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

const (
	maxBody     = 4 << 20
	parallelism = 4
	// upstreamThrottleBackoff bounds re-querying a usage endpoint that answered
	// 429 without Retry-After.
	upstreamThrottleBackoff = 5 * time.Minute
	// manualRefreshFloor is the minimum age of an account's last real usage
	// query before a manual refresh may query it again.
	manualRefreshFloor = 60 * time.Second
)

// HTTPError never includes response bodies, URLs, or management credentials.
// RetryAt is populated for Retry-After on a management or upstream 429.
type HTTPError struct {
	StatusCode int
	RetryAt    *time.Time
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("quota request returned HTTP %d", e.StatusCode)
}

type Client struct {
	cfg           config.Config
	http          *http.Client
	base          string
	initErr       error
	gate          chan struct{}
	mu            sync.RWMutex
	caps          domain.Capabilities
	prefix        string
	retryAt       time.Time
	authFailure   int
	rejectedKey   [32]byte
	creditRetry   map[string]time.Time
	cache         map[string]probeCache
	anchors       map[string]map[string]time.Time
	anchorStorage AnchorStorage
	now           func() time.Time
	lastRefreshAt time.Time
	nextRefreshAt time.Time
}

type probeCache struct {
	quota     evidence
	next      time.Time
	failures  int
	signature string
	// probed is when the usage endpoint was last actually queried.
	probed time.Time
	// provider is the credential's provider, used by reset signals.
	provider string
}

// New does not make network requests. Configuration errors are returned by Refresh.
func New(c config.Config) *Client {
	if c.HTTPTimeoutSeconds <= 0 {
		c.HTTPTimeoutSeconds = 25
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 180
	}
	if c.Policy.UnknownProbeSeconds <= 0 {
		c.Policy.UnknownProbeSeconds = 900
	}
	if c.Policy.LongProbeSeconds <= 0 {
		c.Policy.LongProbeSeconds = 3600
	}
	if c.Policy.FreshnessSeconds <= 0 {
		c.Policy.FreshnessSeconds = 240
	}
	if c.ManagementKeyEnv == "" {
		c.ManagementKeyEnv = "CPA_MANAGEMENT_KEY"
	}
	if c.ManagementPrefix == "" {
		c.ManagementPrefix = "auto"
	}
	// Copy mutable query configuration so callers cannot race with a refresh.
	c.Queries = append([]config.QuotaQuery(nil), c.Queries...)
	for i := range c.Queries {
		q := &c.Queries[i]
		q.Models = append([]string(nil), q.Models...)
		h := make(map[string]string, len(q.Headers))
		for k, v := range q.Headers {
			h[k] = v
		}
		q.Headers = h
	}
	cl := &Client{cfg: c, gate: make(chan struct{}, 1), cache: map[string]probeCache{}, creditRetry: map[string]time.Time{}, anchors: map[string]map[string]time.Time{}, now: time.Now}
	cl.http = &http.Client{
		Timeout: time.Duration(c.HTTPTimeoutSeconds) * time.Second,
		// Reject all redirects, including same-origin redirects. A management key
		// must never be forwarded to an unverified redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u, err := url.Parse(c.ProxyURL)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		cl.initErr = errors.New("invalid management proxy URL")
	} else if u.Scheme == "http" && u.Hostname() != "localhost" && !isLoopback(u.Hostname()) {
		cl.initErr = errors.New("non-local management proxy requires HTTPS")
	} else {
		cl.base = strings.TrimRight(u.String(), "/")
	}
	if c.ManagementPrefix != "auto" && c.ManagementPrefix != "/v8/management" && c.ManagementPrefix != "/v0/management" {
		cl.initErr = errors.New("unsupported management prefix")
	}
	return cl
}

func isLoopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }

func (c *Client) Capabilities() domain.Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps
}

func (c *Client) setCapabilities(prefix string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps.ManagementPrefix = prefix
	if strings.HasPrefix(prefix, "/v8/") {
		c.caps.ProxyVersion = "v8"
	}
	if strings.HasPrefix(prefix, "/v0/") {
		c.caps.ProxyVersion = "v0"
	}
	c.caps.ProxyConnected = err == nil
	c.caps.Message = "read-only quota monitoring"
	if err != nil {
		c.caps.Message = err.Error()
	}
}

// Refresh lists accounts, then probes independent accounts with bounded parallelism.
// An individual quota error is represented on its Account, not returned as a list error.
func (c *Client) Refresh(ctx context.Context) ([]domain.Account, error) {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { c.publishSchedule(c.now().UTC(), true) }()
	if c.initErr != nil {
		c.setCapabilities("", c.initErr)
		return nil, c.initErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := os.Getenv(c.cfg.ManagementKeyEnv)
	if strings.TrimSpace(key) == "" {
		err := errors.New("management key environment variable is not set")
		c.setCapabilities("", err)
		return nil, err
	}
	keyHash := sha256.Sum256([]byte(key))
	c.mu.RLock()
	authFailure := c.authFailure
	rejected := c.rejectedKey == keyHash
	c.mu.RUnlock()
	if authFailure != 0 && rejected {
		return nil, &HTTPError{StatusCode: authFailure}
	}
	now := c.now().UTC()
	c.mu.RLock()
	retry := c.retryAt
	c.mu.RUnlock()
	if now.Before(retry) {
		return nil, &HTTPError{StatusCode: 429, RetryAt: &retry}
	}
	entries, prefix, err := c.list(ctx, key)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.StatusCode == 401 || he.StatusCode == 403) {
			c.mu.Lock()
			c.authFailure = he.StatusCode
			c.rejectedKey = keyHash
			c.mu.Unlock()
		}
		if errors.As(err, &he) && he.StatusCode == 429 && he.RetryAt != nil {
			c.mu.Lock()
			c.retryAt = *he.RetryAt
			c.mu.Unlock()
		}
		c.setCapabilities(prefix, err)
		return nil, err
	}
	c.prefix = prefix
	c.setCapabilities(prefix, nil)
	filtered := make([]credential, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		if c.cfg.OnlyEmail != "" && !strings.Contains(strings.ToLower(e.account.Email), strings.ToLower(strings.TrimSpace(c.cfg.OnlyEmail))) {
			continue
		}
		if seen[e.account.ID] {
			continue
		}
		seen[e.account.ID] = true
		e.negative = c.credentialEvidence(e, now)
		filtered = append(filtered, e)
	}
	// Stop retaining credentials and cached evidence for removed accounts.
	for id := range c.cache {
		if !seen[id] {
			delete(c.cache, id)
		}
	}
	for id := range c.anchors {
		if !seen[id] {
			delete(c.anchors, id)
		}
	}
	if c.anchorStorage != nil {
		if err := c.anchorStorage.SaveQuotaAnchors(ctx, c.anchors); err != nil {
			c.setCapabilities(prefix, errors.New("quota reset anchor persistence failed"))
			return nil, errors.New("quota reset anchor persistence failed")
		}
	}
	accounts := make([]domain.Account, len(filtered))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var cacheMu sync.Mutex
	workers := min(parallelism, len(filtered))
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				e := filtered[i]
				accountNow := c.now().UTC()
				cacheMu.Lock()
				cached, ok := c.cache[e.account.ID]
				cacheMu.Unlock()
				q, next, failures := cached.quota, cached.next, cached.failures
				// Retry-After is a hard floor. Other cached probes may be invalidated
				// by an actual credential status change or expired positive evidence.
				due := !ok || !accountNow.Before(next)
				if !q.retryAt.After(accountNow) && (cached.signature != e.signature || (q.allowed && !c.fresh(q.checked, accountNow))) {
					due = true
				}
				if due {
					probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.HTTPTimeoutSeconds)*time.Second)
					q = c.probe(probeCtx, key, prefix, e)
					if q.failed && len(q.groups) == 0 && ok && cached.signature == e.signature {
						q.groups = cached.quota.groups
						q.windows = cached.quota.windows
						q.metrics = cached.quota.metrics
						q.models = cached.quota.models
						q.plan = cached.quota.plan
						q.source = cached.quota.source
						q.checked = cached.quota.checked
						// A failed query (often a throttled usage endpoint) is not
						// recovery evidence: an exhaustion already observed stays
						// in force until its windows show a reset.
						if cached.quota.blocked && stillBlocked(cached.quota.windows, accountNow) {
							q.blocked = true
						}
					}
					if !q.failed && len(e.account.Models) == 0 && len(q.models) == 0 {
						for _, model := range c.loadModels(probeCtx, key, prefix, e) {
							q.models = appendUnique(q.models, model)
						}
					}
					cancel()
					if q.failed {
						failures++
					} else {
						failures = 0
					}
					probeNow := c.now().UTC()
					scheduleEvidence := q
					if q.allowed && !q.blocked && !q.failed && completeWindows(q.windows) && c.fresh(q.checked, probeNow) {
						scheduleEvidence = combine(q, independentModelEvidence(e.negative, probeNow))
					} else {
						scheduleEvidence = combine(e.negative, q)
					}
					next = c.nextProbe(scheduleEvidence, probeNow, failures)
					if e.account.Disabled {
						next = probeNow.Add(time.Duration(c.cfg.Policy.UnknownProbeSeconds) * time.Second)
						if q.retryAt.After(next) {
							next = q.retryAt
						}
					}
					cacheMu.Lock()
					c.cache[e.account.ID] = probeCache{q, next, failures, e.signature, probeNow, e.account.Provider}
					cacheMu.Unlock()
				}
				accounts[i] = c.finish(e, q, next, c.now().UTC())
			}
		}()
	}
	for i := range filtered {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	c.mu.RLock()
	denied := c.authFailure
	rejected = c.rejectedKey == keyHash
	c.mu.RUnlock()
	if denied != 0 && rejected {
		err := &HTTPError{StatusCode: denied}
		c.setCapabilities(prefix, err)
		return nil, err
	}
	if c.cfg.PreferPlus {
		sort.SliceStable(accounts, func(i, j int) bool {
			return strings.EqualFold(accounts[i].Plan, "plus") && !strings.EqualFold(accounts[j].Plan, "plus")
		})
	}
	return accounts, ctx.Err()
}

func (c *Client) list(ctx context.Context, key string) ([]credential, string, error) {
	prefix := c.cfg.ManagementPrefix
	if prefix == "auto" {
		prefix = c.prefix
		if prefix == "" {
			prefix = "/v8/management"
		}
	}
	path := "/credentials"
	if prefix == "/v0/management" {
		path = "/auth-files"
	}
	b, _, err := c.request(ctx, key, http.MethodGet, prefix+path, nil)
	var he *HTTPError
	if c.cfg.ManagementPrefix == "auto" && prefix == "/v8/management" && errors.As(err, &he) && (he.StatusCode == 404 || he.StatusCode == 405) {
		prefix = "/v0/management"
		b, _, err = c.request(ctx, key, http.MethodGet, prefix+"/auth-files", nil)
	}
	if err != nil {
		return nil, prefix, err
	}
	entries, err := parseCredentials(b)
	return entries, prefix, err
}

func (c *Client) request(ctx context.Context, key, method, path string, payload any) ([]byte, http.Header, error) {
	c.mu.RLock()
	retry := c.retryAt
	denied := c.authFailure
	rejected := c.rejectedKey == sha256.Sum256([]byte(key))
	c.mu.RUnlock()
	if denied != 0 && rejected {
		return nil, nil, &HTTPError{StatusCode: denied}
	}
	if c.now().Before(retry) {
		return nil, nil, &HTTPError{StatusCode: 429, RetryAt: &retry}
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, errors.New("invalid quota request payload")
		}
		if len(b) > maxBody {
			return nil, nil, errors.New("quota request payload too large")
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, nil, errors.New("invalid management request")
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Accept", "application/json")
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("management request failed (transport or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		he := &HTTPError{StatusCode: resp.StatusCode}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			c.mu.Lock()
			c.authFailure = resp.StatusCode
			c.rejectedKey = sha256.Sum256([]byte(key))
			c.mu.Unlock()
		}
		if resp.StatusCode == 429 {
			he.RetryAt = parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
			if he.RetryAt == nil {
				t := c.now().Add(time.Duration(c.cfg.Policy.UnknownProbeSeconds) * time.Second)
				he.RetryAt = &t
			}
			c.mu.Lock()
			if he.RetryAt.After(c.retryAt) {
				c.retryAt = *he.RetryAt
			}
			c.mu.Unlock()
		}
		return nil, resp.Header, he
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, resp.Header, errors.New("management response read failed")
	}
	if len(b) > maxBody {
		return nil, resp.Header, errors.New("management response body too large")
	}
	return b, resp.Header, nil
}

func parseRetryAfter(value string, now time.Time) *time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64((365*24*time.Hour)/time.Second) {
			seconds = int64((365 * 24 * time.Hour) / time.Second)
		}
		t := now.Add(time.Duration(seconds) * time.Second).UTC()
		return &t
	}
	if t, err := http.ParseTime(value); err == nil {
		t = t.UTC()
		if t.Before(now) {
			t = now.UTC()
		}
		return &t
	}
	return nil
}

func (c *Client) fresh(checked, now time.Time) bool {
	return !checked.IsZero() && !checked.After(now.Add(5*time.Second)) && now.Sub(checked) <= time.Duration(c.cfg.Policy.FreshnessSeconds)*time.Second
}

// resetProbe is the next read-only query for a quota observed exhausted until
// reset. Early is a cadence, not a guessed recovery timestamp; near-reset
// cadence starts within the configured lookahead window. The cadence tightens
// as the reset approaches, and a distant reset is still re-queried every 15
// minutes so an unannounced early reset (a provider-wide reset, for example)
// is noticed without hammering the usage endpoint for days.
func (c *Client) resetProbe(reset, now time.Time) time.Time {
	p := config.EffectivePolicy(c.cfg.Policy)
	seconds := p.EarlyProbeSeconds
	switch until := reset.Sub(now); {
	case until <= time.Duration(p.ResetLookaheadSeconds)*time.Second:
		seconds = p.NearResetProbeSeconds
	case until > 6*time.Hour:
		seconds = max(seconds, 900)
	case until > time.Hour:
		seconds = max(seconds, 300)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func (c *Client) nextProbe(q evidence, now time.Time, failures int) time.Time {
	seconds := c.cfg.PollSeconds
	if q.failed || (!q.allowed && !q.blocked) {
		seconds = c.cfg.Policy.UnknownProbeSeconds
	}
	if q.blocked {
		seconds = c.cfg.Policy.LongProbeSeconds
	}
	// Healthy independent groups must not inherit an exhausted group's long cadence.
	if !q.failed {
		for _, group := range q.groups {
			state, _ := domain.WindowState(group.Windows)
			if state == "allowed" {
				seconds = c.cfg.PollSeconds
				break
			}
		}
	}
	// Exponential error backoff bounded by the configured long-probe interval.
	if failures > 1 {
		for i := 1; i < min(failures, 8); i++ {
			if seconds >= c.cfg.Policy.LongProbeSeconds {
				break
			}
			seconds = min(seconds*2, c.cfg.Policy.LongProbeSeconds)
		}
	}
	next := now.Add(time.Duration(seconds) * time.Second)
	var resets []time.Time
	if q.blocked {
		if reset := availability(q.windows); reset != nil {
			resets = append(resets, *reset)
		}
	}
	if !q.failed {
		// A limited model-scoped group (an Opus weekly limit, say) follows the
		// same reset-aware cadence, so sessions waiting on it resume promptly.
		for _, group := range q.groups {
			if state, at := domain.WindowState(group.Windows); state == "limited" && at != nil && group.Scope != "operation" {
				resets = append(resets, *at)
			}
		}
	}
	for _, reset := range resets {
		if probeAt := c.resetProbe(reset, now); probeAt.Before(next) {
			next = probeAt
		}
	}
	if !q.failed {
		for _, group := range q.groups {
			state, _ := domain.WindowState(group.Windows)
			if state == "allowed" {
				expiry := group.CheckedAt.Add(time.Duration(c.cfg.Policy.FreshnessSeconds) * time.Second)
				if expiry.After(now) && expiry.Before(next) {
					next = expiry
				}
			}
		}
	}
	if q.allowed {
		expiry := q.checked.Add(time.Duration(c.cfg.Policy.FreshnessSeconds) * time.Second)
		if expiry.After(now) && expiry.Before(next) {
			next = expiry
		}
	}
	if floor := now.Add(30 * time.Second); next.Before(floor) {
		next = floor
	}
	if q.retryAt.After(next) {
		next = q.retryAt
	}
	return next
}
