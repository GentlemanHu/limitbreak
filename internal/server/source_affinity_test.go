package server

import "testing"

func TestHTTPBindingCannotSupplySourceAffinity(t *testing.T) {
	s, engine := setupServer()
	for _, field := range []string{"account_sources", "bound_sources", "source_affinity"} {
		body := `{"session_id":"session","monitor_resources":[{"account_id":"missing"}],"` + field + `":{"missing":"cpa-default"}}`
		w := request(s, "POST", "/api/bind", body, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
		if w.Code != 400 || engine.calls != 0 {
			t.Fatalf("client-supplied identity mapping accepted: %s %d", field, w.Code)
		}
	}
}
