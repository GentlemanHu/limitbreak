package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/config"
)

type authTransport struct {
	calls  int
	denied int
}

func (r *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	status, body := http.StatusOK, `{"files":[]}`
	if req.Header.Get("Authorization") == "Bearer rejected-secret" {
		status = r.denied
		body = `{"error":"unauthorized"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestRejectedManagementKeyIsNotRepeated(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cfg := config.Default(t.TempDir())
			cfg.ManagementKeyEnv = "QSO_AUTH_REGRESSION_KEY"
			t.Setenv(cfg.ManagementKeyEnv, "rejected-secret")
			client := New(cfg)
			transport := &authTransport{denied: status}
			client.http.Transport = transport
			for i := 0; i < 6; i++ {
				if _, err := client.Refresh(context.Background()); err == nil {
					t.Fatal("rejected key accepted")
				}
			}
			if transport.calls != 1 {
				t.Fatalf("bad key repeated %d times", transport.calls)
			}
			t.Setenv(cfg.ManagementKeyEnv, "new-authorized-secret")
			if _, err := client.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if transport.calls != 2 {
				t.Fatal("new key was not tested")
			}
		})
	}
}
