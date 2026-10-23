package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id,omitempty"`
}
type Result struct {
	Type   string `json:"content_type"`
	Size   int64  `json:"size_bytes"`
	SHA256 string `json:"sha256"`
}
type View struct {
	Version     string     `json:"api_version"`
	Instance    string     `json:"instance_id"`
	ID          string     `json:"job_id"`
	Status      string     `json:"status"`
	Created     time.Time  `json:"created_at"`
	Completed   *time.Time `json:"completed_at"`
	Expires     *time.Time `json:"expires_at"`
	StatusURL   string     `json:"status_url"`
	DownloadURL *string    `json:"download_url"`
	Result      *Result    `json:"result"`
	Error       *APIError  `json:"error"`
}
type job struct {
	view                         View
	owner, key, fingerprint, dir string
	request                      Request
	params                       Params
	reserved                     int64
	accepted, active             bool
	cancel                       context.CancelFunc
	downloads                    map[string]context.CancelFunc
	tombstone                    time.Time
}

type Processor interface {
	Process(context.Context, string, Request, Params) (*Result, *APIError)
}
type Service struct {
	cfg         Config
	keys        []Key
	processor   Processor
	mu          sync.Mutex
	jobs        map[string]*job
	idempotency map[string]string
	reserved    int64
	retained    int
	instance    string
	slots       chan struct{}
	requests    chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	closing     bool
}

func New(c Config, keys []Key, p Processor) (*Service, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if len(keys) == 0 || p == nil {
		return nil, errors.New("keys and processor required")
	}
	if e := os.MkdirAll(c.WorkDir, 0700); e != nil {
		return nil, e
	}
	// WorkDir must be dedicated and empty. The entrypoint clears only its own job-* children.
	entries, e := os.ReadDir(c.WorkDir)
	if e != nil {
		return nil, e
	}
	if len(entries) > 0 {
		return nil, errors.New("work directory must be empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: c, keys: keys, processor: p, jobs: map[string]*job{}, idempotency: map[string]string{}, instance: newID(), slots: make(chan struct{}, c.Concurrent), requests: make(chan struct{}, 32), ctx: ctx, cancel: cancel}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.sweep(now)
			}
		}
	}()
	return s, nil
}
func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

func (s *Service) reserve(owner, key string, r Request, p Params) (*job, int, string) {
	b, _ := json.Marshal(struct {
		Operation  string
		Format     string
		Input      any
		Parameters Params
	}{r.Operation, r.Format, r.Input, p})
	d := sha256.Sum256(b)
	fp := hex.EncodeToString(d[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, 503, "processor_unavailable"
	}
	ik := owner + "\x00" + key
	if id, ok := s.idempotency[ik]; ok {
		j := s.jobs[id]
		if j.fingerprint != fp {
			return nil, 409, "idempotency_conflict"
		}
		if !j.accepted {
			return nil, 409, "idempotency_in_progress"
		}
		if j.view.Status == "deleted" || j.view.Status == "expired" {
			return nil, 410, "job_" + j.view.Status
		}
		return j, 202, ""
	}
	if len(s.jobs) >= s.cfg.MaxRecords || s.retained >= s.cfg.Retained || s.reserved+r.Input.Size+s.cfg.MaxOutput > s.cfg.StorageBudget {
		return nil, 503, "capacity_exhausted"
	}
	n := 0
	for _, j := range s.jobs {
		if j.owner == owner && j.reserved > 0 {
			n++
		}
	}
	if n >= s.cfg.PerConsumer {
		return nil, 429, "consumer_limit"
	}
	dir, e := os.MkdirTemp(s.cfg.WorkDir, "job-")
	if e != nil {
		return nil, 503, "storage_unavailable"
	}
	id := newID()
	j := &job{owner: owner, key: key, fingerprint: fp, dir: dir, request: r, params: p, reserved: r.Input.Size + s.cfg.MaxOutput, downloads: map[string]context.CancelFunc{}}
	j.view = View{Version: "1", Instance: s.instance, ID: id, Status: "uploading", Created: time.Now().UTC(), StatusURL: "/v1/jobs/" + id}
	s.jobs[id] = j
	s.idempotency[ik] = id
	s.reserved += j.reserved
	s.retained++
	return j, 0, ""
}
func (s *Service) abort(j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release(j)
	delete(s.jobs, j.view.ID)
	delete(s.idempotency, j.owner+"\x00"+j.key)
}
func (s *Service) release(j *job) {
	if j.reserved > 0 {
		os.RemoveAll(j.dir)
		s.reserved -= j.reserved
		s.retained--
		j.reserved = 0
	}
}
func (s *Service) accept(j *job) (View, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return View{}, false
	}
	j.accepted = true
	j.view.Status = "queued"
	ctx, cancel := context.WithCancel(s.ctx)
	j.cancel = cancel
	j.active = true
	s.workers.Add(1)
	go s.execute(ctx, j)
	return j.view, true
}
func (s *Service) execute(ctx context.Context, j *job) {
	defer s.workers.Done()
	var result *Result
	var fault *APIError
	select {
	case s.slots <- struct{}{}:
		s.mu.Lock()
		if ctx.Err() == nil {
			j.view.Status = "processing"
		}
		s.mu.Unlock()
		if ctx.Err() == nil {
			timed, cancel := context.WithTimeout(ctx, s.cfg.ProcessTimeout)
			result, fault = s.processor.Process(timed, j.dir, j.request, j.params)
			cancel()
		}
		<-s.slots
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j.active = false
	j.cancel()
	now := time.Now().UTC()
	exp := now.Add(s.cfg.TTL)
	j.view.Completed = &now
	j.view.Expires = &exp
	if ctx.Err() != nil && j.view.Status == "cancelling" {
		// A client DELETE always ends in deleted (03, Lifecycle).
		j.view.Status = "deleted"
		j.view.Expires = nil
		j.tombstone = now.Add(s.cfg.TombstoneTTL)
		s.release(j)
		return
	}
	if s.closing {
		s.release(j)
		j.view.Status = "cancelled"
		return
	}
	if fault != nil || result == nil {
		if fault == nil {
			fault = &APIError{Code: "processing_failed", Message: "Processing did not complete."}
		}
		j.view.Status = "failed"
		j.view.Error = fault
		s.release(j)
		return
	}
	j.view.Status = "completed"
	j.view.Result = result
	url := j.view.StatusURL + "/download"
	j.view.DownloadURL = &url
	os.Remove(filepath.Join(j.dir, "input"))
}
func (s *Service) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, j := range s.jobs {
		if !j.accepted || j.active {
			continue
		}
		if !j.tombstone.IsZero() {
			if !now.Before(j.tombstone) && len(j.downloads) == 0 {
				delete(s.jobs, id)
				delete(s.idempotency, j.owner+"\x00"+j.key)
			}
			continue
		}
		if j.view.Expires != nil && !now.Before(*j.view.Expires) {
			j.view.Status = "expired"
			j.view.DownloadURL = nil
			j.view.Result = nil
			j.tombstone = now.Add(s.cfg.TombstoneTTL)
			if len(j.downloads) == 0 {
				s.release(j)
			}
		}
	}
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closing = true
	s.cancel()
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel()
		}
		for _, cancel := range j.downloads {
			cancel()
		}
	}
	s.mu.Unlock()
	s.workers.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if len(j.downloads) == 0 {
			s.release(j)
		}
	}
}
