package signals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

// codexResets reads the public Codex Resets API (https://codex-resets.com/api/docs).
// The API is free and keyless; its terms ask that displayed data credits
// codex-resets.com with a link, which Descriptor's attribution carries.
// Watch data is an AI-classified forecast, never an official commitment, and
// is shown only; it never triggers a quota query.
type codexResets struct {
	cfg  config.SignalSource
	http *http.Client
}

const (
	codexResetsMaxBody = 1 << 20
	// recentReset lets the first poll after installation still react to a
	// reset announced moments earlier.
	recentReset = 30 * time.Minute
	userAgent   = "LimitBreak (+https://github.com/GentlemanHu/limitbreak)"
)

func NewCodexResets(c config.SignalSource) (Source, error) {
	if err := config.ValidateSourceURL(c.URL); err != nil {
		return nil, err
	}
	return &codexResets{cfg: c, http: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *codexResets) Descriptor() domain.SignalSourceState {
	return domain.SignalSourceState{ID: c.cfg.ID, Type: c.cfg.Type, Name: "Codex Resets", Provider: c.cfg.Provider,
		Attribution: "Data from Codex Resets", AttributionURL: "https://codex-resets.com"}
}

type crSource struct {
	Type   string `json:"type"`
	Author string `json:"author"`
	URL    string `json:"url"`
}

type crReset struct {
	ID          string    `json:"id"`
	ResetType   string    `json:"reset_type"`
	AnnouncedAt time.Time `json:"announced_at"`
	Text        string    `json:"text"`
	Source      crSource  `json:"source"`
}

type crScheduled struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	ScheduledFor *time.Time `json:"scheduled_for"`
	Text         string     `json:"text"`
	Source       crSource   `json:"source"`
}

type crWatch struct {
	Level              string     `json:"level"`
	ResetChancePercent *int       `json:"reset_chance_percent"`
	ForecastWindow     string     `json:"forecast_window"`
	ExpiresAt          *time.Time `json:"expires_at"`
	Text               string     `json:"text"`
	Source             crSource   `json:"source"`
}

type crStatus struct {
	Data struct {
		LatestReset    *crReset     `json:"latest_reset"`
		ScheduledReset *crScheduled `json:"scheduled_reset"`
		ActiveWatch    *crWatch     `json:"active_watch"`
	} `json:"data"`
}

func (c *codexResets) Poll(ctx context.Context, prev domain.SignalSourceState, now time.Time) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.cfg.URL, "/")+"/api/v1/status", nil)
	if err != nil {
		return Result{}, errors.New("invalid Codex Resets URL")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, errors.New("Codex Resets request failed (transport or timeout)")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return Result{State: prev}, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		err := fmt.Errorf("Codex Resets returned HTTP %d", resp.StatusCode)
		if at := retryAfter(resp.Header.Get("Retry-After"), now); !at.IsZero() {
			return Result{}, &RetryError{At: at, Err: err}
		}
		return Result{}, err
	case resp.StatusCode != http.StatusOK:
		return Result{}, fmt.Errorf("Codex Resets returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, codexResetsMaxBody+1))
	if err != nil || len(body) > codexResetsMaxBody {
		return Result{}, errors.New("Codex Resets response could not be read")
	}
	var status crStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return Result{}, errors.New("Codex Resets response is not valid JSON")
	}
	state := prev
	if etag := resp.Header.Get("ETag"); len(etag) <= 256 {
		state.ETag = etag
	}
	var out []domain.ResetSignal
	if latest := status.Data.LatestReset; latest != nil && latest.ID != "" {
		signal, err := domain.NormalizeSignal(domain.ResetSignal{ID: safeID(latest.ID), Source: c.cfg.ID, Provider: c.cfg.Provider,
			Kind: domain.SignalResetExecuted, At: latest.AnnouncedAt.UTC(), ReceivedAt: now, Text: latest.Text, URL: latest.Source.URL})
		if err != nil {
			return Result{}, fmt.Errorf("Codex Resets returned an unusable reset: %w", err)
		}
		if isNewReset(prev, signal, now) {
			out = append(out, signal)
		}
		state.LastID = signal.ID
		state.Latest = &signal
	}
	state.Scheduled = nil
	if s := status.Data.ScheduledReset; s != nil && s.ID != "" {
		state.Scheduled = &domain.ScheduledReset{ID: s.ID, At: utc(s.ScheduledFor), Text: trimText(s.Text), URL: httpsOnly(s.Source.URL)}
	}
	state.Watch = nil
	if w := status.Data.ActiveWatch; w != nil && w.Level != "" {
		state.Watch = &domain.ResetWatch{Level: w.Level, ChancePercent: w.ResetChancePercent, Window: trimText(w.ForecastWindow),
			ExpiresAt: utc(w.ExpiresAt), Text: trimText(w.Text), URL: httpsOnly(w.Source.URL)}
	}
	return Result{State: state, Signals: out}, nil
}

// isNewReset emits a reset that differs from the last one seen and is not
// older than it. The first poll only reacts to a recent reset.
func isNewReset(prev domain.SignalSourceState, s domain.ResetSignal, now time.Time) bool {
	if prev.LastID == "" {
		return !s.At.IsZero() && now.Sub(s.At) <= recentReset && !s.At.After(now.Add(time.Minute))
	}
	if s.ID == prev.LastID {
		return false
	}
	return prev.Latest == nil || !s.At.Before(prev.Latest.At)
}

// safeID keeps an upstream ID that is already a plain token and replaces any
// other ID by a stable digest, so an odd ID never stops polling.
func safeID(id string) string {
	if _, err := domain.NormalizeSignal(domain.ResetSignal{ID: id, Source: "x", Provider: "x"}); err == nil {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256-" + hex.EncodeToString(sum[:16])
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
		return now.Add(time.Duration(min(n, 86400)) * time.Second)
	}
	if t, err := http.ParseTime(value); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func utc(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func trimText(s string) string {
	if len(s) > 500 {
		s = s[:500]
	}
	return strings.ToValidUTF8(strings.TrimSpace(s), "")
}

func httpsOnly(u string) string {
	if !strings.HasPrefix(u, "https://") || len(u) > 512 {
		return ""
	}
	return u
}
