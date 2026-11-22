package storage

import (
	"errors"
	"fmt"

	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

var ErrQueueOutcomeConflict = errors.New("queue outcome no longer owns active work")

type QueueOutcomeKind string

const (
	QueueOutcomeFailed    QueueOutcomeKind = "failed"
	QueueOutcomeDeferred  QueueOutcomeKind = "deferred"
	QueueOutcomeCompleted QueueOutcomeKind = "completed"
)

type QueueOutcome struct {
	Kind                 QueueOutcomeKind
	Err                  error
	ContentPath          string
	SeasonFanoutComplete bool
}

// CommitQueuedOutcome is a single-row durable transition boundary. It checks
// the durable incarnation under the existing lifecycle lock, mutates only
// outcome fields on a fresh snapshot, and publishes only after fsync succeeds.
// It never recreates a missing row or changes deletion/provider authorization.
// Legacy rows with no incarnation remain protected by the manager generation
// fence; they may transition only while the stored incarnation is also empty.
func (s *Storage) CommitQueuedOutcome(expected *Entry, outcome QueueOutcome) (*Entry, error) {
	if expected == nil || expected.InfoHash == "" {
		return nil, fmt.Errorf("queued outcome identity is required")
	}
	if outcome.Kind != QueueOutcomeFailed && outcome.Kind != QueueOutcomeDeferred && outcome.Kind != QueueOutcomeCompleted {
		return nil, fmt.Errorf("unknown queue outcome %q", outcome.Kind)
	}
	if outcome.Kind == QueueOutcomeFailed && outcome.Err == nil {
		return nil, fmt.Errorf("failed queue outcome requires an error")
	}
	if outcome.SeasonFanoutComplete && (outcome.Kind != QueueOutcomeCompleted || outcome.ContentPath != "") {
		return nil, fmt.Errorf("season fanout completion cannot claim an import path")
	}
	ref, err := s.acquireMainEntryState(expected.InfoHash)
	if err != nil {
		return nil, err
	}
	defer ref.release()
	ref.state.mu.Lock()
	defer ref.state.mu.Unlock()
	if ref.state.queueDeleting {
		return nil, ErrQueuedEntryDeleting
	}
	current, err := s.getQueuedRaw(ref.key)
	if err != nil {
		return nil, err
	}
	if current.QueueIncarnation != expected.QueueIncarnation || current.State != EntryStateDownloading {
		return nil, ErrQueueOutcomeConflict
	}
	switch outcome.Kind {
	case QueueOutcomeFailed:
		current.MarkAsError(outcome.Err)
	case QueueOutcomeDeferred:
		current.Status = debridTypes.TorrentStatusQueued
		current.IsDownloading = false
	case QueueOutcomeCompleted:
		if outcome.SeasonFanoutComplete && (!current.IsTorrent() || current.FileScopeRestricted ||
			current.SeasonSource != nil || current.SkipMultiSeason) {
			return nil, fmt.Errorf("invalid season fanout parent")
		}
		current.MarkAsCompleted(outcome.ContentPath)
		current.SeasonFanoutComplete = outcome.SeasonFanoutComplete
	}
	if err := s.writeQueueRecord(current, s.queue.PutExistingSynced); err != nil {
		return nil, err
	}
	return current, nil
}
