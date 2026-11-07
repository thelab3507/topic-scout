// Package sources fetches candidates from the official feeds and APIs listed
// in sources.json. A failing source is reported and skipped; it never aborts
// the run.
package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/config"
)

const (
	userAgent    = "topic-scout/0.1 (+https://github.com/thelab3507/topic-scout)"
	maxBodyBytes = 8 << 20
)

// Fetcher returns raw candidates for one source. Window, exclusion and
// per-source caps are applied afterwards by FetchAll.
type Fetcher interface {
	Fetch(ctx context.Context, since time.Time) ([]candidate.Candidate, error)
}

// New builds the fetcher for a configured source.
func New(s config.Source, hc *http.Client) (Fetcher, error) {
	switch s.Type {
	case config.TypeRSS:
		return &rssFetcher{src: s, hc: hc}, nil
	case config.TypeHN:
		return &hnFetcher{src: s, hc: hc, base: "https://hacker-news.firebaseio.com/v0"}, nil
	case config.TypeHDX:
		return &hdxFetcher{src: s, hc: hc, base: "https://data.humdata.org"}, nil
	}
	return nil, fmt.Errorf("unknown source type %q", s.Type)
}

// Result is everything FetchAll collected plus a warning per failed source.
type Result struct {
	Candidates []candidate.Candidate
	Warnings   []string
}

// FetchAll runs every configured source independently and applies the lookback
// window, title exclusions and per-source cap (newest first).
func FetchAll(ctx context.Context, cfg *config.Config, hc *http.Client, now time.Time) Result {
	since := now.AddDate(0, 0, -cfg.LookbackDays)
	var out Result
	for _, s := range cfg.Sources {
		f, err := New(s, hc)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: skipped: %v", s.Name, err))
			continue
		}
		items, err := f.Fetch(ctx, since)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", s.Name, err))
			continue
		}
		out.Candidates = append(out.Candidates, limit(s, items, since)...)
	}
	return out
}

func limit(s config.Source, items []candidate.Candidate, since time.Time) []candidate.Candidate {
	kept := items[:0:0]
	for _, c := range items {
		if c.Title == "" || c.URL == "" || c.Published.IsZero() || c.Published.Before(since) || s.Excluded(c.Title) {
			continue
		}
		kept = append(kept, c)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Published.After(kept[j].Published) })
	if len(kept) > s.MaxItems {
		kept = kept[:s.MaxItems]
	}
	return kept
}

// maxRateLimitWait caps how long one 429 may stall a source. Reddit's
// unauthenticated limit resets in under a minute.
const maxRateLimitWait = 65 * time.Second

// sleep is a variable so tests do not really wait.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// get fetches a URL. On HTTP 429 it waits as long as the server asks (capped)
// and retries once; some feeds, notably Reddit's, allow about one request per
// minute without credentials.
func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	body, wait, err := getOnce(ctx, hc, url)
	if wait == 0 {
		return body, err
	}
	if err := sleep(ctx, wait); err != nil {
		return nil, err
	}
	body, _, err = getOnce(ctx, hc, url)
	return body, err
}

// getOnce returns a non-zero wait only when the server answered 429.
func getOnce(ctx context.Context, hc *http.Client, url string) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, retryDelay(resp.Header), fmt.Errorf("HTTP 429 from %s", req.URL.Host)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	return body, 0, err
}

// retryDelay reads Retry-After or Reddit's X-Ratelimit-Reset (whole seconds),
// defaulting to 30 s, and adds a small margin.
func retryDelay(h http.Header) time.Duration {
	d := 30 * time.Second
	for _, k := range []string{"Retry-After", "X-Ratelimit-Reset"} {
		if secs, err := strconv.Atoi(strings.TrimSpace(h.Get(k))); err == nil && secs > 0 {
			d = time.Duration(secs) * time.Second
			break
		}
	}
	d += 2 * time.Second
	if d > maxRateLimitWait {
		d = maxRateLimitWait
	}
	return d
}
