package sabnzbd

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *SABnzbd) writeUnsupportedControl(w http.ResponseWriter) {
	s.writeError(w, "SAB pause/resume and priority changes are not supported by Tessarr", http.StatusNotImplemented)
}

// validateAddPriority requires the bounded form parsing performed by the route
// middleware. Check all values: FormValue alone would let an active priority
// shadow a requested hold in a repeated field or the other parameter source.
func (s *SABnzbd) validateAddPriority(w http.ResponseWriter, r *http.Request) bool {
	for _, raw := range r.Form["priority"] {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		priority, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			s.writeError(w, "Invalid SAB add priority", http.StatusBadRequest)
			return false
		}
		switch priority {
		case -100, -1, 0, 1, 2:
			// Existing active priorities retain normal scheduling. Tessarr does
			// not implement SAB's category, low/high, or force scheduling.
		case -2, -4:
			// SAB documents -2 as Paused; -4 is its queue-priority Stop value.
			// Neither can safely be admitted as ordinary active work.
			s.writeError(w, "Paused or stopped NZB admission is not supported by Tessarr; no NZBs were added", http.StatusNotImplemented)
			return false
		default:
			s.writeError(w, "Invalid SAB add priority", http.StatusBadRequest)
			return false
		}
	}
	return true
}
