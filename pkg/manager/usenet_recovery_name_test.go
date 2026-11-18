package manager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/rs/zerolog"
)

const recoveryNameNZB = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="fixture" date="1" subject="&quot;payload.mkv&quot; [1/1] yEnc (1/1)">
  <groups><group>alt.test</group></groups>
  <segments><segment bytes="1024" number="1">payload@recovery.invalid</segment></segments>
 </file>
</nzb>`

var recoveryNameTLSOnce sync.Once
var recoveryNameTLSCertificate tls.Certificate

const recoveryNameTLSChild = "TESSARR_RECOVERY_NAME_TLS_CHILD"

// SetFallbackRoots changes process-wide trust and cannot be undone. Keep every
// recovery/admission fixture that needs these roots in its own test process.
func runNZBRecoveryTLSChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv(recoveryNameTLSChild) == t.Name() {
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.timeout=80s", "-test.v")
	child.Env = append(os.Environ(), recoveryNameTLSChild+"="+t.Name())
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated recovery TLS fixture failed: %v context=%v\n%s", err, ctx.Err(), output)
	}
	t.Logf("isolated recovery TLS fixture:\n%s", output)
	return true
}

func TestNZBRecoveryTLSFixturePreservesParentTrust(t *testing.T) {
	if os.Getenv(recoveryNameTLSChild) == t.Name() {
		m := newNZBRecoveryNameTestManager(t)
		admitRecoveryNameNZB(t, m, "Synthetic.nzb")
		return
	}
	t.Run("before", TestStreamClientRejectsUntrustedTLSByDefault)
	runNZBRecoveryTLSChild(t)
	t.Run("after", TestStreamClientRejectsUntrustedTLSByDefault)
}

type nzbRecoveryFixtureOptions struct {
	statCode  int
	bodyCodes map[string]int
}

func newNZBRecoveryNameTestManager(t *testing.T, options ...nzbRecoveryFixtureOptions) *Manager {
	t.Helper()
	childTest := os.Getenv(recoveryNameTLSChild)
	if childTest == "" || (t.Name() != childTest && !strings.HasPrefix(t.Name(), childTest+"/")) {
		t.Fatal("recovery TLS fixture requires an isolated test process")
	}
	statCode := 223
	var bodyCodes map[string]int
	if len(options) > 0 {
		if options[0].statCode != 0 {
			statCode = options[0].statCode
		}
		bodyCodes = options[0].bodyCodes
	}
	config.Reset()
	t.Cleanup(config.Reset)
	m := newQueueRecoveryTestManager(t)
	m.config = config.Get()
	m.config.DownloadFolder = t.TempDir()
	m.config.Usenet.DiskBufferPath = t.TempDir()
	m.config.Retries = 0
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	recoveryNameTLSOnce.Do(func() {
		server := httptest.NewTLSServer(nil)
		defer server.Close()
		recoveryNameTLSCertificate = server.TLS.Certificates[0]
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		x509.SetFallbackRoots(roots)
	})
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{recoveryNameTLSCertificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	closed := false
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			connections[conn] = true
			mu.Unlock()
			workers.Go(func() {
				defer func() { _ = conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				protocol := textproto.NewConn(conn)
				if protocol.PrintfLine("200 local recovery fixture") != nil {
					return
				}
				for {
					line, err := protocol.ReadLine()
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) == 0 {
						return
					}
					switch strings.ToUpper(fields[0]) {
					case "STAT":
						if statCode == 223 {
							err = protocol.PrintfLine("223 0 %s", fields[len(fields)-1])
						} else {
							err = protocol.PrintfLine("%d synthetic probe failure", statCode)
						}
					case "DATE":
						err = protocol.PrintfLine("111 20260101000000")
					case "BODY":
						code := bodyCodes[strings.Trim(fields[len(fields)-1], "<>")]
						if code == 0 {
							code = 500
						}
						err = protocol.PrintfLine("%d synthetic header failure", code)
					case "QUIT":
						_ = protocol.PrintfLine("205 bye")
						return
					default:
						err = protocol.PrintfLine("500 unexpected fixture command")
					}
					if err != nil {
						return
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		closed = true
		for conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	m.config.Usenet.Providers = []config.UsenetProvider{{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, SSL: true, MaxConnections: 1}}
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
	// Admit through the production path while holding execution at the worker
	// boundary. Recovery is exercised explicitly with real local TLS STATs.
	m.jobQueue = NewJobQueueWithCapacity(m.ctx, 1, 8, func(ctx context.Context, _ *Job) { <-ctx.Done() }, m.entryLifecycle)
	m.jobQueue.logger = zerolog.Nop()
	return m
}

func admitRecoveryNameNZB(t *testing.T, m *Manager, filename string) *storage.Entry {
	t.Helper()
	req := NewNZBRequest(filename, m.config.DownloadFolder, []byte(recoveryNameNZB), m.arr.GetOrCreate("tv"), config.DownloadActionDownload, "", ImportTypeSABnzbd, true)
	id, err := m.AddNewNZB(m.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := m.queue.GetTorrent(id)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestQueuedNZBRecoveryPreservesCanonicalNameAndOutput(t *testing.T) {
	if runNZBRecoveryTLSChild(t) {
		return
	}
	for _, tc := range []struct {
		filename, canonical                         string
		stagedOnly, legacyOutput, originalDifferent bool
	}{
		{filename: "Demo.S01E01.WEB.H264.nzb", canonical: "Demo.S01E01.WEB.H264"},
		{filename: "Demo.S01E01.WEB.H264", canonical: "Demo.S01E01.WEB.H264"},
		{filename: "Demo.S01E01.H264.NZB", canonical: "Demo.S01E01.H264", stagedOnly: true},
		{filename: "Demo.nzb.nzb", canonical: "Demo.nzb"},
		{filename: ".nzb.nzb", canonical: ".nzb"},
		{filename: "Demo.S01E01.mkv", canonical: "Demo.S01E01.mkv"},
		{filename: "Demo.S01E01.H264.nzb", canonical: "Demo.S01E01.H264", legacyOutput: true},
		{filename: "Demo.S01E01.H264.nzb", canonical: "Demo.S01E01.H264", originalDifferent: true},
	} {
		t.Run(fmt.Sprintf("%s/staged=%v/legacy=%v/original=%v", tc.filename, tc.stagedOnly, tc.legacyOutput, tc.originalDifferent), func(t *testing.T) {
			m := newNZBRecoveryNameTestManager(t)
			entry := admitRecoveryNameNZB(t, m, tc.filename)
			if entry.Name != tc.canonical {
				t.Fatalf("admitted name=%q, want %q", entry.Name, tc.canonical)
			}
			if tc.legacyOutput {
				entry.OutputName = ""
			}
			if tc.originalDifferent {
				entry.OriginalFilename = tc.filename
			}
			if err := m.queue.Update(entry); err != nil {
				t.Fatal(err)
			}
			if tc.stagedOnly {
				meta, err := m.usenet.GetNZBHeader(entry.InfoHash)
				if err != nil {
					t.Fatal(err)
				}
				meta.Path = ""
				if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
					t.Fatal(err)
				}
			}
			name, original, output, downloadPath := entry.Name, entry.OriginalFilename, entry.OutputName, entry.DownloadPath()
			before, err := os.Stat(downloadPath)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(downloadPath, "preserve-existing.txt")
			if err := os.WriteFile(sentinel, []byte("existing output"), 0o600); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				job, err := m.rebuildQueuedNZBJob(m.ctx, entry)
				if err != nil {
					t.Fatal(err)
				}
				if entry.Name != name || entry.OriginalFilename != original || entry.OutputName != output || entry.DownloadPath() != downloadPath || job.NZBMeta.Name != name || job.Request.Name != name {
					t.Fatalf("rebuild %d changed name/output identity: entry=%+v metadata=%q request=%q", attempt+1, entry, job.NZBMeta.Name, job.Request.Name)
				}
				if _, err := safeUsenetEntryDownloadPath(m.config.DownloadFolder, entry); err != nil {
					t.Fatalf("rebuild %d broke output containment: %v", attempt+1, err)
				}
				if err := m.queue.Update(entry); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					m.cancel()
					m.jobQueue.Close()
					m.jobQueue = nil
					if err := m.usenet.Close(); err != nil {
						t.Fatal(err)
					}
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
					m.storage = store
					restartedUsenet, err := usenet.New()
					if err != nil {
						t.Fatal(err)
					}
					m.usenet = restartedUsenet
					t.Cleanup(func() {
						if err := restartedUsenet.Close(); err != nil {
							t.Error(err)
						}
					})
					m.entryLifecycle = newEntryLifecycle()
					m.queue = newLifecycleTestQueue(store, m.entryLifecycle)
					m.resetLifecycle()
					entry, err = m.queue.GetTorrent(entry.InfoHash)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			after, err := os.Stat(downloadPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("recovery replaced the existing output directory", err)
			}
			assertManagerFileContents(t, sentinel, "existing output")
		})
	}
}

func TestQueuedNZBRecoveryRejectsUnsafePlacementBeforeChangingArtifacts(t *testing.T) {
	if runNZBRecoveryTLSChild(t) {
		return
	}
	for _, scenario := range []string{"different output", "different save root", "unsafe name", "previously shortened name"} {
		t.Run(scenario, func(t *testing.T) {
			m := newNZBRecoveryNameTestManager(t)
			entry := admitRecoveryNameNZB(t, m, "Demo.S01E01.WEB.H264.nzb")
			metadata, err := m.usenet.GetNZBHeader(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := os.ReadFile(entry.Magnet)
			if err != nil {
				t.Fatal(err)
			}
			source, err := os.ReadFile(metadata.Path)
			if err != nil {
				t.Fatal(err)
			}
			downloadPath := entry.DownloadPath()
			sentinel := filepath.Join(downloadPath, "preserve-existing.txt")
			if err := os.WriteFile(sentinel, []byte("existing output"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "different output":
				entry.OutputName = "different-release"
			case "different save root":
				entry.SavePath = t.TempDir()
			case "unsafe name":
				entry.Name = "../outside"
			case "previously shortened name":
				entry.Name = "Demo.S01E01.WEB"
			}
			if _, err := m.rebuildQueuedNZBJob(m.ctx, entry); err == nil {
				t.Fatal("unsafe/stale placement was accepted")
			}
			assertManagerFileContents(t, entry.Magnet, string(staged))
			assertManagerFileContents(t, metadata.Path, string(source))
			assertManagerFileContents(t, sentinel, "existing output")
			current, err := m.usenet.GetNZBHeader(entry.InfoHash)
			if err != nil || current.Name != metadata.Name {
				t.Fatalf("rejected recovery rewrote metadata: %+v %v", current, err)
			}
		})
	}
}

func TestQueuedNZBRecoveryCancellationPreservesSourceAndIdentity(t *testing.T) {
	if runNZBRecoveryTLSChild(t) {
		return
	}
	m := newNZBRecoveryNameTestManager(t)
	entry := admitRecoveryNameNZB(t, m, "Demo.S01E01.WEB.H264.nzb")
	staged, err := os.ReadFile(entry.Magnet)
	if err != nil {
		t.Fatal(err)
	}
	name, original, output, stagedPath := entry.Name, entry.OriginalFilename, entry.OutputName, entry.Magnet
	ctx, cancel := context.WithCancel(m.ctx)
	cancel()
	if _, err := m.rebuildQueuedNZBJob(ctx, entry); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled recovery was admitted", err)
	}
	if entry.Name != name || entry.OriginalFilename != original || entry.OutputName != output || entry.Magnet != stagedPath {
		t.Fatal("canceled recovery changed persisted identities")
	}
	assertManagerFileContents(t, stagedPath, string(staged))
}

func TestReparseNZBRejectsInvalidCanonicalIdentityWithoutIO(t *testing.T) {
	for _, tc := range []struct{ id, name string }{
		{id: "", name: "valid"},
		{id: "../outside", name: "valid"},
		{id: "33333333-3333-4333-8333-333333333333", name: ""},
		{id: "33333333-3333-4333-8333-333333333333", name: "../outside"},
		{id: "33333333-3333-4333-8333-333333333333", name: "CON"},
	} {
		// An uninitialized client would panic if invalid identity reached any
		// network/storage operation instead of failing before parsing.
		u := &usenet.Usenet{}
		if _, _, err := u.ReparseWithID(context.Background(), tc.id, tc.name, []byte(recoveryNameNZB), "tv"); err == nil {
			t.Fatalf("invalid canonical identity accepted: %+v", tc)
		}
	}
}
