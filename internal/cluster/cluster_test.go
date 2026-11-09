package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func rec(id, source string, ageDays int, tags ...string) store.RecordLine {
	return store.RecordLine{
		RunAt: now.AddDate(0, 0, -ageDays),
		Candidate: candidate.Candidate{
			ID: id, Source: source, Title: "Title " + id, URL: "https://e.com/" + id,
			Published: now.AddDate(0, 0, -ageDays).Add(-time.Hour),
		},
		Result: score.Result{ID: id, Status: score.StatusScored, Scores: &score.Scores{Gist: "Gist " + id, Tags: tags}},
	}
}

func ids(g Group) string {
	var out []string
	for _, r := range g.Records {
		out = append(out, r.Candidate.ID)
	}
	return strings.Join(out, ",")
}

func TestFindNeedsThreeItemsFromTwoSources(t *testing.T) {
	recs := []store.RecordLine{
		// enough of both
		rec("a1", "hn", 1, "local llm"), rec("a2", "blog", 2, "local llm"), rec("a3", "hn", 3, "local llm"),
		// three items, one source
		rec("b1", "hn", 1, "one source"), rec("b2", "hn", 2, "one source"), rec("b3", "hn", 3, "one source"),
		// two sources, two items
		rec("c1", "hn", 1, "too few"), rec("c2", "blog", 2, "too few"),
	}
	got := Find(recs, now)
	if len(got) != 1 || got[0].Tag != "local llm" || got[0].Sources != 2 || len(got[0].Records) != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestFindNormalisesTagsAndCountsAnItemOncePerTag(t *testing.T) {
	recs := []store.RecordLine{
		rec("a1", "hn", 1, "Local LLM Inference", "local llm inference."),
		rec("a2", "blog", 2, "  local   llm inference "),
		rec("a3", "reddit", 3, "local llm inference"),
	}
	got := Find(recs, now)
	if len(got) != 1 || got[0].Tag != "local llm inference" || len(got[0].Records) != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestFindIgnoresOldUnscoredAndUntaggedRecords(t *testing.T) {
	recs := []store.RecordLine{
		rec("a1", "hn", 1, "topic"), rec("a2", "blog", 2, "topic"),
		rec("old", "blog", WindowDays+1, "topic"), // outside the window
		rec("none", "blog", 1),                    // scored before tags existed
	}
	unscored := rec("uns", "blog", 1, "topic")
	unscored.Result = score.Result{ID: "uns", Status: score.StatusUnscored}
	recs = append(recs, unscored)
	if got := Find(recs, now); len(got) != 0 {
		t.Fatalf("only two usable items, want no pattern: %+v", got)
	}
	recs = append(recs, rec("a3", "hn", WindowDays, "topic")) // exactly on the edge counts
	if got := Find(recs, now); len(got) != 1 || len(got[0].Records) != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestFindKeepsTheBiggestThreeNewestFirst(t *testing.T) {
	var recs []store.RecordLine
	add := func(tag string, n int) {
		for i := 0; i < n; i++ {
			src := []string{"hn", "blog"}[i%2]
			recs = append(recs, rec(fmt.Sprintf("%s-%d", tag, i), src, i+1, tag))
		}
	}
	add("small", 3)
	add("huge", 6)
	add("big", 5)
	add("medium", 4)
	add("alpha", 4) // ties medium on size and sources: alphabetical wins
	got := Find(recs, now)
	var tags []string
	for _, g := range got {
		tags = append(tags, g.Tag)
	}
	if strings.Join(tags, ",") != "huge,big,alpha" {
		t.Fatalf("tags = %v", tags)
	}
	if ids(got[1]) != "big-0,big-1,big-2,big-3,big-4" {
		t.Fatalf("items must be newest published first: %s", ids(got[1]))
	}
}

func TestDue(t *testing.T) {
	for name, tc := range map[string]struct {
		last File
		want bool
	}{
		"never ran":    {File{}, true},
		"yesterday":    {File{GeneratedAt: now.Add(-24 * time.Hour)}, false},
		"just under 6": {File{GeneratedAt: now.Add(-6*24*time.Hour + time.Minute)}, false},
		"exactly 6":    {File{GeneratedAt: now.Add(-6 * 24 * time.Hour)}, true},
		"a month ago":  {File{GeneratedAt: now.AddDate(0, -1, 0)}, true},
	} {
		if got := Due(tc.last, now); got != tc.want {
			t.Errorf("%s: Due = %v, want %v", name, got, tc.want)
		}
	}
}

// --- model calls ------------------------------------------------------------

const goodReply = `{"name":"Local runtimes","forming":"Many runtimes now ship quant picks.","open_question":"Which quant is best for 8 GB?","ideas":[{"kind":"Build","title":"What if a CLI chose the quant?","detail":"Write a Go tool that tests quants."},{"kind":"Contribute","title":"What if the runtime had a picker?","detail":"Send a pull request."}]}`

type fakeServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	reply  func(n int, body string) (int, string) // n is the 1-based call number
}

func newFake(t *testing.T, reply func(n int, body string) (int, string)) *fakeServer {
	t.Helper()
	f := &fakeServer{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		n := len(f.bodies)
		f.mu.Unlock()
		code, out := f.reply(n, string(b))
		w.WriteHeader(code)
		io.WriteString(w, out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func envelope(output string) string {
	b, _ := json.Marshal(map[string]any{"output": output, "model_tier": "strong"})
	return string(b)
}

func (f *fakeServer) scorer() *score.Scorer {
	s := score.New()
	s.BaseURL = f.srv.URL
	s.Sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

func always(code int, body string) func(int, string) (int, string) {
	return func(int, string) (int, string) { return code, body }
}

func groupOf(tag string, n int) Group {
	g := Group{Tag: tag, Sources: 2}
	for i := 0; i < n; i++ {
		g.Records = append(g.Records, rec(fmt.Sprintf("%s-%d", tag, i), []string{"hn", "blog"}[i%2], i+1, tag))
	}
	return g
}

type logs struct{ lines []string }

func (l *logs) logf(f string, a ...any) { l.lines = append(l.lines, fmt.Sprintf(f, a...)) }

func TestGenerateMakesOneCallPerPatternWithTheRightRequest(t *testing.T) {
	f := newFake(t, always(200, envelope(goodReply)))
	g1, g2 := groupOf("local llm", 12), groupOf("open data", 3)
	var l logs
	file, failed, err := Generate(context.Background(), f.scorer(), []Group{g1, g2}, now, "Go preferred.", l.logf)
	if err != nil || failed != 0 || len(file.Clusters) != 2 || len(f.bodies) != 2 {
		t.Fatalf("err=%v failed=%d clusters=%d calls=%d", err, failed, len(file.Clusters), len(f.bodies))
	}
	if !file.GeneratedAt.Equal(now) || file.Note != "" {
		t.Fatalf("file: %+v", file)
	}

	var req map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &req)
	if req["max_tokens"] != float64(600) || req["reasoning"] != "off" || req["temperature"] != float64(0) {
		t.Fatalf("bad request: %v", req)
	}
	rf := req["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["name"] != "topic_pattern" || rf["json_schema"].(map[string]any)["strict"] != true {
		t.Fatalf("bad response_format: %v", rf)
	}
	msgs := req["text_input"].(map[string]any)["messages"].([]any)
	system, user := msgs[0].(map[string]any)["content"].(string), msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(system, "About the builder") || !strings.HasSuffix(system, "Go preferred.") {
		t.Error("profile not appended to the pattern prompt")
	}
	if !strings.Contains(user, "Tag: local llm") || !strings.Contains(user, "10 shown of 12") {
		t.Errorf("user message: %s", user)
	}
	if n := strings.Count(user, "\n") - 2; n != MaxItemsSent { // minus the Tag and Items header lines
		t.Errorf("sent %d items, want at most %d", n, MaxItemsSent)
	}
	if !strings.Contains(user, "[hn] Title local llm-0: Gist local llm-0") || strings.Contains(user, "local llm-11") {
		t.Errorf("items must be source, title, gist, newest first, capped: %s", user)
	}

	c := file.Clusters[0]
	if c.Tag != "local llm" || c.Count != 12 || c.Sources != 2 || len(c.Items) != 12 || c.Name != "Local runtimes" || len(c.Ideas) != 2 {
		t.Fatalf("cluster: %+v", c)
	}
}

func TestGenerateSkipsAFailedPatternWithAWarning(t *testing.T) {
	// Pattern 1 fails both attempts (calls 1 and 2); pattern 2 succeeds.
	f := newFake(t, func(n int, _ string) (int, string) {
		if n <= 2 {
			return 200, envelope(`{"name":"only a name"}`)
		}
		return 200, envelope(goodReply)
	})
	var l logs
	file, failed, err := Generate(context.Background(), f.scorer(), []Group{groupOf("one", 3), groupOf("two", 3)}, now, "", l.logf)
	if err != nil || failed != 1 || len(file.Clusters) != 1 || file.Clusters[0].Tag != "two" {
		t.Fatalf("err=%v failed=%d clusters=%+v", err, failed, file.Clusters)
	}
	if len(l.lines) != 1 || !strings.Contains(l.lines[0], `pattern "one" skipped`) {
		t.Fatalf("log: %v", l.lines)
	}
	if !strings.Contains(file.Note, "1 of 2") {
		t.Errorf("note: %q", file.Note)
	}
}

func TestGenerateStopsWhenTheServerIsGone(t *testing.T) {
	f := newFake(t, func(n int, _ string) (int, string) {
		if n == 1 {
			return 200, envelope(goodReply)
		}
		return 503, `{"code":"draining"}`
	})
	var l logs
	file, _, err := Generate(context.Background(), f.scorer(), []Group{groupOf("one", 3), groupOf("two", 3), groupOf("three", 3)}, now, "", l.logf)
	var fe *score.FatalError
	if !errors.As(err, &fe) || len(f.bodies) != 2 || len(file.Clusters) != 1 {
		t.Fatalf("err=%v calls=%d clusters=%d", err, len(f.bodies), len(file.Clusters))
	}
}

func TestGenerateWithNoGroupsCallsNothingAndLeavesANote(t *testing.T) {
	f := newFake(t, always(200, envelope(goodReply)))
	file, failed, err := Generate(context.Background(), f.scorer(), nil, now, "", (&logs{}).logf)
	if err != nil || failed != 0 || len(file.Clusters) != 0 || file.Note != NoneNote || len(f.bodies) != 0 {
		t.Fatalf("file=%+v failed=%d err=%v calls=%d", file, failed, err, len(f.bodies))
	}
}

func TestWorstCasePromptFitsTheBudget(t *testing.T) {
	g := Group{Tag: strings.Repeat("t", 100), Sources: 2}
	for i := 0; i < 20; i++ {
		r := rec(fmt.Sprint(i), strings.Repeat("s", 200), i+1, "t")
		r.Candidate.Title = strings.Repeat("T", 2000)
		r.Result.Scores.Gist = strings.Repeat("é", 2000)
		g.Records = append(g.Records, r)
	}
	s := score.New()
	system, user := prompt(g, strings.Repeat("p", store.MaxProfileBytes), s.MaxPrompt)
	if got := score.EstimateTokens(system) + score.EstimateTokens(user); got > s.MaxPrompt {
		t.Fatalf("prompt is about %d tokens, budget %d", got, s.MaxPrompt)
	}
	if s.MaxPrompt+MaxTokens > 4096 {
		t.Fatalf("MaxPrompt %d + max_tokens %d does not fit the 4096 context", s.MaxPrompt, MaxTokens)
	}
	if !strings.Contains(user, "1. [") {
		t.Fatal("at least one item must always be sent")
	}
}

func TestParseReplyRejectsBadShapes(t *testing.T) {
	idea := `{"kind":"Build","title":"What if?","detail":"Make it."}`
	two := `[` + idea + `,` + idea + `]`
	mk := func(name, forming, q, ideas string) string {
		return fmt.Sprintf(`{"name":%q,"forming":%q,"open_question":%q,"ideas":%s}`, name, forming, q, ideas)
	}
	if _, err := parseReply(mk("n", "f", "q", two)); err != nil {
		t.Fatalf("a good reply was rejected: %v", err)
	}
	for name, out := range map[string]string{
		"empty name":      mk(" ", "f", "q", two),
		"empty forming":   mk("n", "", "q", two),
		"empty question":  mk("n", "f", " ", two),
		"one idea":        mk("n", "f", "q", `[`+idea+`]`),
		"bad kind":        mk("n", "f", "q", `[`+idea+`,{"kind":"Other","title":"t","detail":"d"}]`),
		"extra key":       `{"name":"n","forming":"f","open_question":"q","ideas":` + two + `,"x":1}`,
		"missing key":     `{"name":"n","forming":"f","ideas":` + two + `}`,
		"not json object": `[]`,
	} {
		if _, err := parseReply(out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSchemaIsValidJSONWithTheExpectedKeys(t *testing.T) {
	var sch map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &sch); err != nil {
		t.Fatal(err)
	}
	props := sch["json_schema"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	for _, k := range replyKeys {
		if props[k] == nil {
			t.Errorf("schema lacks %q", k)
		}
	}
}

func TestSaveLoadAndContaining(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := Load(st); err != nil || !f.GeneratedAt.IsZero() || !Due(f, now) {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	want := File{GeneratedAt: now, Clusters: []Cluster{{Tag: "t", Name: "N", Count: 3, Items: []Item{{ID: "a"}, {ID: "b"}}}}, Note: "n"}
	if err := Save(st, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(st)
	if err != nil || !got.GeneratedAt.Equal(now) || got.Note != "n" || len(got.Clusters) != 1 || got.Clusters[0].Name != "N" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if c := got.Containing("b"); len(c) != 1 || c[0].Tag != "t" {
		t.Fatalf("Containing(b) = %+v", c)
	}
	if c := got.Containing("zzz"); len(c) != 0 {
		t.Fatalf("Containing(zzz) = %+v", c)
	}
}
