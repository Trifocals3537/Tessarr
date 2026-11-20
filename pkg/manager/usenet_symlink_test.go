package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

func TestUsenetSymlinkPreservesForeignDestinations(t *testing.T) {
	for _, kind := range []string{"regular file", "directory", "different target", "missing marker", "different owner"} {
		t.Run(kind, func(t *testing.T) {
			m, entry, _ := newNZBCompletionTestManager(t, config.DownloadActionDownload)
			root := m.config.DownloadFolder
			target := filepath.Join(m.GetTorrentMountPath(entry), "demo.mkv")
			output := entry.DownloadPath()
			if kind == "missing marker" {
				if err := os.MkdirAll(output, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := claimUsenetEntryDirectory(root, entry); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(output, "demo.mkv")
			switch kind {
			case "regular file":
				if err := os.WriteFile(leaf, []byte("preserve existing data"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(leaf, 0o700); err != nil {
					t.Fatal(err)
				}
			case "different target":
				foreign := filepath.Join(t.TempDir(), "foreign.mkv")
				if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
				requireSymlinkCapability(t, foreign, output)
				if err := os.Symlink(foreign, leaf); err != nil {
					t.Fatal(err)
				}
			case "different owner":
				if err := os.WriteFile(filepath.Join(output, usenetOwnerMarkerName), []byte("44444444-4444-4444-8444-444444444444\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Lstat(leaf)
			if _, err := createOwnedUsenetSymlink(root, entry, "demo.mkv", target, false); err == nil {
				t.Fatal("foreign or unowned destination was accepted")
			}
			if before != nil {
				after, err := os.Lstat(leaf)
				if err != nil || !os.SameFile(before, after) {
					t.Fatal("existing destination was replaced", err)
				}
			} else if _, err := os.Lstat(leaf); !os.IsNotExist(err) {
				t.Fatal("unowned directory gained an output", err)
			}
			if kind == "regular file" {
				assertManagerFileContents(t, leaf, "preserve existing data")
			}
		})
	}
}

func TestUsenetSymlinkRejectsUnsafeNames(t *testing.T) {
	m, entry, _ := newNZBCompletionTestManager(t, config.DownloadActionDownload)
	if _, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside.mkv", `sub\outside.mkv`, "/outside.mkv", "CON", "file:stream", usenetOwnerMarkerName, "video.mkv "} {
		if _, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, name, "unused", false); err == nil {
			t.Errorf("unsafe name %q was accepted", name)
		}
	}
}

func TestUsenetSymlinkReusesEquivalentAbsoluteAndRelativeTargets(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "relative"}[relative], func(t *testing.T) {
			m, entry, _ := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
			if _, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(m.GetTorrentMountPath(entry), "demo.mkv")
			link, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, "demo.mkv", target, relative)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, "demo.mkv", target, !relative); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(link)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("matching link was replaced during recovery", err)
			}
			if _, err := safeUsenetFilePath(m.config.DownloadFolder, entry, "demo.mkv"); err == nil {
				t.Fatal("ordinary file-writing validation started accepting symlinks")
			}
		})
	}
}

func TestNZBCompletionResumesPartialSymlinksAfterStorageReopens(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
	metadata.Files = append(metadata.Files, storage.NZBFile{Name: "second.mkv", Size: 1024})
	metadata.TotalSize += 1024
	entry.AddUsenetProvider(metadata)
	entry.IsDownloading = true
	if err := m.queue.Update(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.GetTorrentMountPath(entry), "second.mkv"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry); err != nil {
		t.Fatal(err)
	}
	link, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, "demo.mkv", filepath.Join(m.GetTorrentMountPath(entry), "demo.mkv"), false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Sync(); err != nil {
		t.Fatal(err)
	}
	restored := reopenNZBCompletionStorage(t, m)
	if err := m.processNZB(m.ctx, restored, metadata); err != nil {
		t.Fatal("partial output could not resume", err)
	}
	after, err := os.Lstat(link)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("restart replaced a previously completed link", err)
	}
	if _, err := os.Stat(filepath.Join(entry.DownloadPath(), "second.mkv")); err != nil {
		t.Fatal("restart did not finish the remaining output", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStatePausedUP {
		t.Fatalf("resumed entry did not complete: %+v %v", current, err)
	}
}

func TestNZBCompletionResumedSymlinkStillRequiresReadableContent(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
	if _, _, err := claimUsenetEntryDirectory(m.config.DownloadFolder, entry); err != nil {
		t.Fatal(err)
	}
	link, err := createOwnedUsenetSymlink(m.config.DownloadFolder, entry, "demo.mkv", filepath.Join(m.GetTorrentMountPath(entry), "demo.mkv"), false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
		return &readinessTestFile{size: 1024, shortRead: true}, nil
	}}
	if err := m.processNZB(context.Background(), entry, metadata); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("recovery bypassed the content readiness gate", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateError {
		t.Fatalf("unreadable output reported success: %+v %v", current, err)
	}
	after, err := os.Lstat(link)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("failed readiness check replaced the output", err)
	}
}

func TestNZBCompletionRejectsZeroFilledSymlink(t *testing.T) {
	m, entry, metadata := newNZBCompletionTestManager(t, config.DownloadActionSymlink)
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
		return &readinessTestFile{size: 1024, zeroAll: true}, nil
	}}
	if err := m.processNZB(t.Context(), entry, metadata); !errors.Is(err, errImportReadinessZeroContent) {
		t.Fatalf("zero-filled output was admitted: %v", err)
	}
	current, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || current.State != storage.EntryStateError || current.IsComplete {
		t.Fatalf("zero-filled output reported complete: %+v %v", current, err)
	}
}
