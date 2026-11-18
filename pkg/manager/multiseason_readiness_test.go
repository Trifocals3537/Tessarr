package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/safepath"
	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/Trifocals3537/tessarr/pkg/notifications"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/rs/zerolog"
)

type seasonDiskReadinessFile struct {
	*os.File
	size int64
}

func (f *seasonDiskReadinessFile) Size() int64 { return f.size }
func (f *seasonDiskReadinessFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return f.ReadAt(p, off)
}

func newMultiSeasonReadinessManager(t *testing.T) (*Manager, *storage.Entry) {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	m := newQueueRecoveryTestManager(t)
	m.stopAcceptingBackgroundWork()
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
	parent := readinessTestEntry(m.config.DownloadFolder, map[string]int64{
		"Demo.S01E01.mkv": 1024,
		"Demo.S02E01.mkv": 2048,
	})
	parent.InfoHash = strings.Repeat("1", 40)
	parent.Name, parent.OriginalFilename = "Demo.S01E01.1080p", "Demo.Complete.Series"
	parent.OutputName = storage.NewTorrentOutputName(parent.Name, parent.InfoHash)
	parent.Action = config.DownloadActionSymlink
	parent.State, parent.Status = storage.EntryStateDownloading, debridTypes.TorrentStatusDownloaded
	for _, file := range parent.Files {
		file.InfoHash = parent.InfoHash
	}
	if err := m.storage.AddOrUpdate(parent); err != nil {
		t.Fatal(err)
	}
	var err error
	parent, err = m.storage.Get(parent.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Add(parent); err != nil {
		t.Fatal(err)
	}
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(_ context.Context, path string) (CacheWarmFile, error) {
		file := parent.Files[filepath.Base(path)]
		if file == nil {
			return nil, fmt.Errorf("unexpected synthetic file %q", filepath.Base(path))
		}
		return &readinessTestFile{size: file.Size}, nil
	}}
	return m, parent
}

func prepareMultiSeasonSymlinkMount(t *testing.T, m *Manager, parent *storage.Entry) {
	t.Helper()
	mount := m.GetTorrentMountPath(parent)
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	var target string
	for _, file := range parent.Files {
		target = filepath.Join(mount, file.Name)
		if err := os.WriteFile(target, make([]byte, int(file.Size)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(m.config.DownloadFolder, "symlink-probe")
	if err := safepath.Symlink(m.config.DownloadFolder, target, probe); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatalf("managed symlink fixture must work on Linux: %v", err)
		}
		t.Skipf("managed symlinks unavailable: %v", err)
	}
	if _, err := os.Stat(probe); err != nil {
		_ = os.Remove(probe)
		if runtime.GOOS != "windows" {
			t.Fatalf("managed symlink traversal must work on Linux: %v", err)
		}
		t.Skipf("managed symlink traversal unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	// Integration probes traverse the actual generated symlink and read the
	// synthetic mounted file. Unit tests above use controlled failure handles.
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(_ context.Context, path string) (CacheWarmFile, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		return &seasonDiskReadinessFile{File: file, size: info.Size()}, nil
	}}
}

func TestMultiSeasonReadinessFanout(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	prepareMultiSeasonSymlinkMount(t, m, parent)
	if err := m.downloader.download(context.Background(), parent); err != nil {
		t.Fatalf("multi-season import: %v", err)
	}
	for season := 1; season <= 2; season++ {
		child, err := m.queue.GetTorrent(generateSeasonHash(parent.InfoHash, season))
		if err != nil {
			t.Fatal(err)
		}
		if !child.IsComplete || child.LastError != "" {
			t.Errorf("season %d incomplete: state=%s error=%s", season, child.State, child.LastError)
		}
		if child.SeasonSource == nil || child.SeasonSource.InfoHash != parent.InfoHash || child.SeasonSource.Season != season {
			t.Fatalf("season %d lost durable source reference: %+v", season, child.SeasonSource)
		}
		if len(child.Files) != 1 || strings.Contains(child.Name, "E01") || !strings.Contains(child.Name, fmt.Sprintf("S%02d", season)) {
			t.Fatalf("season %d has wrong scope or name: %+v", season, child)
		}
		for _, file := range child.Files {
			output := filepath.Join(child.DownloadPath(), file.Path)
			stored, err := os.Readlink(output)
			if err != nil || !sameSymlinkTarget(output, stored, filepath.Join(m.GetTorrentMountPath(parent), file.Path)) {
				t.Fatalf("season output target %q: %v", stored, err)
			}
		}
		if _, err := m.GetEntry(child.InfoHash); !errors.Is(err, storage.ErrEntryNotFound) {
			t.Fatalf("synthetic child published to main storage: %v", err)
		}
	}
	current, err := m.queue.GetTorrent(parent.InfoHash)
	if err != nil || !current.IsComplete || current.State != storage.EntryStatePausedUP {
		t.Fatalf("successful parent outcome: %+v, %v", current, err)
	}
}

func TestMultiSeasonChildrenRetainCanonicalFileIdentity(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	_, seasons := m.downloader.detectMultiSeason(parent)
	for _, child := range convertToMultiSeason(parent, seasons) {
		for name, file := range child.Files {
			if file.ID == "" || file.ID != parent.Files[name].ID {
				t.Errorf("season file %q lost canonical identity: got %q, want %q", name, file.ID, parent.Files[name].ID)
			}
		}
	}
}

func TestMultiSeasonReadinessScopedChild(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	_, seasons := m.downloader.detectMultiSeason(parent)
	for _, child := range convertToMultiSeason(parent, seasons) {
		var probes []importReadinessFile
		for _, file := range child.Files {
			probes = append(probes, importReadinessFile{path: file.Name, logicalName: file.Name, expectedSize: file.Size})
		}
		if err := m.verifyImportReadiness(context.Background(), child, probes); err != nil {
			t.Fatalf("scoped season readiness: %v", err)
		}
	}
}

func TestMultiSeasonParentRetainsFailure(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	// Force a path validation failure before mount I/O, even on hosts where
	// symlinks are unavailable. A child failure must reach the parent caller.
	parent.SavePath = filepath.Join(t.TempDir(), "outside-download-root")
	if err := m.downloader.download(context.Background(), parent); err == nil || !strings.Contains(err.Error(), "multi-season import failed") {
		t.Fatal("parent reported success after every child failed")
	}
	current, err := m.queue.GetTorrent(parent.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if current.IsComplete || current.State == storage.EntryStatePausedUP {
		t.Fatal("parent was completed despite failed season actions")
	}
}

func seasonReadinessChild(t *testing.T, m *Manager, parent *storage.Entry) (*storage.Entry, []importReadinessFile) {
	t.Helper()
	_, seasons := m.downloader.detectMultiSeason(parent)
	child := convertToMultiSeason(parent, seasons)[0]
	var probes []importReadinessFile
	for _, file := range child.Files {
		probes = append(probes, importReadinessFile{path: file.Name, logicalName: file.Name, expectedSize: file.Size})
	}
	return child, probes
}

func TestMultiSeasonReadinessSourceGuards(t *testing.T) {
	tests := map[string]func(*storage.Entry){
		"legacy missing source": func(e *storage.Entry) { e.SeasonSource = nil },
		"wrong source":          func(e *storage.Entry) { e.SeasonSource.InfoHash = strings.Repeat("2", 40) },
		"missing parent": func(e *storage.Entry) {
			e.SeasonSource.InfoHash = strings.Repeat("2", 40)
			e.InfoHash = generateSeasonHash(e.SeasonSource.InfoHash, e.SeasonSource.Season)
		},
		"invalid season":         func(e *storage.Entry) { e.SeasonSource.Season = 0 },
		"wrong child identity":   func(e *storage.Entry) { e.InfoHash = "unrelated" },
		"unrestricted child":     func(e *storage.Entry) { e.FileScopeRestricted = false },
		"recursive child":        func(e *storage.Entry) { e.SkipMultiSeason = false },
		"wrong protocol":         func(e *storage.Entry) { e.Protocol = config.ProtocolNZB },
		"empty scope":            func(e *storage.Entry) { e.Files = nil },
		"nil canonical file":     func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"] = nil },
		"deleted canonical file": func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"].Deleted = true },
		"changed canonical ID":   func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"].ID = "replacement" },
		"changed local path":     func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"].Path = "other.mkv" },
		"changed size":           func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"].Size++ },
		"changed byte range":     func(e *storage.Entry) { e.Files["Demo.S01E01.mkv"].ByteRange = &[2]int64{1, 1024} },
		"sibling injected": func(e *storage.Entry) {
			e.Files["Demo.S02E01.mkv"] = &storage.File{Name: "Demo.S02E01.mkv", Size: 2048}
		},
		"changed provider":      func(e *storage.Entry) { e.ActiveProvider = "other" },
		"changed placement":     func(e *storage.Entry) { e.GetActiveProvider().ID = "other" },
		"missing provider file": func(e *storage.Entry) { delete(e.GetActiveProvider().Files, "Demo.S01E01.mkv") },
		"changed provider ID":   func(e *storage.Entry) { e.GetActiveProvider().Files["Demo.S01E01.mkv"].Id = "other" },
		"changed provider path": func(e *storage.Entry) { e.GetActiveProvider().Files["Demo.S01E01.mkv"].Path = "other" },
		"changed provider link": func(e *storage.Entry) { e.GetActiveProvider().Files["Demo.S01E01.mkv"].Link = "other" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m, parent := newMultiSeasonReadinessManager(t)
			child, probes := seasonReadinessChild(t, m, parent)
			mutate(child)
			var opens atomic.Int32
			m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
				opens.Add(1)
				return &readinessTestFile{size: 1024}, nil
			}}
			if err := m.verifyImportReadiness(context.Background(), child, probes); err == nil {
				t.Fatal("accepted invalid season source/scope")
			}
			if opens.Load() != 0 {
				t.Fatal("invalid season source reached mount I/O")
			}
		})
	}
}

func TestMultiSeasonReadinessDetectsSourceChanges(t *testing.T) {
	for _, change := range []string{"delete", "placement", "provider file", "canonical file", "identical republish"} {
		t.Run(change, func(t *testing.T) {
			m, parent := newMultiSeasonReadinessManager(t)
			child, probes := seasonReadinessChild(t, m, parent)
			var once sync.Once
			m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) {
				once.Do(func() {
					current, err := m.GetEntry(parent.InfoHash)
					if err != nil {
						t.Error(err)
						return
					}
					if change == "delete" {
						if err := m.storage.DeleteSnapshot(current); err != nil {
							t.Error(err)
						}
						return
					}
					switch change {
					case "placement":
						current.GetActiveProvider().ID = "replacement"
					case "provider file":
						current.GetActiveProvider().Files["Demo.S01E01.mkv"].Path = "replacement"
					case "canonical file":
						current.Files["Demo.S01E01.mkv"].Size++
					}
					if err := m.storage.AddOrUpdate(current); err != nil {
						t.Error(err)
					}
				})
				return &readinessTestFile{size: 1024}, nil
			}}
			if err := m.verifyImportReadiness(context.Background(), child, probes); err == nil {
				t.Fatal("accepted a source deleted, replaced or changed during verification")
			}
		})
	}
}

func TestMultiSeasonReadinessCancellation(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	child, probes := seasonReadinessChild(t, m, parent)
	blocked := &canceledReadinessFile{started: make(chan struct{})}
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(context.Context, string) (CacheWarmFile, error) { return blocked, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.verifyImportReadiness(ctx, child, probes) }()
	select {
	case <-blocked.started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled probe: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled probe did not stop")
	}
}

func TestMultiSeasonReadinessFanoutRetainsFailedParent(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	prepareMultiSeasonSymlinkMount(t, m, parent)
	m.mountManager = &testCacheWarmMount{ready: true, opener: func(_ context.Context, path string) (CacheWarmFile, error) {
		file := parent.Files[filepath.Base(path)]
		return &readinessTestFile{size: file.Size, shortRead: strings.Contains(path, "S01E01")}, nil
	}}
	if err := m.downloader.download(context.Background(), parent); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("parent lost child read failure: %v", err)
	}
	for season := 1; season <= 2; season++ {
		child, err := m.queue.GetTorrent(generateSeasonHash(parent.InfoHash, season))
		if err != nil {
			t.Fatal(err)
		}
		if child.IsComplete != (season == 2) || (season == 1 && child.LastError == "") {
			t.Fatalf("incorrect season %d outcome: %+v", season, child)
		}
	}
	current, err := m.queue.GetTorrent(parent.InfoHash)
	if err != nil || current.IsComplete || current.State == storage.EntryStatePausedUP {
		t.Fatalf("failed parent marked complete: %+v, %v", current, err)
	}
}

func TestMultiSeasonReadinessRetryUsesParentMount(t *testing.T) {
	for _, naming := range []config.WebDavFolderNaming{config.WebDavUseFileName, config.WebDavUseOriginalName, config.WebdavUseHash} {
		t.Run(string(naming), func(t *testing.T) {
			m, parent := newMultiSeasonReadinessManager(t)
			m.config.FolderNaming = naming
			prepareMultiSeasonSymlinkMount(t, m, parent)
			child, _ := seasonReadinessChild(t, m, parent)
			if err := m.queue.Add(child); err != nil {
				t.Fatal(err)
			}
			// Loading from durable queue storage drops all in-memory pointers.
			restored, err := m.queue.GetTorrent(child.InfoHash)
			if err != nil || restored.SeasonSource == nil || restored.SeasonSource == child.SeasonSource {
				t.Fatalf("restored source reference: %+v, %v", restored, err)
			}
			if err := m.processAction(context.Background(), restored); err != nil {
				t.Fatalf("retry under %s naming: %v", naming, err)
			}
			if _, err := m.GetEntry(restored.InfoHash); !errors.Is(err, storage.ErrEntryNotFound) {
				t.Fatalf("retry invented an authoritative child identity: %v", err)
			}
			current, err := m.queue.GetTorrent(restored.InfoHash)
			if err != nil || !current.IsComplete {
				t.Fatalf("retry did not complete: %+v, %v", current, err)
			}
		})
	}
}

func TestMultiSeasonReadinessStoredSourceRoundTrip(t *testing.T) {
	m, parent := newMultiSeasonReadinessManager(t)
	child, probes := seasonReadinessChild(t, m, parent)
	disk := t.TempDir()
	store, err := storage.NewStorage(disk)
	if err != nil {
		t.Fatal(err)
	}
	copy := *parent
	copy.MainGeneration = 0
	if err := store.AddOrUpdate(&copy); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.AddQueue(child); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewStorage(disk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	m.storage = reopened
	m.entryLifecycle = newEntryLifecycle()
	m.queue = newLifecycleTestQueue(reopened, m.entryLifecycle)
	loaded, err := m.queue.GetTorrent(child.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SeasonSource == nil || *loaded.SeasonSource != *child.SeasonSource {
		t.Fatalf("durable source reference changed: %+v", loaded.SeasonSource)
	}
	paths := []string{filepath.Join(loaded.DownloadPath(), "Demo.S02E01.mkv")}
	if _, err := m.downloader.importReadinessFiles(loaded, paths, MaxImportReadinessFiles); err == nil {
		t.Fatal("restored season accepted a sibling output path")
	}
	if err := m.verifyImportReadiness(context.Background(), loaded, probes); err != nil {
		t.Fatalf("restored child readiness: %v", err)
	}
	job, err := m.rebuildQueuedJob(context.Background(), loaded)
	if err != nil || !job.ResumeExisting || job.Entry.SeasonSource == nil {
		t.Fatalf("restart lost existing provider/source identity: %+v, %v", job, err)
	}
	loaded.Action = config.DownloadActionNone
	if err := m.processAction(context.Background(), loaded); !errors.Is(err, errDeleteQueueEntryOnJobFinish) {
		t.Fatalf("restored queue-only child action: %v", err)
	}
	if _, err := m.GetEntry(loaded.InfoHash); !errors.Is(err, storage.ErrEntryNotFound) {
		t.Fatalf("queue-only source became a main entry: %v", err)
	}
}

func TestMultiSeasonNamesDescribeTheirOwnSeason(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Demo.S01E01.1080p", "Demo.S02.1080p"},
		{"Demo.S01E01E02.1080p", "Demo.S02.1080p"},
		{"Demo S01", "Demo S02"},
		{"Demo Season 1", "Demo Season 02"},
		{"Demo S01-S08", "Demo S02"},
		{"Demo Complete Series", "Demo Season 02"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := replaceMultiSeasonPattern(tc.input, 2); got != tc.want {
				t.Fatalf("season name %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMultiSeasonSTRMUsesParentSource(t *testing.T) {
	for _, naming := range []config.WebDavFolderNaming{config.WebDavUseFileName, config.WebDavUseOriginalName, config.WebdavUseHash} {
		for _, phase := range []string{"fanout", "retry"} {
			t.Run(string(naming)+"/"+phase, func(t *testing.T) {
				m, parent := newMultiSeasonReadinessManager(t)
				m.config.FolderNaming = naming
				parent.Action = config.DownloadActionStrm
				m.downloader.strmURL = "https://tessarr.invalid/base"
				// Materialize the source's actual name index under this policy,
				// so URL resolution checks the same backend used by WebDAV.
				if err := m.storage.AddOrUpdate(parent); err != nil {
					t.Fatal(err)
				}
				wantOutputs := 2
				if phase == "fanout" {
					if err := m.downloader.download(t.Context(), parent); err != nil {
						t.Fatal(err)
					}
				} else {
					child, _ := seasonReadinessChild(t, m, parent)
					if err := m.queue.Add(child); err != nil {
						t.Fatal(err)
					}
					restored, err := m.queue.GetTorrent(child.InfoHash)
					if err != nil {
						t.Fatal(err)
					}
					if err := m.processAction(t.Context(), restored); err != nil {
						t.Fatal(err)
					}
					wantOutputs = 1
				}
				outputs := 0
				err := filepath.WalkDir(m.config.DownloadFolder, func(path string, item fs.DirEntry, err error) error {
					if err != nil || item.IsDir() || filepath.Ext(path) != ".strm" {
						return err
					}
					outputs++
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					stream, err := url.Parse(strings.TrimSpace(string(data)))
					if err != nil || stream.Host != "tessarr.invalid" {
						return fmt.Errorf("invalid generated stream URL %q: %v", data, err)
					}
					parts := strings.Split(strings.TrimPrefix(stream.Path, "/base/webdav/stream/__all__/"), "/")
					if len(parts) != 2 {
						return fmt.Errorf("unexpected stream route %q", stream.Path)
					}
					source, err := m.GetEntryByName(parts[0], parts[1])
					if err != nil || source.InfoHash != parent.InfoHash {
						return fmt.Errorf("season stream points outside its authoritative parent: %q: %v", stream.Path, err)
					}
					return nil
				})
				if err != nil || outputs != wantOutputs {
					t.Fatalf("season stream outputs=%d, want %d: %v", outputs, wantOutputs, err)
				}
			})
		}
	}
}
