package usenet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/logger"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
	"github.com/google/uuid"
)

func TestManualPAR2ThroughProductionNNTPAndReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualPAR2NNTPHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "TESSARR_PAR2_NNTP_HELPER=1", "GODEBUG=x509usefallbackroots=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("production PAR2 fixture: %v %s", err, out)
	}
}

type recoveryWireArticle struct {
	data        []byte
	name        string
	size, begin int
	part, total int
}

func TestManualPAR2NNTPHelper(t *testing.T) {
	if os.Getenv("TESSARR_PAR2_NNTP_HELPER") != "1" {
		return
	}
	defer logger.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	x509.SetFallbackRoots(roots)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	articles := make(map[string]recoveryWireArticle)
	requests := make(map[string]int)
	missing := map[string]bool{"fileA.bin-0@fixture.invalid": true, "fileA.bin-3@fixture.invalid": true, "fileB.bin-5@fixture.invalid": true}
	var source strings.Builder
	source.WriteString("<nzb>")
	id := uuid.NewString()
	var files []storage.NZBFile
	var expected []byte
	for _, name := range []string{"fileA.bin", "fileB.bin", "realtool.par2", "realtool.vol0+4.par2"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "par2", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "fileA.bin" {
			expected = bytes.Clone(data)
		}
		const partSize = 512
		total := (len(data) + partSize - 1) / partSize
		fmt.Fprintf(&source, `<file poster="fixture" date="1" subject="&quot;%s&quot; yEnc (1/%d)"><groups><group>alt.test</group></groups><segments>`, name, total)
		file := storage.NZBFile{NzbID: id, Name: name, Size: int64(len(data)), FileType: storage.NZBFileTypeMedia}
		for i, off := 0, 0; off < len(data); i, off = i+1, off+partSize {
			msg := fmt.Sprintf("%s-%d@fixture.invalid", name, i)
			end := min(off+partSize, len(data))
			articles[msg] = recoveryWireArticle{data: bytes.Clone(data[off:end]), name: name, size: len(data), begin: off + 1, part: i + 1, total: total}
			fmt.Fprintf(&source, `<segment number="%d" bytes="%d">%s</segment>`, i+1, end-off+30, msg)
			file.Segments = append(file.Segments, storage.NZBSegment{Number: i + 1, MessageID: msg, Bytes: int64(end - off), StartOffset: int64(off), EndOffset: int64(end - 1)}) // Legacy maps have no Source field.
		}
		source.WriteString("</segments></file>")
		if !strings.HasSuffix(name, ".par2") {
			files = append(files, file)
		}
	}
	source.WriteString("</nzb>")
	articles["abort@fixture.invalid"] = recoveryWireArticle{data: bytes.Repeat([]byte("synthetic interrupted article"), 10000), name: "abort.bin", size: len("synthetic interrupted article") * 10000, begin: 1, part: 1, total: 1}
	abortedClosed := make(chan struct{}, 1)
	var mu sync.Mutex
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				didAbort := false
				defer func() {
					if didAbort {
						select {
						case abortedClosed <- struct{}{}:
						default:
						}
					}
				}()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				p := textproto.NewConn(conn)
				_ = p.PrintfLine("200 synthetic fixture ready")
				for {
					line, err := p.ReadLine()
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) == 0 {
						return
					}
					arg := ""
					if len(fields) > 1 {
						arg = strings.Trim(fields[len(fields)-1], "<>")
					}
					switch strings.ToUpper(fields[0]) {
					case "AUTHINFO":
						if len(fields) > 1 && strings.EqualFold(fields[1], "USER") {
							_ = p.PrintfLine("381 password required")
						} else {
							_ = p.PrintfLine("281 accepted")
						}
					case "DATE":
						_ = p.PrintfLine("111 20261116000000")
					case "STAT", "BODY", "ARTICLE":
						if arg == "abort@fixture.invalid" {
							didAbort = true
						}
						mu.Lock()
						article, exists := articles[arg]
						absent := missing[arg]
						if strings.EqualFold(fields[0], "BODY") {
							requests[arg]++
						}
						mu.Unlock()
						if !exists || absent {
							_ = p.PrintfLine("430 missing")
							continue
						}
						if fields[0] == "STAT" {
							_ = p.PrintfLine("223 0 <%s>", arg)
							continue
						}
						_ = p.PrintfLine("222 0 <%s> body follows", arg)
						w := p.DotWriter()
						_, _ = w.Write(recoveryWireYenc(article))
						_ = w.Close()
					case "QUIT":
						_ = p.PrintfLine("205 bye")
						return
					default:
						_ = p.PrintfLine("500 unsupported")
					}
				}
			}()
		}
	}()
	defer func() { _ = listener.Close(); <-acceptDone; workers.Wait() }()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	root := t.TempDir()
	if err := config.SetConfigPath(root); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Debrids: []config.Debrid{{Name: "realdebrid", APIKey: "synthetic"}}, Usenet: config.Usenet{DiskBufferPath: filepath.Join(root, "streams"), Providers: []config.UsenetProvider{{Host: host, Port: port, SSL: true, Username: "synthetic-user", Password: "synthetic-password", MaxConnections: 3}}}}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	_ = config.Get()
	u, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Close() }()
	if u.recovery == nil {
		t.Fatal("recovery failed to initialize")
	}
	nzb := &storage.NZB{ID: id, Name: "Synthetic plain-file release", Status: NZBStatusCompleted, Files: files}
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// A writer may stop while the response body still has unread bytes. It
	// must never return that connection to the shared playback pool.
	_, _, err = (recoveryHost{u: u}).Download(ctx, "abort@fixture.invalid", nil, func(int) error { return recovery.ErrLimit })
	if !errors.Is(err, recovery.ErrLimit) {
		t.Fatalf("interrupted transfer result: %v", err)
	}
	select {
	case <-abortedClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupted body connection remained pooled")
	}
	payload, _, err := (recoveryHost{u: u}).Download(ctx, "fileA.bin-8@fixture.invalid", nil, func(int) error { return nil })
	if err != nil || !bytes.Equal(payload, expected[4096:4608]) {
		t.Fatalf("shared pool was not usable after interrupted body: %v", err)
	}
	if err := u.Stream(ctx, id, "fileA.bin", 0, 511, io.Discard); err == nil {
		t.Fatal("missing bytes unexpectedly streamed before repair")
	}
	before, _ := u.FileRepairFingerprint(id, "fileA.bin")
	j, err := u.recovery.Start(ctx, id, "fileA.bin", []byte(source.String()))
	if err != nil {
		t.Fatal(err)
	}
	for {
		jobs, err := u.recovery.Jobs()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, job := range jobs {
			if job.ID == j.ID {
				if job.State == "failed" {
					t.Fatal(job.Reason)
				}
				found = job.State == "succeeded"
			}
		}
		if found {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("recovery did not complete")
		case <-time.After(5 * time.Millisecond):
		}
	}
	after, _ := u.FileRepairFingerprint(id, "fileA.bin")
	if before == after {
		t.Fatal("patch publication did not invalidate availability progress")
	}
	if err := u.CheckFile(ctx, id, "fileA.bin"); err != nil {
		t.Fatalf("saved patches were not recognized by availability: %v", err)
	}
	if err := u.EnsureStreamReady(ctx, id, "fileA.bin"); err != nil {
		t.Fatalf("failure quarantine did not recover: %v", err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	u, err = New()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	requests = make(map[string]int)
	mu.Unlock()
	for _, r := range [][2]int64{{0, 7999}, {250, 2200}, {1536, 1536}, {7900, 7999}} {
		var output bytes.Buffer
		if err := u.Stream(ctx, id, "fileA.bin", r[0], r[1], &output); err != nil {
			t.Fatalf("stream range %v: %v", r, err)
		}
		if !bytes.Equal(output.Bytes(), expected[r[0]:r[1]+1]) {
			t.Fatalf("stream range %v differed from original", r)
		}
	}
	mu.Lock()
	for msg := range missing {
		if strings.HasPrefix(msg, "fileA.bin") && requests[msg] != 0 {
			t.Errorf("repaired article was fetched again: %s", msg)
		}
	}
	mu.Unlock()
	items, _, err := u.PAR2Targets(ctx, "Synthetic", "")
	if err != nil || len(items) != 2 {
		t.Fatalf("target discovery: %v %+v", err, items)
	}
	// A page cursor advances by NZB, so finish a large pack before advancing.
	for i := 0; i < 60; i++ {
		f := files[0]
		f.Name = fmt.Sprintf("synthetic-%03d.mkv", i)
		nzb.Files = append(nzb.Files, f)
	}
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		t.Fatal(err)
	}
	items, next, err := u.PAR2Targets(ctx, "Synthetic", "")
	if err != nil || len(items) != 62 || next != "" {
		t.Fatalf("pack pagination dropped targets: %d %q %v", len(items), next, err)
	}
	gen := recovery.Generation(files[0])
	if err := u.Delete(id); err != nil {
		t.Fatal(err)
	}
	if u.recovery.Store().Revision(gen) != "" {
		t.Fatal("NZB deletion retained recovery data")
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usenet", "recovery", "jobs.json"), []byte("synthetic corrupt recovery state"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err = New()
	if err != nil {
		t.Fatalf("recovery failure disabled ordinary Usenet: %v", err)
	}
	if u.recovery != nil {
		t.Fatal("corrupt recovery state was accepted")
	}
	if err := u.nzbStorage.AddNZB(&storage.NZB{ID: id, Name: "Synthetic ordinary read", Status: NZBStatusCompleted, Files: files}); err != nil {
		t.Fatal(err)
	}
	var normal bytes.Buffer
	if err := u.Stream(ctx, id, "fileA.bin", 4096, 4607, &normal); err != nil || !bytes.Equal(normal.Bytes(), expected[4096:4608]) {
		t.Fatalf("ordinary read after recovery failure: %v", err)
	}
}

func recoveryWireYenc(a recoveryWireArticle) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n=ypart begin=%d end=%d\r\n", a.part, a.total, a.size, a.name, a.begin, a.begin+len(a.data)-1)
	column := 0
	for _, v := range a.data {
		v += 42
		if v == 0 || v == '\r' || v == '\n' || v == '=' {
			b.WriteByte('=')
			v += 64
			column++
		}
		b.WriteByte(v)
		column++
		if column >= 128 {
			b.WriteString("\r\n")
			column = 0
		}
	}
	fmt.Fprintf(&b, "\r\n=yend size=%d part=%d pcrc32=%08x\r\n", len(a.data), a.part, crc32.ChecksumIEEE(a.data))
	return b.Bytes()
}
