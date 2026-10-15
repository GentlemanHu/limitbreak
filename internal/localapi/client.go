// Package localapi is a restricted client for an already-running local daemon.
package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/store"
)

type Profile struct {
	DaemonURL   string `json:"daemon_url"`
	AccessToken string `json:"access_token"`
}

type Item struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id,omitempty"`
	SourceID    string     `json:"source_id,omitempty"`
	AccountID   string     `json:"account_id,omitempty"`
	GroupID     string     `json:"group_id,omitempty"`
	WindowID    string     `json:"window_id,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	State       string     `json:"state,omitempty"`
	Generation  int64      `json:"generation,omitempty"`
	Connected   bool       `json:"connected,omitempty"`
	Implemented bool       `json:"implemented,omitempty"`
	ReadOnly    bool       `json:"read_only,omitempty"`
	HasError    bool       `json:"has_error,omitempty"`
	AutoResume  bool       `json:"auto_resume,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
	NotBefore   *time.Time `json:"not_before,omitempty"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	Blocked     bool       `json:"blocked,omitempty"`
}

type Page struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type Status struct {
	Time                 time.Time  `json:"time"`
	Paused               bool       `json:"paused"`
	PolicyDigest         string     `json:"policy_digest"`
	Sessions             int        `json:"sessions"`
	Jobs                 int        `json:"jobs"`
	Accounts             int        `json:"accounts"`
	ProxyConnected       bool       `json:"proxy_connected"`
	BridgeChecked        bool       `json:"bridge_checked"`
	BridgeAvailable      bool       `json:"bridge_available"`
	AccessibilityGranted bool       `json:"accessibility_granted"`
	ScreenLocked         bool       `json:"screen_locked"`
	DesktopCheckedAt     *time.Time `json:"desktop_checked_at,omitempty"`
	// This is observation only, never authority to send or claim readiness.
	ControlRequiresConfirmation bool `json:"control_requires_confirmation"`
}

type ActionInput struct {
	RequestID  string              `json:"request_id"`
	TTLSeconds int                 `json:"ttl_seconds"`
	Action     store.ControlAction `json:"action"`
}

type RequestView struct {
	ID          string               `json:"id"`
	GrantID     string               `json:"grant_id"`
	Action      store.ControlAction  `json:"action"`
	Digest      string               `json:"digest"`
	State       string               `json:"state"`
	CreatedAt   time.Time            `json:"created_at"`
	ExpiresAt   time.Time            `json:"expires_at"`
	ApprovedAt  *time.Time           `json:"approved_at,omitempty"`
	ExecutingAt *time.Time           `json:"executing_at,omitempty"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
	Result      *store.ControlResult `json:"result,omitempty"`
}

type RequestResponse struct {
	Request RequestView `json:"request"`
}
type RequestPage struct {
	Requests   []RequestView `json:"requests"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type Client struct {
	base  string
	token string
	http  *http.Client
}

var ErrProfile = errors.New("invalid restricted access profile")
var ErrDaemon = errors.New("local daemon request failed; operation not retried")

func ValidateProfile(p Profile) error {
	u, err := url.Parse(p.DaemonURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return ErrProfile
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 || strings.Contains(u.Hostname(), "%") {
		return ErrProfile
	}
	if !strings.HasPrefix(p.AccessToken, "qso_scope_") || len(p.AccessToken) != len("qso_scope_")+64 {
		return ErrProfile
	}
	for _, c := range strings.TrimPrefix(p.AccessToken, "qso_scope_") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ErrProfile
		}
	}
	return nil
}

func LoadProfile(path string) (Profile, error) {
	var p Profile
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		return p, ErrProfile
	}
	f, err := os.Open(path)
	if err != nil {
		return p, ErrProfile
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return p, ErrProfile
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return Profile{}, ErrProfile
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || ValidateProfile(p) != nil {
		return Profile{}, ErrProfile
	}
	return p, nil
}

func New(p Profile) (*Client, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	t := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, MaxConnsPerHost: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, DisableCompression: true}
	return &Client{base: p.DaemonURL, token: p.AccessToken, http: &http.Client{Transport: t, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// do is intentionally unexported: callers cannot supply arbitrary paths or headers.
func (c *Client) do(ctx context.Context, method, path string, body, out any, want int) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil || len(b) > 16<<10 {
			return ErrDaemon
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
	if err != nil {
		return ErrDaemon
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return ErrDaemon
	}
	defer resp.Body.Close()
	if resp.StatusCode != want || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return ErrDaemon
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(data) > 256<<10 {
		return ErrDaemon
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrDaemon
	}
	return nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	if c.do(ctx, "GET", "/mcp/status", nil, &out, 200) != nil || ValidateStatus(out) != nil {
		return Status{}, ErrDaemon
	}
	return out, nil
}

func pageQuery(cursor string, limit int) (string, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || len(cursor) > 128 {
		return "", ErrDaemon
	}
	v := url.Values{}
	v.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	return "?" + v.Encode(), nil
}

func (c *Client) List(ctx context.Context, collection, cursor string, limit int) (Page, error) {
	switch collection {
	case "resources", "sources", "sessions", "queue", "adapters", "diagnostics":
	default:
		return Page{}, ErrDaemon
	}
	q, err := pageQuery(cursor, limit)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: []Item{}}
	if limit == 0 {
		limit = 50
	}
	if c.do(ctx, "GET", "/mcp/"+collection+q, nil, &out, 200) != nil || ValidatePage(out, collection, limit) != nil {
		return Page{}, ErrDaemon
	}
	return out, nil
}

func (c *Client) Requests(ctx context.Context, cursor string, limit int) (RequestPage, error) {
	q, err := pageQuery(cursor, limit)
	if err != nil {
		return RequestPage{}, err
	}
	out := RequestPage{Requests: []RequestView{}}
	if limit == 0 {
		limit = 50
	}
	if c.do(ctx, "GET", "/mcp/requests"+q, nil, &out, 200) != nil || ValidateRequests(out, limit) != nil {
		return RequestPage{}, ErrDaemon
	}
	return out, nil
}

func (c *Client) RequestAction(ctx context.Context, in ActionInput) (RequestResponse, error) {
	if ValidateActionInput(in) != nil {
		return RequestResponse{}, ErrDaemon
	}
	var out RequestResponse
	if c.do(ctx, "POST", "/mcp/requests", in, &out, 202) != nil || ValidateRequest(out.Request) != nil || out.Request.ID != in.RequestID || store.ControlDigest(out.Request.Action) != store.ControlDigest(in.Action) {
		return RequestResponse{}, ErrDaemon
	}
	return out, nil
}
