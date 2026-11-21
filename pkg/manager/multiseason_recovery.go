package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Trifocals3537/tessarr/internal/safepath"
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
		if !completedFanoutParentOutputAbsent(m.config.DownloadFolder, parent) {
			continue
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

// Prove absence using a pinned, existing download tree. The persisted path
// is user/provider-derived; validating its spelling alone does not protect
// a later absolute-path lookup or distinguish an unavailable parent folder.
func completedFanoutParentOutputAbsent(downloadRoot string, entry *storage.Entry) bool {
	output, err := safeTorrentEntryDownloadPath(downloadRoot, entry)
	if err != nil || (entry.ContentPath != "" && !sameFilesystemPath(entry.ContentPath, output)) {
		return false
	}
	rooted, absoluteRoot, err := safepath.OpenRoot(downloadRoot)
	if err != nil {
		return false
	}
	defer rooted.Close()
	relative, err := filepath.Rel(absoluteRoot, output)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return false
	}

	// Pin the existing save directory before checking the single release
	// component. Root-relative filesystem operations cannot escape the
	// configured directory even if an intermediate path changes.
	releaseParent := rooted
	parentRelative := filepath.Dir(relative)
	if parentRelative != "." {
		info, err := rooted.Lstat(parentRelative)
		if err != nil {
			return false
		}
		releaseParent, err = openStableLegacyDirectory(rooted, parentRelative, info)
		if err != nil {
			return false
		}
		defer releaseParent.Close()
	}
	leaf := filepath.Base(relative)
	if err := safepath.ValidateIdentifier(leaf); err != nil {
		return false
	}
	if err := rejectPortableTorrentSiblingAlias(releaseParent, ".", leaf); err != nil {
		return false
	}
	if _, err := releaseParent.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		return false // Preserve existing output and ambiguous lookups.
	}
	// A renamed, detached, or otherwise changed save directory is not
	// evidence that the visible import path is absent.
	return safepath.VerifyOpenRoot(releaseParent, filepath.Dir(output)) == nil
}
