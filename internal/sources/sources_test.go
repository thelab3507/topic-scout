package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/config"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const rssFixture = `<?xml version="1.0"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>t</title>
<item><title>Fresh item</title><link>https://e.com/fresh?utm_source=x</link>
<description>&lt;p&gt;short&lt;/p&gt;</description>
<content:encoded><![CDATA[<p>The full body of the fresh item.</p>]]></content:encoded>
<pubDate>Thu, 05 Nov 2026 10:00:00 +0000</pubDate></item>
<item><title>Old item</title><link>https://e.com/old</link><pubDate>Mon, 01 Jun 2026 10:00:00 +0000</pubDate></item>
<item><title>v2.0 beta</title><link>https://e.com/beta</link><pubDate>Fri, 06 Nov 2026 10:00:00 +0000</pubDate></item>
<item><title>Undated</title><link>https://e.com/undated</link></item>
</channel></rss>`

const atomFixture = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>Release v1.2</title>
<link rel="alternate" type="text/html" href="https://github.com/o/r/releases/tag/v1.2"/>
<updated>2026-11-04T09:00:00Z</updated>
<content type="html">&lt;p&gt;Changelog &amp;amp; notes&lt;/p&gt;</content></entry>
</feed>`

func serve(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(srv.Close)
	return srv
}

func cfgFor(url, exclude string) *config.Config {
	return &config.Config{LookbackDays: 14, MaxPerRun: 100, Sources: []config.Source{
		{Name: "feed", Type: config.TypeRSS, URL: url, Category: "tech", MaxItems: 10, ExcludeTitle: exclude},
	}}
}

// loadCfg round-trips through config.Load so exclude regexes compile exactly as in production.
func loadCfg(t *testing.T, c *config.Config) *config.Config {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "sources.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestRSSWindowExclusionAndUndated(t *testing.T) {
	srv := serve(t, rssFixture)
	cfg := loadCfg(t, cfgFor(srv.URL, "(?i)beta"))
	res := FetchAll(context.Background(), cfg, srv.Client(), now)
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Title != "Fresh item" {
		t.Fatalf("got %+v", res.Candidates)
	}
	c := res.Candidates[0]
	if c.URL != "https://e.com/fresh" {
		t.Errorf("url not canonical: %s", c.URL)
	}
	if !strings.Contains(c.Summary, "full body") {
		t.Errorf("summary should come from content:encoded, got %q", c.Summary)
	}
}

func TestAtomFeed(t *testing.T) {
	srv := serve(t, atomFixture)
	res := FetchAll(context.Background(), loadCfg(t, cfgFor(srv.URL, "")), srv.Client(), now)
	if len(res.Candidates) != 1 {
		t.Fatalf("got %+v warnings=%v", res.Candidates, res.Warnings)
	}
	c := res.Candidates[0]
	if c.URL != "https://github.com/o/r/releases/tag/v1.2" || c.Summary != "Changelog & notes" {
		t.Errorf("unexpected candidate %+v", c)
	}
}

func TestTitlePrefixIsApplied(t *testing.T) {
	srv := serve(t, atomFixture)
	cfg := cfgFor(srv.URL, "")
	cfg.Sources[0].TitlePrefix = "o/r release "
	res := FetchAll(context.Background(), loadCfg(t, cfg), srv.Client(), now)
	if len(res.Candidates) != 1 || res.Candidates[0].Title != "o/r release Release v1.2" {
		t.Fatalf("got %+v", res.Candidates)
	}
}

func TestFailingSourceDoesNotAbortOthers(t *testing.T) {
	good := serve(t, rssFixture)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	cfg := loadCfg(t, &config.Config{LookbackDays: 14, Sources: []config.Source{
		{Name: "bad", Type: config.TypeRSS, URL: bad.URL, Category: "tech"},
		{Name: "good", Type: config.TypeRSS, URL: good.URL, Category: "tech"},
	}})
	res := FetchAll(context.Background(), cfg, good.Client(), now)
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "bad:") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("good source produced nothing")
	}
}

func TestPerSourceCapKeepsNewest(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel>`)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&b, `<item><title>Item %d</title><link>https://e.com/%d</link><pubDate>Fri, 0%d Nov 2026 10:00:00 +0000</pubDate></item>`, i, i, i)
	}
	b.WriteString(`</channel></rss>`)
	srv := serve(t, b.String())
	cfg := cfgFor(srv.URL, "")
	cfg.Sources[0].MaxItems = 2
	res := FetchAll(context.Background(), loadCfg(t, cfg), srv.Client(), now)
	if len(res.Candidates) != 2 || res.Candidates[0].Title != "Item 5" || res.Candidates[1].Title != "Item 4" {
		t.Fatalf("got %+v", res.Candidates)
	}
}

func TestHN(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/beststories.json", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[11,12]`) })
	mux.HandleFunc("/item/11.json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":11,"type":"story","title":"Show HN: thing","url":"https://e.com/thing","score":321,"descendants":45,"time":1793000000}`)
	})
	mux.HandleFunc("/item/12.json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":12,"type":"story","title":"Ask HN: why?","text":"<p>because</p>","score":10,"descendants":2,"time":1793000100}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &hnFetcher{src: config.Source{Name: "hn", Category: "tech", MaxItems: 10}, hc: srv.Client(), base: srv.URL}
	got, err := f.Fetch(context.Background(), now)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	if !strings.Contains(got[0].Summary, "321 points and 45 comments") {
		t.Errorf("summary %q", got[0].Summary)
	}
	if got[1].URL != "https://news.ycombinator.com/item?id=12" {
		t.Errorf("fallback url %q", got[1].URL)
	}
}

func TestHDX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") != "metadata_created desc" || r.URL.Query().Get("rows") != "5" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"success":true,"result":{"results":[{"name":"flood-extent","title":"Flood extent 2026",
		"notes":"Satellite-derived extent.","metadata_created":"2026-11-05T10:00:00.123456","organization":{"title":"UNOSAT"}}]}}`)
	}))
	defer srv.Close()
	f := &hdxFetcher{src: config.Source{Name: "hdx", Category: "humanitarian", MaxItems: 5}, hc: srv.Client(), base: srv.URL}
	got, err := f.Fetch(context.Background(), now)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v err %v", got, err)
	}
	if got[0].URL != "https://"+strings.TrimPrefix(srv.URL, "http://")+"/dataset/flood-extent" || !strings.Contains(got[0].Summary, "UNOSAT") || got[0].Published.IsZero() {
		t.Errorf("unexpected %+v", got[0])
	}
}

func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	orig := sleep
	sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { sleep = orig })
	return &waits
}

func TestRateLimitedFeedWaitsAsAskedThenRetriesOnce(t *testing.T) {
	waits := noSleep(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("X-Ratelimit-Reset", "44")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, rssFixture)
	}))
	defer srv.Close()
	res := FetchAll(context.Background(), loadCfg(t, cfgFor(srv.URL, "")), srv.Client(), now)
	if len(res.Warnings) != 0 || len(res.Candidates) == 0 {
		t.Fatalf("expected success after retry: %+v", res)
	}
	if hits != 2 || len(*waits) != 1 || (*waits)[0] != 46*time.Second {
		t.Errorf("hits=%d waits=%v, want 2 hits and one 46s wait", hits, *waits)
	}
}

func TestRateLimitedTwiceBecomesAWarningAndWaitIsCapped(t *testing.T) {
	waits := noSleep(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res := FetchAll(context.Background(), loadCfg(t, cfgFor(srv.URL, "")), srv.Client(), now)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "429") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if hits != 2 || (*waits)[0] != maxRateLimitWait {
		t.Errorf("hits=%d waits=%v, want 2 hits and wait capped at %v", hits, *waits, maxRateLimitWait)
	}
}

func TestOtherHTTPErrorsAreNotRetried(t *testing.T) {
	noSleep(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	res := FetchAll(context.Background(), loadCfg(t, cfgFor(srv.URL, "")), srv.Client(), now)
	if hits != 1 || len(res.Warnings) != 1 {
		t.Errorf("hits=%d warnings=%v", hits, res.Warnings)
	}
}
