package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func testConfig() config.Config {
	c := config.Default("")
	c.ProxyURL = "https://proxy.invalid"
	return c
}
func testClient(t *testing.T, cfg config.Config, f roundTripFunc) *Client {
	t.Helper()
	t.Setenv("CPA_MANAGEMENT_KEY", "private-test-management-key")
	c := New(cfg)
	c.http.Transport = f
	return c
}
func upstreamResponse(body string) *http.Response {
	b, _ := json.Marshal(map[string]any{"status_code": 200, "header": map[string]any{}, "body": body})
	return response(200, string(b))
}

// Fixtures use management credential and WHAM response schemas. Extra secret
// fields exercise safe extraction. The HTTP transport never binds a socket.
const credentialFixture = `{"observed_at":"2026-10-11T04:00:00Z","files":[{"id":"codex-one.json","name":"codex-one.json","auth_index":"a1","provider":"codex","email":"One@Example.org","disabled":false,"unavailable":true,"status":"error","status_message":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}","updated_at":"2026-10-11T12:00:00+08:00","id_token":{"chatgpt_account_id":"private-codex-account","plan_type":"plus"},"access_token":"private-access-token","refresh_token":"private-refresh-token"}]}`
const whamAvailable = `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1791716400},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1792000000}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`

func TestVersionDetectionFallbackOnlyMissingRoute(t *testing.T) {
	for _, status := range []int{404, 405, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var paths []string
			c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/v8/management/credentials" {
					return response(status, `{"error":"private-test-management-key"}`), nil
				}
				return response(200, `{"files":[]}`), nil
			})
			_, err := c.Refresh(context.Background())
			fallback := status == 404 || status == 405
			if (err == nil) != fallback {
				t.Fatalf("status %d: error %v", status, err)
			}
			if fallback {
				if len(paths) != 2 || paths[1] != "/v0/management/auth-files" || c.Capabilities().ProxyVersion != "v0" {
					t.Fatalf("unexpected paths/caps: %v %+v", paths, c.Capabilities())
				}
			} else if len(paths) != 1 {
				t.Fatalf("must not mask HTTP %d with fallback: %v", status, paths)
			}
			if strings.Contains(c.Capabilities().Message, "private-test-management-key") {
				t.Fatal("key leaked")
			}
		})
	}
}

func TestCodexRealAPICallAndPrivateSafeFields(t *testing.T) {
	for _, prefix := range []string{"/v8/management", "/v0/management"} {
		t.Run(prefix, func(t *testing.T) {
			cfg := testConfig()
			cfg.ManagementPrefix = prefix
			cfg.Queries = []config.QuotaQuery{{Provider: "codex", URL: codexUsageURL, Models: []string{"explicit-model"}}}
			var calls atomic.Int32
			c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer private-test-management-key" {
					t.Error("missing management bearer")
				}
				if r.Method == http.MethodGet {
					return response(200, credentialFixture), nil
				}
				expected := prefix + "/requests/api-call"
				if prefix == "/v0/management" {
					expected = prefix + "/api-call"
				}
				if r.URL.Path != expected {
					t.Errorf("wrong api-call path: %s", r.URL.Path)
				}
				b, _ := io.ReadAll(r.Body)
				v, err := decodeJSON(b)
				if err != nil {
					return nil, err
				}
				p := object(v)
				if p["url"] == codexResetCreditsURL {
					if p["method"] != "GET" {
						t.Fatal("reset credits must be read-only")
					}
					return upstreamResponse(`{"available_count":3,"applicable_available_count":3,"credits":[]}`), nil
				}
				calls.Add(1)
				if p["url"] != codexUsageURL || p["method"] != "GET" || p["auth_index"] != "a1" {
					t.Errorf("wrong api-call payload: %v", p)
				}
				if _, ok := p["headers"]; ok {
					t.Error("plural headers is not the API schema")
				}
				h := object(p["header"])
				if h["Authorization"] != "Bearer $TOKEN$" || h["Chatgpt-Account-Id"] != "private-codex-account" {
					t.Errorf("wrong upstream header schema")
				}
				return upstreamResponse(whamAvailable), nil
			})
			accounts, err := c.Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(accounts) != 1 || accounts[0].State != "allowed" || accounts[0].Plan != "plus" || len(accounts[0].Models) != 1 {
				t.Fatalf("unexpected accounts: %+v", accounts)
			}
			if !completeWindows(accounts[0].Windows) {
				t.Fatal("incomplete allowed evidence")
			}
			b, _ := json.Marshal(accounts)
			for _, secret := range []string{"private-codex-account", "private-access-token", "private-refresh-token", "private-test-management-key"} {
				if strings.Contains(string(b), secret) {
					t.Errorf("export leaked %s", secret)
				}
			}
			checked := accounts[0].CheckedAt
			accounts, err = c.Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || !accounts[0].CheckedAt.Equal(checked) {
				t.Fatal("cached quota was reprobed or relabelled as fresh")
			}
		})
	}
}

func TestFilteringPlusAndUnknownProvider(t *testing.T) {
	cfg := testConfig()
	cfg.OnlyEmail = "EXAMPLE"
	cfg.Queries = []config.QuotaQuery{{Provider: "unknown-provider", Models: []string{"declared-model"}}}
	var calls int
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, `{"credentials":[{"id":"free","auth_index":"f","provider":"unknown-provider","email":"x@example.org","plan":"free","unavailable":true},{"id":"plus","auth_index":"p","provider":"unknown-provider","email":"y@EXAMPLE.org","plan":"plus"},{"id":"other","provider":"other","email":"z@else.org"}]}`), nil
	})
	accounts, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != "plus" || accounts[1].State != "limited" {
		t.Fatalf("bad filtering or unavailable was removed: %+v", accounts)
	}
	if accounts[0].State != "unknown" || len(accounts[0].Models) != 1 || calls != 1 {
		t.Fatal("provider API/model was invented")
	}
}

func TestExplicitPluginOnlyAndBucketEvidence(t *testing.T) {
	cfg := testConfig()
	cfg.Queries = []config.QuotaQuery{{Provider: "other", PluginID: "configured-plugin", Models: []string{"declared-model"}}}
	var mu sync.Mutex
	var paths []string
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Path == "/v8/management/credentials" {
			return response(200, `{"credentials":[{"id":"other","auth_index":"a/b","provider":"other"}]}`), nil
		}
		return response(200, `{"subscription":{"plan":"paid"},"groups":[{"displayName":"quota","buckets":[{"window":"daily","remainingFraction":0.8,"resetTime":"2026-10-12T00:00:00+08:00"}]}]}`), nil
	})
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].State != "allowed" || a[0].Plan != "paid" {
		t.Fatalf("plugin evidence: %+v", a)
	}
	if len(paths) != 2 || paths[1] != "/v8/management/plugins/configured-plugin/quota?auth_index=a%2Fb" {
		t.Fatalf("unexpected plugin request: %v", paths)
	}
}

func TestUpstreamHTTP200EnvelopeIsNotSuccess(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					return response(200, credentialFixture), nil
				}
				return response(200, fmt.Sprintf(`{"status_code":%d,"header":{"Retry-After":["1200"]},"body":"private-access-token"}`, status)), nil
			})
			a, err := c.Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if a[0].State == "allowed" {
				t.Fatalf("upstream HTTP %d falsely allowed", status)
			}
			if status == 429 && a[0].NextCheckAt.Before(c.now().Add(1199*time.Second)) {
				t.Fatal("upstream Retry-After ignored")
			}
			b, _ := json.Marshal(a)
			if strings.Contains(string(b), "private-access-token") {
				t.Fatal("error body leaked")
			}
		})
	}
}

func TestManagementRetryAfterAndBodyLimitAndRedirect(t *testing.T) {
	t.Run("429 date and backoff", func(t *testing.T) {
		var calls int
		now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
		c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
			calls++
			resp := response(429, "")
			resp.Header.Set("Retry-After", now.Add(10*time.Minute).Format(http.TimeFormat))
			return resp, nil
		})
		c.now = func() time.Time { return now }
		_, err := c.Refresh(context.Background())
		var he *HTTPError
		if !errors.As(err, &he) || he.RetryAt == nil || !he.RetryAt.Equal(now.Add(10*time.Minute)) {
			t.Fatalf("bad retry date: %v", err)
		}
		_, _ = c.Refresh(context.Background())
		if calls != 1 {
			t.Fatal("management retry floor ignored")
		}
	})
	t.Run("body limit", func(t *testing.T) {
		c := testClient(t, testConfig(), func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat(" ", maxBody+1)), nil })
		if _, err := c.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("missing body limit: %v", err)
		}
	})
	t.Run("external redirect", func(t *testing.T) {
		var calls int
		c := testClient(t, testConfig(), func(*http.Request) (*http.Response, error) {
			calls++
			resp := response(302, "")
			resp.Header.Set("Location", "https://untrusted.invalid/steal")
			return resp, nil
		})
		if _, err := c.Refresh(context.Background()); err == nil || calls != 1 {
			t.Fatalf("redirect followed or falsely succeeded: %v", err)
		}
	})
}

func TestBoundedParallelismAndSingleTimeout(t *testing.T) {
	cfg := testConfig()
	var active, maximum atomic.Int32
	var fast atomic.Int32
	var entries []map[string]any
	for i := 0; i < 12; i++ {
		entries = append(entries, map[string]any{"id": fmt.Sprint(i), "auth_index": fmt.Sprint(i), "provider": "codex", "id_token": map[string]any{"chatgpt_account_id": "private-id"}})
	}
	list, _ := json.Marshal(map[string]any{"credentials": entries})
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(200, string(list)), nil
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		b, _ := io.ReadAll(r.Body)
		v, _ := decodeJSON(b)
		if object(v)["auth_index"] == "0" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		fast.Add(1)
		if object(v)["url"] == codexResetCreditsURL {
			return upstreamResponse(`{"available_count":0,"credits":[]}`), nil
		}
		return upstreamResponse(whamAvailable), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	a, err := c.Refresh(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted context deadline, got %v", err)
	}
	// Each healthy credential performs usage and read-only reset-credit queries.
	if len(a) != 12 || fast.Load() != 22 || maximum.Load() > parallelism {
		t.Fatalf("starvation/concurrency failure: count=%d fast=%d max=%d", len(a), fast.Load(), maximum.Load())
	}
	allowed := 0
	for _, a := range a {
		if a.State == "allowed" {
			allowed++
		}
	}
	if allowed != 11 {
		t.Fatalf("one timeout affected other accounts: allowed=%d", allowed)
	}
}

func TestMissingKeyInvalidURLAndCancellation(t *testing.T) {
	t.Setenv("CPA_MANAGEMENT_KEY", "")
	if _, err := New(testConfig()).Refresh(context.Background()); err == nil {
		t.Fatal("missing key accepted")
	}
	cfg := testConfig()
	cfg.ProxyURL = "http://non-local.invalid"
	if _, err := New(cfg).Refresh(context.Background()); err == nil {
		t.Fatal("remote cleartext management accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(testConfig()).Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("context ignored")
	}
}
