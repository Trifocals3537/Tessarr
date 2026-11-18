package nntp

import (
	"context"
	"time"
)

type backgroundKey struct{}

// WithBackground keeps one provider slot available for foreground work when
// the provider has multiple slots. A one-slot account remains usable while
// idle; its current bounded article must finish before another caller uses it.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func trySlot(ctx context.Context, pp *ProviderPool) bool {
	background, _ := ctx.Value(backgroundKey{}).(bool)
	limit := cap(pp.slots)
	if background {
		limit = max(1, limit-1)
	}
	if len(pp.slots) >= limit {
		return false
	}
	select {
	case pp.slots <- struct{}{}:
		if background && len(pp.slots) > limit {
			<-pp.slots
			return false
		}
		return true
	default:
		return false
	}
}

func waitSlot(ctx context.Context, pp *ProviderPool) error {
	background, _ := ctx.Value(backgroundKey{}).(bool)
	if !background {
		select {
		case pp.slots <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if trySlot(ctx, pp) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
