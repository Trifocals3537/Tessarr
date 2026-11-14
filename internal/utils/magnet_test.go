package utils

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/testutil"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// checkMagnet is a helper function that verifies magnet properties
func checkMagnet(t *testing.T, magnet *Magnet, expectedInfoHash, expectedName, expectedLink string, expectedTrackerCount int, shouldBeTorrent bool) {
	t.Helper() // This marks the function as a test helper

	// Verify basic properties
	if magnet.Name != expectedName {
		t.Errorf("Expected name '%s', got '%s'", expectedName, magnet.Name)
	}
	if magnet.InfoHash != expectedInfoHash {
		t.Errorf("Expected InfoHash '%s', got '%s'", expectedInfoHash, magnet.InfoHash)
	}
	if magnet.Link != expectedLink {
		t.Errorf("Expected Link '%s', got '%s'", expectedLink, magnet.Link)
	}

	// Verify the magnet link contains the essential info hash
	if !strings.Contains(magnet.Link, "xt=urn:btih:"+expectedInfoHash) {
		t.Error("Magnet link should contain info hash")
	}

	// Verify tracker count
	trCount := strings.Count(magnet.Link, "tr=")
	if trCount != expectedTrackerCount {
		t.Errorf("Expected %d tracker URLs, got %d", expectedTrackerCount, trCount)
	}
}

// testMagnetFromFile is a helper function for tests that use GetMagnetFromFile with file operations
func testMagnetFromFile(t *testing.T, filePath string, rmTrackerUrls bool, expectedInfoHash, expectedName, expectedLink string, expectedTrackerCount int) {
	t.Helper()

	file, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("Failed to open torrent file %s: %v", filePath, err)
	}
	defer file.Close()

	magnet, err := GetMagnetFromFile(file, filepath.Base(filePath), rmTrackerUrls)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount, true)

	// Log the result
	if rmTrackerUrls {
		t.Logf("Generated clean magnet link: %s", magnet.Link)
	} else {
		t.Logf("Generated magnet link with trackers: %s", magnet.Link)
	}
}

func TestGetMagnetFromFile_RealTorrentFile_StripTrue(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0 // Should be 0 when stripping trackers

	torrentPath := testutil.GetTestTorrentPath()
	testMagnetFromFile(t, torrentPath, true, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFile_RealTorrentFile_StripFalse(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2 // Should be 2 when preserving trackers

	torrentPath := testutil.GetTestTorrentPath()
	testMagnetFromFile(t, torrentPath, false, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromBytesStripsTrackersFromUploadedFileWithoutChangingHash(t *testing.T) {
	torrentData, err := os.ReadFile(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatal(err)
	}
	original, err := GetMagnetFromBytes(torrentData, false)
	if err != nil {
		t.Fatal(err)
	}
	sanitized, err := GetMagnetFromBytes(torrentData, true)
	if err != nil {
		t.Fatal(err)
	}
	if sanitized.InfoHash != original.InfoHash {
		t.Fatalf("sanitized infohash = %q, want %q", sanitized.InfoHash, original.InfoHash)
	}
	if bytes.Equal(sanitized.File, torrentData) {
		t.Fatal("sanitized torrent bytes were not rewritten")
	}
	metadata, err := metainfo.Load(bytes.NewReader(sanitized.File))
	if err != nil {
		t.Fatalf("load sanitized torrent: %v", err)
	}
	if metadata.Announce != "" || len(metadata.AnnounceList) != 0 {
		t.Fatalf("sanitized torrent retains trackers: announce=%q tiers=%d", metadata.Announce, len(metadata.AnnounceList))
	}
	if got := metadata.HashInfoBytes().HexString(); got != original.InfoHash {
		t.Fatalf("sanitized file infohash = %q, want %q", got, original.InfoHash)
	}
}

func TestGetMagnetFromBytesRejectsExcessiveNesting(t *testing.T) {
	// Valid torrent metadata with an unknown extension field. Unknown fields
	// are accepted at ordinary depths, but must not permit unbounded recursion
	// either in the outer document or inside the raw info dictionary.
	const info = "d6:lengthi1e4:name1:x12:piece lengthi16384e6:pieces20:abcdefghijklmnopqrste"
	for _, location := range []string{"document", "info"} {
		for _, kind := range []string{"list", "dictionary"} {
			t.Run(location+"/"+kind, func(t *testing.T) {
				for _, depth := range []int{4, 1024} {
					t.Run(fmt.Sprint(depth), func(t *testing.T) {
						prefix := "l"
						if kind == "dictionary" {
							prefix = "d1:a"
						}
						value := strings.Repeat(prefix, depth) + "i0e" + strings.Repeat("e", depth)
						document := "d4:info" + info + "2:zz" + value + "e"
						if location == "info" {
							document = "d4:info" + info[:len(info)-1] + "2:zz" + value + "ee"
						}
						got, err := GetMagnetFromBytes([]byte(document), true)
						if depth == 4 {
							if err != nil || got == nil || got.Name != "x" || got.Size != 1 {
								t.Fatalf("ordinary extension metadata rejected: %+v %v", got, err)
							}
						} else if err == nil || got != nil {
							t.Fatal("excessively nested torrent metadata was admitted")
						}
					})
				}
			})
		}
	}
}

func TestGetMagnetFromBytesPreservesV1MagnetParameters(t *testing.T) {
	source, err := os.ReadFile(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := metainfo.Load(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	info, err := metadata.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.NameUtf8 = "friendly title"
	metadata.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	const webSeed = "https://seed.invalid/file?a=1&b=2"
	metadata.UrlList = []string{webSeed}
	var encoded bytes.Buffer
	if err := metadata.Write(&encoded); err != nil {
		t.Fatal(err)
	}
	wantHash := metadata.HashInfoBytes().HexString()
	for _, strip := range []bool{false, true} {
		t.Run(fmt.Sprint(strip), func(t *testing.T) {
			got, err := GetMagnetFromBytes(encoded.Bytes(), strip)
			if err != nil {
				t.Fatal(err)
			}
			link, err := url.Parse(got.Link)
			if err != nil {
				t.Fatal(err)
			}
			query := link.Query()
			if got.InfoHash != wantHash || got.Name != info.Name || query.Get("dn") != info.NameUtf8 ||
				len(query["xt"]) != 1 || query.Get("xt") != "urn:btih:"+wantHash ||
				len(query["ws"]) != 1 || query.Get("ws") != webSeed {
				t.Fatalf("legacy magnet parameters changed: %+v", got)
			}
			wantTrackers := len(metadata.UpvertedAnnounceList().DistinctValues())
			if strip {
				wantTrackers = 0
			}
			if len(query["tr"]) != wantTrackers {
				t.Fatalf("trackers=%v, want %d", query["tr"], wantTrackers)
			}
		})
	}
}

func TestGetMagnetFromFile_MagnetFile_StripTrue(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0 // Should be 0 when stripping trackers

	torrentPath := testutil.GetTestMagnetPath()
	testMagnetFromFile(t, torrentPath, true, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFile_MagnetFile_StripFalse(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	torrentPath := testutil.GetTestMagnetPath()
	testMagnetFromFile(t, torrentPath, false, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromUrl_MagnetLink_StripTrue(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0

	// Load the magnet URL from the test file
	magnetUrl, err := testutil.GetTestMagnetContent()
	if err != nil {
		t.Fatalf("Failed to load magnet URL from test file: %v", err)
	}

	magnet, err := GetMagnetFromUrl(magnetUrl, true)
	if err != nil {
		t.Fatalf("GetMagnetFromUrl failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount, false)
	t.Logf("Generated clean magnet link: %s", magnet.Link)
}

func TestGetMagnetFromUrl_MagnetLink_StripFalse(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	// Load the magnet URL from the test file
	magnetUrl, err := testutil.GetTestMagnetContent()
	if err != nil {
		t.Fatalf("Failed to load magnet URL from test file: %v", err)
	}

	magnet, err := GetMagnetFromUrl(magnetUrl, false)
	if err != nil {
		t.Fatalf("GetMagnetFromUrl failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount, false)
	t.Logf("Generated magnet link with trackers: %s", magnet.Link)
}

func TestGetMagnetFromURLContextAcceptsRawInfoHashes(t *testing.T) {
	t.Parallel()

	const expectedInfoHash = "8a19577fb5f690970ca43a57ff1011ae202244b8"
	decoded, err := hex.DecodeString(expectedInfoHash)
	if err != nil {
		t.Fatal(err)
	}
	base32Hash := base32.StdEncoding.EncodeToString(decoded)

	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "uppercase hex", source: "  " + strings.ToUpper(expectedInfoHash) + "  "},
		{name: "base32", source: strings.ToLower(base32Hash)},
	} {
		t.Run(test.name, func(t *testing.T) {
			magnet, err := GetMagnetFromURLContext(
				context.Background(),
				test.source,
				false,
				MaxMetadataFileBytes,
			)
			if err != nil {
				t.Fatalf("GetMagnetFromURLContext failed: %v", err)
			}

			checkMagnet(
				t,
				magnet,
				expectedInfoHash,
				"",
				"magnet:?xt=urn:btih:"+expectedInfoHash,
				0,
				false,
			)
		})
	}
}

func TestGetMagnetFromURLContextRejectsInvalidTorrentSource(t *testing.T) {
	t.Parallel()

	_, err := GetMagnetFromURLContext(
		context.Background(),
		"not-a-torrent-source",
		false,
		MaxMetadataFileBytes,
	)
	if err == nil || err.Error() != "invalid torrent URL or infohash" {
		t.Fatalf("expected a generic invalid source error, got %v", err)
	}
}

// testMagnetFromHttpTorrent is a helper function for tests that use GetMagnetFromUrl with HTTP torrent links
func testMagnetFromHttpTorrent(t *testing.T, torrentPath string, rmTrackerUrls bool, expectedInfoHash, expectedName, expectedLink string, expectedTrackerCount int) {
	t.Helper()

	// Read the torrent file content
	torrentData, err := testutil.GetTestDataBytes(torrentPath)
	if err != nil {
		t.Fatalf("Failed to read torrent file: %v", err)
	}

	// Create a test HTTP server that serves the torrent file
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrentData)
	}))
	defer server.Close()

	// Test the function with the mock server URL
	magnet, err := GetMagnetFromUrl(server.URL, rmTrackerUrls)
	if err != nil {
		t.Fatalf("GetMagnetFromUrl failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount, true)

	// Log the result
	if rmTrackerUrls {
		t.Logf("Generated clean magnet link from HTTP torrent: %s", magnet.Link)
	} else {
		t.Logf("Generated magnet link with trackers from HTTP torrent: %s", magnet.Link)
	}
}

func TestGetMagnetFromUrl_TorrentLink_StripTrue(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0

	testMagnetFromHttpTorrent(t, "ubuntu-25.04-desktop-amd64.iso.torrent", true, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromUrl_TorrentLink_StripFalse(t *testing.T) {
	expectedInfoHash := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	testMagnetFromHttpTorrent(t, "ubuntu-25.04-desktop-amd64.iso.torrent", false, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFileBoundedRejectsOversizedTorrent(t *testing.T) {
	t.Parallel()

	file, err := os.Open(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	_, err = GetMagnetFromFileBounded(file, "sample.torrent", false, 8)
	if !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("expected ErrContentTooLarge, got %v", err)
	}
}

func TestGetMagnetFromURLContextRejectsStatusChunkedOversizeAndTimeout(t *testing.T) {
	t.Parallel()

	t.Run("status is redacted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusForbidden)
		}))
		defer server.Close()

		_, err := GetMagnetFromURLContext(
			context.Background(),
			server.URL+"/private/passkey?token=secret-value",
			false,
			32,
		)
		if err == nil || !strings.Contains(err.Error(), "status code 403") {
			t.Fatalf("expected status error, got %v", err)
		}
		for _, secret := range []string{"passkey", "secret-value"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("magnet error exposed %q: %v", secret, err)
			}
		}
	})

	t.Run("chunked oversize", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("123456789"))
		}))
		defer server.Close()

		_, err := GetMagnetFromURLContext(context.Background(), server.URL, false, 8)
		if !errors.Is(err, ErrContentTooLarge) {
			t.Fatalf("expected ErrContentTooLarge, got %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(
			_ http.ResponseWriter,
			r *http.Request,
		) {
			<-r.Context().Done()
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		_, err := GetMagnetFromURLContext(ctx, server.URL, false, 32)
		if err == nil || !strings.Contains(err.Error(), "request timed out") {
			t.Fatalf("expected timeout error, got %v", err)
		}
	})
}
