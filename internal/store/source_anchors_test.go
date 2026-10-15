package store

import (
	"context"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestSourceAnchorNamespacesPreserveLegacyAndDoNotOverwritePeers(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := map[string]map[string]time.Time{"same": {"window": at}}
	if err := db.SaveQuotaAnchors(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	defaultNS := db.QuotaAnchorNamespace(domain.DefaultSourceID)
	got, err := defaultNS.LoadQuotaAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got["same"]["window"].Equal(at) {
		t.Fatal("legacy anchor migration lost existing relative reset")
	}
	one, two := db.QuotaAnchorNamespace("one"), db.QuotaAnchorNamespace("two")
	if err := one.SaveQuotaAnchors(ctx, map[string]map[string]time.Time{"same": {"window": at.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := two.SaveQuotaAnchors(ctx, map[string]map[string]time.Time{"same": {"window": at.Add(2 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := one.SaveQuotaAnchors(ctx, map[string]map[string]time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err = two.LoadQuotaAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got["same"]["window"].Equal(at.Add(2 * time.Hour)) {
		t.Fatal("one source pruning erased peer anchors")
	}
	got, err = defaultNS.LoadQuotaAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got["same"]["window"].Equal(at) {
		t.Fatal("nondefault source overwrote legacy anchors")
	}
}
