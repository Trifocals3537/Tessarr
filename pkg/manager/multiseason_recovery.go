package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

// The reference and deterministic child hash establish the fanout identity.
// Completed children can outlive a changed provider placement; their
// historical queue identity does not require revalidating the provider.
func completedSeasonChild(e *storage.Entry) bool {
	if e == nil || !e.IsTorrent() || !e.FileScopeRestricted || !e.SkipMultiSeason ||
		!e.IsComplete || e.State != storage.EntryStatePausedUP || e.SeasonSource == nil {
		return false
	}
	source := e.SeasonSource
	return source.InfoHash != "" && source.Season > 0 && source.Season < 100 &&
		e.InfoHash == generateSeasonHash(source.InfoHash, source.Season)
}

// recoverCompletedSeasonFanouts repairs the legacy outcome before intake and
// HTTP serving start. A parent is hidden only with durable season-child
// evidence at least as recent as the parent and a positively absent output
// inside the owned download root. Do not infer from titles or remove any
// files/provider data.
func (m *Manager) recoverCompletedSeasonFanouts(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	children, err := m.queue.storage.FilterQueued(completedSeasonChild)
	if err != nil {
		return err
	}
	parents := make(map[string]time.Time, len(children))
	updated := 0
	for _, snapshot := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		child, err := m.queue.GetTorrent(snapshot.InfoHash)
		if storage.IsQueuedEntryNotFound(err) || errors.Is(err, ErrQueueEntryDeleting) {
			continue
		}
		if err != nil {
			return err
		}
		if !completedSeasonChild(child) {
			continue
		}
		source := child.SeasonSource.InfoHash
		if child.CreatedAt.After(parents[source]) {
			parents[source] = child.CreatedAt
		}
		if child.Status == debridTypes.TorrentStatusDownloading {
			child.Status = debridTypes.TorrentStatusDownloaded
			if err := m.queue.Update(child); err != nil {
				return fmt.Errorf("reconcile completed season child %s: %w", child.InfoHash, err)
			}
			updated++
		}
	}
	for hash, childCreated := range parents {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent, err := m.queue.GetTorrent(hash)
		if storage.IsQueuedEntryNotFound(err) || errors.Is(err, ErrQueueEntryDeleting) {
			continue
		}
		if err != nil {
			return err
		}
		if !parent.IsTorrent() || parent.FileScopeRestricted || parent.SeasonSource != nil ||
			parent.SkipMultiSeason || !parent.IsComplete || parent.State != storage.EntryStatePausedUP ||
			parent.SeasonFanoutComplete || parent.CreatedAt.IsZero() || parent.CreatedAt.After(childCreated) {
			continue
		}
		output, err := safeTorrentEntryDownloadPath(m.config.DownloadFolder, parent)
		if err != nil || (parent.ContentPath != "" && !sameFilesystemPath(parent.ContentPath, output)) {
			continue
		}
		if info, err := os.Stat(filepath.Dir(output)); err != nil || !info.IsDir() {
			continue // An unavailable output root is not evidence of fanout.
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			continue // Preserve a real parent output, or an ambiguous lookup.
		}
		parent.SeasonFanoutComplete = true
		parent.ContentPath = ""
		if err := m.queue.Update(parent); err != nil {
			return fmt.Errorf("reconcile completed season parent %s: %w", parent.InfoHash, err)
		}
		updated++
	}
	if updated > 0 {
		if err := m.queue.Sync(); err != nil {
			return err
		}
		m.logger.Info().Int("count", updated).Msg("Reconciled completed multi-season queue records")
	}
	return ctx.Err()
}
