package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const otherKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fakeProcessor struct {
	gate <-chan struct{}
	fail bool
}

func (f fakeProcessor) Process(ctx context.Context, dir string, r Request, p Params) (*Result, *APIError) {
	if f.gate != nil {
		select {
		case <-ctx.Done():
			return nil, fault("processing_timeout")
		case <-f.gate:
		}
	}
	if f.fail {
		return nil, fault("invalid_media")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "input"))
	_ = os.WriteFile(filepath.Join(dir, "output"), data, 0600)
	h := sha256.Sum256(data)
	return &Result{"application/json", int64(len(data)), hex.EncodeToString(h[:])}, nil
}
func setup(t *testing.T, p Processor) *Service {
	t.Helper()
	c := DefaultConfig()
	c.WorkDir = t.TempDir()
	c.MaxUpload = 1024
	c.MaxOutput = 1024
	c.StorageBudget = 4096
	s, e := New(c, []Key{{"one", sha256.Sum256([]byte(testKey))}, {"two", sha256.Sum256([]byte(otherKey))}}, p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}
func request(data []byte) Request {
	h := sha256.Sum256(data)
	r := Request{Operation: "metadata", Format: "json", Parameters: map[string]json.RawMessage{}}
	r.Input.Size = int64(len(data))
	r.Input.SHA256 = hex.EncodeToString(h[:])
	return r
}
func uploadRequest(t *testing.T, r Request, data []byte, key string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="request"`)
	h.Set("Content-Type", "application/json")
	part, _ := m.CreatePart(h)
	_ = json.NewEncoder(part).Encode(r)
	part, _ = m.CreateFormFile("file", "../../secret.txt")
	_, _ = part.Write(data)
	m.Close()
	req := httptest.NewRequest("POST", "/v1/jobs", &b)
	req.Header.Set("Content-Type", m.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Idempotency-Key", key)
	return req
}
func call(s *Service, method, path, key string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+key)
	s.ServeHTTP(w, r)
	return w
}
func submit(t *testing.T, s *Service, data []byte, key string) View {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, request(data), data, key))
	if w.Code != 202 {
		t.Fatalf("submit %d: %s", w.Code, w.Body.String())
	}
	var v View
	json.Unmarshal(w.Body.Bytes(), &v)
	return v
}
func waitState(t *testing.T, s *Service, id, state string) View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := call(s, "GET", "/v1/jobs/"+id, testKey)
		var v View
		json.Unmarshal(w.Body.Bytes(), &v)
		if v.Status == state {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not reach %s: %s", state, call(s, "GET", "/v1/jobs/"+id, testKey).Body.String())
	return View{}
}

func TestLifecycleOwnershipDownloadAndDelete(t *testing.T) {
	s := setup(t, fakeProcessor{})
	v := submit(t, s, []byte("hello world"), "idempotency-000001")
	v = waitState(t, s, v.ID, "completed")
	if w := call(s, "GET", v.StatusURL, otherKey); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for i := 0; i < 2; i++ {
		w := call(s, "GET", v.StatusURL+"/download", testKey)
		if w.Code != 200 || w.Body.String() != "hello world" {
			t.Fatal(w)
		}
	}
	r := httptest.NewRequest("GET", v.StatusURL+"/download", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "ell" {
		t.Fatal(w)
	}
	r.Header.Set("Range", "bytes=0-1,4-5")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 416 {
		t.Fatal(w.Code)
	}
	if w = call(s, "DELETE", v.StatusURL, testKey); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w = call(s, "GET", v.StatusURL, testKey); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if w = call(s, "DELETE", v.StatusURL, testKey); w.Code != 204 {
		t.Fatal(w.Code)
	}
	s.mu.Lock()
	if s.reserved != 0 || s.retained != 0 {
		t.Fatal("reservation leak")
	}
	s.mu.Unlock()
}
func TestIdempotencyAndConflict(t *testing.T) {
	s := setup(t, fakeProcessor{})
	data := []byte("abc")
	a := submit(t, s, data, "idempotency-000001")
	b := submit(t, s, data, "idempotency-000001")
	if a.ID != b.ID {
		t.Fatal("duplicate job")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, request([]byte("def")), []byte("def"), "idempotency-000001"))
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestIdempotencyCanonicalDefaults(t *testing.T) {
	s := setup(t, fakeProcessor{})
	data := []byte("abc")
	r := request(data)
	r.Operation = "transcode"
	r.Format = "mp4"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, r, data, "idempotency-000001"))
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var first View
	json.Unmarshal(w.Body.Bytes(), &first)
	r.Parameters["quality"] = json.RawMessage("23")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, r, data, "idempotency-000001"))
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var second View
	json.Unmarshal(w.Body.Bytes(), &second)
	if first.ID != second.ID {
		t.Fatal("default changed fingerprint")
	}
}
func TestBadUploadsReleaseReservations(t *testing.T) {
	for _, kind := range []string{"checksum", "short", "oversize", "extra", "truncated", "order", "unknown", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			s := setup(t, fakeProcessor{})
			data := []byte("abc")
			r := request(data)
			switch kind {
			case "checksum":
				r.Input.SHA256 = strings.Repeat("0", 64)
			case "short":
				r.Input.Size++
			case "oversize":
				r.Input.Size--
			case "unknown":
				r.Parameters["command"] = json.RawMessage(`"rm -rf /"`)
			}
			req := uploadRequest(t, r, data, "idempotency-000001")
			if kind == "extra" || kind == "truncated" || kind == "order" || kind == "duplicate" {
				body, _ := io.ReadAll(req.Body)
				switch kind {
				case "extra":
					body = bytes.Replace(body, []byte(`name="file"`), []byte(`name="another"`), 1)
				case "truncated":
					body = body[:len(body)-12]
				case "order":
					body = bytes.Replace(body, []byte(`name="request"`), []byte(`name="file"`), 1)
				case "duplicate":
					body = bytes.Replace(body, []byte(`"operation":"metadata"`), []byte(`"operation":"metadata","operation":"metadata"`), 1)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code < 400 {
				t.Fatal(w.Code)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.reserved != 0 || len(s.jobs) != 0 {
				t.Fatal("upload leaked state")
			}
		})
	}
}
func TestCancellationAndCapacity(t *testing.T) {
	gate := make(chan struct{})
	s := setup(t, fakeProcessor{gate: gate})
	a := submit(t, s, []byte("abc"), "idempotency-000001")
	waitState(t, s, a.ID, "processing")
	b := submit(t, s, []byte("abc"), "idempotency-000002")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, request([]byte("abc")), []byte("abc"), "idempotency-000003"))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w = call(s, "DELETE", b.StatusURL, testKey); w.Code != 202 {
		t.Fatal(w.Code)
	}
	waitState(t, s, b.ID, "cancelled")
	if w = call(s, "DELETE", a.StatusURL, testKey); w.Code != 202 {
		t.Fatal(w.Code)
	}
	waitState(t, s, a.ID, "cancelled")
}
func TestStatusRepresentationsAndETag(t *testing.T) {
	s := setup(t, fakeProcessor{fail: true})
	v := submit(t, s, []byte("abc"), "idempotency-000001")
	waitState(t, s, v.ID, "failed")
	r := httptest.NewRequest("GET", v.StatusURL, nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "invalid_media") {
		t.Fatal(w)
	}
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	r.Header.Set("Accept", "image/png")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 406 {
		t.Fatal(w.Code)
	}
}
func TestExpiryAndTombstone(t *testing.T) {
	s := setup(t, fakeProcessor{})
	v := submit(t, s, []byte("abc"), "idempotency-000001")
	waitState(t, s, v.ID, "completed")
	now := time.Now().Add(s.cfg.TTL + time.Second)
	s.sweep(now)
	if w := call(s, "GET", v.StatusURL, testKey); w.Code != 410 {
		t.Fatal(w.Code)
	}
	s.sweep(now.Add(s.cfg.TombstoneTTL + time.Second))
	if w := call(s, "GET", v.StatusURL, testKey); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestAuthenticationAndSecrets(t *testing.T) {
	s := setup(t, fakeProcessor{})
	for _, path := range []string{"/v1/capabilities", "/v1/jobs/01234567890123456789012345678901"} {
		if w := call(s, "GET", path, ""); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	path := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(path, []byte(`{"consumers":[{"id":"one","keys":["`+testKey+`","`+otherKey+`"]}]}`), 0600)
	keys, e := LoadKeys(path)
	if e != nil || len(keys) != 2 || keys[0].Consumer != keys[1].Consumer {
		t.Fatal(e)
	}
}
func TestConcurrentReplay(t *testing.T) {
	s := setup(t, fakeProcessor{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := uploadRequest(t, request([]byte("abc")), []byte("abc"), "idempotency-000001")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 202 && w.Code != 409 {
				t.Errorf("response %d", w.Code)
			}
		}()
	}
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) != 1 {
		t.Fatal(len(s.jobs))
	}
}
func TestValidation(t *testing.T) {
	c := DefaultConfig()
	r := request([]byte("abc"))
	for _, input := range []string{
		`{"operation":"metadata","input":{"size_bytes":3,"sha256":"` + r.Input.SHA256 + `"},"parameters":{"filter":"movie=/etc/passwd"},"output_format":"json"}`,
		`{"operation":"resize","input":{"size_bytes":3,"sha256":"` + r.Input.SHA256 + `"},"parameters":{"width":0,"height":32},"output_format":"png"}`,
		`{"operation":"thumbnail","input":{"size_bytes":3,"sha256":"` + r.Input.SHA256 + `"},"parameters":{"width":32,"height":32,"timestamp_seconds":null},"output_format":"png"}`,
	} {
		var req Request
		if e := decodeStrict(strings.NewReader(input), &req, 16384); e == nil {
			if _, e = req.Validate(c); e == nil {
				t.Fatal("accepted invalid input")
			}
		}
	}
}

func TestRangeValidation(t *testing.T) {
	for _, value := range []string{"bytes=0-1", "bytes=2-", "bytes=-2", "bytes=0-99", ""} {
		if !validRange(value, 10) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"items=0-1", "bytes=2-1", "bytes=-0", "bytes=10-", "bytes=0-1,2-3", "bytes=+1-2", "bytes=99999999999999999999999999-"} {
		if validRange(value, 10) {
			t.Fatal(value)
		}
	}
}

func TestShutdownRejectsUploadAcceptance(t *testing.T) {
	s := setup(t, fakeProcessor{})
	r := request([]byte("abc"))
	params, _ := r.Validate(s.cfg)
	j, code, _ := s.reserve("one", "idempotency-000001", r, params)
	if code != 0 {
		t.Fatal(code)
	}
	s.Close()
	if _, ok := s.accept(j); ok {
		t.Fatal("accepted during shutdown")
	}
	s.abort(j)
}

func TestPinnedExpiry(t *testing.T) {
	s := setup(t, fakeProcessor{})
	v := submit(t, s, []byte("abc"), "idempotency-000001")
	waitState(t, s, v.ID, "completed")
	s.mu.Lock()
	j := s.jobs[v.ID]
	j.downloads["test"] = func() {}
	dir := j.dir
	s.mu.Unlock()
	s.sweep(time.Now().Add(s.cfg.TTL + time.Second))
	if _, e := os.Stat(filepath.Join(dir, "output")); e != nil {
		t.Fatal("expired a pinned file")
	}
	if w := call(s, "GET", v.StatusURL+"/download", testKey); w.Code != 410 {
		t.Fatal(w.Code)
	}
	s.mu.Lock()
	delete(j.downloads, "test")
	s.release(j)
	s.mu.Unlock()
}

func TestConfigRejectsUnsafeBudgets(t *testing.T) {
	for key, value := range map[string]string{"MAX_CONCURRENT_JOBS": "0", "MAX_UPLOAD_BYTES": "-1", "MAX_PIXELS": "999999999", "PROCESS_TIMEOUT_SECONDS": "0", "WORK_DIR": "/"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, e := FromEnv(); e == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	if _, e := FromEnv(); e != nil {
		t.Fatal(e)
	}
}
