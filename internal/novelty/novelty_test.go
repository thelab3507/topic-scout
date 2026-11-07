package novelty

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeywords(t *testing.T) {
	cases := map[string][]string{
		"Ollama v0.5.2 released with new vision support": {"ollama", "vision", "support"},
		"Show HN: A tiny SQLite viewer":                  {"sqlite", "viewer", "tiny"},
		"The 10 best things":                             {"best", "things"},
		"UNOSAT flood extent map for Pakistan published": {"unosat", "pakistan", "extent"},
		"!!! ???": nil,
	}
	for in, want := range cases {
		got := Keywords(in)
		// compare as sets: ranking decides membership, title order decides position
		if len(got) != len(want) {
			t.Errorf("Keywords(%q) = %v, want %v", in, got, want)
			continue
		}
		set := map[string]bool{}
		for _, w := range want {
			set[w] = true
		}
		for _, g := range got {
			if !set[g] {
				t.Errorf("Keywords(%q) = %v, want %v", in, got, want)
			}
		}
	}
}

func TestKeywordsAreInTitleOrder(t *testing.T) {
	got := Keywords("Cloudflare Workers adds Python support")
	if !reflect.DeepEqual(got, []string{"cloudflare", "workers", "python"}) {
		t.Fatalf("got %v", got)
	}
}

func newChecker(t *testing.T, h http.HandlerFunc) (*Checker, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New("tok")
	c.BaseURL = srv.URL
	c.Gap = 0
	c.Now = func() time.Time { return time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) }
	return c, &calls
}

func TestCheckRunsRecentAndAllTimeQueries(t *testing.T) {
	var queries []string
	c, calls := newChecker(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("q"))
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("sort") != "stars" {
			t.Errorf("bad request: %v %v", r.Header, r.URL)
		}
		io.WriteString(w, `{"total_count":7,"items":[{"full_name":"a/b","html_url":"https://github.com/a/b","description":"d","stargazers_count":12,"created_at":"2026-11-01T00:00:00Z"}]}`)
	})
	ev := c.Check(context.Background(), "Ollama adds vision support")
	if *calls != 2 || ev.Skipped != "" || ev.RecentTotal != 7 || ev.AllTimeTotal != 7 || len(ev.Top) != 1 || ev.Top[0].Stars != 12 {
		t.Fatalf("calls=%d ev=%+v", *calls, ev)
	}
	if !strings.HasSuffix(queries[0], "created:>=2026-10-08") || strings.Contains(queries[1], "created:") {
		t.Fatalf("queries: %v", queries)
	}
}

func TestRateLimitSkipsThisAndLaterChecksWithoutCalling(t *testing.T) {
	c, calls := newChecker(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	first := c.Check(context.Background(), "Ollama adds vision support")
	second := c.Check(context.Background(), "Another distinctive title here")
	if !strings.Contains(first.Skipped, "rate limited") || !strings.Contains(second.Skipped, "rate limit") || *calls != 1 {
		t.Fatalf("calls=%d first=%+v second=%+v", *calls, first, second)
	}
}

func TestNoKeywordsIsSkippedWithoutCalling(t *testing.T) {
	c, calls := newChecker(t, func(w http.ResponseWriter, r *http.Request) {})
	ev := c.Check(context.Background(), "the new ... 2026")
	if ev.Skipped == "" || *calls != 0 {
		t.Fatalf("calls=%d ev=%+v", *calls, ev)
	}
}

func TestGapIsAppliedBetweenRequests(t *testing.T) {
	c, _ := newChecker(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"total_count":0,"items":[]}`) })
	c.Gap = 5 * time.Second
	slept := 0
	c.Sleep = func(context.Context, time.Duration) error { slept++; return nil }
	c.Check(context.Background(), "Ollama adds vision support")
	if slept != 1 {
		t.Fatalf("expected one pause between two requests, got %d", slept)
	}
}

func TestServerErrorIsSkippedNotFatal(t *testing.T) {
	c, _ := newChecker(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	ev := c.Check(context.Background(), "Ollama adds vision support")
	if !strings.Contains(ev.Skipped, "HTTP 500") {
		t.Fatalf("ev=%+v", ev)
	}
}
