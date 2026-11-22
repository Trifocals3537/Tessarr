package link

import (
	"context"
	"errors"
	"fmt"
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
	link := lifecycleDownloadLink("https://cdn.example/video")
	entry.Files["video.mkv"].ID = "original-file"
	before := replacementRejectionKey(entry, "video.mkv", link)
	entry.Files["video.mkv"].ID = "replacement-file"
	if after := replacementRejectionKey(entry, "video.mkv", link); after == before {
		t.Fatal("rejection evidence survived replacement of the canonical file")
	}
}

func TestReplacementRejectionUsesActualReplacementAccount(t *testing.T) {
	type attempt struct {
		account   string
		healthy   bool
		confirmed bool
	}
	for _, tc := range []struct {
		name     string
		attempts []attempt
	}{
		{
			name: "switch requires independent confirmation",
			attempts: []attempt{
				{account: "account-a"}, {account: "account-a"},
				{account: "account-b"}, {account: "account-b"},
				{account: "account-b", confirmed: true},
			},
		},
		{
			name: "success on another account does not clear evidence",
			attempts: []attempt{
				{account: "account-a"}, {account: "account-b"},
				{account: "account-a"}, {account: "account-b", healthy: true},
				{account: "account-b"}, {account: "account-a", confirmed: true},
				{account: "account-b"},
			},
		},
		{
			name: "missing account cannot confirm",
			attempts: []attempt{
				{}, {}, {}, {},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Query().Get("healthy") == "true" {
					writeValidLinkProbe(w)
					return
				}
				w.Header().Set("X-Error", "invalid_download_code")
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()

			client := &lifecycleTestClient{cacheLinks: true}
			for i, attempt := range tc.attempts {
				l := lifecycleDownloadLink(fmt.Sprintf("%s/%d?healthy=%t", server.URL, i, attempt.healthy))
				l.Token = attempt.account
				client.links = append(client.links, l)
			}
			service := newLifecycleService(client, server.Client(), 0)
			now := time.Now()
			service.now = func() time.Time { return now }
			entry := lifecycleTestEntry()
			rejected := lifecycleDownloadLink("https://cdn.example/stale")
			rejected.Token = "previous-account"

			for i, attempt := range tc.attempts {
				_, err := service.Refresh(t.Context(), entry, "video.mkv", rejected)
				if attempt.healthy != (err == nil) || attempt.confirmed != errors.Is(err, ErrRepeatedLinkRejection) {
					t.Fatalf("replacement %d (healthy=%t, confirmed=%t): %v", i, attempt.healthy, attempt.confirmed, err)
				}
				now = now.Add(refreshBackoffMax)
				// The previous URL may still belong to a different account.
			}
		})
	}
}
