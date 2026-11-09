package score

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
)

const goodIdeas = `[{"kind":"Build","title":"What if a CLI picked the best quant for your GPU?","detail":"Write a Go tool that tests quants and picks one."},{"kind":"Contribute","title":"What if the runtime shipped a Windows installer?","detail":"Send a pull request that adds an installer script."}]`

// scoreJSON builds a full reply with the given gist, tags and ideas JSON.
func scoreJSON(gist, tags, ideas string) string {
	return `{"gist":` + gist + `,"tags":` + tags + `,"ideas":` + ideas +
		`,"first_or_different":4,"testable_question":5,"fits_timebox":4,"ships_publicly":4,"has_community":4,"pass":true,"reason":"Concrete and testable."}`
}

var goodOutput = scoreJSON(`"A new local runtime shipped."`, `["Local LLM Inference"]`, goodIdeas)

func envelope(output string, truncated bool) string {
	b, _ := json.Marshal(map[string]any{"output": output, "truncated": truncated, "model_tier": "strong", "inference_ms": 3000.5, "tokens_generated": 90})
	return string(b)
}

type reply struct {
	status int
	body   string
}

// fake serves scripted replies in order (repeating the last) and records requests.
type fake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	replies  []reply
	bodies   []string
	inflight int32
	maxSeen  int32
}

func newFake(t *testing.T, replies ...reply) *fake {
	t.Helper()
	f := &fake{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		n := atomic.AddInt32(&f.inflight, 1)
		defer atomic.AddInt32(&f.inflight, -1)
		if n > atomic.LoadInt32(&f.maxSeen) {
			atomic.StoreInt32(&f.maxSeen, n)
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		i := len(f.bodies)
		f.bodies = append(f.bodies, string(b))
		rep := f.replies[min(i, len(f.replies)-1)]
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(rep.status)
		io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) scorer() (*Scorer, *int) {
	sleeps := 0
	s := New()
	s.BaseURL = f.srv.URL
	s.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	return s, &sleeps
}

func cand(id, category string) candidate.Candidate {
	return candidate.Candidate{ID: id, Category: category, Title: "Title " + id, Summary: "Summary", Snippet: "Snippet"}
}

func ok() reply { return reply{200, envelope(goodOutput, false)} }

func TestRequestShape(t *testing.T) {
	f := newFake(t, ok())
	s, _ := f.scorer()
	if _, err := s.ScoreOne(context.Background(), cand("1", "tech")); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req["reasoning"] != "off" || req["max_tokens"] != float64(800) || req["temperature"] != float64(0) ||
		req["preferred_tier"] != "strong" || req["min_tier"] != "strong" || req["modality"] != "text" {
		t.Fatalf("bad request fields: %v", req)
	}
	rf := req["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "topic_score" || js["strict"] != true || js["schema"] == nil {
		t.Fatalf("bad response_format: %v", rf)
	}
	msgs := req["text_input"].(map[string]any)["messages"].([]any)
	user := msgs[1].(map[string]any)["content"].(string)
	if msgs[0].(map[string]any)["role"] != "system" || !strings.HasPrefix(user, "Category: tech\nTitle: Title 1\nSummary: Summary\nSnippet: ") {
		t.Fatalf("bad messages: %v", msgs)
	}
}

func TestExtraRulesOnlyForNonTech(t *testing.T) {
	if strings.Contains(systemPrompt("tech", ""), "stricter bar") {
		t.Error("tech must not get the extra rules")
	}
	for _, cat := range []string{"humanitarian", "global", "local"} {
		if !strings.Contains(systemPrompt(cat, ""), "stricter bar") {
			t.Errorf("%s must get the extra rules", cat)
		}
	}
}

func TestScoredAndShortlisted(t *testing.T) {
	f := newFake(t, ok())
	s, _ := f.scorer()
	res, err := s.ScoreOne(context.Background(), cand("1", "tech"))
	if err != nil || res.Status != StatusScored || !res.Shortlisted || res.Attempts != 1 || res.Scores.Total() != 21 || res.ModelTier != "strong" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestShortlistRequiresPassAndAllScoresAtLeastFour(t *testing.T) {
	low := strings.Replace(strings.Replace(goodOutput, `"first_or_different":4`, `"first_or_different":3`, 1), `"testable_question":5`, `"testable_question":5`, 1)
	f := newFake(t, reply{200, envelope(low, false)})
	s, _ := f.scorer()
	res, _ := s.ScoreOne(context.Background(), cand("1", "tech"))
	if res.Shortlisted || res.Status != StatusScored || len(res.Warnings) != 1 {
		t.Fatalf("pass=true with a 3 must not shortlist: %+v", res)
	}

	rejected := strings.Replace(strings.Replace(goodOutput, `"pass":true`, `"pass":false`, 1), `"first_or_different":4`, `"first_or_different":5`, 1)
	f2 := newFake(t, reply{200, envelope(rejected, false)})
	s2, _ := f2.scorer()
	res2, _ := s2.ScoreOne(context.Background(), cand("1", "tech"))
	if res2.Shortlisted {
		t.Fatalf("pass=false must not shortlist: %+v", res2)
	}
}

func TestRetryOnceThenSucceed(t *testing.T) {
	for name, bad := range map[string]reply{
		"empty output":    {200, envelope("", false)},
		"truncated":       {200, envelope("", true)},
		"missing key":     {200, envelope(`{"pass":true}`, false)},
		"extra key":       {200, envelope(strings.Replace(goodOutput, `"pass"`, `"extra":1,"pass"`, 1), false)},
		"float score":     {200, envelope(strings.Replace(goodOutput, `"fits_timebox":4`, `"fits_timebox":3.5`, 1), false)},
		"out of range":    {200, envelope(strings.Replace(goodOutput, `"fits_timebox":4`, `"fits_timebox":6`, 1), false)},
		"empty reason":    {200, envelope(strings.Replace(goodOutput, `Concrete and testable.`, ` `, 1), false)},
		"prose output":    {200, envelope("Sure! Here you go", false)},
		"http 500":        {500, `{"code":"internal"}`},
		"bad envelope":    {200, `not json`},
		"string for int":  {200, envelope(strings.Replace(goodOutput, `"fits_timebox":4`, `"fits_timebox":"4"`, 1), false)},
		"model load fail": {503, `{"code":"model_load_failed","message":"llama-server returned HTTP 400"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, bad, ok())
			s, _ := f.scorer()
			res, err := s.ScoreOne(context.Background(), cand("1", "tech"))
			if err != nil || res.Status != StatusScored || res.Attempts != 2 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
}

func TestTwoFailuresMeanUnscoredNeverFailed(t *testing.T) {
	f := newFake(t, reply{200, envelope("", true)})
	s, _ := f.scorer()
	res, err := s.ScoreOne(context.Background(), cand("1", "tech"))
	if err != nil || res.Status != StatusUnscored || res.Scores != nil || res.Shortlisted || res.Attempts != 2 || res.Error != "reply truncated" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestBusyStatusesWaitBeforeRetry(t *testing.T) {
	for _, code := range []int{202, 429, 503} {
		f := newFake(t, reply{code, `{"code":"overloaded"}`}, ok())
		s, sleeps := f.scorer()
		res, _ := s.ScoreOne(context.Background(), cand("1", "tech"))
		if res.Status != StatusScored || *sleeps != 1 {
			t.Errorf("status %d: res=%+v sleeps=%d", code, res, *sleeps)
		}
	}
}

func TestParseFailureRetriesWithoutWaiting(t *testing.T) {
	f := newFake(t, reply{200, envelope("", false)}, ok())
	s, sleeps := f.scorer()
	s.ScoreOne(context.Background(), cand("1", "tech"))
	if *sleeps != 0 {
		t.Fatalf("slept %d times", *sleeps)
	}
}

func TestFatalStatuses(t *testing.T) {
	for name, rep := range map[string]reply{
		"invalid_grammar": {400, `{"code":"invalid_grammar","message":"bad schema"}`},
		"draining":        {503, `{"code":"draining","message":"maintenance"}`},
	} {
		f := newFake(t, rep)
		s, _ := f.scorer()
		_, err := s.ScoreOne(context.Background(), cand("1", "tech"))
		var fe *FatalError
		if !errors.As(err, &fe) {
			t.Errorf("%s: want FatalError, got %v", name, err)
		}
		if len(f.bodies) != 1 {
			t.Errorf("%s: fatal status must not be retried (%d requests)", name, len(f.bodies))
		}
	}
}

func TestServerDownIsFatal(t *testing.T) {
	f := newFake(t, ok())
	f.srv.Close()
	s, _ := f.scorer()
	_, err := s.ScoreOne(context.Background(), cand("1", "tech"))
	var fe *FatalError
	if !errors.As(err, &fe) || !strings.Contains(fe.Reason, "unreachable") {
		t.Fatalf("got %v", err)
	}
	if s.Healthy(context.Background()) == nil {
		t.Fatal("Healthy must fail when the server is down")
	}
}

func TestHealthy(t *testing.T) {
	f := newFake(t, ok())
	s, _ := f.scorer()
	if err := s.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCircuitBreakerAfterFiveUnscored(t *testing.T) {
	f := newFake(t, reply{200, envelope("", false)})
	s, _ := f.scorer()
	var cands []candidate.Candidate
	for i := 0; i < 8; i++ {
		cands = append(cands, cand(fmt.Sprint(i), "tech"))
	}
	emitted := 0
	err := s.ScoreAll(context.Background(), cands, func(candidate.Candidate, Result) { emitted++ })
	var fe *FatalError
	if !errors.As(err, &fe) || emitted != 5 {
		t.Fatalf("err=%v emitted=%d", err, emitted)
	}
}

func TestSuccessResetsBreakerAndCandidatesAreSequential(t *testing.T) {
	f := newFake(t, reply{200, envelope("", false)}, reply{200, envelope("", false)}, ok())
	s, _ := f.scorer()
	var cands []candidate.Candidate
	for i := 0; i < 6; i++ {
		cands = append(cands, cand(fmt.Sprint(i), "tech"))
	}
	n := 0
	if err := s.ScoreAll(context.Background(), cands, func(candidate.Candidate, Result) { n++ }); err != nil {
		t.Fatal(err)
	}
	if n != 6 || atomic.LoadInt32(&f.maxSeen) != 1 {
		t.Fatalf("emitted=%d max concurrent requests=%d", n, f.maxSeen)
	}
}

func TestUserMessageStaysUnderBudget(t *testing.T) {
	s := New()
	huge := candidate.Candidate{Category: "humanitarian", Title: strings.Repeat("T", 5000), Summary: strings.Repeat("S", 9000), Snippet: strings.Repeat("é", 50000)}
	sys := systemPrompt(huge.Category, strings.Repeat("p", 1500))
	msg := s.userMessage(huge, sys)
	if got := estimateTokens(sys) + estimateTokens(msg); got > s.MaxPrompt {
		t.Fatalf("estimated %d tokens, budget %d", got, s.MaxPrompt)
	}
	if !strings.Contains(msg, "Snippet: ") {
		t.Fatal("snippet label lost")
	}
}

func TestTruncationIsRuneSafe(t *testing.T) {
	for _, in := range []string{strings.Repeat("é", 100), strings.Repeat("日本語", 50), "plain ascii text here"} {
		for max := 0; max < 40; max++ {
			out := truncateBytes(in, max)
			if len(out) > max && max > 0 {
				t.Fatalf("len %d > max %d", len(out), max)
			}
			if !json.Valid([]byte(fmt.Sprintf("%q", out))) {
				t.Fatalf("invalid utf8 at max %d", max)
			}
		}
	}
}

func TestRubricVersionIsStableAndShort(t *testing.T) {
	if v := RubricVersion(""); len(v) != 8 || v != RubricVersion("") {
		t.Fatalf("version %q", v)
	}
}

func TestMissingTitleOrCategoryIsUnscored(t *testing.T) {
	f := newFake(t, ok())
	s, _ := f.scorer()
	res, err := s.ScoreOne(context.Background(), candidate.Candidate{ID: "x", Category: "tech"})
	if err != nil || res.Status != StatusUnscored || len(f.bodies) != 0 {
		t.Fatalf("res=%+v err=%v requests=%d", res, err, len(f.bodies))
	}
}

func TestSchemaAndRubricCarryTheV2Fields(t *testing.T) {
	var sch map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &sch); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	inner := sch["json_schema"].(map[string]any)["schema"].(map[string]any)
	props := inner["properties"].(map[string]any)
	required := inner["required"].([]any)
	if len(props) != len(scoreKeys) || len(required) != len(scoreKeys) {
		t.Fatalf("schema has %d properties and %d required keys, want %d", len(props), len(required), len(scoreKeys))
	}
	for _, k := range scoreKeys {
		if props[k] == nil {
			t.Errorf("schema lacks %q", k)
		}
	}
	sys := systemPrompt("tech", "")
	for _, want := range []string{"project hook", "at most 3", "Build or Contribute", `"What if...?"`, "last 2 weeks"} {
		if !strings.Contains(sys, want) {
			t.Errorf("rubric lacks %q", want)
		}
	}
}

func TestPromptPlusMaxTokensFitsTheContext(t *testing.T) {
	s := New()
	if s.MaxPrompt+scoreMaxTokens > contextTokens {
		t.Fatalf("MaxPrompt %d + max_tokens %d exceeds the %d-token context", s.MaxPrompt, scoreMaxTokens, contextTokens)
	}
	// A full-size profile and the longest system prompt (non-tech) must still
	// leave room for the candidate.
	sys := systemPrompt("humanitarian", strings.Repeat("x", 1500))
	if left := s.MaxPrompt - estimateTokens(sys); left < 600 {
		t.Fatalf("only %d tokens left for the candidate", left)
	}
}

func TestProfileIsAppendedToEverySystemPrompt(t *testing.T) {
	for _, cat := range []string{"tech", "humanitarian"} {
		sys := systemPrompt(cat, "  Go preferred.\n")
		if !strings.Contains(sys, "About the builder") || !strings.HasSuffix(sys, "\nGo preferred.") {
			t.Errorf("%s: profile not appended: ...%s", cat, sys[max(0, len(sys)-150):])
		}
		if strings.Contains(systemPrompt(cat, ""), "About the builder") || strings.Contains(systemPrompt(cat, " \n"), "About the builder") {
			t.Errorf("%s: an empty profile must add nothing", cat)
		}
	}
	f := newFake(t, ok())
	s, _ := f.scorer()
	s.Profile = "Likes geospatial."
	if _, err := s.ScoreOne(context.Background(), cand("1", "tech")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.bodies[0], "Likes geospatial.") {
		t.Fatal("request did not carry the profile")
	}
}

func TestRubricVersionIncludesTheProfile(t *testing.T) {
	a, b := RubricVersion("Go preferred."), RubricVersion("Python preferred.")
	if a == b || a == RubricVersion("") {
		t.Fatalf("profile does not change the version: %s %s %s", a, b, RubricVersion(""))
	}
	if RubricVersion("Go preferred.") != RubricVersion("  Go preferred.\n") {
		t.Fatal("surrounding whitespace must not change the version")
	}
}

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{
		"Local LLM Inference":       "local llm inference",
		"  local   llm\tinference ": "local llm inference",
		"decision models.":          "decision models",
		`"Open Data",`:              "open data",
		"--":                        "",
		"   ":                       "",
		"C++":                       "c++",
		"Édition  Géo":              "édition géo",
	} {
		if got := NormalizeTag(in); got != want {
			t.Errorf("NormalizeTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseScoresAcceptsAndNormalisesTheRichReply(t *testing.T) {
	sc, err := parseScores(scoreJSON(`"  Gist here. "`, `["Local LLM  Inference.","local llm inference","Decision Models"]`, goodIdeas))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Gist != "Gist here." || len(sc.Tags) != 2 || sc.Tags[0] != "local llm inference" || sc.Tags[1] != "decision models" {
		t.Fatalf("gist/tags not normalised: %+v", sc)
	}
	if len(sc.Ideas) != 2 || sc.Ideas[0].Kind != KindBuild || sc.Ideas[1].Kind != KindContribute {
		t.Fatalf("ideas: %+v", sc.Ideas)
	}
}

func TestParseScoresRejectsBadGistTagsAndIdeas(t *testing.T) {
	idea := func(kind, title, detail string) string {
		return fmt.Sprintf(`{"kind":%q,"title":%q,"detail":%q}`, kind, title, detail)
	}
	one := idea("Build", "What if a?", "Make a.")
	two := "[" + one + "," + idea("Contribute", "What if b?", "Make b.") + "]"
	for name, out := range map[string]string{
		"empty gist":           scoreJSON(`" "`, `["a"]`, two),
		"no tags":              scoreJSON(`"g"`, `[]`, two),
		"four tags":            scoreJSON(`"g"`, `["a","b","c","d"]`, two),
		"tag empty after norm": scoreJSON(`"g"`, `["a","--"]`, two),
		"blank tag":            scoreJSON(`"g"`, `[" "]`, two),
		"one idea":             scoreJSON(`"g"`, `["a"]`, "["+one+"]"),
		"four ideas":           scoreJSON(`"g"`, `["a"]`, "["+one+","+one+","+one+","+one+"]"),
		"bad kind":             scoreJSON(`"g"`, `["a"]`, "["+one+","+idea("build", "What if b?", "Make b.")+"]"),
		"empty title":          scoreJSON(`"g"`, `["a"]`, "["+one+","+idea("Build", " ", "Make b.")+"]"),
		"empty detail":         scoreJSON(`"g"`, `["a"]`, "["+one+","+idea("Build", "What if b?", "")+"]"),
		"extra idea key":       scoreJSON(`"g"`, `["a"]`, `[{"kind":"Build","title":"t","detail":"d","x":1},{"kind":"Build","title":"t","detail":"d"}]`),
		"tags not an array":    scoreJSON(`"g"`, `"a"`, two),
		"gist not a string":    scoreJSON(`5`, `["a"]`, two),
		"old 7-key reply":      `{"first_or_different":4,"testable_question":5,"fits_timebox":4,"ships_publicly":4,"has_community":4,"pass":true,"reason":"x"}`,
		"extra top-level key":  strings.Replace(goodOutput, `"pass"`, `"extra":1,"pass"`, 1),
	} {
		if _, err := parseScores(out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBadRichReplyIsRetriedThenUnscored(t *testing.T) {
	bad := envelope(scoreJSON(`"g"`, `[]`, goodIdeas), false)
	f := newFake(t, reply{200, bad}, ok())
	s, _ := f.scorer()
	res, err := s.ScoreOne(context.Background(), cand("1", "tech"))
	if err != nil || res.Status != StatusScored || res.Attempts != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	f2 := newFake(t, reply{200, bad})
	s2, _ := f2.scorer()
	res2, _ := s2.ScoreOne(context.Background(), cand("1", "tech"))
	if res2.Status != StatusUnscored || !strings.Contains(res2.Error, "tags") {
		t.Fatalf("res=%+v", res2)
	}
}

func TestFewActionableIdeasWarnButAreKept(t *testing.T) {
	weak := scoreJSON(`"g"`, `["a"]`, `[{"kind":"Experiment","title":"What if x?","detail":"Measure x."},{"kind":"Experiment","title":"What if y?","detail":"Measure y."},{"kind":"Build","title":"What if z?","detail":"Make z."}]`)
	f := newFake(t, reply{200, envelope(weak, false)}, ok())
	s, _ := f.scorer()
	res, _ := s.ScoreOne(context.Background(), cand("1", "tech"))
	if res.Status != StatusScored || res.Attempts != 1 || res.Scores == nil || len(res.Scores.Ideas) != 3 {
		t.Fatalf("a thin idea mix must not be rejected: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Build or Contribute") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if !res.Shortlisted {
		t.Error("the warning must not change the shortlist decision")
	}
	good, _ := s.ScoreOne(context.Background(), cand("2", "tech"))
	if len(good.Warnings) != 0 {
		t.Errorf("a reply with two Build/Contribute ideas must not warn: %v", good.Warnings)
	}
}

const askSchema = `{"type":"json_schema","json_schema":{"name":"thing","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}}}`

func TestAskSendsTheSharedRequestShape(t *testing.T) {
	f := newFake(t, reply{200, envelope(`{"a":"x"}`, false)})
	s, _ := f.scorer()
	rep, err := s.Ask(context.Background(), "SYS", "USER", askSchema, 600, nil)
	if err != nil || rep.Output != `{"a":"x"}` || rep.Attempts != 1 || rep.ModelTier != "strong" {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	var req map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &req)
	if req["reasoning"] != "off" || req["max_tokens"] != float64(600) || req["temperature"] != float64(0) || req["min_tier"] != "strong" {
		t.Fatalf("bad request: %v", req)
	}
	if rf := req["response_format"].(map[string]any); rf["json_schema"].(map[string]any)["name"] != "thing" {
		t.Fatalf("schema not forwarded: %v", rf)
	}
	msgs := req["text_input"].(map[string]any)["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "SYS" || msgs[1].(map[string]any)["content"] != "USER" {
		t.Fatalf("messages: %v", msgs)
	}
}

func TestAskFollowsTheScoringRetryRules(t *testing.T) {
	needA := func(out string) error {
		if !strings.Contains(out, `"a"`) {
			return errors.New("no a")
		}
		return nil
	}
	for name, bad := range map[string]reply{
		"empty":     {200, envelope("", false)},
		"truncated": {200, envelope(`{"a":`, true)},
		"invalid":   {200, envelope(`{"b":1}`, false)},
		"http 500":  {500, `{}`},
	} {
		f := newFake(t, bad, reply{200, envelope(`{"a":"x"}`, false)})
		s, sleeps := f.scorer()
		rep, err := s.Ask(context.Background(), "S", "U", askSchema, 600, needA)
		if err != nil || rep.Attempts != 2 || *sleeps != 0 {
			t.Errorf("%s: rep=%+v err=%v sleeps=%d", name, rep, err, *sleeps)
		}
	}
	// A busy server is waited out before the retry.
	f := newFake(t, reply{503, `{"code":"model_load_failed"}`}, reply{200, envelope(`{"a":"x"}`, false)})
	s, sleeps := f.scorer()
	if _, err := s.Ask(context.Background(), "S", "U", askSchema, 600, needA); err != nil || *sleeps != 1 {
		t.Fatalf("err=%v sleeps=%d", err, *sleeps)
	}
	// Two failures: an ordinary error, never fatal, never a third request.
	f2 := newFake(t, reply{200, envelope(`{"b":1}`, false)})
	s2, _ := f2.scorer()
	_, err := s2.Ask(context.Background(), "S", "U", askSchema, 600, needA)
	var fe *FatalError
	if err == nil || errors.As(err, &fe) || len(f2.bodies) != 2 || !strings.Contains(err.Error(), "no a") {
		t.Fatalf("err=%v requests=%d", err, len(f2.bodies))
	}
}

func TestAskFatalStatusIsFatalAndNotRetried(t *testing.T) {
	f := newFake(t, reply{503, `{"code":"draining"}`})
	s, _ := f.scorer()
	_, err := s.Ask(context.Background(), "S", "U", askSchema, 600, nil)
	var fe *FatalError
	if !errors.As(err, &fe) || len(f.bodies) != 1 {
		t.Fatalf("err=%v requests=%d", err, len(f.bodies))
	}
	f.srv.Close()
	if _, err := s.Ask(context.Background(), "S", "U", askSchema, 600, nil); !errors.As(err, &fe) {
		t.Fatalf("a dead server must be fatal, got %v", err)
	}
}

func TestAskRefusesAPromptThatWouldOverflowTheContext(t *testing.T) {
	f := newFake(t, ok())
	s, _ := f.scorer()
	_, err := s.Ask(context.Background(), strings.Repeat("s", 9000), strings.Repeat("u", 3000), askSchema, 600, nil)
	if err == nil || len(f.bodies) != 0 {
		t.Fatalf("err=%v requests=%d: an oversized prompt must not be sent", err, len(f.bodies))
	}
	// The budget shrinks as max_tokens grows.
	if _, err := s.Ask(context.Background(), strings.Repeat("s", 9000), "", askSchema, 2000, nil); err == nil || len(f.bodies) != 0 {
		t.Fatal("prompt + max_tokens over 4096 was sent")
	}
}
