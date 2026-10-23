package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	if status == 429 || status == 503 || code == "idempotency_in_progress" {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, status, map[string]any{"error": APIError{Code: code, Message: strings.ReplaceAll(code, "_", " ") + ".", Retryable: status == 429 || status == 503 || code == "idempotency_in_progress", RequestID: w.Header().Get("X-Request-ID")}})
}

// failWith writes a synchronous error carrying a field and message (03, Error model).
func failWith(w http.ResponseWriter, status int, e *APIError) {
	retry := status == 429 || status == 503 || e.Code == "idempotency_in_progress"
	if retry {
		w.Header().Set("Retry-After", "2")
	}
	out := *e
	out.Retryable = retry
	out.RequestID = w.Header().Get("X-Request-ID")
	writeJSON(w, status, map[string]any{"error": out})
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-ID", newID())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.RawQuery != "" {
		failWith(w, 400, &APIError{Code: "malformed_request", Message: "Query parameters are not supported."})
		return
	}
	if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing && r.URL.Path == "/health/ready" {
			fail(w, 503, "processor_unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	auth := r.Header.Get("Authorization")
	owner := ""
	if strings.HasPrefix(auth, "Bearer ") && len(auth) <= 263 {
		d := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
		for _, key := range s.keys {
			if subtle.ConstantTimeCompare(d[:], key.Digest[:]) == 1 {
				owner = key.Consumer
			}
		}
	}
	if owner == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "invalid_authentication")
		return
	}
	select {
	case s.requests <- struct{}{}:
		defer func() { <-s.requests }()
	default:
		fail(w, 503, "capacity_exhausted")
		return
	}
	switch r.URL.Path {
	case "/v1/capabilities":
		if r.Method != "GET" {
			fail(w, 405, "method_not_allowed")
			return
		}
		writeJSON(w, 200, map[string]any{"api_version": "1", "instance_id": s.instance,
			"operations": map[string][]string{"transcode": {"mp4", "webm"}, "resize": {"jpg", "png"}, "thumbnail": {"jpg", "png"}, "extract_audio": {"m4a", "mp3", "opus"}, "trim": {"mp4", "webm"}, "remux": {"mp4", "webm"}, "metadata": {"json"}},
			"parameters": OperationParameters(),
			"codecs":     map[string]string{"mp4": "h264/aac", "webm": "vp9/opus", "jpg": "mjpeg", "png": "png", "m4a": "aac", "mp3": "mp3", "opus": "opus"},
			"limits":     map[string]any{"max_upload_bytes": s.cfg.MaxUpload, "max_output_bytes": s.cfg.MaxOutput, "max_duration_seconds": s.cfg.MaxDuration, "max_pixels": s.cfg.MaxPixels, "max_concurrent_jobs": s.cfg.Concurrent, "max_retained_jobs": s.cfg.Retained, "max_jobs_per_consumer": s.cfg.PerConsumer, "job_ttl_seconds": int(s.cfg.TTL.Seconds()), "process_timeout_seconds": int(s.cfg.ProcessTimeout.Seconds()), "upload_timeout_seconds": int(s.cfg.UploadTimeout.Seconds())}})
	case "/v1/jobs":
		if r.Method != "POST" {
			fail(w, 405, "method_not_allowed")
			return
		}
		s.upload(w, r, owner)
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")
		if !strings.HasPrefix(r.URL.Path, "/v1/jobs/") || len(parts) > 2 || len(parts[0]) != 32 {
			fail(w, 404, "job_not_found")
			return
		}
		if len(parts) == 2 {
			if parts[1] != "download" {
				fail(w, 404, "job_not_found")
				return
			}
			if r.Method != "GET" {
				fail(w, 405, "method_not_allowed")
				return
			}
			s.download(w, r, owner, parts[0])
			return
		}
		switch r.Method {
		case "GET":
			s.status(w, r, owner, parts[0])
		case "DELETE":
			s.remove(w, r, owner, parts[0])
		default:
			fail(w, 405, "method_not_allowed")
		}
	}
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request, owner string) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.cfg.UploadTimeout))
	defer rc.SetReadDeadline(time.Time{})
	key := r.Header.Get("Idempotency-Key")
	badKey := &APIError{Code: "malformed_request", Field: "Idempotency-Key", Message: "Idempotency-Key must be 16 to 128 printable ASCII characters."}
	if len(key) < 16 || len(key) > 128 {
		failWith(w, 400, badKey)
		return
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			failWith(w, 400, badKey)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload+65536)
	defer r.Body.Close()
	mr, e := r.MultipartReader()
	if e != nil {
		fail(w, 415, "unsupported_content_type")
		return
	}
	part, e := mr.NextPart()
	if e != nil || part.FormName() != "request" || part.FileName() != "" {
		failWith(w, 400, &APIError{Code: "missing_part", Field: "request", Message: "The first part must be the request JSON."})
		return
	}
	mt, _, e := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if e != nil || mt != "application/json" {
		fail(w, 415, "unsupported_content_type")
		return
	}
	var req Request
	if decodeStrict(part, &req, 16384) != nil {
		fail(w, 400, "malformed_request")
		return
	}
	params, invalidErr := req.Validate(s.cfg)
	if invalidErr != nil {
		if req.Input.Size > s.cfg.MaxUpload {
			failWith(w, 413, &APIError{Code: "upload_too_large", Field: "input.size_bytes", Message: "The declared size exceeds the upload limit."})
		} else {
			failWith(w, 422, invalidErr)
		}
		return
	}
	req.Input.SHA256 = strings.ToLower(req.Input.SHA256)
	j, code, msg := s.reserve(owner, key, req, params)
	if code != 0 {
		if code == 202 {
			// Replay of an accepted request: hash the file part without
			// storing it and confirm it matches the declaration (03, Idempotency).
			if apiErr := verifyReplay(mr, req.Input.Size, req.Input.SHA256); apiErr != nil {
				failWith(w, 400, apiErr)
				return
			}
			s.mu.Lock()
			v := j.view
			s.mu.Unlock()
			w.Header().Set("Location", v.StatusURL)
			writeJSON(w, 202, v)
		} else {
			fail(w, code, msg)
		}
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			s.abort(j)
		}
	}()
	part, e = mr.NextPart()
	if e != nil || part.FormName() != "file" || len(part.FileName()) > 255 {
		failWith(w, 400, &APIError{Code: "missing_part", Field: "file", Message: "The second part must be the file."})
		return
	}
	if value := part.Header.Get("Content-Type"); value != "" {
		declared, _, err := mime.ParseMediaType(value)
		if err != nil {
			fail(w, 415, "unsupported_content_type")
			return
		}
		j.request.declaredMIME = declared
	}
	f, e := os.OpenFile(filepath.Join(j.dir, "input"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		fail(w, 503, "storage_unavailable")
		return
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(part, req.Input.Size+1))
	ce := f.Close()
	if n > req.Input.Size {
		fail(w, 413, "upload_too_large")
		return
	}
	if e != nil || ce != nil || n != req.Input.Size {
		failWith(w, 400, &APIError{Code: "malformed_request", Field: "input.size_bytes", Message: "The upload ended before the declared size."})
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != req.Input.SHA256 {
		fail(w, 400, "checksum_mismatch")
		return
	}
	if _, e = mr.NextPart(); e != io.EOF {
		failWith(w, 400, &APIError{Code: "malformed_request", Message: "Exactly two parts are allowed."})
		return
	}
	if r.Context().Err() != nil {
		return
	}
	v, ok := s.accept(j)
	if !ok {
		fail(w, 503, "processor_unavailable")
		return
	}
	accepted = true
	w.Header().Set("Location", v.StatusURL)
	w.Header().Set("Retry-After", "2")
	writeJSON(w, 202, v)
}

func (s *Service) lookup(owner, id string) (*job, int, string) {
	j, ok := s.jobs[id]
	if !ok || j.owner != owner || !j.accepted {
		return nil, 404, "job_not_found"
	}
	if j.view.Status == "expired" || j.view.Status == "deleted" {
		return j, 410, "job_" + j.view.Status
	}
	return j, 0, ""
}
func representation(header string) string {
	if header == "" {
		return "json"
	}
	best := ""
	bestQ := 0.0
	for _, item := range strings.Split(header, ",") {
		m, p, e := mime.ParseMediaType(strings.TrimSpace(item))
		if e != nil {
			continue
		}
		q := 1.0
		if v, ok := p["q"]; ok {
			q, e = strconv.ParseFloat(v, 64)
			if e != nil || q < 0 || q > 1 {
				continue
			}
		}
		if q <= bestQ {
			continue
		}
		switch m {
		case "application/json", "application/*", "*/*":
			best = "json"
			bestQ = q
		case "text/html", "text/*":
			best = "html"
			bestQ = q
		}
	}
	return best
}

var statusTemplate = template.Must(template.New("status").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Media processing</title></head><body><main><h1>Media processing</h1><p>Status: {{.Status}}</p><p>Job: {{.ID}}</p><p>Created: {{.Created}}</p>{{if .Expires}}<p>Expires: {{.Expires}}</p>{{end}}{{if .Error}}<p>Error: {{.Error.Code}} — {{.Error.Message}}</p>{{end}}{{if .DownloadURL}}<p><a href="{{.DownloadURL}}">Download result</a> (authentication required)</p>{{end}}</main></body></html>`))

func (s *Service) status(w http.ResponseWriter, r *http.Request, owner, id string) {
	s.sweep(time.Now())
	s.mu.Lock()
	j, code, msg := s.lookup(owner, id)
	if code != 0 {
		s.mu.Unlock()
		fail(w, code, msg)
		return
	}
	v := j.view
	s.mu.Unlock()
	rep := representation(r.Header.Get("Accept"))
	if rep == "" {
		fail(w, 406, "unsupported_representation")
		return
	}
	b, _ := json.Marshal(v)
	d := sha256.Sum256(append(b, rep...))
	etag := fmt.Sprintf("\"%x\"", d)
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept")
	if v.Status == "queued" || v.Status == "processing" || v.Status == "cancelling" {
		w.Header().Set("Retry-After", "2")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	if rep == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = statusTemplate.Execute(w, v)
	} else {
		writeJSON(w, 200, v)
	}
}
func (s *Service) remove(w http.ResponseWriter, r *http.Request, owner, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, code, msg := s.lookup(owner, id)
	if code != 0 {
		if j != nil && (j.view.Status == "deleted" || j.view.Status == "expired") {
			w.WriteHeader(204)
		} else {
			fail(w, code, msg)
		}
		return
	}
	for _, cancel := range j.downloads {
		cancel()
	}
	if j.active {
		j.view.Status = "cancelling"
		j.cancel()
		writeJSON(w, 202, j.view)
		return
	}
	pinned := len(j.downloads) > 0
	j.view.Status = "deleted"
	j.view.DownloadURL = nil
	j.view.Result = nil
	j.tombstone = time.Now().Add(s.cfg.TombstoneTTL)
	if pinned {
		// Files are removed once the cancelled download releases its pin.
		w.WriteHeader(202)
		return
	}
	s.release(j)
	w.WriteHeader(204)
}

func (s *Service) download(w http.ResponseWriter, r *http.Request, owner, id string) {
	s.sweep(time.Now())
	s.mu.Lock()
	j, code, msg := s.lookup(owner, id)
	if code != 0 {
		s.mu.Unlock()
		fail(w, code, msg)
		return
	}
	if j.view.Status != "completed" {
		state := j.view.Status
		s.mu.Unlock()
		if state == "queued" || state == "processing" {
			fail(w, 409, "result_not_ready")
		} else {
			fail(w, 409, "result_unavailable")
		}
		return
	}
	result := *j.view.Result
	if !validRange(r.Header.Get("Range"), result.Size) {
		s.mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", result.Size))
		fail(w, 416, "invalid_range")
		return
	}
	f, e := os.Open(filepath.Join(j.dir, "output"))
	if e != nil {
		s.mu.Unlock()
		fail(w, 500, "internal_error")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	pin := newID()
	j.downloads[pin] = cancel
	s.mu.Unlock()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(120 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()) })
	defer func() {
		stop()
		cancel()
		f.Close()
		s.mu.Lock()
		delete(j.downloads, pin)
		if len(j.downloads) == 0 && (j.view.Status == "expired" || j.view.Status == "deleted" || s.closing) {
			s.release(j)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", result.Type)
	w.Header().Set("ETag", "\""+result.SHA256+"\"")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "result." + j.request.Format}))
	http.ServeContent(w, r, "result."+j.request.Format, time.Time{}, &cancelReader{ctx: ctx, f: f})
}
func validRange(value string, size int64) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return false
	}
	start, end, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return false
	}
	digits := func(s string) bool {
		for _, v := range s {
			if v < '0' || v > '9' {
				return false
			}
		}
		return true
	}
	if !digits(start) || !digits(end) {
		return false
	}
	if start == "" {
		n, e := strconv.ParseInt(end, 10, 64)
		return e == nil && n > 0
	}
	n, e := strconv.ParseInt(start, 10, 64)
	if e != nil || n >= size {
		return false
	}
	if end == "" {
		return true
	}
	last, e := strconv.ParseInt(end, 10, 64)
	return e == nil && last >= n
}

type cancelReader struct {
	ctx context.Context
	f   *os.File
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.f.Read(p)
}
func (c *cancelReader) Seek(o int64, w int) (int64, error) { return c.f.Seek(o, w) }

// verifyReplay reads a replayed file part into a hash only, never to disk.
func verifyReplay(mr *multipart.Reader, size int64, sha string) *APIError {
	part, e := mr.NextPart()
	if e != nil || part.FormName() != "file" {
		return &APIError{Code: "missing_part", Field: "file", Message: "The second part must be the file."}
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(part, size+1))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return &APIError{Code: "checksum_mismatch", Field: "input.sha256", Message: "The replayed file does not match the declared checksum."}
	}
	if _, e = mr.NextPart(); e != io.EOF {
		return &APIError{Code: "malformed_request", Message: "Exactly two parts are allowed."}
	}
	return nil
}
