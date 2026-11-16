package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/logger"
	"github.com/Trifocals3537/tessarr/pkg/manager"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
	"github.com/google/uuid"
)

type par2Stub struct {
	starts, cancels int
	source          []byte
	err             error
}

func (p *par2Stub) PAR2Targets(context.Context, string, string) ([]usenet.PAR2Target, string, error) {
	return []usenet.PAR2Target{{NZBID: "synthetic-nzb", NZB: "Synthetic NZB", File: "example.mkv", Size: 100}}, "", p.err
}
func (p *par2Stub) PAR2Jobs() ([]recovery.Job, error) { return []recovery.Job{}, p.err }
func (p *par2Stub) StartPAR2(ctx context.Context, id, name string, b []byte) (recovery.Job, error) {
	p.starts++
	p.source = bytes.Clone(b)
	return recovery.Job{ID: uuid.NewString(), NZBID: id, File: name, State: "queued"}, p.err
}
func (p *par2Stub) CancelPAR2(string) error { p.cancels++; return p.err }

func TestPAR2StartBoundsValidationAndSourceUpload(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		length     int64
		want       int
	}{
		{"valid", `{"nzb_id":"synthetic-nzb","file":"example.mkv"}`, 0, 202},
		{"unknown field", `{"nzb_id":"synthetic-nzb","file":"example.mkv","url":"https://unused.invalid/"}`, 0, 400},
		{"path escape", `{"nzb_id":"../outside","file":"example.mkv"}`, 0, 400},
		{"file escape", `{"nzb_id":"synthetic-nzb","file":"../example.mkv"}`, 0, 400},
		{"trailing JSON", `{"nzb_id":"synthetic-nzb","file":"example.mkv"}{}`, 0, 400},
		{"oversized JSON", strings.Repeat(" ", 5000), 0, 413},
		{"oversized request", `{}`, recovery.MaxSourceBytes + (64 << 10) + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &par2Stub{}
			s := &Server{par2: stub}
			request := httptest.NewRequest("POST", "/api/usenet/recovery/jobs", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			if tc.length > 0 {
				request.ContentLength = tc.length
			}
			w := httptest.NewRecorder()
			s.handleStartPAR2(w, request)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want != 202 && stub.starts != 0 {
				t.Fatal("invalid request reached worker")
			}
		})
	}
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	_ = writer.WriteField("nzb_id", "synthetic-nzb")
	_ = writer.WriteField("file", "example.mkv")
	part, err := writer.CreateFormFile("source", "ignored-client-filename.nzb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("<nzb>synthetic fixture</nzb>"))
	_ = writer.Close()
	stub := &par2Stub{}
	request := httptest.NewRequest("POST", "/api/usenet/recovery/jobs", &b)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	(&Server{par2: stub}).handleStartPAR2(w, request)
	if w.Code != 202 || string(stub.source) != "<nzb>synthetic fixture</nzb>" {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<nzb>") {
		t.Fatal("source echoed in API response")
	}
}

func TestPAR2APIReportsSafeErrorsAndSerializesPreparation(t *testing.T) {
	stub := &par2Stub{err: errors.New("private synthetic provider details")}
	s := &Server{par2: stub}
	w := httptest.NewRecorder()
	s.handlePAR2Jobs(w, httptest.NewRequest("GET", "/api/usenet/recovery/jobs", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "private synthetic") {
		t.Fatal("internal failure leaked")
	}
	for _, err := range []error{recovery.ErrSourceRequired, recovery.ErrStale, recovery.ErrParity, recovery.ErrBusy} {
		w := httptest.NewRecorder()
		par2Error(w, err)
		var response map[string]string
		if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil || response["code"] == "" || response["error"] == "" {
			t.Fatalf("missing structured error: %s", w.Body.String())
		}
	}
	s.par2Admission.Lock()
	w = httptest.NewRecorder()
	s.handleStartPAR2(w, httptest.NewRequest("POST", "/api/usenet/recovery/jobs", strings.NewReader(strings.Repeat("x", 1<<20))))
	s.par2Admission.Unlock()
	if w.Code != 409 || stub.starts != 0 {
		t.Fatal("concurrent preparation was not bounded")
	}
}

func TestPAR2RoutesRequireAuthenticationAndProtectWrites(t *testing.T) {
	old := config.GetMainPath()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(func() { config.SetConfigPath(old) })
	cfg := config.Get()
	cfg.DownloadFolder = t.TempDir()
	cfg.Debrids = []config.Debrid{{Name: "realdebrid", APIKey: "synthetic"}}
	cfg.UseAuth = true
	cfg.Auth = &config.Auth{Username: "admin", Password: "hash", APIToken: "synthetic-test-token", SessionVersion: 1}
	m := manager.New()
	t.Cleanup(func() { m.Stop(); _ = logger.Close() })
	s := New(m)
	stub := &par2Stub{}
	s.par2 = stub
	cookie := authenticatedTestCookie(t, s, cfg.Auth.SessionVersion)
	request := func(method, path, body, origin string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://tessarr.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if authenticated {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/api/usenet/recovery/jobs"}, {"GET", "/api/usenet/recovery/targets?q=synthetic"}, {"POST", "/api/usenet/recovery/jobs"}, {"DELETE", "/api/usenet/recovery/jobs/" + uuid.NewString()}} {
		if w := request(tc.method, tc.path, `{"nzb_id":"synthetic-nzb","file":"example.mkv"}`, "", false); w.Code != 401 {
			t.Fatalf("anonymous %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if w := request("GET", "/api/usenet/recovery/jobs", "", "", true); w.Code != 200 {
		t.Fatalf("authenticated GET: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", "/api/usenet/recovery/jobs", `{"nzb_id":"synthetic-nzb","file":"example.mkv"}`, "http://tessarr.example", true); w.Code != 202 {
		t.Fatalf("authenticated start: %d %s", w.Code, w.Body.String())
	}
	before := stub.starts
	if w := request("POST", "/api/usenet/recovery/jobs", `{"nzb_id":"synthetic-nzb","file":"example.mkv"}`, "https://unrelated.example", true); w.Code != 403 || stub.starts != before {
		t.Fatalf("cross-origin write: %d %s", w.Code, w.Body.String())
	}
	// Non-browser API clients use the existing bearer-token authentication.
	r := httptest.NewRequest("POST", "http://tessarr.example/api/usenet/recovery/jobs", strings.NewReader(`{"nzb_id":"synthetic-nzb","file":"example.mkv"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer synthetic-test-token")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("bearer-token start: %d %s", w.Code, w.Body.String())
	}
}
