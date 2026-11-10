// Package serve is the scout's local web console: one page on 127.0.0.1 to
// start and stop a run, follow the log and open the current and earlier
// digests. It is the only way the scout is started: nothing runs on a schedule.
//
// It is a thin layer over run.Run, behind run.lock. Nothing is reachable from
// outside the machine, and every request is checked so that a web page open in
// the same browser cannot drive it (see guard).
package serve

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thelab3507/topic-scout/internal/export"
	"github.com/thelab3507/topic-scout/internal/report"
	"github.com/thelab3507/topic-scout/internal/store"
)

const (
	// Job kinds.
	KindRun      = "run"
	KindPatterns = "patterns"

	// RequestHeader must be present on every request that changes something.
	// A browser will not add a custom header to a cross-site request without a
	// preflight, which this server never approves.
	RequestHeader = "X-Scout-Request"

	// DefaultMaxLimit caps the "score at most N" box on the page.
	DefaultMaxLimit = 100

	logTailLines = 80
	logTailBytes = 64 << 10
)

//go:embed assets/console.html
var pageTmpl string

//go:embed assets/console.css
var pageCSS string

// JobRequest is what the page can ask for.
type JobRequest struct {
	Limit  int  `json:"limit"`   // 0 means no limit
	DryRun bool `json:"dry_run"` // fetch and dedupe only (runs only)
}

// Runner performs one job to completion and returns the scout's exit code
// (0 ok, 1 error, 2 inference server unavailable, 3 stopped). It must return
// promptly once ctx is cancelled: that is how the Stop button works. The console
// never decides how a run works; main supplies run.Run and run.Patterns.
type Runner func(ctx context.Context, kind string, req JobRequest) int

// Server holds the console's state. Fill the exported fields, then use Handler.
type Server struct {
	Store     *store.Store
	ExportDir string // "" means exports are off
	Runner    Runner
	Now       func() time.Time
	MaxLimit  int // 0 means DefaultMaxLimit

	mu      sync.Mutex
	running *runningJob
	last    *JobResult
	wg      sync.WaitGroup
	baseCtx context.Context
}

type runningJob struct {
	kind     string
	started  time.Time
	req      JobRequest
	cancel   context.CancelFunc
	stopping bool
}

// JobResult is how the last job started from the console ended.
type JobResult struct {
	Kind       string    `json:"kind"`
	ExitCode   int       `json:"exit_code"`
	Meaning    string    `json:"meaning"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DryRun     bool      `json:"dry_run,omitempty"`
	Stopped    bool      `json:"stopped,omitempty"` // ended because Stop was pressed
}

// Running describes a job in progress.
type Running struct {
	Kind      string     `json:"kind"`
	Source    string     `json:"source"` // "console", or "other" for a run this console did not start (and cannot stop)
	StartedAt *time.Time `json:"started_at,omitempty"`
	DryRun    bool       `json:"dry_run,omitempty"`
	Stopping  bool       `json:"stopping,omitempty"` // Stop was pressed; the job is winding down
}

// Status is the JSON the page polls.
type Status struct {
	Now         time.Time       `json:"now"`
	Running     *Running        `json:"running"`
	LastJob     *JobResult      `json:"last_job"`
	LastRun     *store.LastRun  `json:"last_run"`
	ExportDir   string          `json:"export_dir"`
	ExportError string          `json:"export_error,omitempty"`
	Reports     []export.Report `json:"reports"`
	HasDigest   bool            `json:"has_digest"`
	HasLatest   bool            `json:"has_latest"`
	Log         []string        `json:"log"`
}

// Start gives the server a context for the jobs it starts: cancelling it
// cancels a running job, and Wait returns once that job has cleaned up.
func (s *Server) Start(ctx context.Context) { s.baseCtx = ctx }

// Wait blocks until any running job has finished.
func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) maxLimit() int {
	if s.MaxLimit > 0 {
		return s.MaxLimit
	}
	return DefaultMaxLimit
}

// Handler returns the console's routes, wrapped in the request guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) { s.startJob(w, r, KindRun) })
	mux.HandleFunc("POST /api/patterns", func(w http.ResponseWriter, r *http.Request) { s.startJob(w, r, KindPatterns) })
	mux.HandleFunc("POST /api/stop", s.stopJob)
	mux.HandleFunc("GET /digest", s.digest)
	mux.HandleFunc("GET /reports/{name}", s.report)
	return guard(mux)
}

// guard rejects what a local console must not answer:
//   - a Host other than localhost, 127.0.0.1 or [::1] (DNS rebinding),
//   - a changing request without the custom header (cross-site form posts),
//   - a changing request whose Origin is not this server (cross-site fetches).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden: this console answers only on localhost", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(RequestHeader) != "1" {
				http.Error(w, "forbidden: missing "+RequestHeader+" header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
					return
				}
			}
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a Host header names this machine's loopback.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ValidAddr checks that a listen address is loopback only. The console can
// start runs and read digests; it must never be reachable from the network.
func ValidAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q must look like 127.0.0.1:8787: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("address %q needs a port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("address %q is not a loopback address; the console only listens on localhost", addr)
}

// --- pages ------------------------------------------------------------------

var pageT = template.Must(template.New("console").Parse(pageTmpl))

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is one inline script and one inline style and talks only to itself.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; font-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'")
	pageT.Execute(w, struct{ CSS template.CSS }{template.CSS(report.BaseCSS() + pageCSS)})
}

// serveHTML sends a stored digest. Digests carry no script, so the policy
// forbids all of it, which also covers anything a feed title could smuggle in.
func serveHTML(w http.ResponseWriter, r *http.Request, path string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not read the report", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:; base-uri 'none'; form-action 'none'")
	w.Write(b)
}

func (s *Server) digest(w http.ResponseWriter, r *http.Request) {
	serveHTML(w, r, s.Store.DigestPath())
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	if s.ExportDir == "" {
		http.NotFound(w, r)
		return
	}
	path, ok := export.Path(s.ExportDir, r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveHTML(w, r, path)
}

// --- status -----------------------------------------------------------------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *Server) snapshot() Status {
	st := Status{Now: s.now(), ExportDir: s.ExportDir, Reports: []export.Report{}, Log: []string{}}

	s.mu.Lock()
	if s.running != nil {
		started := s.running.started
		st.Running = &Running{Kind: s.running.kind, Source: "console", StartedAt: &started, DryRun: s.running.req.DryRun, Stopping: s.running.stopping}
	}
	if s.last != nil {
		last := *s.last
		st.LastJob = &last
	}
	s.mu.Unlock()

	if st.Running == nil && s.Store.Locked() {
		st.Running = &Running{Kind: KindRun, Source: "other"}
	}
	if lr, err := s.Store.ReadLastRun(); err == nil {
		st.LastRun = lr
	}
	if _, err := os.Stat(s.Store.DigestPath()); err == nil {
		st.HasDigest = true
	}
	if s.ExportDir != "" {
		if err := export.Check(s.ExportDir); err != nil {
			st.ExportError = err.Error()
		} else if reports, err := export.List(s.ExportDir); err != nil {
			st.ExportError = err.Error()
		} else if reports != nil {
			st.Reports = reports
		}
		if st.ExportError == "" {
			_, err := os.Stat(filepath.Join(s.ExportDir, export.LatestName))
			st.HasLatest = err == nil
		}
	}
	st.Log = tailLog(s.Store.Dir, logTailLines)
	return st
}

// tailLog returns the last n lines of scout.log. Reading only the end keeps it
// cheap however large the log has grown.
func tailLog(dir string, n int) []string {
	f, err := os.Open(filepath.Join(dir, "scout.log"))
	if err != nil {
		return []string{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return []string{}
	}
	start := int64(0)
	if info.Size() > logTailBytes {
		start = info.Size() - logTailBytes
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) > 0 {
		return []string{}
	}
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line is probably cut in half
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	return lines
}

// --- jobs -------------------------------------------------------------------

type apiReply struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, code int, ok bool, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiReply{OK: ok, Message: msg})
}

func (s *Server) startJob(w http.ResponseWriter, r *http.Request, kind string) {
	var req JobRequest
	if kind == KindRun {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) { // an empty body means "all defaults"
			reply(w, http.StatusBadRequest, false, "the request body must be JSON like {\"limit\":4,\"dry_run\":false}")
			return
		}
		if req.Limit < 0 || req.Limit > s.maxLimit() {
			reply(w, http.StatusBadRequest, false, fmt.Sprintf("limit must be between 0 and %d (0 means no limit)", s.maxLimit()))
			return
		}
	}
	if s.Runner == nil {
		reply(w, http.StatusServiceUnavailable, false, "this console was started without a runner")
		return
	}

	s.mu.Lock()
	if s.running != nil {
		k := s.running.kind
		s.mu.Unlock()
		reply(w, http.StatusConflict, false, "a "+k+" is already running; wait for it to finish")
		return
	}
	if s.Store.Locked() {
		s.mu.Unlock()
		reply(w, http.StatusConflict, false, "another scout process holds the run lock (a second console?); try again when it ends")
		return
	}
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	job := &runningJob{kind: kind, started: s.now(), req: req, cancel: cancel}
	s.running = job
	s.wg.Add(1)
	s.mu.Unlock()

	go s.execute(ctx, job)

	what := "Run started."
	switch {
	case kind == KindPatterns:
		what = "Pattern pass started."
	case req.DryRun:
		what = "Dry run started (fetch and dedupe only)."
	case req.Limit > 0:
		what = fmt.Sprintf("Run started (scoring at most %d).", req.Limit)
	}
	reply(w, http.StatusAccepted, true, what)
}

// stopJob cancels the job this console started. The job winds down on its own
// (an in-flight call is abandoned, nothing half-scored is recorded) and the
// status says "stopping" until it has.
func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.running == nil && s.Store.Locked():
		reply(w, http.StatusConflict, false, "that run was not started from this console, so it cannot be stopped here")
	case s.running == nil:
		reply(w, http.StatusConflict, false, "nothing is running")
	case s.running.stopping:
		reply(w, http.StatusAccepted, true, "Already stopping.")
	default:
		s.running.stopping = true
		s.running.cancel()
		reply(w, http.StatusAccepted, true, "Stopping. Whatever is already scored is kept.")
	}
}

func (s *Server) execute(ctx context.Context, job *runningJob) {
	defer s.wg.Done()
	defer job.cancel()
	code := 1
	func() {
		defer func() {
			if r := recover(); r != nil {
				code = 1
			}
		}()
		code = s.Runner(ctx, job.kind, job.req)
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = &JobResult{
		Kind: job.kind, ExitCode: code, Meaning: meaning(code),
		StartedAt: job.started, FinishedAt: s.now(), DryRun: job.req.DryRun,
		Stopped: job.stopping,
	}
	s.running = nil
}

func meaning(code int) string {
	switch code {
	case 0:
		return "ok"
	case 2:
		return "inference server unavailable or the run was aborted"
	case 3:
		return "stopped"
	default:
		return "error (see the log)"
	}
}
