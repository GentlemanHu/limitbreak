package scheduler

import (
	"context"
	"errors"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
	"time"
)

func applyCapabilities(t *store.Tx, caps domain.Capabilities) {
	// Desktop evidence changes only after an actual probe, independently of CPA.
	caps.BridgeChecked = t.Capabilities.BridgeChecked
	caps.BridgeAvailable = t.Capabilities.BridgeAvailable
	caps.AccessibilityGranted = t.Capabilities.AccessibilityGranted
	caps.ScreenLocked = !caps.BridgeChecked || t.Capabilities.ScreenLocked
	caps.DesktopCheckedAt = t.Capabilities.DesktopCheckedAt
	caps.DesktopCode = t.Capabilities.DesktopCode
	caps.AccessibilityState = t.Capabilities.AccessibilityState
	t.Capabilities = caps
}

func applyDesktopResult(t *store.Tx, result domain.BridgeResult, at time.Time) {
	t.Capabilities.BridgeChecked = true
	t.Capabilities.BridgeAvailable = result.OK
	t.Capabilities.AccessibilityGranted = result.AccessibilityGranted
	t.Capabilities.ScreenLocked = result.ScreenLocked
	t.Capabilities.DesktopCheckedAt = &at
	t.Capabilities.DesktopCode = result.Code
	t.Capabilities.AccessibilityState = "unknown"
	if result.AccessibilityGranted {
		t.Capabilities.AccessibilityState = "granted"
	} else if result.Code == "accessibility_denied" {
		t.Capabilities.AccessibilityState = "denied"
	}
	t.Capabilities.Message = "desktop: " + result.Code
}

func (e *Engine) ValidateDesktopTarget(ctx context.Context, binding domain.Binding) (domain.BridgeResult, error) {
	if err := domain.ValidateResumeFields(binding); err != nil {
		return domain.BridgeResult{Code: "invalid_binding", ScreenLocked: true}, err
	}
	executor, ok := e.bridge.(interface {
		ValidateTarget(context.Context, domain.Binding) domain.BridgeResult
	})
	if !ok {
		return domain.BridgeResult{Code: "target_preflight_unavailable", ScreenLocked: true}, nil
	}
	return executor.ValidateTarget(ctx, binding), ctx.Err()
}

func (e *Engine) ProbeDesktop(ctx context.Context) error {
	if e.bridge == nil {
		return errors.New("desktop bridge is not configured")
	}
	result := e.bridge.Probe(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return e.store.Update(ctx, func(t *store.Tx) error {
		applyDesktopResult(t, result, e.now())
		return nil
	})
}
