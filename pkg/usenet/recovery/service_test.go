package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func waitForJob(t *testing.T, s *Service, id string, condition func(Job) bool) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := s.Jobs()
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.ID == id && condition(j) {
				return j
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	jobs, _ := s.Jobs()
	t.Fatalf("job did not reach expected state: %+v", jobs)
	return Job{}
}

func TestManualUploadIsRequiredBoundAndReusable(t *testing.T) {
	h := newFixtureHost(t)
	original := bytes.Clone(h.target.Source)
	h.target.Source = nil
	blocked := make(chan struct{})
	h.idle = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-blocked:
			return nil
		}
	}
	root := filepath.Join(t.TempDir(), "recovery")
	s, err := NewService(t.Context(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil); !errors.Is(err, ErrSourceRequired) {
		t.Fatalf("missing-source result: %v", err)
	}
	wrong := bytes.Replace(original, []byte("fileA.bin-0@fixture.invalid"), []byte("unrelated-0@fixture.invalid"), 1)
	if _, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, wrong); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong source accepted: %v", err)
	}
	j, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, original)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil || duplicate.ID != j.ID {
		t.Fatalf("duplicate admission: %v %+v", err, duplicate)
	}
	if other, err := NewService(t.Context(), root, h); err == nil {
		_ = other.Close()
		t.Fatal("two owners acquired the same store")
	}
	if err := s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, j.ID, func(j Job) bool { return j.State == "cancelled" })
	close(blocked)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewService(t.Context(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.Jobs()
	if err != nil || len(jobs) != 1 || jobs[0].State != "cancelled" {
		t.Fatalf("cancelled job resumed: %+v %v", jobs, err)
	}
	newJob, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitForJob(t, s, newJob.ID, func(j Job) bool { return j.State == "succeeded" || j.State == "failed" })
	if result.State != "succeeded" {
		t.Fatal(result.Reason)
	}
}

func TestShutdownPersistsUnfinishedWorkAndResumes(t *testing.T) {
	h := newFixtureHost(t)
	h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
	entered := make(chan struct{}, 1)
	h.idle = func(ctx context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	root := filepath.Join(t.TempDir(), "recovery")
	s, err := NewService(t.Context(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.idle = nil
	s, err = NewService(t.Context(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result := waitForJob(t, s, j.ID, func(j Job) bool { return j.State == "succeeded" || j.State == "failed" })
	if result.State != "succeeded" || result.Attempts != 2 || result.ReadBytes < restartReadAllowance {
		t.Fatalf("resume: %+v", result)
	}
}

func TestCrashRecoveryAcrossPublicationBoundaries(t *testing.T) {
	for _, point := range []string{"before", "after"} {
		t.Run(point, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "recovery")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryCrashChild$", "-test.v")
			cmd.Env = append(os.Environ(), "TESSARR_PAR2_CRASH_ROOT="+root, "TESSARR_PAR2_CRASH_POINT="+point)
			out, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 77 {
				t.Fatalf("crash fixture: %v %s", err, out)
			}
			h := newFixtureHost(t)
			h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
			h.target.Source = nil
			s, err := NewService(t.Context(), root, h)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			jobs, _ := s.Jobs()
			if len(jobs) != 1 {
				t.Fatalf("lost durable job: %+v", jobs)
			}
			result := waitForJob(t, s, jobs[0].ID, func(j Job) bool { return j.State == "succeeded" || j.State == "failed" })
			if result.State != "succeeded" || result.Attempts != 2 || !s.Store().Covers(h.target.Generation, 0, 512) {
				t.Fatalf("crash resume failed: %+v", result)
			}
		})
	}
}

func TestRecoveryCrashChild(t *testing.T) {
	root := os.Getenv("TESSARR_PAR2_CRASH_ROOT")
	if root == "" {
		return
	}
	h := newFixtureHost(t)
	h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
	s, err := OpenService(context.Background(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	hook := func() error { os.Exit(77); return nil }
	if os.Getenv("TESSARR_PAR2_CRASH_POINT") == "before" {
		s.store.beforePointer = hook
	} else {
		s.store.afterPointer = hook
	}
	if _, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil); err != nil {
		t.Fatal(err)
	}
	s.Begin()
	select {
	case <-s.done:
		t.Fatal("worker exited before injected crash")
	case <-time.After(8 * time.Second):
		t.Fatal("crash boundary not reached")
	}
}

func TestCorruptJobStateIsPreservedAndExpiredWorkDoesNotRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recovery")
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicBytes(store.root, "jobs.json", []byte("broken synthetic state")); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	h := newFixtureHost(t)
	if s, err := NewService(t.Context(), root, h); err == nil {
		_ = s.Close()
		t.Fatal("accepted corrupt state")
	}
	if b, err := os.ReadFile(filepath.Join(root, "jobs.json")); err != nil || string(b) != "broken synthetic state" {
		t.Fatal("corrupt evidence was overwritten")
	}
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j := Job{ID: uuid.NewString(), NZBID: h.target.ID, File: h.target.File.Name, Generation: h.target.Generation, State: "running", CreatedAt: now.Add(-JobTimeout - time.Minute), Deadline: now.Add(-time.Minute), Attempts: 1}
	if err := atomicJSON(store.root, "jobs.json", savedJobs{Version: 1, Jobs: []Job{j}}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	s, err := NewService(t.Context(), root, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result := waitForJob(t, s, j.ID, func(j Job) bool { return j.State == "failed" })
	if !strings.Contains(result.Reason, "time limit") {
		t.Fatal(result.Reason)
	}
}

func TestRemovingNZBForgetsOnlyItsOwnedRecovery(t *testing.T) {
	h := newFixtureHost(t)
	h.fail["fileA.bin-0@fixture.invalid"] = missingArticle()
	s, err := NewService(t.Context(), filepath.Join(t.TempDir(), "recovery"), h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitForJob(t, s, j.ID, func(j Job) bool { return j.State == "succeeded" || j.State == "failed" })
	if result.State != "succeeded" {
		t.Fatal(result.Reason)
	}
	if err := s.ForgetNZB("unrelated-nzb"); err != nil {
		t.Fatal(err)
	}
	if s.store.Revision(h.target.Generation) == "" {
		t.Fatal("unrelated removal discarded patches")
	}
	if err := s.ForgetNZB(h.target.ID); err != nil {
		t.Fatal(err)
	}
	if s.store.Revision(h.target.Generation) != "" || s.store.HasTarget(h.target.ID, h.target.File.Name) {
		t.Fatal("deleted NZB retained its patch reference")
	}
	if _, err := s.store.ReadSource(h.target.Generation); !errors.Is(err, ErrSourceRequired) {
		t.Fatalf("original source retained: %v", err)
	}
}

func TestCancellationFencesPendingPublication(t *testing.T) {
	h := newFixtureHost(t)
	s, err := OpenService(t.Context(), filepath.Join(t.TempDir(), "recovery"), h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ready, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.run = func(ctx context.Context, target Target, host Host, store *Store, read int64, report progress) error {
		defer close(finished)
		close(ready)
		select {
		case <-proceed:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
		return store.Publish(ctx, target.Generation, target.File.Size, testSet, testMD5, []Block{{Offset: 0, Data: []byte("fixture")}}, nil, Identity{NZBID: target.ID, File: target.File.Name})
	}
	j, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Begin()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not approach publication")
	}
	if err := s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Store().Revision(h.target.Generation) != "" {
		t.Fatal("publication occurred after cancellation returned")
	}
	jobs, err := s.Jobs()
	if err != nil || jobs[0].State != "cancelled" {
		t.Fatalf("cancellation changed: %+v %v", jobs, err)
	}
}

func TestHistoryPruningDoesNotMoveActiveJobProgress(t *testing.T) {
	h := newFixtureHost(t)
	s, err := OpenService(t.Context(), filepath.Join(t.TempDir(), "recovery"), h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	for range 31 {
		s.jobs = append(s.jobs, Job{ID: uuid.NewString(), NZBID: h.target.ID, File: h.target.File.Name, Generation: h.target.Generation, State: "succeeded", Stage: "Verified", CreatedAt: now, Deadline: now.Add(JobTimeout), FinishedAt: now})
	}
	ready, proceed := make(chan struct{}), make(chan struct{})
	s.run = func(ctx context.Context, target Target, host Host, store *Store, read int64, report progress) error {
		close(ready)
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
		return report("Synthetic progress after pruning", 123)
	}
	active, err := s.Start(t.Context(), h.target.ID, h.target.File.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Begin()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	h.mu.Lock()
	h.target.File.Name = "second.mkv"
	h.target.Generation = Generation(h.target.File)
	h.mu.Unlock()
	queued, err := s.Start(t.Context(), h.target.ID, "second.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	result := waitForJob(t, s, active.ID, func(j Job) bool { return j.State == "succeeded" || j.State == "failed" })
	if result.State != "succeeded" || result.ReadBytes != 123 {
		t.Fatalf("active progress moved to another job: %+v", result)
	}
	jobs, err := s.Jobs()
	if err != nil || len(jobs) != 32 {
		t.Fatalf("history bound: %d %v", len(jobs), err)
	}
}
