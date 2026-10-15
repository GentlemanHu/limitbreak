package server

import (
	"encoding/json"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/domain"
)

func TestAdapterSourcesAreAuthenticated(t *testing.T) {
	s, e := setupServer()
	for _, path := range []string{"/api/adapters", "/api/sources"} {
		if w := request(s, "GET", path, "", "", "", "127.0.0.1:8318"); w.Code != 401 {
			t.Fatalf("unprotected %s: %d", path, w.Code)
		}
	}
	// Browser-originated requests are refused before authentication.
	if w := request(s, "GET", "/api/adapters", "", "Bearer "+s.cfg.Token, "http://127.0.0.1:8318", "127.0.0.1:8318"); w.Code != 403 {
		t.Fatalf("origin request accepted: %d", w.Code)
	}
	if e.calls != 0 {
		t.Fatal("unauthorized routes reached production engine")
	}
	w := request(s, "GET", "/api/adapters", "", "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	var adapterBody struct {
		Adapters []domain.AdapterDescriptor `json:"adapters"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &adapterBody) != nil || len(adapterBody.Adapters) != 12 {
		t.Fatalf("adapter contract: %d %s", w.Code, w.Body.String())
	}
	w = request(s, "GET", "/api/sources", "", "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sources":[]`) {
		t.Fatalf("source array contract: %d %s", w.Code, w.Body.String())
	}
	// The simulation endpoints were removed with the browser UI.
	if w := request(s, "GET", "/api/simulation/scenarios", "", "Bearer "+s.cfg.Token, "", "127.0.0.1:8318"); w.Code != 404 {
		t.Fatalf("simulation endpoint still served: %d", w.Code)
	}
}
