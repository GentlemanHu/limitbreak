package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quota-session-orchestrator/internal/store"
)

func restrictedFixture(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	token := "qso_scope_" + strings.Repeat("a", 64)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing restricted token")
		}
		if !strings.HasPrefix(r.URL.Path, "/mcp/") {
			t.Error("unrestricted path requested")
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	c, err := New(Profile{DaemonURL: s.URL, AccessToken: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func writeFixture(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func statusFixture() Status {
	return Status{Time: time.Now().UTC(), PolicyDigest: strings.Repeat("a", 64), ControlRequiresConfirmation: true}
}
func fixtureRequest(in ActionInput) RequestView {
	now := time.Now().UTC()
	return RequestView{ID: in.RequestID, GrantID: "fixture-grant", Action: in.Action, Digest: store.ControlDigest(in.Action), State: store.ControlPending, CreatedAt: now, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second)}
}

func TestProfileOnlyExplicitRestricted0600File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	p := Profile{DaemonURL: "http://127.0.0.1:1", AccessToken: "qso_scope_" + strings.Repeat("a", 64)}
	data, _ := json.Marshal(p)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProfile(path)
	if err != nil || got != p {
		t.Fatal("valid scoped profile rejected")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(path); !errors.Is(err, ErrProfile) {
		t.Fatal("public profile accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(link); !errors.Is(err, ErrProfile) {
		t.Fatal("symlink accepted")
	}
	for _, body := range []string{`{"daemon_url":"http://127.0.0.1:1","access_token":"full-daemon-secret"}`, string(data) + ` {}`, string(data[:len(data)-1]) + `,"config":"private-secret"}`, strings.Repeat("x", 4097)} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadProfile(path)
		if !errors.Is(err, ErrProfile) || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("profile error not fixed")
		}
	}
	for _, url := range []string{"https://127.0.0.1:1", "http://localhost:1", "http://example.invalid:1", "http://127.0.0.1:1/path", "http://127.0.0.1:1?x=secret", "http://secret@127.0.0.1:1", "http://127.0.0.1:0", "http://127.0.0.1:65536"} {
		p.DaemonURL = url
		if ValidateProfile(p) == nil {
			t.Fatalf("accepted %s", url)
		}
	}
}

func TestBoundedTypedReadAndControlEndpoints(t *testing.T) {
	var count atomic.Int32
	in := ActionInput{RequestID: "fixture-request", TTLSeconds: 60, Action: store.ControlAction{Operation: "pause", SessionID: "daemon", PolicyDigest: strings.Repeat("b", 64)}}
	c := restrictedFixture(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		switch r.URL.Path {
		case "/mcp/status":
			if r.Method != "GET" {
				t.Error("status writes")
			}
			writeFixture(w, 200, statusFixture())
		case "/mcp/resources":
			if r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "opaque-cursor" {
				t.Error("pagination lost")
			}
			writeFixture(w, 200, Page{Items: []Item{{ID: "a_" + strings.Repeat("c", 48), State: "allowed"}}})
		case "/mcp/requests":
			if r.Method == "GET" {
				writeFixture(w, 200, RequestPage{Requests: []RequestView{fixtureRequest(in)}})
				return
			}
			var got ActionInput
			if json.NewDecoder(r.Body).Decode(&got) != nil || store.ControlDigest(got.Action) != store.ControlDigest(in.Action) {
				t.Error("typed action changed")
			}
			writeFixture(w, 202, RequestResponse{Request: fixtureRequest(got)})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := c.List(ctx, "resources", "opaque-cursor", 2)
	if err != nil || page.Items[0].ID != "a_"+strings.Repeat("c", 48) {
		t.Fatal("handle changed")
	}
	if _, err := c.Requests(ctx, "", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestAction(ctx, in); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 4 {
		t.Fatal("unexpected request count")
	}
	for _, collection := range []string{"../api/snapshot", "approve", "hook", "http"} {
		if _, err := c.List(ctx, collection, "", 1); err == nil {
			t.Fatal("arbitrary route accepted")
		}
	}
	if _, err := c.List(ctx, "resources", "", 101); err == nil {
		t.Fatal("unbounded page accepted")
	}
	in.TTLSeconds = 301
	if _, err := c.RequestAction(ctx, in); err == nil {
		t.Fatal("unbounded TTL accepted")
	}
	if count.Load() != 4 {
		t.Fatal("invalid input reached daemon")
	}
}

func TestHTTPFailureRedactionNoRedirectNoRetry(t *testing.T) {
	for _, code := range []int{401, 403, 409, 500, 302} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var count atomic.Int32
			c := restrictedFixture(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Location", "http://example.invalid/private-secret")
				writeFixture(w, code, map[string]string{"error": "qso_scope_secret private-prompt private-body"})
			})
			_, err := c.Status(context.Background())
			if !errors.Is(err, ErrDaemon) || err.Error() != ErrDaemon.Error() || count.Load() != 1 {
				t.Fatal("error leaked or request replayed")
			}
		})
	}
}

func TestRejectMalformedOversizedAndPrivateDTOs(t *testing.T) {
	valid, _ := json.Marshal(statusFixture())
	for _, body := range []string{string(valid) + ` {}`, string(valid[:len(valid)-1]) + `,"prompt":"private-prompt"}`, `{"policy_digest":"private-prompt","control_requires_confirmation":true}`, strings.Repeat(" ", 256<<10) + string(valid)} {
		c := restrictedFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		out, err := c.Status(context.Background())
		if !errors.Is(err, ErrDaemon) || out.PolicyDigest != "" {
			t.Fatal("untrusted DTO escaped")
		}
	}
	for _, item := range []Item{{ID: "private@email.invalid"}, {ID: "s_" + strings.Repeat("d", 48), State: "private-body"}, {ID: "s_" + strings.Repeat("d", 48), Kind: "private-title"}} {
		c := restrictedFixture(t, func(w http.ResponseWriter, r *http.Request) { writeFixture(w, 200, Page{Items: []Item{item}}) })
		page, err := c.List(context.Background(), "sessions", "", 1)
		if !errors.Is(err, ErrDaemon) || len(page.Items) != 0 {
			t.Fatal("unsafe item escaped")
		}
	}
}

func TestCancellationAndPostFailuresDoNotRetry(t *testing.T) {
	var count atomic.Int32
	c := restrictedFixture(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		writeFixture(w, 503, map[string]string{"error": "private-body"})
	})
	in := ActionInput{RequestID: "single-attempt", TTLSeconds: 1, Action: store.ControlAction{Operation: "refresh", SessionID: "daemon"}}
	if _, err := c.RequestAction(context.Background(), in); !errors.Is(err, ErrDaemon) {
		t.Fatal("POST failure accepted")
	}
	if count.Load() != 1 {
		t.Fatal("POST replayed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Status(ctx); !errors.Is(err, ErrDaemon) {
		t.Fatal("cancellation accepted")
	}
	if count.Load() != 1 {
		t.Fatal("cancelled request reached daemon")
	}
}
