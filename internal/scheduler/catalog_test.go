package scheduler

import (
	"context"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

const otherHostSession = "local_9f8e7d6c-1111-4222-8333-444455556666"

func catalogEntry(cli, host, title string, at time.Time) domain.CatalogEntry {
	return domain.CatalogEntry{HostSessionID: host, CLISessionID: cli, Cwd: cliCwd, Title: title, Model: "claude-opus-4-8[1m]", LastActivityAt: at}
}

func syncCatalog(t *testing.T, e *Engine, complete bool, entries ...domain.CatalogEntry) {
	t.Helper()
	if err := e.SyncDesktopCatalog(context.Background(), entries, complete); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogCreatesHostableDesktopSessions(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	at := clock.at.Add(-time.Hour)
	syncCatalog(t, e, true, catalogEntry(cliA, testHostSession, "你是什么", at))
	s := state(t, e, cliA)
	if !s.InCatalog || s.Archived || s.Title != "你是什么" || s.Model != "claude-opus-4-8[1m]" || s.Cwd != cliCwd || s.Kind != "code" || s.State != domain.Completed {
		t.Fatalf("catalog session not created: %+v", s)
	}
	if s.Origin == nil || s.Origin.App != domain.AppClaudeDesktop || s.Origin.HostSessionID != testHostSession || s.Origin.BundleID != domain.DesktopBundleID {
		t.Fatalf("catalog session has no desktop link: %+v", s.Origin)
	}
	if s.LastActivityAt == nil || !s.LastActivityAt.Equal(at) {
		t.Fatalf("last activity not kept: %v", s.LastActivityAt)
	}
	// A test send needs only the catalog: it can reach the session before any hook.
	if _, err := e.TestSend(context.Background(), cliA, ""); err != nil {
		t.Fatalf("catalog session cannot be test-sent: %v", err)
	}
}

func TestCatalogMergeKeepsHookStateAndUpdatesMetadata(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "StopFailure", clock.at)
	syncCatalog(t, e, true, catalogEntry(cliA, testHostSession, "新标题", clock.at))
	s := state(t, e, cliA)
	if s.State != domain.Cooldown || s.Title != "新标题" || s.Model != "claude-opus-4-8[1m]" || s.Origin.Entrypoint != "claude-desktop-3p" || len(jobsOf(t, e, cliA)) != 1 {
		t.Fatalf("catalog merge changed queue or lost origin: %+v %+v", s, s.Origin)
	}
}

func TestCatalogArchivesRemovedAndSupersededSessions(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	syncCatalog(t, e, true, catalogEntry(cliA, testHostSession, "A", clock.at), catalogEntry(cliB, otherHostSession, "B", clock.at))
	// An incomplete scan never deletes.
	syncCatalog(t, e, false, catalogEntry(cliA, testHostSession, "A", clock.at))
	if s := state(t, e, cliB); !s.InCatalog || s.Archived {
		t.Fatalf("partial scan removed a session: %+v", s)
	}
	syncCatalog(t, e, true, catalogEntry(cliA, testHostSession, "A", clock.at))
	if s := state(t, e, cliB); s.InCatalog || !s.Archived {
		t.Fatalf("deleted session not archived: %+v", s)
	}
	// The desktop session now runs a newer CLI conversation.
	const cliC = "77777777-8888-4999-aaaa-bbbbbbbbbbbb"
	syncCatalog(t, e, true, catalogEntry(cliC, testHostSession, "A", clock.at))
	if s := state(t, e, cliA); !s.Archived {
		t.Fatalf("superseded conversation still listed: %+v", s)
	}
	if s := state(t, e, cliC); s.Archived || s.Origin.HostSessionID != testHostSession {
		t.Fatalf("current conversation not listed: %+v", s)
	}
}

func TestCatalogDoesNotArchiveSupersededSessionWithQueuedContinue(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "StopFailure", clock.at)
	syncCatalog(t, e, true, catalogEntry(cliB, testHostSession, "A", clock.at))
	if s := state(t, e, cliA); s.Archived || s.State != domain.Cooldown {
		t.Fatalf("queued continue hidden: %+v", s)
	}
}

func TestCatalogKeepsTerminalOrigin(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	origin := terminalOrigin(4242, true, clock.at)
	terminalHook(t, e, cliA, "Stop", origin, clock.at, func(v *domain.HookEvent) { v.Model = "" })
	syncCatalog(t, e, true, catalogEntry(cliA, testHostSession, "T", clock.at))
	s := state(t, e, cliA)
	if s.Origin == nil || s.Origin.App != domain.AppTerminal || s.Model != "claude-opus-4-8[1m]" || !s.InCatalog {
		t.Fatalf("terminal session rewritten by catalog: %+v %+v", s, s.Origin)
	}
}

func TestDesktopSessionEndKeepsQueuedContinue(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "StopFailure", clock.at)
	hostedEvent(t, e, cliA, "SessionEnd", clock.at.Add(time.Second))
	if s := state(t, e, cliA); s.State != domain.Cooldown || len(jobsOf(t, e, cliA)) != 1 || jobsOf(t, e, cliA)[0].State != domain.Cooldown {
		t.Fatalf("desktop exit dropped the queue: %+v", s)
	}
	clock.at = clock.at.Add(time.Minute)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 || b.bindings[0].DeepLink != domain.DesktopDeepLink(testHostSession) {
		t.Fatalf("exited desktop session not reopened: calls=%d", b.count())
	}
}

func TestOpeningLimitedSessionKeepsQueuedContinue(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "StopFailure", clock.at)
	hostedEvent(t, e, cliA, "SessionEnd", clock.at.Add(time.Second))
	// The user only clicks the session; Claude starts its process, no prompt.
	hostedEvent(t, e, cliA, "SessionStart", clock.at.Add(2*time.Second))
	jobs := jobsOf(t, e, cliA)
	if s := state(t, e, cliA); s.State != domain.Cooldown || len(jobs) != 1 || jobs[0].State != domain.Cooldown {
		t.Fatalf("opening the session cancelled its queued continue: %+v %+v", s, jobs)
	}
	clock.at = clock.at.Add(time.Minute)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 {
		t.Fatalf("reopened session not continued: calls=%d", b.count())
	}
}

func TestPromptAfterOpeningSessionSupersedesQueuedContinue(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "StopFailure", clock.at)
	hostedEvent(t, e, cliA, "SessionStart", clock.at.Add(time.Second))
	hostedEvent(t, e, cliA, "UserPromptSubmit", clock.at.Add(2*time.Second))
	for _, j := range jobsOf(t, e, cliA) {
		if j.State != domain.Cancelled {
			t.Fatalf("manual prompt left the continue queued: %+v", j)
		}
	}
	if s := state(t, e, cliA); s.State != domain.Running {
		t.Fatalf("manual prompt not running: %+v", s)
	}
}

func TestDesktopSessionEndWithoutJobStaysReopenable(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "Stop", clock.at)
	hostedEvent(t, e, cliA, "SessionEnd", clock.at.Add(time.Second))
	if s := state(t, e, cliA); s.State != domain.Completed {
		t.Fatalf("idle desktop exit must not cancel the session: %+v", s)
	}
	if _, err := e.TestSend(context.Background(), cliA, ""); err != nil {
		t.Fatalf("exited desktop session cannot be tested: %v", err)
	}
}

func TestRefusedTestSendExplainsItWasNotSent(t *testing.T) {
	e, _, b, clock := hostedFixture(t, true)
	hostedEvent(t, e, cliA, "Stop", clock.at)
	b.result = domain.BridgeResult{Code: "input_unverified", Message: "未找到输入框"}
	if _, err := e.TestSend(context.Background(), cliA, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		scan(t, e)
		clock.at = clock.at.Add(6 * time.Second)
	}
	jobs := jobsOf(t, e, cliA)
	if len(jobs) != 1 || jobs[0].Reason != "未发送（input_unverified）：未找到输入框" {
		t.Fatalf("refusal reason unclear: %+v", jobs)
	}
}
