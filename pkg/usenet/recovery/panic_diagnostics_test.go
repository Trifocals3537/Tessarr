package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type unformattableRecoveryPanic struct{}

func (unformattableRecoveryPanic) String() string { panic("do not format recovery secrets") }

func TestManualRecoveryPanicIsAnUnexpectedFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"string", "private PAR2 fixture payload"},
		{"unformattable", unformattableRecoveryPanic{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFixtureHost(t)
			var capture bytes.Buffer
			root := filepath.Join(t.TempDir(), "recovery")
			service, err := OpenService(t.Context(), root, host, zerolog.New(&capture))
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			service.run = func(context.Context, Target, Host, *Store, int64, progress) error {
				panic(tc.value)
			}
			job, err := service.Start(t.Context(), host.target.ID, host.target.File.Name, nil)
			if err != nil {
				t.Fatal(err)
			}
			service.Begin()
			got := waitForJob(t, service, job.ID, func(j Job) bool { return j.State == "failed" })
			if got.Reason != ErrPanicked.Error() {
				t.Fatalf("panic reason = %q, want %q", got.Reason, ErrPanicked)
			}
			var event struct {
				Level   string `json:"level"`
				Stack   string `json:"stack"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(capture.Bytes(), &event); err != nil {
				t.Fatalf("missing or invalid panic diagnostic: %v", err)
			}
			if event.Level != "error" || event.Message != "Manual PAR2 recovery panicked" || !strings.Contains(event.Stack, "panic_diagnostics_test.go") {
				t.Fatalf("panic diagnostic lacks originating stack: %+v", event)
			}
			for _, secret := range []string{"private PAR2 fixture payload", "do not format recovery secrets", host.target.ID} {
				if strings.Contains(capture.String(), secret) || strings.Contains(got.Reason, secret) {
					t.Fatal("panic diagnostic or job reason included a private value")
				}
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewService(t.Context(), root, host, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			jobs, err := reopened.Jobs()
			if err != nil || len(jobs) != 1 || jobs[0].State != "failed" || jobs[0].Reason != ErrPanicked.Error() {
				t.Fatalf("restart lost sanitized panic failure: jobs=%+v err=%v", jobs, err)
			}
		})
	}
}

func TestManualRecoveryUnsupportedResultRemainsDistinctFromPanic(t *testing.T) {
	host := newFixtureHost(t)
	var capture bytes.Buffer
	service, err := OpenService(t.Context(), filepath.Join(t.TempDir(), "recovery"), host, zerolog.New(&capture))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.run = func(context.Context, Target, Host, *Store, int64, progress) error { return ErrUnsupported }
	job, err := service.Start(t.Context(), host.target.ID, host.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.Begin()
	got := waitForJob(t, service, job.ID, func(j Job) bool { return j.State == "failed" })
	if got.Reason != ErrUnsupported.Error() || capture.Len() != 0 {
		t.Fatalf("unsupported result changed or emitted panic diagnostic: reason=%q, log_bytes=%d", got.Reason, capture.Len())
	}
}
