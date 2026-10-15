package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func validBase() Config {
	c := Default("/tmp/limitbreak-test")
	c.Token = strings.Repeat("x", 64)
	return c
}

func TestClientsValidation(t *testing.T) {
	good := Client{ID: "codex", Provider: "codex", Command: []string{"/usr/local/bin/codex-continue", "{session_id}", "{message}"}, Env: []string{"OPENAI_API_KEY"}}
	c := validBase()
	c.Clients = []Client{good}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Client("codex"); !ok || got.Name != "codex" || got.TimeoutSeconds != 120 {
		t.Fatalf("defaults not applied: %+v", got)
	}
	bad := map[string]Client{
		"relative binary":     {ID: "a", Command: []string{"codex", "{message}"}},
		"no message":          {ID: "a", Command: []string{"/bin/echo", "{session_id}"}},
		"two messages":        {ID: "a", Command: []string{"/bin/echo", "{message}", "{message}"}},
		"embedded holder":     {ID: "a", Command: []string{"/bin/echo", "--session={session_id}", "{message}"}},
		"unknown holder":      {ID: "a", Command: []string{"/bin/echo", "{prompt}", "{message}"}},
		"uppercase id":        {ID: "Codex", Command: []string{"/bin/echo", "{message}"}},
		"bad provider":        {ID: "a", Provider: "Open AI", Command: []string{"/bin/echo", "{message}"}},
		"bad env":             {ID: "a", Env: []string{"lower"}, Command: []string{"/bin/echo", "{message}"}},
		"timeout too short":   {ID: "a", TimeoutSeconds: 1, Command: []string{"/bin/echo", "{message}"}},
		"unclean binary path": {ID: "a", Command: []string{"/bin/../bin/echo", "{message}"}},
	}
	for name, client := range bad {
		c := validBase()
		c.Clients = []Client{client}
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	dup := validBase()
	dup.Clients = []Client{good, good}
	if dup.Validate() == nil {
		t.Fatal("duplicate client accepted")
	}
	var strict Client
	if json.Unmarshal([]byte(`{"id":"a","command":["/bin/echo","{message}"],"shell":true}`), &strict) == nil {
		t.Fatal("unknown client field accepted")
	}
}

func TestSignalsDefaultsAndValidation(t *testing.T) {
	c := validBase()
	sources := c.EffectiveSignalSources()
	if len(sources) != 1 || sources[0].URL != "https://codex-resets.com" || sources[0].Provider != "codex" || sources[0].Mode != SignalModeAuto || sources[0].PollSeconds != 60 {
		t.Fatalf("default feed: %+v", sources)
	}
	if err := json.Unmarshal([]byte(`{"feeds":[]}`), &c.Signals); err != nil || len(c.EffectiveSignalSources()) != 0 {
		t.Fatalf("empty feeds must disable polling: %v %+v", err, c.EffectiveSignalSources())
	}
	for name, s := range map[string]SignalSource{
		"unknown type":  {ID: "x", Type: "rss"},
		"reserved id":   {ID: "webhook", Type: "codex-resets"},
		"plain http":    {ID: "x", Type: "codex-resets", URL: "http://example.com"},
		"bad mode":      {ID: "x", Type: "codex-resets", Mode: "sometimes"},
		"fast polling":  {ID: "x", Type: "codex-resets", PollSeconds: 5},
		"bad provider ": {ID: "x", Type: "codex-resets", Provider: "a b"},
	} {
		c := validBase()
		c.Signals.Sources = []SignalSource{s}
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
