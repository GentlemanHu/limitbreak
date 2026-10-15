package config

import (
	"encoding/json"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/domain"
)

func TestLegacySourcesAreAdditive(t *testing.T) {
	c := Default(".")
	c.Token = strings.Repeat("x", 64)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	legacy := c.EffectiveSources()
	if len(legacy) != 1 || legacy[0].ID != domain.DefaultSourceID || legacy[0].URL != c.ProxyURL || legacy[0].CredentialEnv != c.ManagementKeyEnv || legacy[0].Type != "cliproxyapi" {
		t.Fatalf("legacy defaults changed: %+v", legacy)
	}
	var configured Config
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"sources"`) {
		t.Fatal("legacy defaults unexpectedly force a new sources array")
	}
	if err := json.Unmarshal(data, &configured); err != nil {
		t.Fatal(err)
	}
	if configured.EffectiveSources()[0].ID != domain.DefaultSourceID {
		t.Fatal("legacy single-source identity changed")
	}
	b := domain.Binding{AccountIDs: []string{"legacy-account"}}
	if domain.AccountID(legacy[0].ID, b.AccountIDs[0]) != "legacy-account" {
		t.Fatal("legacy binding must remain valid")
	}
}

func TestExplicitSourceOriginAndSecretGuards(t *testing.T) {
	for _, raw := range []string{"https://quota.example/v1", "http://127.0.0.1:8317", "http://[::1]:8080/quota"} {
		if err := ValidateSourceURL(raw); err != nil {
			t.Errorf("explicit URL %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"/quota", "//quota.example/path", "file:///private/key", "http://quota.example", "http://localhost/quota", "https://user:secret@quota.example/quota", "https://quota.example/quota?token=secret", "https://quota.example/quota#fragment", "javascript:alert(1)"} {
		if ValidateSourceURL(raw) == nil {
			t.Errorf("unsafe source URL accepted %q", raw)
		}
	}
	for _, payload := range []string{
		`{"id":"one","type":"readonly-http","url":"https://quota.example","schema":"normalized-v1","credential":"literal-secret"}`,
		`{"id":"one","type":"cliproxyapi","url":"https://quota.example","credential_file":"/private/key"}`,
		`{"id":"one","type":"readonly-http","url":"https://quota.example","shell":"env"}`,
		`{"id":"one","type":"readonly-http","url":"https://quota.example","dylib":"/private/plugin"}`,
		`{"id":"one","type":"readonly-http","url":"https://quota.example","headers":{"Authorization":"secret"}}`,
	} {
		var source Source
		if json.Unmarshal([]byte(payload), &source) == nil {
			t.Errorf("unknown execution/secret field accepted: %s", payload)
		}
	}
}

func TestSourceConfigurationValidation(t *testing.T) {
	c := Default(".")
	c.Token = strings.Repeat("x", 64)
	good := Source{ID: "other", Type: "cliproxyapi", URL: "https://quota.example", CredentialEnv: "EXPLICIT_QUOTA_KEY"}
	c.Sources = []Source{good}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.EffectiveSources()[0].CredentialEnv != "EXPLICIT_QUOTA_KEY" {
		t.Fatal("source credential reference was overridden")
	}
	for _, mutate := range []func(*Source){
		func(s *Source) { s.ID = "bad::source" },
		func(s *Source) { s.CredentialEnv = "" },
		func(s *Source) { s.CredentialEnv = "$(env)" },
		func(s *Source) { s.CredentialEnv = "PATH" },
		func(s *Source) { s.CredentialEnv = "HOME" },
		func(s *Source) { s.CredentialEnv = "TMPDIR" },
		func(s *Source) { s.CredentialEnv = "DYLD_INSERT_LIBRARIES" },
		func(s *Source) { s.CredentialEnv = "LD_PRELOAD" },
		func(s *Source) { s.CredentialEnv = "https_proxy" },
		func(s *Source) { s.Type = "codex" },
		func(s *Source) { s.ManagementPrefix = "/other" },
		func(s *Source) { s.HTTPTimeoutSeconds = 31 },
		func(s *Source) { s.PollSeconds = 1 },
		func(s *Source) { s.Schema = "normalized-v1" },
	} {
		bad := good
		mutate(&bad)
		c.Sources = []Source{bad}
		if c.Validate() == nil {
			t.Fatalf("invalid explicit source accepted: %+v", bad)
		}
	}
	c.Sources = []Source{good, good}
	if c.Validate() == nil {
		t.Fatal("duplicate source instance IDs accepted")
	}
	second := good
	second.ID = "second"
	c.Sources = []Source{good, second}
	if c.Validate() == nil {
		t.Fatal("different sources shared a credential_env identity")
	}
	second.CredentialEnv = "SECOND_EXPLICIT_QUOTA_KEY"
	c.Sources = []Source{good, second}
	if err := c.Validate(); err != nil {
		t.Fatalf("independent credential references rejected: %v", err)
	}
	legacy := Default(".")
	legacy.Token = c.Token
	legacy.ManagementKeyEnv = "PATH"
	if legacy.Validate() == nil {
		t.Fatal("legacy management key used runtime control variable")
	}
	readOnly := Source{ID: "custom", Type: "readonly-http", URL: "http://127.0.0.1:9000/quota", Schema: "normalized-v1"}
	c.Sources = []Source{readOnly}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	readOnly.ID = domain.DefaultSourceID
	c.Sources = []Source{readOnly}
	if c.Validate() == nil {
		t.Fatal("custom source appropriated legacy account identity")
	}
}
