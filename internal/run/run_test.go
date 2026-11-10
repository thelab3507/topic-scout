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

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/export"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const richPrefix = `{"gist":"A runtime shipped.","tags":["Local LLM"],"ideas":[{"kind":"Build","title":"What if a CLI?","detail":"Make a CLI."},{"kind":"Contribute","title":"What if a patch?","detail":"Send a patch."}],`

const weakOutput = richPrefix + `"first_or_different":2,"testable_question":3,"fits_timebox":3,"ships_publicly":2,"has_community":2,"pass":false,"reason":"Thin."}`

const goodOutput = richPrefix + `"first_or_different":4,"testable_question":5,"fits_timebox":4,"ships_publicly":4,"has_community":4,"pass":true,"reason":"Concrete."}`

const patternOutput = `{"name":"Local runtimes","forming":"Runtimes ship quant pickers.","open_question":"Which quant wins on 8 GB?","ideas":[{"kind":"Build","title":"What if a picker?","detail":"Make a picker."},{"kind":"Build","title":"What if two?","detail":"Make two."}]}`

type env struct {
	st        *store.Store
	feed      *httptest.Server
	his       *httptest.Server
	gh        *httptest.Server
	feedBody  *string
	mu        sync.Mutex
	inferReqs int
	bodies    []string // every inference request body, in order
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
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.inferReqs++
		e.bodies = append(e.bodies, string(b))
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
	if len(recs) != 2 || recs[0].RubricVersion != score.RubricVersion("") {
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

// --- builder profile --------------------------------------------------------

func (e *env) writeProfile(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.st.Dir, store.ProfileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProfileReachesEveryPromptAndTheRubricVersion(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.writeProfile(t, "Go preferred. Likes geospatial.")
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	if !strings.Contains(e.bodies[0], "About the builder") || !strings.Contains(e.bodies[0], "Go preferred. Likes geospatial.") {
		t.Fatalf("the scoring prompt lacks the profile: %.300s", e.bodies[0])
	}
	recs, _ := e.st.Results(time.Time{})
	want := score.RubricVersion("Go preferred. Likes geospatial.")
	if recs[0].RubricVersion != want || want == score.RubricVersion("") {
		t.Fatalf("record version %s, want %s (and different from the no-profile version)", recs[0].RubricVersion, want)
	}
	if html, _ := os.ReadFile(e.st.DigestPath()); !strings.Contains(string(html), "rubric "+want) {
		t.Error("the digest should show the version that includes the profile")
	}
}

func TestOversizedProfileIsCutAndWarnedAbout(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.writeProfile(t, strings.Repeat("a", store.MaxProfileBytes)+"TAILMARKER")
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(e.log.String(), "warning: profile.md is over 1500 bytes") {
		t.Errorf("no cut warning in the log: %s", e.log)
	}
	if strings.Contains(e.bodies[0], "TAILMARKER") || !strings.Contains(e.bodies[0], strings.Repeat("a", store.MaxProfileBytes)) {
		t.Error("the prompt must carry exactly the first 1500 bytes of the profile")
	}
}

func TestUnreadableProfileStopsTheRunInsteadOfScoringWithoutIt(t *testing.T) {
	e := setup(t, "Alpha tool released")
	if err := os.Mkdir(filepath.Join(e.st.Dir, store.ProfileName), 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	if code := e.run(); code != ExitError || e.inferReqs != 0 {
		t.Fatalf("exit %d, requests %d", code, e.inferReqs)
	}
	if lr := e.lastRun(t); lr.Outcome != "error" || !strings.Contains(lr.Message, "profile.md") {
		t.Fatalf("%+v", lr)
	}
}

// --- weekly patterns --------------------------------------------------------

// seedTagged stores n already-scored items under one tag, from alternating
// sources, so a run's one new item (source "feed") completes a pattern.
func (e *env) seedTagged(t *testing.T, n int, tag string) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("seed%d", i)
		err := e.st.AppendResult(store.RecordLine{
			RunAt: now.AddDate(0, 0, -1), RubricVersion: "old",
			Candidate: candidate.Candidate{ID: id, Source: []string{"hn", "blog"}[i%2], Category: "tech", Title: "Seed " + id,
				URL: "https://seed.example/" + id, Published: now.AddDate(0, 0, -2)},
			Result: score.Result{ID: id, Status: score.StatusScored, Scores: &score.Scores{
				Gist: "Gist " + id, Tags: []string{tag}, Reason: "r",
				Ideas: []score.Idea{{Kind: score.KindBuild, Title: "t", Detail: "d"}, {Kind: score.KindBuild, Title: "t", Detail: "d"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// scoreThenPattern answers the first request as a score and later ones as patterns.
func scoreThenPattern(patternReply string) func(int) (int, string) {
	return func(n int) (int, string) {
		if n == 1 {
			return 200, envelope(goodOutput)
		}
		return 200, envelope(patternReply)
	}
}

func (e *env) clusters(t *testing.T) cluster.File {
	t.Helper()
	f, err := cluster.Load(e.st)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWeeklyPatternPassRunsAfterScoringAndShowsInTheDigest(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 2, "local llm")
	e.inferFn = scoreThenPattern(patternOutput)
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	if e.inferReqs != 2 || !strings.Contains(e.bodies[1], "topic_pattern") || !strings.Contains(e.bodies[1], "Tag: local llm") {
		t.Fatalf("want one score then one pattern call, got %d requests", e.inferReqs)
	}
	f := e.clusters(t)
	if !f.GeneratedAt.Equal(now) || len(f.Clusters) != 1 || f.Clusters[0].Name != "Local runtimes" || f.Clusters[0].Count != 3 || f.Clusters[0].Sources != 3 {
		t.Fatalf("clusters.json: %+v", f)
	}
	html, _ := os.ReadFile(e.st.DigestPath())
	if !strings.Contains(string(html), "Local runtimes") || !strings.Contains(string(html), "Which quant wins on 8 GB?") {
		t.Error("the digest does not show the pattern")
	}
	if lr := e.lastRun(t); lr.Outcome != "ok" {
		t.Fatalf("%+v", lr)
	}
}

func TestPatternPassWaitsSixDays(t *testing.T) {
	for name, tc := range map[string]struct {
		age     time.Duration
		wantRun bool
	}{
		"two days ago":   {2 * 24 * time.Hour, false},
		"just under six": {6*24*time.Hour - time.Minute, false},
		"exactly six":    {6 * 24 * time.Hour, true},
		"a month ago":    {30 * 24 * time.Hour, true},
	} {
		e := setup(t, "Alpha tool released")
		e.seedTagged(t, 2, "local llm")
		prev := cluster.File{GeneratedAt: now.Add(-tc.age), Note: "previous"}
		if err := cluster.Save(e.st, prev); err != nil {
			t.Fatal(err)
		}
		e.inferFn = scoreThenPattern(patternOutput)
		if code := e.run(); code != ExitOK {
			t.Fatalf("%s: exit %d", name, code)
		}
		ran := e.inferReqs == 2
		if ran != tc.wantRun {
			t.Errorf("%s: pattern pass ran = %v, want %v", name, ran, tc.wantRun)
		}
		if !tc.wantRun && e.clusters(t).Note != "previous" {
			t.Errorf("%s: a pass that did not run must leave clusters.json alone", name)
		}
	}
}

func TestPatternPassDoesNotRunWhenScoringAborted(t *testing.T) {
	e := setup(t, "Alpha tool released", "Beta framework benchmark")
	e.seedTagged(t, 3, "local llm")
	e.inferFn = func(int) (int, string) { return 503, `{"code":"draining"}` }
	if code := e.run(); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if e.inferReqs != 1 {
		t.Fatalf("an aborted run made %d requests; the pattern pass must not add any", e.inferReqs)
	}
	if _, err := os.Stat(filepath.Join(e.st.Dir, cluster.FileName)); !os.IsNotExist(err) {
		t.Fatal("clusters.json written after an aborted run")
	}
}

func TestPatternFailureNeverFailsTheRunAndIsRetriedNextRun(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 2, "local llm")
	e.inferFn = scoreThenPattern(`{"name":"only a name"}`) // invalid, twice
	if code := e.run(); code != ExitOK {
		t.Fatalf("a failed pattern must not fail the run: exit %d\n%s", code, e.log)
	}
	if lr := e.lastRun(t); lr.Outcome != "ok" || lr.Scored != 1 {
		t.Fatalf("%+v", lr)
	}
	if !strings.Contains(e.log.String(), `pattern "local llm" skipped`) {
		t.Errorf("no skip warning in the log: %s", e.log)
	}
	if _, err := os.Stat(filepath.Join(e.st.Dir, cluster.FileName)); !os.IsNotExist(err) {
		t.Fatal("a pass that wrote nothing must not be saved, or it would not be retried for a week")
	}

	// Next run: nothing new to score, the pass is still due, and it works now.
	e.inferFn = func(int) (int, string) { return 200, envelope(patternOutput) }
	before := e.inferReqs
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if e.inferReqs != before+1 || len(e.clusters(t).Clusters) != 1 {
		t.Fatalf("the quiet-day run should have retried the pass: +%d requests, %+v", e.inferReqs-before, e.clusters(t))
	}
}

func TestOnePatternFailingKeepsTheOthers(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 2, "local llm")
	// A second, smaller pattern: three items under "open data" from two sources.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("od%d", i)
		e.st.AppendResult(store.RecordLine{RunAt: now.AddDate(0, 0, -1), Candidate: candidate.Candidate{ID: id, Source: []string{"hn", "blog"}[i%2], Title: id, Published: now.AddDate(0, 0, -2)},
			Result: score.Result{ID: id, Status: score.StatusScored, Scores: &score.Scores{Tags: []string{"open data"}}}})
	}
	e.inferFn = func(n int) (int, string) {
		switch {
		case n == 1:
			return 200, envelope(goodOutput)
		case n <= 3: // first pattern (3 items, local llm, ties on size with open data: alphabetical => local llm first) fails twice
			return 200, envelope(`{"nope":1}`)
		}
		return 200, envelope(patternOutput)
	}
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	f := e.clusters(t)
	if len(f.Clusters) != 1 || f.Clusters[0].Tag != "open data" || !strings.Contains(f.Note, "1 of 2") {
		t.Fatalf("clusters.json: %+v", f)
	}
}

func TestNoPatternsMeansNoModelCallAndANote(t *testing.T) {
	e := setup(t, "Alpha tool released")
	if code := e.run(); code != ExitOK {
		t.Fatal(code)
	}
	f := e.clusters(t)
	if e.inferReqs != 1 || len(f.Clusters) != 0 || f.Note != cluster.NoneNote || !f.GeneratedAt.Equal(now) {
		t.Fatalf("requests=%d file=%+v", e.inferReqs, f)
	}
	if html, _ := os.ReadFile(e.st.DigestPath()); !strings.Contains(string(html), "No pattern yet") {
		t.Error("the digest should show the note")
	}
}

func TestDuePatternPassOnAQuietDaySkipsQuietlyWhenTheServerIsDown(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 2, "local llm")
	e.inferFn = scoreThenPattern(patternOutput)
	e.run()
	os.Remove(filepath.Join(e.st.Dir, cluster.FileName)) // make the pass due again
	e.his.Close()
	if code := e.run(); code != ExitOK {
		t.Fatalf("a quiet day with the server down must still exit 0, got %d", code)
	}
	if !strings.Contains(e.log.String(), "pattern pass skipped") {
		t.Errorf("log: %s", e.log)
	}
}

func TestPatternsCommandForcesAPassHealthCheckFirstAndUsesTheProfile(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 3, "local llm")
	e.writeProfile(t, "Go preferred.")
	if err := cluster.Save(e.st, cluster.File{GeneratedAt: now.Add(-time.Hour), Note: "fresh"}); err != nil { // not due
		t.Fatal(err)
	}
	e.inferFn = func(int) (int, string) { return 200, envelope(patternOutput) }
	if code := Patterns(context.Background(), e.opts); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	f := e.clusters(t)
	if e.inferReqs != 1 || len(f.Clusters) != 1 || f.Note != "" {
		t.Fatalf("a forced pass must run even when the last one is fresh: requests=%d file=%+v", e.inferReqs, f)
	}
	if !strings.Contains(e.bodies[0], "Go preferred.") {
		t.Error("the pattern prompt lacks the profile")
	}
	if html, _ := os.ReadFile(e.st.DigestPath()); !strings.Contains(string(html), "Local runtimes") {
		t.Error("the digest was not rebuilt")
	}
}

func TestPatternsCommandReportsADeadServerAndKeepsTheOldResult(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 3, "local llm")
	old := cluster.File{GeneratedAt: now.AddDate(0, 0, -10), Note: "old result"}
	cluster.Save(e.st, old)
	e.his.Close()
	if code := Patterns(context.Background(), e.opts); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if e.clusters(t).Note != "old result" {
		t.Fatal("a dead server must not overwrite the previous patterns")
	}
}

func TestPatternsCommandWithNothingToGroupSavesTheNote(t *testing.T) {
	e := setup(t, "Alpha tool released")
	if code := Patterns(context.Background(), e.opts); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if f := e.clusters(t); f.Note != cluster.NoneNote || e.inferReqs != 0 {
		t.Fatalf("file=%+v requests=%d", f, e.inferReqs)
	}
}

func TestPatternsCommandReportsWhenEveryPatternFails(t *testing.T) {
	e := setup(t, "Alpha tool released")
	e.seedTagged(t, 3, "local llm")
	e.inferFn = func(int) (int, string) { return 200, envelope(`{"x":1}`) }
	if code := Patterns(context.Background(), e.opts); code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if _, err := os.Stat(filepath.Join(e.st.Dir, cluster.FileName)); !os.IsNotExist(err) {
		t.Fatal("nothing was written, so nothing should be saved")
	}
}

func TestPatternsCommandRefusesWhileARunHoldsTheLock(t *testing.T) {
	e := setup(t, "Alpha tool released")
	unlock, _ := e.st.Lock()
	defer unlock()
	if code := Patterns(context.Background(), e.opts); code != ExitError || e.inferReqs != 0 {
		t.Fatalf("exit %d requests %d", code, e.inferReqs)
	}
}

// exportTo points the run at a fresh export folder outside the data directory.
func (e *env) exportTo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "exports")
	e.opts.ExportDir = dir
	return dir
}

func TestFinishedRunExportsLatestAndAnArchive(t *testing.T) {
	e := setup(t, "Alpha tool released")
	dir := e.exportTo(t)
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	live, _ := os.ReadFile(e.st.DigestPath())
	latest, err := os.ReadFile(filepath.Join(dir, "latest.html"))
	if err != nil || string(latest) != string(live) {
		t.Fatalf("latest.html must equal the digest: %v", err)
	}
	reports, _ := export.List(dir)
	if len(reports) != 1 {
		t.Fatalf("want one archive, got %v", reports)
	}
	if strings.Contains(string(latest), "Run in progress") {
		t.Error("an interim page was exported; only the final digest may be")
	}
	if !strings.Contains(e.log.String(), "exported ") {
		t.Errorf("the export is not logged:\n%s", e.log)
	}
}

func TestSkippedRunRefreshesLatestButArchivesNothing(t *testing.T) {
	e := setup(t, "Alpha tool released")
	dir := e.exportTo(t)
	e.his.Close() // server down
	if code := e.run(); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "latest.html")); err != nil {
		t.Fatalf("latest.html missing: %v", err)
	}
	if reports, _ := export.List(dir); len(reports) != 0 {
		t.Fatalf("a skipped run must not add an archive: %v", reports)
	}
}

func TestAbortedRunStillArchivesWhatItScored(t *testing.T) {
	e := setup(t, "Alpha tool released", "Beta tool released")
	dir := e.exportTo(t)
	e.inferFn = func(n int) (int, string) {
		if n == 1 {
			return 200, envelope(goodOutput)
		}
		return 503, `{"code":"draining","message":"maintenance"}`
	}
	if code := e.run(); code != ExitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if reports, _ := export.List(dir); len(reports) != 1 {
		t.Fatalf("an aborted run scored something and must be archived: %v", reports)
	}
}

func TestExportProblemsNeverFailTheRun(t *testing.T) {
	e := setup(t, "Alpha tool released")
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	e.opts.ExportDir = filepath.Join(repo, "exports") // inside a git repository
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	if !strings.Contains(e.log.String(), "could not export the digest") {
		t.Errorf("the refusal must be logged:\n%s", e.log)
	}
	if _, err := os.Stat(filepath.Join(repo, "exports")); err == nil {
		t.Error("nothing may be written inside a git repository")
	}
	if _, err := os.Stat(e.st.DigestPath()); err != nil {
		t.Error("the normal digest must still be written")
	}
}

func TestNoExportFolderMeansNoExport(t *testing.T) {
	e := setup(t, "Alpha tool released")
	if code := e.run(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(e.log.String(), "export") {
		t.Errorf("unexpected export logging:\n%s", e.log)
	}
}

func TestDryRunExportsNothing(t *testing.T) {
	e := setup(t, "Alpha tool released")
	dir := e.exportTo(t)
	e.opts.DryRun = true
	e.run()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a dry run must not create the export folder")
	}
}

func TestPatternsCommandRefreshesLatestWithoutArchiving(t *testing.T) {
	e := setup(t, "Alpha tool released")
	dir := e.exportTo(t)
	e.seedTagged(t, 3, "local llm")
	e.inferFn = func(int) (int, string) { return 200, envelope(patternOutput) }
	if code := Patterns(context.Background(), e.opts); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.log)
	}
	latest, err := os.ReadFile(filepath.Join(dir, "latest.html"))
	if err != nil || !strings.Contains(string(latest), "Local runtimes") {
		t.Fatalf("latest.html must carry the new patterns: %v", err)
	}
	if reports, _ := export.List(dir); len(reports) != 0 {
		t.Fatalf("a patterns pass is not a run: %v", reports)
	}
}
