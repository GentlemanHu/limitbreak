package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Webhook signature headers. The signature is
// "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)), and the
// timestamp (Unix seconds) must be within signatureSkew of the daemon clock.
const (
	SignatureHeader = "X-LimitBreak-Signature"
	TimestampHeader = "X-LimitBreak-Timestamp"
	signatureSkew   = 5 * time.Minute
	maxSignalBody   = 16 << 10
)

// WithSignalSecret enables HMAC-signed webhook signals. Without it only the
// local token is accepted on POST /api/signals.
func WithSignalSecret(secret []byte) Option {
	return func(s *Server) { s.signalSecret = append([]byte(nil), secret...) }
}

type signalPayload struct {
	Source   string    `json:"source"`
	Provider string    `json:"provider"`
	ID       string    `json:"id"`
	Kind     string    `json:"kind,omitempty"`
	At       time.Time `json:"at,omitempty"`
	Text     string    `json:"text,omitempty"`
	URL      string    `json:"url,omitempty"`
}

type signalReceiver interface {
	ReceiveSignal(context.Context, domain.ResetSignal) (int, error)
}

// signals receives a pushed reset signal. A signal only makes the provider's
// accounts query their quota sooner; it never sends anything by itself.
func (s *Server) signals(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "content_type", "请求必须为 JSON")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignalBody))
	if err != nil {
		fail(w, 400, "invalid_json", "请求超限")
		return
	}
	if !s.signalAuthorized(r, body) {
		fail(w, 401, "unauthorized", "需要本地令牌或有效的 Webhook 签名")
		return
	}
	var p signalPayload
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		fail(w, 400, "invalid_json", "JSON 格式或字段无效")
		return
	}
	receiver, ok := s.engine.(signalReceiver)
	if !ok {
		fail(w, 503, "signals_unavailable", "当前守护器不接收重置信号")
		return
	}
	n, err := receiver.ReceiveSignal(r.Context(), domain.ResetSignal{ID: p.ID, Source: p.Source, Provider: p.Provider, Kind: p.Kind, At: p.At, Text: p.Text, URL: p.URL})
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]any{"ok": true, "accounts": n})
	case errors.Is(err, domain.ErrDuplicateSignal):
		writeJSON(w, 200, map[string]any{"ok": true, "duplicate": true, "accounts": 0})
	default:
		fail(w, 422, "invalid_signal", err.Error())
	}
}

func (s *Server) signalAuthorized(r *http.Request, body []byte) bool {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.checkToken(token)
	}
	if len(s.signalSecret) == 0 {
		return false
	}
	return VerifySignature(s.signalSecret, r.Header.Get(TimestampHeader), r.Header.Get(SignatureHeader), body, time.Now())
}

// VerifySignature checks a webhook signature in constant time.
func VerifySignature(secret []byte, timestamp, signature string, body []byte, now time.Time) bool {
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(ts, 0)
	if at.Before(now.Add(-signatureSkew)) || at.After(now.Add(signatureSkew)) {
		return false
	}
	got, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok {
		return false
	}
	raw, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	return hmac.Equal(raw, Sign(secret, strconv.FormatInt(ts, 10), body))
}

// Sign returns the raw HMAC-SHA256 of timestamp + "." + body.
func Sign(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}
