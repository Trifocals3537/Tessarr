package reader

import (
	"context"
	"errors"
	"sync/atomic"
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
type articleFailureObservation struct {
	cause   error
	claimed atomic.Bool
}

func (e *articleFailureObservation) Error() string { return e.cause.Error() }
func (e *articleFailureObservation) Unwrap() error { return e.cause }

// ClaimArticleFailureObservation returns true only for the first report of a
// shared failed fetch. Keep the claim on the observation itself, so delayed
// reports remain recognizable after other failures replace the current
// quarantine without retaining an unbounded history per file. Untagged errors
// have no shared fetch identity and are treated as separate observations.
func ClaimArticleFailureObservation(err error) bool {
	var observation *articleFailureObservation
	if !errors.As(err, &observation) {
		return true
	}
	return observation.claimed.CompareAndSwap(false, true)
}
