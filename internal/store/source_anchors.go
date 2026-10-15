package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// NamespacedAnchors implements proxy.AnchorStorage without depending on proxy.
// The default namespace retains the old single-CPA reset anchors verbatim.
type NamespacedAnchors struct {
	store    *Store
	sourceID string
}

func (s *Store) QuotaAnchorNamespace(sourceID string) *NamespacedAnchors {
	return &NamespacedAnchors{store: s, sourceID: sourceID}
}
func (n *NamespacedAnchors) LoadQuotaAnchors(ctx context.Context) (map[string]map[string]time.Time, error) {
	if n.sourceID == domain.DefaultSourceID {
		return n.store.LoadQuotaAnchors(ctx)
	}
	if n.sourceID == "" {
		return nil, errors.New("source anchor namespace is required")
	}
	var b []byte
	err := n.store.db.QueryRowContext(ctx, "SELECT data FROM quota_source_anchors WHERE source_id=?", n.sourceID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, err
	}
	anchors := map[string]map[string]time.Time{}
	if err := json.Unmarshal(b, &anchors); err != nil {
		return nil, err
	}
	return anchors, nil
}
func (n *NamespacedAnchors) SaveQuotaAnchors(ctx context.Context, anchors map[string]map[string]time.Time) error {
	if n.sourceID == domain.DefaultSourceID {
		return n.store.SaveQuotaAnchors(ctx, anchors)
	}
	if n.sourceID == "" {
		return errors.New("source anchor namespace is required")
	}
	b, err := json.Marshal(anchors)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("source quota anchor store exceeds safety limit")
	}
	_, err = n.store.db.ExecContext(ctx, "INSERT INTO quota_source_anchors(source_id,data) VALUES(?,?) ON CONFLICT(source_id) DO UPDATE SET data=excluded.data", n.sourceID, b)
	return err
}
