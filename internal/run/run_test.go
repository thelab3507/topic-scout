package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const weakOutput = `{"first_or_different":2,"testable_question":3,"fits_timebox":3,"ships_publicly":2,"has_community":2,"pass":false,"reason":"Thin."}`

const goodOutput = `{"first_or_different":4,"testable_question":5,"fits_timebox":4,"ships_publicly":4,"has_community":4,"pass":true,"reason":"Concrete."}`

type env struct {
	st        *store.Store
	feed      *httptest.Server
	his       *httptest.Server
	gh        *httptest.Server
	feedBody  *string
	mu        sync.Mutex
	inferReqs int
	ghReqs    int
	inferFn   func(n int) (int, string) // n is the 1-based request count
	opts      Options
	log       *bytes.Buffer
}

func feedXML(titles ...string) string {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel>`)
	for i, t := range titles {
		fmt.Fprintf(&b, `<item><title>%s</title><link>https://e.com/%d</link><description>About %s</description><pubDate>Thu, 05 Nov 2026 1%d:00:00 +0000</pubDate></item>`, t, i, t, i%10)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func envelope(output string) string {
	b, _ := json.Marshal(map[string]any{"output": output, "model_tier": "strong"})
	return string(b)
}

func setup(t *testing.T, titles ...string) *env {
	t.Helper()
	e := &env{log: &bytes.Buffer{}}
	body := feedXML(titles...)
	e.feedBody = &body
	e.feed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, *e.feedBody) }))
	e.inferFn = func(int) (int, string) { return 200, envelope(goodOutput) }
	e.his = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		e.mu.Lock()
		e.inferReqs++
		n := e.inferReqs
		e.mu.Unlock()
		code, rep := e.inferFn(n)
		w.WriteHeader(code)
		io.WriteString(w, rep)
	}))
	e.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.ghReqs++
		e.mu.Unlock()
		io.WriteString(w, `{"total_count":1,"items":[{"full_name":"a/b","html_url":"https://github.com/a/b","stargazers_count":3}]}`)
	}))
	t.Cleanup(func() { e.feed.Close(); e.his.Close(); e.gh.Close() })

	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	e.st = st

	scorer := score.New()
	scorer.BaseURL = e.his.URL
	scorer.Sleep = func(context.Context, time.Duration) error { return nil }
	nov := novelty.New("")
	nov.BaseURL = e.gh.URL
	nov.Gap = 0
	nov.Now = func() time.Time { return now }

	cfg := &config.Config{LookbackDays: 14, MaxPerRun: 100, Sources: []config.Source{
		{Name: "feed", Type: config.TypeRSS, URL: e.feed.URL, Category: "tech", MaxItems: 50},
	}}
	e.opts = Options{Config: cfg, Store: st, HTTP: e.feed.Client(), Scorer: scorer, Novelty: nov, Now: func() time.Time { return now }, Log: e.log}
	return e
}

func (e *env) run() int { return Run(context.Background(), e.opts) }

func (e *env) lastRun(t *testing.T) *store.LastRun {
	t.Helper()
	lr, err := e.st.ReadLastRun()
	if err != nil || lr == nil {
		t.Fatalf("no last_run.json: %v", err)
	}
	return lr
}

func TestFullRunScoresShortlistsAndWritesDigest(t *testing.T) {
	e := setup(t, "Alpha tool released", "Beta framework benchmark")
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	lr := e.lastRun(t)
	if lr.Outcome != "ok" || lr.Fetched != 2 || lr.Scored != 2 || lr.Shortlisted != 2 || lr.Unscored != 0 {
		t.Fatalf("%+v", lr)
	}
	recs, _ := e.st.Results(time.Time{})
	if len(recs) != 2 || recs[0].RubricVersion != score.RubricVersion() {
		t.Fatalf("records: %+v", recs)
	}
	seen, _ := e.st.LoadSeen()
	if len(seen) != 2 {
		t.Fatalf("seen: %v", seen)
	}
	nov, _ := e.st.LoadNovelty()
	if len(nov) != 2 || e.ghReqs != 4 { // two checks, two queries each
		t.Fatalf("novelty=%d ghReqs=%d", len(nov), e.ghReqs)
	}
	html, err := os.ReadFile(e.st.DigestPath())
	if err != nil || !strings.Contains(string(html), "Alpha tool released") {
		t.Fatalf("digest missing or incomplete: %v", err)
	}
}

func TestSecondRunScoresNothingAgain(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.run()
	before := e.inferReqs
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if e.inferReqs != before {
		t.Fatalf("second run called the server %d more times", e.inferReqs-before)
	}
	if lr := e.lastRun(t); lr.Duplicates != 1 || lr.Scored != 0 {
		t.Fatalf("%+v", lr)
	}
}

func TestNewItemInLaterRunIsScoredOnlyOnce(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.run()
	*e.feedBody = feedXML("Alpha tool released", "Gamma compiler announced")
	e.run()
	if e.inferReqs != 2 {
		t.Fatalf("expected 2 inference calls in total, got %d", e.inferReqs)
	}
}

func TestOldSeenItemIsNotRescored(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.run()
	seen, _ := e.st.LoadSeen()
	for id, v := range seen {
		v.FirstSeen = now.AddDate(0, 0, -45) // outside the 30-day title window, still remembered
		seen[id] = v
	}
	e.st.SaveSeen(seen, now)
	before := e.inferReqs
	e.run()
	if e.inferReqs != before {
		t.Fatal("an item remembered for 45 days was scored again")
	}
}

func TestServerDownSkipsRunAndKeepsItemsForNextTime(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.his.Close()
	if code := e.run(); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	lr := e.lastRun(t)
	if lr.Outcome != "skipped" || !strings.Contains(lr.Message, "not starting it") {
		t.Fatalf("%+v", lr)
	}
	if seen, _ := e.st.LoadSeen(); len(seen) != 0 {
		t.Fatalf("items were marked seen while the server was down: %v", seen)
	}
	html, _ := os.ReadFile(e.st.DigestPath())
	if !strings.Contains(string(html), "did not finish normally (skipped)") {
		t.Fatal("digest does not warn that the run was skipped")
	}
}

func TestUnscoredItemsAreRetriedNextRunAndAttemptsAccumulate(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.inferFn = func(int) (int, string) { return 200, envelope("") } // empty output, always
	e.run()
	seen, _ := e.st.LoadSeen()
	for _, v := range seen {
		if v.Status != score.StatusUnscored || v.Attempts != 2 {
			t.Fatalf("after run 1: %+v", v)
		}
	}
	if lr := e.lastRun(t); lr.Unscored != 1 || lr.Shortlisted != 0 {
		t.Fatalf("%+v", lr)
	}

	e.inferFn = func(int) (int, string) { return 200, envelope(goodOutput) }
	e.run()
	seen, _ = e.st.LoadSeen()
	for _, v := range seen {
		if v.Status != score.StatusScored || v.Attempts != 3 {
			t.Fatalf("after run 2: %+v", v)
		}
	}
	recs, _ := e.st.Results(time.Time{})
	if len(recs) != 1 || recs[0].Result.Status != score.StatusScored {
		t.Fatalf("latest record should be the scored retry: %+v", recs)
	}
}

func TestUnscoredItemStopsBeingRetriedAfterMaxAttempts(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.inferFn = func(int) (int, string) { return 200, envelope("") }
	e.run() // attempts 2
	e.run() // attempts 4 -> parked
	before := e.inferReqs
	e.run()
	if e.inferReqs != before {
		t.Fatal("a parked item was sent to the server again")
	}
}

func TestDrainingAbortsRun(t *testing.T) {
	e := setup(t, "Alpha tool released", "Beta framework benchmark")
	e.inferFn = func(int) (int, string) { return 503, `{"code":"draining","message":"maintenance"}` }
	if code := e.run(); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if lr := e.lastRun(t); lr.Outcome != "aborted" || e.inferReqs != 1 {
		t.Fatalf("%+v requests=%d", lr, e.inferReqs)
	}
}

func TestLimitCapsHowManyAreScored(t *testing.T) {
	e := setup(t, "Alpha tool released", "Beta framework benchmark", "Gamma compiler announced")
	e.opts.Limit = 2
	e.run()
	if e.inferReqs != 2 {
		t.Fatalf("scored %d, want 2", e.inferReqs)
	}
}

func TestDryRunTouchesNothingAndNeverCallsServer(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.opts.DryRun = true
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if e.inferReqs != 0 || e.ghReqs != 0 {
		t.Fatal("dry run called a server")
	}
	entries, _ := os.ReadDir(e.st.Dir)
	if len(entries) != 0 {
		t.Fatalf("dry run wrote files: %v", entries)
	}
	if !strings.Contains(e.log.String(), "would score") {
		t.Fatal("dry run did not list candidates")
	}
}

func TestRunRefusesToStartWhileLocked(t *testing.T) {
	e := setup(t, "Alpha tool released")
	unlock, _ := e.st.Lock()
	defer unlock()
	if code := e.run(); code != ExitError || e.inferReqs != 0 {
		t.Fatalf("exit %d requests %d", code, e.inferReqs)
	}
}

func TestEverySourceFailingIsAnError(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.feed.Close()
	if code := e.run(); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if lr := e.lastRun(t); lr.Outcome != "error" {
		t.Fatalf("%+v", lr)
	}
}

func TestOneFailingSourceDoesNotStopTheRun(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.opts.Config.Sources = append(e.opts.Config.Sources,
		config.Source{Name: "dead", Type: config.TypeRSS, URL: "http://127.0.0.1:1/none", Category: "tech", MaxItems: 5})
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if lr := e.lastRun(t); len(lr.Warnings) != 1 || lr.Scored != 1 {
		t.Fatalf("%+v", lr)
	}
}

func TestNothingNewDoesNotTouchTheServer(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.run()
	e.his.Close() // server gone, but there is nothing to score
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d: a quiet day must not require the server", code)
	}
}

func TestNonTechCategoryReachesServerWithExtraRules(t *testing.T) {
	e := setup(t, "Flood dataset published")
	e.opts.Config.Sources[0].Category = "humanitarian"
	var body string
	e.his.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, envelope(goodOutput))
	})
	e.run()
	if !strings.Contains(body, "stricter bar") || !strings.Contains(body, "Category: humanitarian") {
		t.Fatalf("request body lacked extra rules: %.200s", body)
	}
}

func TestDigestIsWrittenWhileScoringAndFinishesStatic(t *testing.T) {
	words := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel", "India", "Juliet", "Kilo", "Lima"}
	titles := make([]string, len(words))
	for i, w := range words {
		titles[i] = w + " widget " + w + "ification"
	}
	e := setup(t, titles...)
	seen := map[int]string{}
	e.inferFn = func(n int) (int, string) {
		b, _ := os.ReadFile(e.st.DigestPath())
		seen[n] = string(b)
		return 200, envelope(weakOutput)
	}
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d: %s", code, e.log)
	}
	if !strings.Contains(seen[1], "0 of 12 scored.") || !strings.Contains(seen[1], `http-equiv="refresh"`) {
		t.Error("digest should say the run started before the first call")
	}
	if !strings.Contains(seen[12], "10 of 12 scored.") {
		t.Error("digest should be rewritten after every 10 results")
	}
	final, _ := os.ReadFile(e.st.DigestPath())
	if strings.Contains(string(final), `http-equiv="refresh"`) || strings.Contains(string(final), "Run in progress") {
		t.Error("final digest must be static")
	}
}
