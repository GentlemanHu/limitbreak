package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestVerifiedRootWindowCanBeMonitoredWithoutGrantingResume(t *testing.T) {
	s, db, e := controlFixture(t)
	used := 15.0
	if err := db.Update(context.Background(), func(tx *store.Tx) error {
		tx.Accounts[0].Windows = []domain.Window{{ID: "root-window", Name: "short-window", UsedPercent: &used}}
		tx.Accounts[0].CheckedAt = time.Now().UTC()
		tx.Accounts[0].QuotaError = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	binding := domain.Binding{SessionID: "private-session", MonitorMode: "all", MonitorResources: []domain.ResourceRef{{AccountID: "private-account", WindowID: "root-window"}}}
	body, _ := json.Marshal(binding)
	w := request(s, "POST", "/api/bind", string(body), "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 200 {
		t.Fatalf("root-window monitor rejected: %d", w.Code)
	}
	snap, _ := e.Snapshot(context.Background())
	if snap.Sessions[0].Binding.AutoResume || snap.Sessions[0].Monitor == nil || snap.Sessions[0].Monitor.State != "allowed" {
		t.Fatal("root-window observation incorrectly granted resume or was not observed")
	}
}
