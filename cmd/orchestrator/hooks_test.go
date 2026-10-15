package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookNormalizationDropsPrivateContent(t *testing.T) {
	raw := []byte(`{"session_id":"session","hook_event_name":"PostModelSwitch","to_model":"configured-model","from_model":"old-model","prompt":"PRIVATE PROMPT","last_assistant_message":"PRIVATE ANSWER","session_title":"用户绑定的标题"}`)
	now := time.Now()
	e, err := normalizeHook(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.NewModel != "configured-model" || e.SessionTitle != "用户绑定的标题" || e.EventID == "" || !e.ObservedAt.Equal(now) {
		t.Fatal("incorrect normalization")
	}
	encoded, _ := json.Marshal(e)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatal("prompt or full message retained")
	}
}

func TestHookKeepsOnlyKnownPermissionModes(t *testing.T) {
	for raw, want := range map[string]string{
		`{"session_id":"s","hook_event_name":"StopFailure","permission_mode":"bypassPermissions"}`: "bypassPermissions",
		`{"session_id":"s","hook_event_name":"Stop","permission_mode":"default"}`:                  "default",
		`{"session_id":"s","hook_event_name":"Stop","permission_mode":"made-up"}`:                  "",
		`{"session_id":"s","hook_event_name":"Stop"}`:                                              "",
	} {
		e, err := normalizeHook([]byte(raw), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if e.PermissionMode != want {
			t.Fatalf("%s: mode %q, want %q", raw, e.PermissionMode, want)
		}
	}
}

func TestHookTitleComesFromTranscriptTitleRecords(t *testing.T) {
	dir := t.TempDir()
	id := "9c4e2a71-5d3b-4f8e-a6c2-1b7d0e9f3a54"
	path := filepath.Join(dir, id+".jsonl")
	lines := []string{
		`{"type":"custom-title","customTitle":"旧标题","sessionId":"` + id + `"}`,
		`{"type":"user","message":{"content":"PRIVATE PROMPT custom-title"},"sessionId":"` + id + `"}`,
		`{"type":"custom-title","customTitle":"其他会话","sessionId":"other"}`,
		`{"type":"custom-title","customTitle":" 你是什么\n","sessionId":"` + id + `"}`,
		`{"type":"assistant","message":{"content":"` + strings.Repeat("x", 100<<10) + `"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"session_id":"` + id + `","hook_event_name":"Stop","transcript_path":"` + path + `"}`)
	e, err := normalizeHook(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if e.SessionTitle != "你是什么" {
		t.Fatalf("title %q", e.SessionTitle)
	}
	encoded, _ := json.Marshal(e)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatal("transcript content retained")
	}
	// A transcript of another session, a symlink or a relative path is ignored.
	link := filepath.Join(dir, "linked.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, id string }{{path, "other"}, {link, "linked"}, {id + ".jsonl", id}} {
		if got := transcriptTitle(c.path, c.id); got != "" {
			t.Fatalf("%s: unexpected title %q", c.path, got)
		}
	}
}

func TestHookUnknownEventAndBadSession(t *testing.T) {
	for _, raw := range []string{`{"session_id":"a/b","hook_event_name":"Stop"}`, `{"session_id":"a","hook_event_name":"MadeUp"}`, `{}`} {
		if _, err := normalizeHook([]byte(raw), time.Now()); err == nil {
			t.Fatal("invalid hook accepted")
		}
	}
}

func TestVersionGate(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{{"2.1.250", false}, {"2.1.251", true}, {"2.1.267", true}, {"3.0.0", true}, {"unknown", false}, {"2.1.78", false}} {
		if got := versionAtLeast(c.v, 2, 1, 251); got != c.want {
			t.Errorf("version %s got %v", c.v, got)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != "'a'\"'\"'b'" {
		t.Fatalf("unsafe shell quote: %s", got)
	}
}
