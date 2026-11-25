package usenet

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/customerror"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

func TestInvalidLegacyMapRejectedBeforeServingOrVerifying(t *testing.T) {
	u := newMetadataTestUsenet(t)
	// The advertised end offset is correct, but the middle article is not
	// mapped. A cumulative fallback would silently shift the last article.
	file := storage.NZBFile{NzbID: testNZBID, Name: "gapped.mkv", Size: 48,
		Segments: []storage.NZBSegment{
			{MessageID: "first", Bytes: 16, EndOffset: 15},
			{MessageID: "last", Bytes: 16, StartOffset: 32, EndOffset: 47},
		}}
	if err := u.nzbStorage.AddNZB(&storage.NZB{ID: testNZBID, Name: "fixture", Files: []storage.NZBFile{file}}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	err := u.PrepareStream(ctx, testNZBID, file.Name)
	var statusErr *customerror.Error
	if !errors.Is(err, storage.ErrInvalidNZBMap) || !errors.As(err, &statusErr) || !statusErr.IsPermanent() || statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("prepared an invalid map or lost its classification: %v", err)
	}
	var out bytes.Buffer
	if err := u.Stream(ctx, testNZBID, file.Name, 0, file.Size-1, &out); !errors.Is(err, storage.ErrInvalidNZBMap) || out.Len() != 0 {
		t.Fatalf("invalid map served %d bytes or returned wrong error: %v", out.Len(), err)
	}
	if err := u.VerifyFileContent(ctx, testNZBID, file.Name); !errors.Is(err, storage.ErrInvalidNZBMap) {
		t.Fatalf("head verification accepted invalid map: %v", err)
	}
	if err := u.VerifyFileRanges(ctx, testNZBID, file.Name, []storage.FailedReadRange{{InfoHash: testNZBID, FileName: file.Name, Offset: 32, Length: 1}}); !errors.Is(err, storage.ErrInvalidNZBMap) {
		t.Fatalf("range verification accepted invalid map: %v", err)
	}
	if u.fs.Size() != 0 {
		t.Fatal("invalid map was cached as a filesystem entry")
	}
}
