package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

type httpSource struct {
	cfg    config.Source
	client *http.Client
	now    func() time.Time
	lookup func(string) (string, bool)
	mu     sync.Mutex
	caps   domain.Capabilities
}

func newHTTP(_ context.Context, _ config.Config, source config.Source, _ *store.Store) (QuotaSource, error) {
	if err := config.ValidateSourceURL(source.URL); err != nil {
		return nil, err
	}
	if source.Schema != "normalized-v1" {
		return nil, errors.New("unsupported readonly-http schema")
	}
	if err := config.ValidateCredentialEnv(source.CredentialEnv); err != nil {
		return nil, err
	}
	if source.HTTPTimeoutSeconds == 0 {
		source.HTTPTimeoutSeconds = 10
	}
	if source.MaxBodyBytes == 0 {
		source.MaxBodyBytes = 1 << 20
	}
	if source.PollSeconds == 0 {
		source.PollSeconds = 180
	}
	if source.HTTPTimeoutSeconds < 1 || source.HTTPTimeoutSeconds > 30 || source.MaxBodyBytes < 1 || source.MaxBodyBytes > 4<<20 {
		return nil, errors.New("invalid readonly-http request bounds")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Do not forward explicit credentials through environment proxies.
	return &httpSource{cfg: source, now: time.Now, lookup: os.LookupEnv, client: &http.Client{
		Timeout: time.Duration(source.HTTPTimeoutSeconds) * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (s *httpSource) Capabilities() domain.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}
func (s *httpSource) Refresh(ctx context.Context) (accounts []domain.Account, err error) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.caps = domain.Capabilities{ProxyConnected: err == nil, Message: "normalized-v1 read-only quota"}
		if err != nil {
			s.caps.Message = "readonly-http refresh failed"
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, errors.New("invalid readonly-http request")
	}
	req.Header.Set("Accept", "application/json")
	if s.cfg.CredentialEnv != "" {
		key, exists := s.lookup(s.cfg.CredentialEnv)
		if !exists || strings.TrimSpace(key) == "" || len(key) > 16384 || strings.ContainsAny(key, "\r\n\x00") {
			return nil, errors.New("explicit source credential is unavailable or invalid")
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("readonly-http request failed (transport or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("readonly-http returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.cfg.MaxBodyBytes {
		return nil, errors.New("readonly-http response exceeds read/body limits")
	}
	return decodeNormalized(body, s.now().UTC(), time.Duration(s.cfg.PollSeconds)*time.Second)
}

// normalized-v1 is exactly {"accounts":[{id,provider,email,plan,models,
// disabled,checked_at,windows,groups,metrics}]}. States and availability are
// derived from explicit percent/blocked/reset evidence, never trusted from JSON.
// Account and group checked_at are required RFC3339 observations from the source.
type normalizedAccount struct {
	ID        string               `json:"id"`
	Provider  string               `json:"provider"`
	Email     string               `json:"email"`
	Plan      string               `json:"plan"`
	Models    []string             `json:"models"`
	Disabled  bool                 `json:"disabled"`
	CheckedAt time.Time            `json:"checked_at"`
	Windows   []domain.Window      `json:"windows"`
	Groups    []normalizedGroup    `json:"groups"`
	Metrics   []domain.QuotaMetric `json:"metrics"`
}
type normalizedGroup struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Scope       string          `json:"scope"`
	Models      []string        `json:"models,omitempty"`
	Windows     []domain.Window `json:"windows"`
	CheckedAt   time.Time       `json:"checked_at"`
}

func decodeNormalized(body []byte, now time.Time, poll time.Duration) ([]domain.Account, error) {
	var document struct {
		Accounts *[]normalizedAccount `json:"accounts"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&document); err != nil {
		return nil, errors.New("invalid normalized-v1 quota document")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) || document.Accounts == nil || len(*document.Accounts) > 4096 {
		return nil, errors.New("normalized-v1 requires a single bounded accounts array")
	}
	accounts := []domain.Account{}
	seen := map[string]bool{}
	for _, raw := range *document.Accounts {
		if raw.ID == "" || len(raw.ID) > 400 || strings.ContainsAny(raw.ID, "\x00\r\n") || seen[raw.ID] || raw.Provider == "" || len(raw.Provider) > 128 || len(raw.Models) > 256 || len(raw.Groups) > 128 || len(raw.Metrics) > 128 || raw.CheckedAt.IsZero() || raw.CheckedAt.After(now) {
			return nil, errors.New("invalid normalized account identity, bounds or observation time")
		}
		seen[raw.ID] = true
		a := domain.Account{ID: raw.ID, Provider: raw.Provider, Email: raw.Email, Plan: raw.Plan, Models: raw.Models, Disabled: raw.Disabled,
			CheckedAt: raw.CheckedAt, NextCheckAt: now.Add(poll), QuotaSource: "readonly-http:normalized-v1", Groups: []domain.QuotaGroup{}, Metrics: raw.Metrics,
			Windows: append([]domain.Window{}, raw.Windows...), Reason: "normalized-v1 quota evidence; not routing authority"}
		if err := validateWindows(raw.Windows); err != nil {
			return nil, err
		}
		groups := map[string]bool{}
		for _, g := range raw.Groups {
			if g.ID == "" || len(g.ID) > 128 || groups[g.ID] || g.CheckedAt.IsZero() || g.CheckedAt.After(now) || len(g.Models) > 256 || g.Scope != "account" && g.Scope != "models" && g.Scope != "unknown" || g.Scope == "models" && len(g.Models) == 0 {
				return nil, errors.New("invalid normalized quota group")
			}
			groups[g.ID] = true
			if err := validateWindows(g.Windows); err != nil {
				return nil, err
			}
			state, at := domain.WindowState(g.Windows)
			a.Groups = append(a.Groups, domain.QuotaGroup{ID: g.ID, Name: g.Name, Description: g.Description, Scope: g.Scope, Models: g.Models, Windows: g.Windows, State: state, AvailableAt: at, CheckedAt: g.CheckedAt})
			// Whole-account bindings conservatively require all observed windows;
			// group bindings use only explicit scope plus applicable shared limits.
			a.Windows = append(a.Windows, g.Windows...)
		}
		a.State, a.AvailableAt = domain.WindowState(a.Windows)
		if a.Disabled {
			a.State = "disabled"
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

func validateWindows(windows []domain.Window) error {
	if len(windows) > 256 {
		return errors.New("too many normalized quota windows")
	}
	seen := map[string]bool{}
	for _, w := range windows {
		if w.ID == "" || len(w.ID) > 128 || seen[w.ID] || w.Name == "" || w.LimitSeconds < 0 {
			return errors.New("invalid normalized quota window identity")
		}
		seen[w.ID] = true
		if w.UsedPercent != nil && (math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100) {
			return errors.New("invalid normalized used_percent")
		}
	}
	return nil
}
