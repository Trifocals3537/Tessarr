package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/rs/zerolog"
)

// The child exits without running deferred cleanup after its output is
// durable, before the queue completion commit. Reopening a gracefully closed
// store alone would not exercise this crash boundary or release its OS locks.
func TestNZBCompletionResumesAfterProcessExit(t *testing.T) {
	const crashExit = 78
	if root := os.Getenv("TESSARR_TEST_NZB_CRASH_ROOT"); root != "" {
		config.Reset()
		config.SetConfigPath(root)
		cfg := config.Get()
		cfg.DownloadFolder = os.Getenv("TESSARR_TEST_NZB_CRASH_DOWNLOAD")
		cfg.Mount.MountPath = os.Getenv("TESSARR_TEST_NZB_CRASH_MOUNT")
		cfg.SkipPreCache = false
		store, err := storage.NewStorage(filepath.Join(root, "db"))
		if err != nil {
			t.Fatal(err)
		}
		lifecycle := newEntryLifecycle()
		m := &Manager{storage: store, queue: newLifecycleTestQueue(store, lifecycle), entryLifecycle: lifecycle, config: cfg, logger: zerolog.Nop()}
		m.resetLifecycle()
		m.stopAcceptingBackgroundWork()
		m.downloader = &Downloader{manager: m, dest: cfg.DownloadFolder, logger: zerolog.Nop()}
		m.downloader.usenetDownload = func(_ context.Context, _, _ string, writer io.Writer, _ func(int64, int64)) error {
			if _, err := writer.Write(make([]byte, 1024)); err != nil {
				return err
			}
			if err := writer.(*os.File).Sync(); err != nil {
				return err
			}
			if err := m.queue.Sync(); err != nil {
				return err
			}
			os.Exit(crashExit)
			return nil
		}
		m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
			if err := m.queue.Sync(); err != nil {
				return nil, err
			}
			os.Exit(crashExit)
			return nil, nil
		}}
		entry, err := m.queue.GetTorrent("33333333-3333-4333-8333-333333333333")
		if err != nil {
			t.Fatal(err)
		}
		metadata := &storage.NZB{ID: entry.InfoHash, Name: entry.Name, Status: usenet.NZBStatusCompleted, TotalSize: 1024, Files: []storage.NZBFile{{Name: "demo.mkv", Size: 1024}}}
		if err := m.processNZB(m.ctx, entry, metadata); err != nil {
			t.Fatal(err)
		}
		t.Fatal("child did not reach the crash boundary")
	}
	action := config.DownloadActionSymlink
	if runtime.GOOS == "windows" {
		action = config.DownloadActionDownload
	}
	m, entry, metadata := newNZBCompletionTestManager(t, action)
	if err := m.storage.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestNZBCompletionResumesAfterProcessExit$", "-test.timeout=8s")
	child.Env = append(os.Environ(),
		"TESSARR_TEST_NZB_CRASH_ROOT="+config.GetMainPath(),
		"TESSARR_TEST_NZB_CRASH_DOWNLOAD="+m.config.DownloadFolder,
		"TESSARR_TEST_NZB_CRASH_MOUNT="+m.config.Mount.MountPath)
	output, err := child.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != crashExit {
		t.Fatalf("child did not exit at the intended boundary: %v\n%s", err, output)
	}
	link := filepath.Join(entry.DownloadPath(), "demo.mkv")
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("crashed child did not leave its owned output: %v", err)
	}
	if action == config.DownloadActionSymlink && before.Mode()&os.ModeSymlink == 0 {
		t.Fatal("crashed symlink action did not leave a link")
	}
	restored := reopenNZBCompletionStorage(t, m)
	if restored.State != storage.EntryStateDownloading || restored.IsDownloading {
		t.Fatalf("crash did not leave resumable queue state: %+v", restored)
	}
	if err := m.processNZB(m.ctx, restored, metadata); err != nil {
		t.Fatal("crashed work did not resume", err)
	}
	after, err := os.Lstat(link)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("crash recovery replaced the completed output", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStatePausedUP || current.LastError != "" {
		t.Fatalf("crash recovery did not complete: %+v %v", current, err)
	}
}
