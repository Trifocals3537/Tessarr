package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/Trifocals3537/tessarr/internal/workflow"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

// The finalizer publishes failures before releasing its exclusive lease.
// Callers must not publish the same failure again after another worker has
// acquired the lease, completed the entry, or observed a replacement row.
type nzbFinalizationError struct{ err error }

func (e *nzbFinalizationError) Error() string { return e.err.Error() }
func (e *nzbFinalizationError) Unwrap() error { return e.err }

func isNZBFinalizationError(err error) bool {
	_, ok := errors.AsType[*nzbFinalizationError](err)
	return ok
}

// runNZBFinalization is shared by active jobs and scheduler recovery. The
// durable row is reloaded after acquiring ownership; a waiting snapshot never
// restores a completed/error state to downloading or acts on a new incarnation.
func (m *Manager) runNZBFinalization(ctx context.Context, expected *storage.Entry, work func(context.Context, *storage.Entry) error) error {
	handled := func(err error) error { return &nzbFinalizationError{err: err} }
	if expected == nil || !expected.IsNZB() || work == nil {
		return handled(fmt.Errorf("NZB finalization requires a queued NZB and work"))
	}
	lease, err := m.queue.lifecycle.startFinalization(ctx, expected.InfoHash, expected.QueueGeneration)
	if err != nil {
		return handled(err)
	}
	defer lease.Close()
	ctx = lease.Context()
	current, err := m.queue.GetTorrent(expected.InfoHash)
	if err != nil {
		if !obsoleteWorkflow(err) {
			m.recordWorkflowFault(err)
		}
		return handled(err)
	}
	if current.QueueGeneration != expected.QueueGeneration ||
		current.QueueIncarnation != expected.QueueIncarnation || !current.IsNZB() {
		return handled(storage.ErrQueueOutcomeConflict)
	}
	if current.State != storage.EntryStateDownloading {
		return nil
	}
	if err := m.workflowAvailable(); err != nil {
		return handled(err)
	}
	outcome := workflow.Run(ctx, func(workCtx context.Context) error { return work(workCtx, current) })
	if outcome.Kind == workflow.Panicked {
		m.logger.Error().Str("entry", current.InfoHash).Bytes("stack", outcome.Stack).Msg("NZB finalization panicked")
	}
	if outcome.Err == nil || errors.Is(outcome.Err, errDeleteQueueEntryOnJobFinish) {
		return outcome.Err // Queue deletion waits until this lease has closed.
	}
	if outcome.Kind == workflow.Canceled || obsoleteWorkflow(outcome.Err) {
		return handled(outcome.Err)
	}
	// A failed fsync may have committed part of the operation. Leave the
	// downloading row recoverable and keep the existing admission fault latched.
	if err := m.workflowAvailable(); err != nil {
		return handled(errors.Join(outcome.Err, err))
	}
	if err := m.queue.commitOutcome(current, storage.QueueOutcome{Kind: storage.QueueOutcomeFailed, Err: outcome.Err}); err != nil {
		m.recordWorkflowFault(err)
		return handled(errors.Join(outcome.Err, fmt.Errorf("persist NZB finalization failure: %w", err)))
	}
	m.logger.Error().Err(outcome.Err).Str("name", current.Name).Msg("NZB finalization failed")
	return handled(outcome.Err)
}
