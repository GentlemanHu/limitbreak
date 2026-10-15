package proxy

import (
	"context"
	"errors"
	"time"
)

// AnchorStorage persists the first observation of an otherwise relative reset.
// Only timestamps and fingerprints are stored; credentials never leave CPA.
type AnchorStorage interface {
	LoadQuotaAnchors(context.Context) (map[string]map[string]time.Time, error)
	SaveQuotaAnchors(context.Context, map[string]map[string]time.Time) error
}

// SetAnchorStorage must be called before the engine starts refreshing.
func (c *Client) SetAnchorStorage(ctx context.Context, storage AnchorStorage) error {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	default:
		return errors.New("quota refresh is active")
	}
	if storage == nil {
		return errors.New("anchor storage is required")
	}
	anchors, err := storage.LoadQuotaAnchors(ctx)
	if err != nil {
		return err
	}
	if anchors == nil {
		anchors = map[string]map[string]time.Time{}
	}
	c.anchors = anchors
	c.anchorStorage = storage
	return nil
}
