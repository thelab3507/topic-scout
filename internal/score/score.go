// Package score rates candidates against the Lab charter by calling the home
// inference server directly (never through the Railway relay).
//
// Rules, each observed against the real server:
//   - A reply is accepted only if it is HTTP 200, non-empty, not truncated and
//     parses into exactly the 10 schema keys. Otherwise retry once; after a
//     second failure the candidate is "unscored", never recorded as a fail.
//   - reasoning is always "off": with thinking on, a 400-token budget can
//     return an empty, truncated 200 and scores stop being comparable.
//   - Prompt plus max_tokens must fit a 4096-token context, so inputs are
//     truncated here; an overflow comes back as a mislabelled 503. The builder
//     profile is part of the prompt, so its size is capped (store.MaxProfileBytes).
//   - The server runs one inference at a time, so candidates go out sequentially.
package score

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thelab3507/topic-scout/internal/candidate"
)

const (
	StatusScored   = "scored"
	StatusUnscored = "unscored"

	maxAttempts = 2 // first try plus one retry

	// contextTokens is the server's context window. Every prompt plus its
	// max_tokens must fit inside it.
	contextTokens    = 4096
	scoreMaxTokens   = 800
	defaultMaxPrompt = 3200

	maxTags  = 3
	minIdeas = 2
	maxIdeas = 3

	// minActionableIdeas is how many ideas must be Build or Contribute before the
	// scorer stops warning about the mix.
	minActionableIdeas = 2

	// ShortlistMin is the lowest score allowed on every criterion for a
	// candidate to be shortlisted.
	ShortlistMin = 4

	// consecutiveFailureLimit aborts a run that is clearly not working rather
	// than burning minutes of retries on every remaining candidate.
	consecutiveFailureLimit = 5
)

//go:embed rubric/general.txt
var generalRubric string

//go:embed rubric/extra_rules.txt
var extraRules string

// Idea kinds.
const (
	KindBuild      = "Build"
	KindExperiment = "Experiment"
	KindContribute = "Contribute"
)

// IdeasSchema is the JSON schema of an ideas array. The pattern pass shares it
// so both replies describe ideas the same way.
const IdeasSchema = `{"type": "array", "minItems": 2, "maxItems": 3, "items": {` +
	`"type": "object", ` +
	`"properties": {` +
	`"kind": {"type": "string", "enum": ["Build","Experiment","Contribute"]}, ` +
	`"title": {"type": "string"}, ` +
	`"detail": {"type": "string"}}, ` +
	`"required": ["kind","title","detail"], "additionalProperties": false}}`

// schemaJSON is the strict json_schema the server enforces with a grammar.
// The nesting under "json_schema" -> "schema" is required. The grammar follows
// property order, so the model writes gist, tags and ideas before it scores:
// the project-hook score can then rest on an idea it has already written.
const schemaJSON = `{
  "type": "json_schema",
  "json_schema": {
    "name": "topic_score",
    "strict": true,
    "schema": {
      "type": "object",
      "properties": {
        "gist":  {"type": "string"},
        "tags":  {"type": "array", "minItems": 1, "maxItems": 3, "items": {"type": "string"}},
        "ideas": ` + IdeasSchema + `,
        "first_or_different": {"type": "integer", "minimum": 1, "maximum": 5},
        "testable_question":  {"type": "integer", "minimum": 1, "maximum": 5},
        "fits_timebox":       {"type": "integer", "minimum": 1, "maximum": 5},
        "ships_publicly":     {"type": "integer", "minimum": 1, "maximum": 5},
        "has_community":      {"type": "integer", "minimum": 1, "maximum": 5},
        "pass":   {"type": "boolean"},
        "reason": {"type": "string"}
      },
      "required": ["gist","tags","ideas","first_or_different","testable_question","fits_timebox","ships_publicly","has_community","pass","reason"],
      "additionalProperties": false
    }
  }
}`

var scoreKeys = []string{
	"gist", "tags", "ideas",
	"first_or_different", "testable_question", "fits_timebox",
	"ships_publicly", "has_community", "pass", "reason",
}

// RubricVersion identifies the exact prompt, schema and builder profile behind
// a score, so results stay comparable across edits. Scores written under
// different profiles are not comparable, which is why the profile is in here.
func RubricVersion(profile string) string {
	sum := sha256.Sum256([]byte(generalRubric + "\x00" + extraRules + "\x00" + schemaJSON + "\x00" + profileHeading + "\x00" + strings.TrimSpace(profile)))
	return hex.EncodeToString(sum[:])[:8]
}

// Scores is the validated model verdict.
//
// TestableQuestion is the "project hook" criterion. The JSON key keeps its old
// name so stored results stay readable.
type Scores struct {
	Gist             string   `json:"gist,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Ideas            []Idea   `json:"ideas,omitempty"`
	FirstOrDifferent int      `json:"first_or_different"`
	TestableQuestion int      `json:"testable_question"`
	FitsTimebox      int      `json:"fits_timebox"`
	ShipsPublicly    int      `json:"ships_publicly"`
	HasCommunity     int      `json:"has_community"`
	Pass             bool     `json:"pass"`
	Reason           string   `json:"reason"`
}

// Idea is one project the model thinks an item could spark.
type Idea struct {
	Kind   string `json:"kind"`   // Build, Experiment or Contribute
	Title  string `json:"title"`  // a "What if...?" question
	Detail string `json:"detail"` // one sentence on what to make
}

// Min is the lowest of the five criterion scores.
func (s Scores) Min() int {
	m := s.FirstOrDifferent
	for _, v := range []int{s.TestableQuestion, s.FitsTimebox, s.ShipsPublicly, s.HasCommunity} {
		if v < m {
			m = v
		}
	}
	return m
}

// Total is the sum of the five criterion scores, used only to rank the shortlist.
func (s Scores) Total() int {
	return s.FirstOrDifferent + s.TestableQuestion + s.FitsTimebox + s.ShipsPublicly + s.HasCommunity
}

// Result is the outcome for one candidate. Status "unscored" means the server
// never produced a valid verdict; it is kept for review and is not a fail.
// Scores is nil in that case.
type Result struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Scores          *Scores  `json:"scores,omitempty"`
	Shortlisted     bool     `json:"shortlisted"`
	Warnings        []string `json:"warnings,omitempty"`
	Error           string   `json:"error,omitempty"`
	Attempts        int      `json:"attempts"`
	ModelTier       string   `json:"model_tier,omitempty"`
	InferenceMS     float64  `json:"inference_ms,omitempty"`
	TokensGenerated int      `json:"tokens_generated,omitempty"`
}

// FatalError means the run itself cannot continue (server down, draining, bad
// schema, too many consecutive failures). It is never about one candidate.
type FatalError struct{ Reason string }

func (e *FatalError) Error() string { return e.Reason }

// Scorer talks to the inference server.
type Scorer struct {
	BaseURL    string
	HTTP       *http.Client
	RetryDelay time.Duration // wait before the retry after 202/429/503
	MaxPrompt  int           // estimated prompt-token ceiling (rubric + profile + candidate)

	// Profile is the builder profile (already capped) appended to every system
	// prompt. Empty means none.
	Profile string

	// Sleep waits for d or until ctx ends. Replaceable in tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// New returns a Scorer for the loopback server. The address is 127.0.0.1 on
// purpose: the server binds IPv4 loopback only, so "localhost" may resolve to
// ::1 and fail.
func New() *Scorer {
	return &Scorer{
		BaseURL:    "http://127.0.0.1:8080",
		HTTP:       &http.Client{Timeout: 150 * time.Second}, // server advice: at least 120s
		RetryDelay: 20 * time.Second,
		MaxPrompt:  defaultMaxPrompt,
		Sleep:      sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Healthy checks GET /healthz. If it fails the caller must stop and tell the
// user; the server is an elevated scheduled task and is never started here.
func (s *Scorer) Healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("inference server unreachable at %s: %w", s.BaseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var h struct {
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &h) != nil || h.Status != "ok" {
		return fmt.Errorf("inference server not healthy: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ScoreAll scores candidates strictly one at a time, calling emit after each.
// It returns a *FatalError if the run had to stop early.
func (s *Scorer) ScoreAll(ctx context.Context, cands []candidate.Candidate, emit func(candidate.Candidate, Result)) error {
	consecutive := 0
	for _, c := range cands {
		res, err := s.ScoreOne(ctx, c)
		if err != nil {
			return err
		}
		emit(c, res)
		if res.Status == StatusUnscored {
			consecutive++
			if consecutive >= consecutiveFailureLimit {
				return &FatalError{fmt.Sprintf("stopping: %d candidates in a row came back unscored (last: %s)", consecutive, res.Error)}
			}
		} else {
			consecutive = 0
		}
	}
	return nil
}

// ScoreOne scores a single candidate with one retry. A non-nil error is fatal
// for the whole run; per-candidate failure is reported as an unscored Result.
func (s *Scorer) ScoreOne(ctx context.Context, c candidate.Candidate) (Result, error) {
	res := Result{ID: c.ID}
	if strings.TrimSpace(c.Category) == "" || strings.TrimSpace(c.Title) == "" {
		res.Status = StatusUnscored
		res.Error = "candidate needs a category and a title"
		return res, nil
	}
	body, err := s.buildRequest(c)
	if err != nil {
		return res, &FatalError{"building request: " + err.Error()}
	}

	var sc Scores
	rep, reason, err := s.exchange(ctx, body, func(output string) error {
		var perr error
		sc, perr = parseScores(output)
		return perr
	})
	res.Attempts = rep.Attempts
	if err != nil {
		return res, err
	}
	if reason != "" {
		res.Status = StatusUnscored
		res.Error = reason
		return res, nil
	}

	res.Status = StatusScored
	res.Scores = &sc
	res.Shortlisted = sc.Pass && sc.Min() >= ShortlistMin
	res.ModelTier, res.InferenceMS, res.TokensGenerated = rep.ModelTier, rep.InferenceMS, rep.Tokens
	if sc.Pass && sc.Min() < ShortlistMin {
		res.Warnings = append(res.Warnings, "model said pass but a score is below 4; not shortlisted")
	}
	if !sc.Pass && sc.Min() >= ShortlistMin && !needsExtraRules(c.Category) {
		res.Warnings = append(res.Warnings, "model rejected despite every score being 4 or higher")
	}
	if BuildOrContribute(sc.Ideas) < minActionableIdeas {
		res.Warnings = append(res.Warnings, "fewer than two ideas are Build or Contribute")
	}
	return res, nil
}

// Reply is one accepted model reply.
type Reply struct {
	Output      string
	ModelTier   string
	InferenceMS float64
	Tokens      int
	Attempts    int
}

type failure struct {
	reason string
	wait   bool // server is busy or loading: pause before retrying
	fatal  bool // retrying cannot help and the whole run should stop
}

// Ask sends one system and user prompt under a strict response_format and
// returns the first reply that passes every transport rule and validate (nil
// validate accepts any non-empty output). It applies the same rules as scoring:
// reasoning off, temperature 0, an empty, truncated or invalid reply is retried
// once, and a busy server is waited out before the retry.
//
// A *FatalError means the server cannot be used at all. Any other error means
// both attempts failed, or the prompt could not fit the context window, and
// only this question should be given up on.
func (s *Scorer) Ask(ctx context.Context, system, user, schema string, maxTokens int, validate func(output string) error) (Reply, error) {
	budget := min(s.MaxPrompt, contextTokens-maxTokens)
	if need := EstimateTokens(system) + EstimateTokens(user); need > budget {
		return Reply{}, fmt.Errorf("prompt is about %d tokens, over the %d-token budget", need, budget)
	}
	body, err := marshalRequest(system, user, schema, maxTokens)
	if err != nil {
		return Reply{}, fmt.Errorf("building request: %w", err)
	}
	rep, reason, err := s.exchange(ctx, body, validate)
	if err != nil {
		return rep, err
	}
	if reason != "" {
		return rep, errors.New(reason)
	}
	return rep, nil
}

// exchange POSTs body up to maxAttempts times. It returns the accepted reply;
// or, when every attempt failed, a non-empty reason (the candidate is then
// "unscored", not failed); or a *FatalError. Reply.Attempts is always set.
func (s *Scorer) exchange(ctx context.Context, body []byte, validate func(string) error) (Reply, string, error) {
	var rep Reply
	var last string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		rep.Attempts = attempt
		out, f := s.attempt(ctx, body, validate)
		if f == nil {
			out.Attempts = attempt
			return *out, "", nil
		}
		if f.fatal {
			return rep, "", &FatalError{f.reason}
		}
		last = f.reason
		if attempt < maxAttempts && f.wait {
			if err := s.Sleep(ctx, s.RetryDelay); err != nil {
				return rep, "", err
			}
		}
	}
	return rep, last, nil
}

// attempt performs one POST and applies every reject rule. This is the single
// place the server is called, for scoring and for every other question.
func (s *Scorer) attempt(ctx context.Context, body []byte, validate func(string) error) (*Reply, *failure) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/v1/infer", bytes.NewReader(body))
	if err != nil {
		return nil, &failure{reason: err.Error(), fatal: true}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &failure{reason: ctx.Err().Error(), fatal: true}
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return nil, &failure{reason: "inference server unreachable (is it running? use 127.0.0.1): " + err.Error(), fatal: true}
		}
		return nil, &failure{reason: "request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &failure{reason: "reading response: " + err.Error()}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus(resp.StatusCode, raw)
	}

	var r struct {
		Output          string  `json:"output"`
		Truncated       bool    `json:"truncated"`
		TokensGenerated int     `json:"tokens_generated"`
		ModelTier       string  `json:"model_tier"`
		InferenceMS     float64 `json:"inference_ms"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, &failure{reason: "response envelope is not JSON: " + err.Error()}
	}
	if r.Truncated {
		return nil, &failure{reason: "reply truncated"}
	}
	if strings.TrimSpace(r.Output) == "" {
		return nil, &failure{reason: "empty output"}
	}
	if validate != nil {
		if err := validate(r.Output); err != nil {
			return nil, &failure{reason: "invalid output: " + err.Error()}
		}
	}
	return &Reply{Output: r.Output, ModelTier: r.ModelTier, InferenceMS: r.InferenceMS, Tokens: r.TokensGenerated}, nil
}

func classifyStatus(code int, raw []byte) *failure {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := strings.TrimSpace(e.Code + " " + e.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
		if len(detail) > 200 {
			detail = detail[:200]
		}
	}
	msg := fmt.Sprintf("HTTP %d: %s", code, detail)

	switch {
	case code == http.StatusBadRequest && e.Code == "invalid_grammar":
		return &failure{reason: msg + " (the schema is bad; this is a bug)", fatal: true}
	case code == http.StatusServiceUnavailable && e.Code == "draining":
		return &failure{reason: msg + " (server is in maintenance)", fatal: true}
	case code == http.StatusAccepted, code == http.StatusTooManyRequests, code == http.StatusServiceUnavailable:
		// 202 = deferred to the relay under CPU/VRAM pressure; 429 = overloaded;
		// other 503 = model load trouble. All mean "try again later".
		return &failure{reason: msg, wait: true}
	default:
		return &failure{reason: msg}
	}
}

// parseScores demands exactly the 10 keys, ints 1-5, a bool, non-empty strings,
// 1-3 tags that are non-empty once normalised and 2-3 well-formed ideas. Tags
// come back normalised and de-duplicated. The mix of idea kinds is not checked
// here: a reply with too few Build or Contribute ideas is kept and flagged.
func parseScores(output string) (Scores, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &fields); err != nil {
		return Scores{}, fmt.Errorf("not a JSON object: %w", err)
	}
	if len(fields) != len(scoreKeys) {
		return Scores{}, fmt.Errorf("expected exactly %d keys, got %d", len(scoreKeys), len(fields))
	}
	for _, k := range scoreKeys {
		if _, ok := fields[k]; !ok {
			return Scores{}, fmt.Errorf("missing key %q", k)
		}
	}
	dec := json.NewDecoder(strings.NewReader(output))
	dec.DisallowUnknownFields()
	var sc Scores
	if err := dec.Decode(&sc); err != nil {
		return Scores{}, err // e.g. 3.5 or "4" where an int is required
	}
	for name, v := range map[string]int{
		"first_or_different": sc.FirstOrDifferent, "testable_question": sc.TestableQuestion,
		"fits_timebox": sc.FitsTimebox, "ships_publicly": sc.ShipsPublicly, "has_community": sc.HasCommunity,
	} {
		if v < 1 || v > 5 {
			return Scores{}, fmt.Errorf("%s=%d is outside 1-5", name, v)
		}
	}
	if strings.TrimSpace(sc.Reason) == "" {
		return Scores{}, errors.New("reason is empty")
	}
	sc.Gist = strings.TrimSpace(sc.Gist)
	if sc.Gist == "" {
		return Scores{}, errors.New("gist is empty")
	}
	if n := len(sc.Tags); n < 1 || n > maxTags {
		return Scores{}, fmt.Errorf("expected 1-%d tags, got %d", maxTags, n)
	}
	var tags []string
	seen := map[string]bool{}
	for _, t := range sc.Tags {
		n := NormalizeTag(t)
		if n == "" {
			return Scores{}, fmt.Errorf("tag %q is empty after normalising", t)
		}
		if !seen[n] {
			seen[n] = true
			tags = append(tags, n)
		}
	}
	sc.Tags = tags
	if err := ValidateIdeas(sc.Ideas); err != nil {
		return Scores{}, err
	}
	for i := range sc.Ideas {
		sc.Ideas[i].Title = strings.TrimSpace(sc.Ideas[i].Title)
		sc.Ideas[i].Detail = strings.TrimSpace(sc.Ideas[i].Detail)
	}
	return sc, nil
}

// NormalizeTag lower-cases a tag, collapses runs of whitespace and trims
// punctuation from both ends, so "  Local LLM  Inference. " and
// "local llm inference" group together.
func NormalizeTag(tag string) string {
	t := strings.Join(strings.Fields(strings.ToLower(tag)), " ")
	return strings.TrimFunc(t, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
}

// ValidateIdeas checks the shape the schema promises: 2-3 ideas, each with a
// valid kind, a title and a detail.
func ValidateIdeas(ideas []Idea) error {
	if len(ideas) < minIdeas || len(ideas) > maxIdeas {
		return fmt.Errorf("expected %d-%d ideas, got %d", minIdeas, maxIdeas, len(ideas))
	}
	for i, idea := range ideas {
		switch idea.Kind {
		case KindBuild, KindExperiment, KindContribute:
		default:
			return fmt.Errorf("idea %d has kind %q, want Build, Experiment or Contribute", i+1, idea.Kind)
		}
		if strings.TrimSpace(idea.Title) == "" {
			return fmt.Errorf("idea %d has an empty title", i+1)
		}
		if strings.TrimSpace(idea.Detail) == "" {
			return fmt.Errorf("idea %d has an empty detail", i+1)
		}
	}
	return nil
}

// BuildOrContribute counts the ideas that make something rather than only
// measure it.
func BuildOrContribute(ideas []Idea) int {
	n := 0
	for _, idea := range ideas {
		if idea.Kind == KindBuild || idea.Kind == KindContribute {
			n++
		}
	}
	return n
}

// --- request building -------------------------------------------------------

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferRequest struct {
	Modality  string `json:"modality"`
	TextInput struct {
		Messages []message `json:"messages"`
	} `json:"text_input"`
	ResponseFormat json.RawMessage `json:"response_format"`
	Reasoning      string          `json:"reasoning"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    float64         `json:"temperature"`
	PreferredTier  string          `json:"preferred_tier"`
	MinTier        string          `json:"min_tier"`
}

// needsExtraRules is true for every category except "tech", per the charter:
// non-tech and humanitarian topics carry real people and a stricter bar.
func needsExtraRules(category string) bool {
	return !strings.EqualFold(strings.TrimSpace(category), "tech")
}

// profileHeading introduces the builder profile in every system prompt.
const profileHeading = "About the builder (use it to judge fit and to avoid what they want to avoid; never repeat it in your reply):"

// WithProfile appends the builder profile to a system prompt. An empty profile
// leaves the prompt unchanged.
func WithProfile(system, profile string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return system
	}
	return strings.TrimRight(system, "\n") + "\n\n" + profileHeading + "\n" + profile
}

func systemPrompt(category, profile string) string {
	p := strings.TrimSpace(generalRubric)
	if needsExtraRules(category) {
		p += "\n" + strings.TrimRight(extraRules, "\n")
	}
	return WithProfile(p, profile)
}

// marshalRequest builds the one request shape the server gets, whatever the
// question: strict schema, reasoning off, temperature 0, strong tier.
func marshalRequest(system, user, schema string, maxTokens int) ([]byte, error) {
	req := inferRequest{
		Modality:       "text",
		ResponseFormat: json.RawMessage(schema),
		Reasoning:      "off",
		MaxTokens:      maxTokens,
		Temperature:    0,
		PreferredTier:  "strong",
		MinTier:        "strong",
	}
	req.TextInput.Messages = []message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	return json.Marshal(req)
}

func (s *Scorer) buildRequest(c candidate.Candidate) ([]byte, error) {
	sys := systemPrompt(c.Category, s.Profile)
	return marshalRequest(sys, s.userMessage(c, sys), schemaJSON, scoreMaxTokens)
}

// EstimateTokens is deliberately pessimistic (3 bytes per token) because the
// server's own tokenize count is only exact while the model is loaded.
func EstimateTokens(s string) int { return estimateTokens(s) }

func estimateTokens(s string) int { return (len(s) + 2) / 3 }

// userMessage formats the candidate and trims it so system + user stay under
// MaxPrompt estimated tokens. Title and summary are capped first; whatever
// budget remains goes to the snippet.
func (s *Scorer) userMessage(c candidate.Candidate, sys string) string {
	title := TruncateRunes(OneLine(c.Title), 300)
	summary := TruncateRunes(OneLine(c.Summary), 1500)
	head := fmt.Sprintf("Category: %s\nTitle: %s\nSummary: %s\nSnippet: ", OneLine(c.Category), title, summary)

	budget := s.MaxPrompt - estimateTokens(sys) - estimateTokens(head)
	snippet := ""
	if budget > 0 {
		snippet = truncateBytes(OneLine(c.Snippet), budget*3)
	}
	return head + snippet
}

// OneLine collapses all whitespace, newlines included, to single spaces.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// TruncateRunes cuts s to at most max runes, ending in an ellipsis when it cut.
func TruncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// truncateBytes cuts at a rune boundary so the result is at most max bytes.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	cut := max - len(ell)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}
