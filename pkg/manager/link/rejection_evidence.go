package link

import (
	"time"

	"github.com/Trifocals3537/tessarr/pkg/storage"
)

const (
	replacementRejectionThreshold = 3
	replacementRejectionMaxAge    = 30 * time.Minute
)

type replacementRejectionState struct {
	code         string
	count        int
	lastCounted  time.Time
	lastObserved time.Time
}

func replacementRejectionKey(entry *storage.Entry, filename string) string {
	key := linkLifecycleKey(entry, filename)
	if file := entry.Files[filename]; file != nil {
		key += "\x00" + file.ID
	}
	if placement := entry.GetActiveProvider(); placement != nil {
		if file := placement.Files[filename]; file != nil {
			key += "\x00" + file.Id + "\x00" + file.Path
		}
	}
	return key
}

func definitiveReplacementRejection(code string) bool {
	switch code {
	case "invalid_download_code", "link_not_found", "404", "410":
		return true
	default:
		return false
	}
}

// Count independently observed hard rejections of new links. Share evidence
// across playback and read-only health policies but never double count a
// concurrent burst. Link expiry, auth, throttling, and transport errors are
// not evidence of persistently unavailable content.
func (s *Service) noteReplacementRejection(key, code string) bool {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if !definitiveReplacementRejection(code) {
		delete(s.replacementRejections, key)
		return false
	}
	now := s.now()
	if s.replacementRejections == nil {
		s.replacementRejections = make(map[string]replacementRejectionState)
	}
	state, exists := s.replacementRejections[key]
	if !exists && len(s.replacementRejections) >= maxRefreshBackoffs {
		var oldestKey string
		var oldest time.Time
		for candidate, other := range s.replacementRejections {
			if oldestKey == "" || other.lastObserved.Before(oldest) {
				oldestKey, oldest = candidate, other.lastObserved
			}
		}
		delete(s.replacementRejections, oldestKey)
	}
	if state.code != code || now.Before(state.lastObserved) ||
		now.Sub(state.lastObserved) > replacementRejectionMaxAge {
		state = replacementRejectionState{code: code}
	}
	if state.count == 0 || now.Sub(state.lastCounted) >= refreshBackoffBase {
		state.count = min(state.count+1, replacementRejectionThreshold)
		state.lastCounted = now
	}
	state.lastObserved = now
	s.replacementRejections[key] = state
	return state.count >= replacementRejectionThreshold
}

func (s *Service) clearReplacementRejections(key string) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	delete(s.replacementRejections, key)
}
