package adapters

import (
	"context"
	"errors"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

type quotaFixture struct {
	accounts []domain.Account
	err      error
}

func (q *quotaFixture) Refresh(context.Context) ([]domain.Account, error) { return q.accounts, q.err }
func (q *quotaFixture) Capabilities() domain.Capabilities {
	return domain.Capabilities{ProxyConnected: q.err == nil}
}
func fixtureAccount(id string, now time.Time) domain.Account {
	percent := 10.0
	windows := []domain.Window{{ID: "short", Name: "short", UsedPercent: &percent}}
	return domain.Account{ID: id, AuthIndex: "index", Provider: "anthropic", Models: []string{"opus"}, State: "allowed", CheckedAt: now,
		Windows: windows, Groups: []domain.QuotaGroup{{ID: "group", Name: "Shared", Scope: "account", State: "allowed", CheckedAt: now, Windows: windows}}}
}

func TestAccountNamespaceAndFailedSourceIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	a := &quotaFixture{accounts: []domain.Account{fixtureAccount("same", now)}}
	b := &quotaFixture{accounts: []domain.Account{fixtureAccount("same", now)}}
	q, err := NewAggregator([]SourceInstance{{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a}, {ID: "second", Type: "readonly-http", Quota: b}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID == accounts[1].ID {
		t.Fatal("namespace collision")
	}
	if domain.AccountID(domain.DefaultSourceID, "same") != "same" || domain.AccountID("second", "same") != "second::same" {
		t.Fatal("identity contract changed")
	}
	if a.accounts[0].SourceID != "" || b.accounts[0].ID != "same" {
		t.Fatal("aggregation mutated source cache")
	}
	observed := now
	now = now.Add(time.Minute)
	a.err = errors.New("secret exception must not be echoed")
	b.accounts = []domain.Account{fixtureAccount("same", now)}
	accounts, err = q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	caps := q.Capabilities()
	if !caps.ProxyConnected || len(caps.Sources) != 2 {
		t.Fatal("independent healthy source lost")
	}
	for _, account := range accounts {
		if account.SourceID == domain.DefaultSourceID {
			if account.State != "unknown" || account.QuotaError == "" || !account.CheckedAt.Equal(observed) || account.Groups[0].State != "unknown" || !account.Groups[0].CheckedAt.Equal(observed) {
				t.Fatalf("failed evidence not downgraded: %+v", account)
			}
			if domain.SourceConnected(account, caps) {
				t.Fatal("healthy B authorized failed A")
			}
		} else if account.ID != "second::same" || account.State != "allowed" || !domain.SourceConnected(account, caps) {
			t.Fatal("healthy source not isolated")
		}
	}
	if caps.Sources[0].Error == a.err.Error() || caps.Sources[0].Connected || caps.Sources[0].LastSuccessAt == nil || !caps.Sources[0].LastSuccessAt.Equal(observed) {
		t.Fatal("failure leaked error detail or refreshed success time")
	}
	// Returned slices cannot mutate cache, including shared window pointers.
	accounts[0].Groups[0].State = "injected"
	again, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range again {
		if account.SourceID == domain.DefaultSourceID && account.Groups[0].State != "unknown" {
			t.Fatal("returned data aliases aggregator state")
		}
	}
}

func TestInvalidSourceAccountsRemainSourceLocal(t *testing.T) {
	at := time.Now().UTC()
	a := &quotaFixture{accounts: []domain.Account{fixtureAccount("a", at)}}
	b := &quotaFixture{accounts: []domain.Account{fixtureAccount("b", at)}}
	q, err := NewAggregator([]SourceInstance{{ID: "a", Type: "cliproxyapi", Quota: a}, {ID: "b", Type: "cliproxyapi", Quota: b}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.accounts = []domain.Account{fixtureAccount("duplicated", at), fixtureAccount("duplicated", at)}
	accounts, err := q.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatal("invalid source erased its last-known display or healthy source")
	}
	for _, account := range accounts {
		if account.SourceID == "a" && (account.ID != "a::a" || account.QuotaError == "") {
			t.Fatal("invalid source reused unvalidated new IDs")
		}
	}
}

func TestTypedBuiltinRegistriesAndHonestDeclarations(t *testing.T) {
	r := NewRegistry()
	observed := false
	observer, err := r.Observer("codehooks", func(_ context.Context, e domain.HookEvent) error { observed = e.Kind == "code"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Observe(context.Background(), domain.HookEvent{}); err != nil || !observed {
		t.Fatal("registered Code observer unavailable")
	}
	if _, err := r.Executor("ax", "/explicit/bridge"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Executor("codehooks", "/explicit/bridge"); err == nil {
		t.Fatal("observer factory was treated as executor")
	}
	if _, err := r.Observer("ax", nil); err == nil {
		t.Fatal("executor factory was treated as observer")
	}
	for _, d := range Descriptors() {
		if d.ID == "codex-standalone" || d.ID == "claude-standalone" || d.ID == "ccswitch" {
			if d.Implemented || len(d.Capabilities) > 0 {
				t.Fatalf("unimplemented integration advertised: %+v", d)
			}
		} else if !d.Implemented {
			t.Fatalf("builtin not registered: %+v", d)
		}
	}
}
