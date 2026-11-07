// Package novelty gathers GitHub search evidence for the charter's "first or
// different: verify with a quick search" check. It informs the human review and
// never rejects anything on its own.
package novelty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

const recentDays = 30

// Repo is one search hit.
type Repo struct {
	FullName    string `json:"full_name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Stars       int    `json:"stars"`
	Created     string `json:"created"`
}

// Evidence is what the search found. Skipped is set instead of results when the
// check could not run.
type Evidence struct {
	Query        string `json:"query,omitempty"`
	RecentTotal  int    `json:"recent_total"`
	Recent       []Repo `json:"recent,omitempty"`
	AllTimeTotal int    `json:"all_time_total"`
	Top          []Repo `json:"top,omitempty"`
	Skipped      string `json:"skipped,omitempty"`
}

// Checker queries the GitHub repository search API.
type Checker struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
	// Gap is the pause between requests, to stay under the search rate limit
	// (10/min unauthenticated, 30/min with a token).
	Gap   time.Duration
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	requests    int
	rateLimited bool
}

// New returns a Checker. A token raises the rate limit; it is optional.
func New(token string) *Checker {
	gap := 7 * time.Second
	if token != "" {
		gap = 2200 * time.Millisecond
	}
	return &Checker{
		BaseURL: "https://api.github.com",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Token:   token,
		Gap:     gap,
		Now:     time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// Check searches for repositories matching the title's keywords: once for
// anything created recently, once all-time by stars. After the first rate-limit
// response every later check is skipped without calling GitHub.
func (c *Checker) Check(ctx context.Context, title string) Evidence {
	kw := Keywords(title)
	if len(kw) == 0 {
		return Evidence{Skipped: "no usable keywords in the title"}
	}
	if c.rateLimited {
		return Evidence{Skipped: "GitHub search rate limit reached earlier in this run"}
	}
	query := strings.Join(kw, " ")
	ev := Evidence{Query: query}

	since := c.Now().AddDate(0, 0, -recentDays).Format("2006-01-02")
	total, repos, err := c.search(ctx, query+" created:>="+since)
	if err != nil {
		return Evidence{Query: query, Skipped: err.Error()}
	}
	ev.RecentTotal, ev.Recent = total, repos

	total, repos, err = c.search(ctx, query)
	if err != nil {
		return Evidence{Query: query, Skipped: err.Error()}
	}
	ev.AllTimeTotal, ev.Top = total, repos
	return ev
}

func (c *Checker) search(ctx context.Context, q string) (int, []Repo, error) {
	if c.requests > 0 && c.Gap > 0 {
		if err := c.Sleep(ctx, c.Gap); err != nil {
			return 0, nil, err
		}
	}
	c.requests++
	params := url.Values{"q": {q}, "sort": {"stars"}, "order": {"desc"}, "per_page": {"3"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/search/repositories?"+params.Encode(), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "topic-scout/0.1 (+https://github.com/thelab3507/topic-scout)")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GitHub search failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		c.rateLimited = true
		return 0, nil, fmt.Errorf("GitHub search rate limited (HTTP %d)", resp.StatusCode)
	default:
		return 0, nil, fmt.Errorf("GitHub search returned HTTP %d", resp.StatusCode)
	}
	var r struct {
		Total int `json:"total_count"`
		Items []struct {
			FullName    string `json:"full_name"`
			HTMLURL     string `json:"html_url"`
			Description string `json:"description"`
			Stars       int    `json:"stargazers_count"`
			CreatedAt   string `json:"created_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, nil, fmt.Errorf("GitHub search reply: %w", err)
	}
	repos := make([]Repo, 0, len(r.Items))
	for _, it := range r.Items {
		repos = append(repos, Repo{it.FullName, it.HTMLURL, it.Description, it.Stars, it.CreatedAt})
	}
	return r.Total, repos, nil
}

var filler = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "is": true, "are": true, "with": true, "at": true, "by": true,
	"from": true, "as": true, "its": true, "it": true, "this": true, "that": true, "how": true,
	"why": true, "what": true, "new": true, "now": true, "via": true, "using": true, "use": true,
	"launches": true, "launch": true, "announces": true, "announced": true, "release": true,
	"released": true, "releases": true, "update": true, "updates": true, "version": true,
	"show": true, "hn": true, "ask": true, "says": true, "report": true, "reports": true,
	"published": true, "publishes": true, "unveils": true, "introduces": true,
}

// Keywords picks up to three distinctive words from a title, in title order.
// Capitalised words and acronyms rank highest, then longer words. Pure numbers
// and version strings are dropped because they match nothing useful.
func Keywords(title string) []string {
	type kw struct {
		word  string
		score int
		pos   int
	}
	var cands []kw
	seen := map[string]bool{}
	for i, f := range strings.FieldsFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		low := strings.ToLower(f)
		if len(low) < 2 || filler[low] || seen[low] || isNumeric(low) || isVersion(low) {
			continue
		}
		seen[low] = true
		s := len(low)
		if hasUpper(f[1:]) || unicode.IsUpper([]rune(f)[0]) {
			s += 10
		}
		cands = append(cands, kw{low, s, i})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > 3 {
		cands = cands[:3]
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].pos < cands[j].pos })
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.word
	}
	return out
}

func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func isVersion(s string) bool {
	return len(s) > 1 && s[0] == 'v' && isNumeric(s[1:])
}

func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
