package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestFailedProbeKeepsGroupsWithoutRefreshingEvidence(t *testing.T) {
	failed := false
	now := time.Now().UTC()
	c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(200, credentialFixture), nil
		}
		if failed {
			return response(502, `{}`), nil
		}
		return upstreamResponse(whamAvailable), nil
	})
	c.now = func() time.Time { return now }
	first, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first[0].Groups) == 0 {
		t.Fatal("missing initial groups")
	}
	failed = true
	// Past the manual-refresh floor, so the refresh really queries again.
	now = now.Add(manualRefreshFloor + time.Second)
	if err := c.InvalidateQuotaCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(next[0].Groups) != len(first[0].Groups) || next[0].Groups[0].ID != first[0].Groups[0].ID || !next[0].CheckedAt.Equal(first[0].CheckedAt) {
		t.Fatal("last-known groups lost or relabelled as fresh")
	}
	if next[0].QuotaError == "" || next[0].State == "allowed" || next[0].Groups[0].State == "allowed" {
		t.Fatal("failed refresh authorized last-known evidence")
	}
}
