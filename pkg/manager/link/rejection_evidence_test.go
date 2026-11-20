package link

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

func TestPersistentReplacementRejectionRequiresFreshObservationsAndRecovers(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reject.Load() {
			w.Header().Set("X-Error", "invalid_download_code")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeValidLinkProbe(w)
	}))
	defer server.Close()
	client := &lifecycleTestClient{cacheLinks: true, links: []types.DownloadLink{
		lifecycleDownloadLink(server.URL + "/one"),
		lifecycleDownloadLink(server.URL + "/two"),
		lifecycleDownloadLink(server.URL + "/three"),
		lifecycleDownloadLink(server.URL + "/four"),
	}}
	service := newLifecycleService(client, server.Client(), 0)
	now := time.Now()
	service.now = func() time.Time { return now }
	entry := lifecycleTestEntry()
	ctx := context.Background()

	if _, err := service.GetLink(ctx, entry, "video.mkv"); err == nil || errors.Is(err, ErrRepeatedLinkRejection) {
		t.Fatalf("first rejection was prematurely escalated: %v", err)
	}
	for range 5 {
		_, err := service.GetLink(ctx, entry, "video.mkv")
		if le := GetLinkError(err); le == nil || le.Code != CodeLinkRefreshCooldown {
			t.Fatalf("cooldown was treated as a fresh rejection: %v", err)
		}
	}
	now = now.Add(refreshBackoffBase)
	if _, err := service.GetLink(WithoutRepair(ctx), entry, "video.mkv"); err == nil || errors.Is(err, ErrRepeatedLinkRejection) {
		t.Fatalf("second rejection was prematurely escalated: %v", err)
	}
	now = now.Add(refreshBackoffBase)
	_, err := service.GetLink(ctx, entry, "video.mkv")
	if le := GetLinkError(err); !errors.Is(err, ErrRepeatedLinkRejection) || le == nil || !le.ShouldRefetch() {
		t.Fatalf("confirmed repeated replacement failure = %v", err)
	}

	reject.Store(false)
	now = now.Add(refreshBackoffMax)
	recovered, err := service.GetLink(ctx, entry, "video.mkv")
	if err != nil || recovered.Empty() {
		t.Fatalf("persistent rejection prevented later recovery: %v", err)
	}
	reject.Store(true)
	if _, err := service.Refresh(ctx, entry, "video.mkv", recovered); err == nil || errors.Is(err, ErrRepeatedLinkRejection) {
		t.Fatalf("successful validation did not clear old rejection evidence: %v", err)
	}
}

func TestReplacementRejectionEvidenceIsScopedAndNotTransient(t *testing.T) {
	service := newLifecycleService(&lifecycleTestClient{}, http.DefaultClient, 0)
	now := time.Now()
	service.now = func() time.Time { return now }
	for range 5 {
		if service.noteReplacementRejection("file-a", "invalid_download_code") {
			t.Fatal("concurrent observations counted as independent replacements")
		}
	}
	now = now.Add(refreshBackoffBase)
	if service.noteReplacementRejection("file-a", "invalid_download_code") {
		t.Fatal("second observation escalated")
	}
	now = now.Add(refreshBackoffBase)
	if service.noteReplacementRejection("file-b", "invalid_download_code") {
		t.Fatal("file-a evidence contaminated another file")
	}
	if service.noteReplacementRejection("file-a", "503") ||
		service.noteReplacementRejection("file-a", "invalid_download_code") {
		t.Fatal("inconclusive failure was counted as persistent rejection")
	}
}

func TestReplacementRejectionUsesCurrentFileIdentity(t *testing.T) {
	entry := lifecycleTestEntry()
	entry.Files["video.mkv"].ID = "original-file"
	before := replacementRejectionKey(entry, "video.mkv")
	entry.Files["video.mkv"].ID = "replacement-file"
	if after := replacementRejectionKey(entry, "video.mkv"); after == before {
		t.Fatal("rejection evidence survived replacement of the canonical file")
	}
}
