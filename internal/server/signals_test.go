package server

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

type signalEngine struct {
	recordingEngine
	got []domain.ResetSignal
}

func (e *signalEngine) ReceiveSignal(_ context.Context, s domain.ResetSignal) (int, error) {
	for _, old := range e.got {
		if old.ID == s.ID {
			return 0, domain.ErrDuplicateSignal
		}
	}
	e.got = append(e.got, s)
	return 2, nil
}

func signedRequest(s *Server, body string, secret []byte, ts time.Time, tamper bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8318/api/signals", strings.NewReader(body))
	r.Host = "127.0.0.1:8318"
	r.Header.Set("Content-Type", "application/json")
	stamp := strconv.FormatInt(ts.Unix(), 10)
	sig := Sign(secret, stamp, []byte(body))
	if tamper {
		sig[0] ^= 1
	}
	r.Header.Set(TimestampHeader, stamp)
	r.Header.Set(SignatureHeader, "sha256="+hex.EncodeToString(sig))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSignalWebhookAuthentication(t *testing.T) {
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	secret := []byte(strings.Repeat("s", 40))
	e := &signalEngine{}
	s := New(c, e, WithSignalSecret(secret))
	body := `{"source":"webhook","provider":"codex","id":"r1","kind":"reset_executed"}`
	if w := request(s, "POST", "/api/signals", body, "", "", "127.0.0.1:8318"); w.Code != 403 {
		t.Fatalf("unauthenticated signal: %d", w.Code)
	}
	if w := signedRequest(s, body, secret, time.Now(), true); w.Code != 401 {
		t.Fatalf("tampered signature: %d", w.Code)
	}
	if w := signedRequest(s, body, secret, time.Now().Add(-10*time.Minute), false); w.Code != 401 {
		t.Fatalf("stale signature: %d", w.Code)
	}
	if w := signedRequest(s, body, secret, time.Now(), false); w.Code != 200 || !strings.Contains(w.Body.String(), `"accounts":2`) {
		t.Fatalf("signed signal: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/signals", body, "Bearer "+c.Token, "", "127.0.0.1:8318"); w.Code != 200 || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("token signal duplicate: %d %s", w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/signals", `{"id":"r2","provider":"codex","extra":1}`, "Bearer "+c.Token, "", "127.0.0.1:8318"); w.Code != 400 {
		t.Fatalf("unknown field accepted: %d", w.Code)
	}
	if len(e.got) != 1 || e.got[0].Provider != "codex" {
		t.Fatalf("received: %+v", e.got)
	}
	// Without a configured secret, signatures are never accepted.
	plain := New(c, &signalEngine{})
	if w := signedRequest(plain, body, secret, time.Now(), false); w.Code != 401 {
		t.Fatalf("signature accepted without secret: %d", w.Code)
	}
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("k")
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{}`)
	good := "sha256=" + hex.EncodeToString(Sign(secret, "1800000000", body))
	if !VerifySignature(secret, "1800000000", good, body, now) {
		t.Fatal("valid signature rejected")
	}
	for name, sig := range map[string]string{"no prefix": strings.TrimPrefix(good, "sha256="), "not hex": "sha256=zz", "empty": ""} {
		if VerifySignature(secret, "1800000000", sig, body, now) {
			t.Errorf("%s accepted", name)
		}
	}
	if VerifySignature(secret, "1800000000", good, []byte(`{"a":1}`), now) {
		t.Fatal("other body accepted")
	}
}
