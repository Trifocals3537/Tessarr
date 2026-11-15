package manager

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
)

func TestNZBProcessingFailureIsPublishedOnceUnderOwnership(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	metadata.Status = usenet.NZBStatusFailed
	attachNZBCompletionMetadata(t, m, metadata)
	m.usenetTimeout = 5 * time.Second
	outcomes := &nzbCompletionOutcomes{store: m.storage}
	m.queue.outcomes = outcomes
	m.processJob(m.ctx, &Job{ID: entry.InfoHash, Type: JobTypeNZB, Entry: entry, NZBMeta: metadata})
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateError || outcomes.failed.Load() != 1 || outcomes.completed.Load() != 0 {
		t.Fatalf("genuine processing failure was lost or duplicated: entry=%+v err=%v failures=%d", current, err, outcomes.failed.Load())
	}
}

func TestNZBProcessingErrorCannotRacePublishedCompletion(t *testing.T) {
	for _, schedulerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker reconciles", true: "scheduler owns output"}[schedulerFirst], func(t *testing.T) {
			m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
			attachNZBCompletionMetadata(t, m, metadata)
			m.usenetTimeout = 5 * time.Second
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			defer cancel()
			outcomes := &nzbCompletionOutcomes{store: m.storage}
			m.queue.outcomes = outcomes
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			m.downloader.usenetDownload = func(ctx context.Context, _, _ string, w io.Writer, _ func(int64, int64)) error {
				if schedulerFirst {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				_, err := w.Write(make([]byte, 1024))
				return err
			}
			schedulerDone := make(chan error, 1)
			if schedulerFirst {
				go func() { schedulerDone <- m.processQueuedNZB(ctx, entry) }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			// Exercise the real active-job error path. Empty parse groups fail
			// locally. An invalid stale source path prevents that obsolete parse
			// from modifying the completion already published in NZB storage.
			stale := *metadata
			stale.Path = filepath.Join(t.TempDir(), "foreign.nzb")
			job := &Job{ID: entry.InfoHash, Type: JobTypeNZB, Entry: entry, NZBMeta: &stale}
			workerDone := make(chan struct{})
			go func() { m.processJob(ctx, job); close(workerDone) }()
			if schedulerFirst {
				select {
				case <-workerDone:
				case <-time.After(100 * time.Millisecond):
					waitForFinalizationWork(t, m.entryLifecycle, entry.InfoHash, 2)
				}
				unblock()
				if err := <-schedulerDone; err != nil {
					t.Error("scheduler lost its completion", err)
				}
			}
			select {
			case <-workerDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil || current.State != storage.EntryStatePausedUP || outcomes.completed.Load() != 1 || outcomes.failed.Load() != 0 {
				t.Fatalf("processing error overwrote published completion: entry=%+v err=%v completions=%d failures=%d", current, err, outcomes.completed.Load(), outcomes.failed.Load())
			}
		})
	}
}

func TestSymlinkTargetComparisonRejectsNonCleanTraversal(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "release", "demo.mkv")
	target := filepath.Join(base, "mount", "demo.mkv")
	separator := string(filepath.Separator)
	for _, actual := range []string{
		base + separator + "hop" + separator + ".." + separator + "mount" + separator + "demo.mkv",
		".." + separator + "hop" + separator + ".." + separator + "mount" + separator + "demo.mkv",
	} {
		if sameSymlinkTarget(link, actual, target) {
			t.Errorf("non-clean target accepted: %q", actual)
		}
	}
}

func TestUsenetSymlinkRejectsTraversalThroughForeignDirectory(t *testing.T) {
	m, entry, _ := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
	if _, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(m.GetTorrentMountPath(entry), "demo.mkv")
	base := filepath.Dir(m.GetTorrentMountPath(entry))
	foreign := t.TempDir()
	if err := os.MkdirAll(filepath.Join(foreign, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreignTarget := filepath.Join(foreign, filepath.Base(m.GetTorrentMountPath(entry)), "demo.mkv")
	if err := os.MkdirAll(filepath.Dir(foreignTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignTarget, []byte("preserve foreign data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreign, "nested"), filepath.Join(base, "hop")); err != nil {
		t.Fatal(err)
	}
	separator := string(filepath.Separator)
	actual := base + separator + "hop" + separator + ".." + separator + filepath.Base(m.GetTorrentMountPath(entry)) + separator + "demo.mkv"
	link := filepath.Join(entry.DownloadPath(), "demo.mkv")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	assertManagerFileContents(t, link, "preserve foreign data")
	if _, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, "demo.mkv", target, false); err == nil {
		t.Fatal("accepted a link that resolves through a foreign directory")
	}
	after, err := os.Lstat(link)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("rejected link was replaced", err)
	}
	assertManagerFileContents(t, foreignTarget, "preserve foreign data")
}
