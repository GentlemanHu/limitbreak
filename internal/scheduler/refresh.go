package scheduler

import "context"

func (e *Engine) RefreshNow(ctx context.Context) error {
	if source, ok := e.quota.(interface{ InvalidateQuotaCache(context.Context) error }); ok {
		if err := source.InvalidateQuotaCache(ctx); err != nil {
			return err
		}
	}
	return e.Refresh(ctx)
}
