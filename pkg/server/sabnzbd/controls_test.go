package sabnzbd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/utils"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

const controlTestNZB = `<nzb><file poster="fixture" date="1" subject="&quot;video.mkv&quot; yEnc (1/1)"><groups><group>alt.test</group></groups><segments><segment number="1" bytes="100">part@fixture.invalid</segment></segments></file></nzb>`

func sabControlFields(mode string) url.Values {
	return url.Values{
		"mode": {mode}, "cat": {"tv"},
		"ma_username": {"https://sonarr.example.test"}, "ma_password": {"arr-token"},
	}
}

func requireSABControlError(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var body AddNZBResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status || body.Error == "" || len(body.NzoIds) != 0 {
		t.Fatalf("expected an explicit failure with no accepted IDs: %#v", body)
	}
	if status == http.StatusNotImplemented && !strings.Contains(body.Error, "not supported") {
		t.Fatalf("unsupported operation needs a clear explanation: %q", body.Error)
	}
}

func TestSABUnsupportedControlsLeaveQueueUnchanged(t *testing.T) {
	sab, router := newSABRouteTestServer(t)
	for i, state := range []storage.TorrentState{
		storage.EntryStateDownloading, storage.EntryStatePausedDL,
		storage.EntryStatePausedUP, storage.EntryStateError,
	} {
		if err := sab.manager.Queue().Add(&storage.Entry{
			InfoHash: fmt.Sprintf("control-%d", i), Name: fmt.Sprintf("Release.%d", i),
			Category: "tv", Protocol: config.ProtocolNZB, State: state, Progress: 0.25,
		}); err != nil {
			t.Fatal(err)
		}
	}
	queueSnapshot := func() string {
		t.Helper()
		entries := sab.manager.Queue().ListFilter("", "", "", nil, "name", false)
		if len(entries) != 4 {
			t.Fatalf("queue contains %d entries, want 4", len(entries))
		}
		data, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	before := queueSnapshot()
	for _, control := range []struct{ mode, name string }{
		{ModeQueue, "pause"}, {ModeQueue, "resume"}, {ModeQueue, "priority"},
		{ModeQueue, "unknown-action"}, {ModePause, ""}, {ModeResume, ""},
		{ModeConfig, "set_pause"}, {ModeGetConfig, "set_pause"},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, ids := range []string{"control-0,control-1", "missing", ""} {
				t.Run(method+"/"+control.mode+"/"+control.name+"/"+ids, func(t *testing.T) {
					values := sabControlFields(control.mode)
					values.Set("name", control.name)
					values.Set("value", ids)
					values.Set("value2", "-2")
					// Repeating an unsupported operation must never report success.
					for range 2 {
						request := newSABFormRequest(values)
						if method == http.MethodGet {
							request = httptest.NewRequest(method, "/api/?"+values.Encode(), nil)
						}
						response := httptest.NewRecorder()
						router.ServeHTTP(response, request)
						requireSABControlError(t, response, http.StatusNotImplemented)
						if after := queueSnapshot(); after != before {
							t.Fatal("rejected control changed a durable queue entry")
						}
					}
				})
			}
		}
	}
}

func newSABMultipartControlRequest(t *testing.T, fields url.Values) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A request-wide hold must reject the entire batch, not admit an earlier file.
	for i := range 2 {
		file, err := writer.CreateFormFile("name", fmt.Sprintf("Release.%d.nzb", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(controlTestNZB)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestSABRejectedAddPrioritiesNeverFetchOrAdmit(t *testing.T) {
	sab, router := newSABRouteTestServer(t)
	downloadArtifacts := func() string {
		t.Helper()
		entries, err := os.ReadDir(sab.downloadFolder)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return strings.Join(names, "\n")
	}
	// Manager startup creates ownership lock/adoption files. Record that
	// baseline so the assertion detects only artifacts from this request.
	beforeArtifacts := downloadArtifacts()
	var fetches atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(controlTestNZB))
	}))
	t.Cleanup(source.Close)
	for _, mode := range []string{ModeAddURL, ModeAddFile} {
		for _, priority := range []struct {
			name        string
			body, query []string
			status      int
		}{
			{"paused", []string{"-2"}, nil, http.StatusNotImplemented},
			{"stopped", []string{"-4"}, nil, http.StatusNotImplemented},
			{"whitespace", []string{" -2 "}, nil, http.StatusNotImplemented},
			{"numeric spelling", []string{"-02"}, nil, http.StatusNotImplemented},
			{"query", nil, []string{"-2"}, http.StatusNotImplemented},
			{"repeated hold last", []string{"0", "-2"}, nil, http.StatusNotImplemented},
			{"repeated hold first", []string{"-2", "0"}, nil, http.StatusNotImplemented},
			{"query hold shadowed", []string{"0"}, []string{"-2"}, http.StatusNotImplemented},
			{"form hold shadowed", []string{"-2"}, []string{"0"}, http.StatusNotImplemented},
			{"unknown", []string{"-3"}, nil, http.StatusBadRequest},
			{"invalid", []string{"paused"}, nil, http.StatusBadRequest},
			{"overflow", []string{"9999999999999999999999999"}, nil, http.StatusBadRequest},
		} {
			t.Run(mode+"/"+priority.name, func(t *testing.T) {
				fields := sabControlFields(mode)
				fields["priority"] = priority.body
				fields.Set("name", source.URL+"/first.nzb\n"+source.URL+"/second.nzb")
				request := newSABFormRequest(fields)
				if mode == ModeAddFile {
					fields.Del("name")
					request = newSABMultipartControlRequest(t, fields)
				}
				query := url.Values{"priority": priority.query}
				request.URL.RawQuery = query.Encode()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				requireSABControlError(t, response, priority.status)
				if fetches.Load() != 0 {
					t.Fatal("rejected priority fetched NZB metadata")
				}
				if entries := sab.manager.Queue().ListFilter("", "", "", nil, "", false); len(entries) != 0 {
					t.Fatalf("rejected priority admitted %d queue entries", len(entries))
				}
				if downloadArtifacts() != beforeArtifacts {
					t.Fatal("rejected priority created download artifacts")
				}
			})
		}
	}
}

func TestSABActivePrioritiesStillReachNormalAdmission(t *testing.T) {
	_, router := newSABRouteTestServer(t)
	var fetches atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(controlTestNZB))
	}))
	t.Cleanup(source.Close)
	for _, mode := range []string{ModeAddURL, ModeAddFile} {
		for _, priority := range []string{"", "-100", "-1", "0", "1", "2"} {
			t.Run(mode+"/"+priority, func(t *testing.T) {
				fields := sabControlFields(mode)
				if priority != "" {
					fields.Set("priority", priority)
				}
				fields.Set("name", source.URL+"/active.nzb")
				request := newSABFormRequest(fields)
				if mode == ModeAddFile {
					fields.Del("name")
					request = newSABMultipartControlRequest(t, fields)
				}
				before := fetches.Load()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				// This fixture deliberately has no NNTP provider. Reaching its
				// ordinary manager error proves active priorities were not blocked.
				if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "usenet not configured") {
					t.Fatalf("normal admission was not reached: %d %s", response.Code, response.Body.String())
				}
				if mode == ModeAddURL && fetches.Load() != before+1 {
					t.Fatal("ordinary addurl no longer fetches its metadata")
				}
			})
		}
	}
}

func TestSABPausedAdmissionRejectsBeforeManagerAccess(t *testing.T) {
	for _, mode := range []string{ModeAddURL, ModeAddFile} {
		t.Run(mode, func(t *testing.T) {
			request := newSABFormRequest(url.Values{"priority": {"-2"}})
			response := httptest.NewRecorder()
			if err := utils.ParseFormBounded(response, request, utils.MaxImportRequestBytes); err != nil {
				t.Fatal(err)
			}
			request = request.WithContext(context.WithValue(request.Context(), modeKey, mode))
			// A nil manager is intentional: the rejection must not depend on
			// provider configuration or enter either admission handler.
			(&SABnzbd{}).handleAPI(response, request)
			requireSABControlError(t, response, http.StatusNotImplemented)
		})
	}
}

func TestSABControlsPreserveRequestBoundaries(t *testing.T) {
	_, router := newSABRouteTestServer(t)
	config.Get().UseAuth = false
	for _, mode := range []string{ModePause, ModeResume, ModeQueue, ModeConfig} {
		t.Run("GET credentials/"+mode, func(t *testing.T) {
			fields := url.Values{"mode": {mode}, "name": {"pause"}}
			if mode == ModeConfig {
				fields.Set("name", "set_pause")
			}
			request := httptest.NewRequest(http.MethodGet, "http://192.0.2.20/api/?"+fields.Encode(), nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
			}
		})
	}
	config.Get().UseAuth = true
	t.Run("invalid credentials", func(t *testing.T) {
		fields := sabControlFields(ModeAddURL)
		fields.Set("priority", "-2")
		fields.Set("ma_password", "wrong")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, newSABFormRequest(fields))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.Code)
		}
	})
	t.Run("cross-site", func(t *testing.T) {
		request := newSABFormRequest(sabControlFields(ModePause))
		request.Header.Set("Origin", "https://untrusted.example.test")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", response.Code)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		fields := sabControlFields(ModeAddFile)
		fields.Set("priority", "-2")
		request := newSABMultipartControlRequest(t, fields)
		request.ContentLength = utils.MaxImportRequestBytes + 1
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		requireSABControlError(t, response, http.StatusRequestEntityTooLarge)
	})
}
