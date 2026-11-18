package recovery

import (
	"context"
	"errors"
	"os"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/Trifocals3537/tessarr/internal/filelock"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

var ErrBusy = errors.New("recovery is busy; try again shortly")
var ErrNotFound = errors.New("recovery job not found")
var ErrState = errors.New("recovery state could not be saved")
var ErrUnavailable = errors.New("manual recovery is unavailable; check the application logs")
var ErrPanicked = errors.New("recovery stopped unexpectedly; check the application logs")

// Progress is checkpointed at least once per second of incoming data. Include
// one bounded write ahead of the pacer as well as that interval after a crash.
const restartReadAllowance = DownloadBytesPerSecond + MaxArticleBytes

type Job struct {
	ID         string    `json:"id"`
	NZBID      string    `json:"nzb_id"`
	File       string    `json:"file"`
	Generation string    `json:"generation"`
	State      string    `json:"state"`
	Stage      string    `json:"stage"`
	Reason     string    `json:"reason,omitempty"`
	ReadBytes  int64     `json:"read_bytes"`
	Attempts   int       `json:"attempts"`
	CreatedAt  time.Time `json:"created_at"`
	Deadline   time.Time `json:"deadline"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}
type savedJobs struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

type Service struct {
	mu            sync.Mutex
	store         *Store
	owner         *filelock.Lock
	host          Host
	logger        zerolog.Logger
	jobs          []Job
	ctx           context.Context
	cancel        context.CancelFunc
	runningCancel context.CancelFunc
	runningID     string
	notify        chan struct{}
	done          chan struct{}
	fatal         error
	closeOnce     sync.Once
	startOnce     sync.Once
	closeErr      error
	// Tests can stop a synthetic run at durable boundaries.
	run func(context.Context, Target, Host, *Store, int64, progress) error
}

func NewService(parent context.Context, path string, host Host, logger zerolog.Logger) (*Service, error) {
	s, err := OpenService(parent, path, host, logger)
	if err == nil {
		s.Begin()
	}
	return s, err
}

// OpenService lets the owner finish wiring callbacks before saved jobs resume.
func OpenService(parent context.Context, path string, host Host, logger zerolog.Logger) (*Service, error) {
	root, absolute, err := safepath.EnsureOpenRoot(path, 0o700)
	if err != nil {
		return nil, err
	}
	// Lock the same pinned directory used for all subsequent state access.
	// Never re-resolve an absolute lock path or clean state before ownership.
	owner, err := filelock.TryAcquire(root, ".owner.lock", 0o600)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if err := safepath.VerifyOpenRoot(root, absolute); err != nil {
		return nil, errors.Join(err, owner.Unlock(), root.Close())
	}
	store, err := loadStore(root, absolute)
	if err != nil {
		_ = owner.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Service{store: store, owner: owner, host: host, logger: logger, ctx: ctx, cancel: cancel, notify: make(chan struct{}, 1), done: make(chan struct{}), run: Run}
	if err := s.load(); err != nil {
		cancel()
		_ = store.Close()
		_ = owner.Unlock()
		return nil, err
	}
	return s, nil
}

func (s *Service) Begin() { s.startOnce.Do(func() { go s.worker(); s.wake() }) }

type sourceHost struct {
	Host
	store *Store
}

func (h sourceHost) Snapshot(ctx context.Context, id, name string) (Target, error) {
	t, err := h.Host.Snapshot(ctx, id, name)
	if err != nil {
		return Target{}, err
	}
	source, e := h.store.ReadSource(t.Generation)
	if e == nil {
		t.Source = source
	} else if !errors.Is(e, ErrSourceRequired) {
		return Target{}, e
	}
	if len(t.Source) == 0 {
		return Target{}, ErrSourceRequired
	}
	return t, nil
}

func (s *Service) load() error {
	var saved savedJobs
	err := s.store.readJSON("jobs.json", &saved, 128<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s.store.cleanSources(map[string]bool{})
	}
	if err != nil {
		return err
	}
	if saved.Version != 1 || len(saved.Jobs) > 32 {
		return errors.New("invalid recovery job history")
	}
	seen := make(map[string]bool)
	for _, j := range saved.Jobs {
		if uuid.Validate(j.ID) != nil || seen[j.ID] || j.NZBID == "" || len(j.NZBID) > 255 || j.File == "" || len(j.File) > 1024 || !digestString(j.Generation, 64) || j.ReadBytes < 0 || j.ReadBytes > MaxReadBytes || j.Attempts < 0 || j.Attempts > 3 || j.CreatedAt.IsZero() || j.Deadline.Before(j.CreatedAt) || j.Deadline.Sub(j.CreatedAt) > JobTimeout {
			return errors.New("invalid saved recovery job")
		}
		seen[j.ID] = true
		switch j.State {
		case "running", "queued":
			j.State = "queued"
			j.Stage = "Waiting to resume"
			if j.Attempts >= 3 || time.Now().After(j.Deadline) {
				j.State = "failed"
				j.Stage = "Stopped"
				j.Reason = "Recovery was interrupted too often or exceeded its time limit."
				j.FinishedAt = time.Now()
			}
		case "succeeded", "failed", "cancelled":
		default:
			return errors.New("invalid saved recovery state")
		}
		s.jobs = append(s.jobs, j)
	}
	allowed := make(map[string]bool)
	for _, j := range s.jobs {
		allowed[j.Generation] = true
	}
	if err := s.store.cleanSources(allowed); err != nil {
		return err
	}
	return s.save()
}

func (s *Service) save() error {
	if err := atomicJSON(s.store.root, "jobs.json", savedJobs{Version: 1, Jobs: s.jobs}); err != nil {
		return errors.Join(ErrState, err)
	}
	return nil
}
func (s *Service) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}
func (s *Service) Store() *Store { return s.store }
func (s *Service) find(id string) *Job {
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			return &s.jobs[i]
		}
	}
	return nil
}

func (s *Service) Start(ctx context.Context, id, file string, original []byte) (Job, error) {
	target, err := s.host.Snapshot(ctx, id, file)
	if err != nil {
		return Job{}, err
	}
	if err := Eligible(&target.File); err != nil {
		return Job{}, err
	}
	if target.Generation != Generation(target.File) {
		return Job{}, ErrStale
	}
	if len(original) > 0 {
		target.Source = original
	} else if b, e := s.store.ReadSource(target.Generation); e == nil {
		target.Source = b
	} else if !errors.Is(e, ErrSourceRequired) {
		return Job{}, e
	}
	if err := ValidateSource(target.Source, target.File); err != nil {
		return Job{}, err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	// A target may have been removed or replaced while its upload was parsed.
	// Removal holds this same lock through metadata deletion.
	current, err := s.host.Snapshot(ctx, id, file)
	if err != nil {
		return Job{}, err
	}
	if current.Generation != target.Generation {
		return Job{}, ErrStale
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.fatal != nil {
		return Job{}, ErrState
	}
	queued := 0
	for _, j := range s.jobs {
		if j.State == "queued" || j.State == "running" {
			queued++
			if j.Generation == target.Generation {
				return j, nil
			}
		}
	}
	if queued >= 8 {
		return Job{}, ErrBusy
	}
	if err := s.store.writeSource(target.Generation, target.Source); err != nil {
		return Job{}, err
	}
	previous := append([]Job(nil), s.jobs...)
	if len(s.jobs) >= 32 {
		removed := false
		for i, j := range s.jobs {
			if j.State != "running" && j.State != "queued" {
				s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			return Job{}, ErrBusy
		}
	}
	now := time.Now().UTC()
	j := Job{ID: uuid.NewString(), NZBID: id, File: file, Generation: target.Generation, State: "queued", Stage: "Queued", CreatedAt: now, Deadline: now.Add(JobTimeout)}
	s.jobs = append(s.jobs, j)
	if err := s.save(); err != nil {
		s.jobs = previous
		return Job{}, err
	}
	allowed := make(map[string]bool)
	for _, item := range s.jobs {
		allowed[item.Generation] = true
	}
	// Collection cannot revoke an already persisted admission. Upload storage
	// is independently capped even if old-source collection needs a later retry.
	_ = s.store.cleanSources(allowed)
	s.wake()
	return j, nil
}

func (s *Service) Jobs() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return nil, ErrState
	}
	jobs := append([]Job{}, s.jobs...)
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return jobs, nil
}

func (s *Service) Cancel(id string) error {
	// Serialize cancellation with the atomic pointer publication boundary.
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		j := &s.jobs[i]
		if j.ID != id {
			continue
		}
		if j.State != "queued" && j.State != "running" {
			return nil
		}
		old := *j
		j.State = "cancelled"
		j.Stage = "Cancelled"
		j.Reason = "Cancelled by request."
		j.FinishedAt = time.Now().UTC()
		if err := s.save(); err != nil {
			*j = old
			return err
		}
		if s.runningID == id && s.runningCancel != nil {
			s.runningCancel()
		}
		return nil
	}
	return ErrNotFound
}

func safeReason(err error) string {
	var provider *nntp.Error
	switch {
	case errors.Is(err, ErrStale):
		return ErrStale.Error()
	case errors.Is(err, ErrLimit):
		return ErrLimit.Error()
	case errors.Is(err, ErrUnsupported):
		return ErrUnsupported.Error()
	case errors.Is(err, ErrPanicked):
		return ErrPanicked.Error()
	case errors.Is(err, ErrSourceRequired):
		return ErrSourceRequired.Error()
	case errors.Is(err, ErrState):
		return ErrState.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "Recovery exceeded its time limit."
	case errors.As(err, &provider) && !nntp.IsArticleNotFoundError(err):
		return ErrTemporary.Error()
	case errors.Is(err, ErrParity):
		return ErrParity.Error()
	default:
		return "Recovery could not finish. Check provider availability and the application logs before retrying."
	}
}

func (s *Service) ForgetNZB(id string) error {
	return s.RemoveNZB(id, nil)
}

// RemoveNZB fences admissions and publication through the caller's metadata
// removal. Snapshot is rechecked under this lock before an upload is saved.
func (s *Service) RemoveNZB(id string, remove func() error) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if err := s.forgetNZBLocked(id); err != nil {
		return err
	}
	if remove != nil {
		return remove()
	}
	return nil
}

func (s *Service) forgetNZBLocked(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]Job(nil), s.jobs...)
	generations := make(map[string]bool)
	changed := false
	for i := range s.jobs {
		j := &s.jobs[i]
		if j.NZBID != id {
			continue
		}
		generations[j.Generation] = true
		if j.State == "queued" || j.State == "running" {
			changed = true
			j.State = "cancelled"
			j.Stage = "Cancelled"
			j.Reason = "The NZB was removed."
			j.FinishedAt = time.Now().UTC()
		}
	}
	if changed {
		if err := s.save(); err != nil {
			s.jobs = previous
			return err
		}
	}
	if len(generations) == 0 {
		owned := false
		for _, nzbID := range s.store.ids {
			if nzbID == id {
				owned = true
				break
			}
		}
		if !owned {
			return nil
		}
	}
	if j := s.find(s.runningID); j != nil && j.NZBID == id && s.runningCancel != nil {
		s.runningCancel()
	}
	return s.store.forget(id, generations)
}

func (s *Service) worker() {
	defer close(s.done)
	for {
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		index := -1
		for i, j := range s.jobs {
			if j.State == "queued" {
				index = i
				break
			}
		}
		if index < 0 {
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return
			case <-s.notify:
			}
			continue
		}
		j := &s.jobs[index]
		if time.Now().After(j.Deadline) || j.Attempts >= 3 {
			j.State = "failed"
			j.Stage = "Stopped"
			j.Reason = "Recovery exceeded its restart or time limit."
			j.FinishedAt = time.Now().UTC()
			if err := s.save(); err != nil {
				s.fatal = err
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			continue
		}
		j.State = "running"
		j.Stage = "Starting recovery"
		j.Attempts++
		// Charge lost progress conservatively instead of resetting the read cap.
		if j.Attempts > 1 {
			j.ReadBytes = min(MaxReadBytes, j.ReadBytes+restartReadAllowance)
		}
		if err := s.save(); err != nil {
			s.fatal = err
			s.mu.Unlock()
			return
		}
		job := *j
		ctx, cancel := context.WithDeadline(s.ctx, j.Deadline)
		s.runningID = j.ID
		s.runningCancel = cancel
		s.mu.Unlock()
		lastSave := time.Now()
		lastStage := job.Stage
		report := func(stage string, read int64) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			current := s.find(job.ID)
			if current == nil || current.State != "running" {
				return context.Canceled
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			current.Stage = stage
			current.ReadBytes = read
			if stage != lastStage || time.Since(lastSave) >= time.Second {
				if err := s.save(); err != nil {
					return err
				}
				lastSave = time.Now()
				lastStage = stage
			}
			return nil
		}
		host := sourceHost{Host: s.host, store: s.store}
		target, err := host.Snapshot(ctx, job.NZBID, job.File)
		if err == nil && target.Generation != job.Generation {
			err = ErrStale
		}
		if err == nil {
			err = s.runSafely(ctx, target, host, job.ReadBytes, report)
		}
		cancel()
		s.mu.Lock()
		current := s.find(job.ID)
		s.runningID = ""
		s.runningCancel = nil
		if current != nil && current.State == "running" {
			if s.ctx.Err() != nil {
				current.State = "queued"
				current.Stage = "Waiting to resume"
			} else if err != nil {
				current.State = "failed"
				current.Stage = "Stopped"
				current.Reason = safeReason(err)
				current.FinishedAt = time.Now().UTC()
			} else {
				current.State = "succeeded"
				current.Stage = "Recovery verified"
				current.FinishedAt = time.Now().UTC()
			}
		}
		if e := s.save(); e != nil {
			s.fatal = e
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

func (s *Service) runSafely(ctx context.Context, target Target, host Host, alreadyRead int64, report progress) (err error) {
	defer func() {
		if recover() != nil {
			s.logger.Error().Bytes("stack", debug.Stack()).Msg("Manual PAR2 recovery panicked")
			err = ErrPanicked
		}
	}()
	return s.run(ctx, target, host, s.store, alreadyRead, report)
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() { s.cancel(); s.Begin(); <-s.done; s.closeErr = errors.Join(s.store.Close(), s.owner.Unlock()) })
	return s.closeErr
}
