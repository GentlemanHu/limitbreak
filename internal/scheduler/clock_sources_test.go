package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

type manualClock struct{ at time.Time }

func (c *manualClock) Now() time.Time { return c.at }

type healthQuota struct {
	accounts []domain.Account
	caps     domain.Capabilities
}

func (q *healthQuota) Refresh(context.Context) ([]domain.Account, error) { return q.accounts, nil }
func (q *healthQuota) Capabilities() domain.Capabilities                 { return q.caps }

func TestInjectedClockDrivesRealLeaseExpiryAndSnapshot(t *testing.T) {
	e, q, b := fixture(t)
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.clock = clock
	ctx := context.Background()
	if err := e.Bind(ctx, binding("s")); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleEvent(ctx, domain.HookEvent{EventID: "failure", SessionID: "s", HookEventName: "StopFailure", Error: "rate_limit", ObservedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Second)
	a := allowed("a")
	a.CheckedAt = clock.Now()
	q.accounts = []domain.Account{a}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
		t.Fatal("clock-controlled real dispatch did not verify")
	}
	snapshot, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Time.Equal(clock.Now()) || !snapshot.Jobs[0].UpdatedAt.Equal(clock.Now()) || snapshot.Jobs[0].LeaseUntil == nil || !snapshot.Jobs[0].LeaseUntil.Equal(clock.Now().Add(5*time.Second)) {
		t.Fatal("scheduler lease/snapshot bypassed injected clock")
	}
	clock.at = clock.at.Add(6 * time.Second)
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if b.count() != 1 || state(t, e, "s").State != domain.Attention {
		t.Fatal("lease expiry did not use injected clock or resent")
	}
}

func TestHealthyPeerCannotAuthorizeFailedSourceAtDispatch(t *testing.T) {
	e, _, bridge := fixture(t)
	clock := &manualClock{at: time.Now().UTC()}
	e.clock = clock
	ctx := context.Background()
	b := binding("s")
	b.AccountIDs = []string{"source-a::a"}
	if err := e.Bind(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleEvent(ctx, domain.HookEvent{EventID: "failure", SessionID: "s", HookEventName: "StopFailure", Error: "rate_limit", ObservedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Second)
	a := allowed("source-a::a")
	a.CheckedAt = clock.Now()
	a.SourceID = "source-a"
	q := &healthQuota{accounts: []domain.Account{a}, caps: domain.Capabilities{ProxyConnected: true, Sources: []domain.SourceHealth{
		{ID: "source-a", State: "error", Connected: false}, {ID: "source-b", State: "healthy", Connected: true},
	}}}
	e.quota = q
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 0 || state(t, e, "s").State != domain.Cooldown {
		t.Fatal("global proxy health authorized a failed source")
	}
	// Missing source is not inferred from provider, model, email or another source.
	q.caps.Sources = q.caps.Sources[1:]
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 0 {
		t.Fatal("removed source account automatically rebound")
	}
}

func TestLegacySourceListErrorKeepsTimestampAndUnknownDisplay(t *testing.T) {
	e, q, b := fixture(t)
	ctx := context.Background()
	queue(t, e, "s")
	a := allowed("a")
	a.Groups = []domain.QuotaGroup{{ID: "group", Name: "Shared", Scope: "account", State: "allowed", CheckedAt: a.CheckedAt, Windows: a.Windows}}
	refresh(t, e, q, a)
	q.err = errors.New("source list failed")
	if e.Refresh(ctx) == nil {
		t.Fatal("source list error not surfaced")
	}
	snapshot, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].QuotaError == "" || snapshot.Accounts[0].State != "unknown" || !snapshot.Accounts[0].CheckedAt.Equal(a.CheckedAt) || snapshot.Accounts[0].Groups[0].State != "unknown" || !snapshot.Accounts[0].Groups[0].CheckedAt.Equal(a.CheckedAt) {
		t.Fatal("failure refreshed or hid retained evidence")
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if b.count() != 0 {
		t.Fatal("legacy failure authorized dispatch")
	}
}

func TestWholeAccountChecksEveryGroupAfterFailure(t *testing.T) {
	at := time.Now().UTC()
	a := allowed("a")
	a.CheckedAt = at.Add(time.Second)
	a.Groups = []domain.QuotaGroup{{ID: "old-group", Scope: "models", Models: []string{"opus"}, State: "allowed", CheckedAt: at, Windows: a.Windows}}
	if evidenceAfterFailure(a, binding("s"), at) {
		t.Fatal("whole-account accepted pre/equal-failure group evidence")
	}
}
