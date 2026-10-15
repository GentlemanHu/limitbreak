package scheduler

import (
	"context"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func (e *Engine) rejectQuota(ctx context.Context, caps domain.Capabilities) error {
	return e.store.Update(ctx, func(t *store.Tx) error {
		if len(caps.Sources) == 0 {
			caps.Sources = append([]domain.SourceHealth{}, t.Capabilities.Sources...)
		}
		if len(caps.Sources) == 0 {
			caps.Sources = []domain.SourceHealth{{ID: domain.DefaultSourceID, Type: "cliproxyapi"}}
		}
		for i := range caps.Sources {
			health := &caps.Sources[i]
			health.Connected = false
			health.State = "error"
			health.CheckedAt = e.now()
			health.Error = "quota source refresh failed; last-known data is not sending evidence"
		}
		for i := range t.Accounts {
			a := &t.Accounts[i]
			if a.SourceID == "" {
				a.SourceID = domain.DefaultSourceID
			}
			a.QuotaError = "quota source refresh failed; last-known data is not sending evidence"
			a.State = "unknown"
			if a.Disabled {
				a.State = "disabled"
			}
			for n := range a.Groups {
				a.Groups[n].State = "unknown"
			}
		}
		applyCapabilities(t, caps)
		t.Capabilities.ProxyConnected = false
		return nil
	})
}
