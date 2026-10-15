package signals

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

type feed struct {
	mu         sync.Mutex
	id         string
	at         time.Time
	status     int
	retryAfter string
	etag       string
	sawETag    string
}

func (f *feed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/api/v1/status" {
		http.NotFound(w, r)
		return
	}
	f.sawETag = r.Header.Get("If-None-Match")
	if f.status != 0 {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(f.status)
		return
	}
	if f.etag != "" && f.sawETag == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if f.etag != "" {
		w.Header().Set("ETag", f.etag)
	}
	fmt.Fprintf(w, `{"data":{"latest_reset":{"id":%q,"reset_type":"rate_limit","announced_at":%q,"text":"reset","source":{"url":"https://example.com/post"}},
		"scheduled_reset":null,"active_watch":{"level":"elevated","reset_chance_percent":40,"forecast_window":"24h","text":"maybe"}}}`, f.id, f.at.Format(time.RFC3339))
}

func (f *feed) set(id string, at time.Time) {
	f.mu.Lock()
	f.id, f.at = id, at
	f.mu.Unlock()
}

func newSource(t *testing.T, f *feed) (Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := NewCodexResets(config.SignalSource{ID: "codex-resets", Type: "codex-resets", URL: srv.URL, Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	return s, srv
}

func TestCodexResetsBaselineThenNewReset(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	f := &feed{id: "r1", at: now.Add(-6 * time.Hour), etag: `"v1"`}
	s, _ := newSource(t, f)
	// First poll only baselines an old reset.
	res, err := s.Poll(context.Background(), domain.SignalSourceState{}, now)
	if err != nil || len(res.Signals) != 0 || res.State.LastID != "r1" || res.State.ETag != `"v1"` || res.State.Watch == nil {
		t.Fatalf("baseline: %+v %v", res, err)
	}
	// Unchanged content answers 304 and keeps the state.
	again, err := s.Poll(context.Background(), res.State, now.Add(time.Minute))
	if err != nil || len(again.Signals) != 0 || again.State.LastID != "r1" || f.sawETag != `"v1"` {
		t.Fatalf("not modified: %+v %v", again, err)
	}
	// A new reset is emitted once.
	f.set("r2", now.Add(2*time.Minute))
	f.etag = `"v2"`
	next, err := s.Poll(context.Background(), again.State, now.Add(3*time.Minute))
	if err != nil || len(next.Signals) != 1 || next.Signals[0].ID != "r2" || next.Signals[0].Kind != domain.SignalResetExecuted || next.Signals[0].Provider != "codex" {
		t.Fatalf("new reset: %+v %v", next, err)
	}
	f.etag = ""
	dup, err := s.Poll(context.Background(), next.State, now.Add(4*time.Minute))
	if err != nil || len(dup.Signals) != 0 {
		t.Fatalf("duplicate emitted: %+v %v", dup, err)
	}
	// An older reset replacing the latest one is not a new reset.
	f.set("r0", now.Add(-time.Hour))
	old, err := s.Poll(context.Background(), dup.State, now.Add(5*time.Minute))
	if err != nil || len(old.Signals) != 0 {
		t.Fatalf("older reset emitted: %+v %v", old, err)
	}
}

func TestCodexResetsFirstPollReactsToRecentReset(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	s, _ := newSource(t, &feed{id: "fresh", at: now.Add(-10 * time.Minute)})
	res, err := s.Poll(context.Background(), domain.SignalSourceState{}, now)
	if err != nil || len(res.Signals) != 1 {
		t.Fatalf("recent reset ignored: %+v %v", res, err)
	}
}

func TestCodexResetsRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	s, _ := newSource(t, &feed{status: http.StatusTooManyRequests, retryAfter: "120"})
	_, err := s.Poll(context.Background(), domain.SignalSourceState{}, now)
	var retry *RetryError
	if !errors.As(err, &retry) || !retry.At.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("retry-after not honored: %v", err)
	}
}

func TestSafeIDDigestsOddUpstreamIDs(t *testing.T) {
	if safeID("abc-123") != "abc-123" {
		t.Fatal("plain id changed")
	}
	if got := safeID("weird id/with spaces"); len(got) != len("sha256-")+32 {
		t.Fatalf("odd id not digested: %q", got)
	}
}

type fakeSource struct {
	desc   domain.SignalSourceState
	result Result
	polls  int
}

func (f *fakeSource) Descriptor() domain.SignalSourceState { return f.desc }
func (f *fakeSource) Poll(context.Context, domain.SignalSourceState, time.Time) (Result, error) {
	f.polls++
	return f.result, nil
}

type fakeSink struct {
	states   map[string]domain.SignalSourceState
	applied  []domain.ResetSignal
	followUp []bool
	provider bool
}

func (s *fakeSink) SignalStates(context.Context) (map[string]domain.SignalSourceState, error) {
	return s.states, nil
}
func (s *fakeSink) SaveSignalState(_ context.Context, st domain.SignalSourceState) error {
	s.states[st.ID] = st
	return nil
}
func (s *fakeSink) ApplyResetSignal(_ context.Context, sig domain.ResetSignal, followUp bool) (int, error) {
	s.applied = append(s.applied, sig)
	s.followUp = append(s.followUp, followUp)
	return 1, nil
}
func (s *fakeSink) HasProvider(context.Context, string) (bool, error) { return s.provider, nil }

func runner(sink *fakeSink, src *fakeSource, mode string, clock *time.Time) *Runner {
	return &Runner{sink: sink, now: func() time.Time { return *clock }, fired: map[string]bool{}, wake: make(chan struct{}, 1),
		sources: []*configured{{source: src, mode: mode, interval: time.Minute}}}
}

func TestRunnerAppliesSignalAndFollowUps(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	sig := domain.ResetSignal{ID: "r9", Kind: domain.SignalResetExecuted, At: now}
	src := &fakeSource{desc: domain.SignalSourceState{ID: "cr", Provider: "codex"}, result: Result{Signals: []domain.ResetSignal{sig}}}
	sink := &fakeSink{states: map[string]domain.SignalSourceState{}, provider: true}
	r := runner(sink, src, config.SignalModeAuto, &now)
	r.tick(context.Background())
	if len(sink.applied) != 1 || sink.followUp[0] || sink.applied[0].Source != "cr" || sink.applied[0].Provider != "codex" {
		t.Fatalf("signal not applied: %+v", sink.applied)
	}
	if st := sink.states["cr"]; st.State != domain.SignalStateOK || st.LastTriggeredAt == nil {
		t.Fatalf("state not saved: %+v", st)
	}
	src.result = Result{}
	for _, d := range FollowUps {
		now = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC).Add(d)
		r.tick(context.Background())
	}
	if len(sink.applied) != 1+len(FollowUps) || !sink.followUp[1] || !sink.followUp[len(FollowUps)] {
		t.Fatalf("follow-ups not applied: %+v %+v", sink.applied, sink.followUp)
	}
}

func TestRunnerIdleWithoutProviderAndOff(t *testing.T) {
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	src := &fakeSource{desc: domain.SignalSourceState{ID: "cr", Provider: "codex"}}
	sink := &fakeSink{states: map[string]domain.SignalSourceState{}}
	runner(sink, src, config.SignalModeAuto, &now).tick(context.Background())
	if src.polls != 0 || sink.states["cr"].State != domain.SignalStateIdle {
		t.Fatalf("auto mode polled without accounts: %d %+v", src.polls, sink.states["cr"])
	}
	sink.provider = true
	runner(sink, src, config.SignalModeOff, &now).tick(context.Background())
	if src.polls != 0 || sink.states["cr"].Enabled {
		t.Fatalf("off mode polled: %d", src.polls)
	}
}
