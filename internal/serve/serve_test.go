package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/export"
	"github.com/thelab3507/topic-scout/internal/store"
)

var fixedNow = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

type rig struct {
	t       *testing.T
	s       *Server
	st      *store.Store
	ts      *httptest.Server
	exports string

	release chan struct{} // closing it lets a blocked job finish
	mu      sync.Mutex
	calls   []call
}

type call struct {
	kind string
	req  JobRequest
}

// newRig starts a console over a temp data dir with a fake runner that blocks
// until release is closed and then returns exit code 0.
func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, exports: filepath.Join(t.TempDir(), "exports"), release: make(chan struct{})}
	r.s = &Server{
		Store:     st,
		ExportDir: r.exports,
		Now:       func() time.Time { return fixedNow },
		Runner: func(ctx context.Context, kind string, req JobRequest) int {
			r.mu.Lock()
			r.calls = append(r.calls, call{kind, req})
			r.mu.Unlock()
			select {
			case <-r.release:
				return 0
			case <-ctx.Done():
				return 2
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.s.Start(ctx)
	r.ts = httptest.NewServer(r.s.Handler())
	t.Cleanup(func() {
		r.ts.Close()
		cancel()
		r.s.Wait()
	})
	return r
}

func (r *rig) finish() { close(r.release); r.s.Wait() }

func (r *rig) do(method, path, body string, headers map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (r *rig) get(path string) (*http.Response, string) { return r.do("GET", path, "", nil) }

func (r *rig) post(path, body string) (*http.Response, string) {
	return r.do("POST", path, body, map[string]string{RequestHeader: "1", "Content-Type": "application/json"})
}

func (r *rig) status() Status {
	r.t.Helper()
	res, body := r.get("/api/status")
	if res.StatusCode != 200 {
		r.t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		r.t.Fatalf("%v: %s", err, body)
	}
	return st
}

func (r *rig) waitCalls(n int) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.calls)
		r.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("runner was not called %d time(s)", n)
}

func TestPageIsSelfContainedAndLocked(t *testing.T) {
	r := newRig(t)
	res, body := r.get("/")
	if res.StatusCode != 200 || !strings.Contains(body, "Scout console") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	for _, want := range []string{"Plus Jakarta Sans Variable", "Run scout", "/api/status", RequestHeader} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Error("the page must not reference anything on the network")
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("responses must not be cached")
	}
	if res, _ := r.get("/no-such-page"); res.StatusCode != 404 {
		t.Errorf("unknown path: %d", res.StatusCode)
	}
}

func TestGuardRejectsForeignHostsAndCrossSiteWrites(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		host    string
		want    int
	}{
		{"rebinding host", "GET", "/api/status", nil, "evil.example.com", 403},
		{"lan ip host", "GET", "/api/status", nil, "192.168.1.5:8787", 403},
		{"localhost with port", "GET", "/api/status", nil, "localhost:8787", 200},
		{"ipv6 loopback", "GET", "/api/status", nil, "[::1]:8787", 200},
		{"post without header", "POST", "/api/run", nil, "", 403},
		{"post with foreign origin", "POST", "/api/run", map[string]string{RequestHeader: "1", "Origin": "https://evil.example.com"}, "", 403},
		{"post with null origin", "POST", "/api/run", map[string]string{RequestHeader: "1", "Origin": "null"}, "", 403},
		{"patterns without header", "POST", "/api/patterns", nil, "", 403},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, r.ts.URL+c.path, strings.NewReader("{}"))
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if c.host != "" {
			req.Host = c.host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 0 {
		t.Fatalf("a rejected request started a job: %v", r.calls)
	}
}

func TestSameOriginPostWithHeaderIsAccepted(t *testing.T) {
	r := newRig(t)
	res, _ := r.do("POST", "/api/run", `{}`, map[string]string{RequestHeader: "1", "Origin": r.ts.URL})
	if res.StatusCode != 202 {
		t.Fatalf("%d", res.StatusCode)
	}
	r.finish()
}

func TestValidAddrAcceptsOnlyLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8787", "localhost:80", "[::1]:9000", "127.1.2.3:1"} {
		if err := ValidAddr(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8787", ":8787", "192.168.1.5:8787", "example.com:80", "[::]:8787", "127.0.0.1", "", "8787"} {
		if err := ValidAddr(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestRunStartsOnceAndASecondRequestConflicts(t *testing.T) {
	r := newRig(t)
	res, body := r.post("/api/run", `{"limit":4}`)
	if res.StatusCode != 202 || !strings.Contains(body, "at most 4") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r.waitCalls(1)
	if st := r.status(); st.Running == nil || st.Running.Kind != KindRun || st.Running.Source != "console" {
		t.Fatalf("running = %+v", st.Running)
	}
	for _, path := range []string{"/api/run", "/api/patterns"} {
		if res, body := r.post(path, `{}`); res.StatusCode != 409 {
			t.Errorf("%s while running: %d %s", path, res.StatusCode, body)
		}
	}
	r.finish()

	st := r.status()
	if st.Running != nil {
		t.Fatalf("still running: %+v", st.Running)
	}
	if st.LastJob == nil || st.LastJob.Kind != KindRun || st.LastJob.ExitCode != 0 || st.LastJob.Meaning != "ok" {
		t.Fatalf("last job = %+v", st.LastJob)
	}
	if len(r.calls) != 1 || r.calls[0].req.Limit != 4 || r.calls[0].req.DryRun {
		t.Fatalf("calls = %+v", r.calls)
	}
}

func TestRunPassesDryRunAndEmptyBodyMeansDefaults(t *testing.T) {
	r := newRig(t)
	if res, body := r.post("/api/run", ``); res.StatusCode != 202 || !strings.Contains(body, "Run started") {
		t.Fatalf("empty body: %d %s", res.StatusCode, body)
	}
	r.finish()
	if r.calls[0].req != (JobRequest{}) {
		t.Fatalf("defaults expected, got %+v", r.calls[0].req)
	}

	r2 := newRig(t)
	res, body := r2.post("/api/run", `{"dry_run":true,"limit":2}`)
	if res.StatusCode != 202 || !strings.Contains(body, "Dry run") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r2.waitCalls(1)
	if st := r2.status(); st.Running == nil || !st.Running.DryRun {
		t.Fatalf("running = %+v", st.Running)
	}
	r2.finish()
	if got := r2.calls[0].req; !got.DryRun || got.Limit != 2 {
		t.Fatalf("req = %+v", got)
	}
	if st := r2.status(); st.LastJob == nil || !st.LastJob.DryRun {
		t.Fatalf("last job = %+v", st.LastJob)
	}
}

func TestRunRejectsBadBodies(t *testing.T) {
	r := newRig(t)
	for name, body := range map[string]string{
		"negative":      `{"limit":-1}`,
		"over the cap":  `{"limit":101}`,
		"wrong type":    `{"limit":"4"}`,
		"unknown field": `{"limit":1,"shell":"calc"}`,
		"not json":      `limit=4`,
	} {
		if res, _ := r.post("/api/run", body); res.StatusCode != 400 {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 0 {
		t.Fatalf("a rejected body started a job: %v", r.calls)
	}
}

func TestPatternsJobIgnoresTheBody(t *testing.T) {
	r := newRig(t)
	res, body := r.post("/api/patterns", `{"limit":9,"junk":1}`)
	if res.StatusCode != 202 || !strings.Contains(body, "Pattern pass") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r.waitCalls(1)
	if r.calls[0].kind != KindPatterns || r.calls[0].req != (JobRequest{}) {
		t.Fatalf("call = %+v", r.calls[0])
	}
	r.finish()
}

func TestRunConflictsWithARunHeldByAnotherProcess(t *testing.T) {
	r := newRig(t)
	unlock, err := r.st.Lock() // the scheduled task, say
	if err != nil {
		t.Fatal(err)
	}
	st := r.status()
	if st.Running == nil || st.Running.Source != "other" {
		t.Fatalf("running = %+v", st.Running)
	}
	res, body := r.post("/api/run", `{}`)
	if res.StatusCode != 409 || !strings.Contains(body, "another scout run") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	unlock()
	if r.status().Running != nil {
		t.Fatal("the lock is gone, so nothing is running")
	}
	if res, _ := r.post("/api/run", `{}`); res.StatusCode != 202 {
		t.Fatalf("after unlock: %d", res.StatusCode)
	}
	r.finish()
}

func TestAJobThatFailsOrPanicsIsReportedAndFreesTheServer(t *testing.T) {
	r := newRig(t)
	r.s.Runner = func(context.Context, string, JobRequest) int { return 2 }
	r.post("/api/run", `{}`)
	r.s.Wait()
	if lj := r.status().LastJob; lj == nil || lj.ExitCode != 2 || !strings.Contains(lj.Meaning, "unavailable") {
		t.Fatalf("last job = %+v", lj)
	}

	r.s.Runner = func(context.Context, string, JobRequest) int { panic("boom") }
	r.post("/api/run", `{}`)
	r.s.Wait()
	lj := r.status().LastJob
	if lj == nil || lj.ExitCode != 1 {
		t.Fatalf("a panic must read as an error: %+v", lj)
	}
	if res, _ := r.post("/api/run", `{}`); res.StatusCode != 202 {
		t.Fatalf("the server must accept a new job after a panic: %d", res.StatusCode)
	}
	r.s.Wait()
}

func TestCancellingTheServerContextStopsARunningJob(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.s.Start(ctx)
	r.post("/api/run", `{}`)
	r.waitCalls(1)
	cancel()
	done := make(chan struct{})
	go func() { r.s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after the context was cancelled")
	}
	if lj := r.status().LastJob; lj == nil || lj.ExitCode != 2 {
		t.Fatalf("last job = %+v", lj)
	}
}

func TestStatusReportsLastRunLogAndDigest(t *testing.T) {
	r := newRig(t)
	st := r.status()
	if st.LastRun != nil || st.HasDigest || len(st.Log) != 0 || st.Reports == nil || st.Log == nil {
		t.Fatalf("a fresh data dir: %+v", st)
	}

	r.st.WriteLastRun(store.LastRun{StartedAt: fixedNow, FinishedAt: fixedNow, Outcome: "ok", Scored: 7, Shortlisted: 2})
	os.WriteFile(r.st.DigestPath(), []byte("<html>digest</html>"), 0o644)
	os.WriteFile(filepath.Join(r.st.Dir, "scout.log"), []byte("line one\r\nline two\r\n"), 0o644)

	st = r.status()
	if st.LastRun == nil || st.LastRun.Scored != 7 || st.LastRun.Shortlisted != 2 || !st.HasDigest {
		t.Fatalf("%+v", st)
	}
	if strings.Join(st.Log, "|") != "line one|line two" {
		t.Fatalf("log = %q", st.Log)
	}
	if !st.Now.Equal(fixedNow) {
		t.Errorf("now = %v", st.Now)
	}
}

func TestLogTailKeepsOnlyTheEndOfAHugeLog(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("a log line that is fairly long, number ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	b.WriteString("the last line\n")
	os.WriteFile(filepath.Join(dir, "scout.log"), []byte(b.String()), 0o644)
	got := tailLog(dir, 5)
	if len(got) != 5 || got[4] != "the last line" {
		t.Fatalf("%q", got)
	}
	if got := tailLog(t.TempDir(), 5); len(got) != 0 {
		t.Fatalf("a missing log: %q", got)
	}
}

func TestDigestIsServedFromTheDataDir(t *testing.T) {
	r := newRig(t)
	if res, _ := r.get("/digest"); res.StatusCode != 404 {
		t.Fatalf("no digest yet: %d", res.StatusCode)
	}
	os.WriteFile(r.st.DigestPath(), []byte("<html>live digest</html>"), 0o644)
	res, body := r.get("/digest")
	if res.StatusCode != 200 || body != "<html>live digest</html>" {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("content type %q", res.Header.Get("Content-Type"))
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("a digest must not be allowed to run script: %q", csp)
	}
}

func TestReportsAreListedAndServedFromTheExportFolder(t *testing.T) {
	r := newRig(t)
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.Local)
	src := filepath.Join(t.TempDir(), "d.html")
	os.WriteFile(src, []byte("<html>day one</html>"), 0o644)
	name, err := export.Snapshot(src, r.exports, at, true)
	if err != nil {
		t.Fatal(err)
	}
	st := r.status()
	if len(st.Reports) != 1 || st.Reports[0].Name != name || st.ExportDir != r.exports || st.ExportError != "" {
		t.Fatalf("%+v", st)
	}
	for _, path := range []string{"/reports/" + name, "/reports/latest.html"} {
		if res, body := r.get(path); res.StatusCode != 200 || body != "<html>day one</html>" {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
	}
	if res, _ := r.get("/reports/digest-20250101-000000.html"); res.StatusCode != 404 {
		t.Errorf("a report that does not exist: %d", res.StatusCode)
	}
}

func TestReportsCannotReachOutsideTheExportFolder(t *testing.T) {
	r := newRig(t)
	secret := filepath.Join(filepath.Dir(r.exports), "secret.html")
	os.MkdirAll(r.exports, 0o755)
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	os.WriteFile(filepath.Join(r.st.Dir, "scout.log"), []byte("SECRET LOG"), 0o644)
	for _, path := range []string{
		"/reports/..%2Fsecret.html", "/reports/..%5Csecret.html", "/reports/%2e%2e/secret.html",
		"/reports/scout.log", "/reports/latest.html.tmp", "/reports/", "/reports",
	} {
		res, body := r.get(path)
		if res.StatusCode == 200 || strings.Contains(body, "SECRET") {
			t.Errorf("%s leaked: %d %q", path, res.StatusCode, body)
		}
	}
}

func TestExportFolderInsideAGitRepoIsReportedAndNeverServed(t *testing.T) {
	r := newRig(t)
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	r.s.ExportDir = filepath.Join(repo, "exports")
	st := r.status()
	if !strings.Contains(st.ExportError, "git repository") || len(st.Reports) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestNoExportFolderMeansNoReports(t *testing.T) {
	r := newRig(t)
	r.s.ExportDir = ""
	if st := r.status(); st.ExportDir != "" || len(st.Reports) != 0 || st.ExportError != "" {
		t.Fatalf("%+v", st)
	}
	if res, _ := r.get("/reports/latest.html"); res.StatusCode != 404 {
		t.Fatalf("%d", res.StatusCode)
	}
}

func TestNextRunStates(t *testing.T) {
	r := newRig(t)
	if nr := r.status().NextRun; nr.Error == "" || nr.At != nil {
		t.Fatalf("without a scheduler query: %+v", nr)
	}

	next := fixedNow.Add(24 * time.Hour)
	var failWith error
	r.s.NextRun = func(context.Context) (time.Time, error) { return next, failWith }
	if nr := r.status().NextRun; !nr.Pending || nr.At != nil {
		t.Fatalf("before the first answer: %+v", nr)
	}
	r.s.RefreshNext(context.Background())
	if nr := r.status().NextRun; nr.At == nil || !nr.At.Equal(next) || nr.Pending || nr.Error != "" {
		t.Fatalf("after: %+v", nr)
	}

	failWith = errors.New("task TheLabScout is not installed")
	r.s.RefreshNext(context.Background())
	if nr := r.status().NextRun; nr.At != nil || !strings.Contains(nr.Error, "not installed") {
		t.Fatalf("a failing query: %+v", nr)
	}
}

func TestRefreshNextGivesUpOnASlowScheduler(t *testing.T) {
	r := newRig(t)
	r.s.NextRun = func(ctx context.Context) (time.Time, error) {
		<-ctx.Done()
		return time.Time{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.s.RefreshNext(ctx)
	if nr := r.status().NextRun; nr.Error == "" {
		t.Fatalf("%+v", nr)
	}
}

func TestHasLatestOnlyOnceAnExportExists(t *testing.T) {
	r := newRig(t)
	if r.status().HasLatest {
		t.Fatal("nothing exported yet")
	}
	src := filepath.Join(t.TempDir(), "d.html")
	os.WriteFile(src, []byte("x"), 0o644)
	export.Snapshot(src, r.exports, fixedNow, false)
	if !r.status().HasLatest {
		t.Fatal("latest.html exists now")
	}
}
