package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func scores(a, b, c, d, e int, pass bool) *score.Scores {
	return &score.Scores{FirstOrDifferent: a, TestableQuestion: b, FitsTimebox: c, ShipsPublicly: d, HasCommunity: e, Pass: pass, Reason: "because"}
}

func rec(id, title, status string, s *score.Scores, shortlisted bool) store.RecordLine {
	return store.RecordLine{
		RunAt: now.Add(-time.Hour), RubricVersion: "abcd1234",
		Candidate: candidate.Candidate{ID: id, Title: title, URL: "https://e.com/" + id, Source: "src", Category: "tech", Published: now.AddDate(0, 0, -2), Summary: "sum " + id},
		Result:    score.Result{ID: id, Status: status, Scores: s, Shortlisted: shortlisted, Attempts: 2, Error: "reply truncated"},
	}
}

func render(t *testing.T, in Input) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func base() Input {
	return Input{Now: now, RubricVersion: "abcd1234",
		LastRun: &store.LastRun{FinishedAt: now, Outcome: "ok"}}
}

func TestSectionsAndOrdering(t *testing.T) {
	in := base()
	in.Records = []store.RecordLine{
		rec("low", "LowShort", score.StatusScored, scores(4, 4, 4, 4, 4, true), true),
		rec("high", "HighShort", score.StatusScored, scores(5, 5, 5, 5, 5, true), true),
		rec("nope", "NotShort", score.StatusScored, scores(2, 3, 3, 2, 2, false), false),
		rec("uns", "UnscoredOne", score.StatusUnscored, nil, false),
		rec("dis", "DismissedOne", score.StatusScored, scores(5, 5, 5, 5, 5, true), true),
		rec("pick", "PickedOne", score.StatusScored, scores(3, 3, 3, 3, 3, false), false),
	}
	in.Decisions = map[string]store.DecisionRecord{
		"dis":  {Decision: store.Dismissed, Note: "meh"},
		"pick": {Decision: store.Picked, Note: "going for it"},
	}
	out := render(t, in)

	pos := func(s string) int { return strings.Index(out, s) }
	if !(pos("PickedOne") < pos("HighShort") && pos("HighShort") < pos("LowShort")) {
		t.Error("shortlist must list picked first, then higher totals first")
	}
	if pos("UnscoredOne") < pos("Needs review") || !strings.Contains(out, "reply truncated") {
		t.Error("unscored section missing its error")
	}
	if !strings.Contains(out, "NotShort") || !strings.Contains(out, "DismissedOne") {
		t.Error("rest and dismissed items must still be listed")
	}
	if strings.Index(out, "DismissedOne") < pos("Dismissed</h2>") {
		t.Error("dismissed item appeared outside the dismissed section")
	}
	if !strings.Contains(out, "going for it") {
		t.Error("picked note missing")
	}
	if !strings.Contains(out, "scout mark dis clear") {
		t.Error("undo command missing for dismissed item")
	}
}

func TestEscapesHostileContent(t *testing.T) {
	in := base()
	r := rec("x", `<script>alert(1)</script>`, score.StatusScored, scores(5, 5, 5, 5, 5, true), true)
	r.Candidate.URL = "javascript:alert(1)"
	r.Result.Scores.Reason = `<img src=x onerror=alert(1)>`
	in.Records = []store.RecordLine{r}
	out := render(t, in)
	for _, bad := range []string{"<script>alert", "<img src=x", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe content leaked: %s", bad)
		}
	}
}

func TestBannerWhenLastRunDidNotFinishNormally(t *testing.T) {
	in := base()
	in.LastRun = &store.LastRun{FinishedAt: now, Outcome: "skipped", Message: "inference server unreachable"}
	out := render(t, in)
	if !strings.Contains(out, "did not finish normally (skipped)") || !strings.Contains(out, "inference server unreachable") {
		t.Fatal("banner missing")
	}
	in.LastRun = nil
	if out := render(t, in); !strings.Contains(out, "No run recorded yet") {
		t.Fatal("no-run banner missing")
	}
	if out := render(t, base()); strings.Contains(out, `class="card alert"`) {
		t.Fatal("banner shown for a healthy run")
	}
}

func TestProgressRefreshesPageAndHidesStaleBanner(t *testing.T) {
	in := base()
	in.LastRun = &store.LastRun{FinishedAt: now, Outcome: "skipped", Message: "old problem"}
	in.Progress = "10 of 129 scored."
	out := render(t, in)
	for _, want := range []string{`http-equiv="refresh" content="30"`, "Run in progress", "10 of 129 scored."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "old problem") {
		t.Error("previous run's banner should not show while a run is in progress")
	}
	if out := render(t, base()); strings.Contains(out, `http-equiv="refresh"`) || strings.Contains(out, "Run in progress") {
		t.Error("a finished digest must be static")
	}
}

func TestNoveltyEvidenceShownAndSkippedExplained(t *testing.T) {
	in := base()
	in.Records = []store.RecordLine{
		rec("a", "WithEvidence", score.StatusScored, scores(5, 5, 5, 5, 5, true), true),
		rec("b", "WithoutEvidence", score.StatusScored, scores(4, 4, 4, 4, 4, true), true),
	}
	in.Novelty = map[string]novelty.Evidence{
		"a": {Query: "ollama vision", RecentTotal: 3, AllTimeTotal: 90, Top: []novelty.Repo{{FullName: "o/r", URL: "https://github.com/o/r", Stars: 5}}},
		"b": {Skipped: "GitHub search rate limited (HTTP 403)"},
	}
	out := render(t, in)
	for _, want := range []string{"3 repos in the last 30 days, 90 all time", "o/r", "GitHub novelty check: skipped", "rate limited"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestItemsOutsideWindowAreHidden(t *testing.T) {
	in := base()
	old := rec("o", "AncientItem", score.StatusScored, scores(5, 5, 5, 5, 5, true), true)
	old.RunAt = now.AddDate(0, 0, -(WindowDays + 1))
	in.Records = []store.RecordLine{old}
	if out := render(t, in); strings.Contains(out, "AncientItem") {
		t.Fatal("item older than the window shown")
	}
}

func TestEmptyDigestRenders(t *testing.T) {
	out := render(t, base())
	if !strings.Contains(out, "Nothing shortlisted") || !strings.Contains(out, "<h1>Digest</h1>") {
		t.Fatal("empty state missing")
	}
}

func TestPageIsSelfContained(t *testing.T) {
	out := render(t, base())
	if !strings.Contains(out, "data:font/woff2;base64,") || !strings.Contains(out, `"Plus Jakarta Sans Variable", "Plus Jakarta Sans", sans-serif`) {
		t.Error("font not embedded")
	}
	for _, ext := range []string{"http://fonts", "https://fonts", "cdn.", "<link rel=\"stylesheet\""} {
		if strings.Contains(out, ext) {
			t.Errorf("page references the network: %s", ext)
		}
	}
	if !strings.Contains(out, "prefers-color-scheme: dark") || !strings.Contains(out, `name="viewport"`) {
		t.Error("dark mode or viewport missing")
	}
}

func TestWriteIsAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "digest.html")
	if err := Write(p, base()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if b, _ := os.ReadFile(p); !bytes.Contains(b, []byte("<!doctype html>")) {
		t.Fatal("not html")
	}
}

func rich(id, title string, shortlisted bool) store.RecordLine {
	r := rec(id, title, score.StatusScored, scores(5, 5, 5, 5, 5, true), shortlisted)
	r.Result.Scores.Gist = "A runtime shipped " + id
	r.Result.Scores.Tags = []string{"local llm inference", "decision models"}
	r.Result.Scores.Ideas = []score.Idea{
		{Kind: score.KindBuild, Title: "What if a CLI picked the quant " + id + "?", Detail: "Write the CLI " + id + "."},
		{Kind: score.KindExperiment, Title: "What if we measured " + id + "?", Detail: "Time the quants."},
		{Kind: score.KindContribute, Title: "What if upstream took a patch " + id + "?", Detail: "Send a patch."},
	}
	return r
}

func TestCardsShowGistTagsIdeasAndTheBriefCommand(t *testing.T) {
	in := base()
	in.Records = []store.RecordLine{rich("short", "ShortItem", true)}
	out := render(t, in)
	for _, want := range []string{
		"<strong>In short:</strong> A runtime shipped short",
		`<span class="badge badge-tag">local llm inference</span>`,
		`<span class="badge badge-tag">decision models</span>`,
		`What if...`,
		`<span class="badge badge-default">Build</span>`,
		`<span class="badge badge-outline">Experiment</span>`,
		`<span class="badge badge-secondary">Contribute</span>`,
		"<strong>What if a CLI picked the quant short?</strong>",
		`<span class="muted">Write the CLI short.</span>`,
		"scout brief short",
		"scout mark short picked",
		"Hook 5", // renamed pill
	} {
		if !strings.Contains(out, want) {
			t.Errorf("card lacks %q", want)
		}
	}
	if strings.Contains(out, "Question 5") || !strings.Contains(out, `title="Project hook"`) {
		t.Error("the second pill must be Hook, with the full name Project hook")
	}
}

func TestNotShortlistedTableShowsTheSameInsight(t *testing.T) {
	in := base()
	r := rich("rest", "RestItem", false)
	r.Result.Scores.Pass = false
	in.Records = []store.RecordLine{r}
	out := render(t, in)
	table := out[strings.Index(out, "Scored, not shortlisted"):]
	for _, want := range []string{"In short:", "A runtime shipped rest", "badge-tag", "What if...", "What if a CLI picked the quant rest?", "because"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q", want)
		}
	}
}

func TestOldRecordsWithoutRichFieldsStillRender(t *testing.T) {
	in := base()
	in.Records = []store.RecordLine{rec("old", "OldItem", score.StatusScored, scores(5, 5, 5, 5, 5, true), true)}
	out := render(t, in)
	if !strings.Contains(out, "OldItem") || strings.Contains(out, "In short:") || strings.Contains(out, "What if...") {
		t.Fatal("an old record must render without empty gist or ideas blocks")
	}
}

func TestInsightIsEscaped(t *testing.T) {
	in := base()
	r := rich("x", "X", true)
	r.Result.Scores.Gist = `<script>alert(1)</script>`
	r.Result.Scores.Tags = []string{`<b onclick=x>`}
	r.Result.Scores.Ideas[0].Title = `<img src=x onerror=alert(1)>`
	in.Records = []store.RecordLine{r}
	in.Clusters = cluster.File{GeneratedAt: now, Clusters: []cluster.Cluster{{
		Name: `<script>pat</script>`, Tag: "t", Forming: `<i>f</i>`, Items: []cluster.Item{{Title: `<u>i</u>`, URL: "javascript:alert(1)"}},
	}}}
	out := render(t, in)
	for _, bad := range []string{"<script>", "<img src=x", "<b onclick", "<i>f</i>", "<u>i</u>", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe content leaked: %s", bad)
		}
	}
}

func TestPatternsSectionSitsAboveTheShortlist(t *testing.T) {
	in := base()
	in.Records = []store.RecordLine{rich("short", "ShortItem", true)}
	in.Clusters = cluster.File{GeneratedAt: now, Clusters: []cluster.Cluster{{
		Tag: "local llm inference", Name: "Local runtimes", Forming: "Runtimes ship quant pickers.", OpenQuestion: "Which quant wins on 8 GB?",
		Count: 4, Sources: 3,
		Ideas: []score.Idea{{Kind: score.KindBuild, Title: "What if a picker?", Detail: "Make a picker."}, {Kind: score.KindBuild, Title: "What if two?", Detail: "Make two."}},
		Items: []cluster.Item{{Title: "Item one", URL: "https://e.com/1", Source: "hn"}, {Title: "Item two", URL: "https://e.com/2", Source: "blog"}},
	}}}
	out := render(t, in)
	if !(strings.Index(out, "<h2>Patterns</h2>") < strings.Index(out, "<h2>Shortlist</h2>")) {
		t.Fatal("Patterns must come before the Shortlist")
	}
	for _, want := range []string{
		"Local runtimes", "local llm inference", "4 items from 3 sources",
		"<strong>Forming:</strong> Runtimes ship quant pickers.", "<strong>Open question:</strong> Which quant wins on 8 GB?",
		"What if a picker?", "<summary>2 items</summary>", `href="https://e.com/1"`, "Item two",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("patterns section lacks %q", want)
		}
	}
	if strings.Contains(out, cluster.NoneNote) {
		t.Error("the none note must not show when there are patterns")
	}
}

func TestPatternsNoteWhenThereAreNone(t *testing.T) {
	in := base()
	in.Clusters = cluster.File{GeneratedAt: now, Note: cluster.NoneNote}
	if out := render(t, in); !strings.Contains(out, "No pattern yet: no topic tag has 3 or more items") {
		t.Fatal("the pass's note is missing")
	}
	in.Clusters = cluster.File{} // never ran
	if out := render(t, in); !strings.Contains(out, "No pattern pass yet") || !strings.Contains(out, "scout patterns") {
		t.Fatal("never-ran note missing")
	}
	in.Clusters = cluster.File{GeneratedAt: now} // ran, found nothing, no note stored
	if out := render(t, in); !strings.Contains(out, "No pattern yet") {
		t.Fatal("an empty pass must still explain itself")
	}
}
