package adapters

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// SourceInstance binds one explicit identity to one quota interface. It is also
// usable with in-memory fixtures; aggregation never creates executors/observers.
type SourceInstance struct {
	ID    string
	Type  string
	Quota QuotaSource
	// Zero keeps direct fixture/legacy interface callers eager; configured sources set a bounded interval.
	PollInterval time.Duration
}

type Aggregator struct {
	mu        sync.Mutex
	instances []SourceInstance
	accounts  map[string][]domain.Account
	health    map[string]domain.SourceHealth
	nextDue   map[string]time.Time
	observed  map[string][]string
	caps      domain.Capabilities
	now       func() time.Time
}

func NewAggregator(instances []SourceInstance, now func() time.Time) (*Aggregator, error) {
	if len(instances) == 0 || len(instances) > 16 {
		return nil, errors.New("aggregator requires 1–16 sources")
	}
	if now == nil {
		now = time.Now
	}
	a := &Aggregator{instances: append([]SourceInstance(nil), instances...), accounts: map[string][]domain.Account{}, health: map[string]domain.SourceHealth{}, nextDue: map[string]time.Time{}, observed: map[string][]string{}, now: now}
	for _, source := range instances {
		if source.ID == "" || source.Quota == nil {
			return nil, errors.New("source identity and quota interface are required")
		}
		if _, ok := a.health[source.ID]; ok {
			return nil, errors.New("duplicate source identity")
		}
		a.health[source.ID] = domain.SourceHealth{ID: source.ID, Type: source.Type, State: "unknown"}
	}
	return a, nil
}

func NewConfigured(ctx context.Context, c config.Config, db *store.Store) (*Aggregator, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	r := NewRegistry()
	var instances []SourceInstance
	for _, source := range c.EffectiveSources() {
		quota, err := r.Quota(ctx, c, source, db)
		if err != nil {
			return nil, err
		}
		seconds := source.PollSeconds
		if seconds == 0 {
			seconds = 180
			if source.Type == "cliproxyapi" {
				seconds = c.PollSeconds
			}
		}
		instances = append(instances, SourceInstance{ID: source.ID, Type: source.Type, Quota: quota, PollInterval: time.Duration(seconds) * time.Second})
	}
	a, err := NewAggregator(instances, nil)
	if err != nil {
		return nil, err
	}
	if db != nil {
		previous, err := db.Read(ctx)
		if err != nil {
			return nil, err
		}
		// Seed only exact source/account identities. Failed first refresh downgrades
		// these display-only observations, without advancing their timestamps.
		for _, account := range previous.Accounts {
			id := account.SourceID
			if id == "" {
				id = domain.DefaultSourceID
			}
			if _, exists := a.health[id]; exists {
				a.accounts[id] = append(a.accounts[id], account)
				a.observed[id] = append(a.observed[id], account.ID)
			}
		}
	}
	return a, nil
}

// Refresh isolates list-level failures to their source. Partial failure is data,
// not a global error that would discard healthy sources' independent evidence.
func (a *Aggregator) Refresh(ctx context.Context) ([]domain.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.caps.ProxyConnected = false
	previous := map[string][]domain.Account{}
	previousHealth := map[string]domain.SourceHealth{}
	for _, source := range a.instances {
		previous[source.ID] = cloneAccounts(a.accounts[source.ID])
		previousHealth[source.ID] = a.health[source.ID]
	}
	for _, source := range a.instances {
		attempt := a.now().UTC()
		if source.PollInterval > 0 && attempt.Before(a.nextDue[source.ID]) {
			continue
		}
		accounts, err := source.Quota.Refresh(ctx)
		caps := source.Quota.Capabilities()
		if source.ID == domain.DefaultSourceID {
			a.caps.ProxyVersion = caps.ProxyVersion
			a.caps.ManagementPrefix = caps.ManagementPrefix
		}
		if err == nil {
			err = validateAccounts(accounts)
		}
		if err == nil && !caps.ProxyConnected {
			err = errors.New("source did not confirm healthy evidence")
		}
		health := a.health[source.ID]
		health.CheckedAt = a.now().UTC()
		a.nextDue[source.ID] = attempt.Add(source.PollInterval)
		if err != nil {
			health.Connected = false
			health.State = "error"
			// Never relay third-party error strings, response bodies, paths or keys.
			health.Error = "quota source refresh failed; last-known data is not sending evidence"
			markUnknown(a.accounts[source.ID], source.ID, health.Error)
		} else {
			health.Connected = true
			health.State = "healthy"
			health.Error = ""
			success := health.CheckedAt
			health.LastSuccessAt = &success
			// Copy nested data so a source's cached slices can never be mutated by a
			// later failing refresh or by the caller of this interface.
			accounts = cloneAccounts(accounts)
			a.observed[source.ID] = []string{}
			for i := range accounts {
				accounts[i].ID = domain.AccountID(source.ID, accounts[i].ID)
				accounts[i].SourceID = source.ID
				a.observed[source.ID] = append(a.observed[source.ID], accounts[i].ID)
				// Preserve CPA's earlier reset/freshness wake-ups; readonly HTTP
				// obeys its explicit per-source request interval without inventing evidence.
				if source.Type == "cliproxyapi" && !accounts[i].NextCheckAt.IsZero() && accounts[i].NextCheckAt.Before(a.nextDue[source.ID]) {
					due := accounts[i].NextCheckAt
					floor := attempt.Add(30 * time.Second)
					if due.Before(floor) {
						due = floor
					}
					a.nextDue[source.ID] = due
				}
			}
			a.accounts[source.ID] = accounts
		}
		health.AccountCount = len(a.accounts[source.ID])
		a.health[source.ID] = health
	}
	ambiguous := a.quarantineCollisions(previous, previousHealth)
	a.caps.Sources = []domain.SourceHealth{}
	var result []domain.Account
	for _, source := range a.instances {
		health := a.health[source.ID]
		health.AccountCount = 0
		for _, account := range a.accounts[source.ID] {
			if ambiguous[account.ID] {
				continue
			} // Never expose duplicate/ambiguous primary IDs.
			result = append(result, account)
			health.AccountCount++
		}
		a.health[source.ID] = health
		a.caps.Sources = append(a.caps.Sources, health)
		a.caps.ProxyConnected = a.caps.ProxyConnected || health.Connected
	}
	a.caps.Message = "read-only per-source quota health; account identity never inferred"
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if result == nil {
		result = []domain.Account{}
	}
	return cloneAccounts(result), nil
}

func validateAccounts(accounts []domain.Account) error {
	if len(accounts) > 4096 {
		return errors.New("too many source accounts")
	}
	seen := map[string]bool{}
	for _, account := range accounts {
		if account.ID == "" || len(account.ID) > 400 || seen[account.ID] {
			return errors.New("invalid or duplicate raw account ID")
		}
		seen[account.ID] = true
	}
	return nil
}

func (a *Aggregator) Capabilities() domain.Capabilities {
	a.mu.Lock()
	defer a.mu.Unlock()
	caps := a.caps
	caps.Sources = append([]domain.SourceHealth{}, caps.Sources...)
	if len(caps.Sources) == 0 {
		for _, source := range a.instances {
			caps.Sources = append(caps.Sources, a.health[source.ID])
		}
	}
	for i := range caps.Sources {
		caps.Sources[i].LastSuccessAt = cloneTime(caps.Sources[i].LastSuccessAt)
	}
	return caps
}

func (a *Aggregator) SetPolicy(ctx context.Context, p domain.Policy) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, source := range a.instances {
		if aware, ok := source.Quota.(interface {
			SetPolicy(context.Context, domain.Policy) error
		}); ok {
			if err := aware.SetPolicy(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Aggregator) InvalidateQuotaCache(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, source := range a.instances {
		a.nextDue[source.ID] = time.Time{}
	}
	for _, source := range a.instances {
		if aware, ok := source.Quota.(interface{ InvalidateQuotaCache(context.Context) error }); ok {
			if err := aware.InvalidateQuotaCache(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// InvalidateProvider expires the probe cadence of one provider's accounts in
// every source that supports it and makes those sources due. It returns the
// number of matching accounts.
func (a *Aggregator) InvalidateProvider(ctx context.Context, provider string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := 0
	for _, source := range a.instances {
		aware, ok := source.Quota.(interface {
			InvalidateProvider(context.Context, string) (int, error)
		})
		if !ok {
			continue
		}
		n, err := aware.InvalidateProvider(ctx, provider)
		if err != nil {
			return total, err
		}
		if n > 0 {
			a.nextDue[source.ID] = time.Time{}
		}
		total += n
	}
	return total, nil
}
