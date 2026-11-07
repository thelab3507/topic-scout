package candidate

import (
	"strings"
	"testing"
	"time"
)

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"http://Example.com/a/b/?utm_source=x&b=2&a=1#frag": "https://example.com/a/b?a=1&b=2",
		"https://example.com/":                              "https://example.com/",
		"https://example.com/x?fbclid=abc":                  "https://example.com/x",
	}
	for in, want := range cases {
		if got := CanonicalURL(in); got != want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameLinkDifferentTrackingGetsSameID(t *testing.T) {
	a := New("s", "tech", "T", "https://example.com/p?utm_campaign=1", "", "", time.Now())
	b := New("s", "tech", "T", "http://EXAMPLE.com/p/#top", "", "", time.Now())
	if a.ID != b.ID {
		t.Fatalf("IDs differ: %s vs %s", a.ID, b.ID)
	}
}

func TestStripHTMLCollapsesMarkupAndEntities(t *testing.T) {
	got := StripHTML(`<p>Hello&nbsp;<b>world</b></p><script>alert(1)</script>
	<style>p{}</style> a &amp; b`)
	if got != "Hello world a & b" {
		t.Fatalf("got %q", got)
	}
}

func TestNewSplitsSummaryAndSnippet(t *testing.T) {
	long := strings.Repeat("word ", 400) // 2000 chars
	c := New("s", "tech", " A\n  title ", "https://e.com/x", long, "", time.Now())
	if c.Title != "A title" {
		t.Errorf("title %q", c.Title)
	}
	if n := len([]rune(c.Summary)); n > 300 || n < 250 {
		t.Errorf("summary len %d", n)
	}
	if n := len([]rune(c.Snippet)); n == 0 || n > 1200 {
		t.Errorf("snippet len %d", n)
	}
}

func TestNewPrefersLongerOfDescriptionAndContent(t *testing.T) {
	c := New("s", "tech", "t", "https://e.com/x", "short", "<p>much longer content body</p>", time.Now())
	if c.Summary != "much longer content body" {
		t.Fatalf("summary %q", c.Summary)
	}
}
