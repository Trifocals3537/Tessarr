package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/Trifocals3537/tessarr/pkg/usenet"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type par2API interface {
	PAR2Targets(context.Context, string, string) ([]usenet.PAR2Target, string, error)
	PAR2Jobs() ([]recovery.Job, error)
	StartPAR2(context.Context, string, string, []byte) (recovery.Job, error)
	CancelPAR2(string) error
}

func (s *Server) par2Client() par2API {
	if s.par2 != nil {
		return s.par2
	}
	if s.manager != nil {
		return s.manager
	}
	return nil
}

func par2Response(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func par2Error(w http.ResponseWriter, err error) {
	status, message, code := http.StatusServiceUnavailable, "Usenet recovery is currently unavailable.", "unavailable"
	switch {
	case errors.Is(err, recovery.ErrSourceRequired):
		status, message, code = http.StatusBadRequest, recovery.ErrSourceRequired.Error(), "source_required"
	case errors.Is(err, recovery.ErrUnsupported):
		status, message, code = http.StatusBadRequest, recovery.ErrUnsupported.Error(), "unsupported"
	case errors.Is(err, recovery.ErrStale):
		status, message, code = http.StatusConflict, recovery.ErrStale.Error(), "stale"
	case errors.Is(err, recovery.ErrParity):
		status, message, code = http.StatusBadRequest, recovery.ErrParity.Error(), "parity_unavailable"
	case errors.Is(err, recovery.ErrLimit):
		status, message, code = http.StatusRequestEntityTooLarge, recovery.ErrLimit.Error(), "limit"
	case errors.Is(err, recovery.ErrBusy):
		status, message, code = http.StatusConflict, recovery.ErrBusy.Error(), "busy"
	case errors.Is(err, recovery.ErrNotFound):
		status, message, code = http.StatusNotFound, recovery.ErrNotFound.Error(), "not_found"
	case errors.Is(err, usenet.ErrPAR2NotConfigured):
		message = "Configure Usenet before requesting recovery."
	}
	par2Response(w, status, map[string]string{"error": message, "code": code})
}

func (s *Server) handlePAR2Targets(w http.ResponseWriter, r *http.Request) {
	if !s.par2Admission.TryLock() {
		par2Error(w, recovery.ErrBusy)
		return
	}
	defer s.par2Admission.Unlock()
	q, after := r.URL.Query().Get("q"), r.URL.Query().Get("after")
	if len(q) > 128 || !utf8.ValidString(q) || (after != "" && safepath.ValidateIdentifier(after) != nil) {
		par2Response(w, 400, map[string]string{"error": "Invalid search."})
		return
	}
	client := s.par2Client()
	if client == nil {
		par2Error(w, usenet.ErrPAR2NotConfigured)
		return
	}
	items, next, err := client.PAR2Targets(r.Context(), q, after)
	if err != nil {
		par2Error(w, err)
		return
	}
	if items == nil {
		items = []usenet.PAR2Target{}
	}
	par2Response(w, 200, map[string]any{"items": items, "next": next})
}

func (s *Server) handlePAR2Jobs(w http.ResponseWriter, r *http.Request) {
	client := s.par2Client()
	if client == nil {
		par2Error(w, usenet.ErrPAR2NotConfigured)
		return
	}
	jobs, err := client.PAR2Jobs()
	if err != nil {
		par2Error(w, err)
		return
	}
	if jobs == nil {
		jobs = []recovery.Job{}
	}
	par2Response(w, 200, map[string]any{"jobs": jobs, "limits": map[string]any{"read_bytes": recovery.MaxReadBytes, "patch_bytes": recovery.MaxPatchBytes, "store_bytes": recovery.MaxStoreBytes, "buffer_bytes": recovery.MaxBufferBytes, "seconds": int64(recovery.JobTimeout.Seconds()), "bytes_per_second": recovery.DownloadBytesPerSecond}})
}

type par2Start struct {
	NZBID string `json:"nzb_id"`
	File  string `json:"file"`
}

func decodePAR2Start(w http.ResponseWriter, r *http.Request) (par2Start, []byte, error) {
	const maxRequest = recovery.MaxSourceBytes + (64 << 10)
	if r.ContentLength > maxRequest {
		return par2Start{}, nil, &http.MaxBytesError{Limit: maxRequest}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return par2Start{}, nil, err
	}
	var input par2Start
	var source []byte
	switch media {
	case "application/json":
		b, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil {
			return input, nil, err
		}
		if len(b) > 4096 {
			return input, nil, &http.MaxBytesError{Limit: 4096}
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return input, nil, err
		}
		if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
			return input, nil, errors.New("unexpected trailing JSON")
		}
	case "multipart/form-data":
		reader, err := r.MultipartReader()
		if err != nil {
			return input, nil, err
		}
		seen := make(map[string]bool)
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return input, nil, err
			}
			name := part.FormName()
			if seen[name] || (name != "nzb_id" && name != "file" && name != "source") {
				_ = part.Close()
				return input, nil, errors.New("unexpected or duplicate field")
			}
			seen[name] = true
			limit := int64(1024)
			if name == "source" {
				limit = recovery.MaxSourceBytes
			}
			b, err := io.ReadAll(io.LimitReader(part, limit+1))
			_ = part.Close()
			if err != nil {
				return input, nil, err
			}
			if int64(len(b)) > limit {
				return input, nil, &http.MaxBytesError{Limit: limit}
			}
			switch name {
			case "nzb_id":
				input.NZBID = string(b)
			case "file":
				input.File = string(b)
			case "source":
				source = b
			}
		}
	default:
		return input, nil, errors.New("expected JSON or multipart form")
	}
	if safepath.ValidateIdentifier(input.NZBID) != nil || input.File == "" || len(input.File) > 255 || !utf8.ValidString(input.File) || strings.ContainsAny(input.File, "/\\\x00") {
		return input, nil, errors.New("invalid NZB or file")
	}
	return input, source, nil
}

func (s *Server) handleStartPAR2(w http.ResponseWriter, r *http.Request) {
	if !s.par2Admission.TryLock() {
		par2Error(w, recovery.ErrBusy)
		return
	}
	defer s.par2Admission.Unlock()
	input, source, err := decodePAR2Start(w, r)
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			par2Response(w, 413, map[string]string{"error": "The original NZB must be no larger than 16 MiB."})
		} else {
			par2Response(w, 400, map[string]string{"error": "Invalid recovery request."})
		}
		return
	}
	client := s.par2Client()
	if client == nil {
		par2Error(w, usenet.ErrPAR2NotConfigured)
		return
	}
	job, err := client.StartPAR2(r.Context(), input.NZBID, input.File, source)
	if err != nil {
		par2Error(w, err)
		return
	}
	par2Response(w, 202, job)
}

func (s *Server) handleCancelPAR2(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if uuid.Validate(id) != nil {
		par2Response(w, 400, map[string]string{"error": "Invalid recovery job."})
		return
	}
	client := s.par2Client()
	if client == nil {
		par2Error(w, usenet.ErrPAR2NotConfigured)
		return
	}
	if err := client.CancelPAR2(id); err != nil {
		par2Error(w, err)
		return
	}
	par2Response(w, 200, map[string]bool{"cancelled": true})
}
