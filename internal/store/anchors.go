package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) LoadQuotaAnchors(ctx context.Context) (map[string]map[string]time.Time, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM quota_anchors WHERE id=1").Scan(&b)
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

func (s *Store) SaveQuotaAnchors(ctx context.Context, anchors map[string]map[string]time.Time) error {
	b, err := json.Marshal(anchors)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("quota anchor store exceeds safety limit")
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO quota_anchors(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", b)
	return err
}
