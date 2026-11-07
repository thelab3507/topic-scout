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

const goodOutput = `{"first_or_different":4,"testable_question":5,"fits_timebox":4,"ships_publicly":4,"has_community":4,"pass":true,"reason":"Concrete and testable."}`

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
	if req["reasoning"] != "off" || req["max_tokens"] != float64(400) || req["temperature"] != float64(0) ||
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
	if strings.Contains(systemPrompt("tech"), "stricter bar") {
		t.Error("tech must not get the extra rules")
	}
	for _, cat := range []string{"humanitarian", "global", "local"} {
		if !strings.Contains(systemPrompt(cat), "stricter bar") {
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
	low := `{"first_or_different":3,"testable_question":5,"fits_timebox":5,"ships_publicly":5,"has_community":5,"pass":true,"reason":"x"}`
	f := newFake(t, reply{200, envelope(low, false)})
	s, _ := f.scorer()
	res, _ := s.ScoreOne(context.Background(), cand("1", "tech"))
	if res.Shortlisted || res.Status != StatusScored || len(res.Warnings) != 1 {
		t.Fatalf("pass=true with a 3 must not shortlist: %+v", res)
	}

	rejected := `{"first_or_different":5,"testable_question":5,"fits_timebox":5,"ships_publicly":5,"has_community":5,"pass":false,"reason":"x"}`
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
	sys := systemPrompt(huge.Category)
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
	if v := RubricVersion(); len(v) != 8 || v != RubricVersion() {
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
