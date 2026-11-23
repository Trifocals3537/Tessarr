package manager

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Trifocals3537/tessarr/internal/customerror"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/manager/link"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

const (
	readFailureLogInterval = time.Minute
	readFailureLogCapacity = 4096
)

func classifyTerminalReadFailure(err error) (class string, retryable bool) {
	if isDeferredReadFailure(err) {
		return "local_cooldown", true
	}
	err = withoutDeferredArticleFailures(err)
	switch {
	case err == nil:
		return "none", false
	case errors.Is(err, context.Canceled):
		return "canceled", false
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	case errors.Is(err, storage.ErrInvalidNZBMap):
		return "invalid_nzb_map", false
	case nntp.IsArticleNotFoundError(err):
		return articleReadFailureClass(nntp.ArticleUnavailableReasonOf(err)), false
	case nntp.IsAuthenticationError(err):
		return "provider_auth", false
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof", true
	}
	if isRepeatedLinkRejection(err) {
		return link.CodeRepeatedLinkRejection, true
	}
	if allReadFailureCauses(err, explicitUnavailableLink) {
		return "content_missing", false
	}

	var customErr *customerror.Error
	if errors.As(err, &customErr) {
		switch customErr.HTTPStatus() {
		case 401, 403:
			return "provider_auth", false
		case 404, 410:
			return "content_missing", false
		case 429:
			return "rate_limited", true
		}
		if customErr.IsPermanent() {
			return "permanent", false
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout", true
		}
		return "transport", true
	}
	if customerror.IsRetriableError(err) {
		return "transient", true
	}
	return "other", false
}

func articleReadFailureClass(reason nntp.ArticleUnavailableReason) string {
	switch {
	case reason == nntp.ArticleAbsent:
		return "content_missing"
	case reason.InvalidData():
		return "invalid_article_data"
	default:
		return "article_unavailable"
	}
}

func readArticleFailureReason(err error) string {
	return string(nntp.ArticleUnavailableReasonOf(withoutDeferredArticleFailures(err)))
}

// A joined failure may include both a cached Usenet result and a newly
// observed result. The latter determines diagnostic class and reason.
func withoutDeferredArticleFailures(err error) error {
	var deferred *customerror.DeferredArticleFailure
	if !errors.As(err, &deferred) {
		return err
	}
	if isDeferredReadFailure(err) {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var fresh []error
		for _, cause := range joined.Unwrap() {
			if remaining := withoutDeferredArticleFailures(cause); remaining != nil {
				fresh = append(fresh, remaining)
			}
		}
		return errors.Join(fresh...)
	}
	return withoutDeferredArticleFailures(errors.Unwrap(err))
}

// A failover error can contain multiple provider attempts. An observation is
// conclusive (or merely deferred) only when every underlying cause agrees.
// Inspect joined errors before unwrapping ordinary context wrappers.
func allReadFailureCauses(err error, matches func(error) bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !allReadFailureCauses(cause, matches) {
				return false
			}
		}
		return true
	}
	if matches(err) {
		return true
	}
	return allReadFailureCauses(errors.Unwrap(err), matches)
}

func isDeferredReadFailure(err error) bool {
	return allReadFailureCauses(err, func(cause error) bool {
		switch e := cause.(type) {
		case streamFileCircuitOpenError, *streamFileCircuitOpenError, *customerror.DeferredArticleFailure:
			return true
		case *link.Error:
			return e != nil && e.Code == link.CodeLinkRefreshCooldown
		default:
			return false
		}
	})
}

func isRepeatedLinkRejection(err error) bool {
	return allReadFailureCauses(err, func(cause error) bool {
		return cause == link.ErrRepeatedLinkRejection
	})
}

func explicitUnavailableLink(cause error) bool {
	if cause == link.ErrFileNotAvailable {
		return true
	}
	e, ok := cause.(*link.Error)
	return ok && e != nil && e.IsPermanent() && e.Code == "file_not_available"
}

func (m *Manager) allowReadFailureLog(key string, now time.Time) bool {
	m.readFailureLogMu.Lock()
	defer m.readFailureLogMu.Unlock()
	if m.readFailureLogLast == nil {
		m.readFailureLogLast = make(map[string]time.Time)
	}
	if last, ok := m.readFailureLogLast[key]; ok && now.Sub(last) < readFailureLogInterval {
		return false
	}
	if len(m.readFailureLogLast) >= readFailureLogCapacity {
		for existing, last := range m.readFailureLogLast {
			if now.Sub(last) >= readFailureLogInterval {
				delete(m.readFailureLogLast, existing)
			}
		}
		if len(m.readFailureLogLast) >= readFailureLogCapacity {
			return false
		}
	}
	m.readFailureLogLast[key] = now
	return true
}

// RecordTerminalReadFailure feeds exhausted DFS reads into the persistent
// health reconciler. It deliberately excludes the raw error from logs because
// provider errors can contain signed URLs, credentials, or article IDs.
func (m *Manager) RecordTerminalReadFailure(entry *storage.Entry, filename string, offset, length int64, err error) {
	if m == nil || entry == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	class, retryable := classifyTerminalReadFailure(err)
	m.terminalReadFailures.Add(1)
	key := strings.Join([]string{entry.InfoHash, filename, class}, "\x00")
	shouldLog := m.allowReadFailureLog(key, time.Now())

	persisted := false
	queued := false
	observed := !isDeferredReadFailure(err)
	upstreamObserved := observed && class != "invalid_nzb_map"
	healthName := entry.Name
	// A local cooldown has not attempted the range upstream. Recording it as
	// a new failure would continuously invalidate in-progress health proof
	// while the player retries a file whose real failure is already durable.
	if observed && m.storage != nil && entry.Name != "" {
		fileID := ""
		if file := entry.Files[filename]; file != nil {
			fileID = file.ID
		}
		var persistErr error
		healthName, persistErr = m.storage.RecordFailedReadAt(entry.Name, entry.Protocol, "read_failure:"+class, storage.FailedReadRange{
			InfoHash: entry.InfoHash, FileName: filename, Offset: offset, Length: length,
			FileID: fileID,
		})
		persisted = persistErr == nil && healthName != ""
	}
	if persisted && m.repair != nil {
		queued = m.repair.QueueDirtyEntry(healthName)
	}
	if !shouldLog {
		return
	}

	provider := entry.ActiveProvider
	if provider == "" && entry.Protocol == "nzb" {
		provider = "usenet"
	}
	m.logger.Warn().
		Str("event", "stream.read_terminal_failure").
		Str("outcome", "failed").
		Str("entry", entry.Name).
		Str("hash", entry.InfoHash).
		Str("file", filename).
		Str("protocol", string(entry.Protocol)).
		Str("provider", provider).
		Int64("offset", offset).
		Int64("length", length).
		Str("failure_class", class).
		Str("article_failure_reason", readArticleFailureReason(err)).
		Bool("retryable", retryable).
		Bool("upstream_failure_observed", upstreamObserved).
		Bool("health_dirty_persisted", persisted).
		Bool("health_recheck_queued", queued).
		Msg("Terminal media read failure")
}
