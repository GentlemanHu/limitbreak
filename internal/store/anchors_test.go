package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaAnchorsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	value := map[string]map[string]time.Time{"account": {"fingerprint": anchor}}
	if err := s.SaveQuotaAnchors(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.LoadQuotaAnchors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !loaded["account"]["fingerprint"].Equal(anchor) {
		t.Fatal("relative reset observation shifted after restart")
	}
}
