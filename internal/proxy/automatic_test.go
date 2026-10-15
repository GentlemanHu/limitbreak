package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestUnifiedQuotaUsesV0WhileCredentialsUseV8(t *testing.T) {
	c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			return response(200, `{"files":[{"id":"claude-account","provider":"claude","auth_index":"index","disabled":true}]}`), nil
		case "/v0/management/quota/fetch":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if r.Method != "POST" || payload["auth_index"] != "index" || payload["provider"] != "claude" || payload["plugin_id"] != "" {
				t.Fatal("invented plugin or invalid fetch contract")
			}
			return response(200, `{"subscription":{"plan":"Pro"},"summary":[{"key":"credits","label":"余额","value":12.5,"format":"currency","currency":"USD"}],"groups":[{"displayName":"Gemini","buckets":[{"window":"5h","remainingFraction":0.9}]},{"displayName":"Claude/GPT","buckets":[{"window":"weekly","remainingFraction":0}]}]}`), nil
		case "/v8/management/credentials/models":
			if r.URL.Query().Get("name") != "claude-account" {
				t.Fatal("wrong models query key")
			}
			return response(200, `{"models":[{"id":"model-id"}]}`), nil
		default:
			t.Fatalf("unexpected or mutating API %s", r.URL.Path)
			return nil, nil
		}
	})
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].State != "disabled" || len(a[0].Groups) != 2 || len(a[0].Metrics) != 1 || a[0].Groups[0].State != "allowed" || a[0].Groups[1].State != "limited" || len(a[0].Models) != 1 {
		t.Fatalf("lost independent groups: %+v", a)
	}
	if a[0].Groups[0].ID == "" || a[0].Groups[0].Windows[0].ID == "" {
		t.Fatal("missing stable resource IDs")
	}
}

func TestBuiltinClaudeFallbackIsReadOnly(t *testing.T) {
	calls := 0
	c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v8/management/credentials" {
			return response(200, `{"files":[{"id":"c","provider":"claude","auth_index":"i","models":["opus"]}]}`), nil
		}
		if r.URL.Path == "/v0/management/quota/fetch" {
			return response(501, `{}`), nil
		}
		if r.URL.Path != "/v8/management/requests/api-call" {
			t.Fatalf("wrong API %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		v, _ := decodeJSON(b)
		payload := object(v)
		if payload["method"] != "GET" || object(payload["header"])["Authorization"] != "Bearer $TOKEN$" {
			t.Fatal("unsafe OAuth request")
		}
		calls++
		if strings.HasSuffix(text(payload["url"]), "/usage") {
			return upstreamResponse(`{"five_hour":{"utilization":4,"resets_at":"2026-10-13T00:00:00Z"},"seven_day":{"utilization":70},"seven_day_opus":{"utilization":100},"extra_usage":{"is_enabled":false,"used_credits":0}}`), nil
		}
		return upstreamResponse(`{"account":{"has_claude_pro":true}}`), nil
	})
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || a[0].Plan != "Pro" || len(a[0].Groups) != 2 || a[0].Groups[0].State != "allowed" || len(a[0].Metrics) != 2 {
		t.Fatalf("Claude quota incomplete: %+v", a)
	}
}

func TestXAIWeeklyAndMoneyUseDifferentUnits(t *testing.T) {
	q, err := parseXAI([]byte(`{"config":{"creditUsagePercent":99,"currentPeriod":{"type":"weekly","end":"2026-10-13T12:00:00Z"},"productUsage":[{"product":"GrokBuild","usagePercent":73},{"product":"GrokChat","usagePercent":7}],"monthlyLimit":{"val":1000},"used":{"val":200},"onDemandCap":{"val":0},"prepaidBalance":{"val":50}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(q.groups) != 1 || len(q.metrics) != 6 || *q.groups[0].Windows[0].UsedPercent != 99 {
		t.Fatalf("billing shape lost: %+v", q)
	}
	for _, m := range q.metrics {
		if m.Key == "billing:monthlyLimit" && m.Value != "10.00" {
			t.Fatal("cents were displayed as dollars")
		}
	}
}

func TestStableGroupIDsAndManualRefresh(t *testing.T) {
	q, err := parsePlugin([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"five","window":"5h","remainingFraction":0.5}]}]}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	one := domain.Account{ID: "account"}
	materializeGroups(&one, q, time.Now())
	q.groups[0].Windows[0].Name = "changed display label"
	two := domain.Account{ID: "account"}
	materializeGroups(&two, q, time.Now())
	if one.Groups[0].ID != two.Groups[0].ID || one.Groups[0].Windows[0].ID != two.Groups[0].Windows[0].ID {
		t.Fatal("stable upstream bucket ID lost")
	}
	c := New(testConfig())
	now := time.Now()
	floor := now.Add(time.Hour)
	c.cache["regular"] = probeCache{next: floor}
	c.cache["retry"] = probeCache{next: floor, quota: evidence{retryAt: floor}}
	if err := c.InvalidateQuotaCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.cache["regular"].next.IsZero() || !c.cache["retry"].next.Equal(floor) {
		t.Fatal("manual refresh ignored cache or bypassed Retry-After")
	}
	q.groups[0].State = "allowed"
	q.groups[0].CheckedAt = now
	q.blocked = true
	q.allowed = false
	if next := c.nextProbe(q, now, 0); next.After(now.Add(240 * time.Second)) {
		t.Fatal("healthy group inherited another group's long cooldown")
	}
}
