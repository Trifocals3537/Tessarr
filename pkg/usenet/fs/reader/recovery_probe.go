package reader

import (
	"context"
	"errors"
)

type recoveryProbeKey struct{}

// WithRecoveryProbe admits a bounded foreground recovery read that refreshes
// failed segments in its actual read range and does not schedule read-ahead.
// The Usenet failed-file cooldown is responsible for gating admission.
func WithRecoveryProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryProbeKey{}, true)
}

func isRecoveryProbe(ctx context.Context) bool {
	probe, _ := ctx.Value(recoveryProbeKey{}).(bool)
	return probe
}

// SegmentReadFailure identifies a logical byte whose read requires the
// unavailable segment, even if a multi-segment read returned no bytes or CBC
// needs an earlier segment for its IV.
type SegmentReadFailure struct {
	Offset int64
	cause  error
}

func (e *SegmentReadFailure) Error() string { return e.cause.Error() }
func (e *SegmentReadFailure) Unwrap() error { return e.cause }

// A unique observation belongs to one actual failed fetch, not each reader
// that later encounters the same cached or shared result.
type articleFailureObservation struct{ cause error }

func (e *articleFailureObservation) Error() string { return e.cause.Error() }
func (e *articleFailureObservation) Unwrap() error { return e.cause }

func SameArticleFailureObservation(a, b error) bool {
	var first, second *articleFailureObservation
	return errors.As(a, &first) && errors.As(b, &second) && first == second
}
