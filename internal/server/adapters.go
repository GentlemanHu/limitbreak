package server

import (
	"net/http"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/domain"
)

func (s *Server) adapterList(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"adapters": adapters.Descriptors()})
}
func (s *Server) sourceList(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	snapshot, err := s.engine.Snapshot(r.Context())
	if err != nil {
		fail(w, 500, "sources_failed", "无法读取来源健康状态")
		return
	}
	sources := snapshot.Sources
	if len(sources) == 0 {
		sources = snapshot.Capabilities.Sources
	}
	if sources == nil {
		sources = []domain.SourceHealth{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}
