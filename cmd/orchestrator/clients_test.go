package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

func TestClientEventMapping(t *testing.T) {
	c := config.Default(t.TempDir())
	c.Clients = []config.Client{{ID: "codex", Provider: "codex", Command: []string{"/bin/echo", "{message}"}}}
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	e, err := clientEvent(c, []string{"--client", "codex", "--session", "thread-1", "--type", "limit_hit", "--cwd", "/tmp", "--event-id", "e1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.HookEventName != "StopFailure" || e.Error != "rate_limit" || e.Provider != "codex" || e.Kind != "code" || e.EventID != "client:codex:e1" ||
		e.Origin == nil || e.Origin.Client != "codex" || domain.ClassifyOrigin(*e.Origin) != domain.AppClient || domain.ValidateOrigin(*e.Origin) != nil {
		t.Fatalf("limit_hit mapping: %+v", e)
	}
	for kind, hook := range map[string]string{"turn_started": "UserPromptSubmit", "turn_finished": "Stop", "session_start": "SessionStart", "session_end": "SessionEnd", "turn_failed": "StopFailure"} {
		e, err := clientEvent(c, []string{"--client", "codex", "--session", "thread-1", "--type", kind}, now)
		if err != nil || e.HookEventName != hook || len(e.EventID) < 20 {
			t.Errorf("%s: %+v %v", kind, e, err)
		}
	}
	for name, args := range map[string][]string{
		"unknown client":  {"--client", "deepseek", "--session", "s", "--type", "turn_started"},
		"bad session":     {"--client", "codex", "--session", "../s", "--type", "turn_started"},
		"bad type":        {"--client", "codex", "--session", "s", "--type", "explode"},
		"relative cwd":    {"--client", "codex", "--session", "s", "--type", "turn_started", "--cwd", "rel"},
		"extra arguments": {"--client", "codex", "--session", "s", "--type", "turn_started", "x"},
	} {
		if _, err := clientEvent(c, args, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoadWebhookSecret(t *testing.T) {
	dir := t.TempDir()
	if s, err := loadWebhookSecret(filepath.Join(dir, "missing")); s != nil || err != nil {
		t.Fatalf("missing file must disable signing: %q %v", s, err)
	}
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := loadWebhookSecret(path); err != nil || string(s) != strings.Repeat("k", 40) {
		t.Fatalf("valid secret: %q %v", s, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWebhookSecret(path); err == nil {
		t.Fatal("world-readable secret accepted")
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWebhookSecret(short); err == nil {
		t.Fatal("short secret accepted")
	}
}
