package adapters

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func httpFixture(t *testing.T) *httpSource {
	t.Helper()
	q, err := newHTTP(context.Background(), config.Config{}, config.Source{ID: "custom", URL: "https://quota.example/quota", Schema: "normalized-v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := q.(*httpSource)
	s.now = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC) }
	s.lookup = func(string) (string, bool) { t.Fatal("unexpected secret lookup"); return "", false }
	return s
}
func response(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

const normalizedDocument = `{"accounts":[{"id":"raw","provider":"anthropic","email":"user@example.invalid","plan":"plus","models":["opus"],"checked_at":"2025-01-01T00:00:00Z","windows":[],"groups":[{"id":"opus","name":"Opus","scope":"models","models":["opus"],"checked_at":"2025-01-01T00:00:00Z","windows":[{"id":"short","name":"Short","used_percent":12,"blocked":false}]}],"metrics":[]}]}`

func TestReadonlyHTTPExplicitGETAndNormalizedMapping(t *testing.T) {
	s := httpFixture(t)
	s.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.Body != nil || r.Header.Get("Authorization") != "" || r.URL.String() != "https://quota.example/quota" {
			t.Fatal("not a fixed credential-free GET")
		}
		return response(r, 200, normalizedDocument), nil
	})
	accounts, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != "raw" || accounts[0].State != "allowed" || len(accounts[0].Windows) != 1 || len(accounts[0].Groups) != 1 || accounts[0].Groups[0].Scope != "models" || accounts[0].CheckedAt.Equal(s.now()) {
		t.Fatalf("incorrect normalized mapping: %+v", accounts)
	}
	if !s.Capabilities().ProxyConnected {
		t.Fatal("healthy source not represented")
	}
}

func TestReadonlyHTTPRejectsRedirectAndDoesNotLeakCredential(t *testing.T) {
	s := httpFixture(t)
	s.cfg.CredentialEnv = "EXPLICIT_SOURCE_KEY"
	lookups, calls := 0, 0
	s.lookup = func(ref string) (string, bool) {
		lookups++
		if ref != "EXPLICIT_SOURCE_KEY" {
			t.Fatal("nonexplicit key discovery")
		}
		return "fixture-private-key", true
	}
	s.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture-private-key" {
			t.Fatal("explicit credential not applied")
		}
		resp := response(r, 302, "sensitive-body")
		resp.Header.Set("Location", "https://redirect.example/steal")
		return resp, nil
	})
	_, err := s.Refresh(context.Background())
	if err == nil || calls != 1 || lookups != 1 {
		t.Fatal("redirect followed or secret discovery attempted")
	}
	for _, text := range []string{err.Error(), s.Capabilities().Message} {
		if strings.Contains(text, "fixture-private-key") || strings.Contains(text, "sensitive-body") || strings.Contains(text, "redirect.example") {
			t.Fatal("credential, body or destination leaked")
		}
	}
}

func TestReadonlyHTTPBodyAndSecretBounds(t *testing.T) {
	s := httpFixture(t)
	s.cfg.MaxBodyBytes = 64
	calls := 0
	s.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(r, 200, strings.Repeat("x", 65)), nil
	})
	if _, err := s.Refresh(context.Background()); err == nil {
		t.Fatal("oversized body accepted")
	}
	s.cfg.CredentialEnv = "EXPLICIT_SOURCE_KEY"
	s.lookup = func(string) (string, bool) { return "malformed\r\nAuthorization: secret", true }
	if _, err := s.Refresh(context.Background()); err == nil || calls != 1 {
		t.Fatal("unsafe credential reached transport")
	}
	s.lookup = func(string) (string, bool) { return "", false }
	if _, err := s.Refresh(context.Background()); err == nil || calls != 1 {
		t.Fatal("missing credential silently fell back to another secret")
	}
	if s.client.Timeout != 10*time.Second || s.client.CheckRedirect == nil {
		t.Fatal("request limits missing")
	}
}

func TestNormalizedSchemaRejectsImplicitStatesAndObservations(t *testing.T) {
	s := httpFixture(t)
	for _, body := range []string{
		`{}`, `{"accounts":null}`, `{"accounts":[]} {}`,
		strings.Replace(normalizedDocument, `"used_percent":12`, `"used_percent":101`, 1),
		strings.Replace(normalizedDocument, `"checked_at":"2025-01-01T00:00:00Z",`, "", 1),
		strings.Replace(normalizedDocument, `"id":"raw"`, `"id":"raw","state":"allowed"`, 1),
		strings.Replace(normalizedDocument, `"scope":"models"`, `"scope":"guessed-name"`, 1),
		strings.Replace(normalizedDocument, `"id":"raw"`, `"id":"raw","credential":"inline-secret"`, 1),
	} {
		if _, err := decodeNormalized([]byte(body), s.now(), time.Minute); err == nil {
			t.Fatalf("unverified schema accepted: %s", body)
		}
	}
	accounts, err := decodeNormalized([]byte(strings.Replace(normalizedDocument, `"used_percent":12`, `"used_percent":100`, 1)), s.now(), time.Minute)
	if err != nil || accounts[0].State != "limited" || accounts[0].Groups[0].State != "limited" {
		t.Fatal("explicit percent did not determine blocked quota")
	}
}
