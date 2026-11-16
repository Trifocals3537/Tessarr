package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

type fixtureArticle struct {
	data []byte
	meta nntp.YencMetadata
}
type fixtureHost struct {
	mu        sync.Mutex
	target    Target
	articles  map[string]fixtureArticle
	fail      map[string]error
	idle      func(context.Context) error
	published int
	stale     bool
	snapshots int
}

func newFixtureHost(t *testing.T) *fixtureHost {
	t.Helper()
	h := &fixtureHost{articles: make(map[string]fixtureArticle), fail: make(map[string]error)}
	var source strings.Builder
	source.WriteString("<nzb>")
	for _, name := range []string{"fileA.bin", "fileB.bin", "realtool.par2", "realtool.vol0+4.par2"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "par2", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		partSize := 512
		count := (len(data) + partSize - 1) / partSize
		fmt.Fprintf(&source, `<file poster="fixture" date="1" subject="&quot;%s&quot; yEnc (1/%d)"><groups><group>alt.test</group></groups><segments>`, name, count)
		f := storage.NZBFile{NzbID: "synthetic-nzb", Name: name, Size: int64(len(data)), FileType: storage.NZBFileTypeMedia}
		for i, off := 0, 0; off < len(data); i, off = i+1, off+partSize {
			id := fmt.Sprintf("%s-%d@fixture.invalid", name, i)
			end := min(off+partSize, len(data))
			payload := bytes.Clone(data[off:end])
			meta := nntp.YencMetadata{Name: name, Size: int64(len(data)), Begin: int64(off + 1), End: int64(end), Part: int64(i + 1), Total: int64(count), PartSize: int64(len(payload))}
			h.articles[id] = fixtureArticle{payload, meta}
			fmt.Fprintf(&source, `<segment number="%d" bytes="%d">%s</segment>`, i+1, len(payload)+30, id)
			f.Segments = append(f.Segments, storage.NZBSegment{Number: i + 1, MessageID: id, Bytes: int64(len(payload)), StartOffset: int64(off), EndOffset: int64(end - 1), Source: &storage.NZBArticleGeometry{Size: f.Size, Offset: int64(off), Bytes: int64(len(payload)), Part: int64(i + 1), Total: int64(count)}})
		}
		source.WriteString("</segments></file>")
		if name == "fileA.bin" {
			h.target = Target{ID: "synthetic-nzb", Name: "Synthetic plain-file fixture", File: f}
		}
		if !strings.HasSuffix(name, ".par2") {
			h.target.Files = append(h.target.Files, f)
		}
	}
	source.WriteString("</nzb>")
	h.target.Source = []byte(source.String())
	h.target.Generation = Generation(h.target.File)
	return h
}

func (h *fixtureHost) Snapshot(ctx context.Context, id, name string) (Target, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshots++
	if err := ctx.Err(); err != nil {
		return Target{}, err
	}
	if id != h.target.ID || name != h.target.File.Name {
		return Target{}, ErrUnsupported
	}
	t := h.target
	if h.stale && h.snapshots > 1 {
		t.Generation = strings.Repeat("e", 64)
	}
	return t, nil
}
func (h *fixtureHost) Download(ctx context.Context, id string, g *storage.NZBArticleGeometry, consume func(int) error) ([]byte, *nntp.YencMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	article, ok := h.articles[id]
	err := h.fail[id]
	h.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound}
	}
	if err := consume(len(article.data)); err != nil {
		return nil, nil, err
	}
	m := article.meta
	if g != nil && !g.Matches(m.Size, m.Begin, m.End, m.Part, m.Total, int64(len(article.data))) {
		return nil, nil, ErrStale
	}
	return bytes.Clone(article.data), &m, nil
}
func (h *fixtureHost) WaitIdle(ctx context.Context) error {
	if h.idle != nil {
		return h.idle(ctx)
	}
	return ctx.Err()
}
func (h *fixtureHost) Published(Target) { h.mu.Lock(); defer h.mu.Unlock(); h.published++ }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func missingArticle() error {
	return &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: "synthetic article absent on every provider"}
}

func TestRecoverReferenceFileAndReadAfterRestart(t *testing.T) {
	h := newFixtureHost(t)
	h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
	h.fail["fileA.bin-3@fixture.invalid"] = missingArticle()
	h.fail["fileB.bin-5@fixture.invalid"] = missingArticle()
	root := filepath.Join(t.TempDir(), "recovery")
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), h.target, h, s, 0, nil); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if h.published != 1 {
		t.Fatal("missing completion callback")
	}
	if !s.Covers(h.target.Generation, 0, 512) || !s.Covers(h.target.Generation, 1536, 512) {
		t.Fatal("missing articles not covered")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cache := &articleCache{ctx: t.Context(), host: h, consume: func(int) error { return nil }}
	raw := nzbRawTarget(t, h)
	remote, err := knownFile(raw, h.target.File, cache, s)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "par2", "testdata", "fileA.bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rangeRead := range [][2]int{{0, len(want)}, {400, 2200}, {1536, 1}, {7900, 100}} {
		got := make([]byte, rangeRead[1])
		n, err := remote.ReadAt(got, int64(rangeRead[0]))
		if err != nil || n != len(got) || !bytes.Equal(got, want[rangeRead[0]:rangeRead[0]+len(got)]) {
			t.Fatalf("read %v: n=%d err=%v", rangeRead, n, err)
		}
	}
	changed := h.target.File
	changed.Segments = append([]storage.NZBSegment(nil), changed.Segments...)
	changed.Segments[0].MessageID = "replacement@fixture.invalid"
	if s.Revision(Generation(changed)) != "" {
		t.Fatal("reused identity inherited stale patches")
	}
}

func nzbRawTarget(t *testing.T, h *fixtureHost) nzbparser.NzbFile {
	t.Helper()
	raw, err := nzbparser.Parse(bytes.NewReader(h.target.Source))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range raw.Files {
		ids, _ := rawIDs(f)
		if sameIDs(ids, h.target.File) {
			return f
		}
	}
	t.Fatal("fixture target missing")
	return nzbparser.NzbFile{}
}

func TestRecoveryRejectsBadParityTransientErrorsAndStaleSource(t *testing.T) {
	for _, kind := range []string{"insufficient", "corrupt parity", "transient", "stale", "budget", "archive"} {
		t.Run(kind, func(t *testing.T) {
			h := newFixtureHost(t)
			s := newTestStore(t)
			h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
			already := int64(0)
			switch kind {
			case "insufficient":
				for i := 0; i < 12; i++ {
					h.fail[fmt.Sprintf("fileA.bin-%d@fixture.invalid", i)] = missingArticle()
				}
			case "corrupt parity":
				for id, a := range h.articles {
					if strings.HasPrefix(id, "realtool.vol") {
						for i := range a.data {
							a.data[i] ^= 0x5a
						}
						h.articles[id] = a
					}
				}
			case "transient":
				h.fail["fileA.bin-2@fixture.invalid"] = &nntp.Error{Type: nntp.ErrorTypeTimeout, Message: "synthetic timeout"}
			case "stale":
				h.stale = true
				h.snapshots = 1
			case "budget":
				already = MaxReadBytes
			case "archive":
				h.target.File.FileType = storage.NZBFileTypeRar
				h.target.Generation = Generation(h.target.File)
			}
			err := Run(t.Context(), h.target, h, s, already, nil)
			if err == nil {
				t.Fatal("accepted failed recovery")
			}
			if s.Revision(h.target.Generation) != "" || h.published != 0 {
				t.Fatal("failed recovery published data")
			}
			if kind == "transient" && safeReason(err) != ErrTemporary.Error() {
				t.Fatalf("misclassified timeout: %v", err)
			}
		})
	}
}

func TestRecoveryCorrectsCRCValidButPAR2InvalidInput(t *testing.T) {
	h := newFixtureHost(t)
	s := newTestStore(t)
	id := "fileA.bin-3@fixture.invalid"
	a := h.articles[id]
	a.data[12] ^= 1
	h.articles[id] = a
	if err := Run(t.Context(), h.target, h, s, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !s.Covers(h.target.Generation, 1536, 512) {
		t.Fatal("corrupt slice was not reconstructed")
	}
}

func TestRecoveryCancellationDoesNotPublish(t *testing.T) {
	h := newFixtureHost(t)
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.idle = func(ctx context.Context) error { cancel(); return ctx.Err() }
	if err := Run(ctx, h.target, h, s, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if s.Revision(h.target.Generation) != "" {
		t.Fatal("cancelled work published")
	}
}

var _ io.ReaderAt = (*remoteFile)(nil)
