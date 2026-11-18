package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/Trifocals3537/tessarr/internal/workflow"
	"github.com/Trifocals3537/tessarr/pkg/notifications"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/rs/zerolog"
)

func newNZBCompletionTestManager(t *testing.T, action config.DownloadAction) (*Manager, *storage.Entry, *storage.NZB) {
	t.Helper()
	config.Reset()
	t.Cleanup(config.Reset)
	m := newQueueRecoveryTestManager(t)
	m.stopAcceptingBackgroundWork() // This fixture exercises finalization synchronously.
	m.config = config.Get()
	m.config.DownloadFolder = t.TempDir()
	m.config.Mount.MountPath = t.TempDir()
	m.config.SkipPreCache = false
	m.Notifications = notifications.New(&m.config.Notifications, zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Notifications.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	m.downloader = &Downloader{manager: m, dest: m.config.DownloadFolder, logger: zerolog.Nop()}
	entry := &storage.Entry{
		InfoHash: "33333333-3333-4333-8333-333333333333", Protocol: config.ProtocolNZB,
		Name: "Synthetic.Release", OriginalFilename: "Synthetic.Release", OutputName: "Synthetic.Release",
		Category: "tv", SavePath: filepath.Join(m.config.DownloadFolder, "tv"),
		State: storage.EntryStateDownloading, Action: action,
		ActiveProvider: "usenet", Files: make(map[string]*storage.File), SkipMultiSeason: true,
	}
	metadata := &storage.NZB{ID: entry.InfoHash, Name: entry.Name, Status: usenet.NZBStatusCompleted,
		TotalSize: 1024, Files: []storage.NZBFile{{Name: "demo.mkv", Size: 1024}}}
	entry.AddUsenetProvider(metadata)
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	mountPath := m.GetTorrentMountPath(entry)
	if err := os.MkdirAll(mountPath, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mountPath, "demo.mkv")
	if err := os.WriteFile(target, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	m.downloader.usenetDownload = func(_ context.Context, _, _ string, w io.Writer, _ func(int64, int64)) error {
		_, err := w.Write(make([]byte, 1024))
		return err
	}
	if action == config.DownloadActionSymlink {
		probe := filepath.Join(m.config.DownloadFolder, "symlink-probe")
		if err := safepath.Symlink(m.config.DownloadFolder, target, probe); err != nil {
			t.Skipf("managed symlinks unavailable: %v", err)
		}
		if _, err := os.Stat(probe); err != nil {
			_ = os.Remove(probe)
			t.Skipf("managed symlink traversal unavailable: %v", err)
		}
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
	}
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
		return &readinessTestFile{size: 1024}, nil
	}}
	return m, entry, metadata
}

func TestNZBCompletionSingleOwnerControl(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	if err := m.processNZB(context.Background(), entry, metadata); err != nil {
		t.Fatalf("single finalizer control: %v", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStatePausedUP {
		t.Fatalf("single finalizer control state=%v err=%v", current, err)
	}
}

func TestNZBCompletionResumesOwnedSymlink(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
	entry.Files["demo.mkv"] = &storage.File{Name: "demo.mkv", Size: 1024, InfoHash: entry.InfoHash}
	output, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry)
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after the legitimate action created its output, before completion.
	if _, err := m.downloader.createSymlinksWhenMountFilesAppear(context.Background(), entry,
		entry.GetActiveFiles(), m.GetTorrentMountPath(entry), output); err != nil {
		t.Fatal(err)
	}
	if err := m.processNZB(context.Background(), entry, metadata); err != nil {
		t.Fatalf("recovery rejected its own valid output instead of reconciling it: %v", err)
	}
}

func TestNZBCompletionOverlappingWorkersPublishOnce(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	active, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var downloadCalls atomic.Int32
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	m.downloader.usenetDownload = func(ctx context.Context, _, _ string, writer io.Writer, _ func(int64, int64)) error {
		if downloadCalls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, err := writer.Write(make([]byte, 1024))
		return err
	}
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- m.processNZB(ctx, recovery, metadata) }()
	select {
	case <-entered:
	case err := <-recoveryDone:
		t.Fatalf("recovery failed before output barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	activeDone := make(chan error, 1)
	go func() {
		err := m.processNZB(ctx, active, metadata)
		activeDone <- err
	}()
	var activeErr error
	select {
	case activeErr = <-activeDone:
		unblock()
	case <-time.After(100 * time.Millisecond):
		// An exclusive finalizer is allowed to wait for the current owner.
		unblock()
		activeErr = <-activeDone
	}
	recoveryErr := <-recoveryDone
	current, getErr := m.queue.GetTorrent(entry.InfoHash)
	if activeErr != nil || recoveryErr != nil || getErr != nil || current.State != storage.EntryStatePausedUP || downloadCalls.Load() != 1 {
		t.Fatalf("overlap ran post-download action %d times: active=%v recovery=%v read=%v state=%s last_error=%q",
			downloadCalls.Load(), activeErr, recoveryErr, getErr, current.State, current.LastError)
	}
}

type nzbCompletionOutcomes struct {
	store         queueOutcomeStore
	completed     atomic.Int32
	failed        atomic.Int32
	beforeFailed  func()
	completionErr error
}

func (s *nzbCompletionOutcomes) CommitQueuedOutcome(entry *storage.Entry, outcome storage.QueueOutcome) (*storage.Entry, error) {
	switch outcome.Kind {
	case storage.QueueOutcomeCompleted:
		s.completed.Add(1)
		if s.completionErr != nil {
			return nil, s.completionErr
		}
	case storage.QueueOutcomeFailed:
		s.failed.Add(1)
		if s.beforeFailed != nil {
			s.beforeFailed()
		}
	}
	return s.store.CommitQueuedOutcome(entry, outcome)
}

func TestNZBCompletionFailureIsPublishedBeforeReleasingOwnership(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, fail, commitStarted, finishCommit := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	failure := errors.New("synthetic file failure")
	m.downloader.usenetDownload = func(ctx context.Context, _, _ string, _ io.Writer, _ func(int64, int64)) error {
		if calls.Add(1) != 1 {
			return errors.New("second output action was admitted")
		}
		close(started)
		select {
		case <-fail:
			return failure
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	outcomes := &nzbCompletionOutcomes{store: m.storage, beforeFailed: func() {
		close(commitStarted)
		select {
		case <-finishCommit:
		case <-ctx.Done():
		}
	}}
	m.queue.outcomes = outcomes
	done, follower := make(chan error, 1), make(chan error, 1)
	go func() { done <- m.processNZB(ctx, entry, metadata) }()
	<-started
	go func() { follower <- m.processNZB(ctx, entry, metadata) }()
	waitForFinalizationWork(t, m.entryLifecycle, entry.InfoHash, 2)
	close(fail)
	<-commitStarted
	select {
	case err := <-follower:
		t.Fatalf("waiter escaped before failure publication: %v", err)
	default:
	}
	close(finishCommit)
	if err := <-done; !errors.Is(err, failure) || !isNZBFinalizationError(err) {
		t.Fatal("owner did not report its committed failure", err)
	}
	if err := <-follower; err != nil {
		t.Fatal("follower should observe the committed outcome", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateError || !strings.Contains(current.LastError, failure.Error()) ||
		calls.Load() != 1 || outcomes.failed.Load() != 1 || outcomes.completed.Load() != 0 {
		t.Fatalf("failure lost ownership: entry=%+v err=%v actions=%d failed=%d completed=%d", current, err, calls.Load(), outcomes.failed.Load(), outcomes.completed.Load())
	}
}

func TestNZBCompletionCancellationDoesNotPublishFailure(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	m.downloader.usenetDownload = func(ctx context.Context, _, _ string, _ io.Writer, _ func(int64, int64)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	outcomes := &nzbCompletionOutcomes{store: m.storage}
	m.queue.outcomes = outcomes
	done := make(chan error, 1)
	go func() { done <- m.processNZB(ctx, entry, metadata) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateDownloading || current.LastError != "" || outcomes.failed.Load() != 0 {
		t.Fatalf("cancellation destroyed resumable work: %+v %v", current, err)
	}
	if err := m.workflowAvailable(); err != nil {
		t.Fatal("cancellation degraded workflow persistence", err)
	}
}

func TestNZBCompletionDeletionCancelsAllWorkersAndFencesReplacement(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	var calls atomic.Int32
	m.downloader.usenetDownload = func(ctx context.Context, _, _ string, _ io.Writer, _ func(int64, int64)) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	done, follower := make(chan error, 1), make(chan error, 1)
	go func() { done <- m.processNZB(ctx, entry, metadata) }()
	<-started
	go func() { follower <- m.processNZB(ctx, entry, metadata) }()
	waitForFinalizationWork(t, m.entryLifecycle, entry.InfoHash, 2)
	if err := m.queue.Delete(entry.InfoHash, nil); err != nil {
		t.Fatal(err)
	}
	for _, result := range []error{<-done, <-follower} {
		if !errors.Is(result, context.Canceled) && !obsoleteWorkflow(result) {
			t.Fatal("deleted worker did not stop", result)
		}
	}
	if _, err := m.queue.GetTorrent(entry.InfoHash); !storage.IsQueuedEntryNotFound(err) {
		t.Fatal("deleted row was restored", err)
	}
	replacement := *entry
	replacement.QueueIncarnation = ""
	replacement.QueueGeneration = 0
	if err := m.queue.Add(&replacement); err != nil {
		t.Fatal(err)
	}
	if err := m.processNZB(ctx, entry, metadata); !errors.Is(err, ErrStaleQueueGeneration) {
		t.Fatal("old worker reached replacement", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateDownloading || current.LastError != "" || calls.Load() != 1 || current.QueueIncarnation == entry.QueueIncarnation {
		t.Fatalf("replacement was altered: %+v err=%v calls=%d", current, err, calls.Load())
	}
}

func TestNZBCompletionIgnoresStalePayloadAndInactiveRows(t *testing.T) {
	for _, state := range []storage.TorrentState{storage.EntryStatePausedDL, storage.EntryStatePausedUP, storage.EntryStateError} {
		t.Run(string(state), func(t *testing.T) {
			m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
			current, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			current.State = state
			if err := m.queue.Update(current); err != nil {
				t.Fatal(err)
			}
			m.downloader.usenetDownload = func(context.Context, string, string, io.Writer, func(int64, int64)) error {
				t.Error("inactive job ran an output action")
				return nil
			}
			if err := m.processNZB(context.Background(), entry, metadata); err != nil {
				t.Fatal(err)
			}
			current, err = m.queue.GetTorrent(entry.InfoHash)
			if err != nil || current.State != state {
				t.Fatalf("inactive state changed: %+v %v", current, err)
			}
		})
	}
	t.Run("use current path and action", func(t *testing.T) {
		m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
		stale := *entry
		stale.Name = "../untrusted"
		stale.SavePath = t.TempDir()
		stale.Action = config.DownloadActionNone
		if err := m.processNZB(context.Background(), &stale, metadata); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(filepath.Join(entry.DownloadPath(), "demo.mkv"))
		if err != nil || len(contents) != 1024 {
			t.Fatalf("current output was not used: bytes=%d err=%v", len(contents), err)
		}
	})
}

func TestNZBCompletionRejectsMismatchedMetadata(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	metadata.ID = "44444444-4444-4444-8444-444444444444"
	if err := m.processNZB(context.Background(), entry, metadata); err == nil {
		t.Fatal("mismatched source was accepted")
	}
	if _, err := os.Stat(entry.DownloadPath()); !os.IsNotExist(err) {
		t.Fatal("mismatched source created output", err)
	}
}

func TestNZBCompletionPanicFailsOnceWithoutLeakingItsPayload(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	outcomes := &nzbCompletionOutcomes{store: m.storage}
	m.queue.outcomes = outcomes
	var downloadCalls atomic.Int32
	m.downloader.usenetDownload = func(context.Context, string, string, io.Writer, func(int64, int64)) error {
		downloadCalls.Add(1)
		panic("private-provider-url")
	}
	if err := m.processNZB(context.Background(), entry, metadata); !errors.Is(err, workflow.ErrPanicked) {
		t.Fatal("panic escaped its owner", err)
	}
	if err := m.processNZB(context.Background(), entry, metadata); err != nil {
		t.Fatal(err)
	}
	if got := downloadCalls.Load(); got != 1 {
		t.Fatalf("download panic fixture invoked %d times, want 1", got)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateError || current.LastError != workflow.ErrPanicked.Error() || outcomes.failed.Load() != 1 {
		t.Fatalf("panic did not publish one safe failure: %+v %v commits=%d", current, err, outcomes.failed.Load())
	}
	if err := m.workflowAvailable(); err != nil {
		t.Fatal("durably handled panic degraded persistence", err)
	}
}

func reopenNZBCompletionStorage(t *testing.T, m *Manager) *storage.Entry {
	t.Helper()
	if err := m.storage.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStorage(filepath.Join(config.GetMainPath(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	m.cancel()
	m.storage = store
	m.entryLifecycle = newEntryLifecycle()
	m.queue = newLifecycleTestQueue(store, m.entryLifecycle)
	m.resetLifecycle()
	m.stopAcceptingBackgroundWork()
	if err := m.recoverInterruptedDownloads(m.ctx); err != nil {
		t.Fatal(err)
	}
	entry, err := m.queue.GetTorrent("33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestNZBCompletionFailedCommitPreservesWorkUntilStorageReopens(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	var calls atomic.Int32
	m.downloader.usenetDownload = func(_ context.Context, _, _ string, w io.Writer, _ func(int64, int64)) error {
		calls.Add(1)
		_, err := w.Write(make([]byte, 1024))
		return err
	}
	failure := errors.New("synthetic completion fsync failure")
	outcomes := &nzbCompletionOutcomes{store: m.storage, completionErr: failure}
	m.queue.outcomes = outcomes
	if err := m.processNZB(context.Background(), entry, metadata); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := m.processNZB(context.Background(), entry, metadata); !errors.Is(err, ErrWorkflowUnavailable) {
		t.Fatal("failed commit was retried in-process", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateDownloading || current.LastError != "" || calls.Load() != 1 || outcomes.failed.Load() != 0 {
		t.Fatalf("failed commit was converted to an error row: %+v %v", current, err)
	}
	restored := reopenNZBCompletionStorage(t, m)
	if restored.IsDownloading {
		t.Fatal("restart did not clear abandoned local work")
	}
	if err := m.processNZB(m.ctx, restored, metadata); err != nil {
		t.Fatal("restart failed to resume preserved work", err)
	}
	current, err = m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStatePausedUP || calls.Load() != 2 {
		t.Fatalf("reopened work did not complete: %+v %v calls=%d", current, err, calls.Load())
	}
}

func TestNZBCompletionBackgroundCallerDoesNotPublishFailureTwice(t *testing.T) {
	m, entry, _ := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	m.backgroundStopping = false
	outcomes := &nzbCompletionOutcomes{store: m.storage}
	m.queue.outcomes = outcomes
	failure := fmt.Errorf("owned finalization failed")
	if !m.startEntryBackground(m.ctx, "NZB failure", entry, func(ctx context.Context) error {
		return m.runNZBFinalization(ctx, entry, func(context.Context, *storage.Entry) error { return failure })
	}) {
		t.Fatal("background work was not admitted")
	}
	if err := m.waitForBackground(); err != nil {
		t.Fatal(err)
	}
	if outcomes.failed.Load() != 1 {
		t.Fatalf("caller republished the finalizer's failure %d times", outcomes.failed.Load())
	}
}

func attachNZBCompletionMetadata(t *testing.T, m *Manager, metadata *storage.NZB) {
	t.Helper()
	// Header inspection and completion use only local synthetic data. The NNTP
	// pool is lazy and no download/parse request is sent to this unused endpoint.
	m.config.Usenet.Providers = []config.UsenetProvider{{Host: "127.0.0.1", Port: 1, MaxConnections: 1}}
	m.config.Usenet.DiskBufferPath = t.TempDir()
	u, err := usenet.New()
	if err != nil {
		t.Fatal(err)
	}
	m.usenet = u
	t.Cleanup(func() {
		if err := u.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := u.NZBStorage().AddNZB(metadata); err != nil {
		t.Fatal(err)
	}
}

func TestNZBCompletionActiveWorkerAndSchedulerShareOwnership(t *testing.T) {
	for _, schedulerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "active first", true: "scheduler first"}[schedulerFirst], func(t *testing.T) {
			m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
			attachNZBCompletionMetadata(t, m, metadata)
			var notificationsSent atomic.Int32
			notifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				notificationsSent.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(notifyServer.Close)
			if err := m.Notifications.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			m.config.Notifications = config.Notifications{Enabled: true, CallbackURL: notifyServer.URL}
			m.Notifications = notifications.New(&m.config.Notifications, zerolog.Nop())
			m.entry = NewEntryCache(m)
			m.backgroundStopping = false
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			m.downloader.usenetDownload = func(ctx context.Context, _, _ string, w io.Writer, _ func(int64, int64)) error {
				if calls.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				_, err := w.Write(make([]byte, 1024))
				return err
			}
			outcomes := &nzbCompletionOutcomes{store: m.storage}
			m.queue.outcomes = outcomes
			staleSchedulerSnapshot, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			startActive := func() { go func() { done <- m.processNZB(ctx, entry, metadata) }() }
			if schedulerFirst {
				m.processQueuedEntries(ctx)
			} else {
				startActive()
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if schedulerFirst {
				startActive()
			} else {
				// A new tick correctly skips IsDownloading. Reproduce the
				// scheduler snapshot captured before the active action set it.
				m.processingEntries.Store(entry.InfoHash, struct{}{})
				if !m.startEntryBackground(ctx, "queued NZB processing", staleSchedulerSnapshot, func(workCtx context.Context) error {
					return m.processQueuedNZB(workCtx, staleSchedulerSnapshot)
				}) {
					t.Fatal("stale scheduler dispatch was not admitted")
				}
			}
			// One owner plus the scheduler's lifecycle lease and the other
			// completion attempt are all present at the deterministic barrier.
			waitForFinalizationWork(t, m.entryLifecycle, entry.InfoHash, 3)
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := m.waitForBackground(); err != nil {
				t.Fatal(err)
			}
			if err := m.Notifications.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil || current.State != storage.EntryStatePausedUP || calls.Load() != 1 || outcomes.completed.Load() != 1 || outcomes.failed.Load() != 0 || notificationsSent.Load() != 1 {
				t.Fatalf("worker/scheduler overlap: entry=%+v err=%v actions=%d completions=%d failures=%d notifications=%d", current, err, calls.Load(), outcomes.completed.Load(), outcomes.failed.Load(), notificationsSent.Load())
			}
		})
	}
}

func TestNZBCompletionFencesIncarnationAndRequiresCurrentSource(t *testing.T) {
	for _, scenario := range []string{"different incarnation", "unbound generation", "different placement", "empty metadata"} {
		t.Run(scenario, func(t *testing.T) {
			m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
			expected := *entry
			switch scenario {
			case "different incarnation":
				expected.QueueIncarnation = "obsolete-incarnation"
			case "unbound generation":
				expected.QueueGeneration = 0
			case "different placement":
				current, err := m.queue.GetTorrent(entry.InfoHash)
				if err != nil {
					t.Fatal(err)
				}
				current.Providers["usenet"].ID = "different-source"
				if err := m.queue.Update(current); err != nil {
					t.Fatal(err)
				}
			case "empty metadata":
				metadata.Files = nil
			}
			if err := m.processNZB(m.ctx, &expected, metadata); err == nil {
				t.Fatal("invalid ownership or source was accepted")
			}
			if _, err := os.Stat(entry.DownloadPath()); !os.IsNotExist(err) {
				t.Fatal("invalid ownership or source created output", err)
			}
			if err := m.workflowAvailable(); err != nil {
				t.Fatal("rejected source degraded persistence", err)
			}
		})
	}
}

func TestNZBCompletionNoneActionDeletesOnlyAfterItsLeasesClose(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionNone)
	m.entry = NewEntryCache(m)
	m.backgroundStopping = false
	if !m.startEntryBackground(m.ctx, "NZB none action", entry, func(ctx context.Context) error { return m.processNZB(ctx, entry, metadata) }) {
		t.Fatal("none action was not admitted")
	}
	if err := m.waitForBackground(); err != nil {
		t.Fatal("none action deadlocked while deleting its own queue entry", err)
	}
	if _, err := m.queue.GetTorrent(entry.InfoHash); !storage.IsQueuedEntryNotFound(err) {
		t.Fatal("none action did not remove its queue entry", err)
	}
}

func TestNZBCompletionSchedulerDoesNotOverwriteNewerSuccessWithMissingMetadata(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	attachNZBCompletionMetadata(t, m, metadata)
	outcomes := &nzbCompletionOutcomes{store: m.storage}
	m.queue.outcomes = outcomes
	if err := m.processNZB(m.ctx, entry, metadata); err != nil {
		t.Fatal(err)
	}
	if err := m.usenet.NZBStorage().DeleteNZB(entry.InfoHash); err != nil {
		t.Fatal(err)
	}
	if err := m.processQueuedNZB(m.ctx, entry); err != nil {
		t.Fatal("stale scheduler inspected missing metadata after completion", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStatePausedUP || outcomes.failed.Load() != 0 || outcomes.completed.Load() != 1 {
		t.Fatalf("stale scheduler spoiled success: %+v %v", current, err)
	}
}
