// Package candidate defines the unit of work that flows through the scout:
// one news item or release that might become a Lab build.
package candidate

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	summaryRunes = 300
	snippetRunes = 1200
)

// Candidate is one item to score. Summary and Snippet are plain text cut from
// the feed's own fields; the scout never fetches article pages.
type Candidate struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Category  string    `json:"category"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Summary   string    `json:"summary"`
	Snippet   string    `json:"snippet"`
	Published time.Time `json:"published"`
}

// New builds a candidate from raw feed fields. body is the longest text the
// feed offers (description or full content); the first ~300 runes become the
// summary and the next ~1200 the snippet.
func New(source, category, title, rawURL, description, content string, published time.Time) Candidate {
	text := StripHTML(content)
	if d := StripHTML(description); utf8.RuneCountInString(d) > utf8.RuneCountInString(text) {
		text = d
	}
	summary, rest := splitAt(text, summaryRunes)
	canonical := CanonicalURL(rawURL)
	return Candidate{
		ID:        ID(canonical),
		Source:    source,
		Category:  category,
		Title:     strings.Join(strings.Fields(html.UnescapeString(title)), " "),
		URL:       canonical,
		Summary:   summary,
		Snippet:   truncate(rest, snippetRunes),
		Published: published.UTC(),
	}
}

// ID is a stable short identifier derived from the canonical URL.
func ID(canonicalURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL))
	return hex.EncodeToString(sum[:])[:12]
}

var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "ref": true, "ref_src": true,
	"mc_cid": true, "mc_eid": true, "igshid": true,
}

// CanonicalURL normalises a link so the same page dedupes regardless of
// tracking parameters, fragments, scheme or a trailing slash.
func CanonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme = "https"
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.User = nil
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String()
}

var (
	blockTags = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	anyTag    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// StripHTML converts feed HTML to plain text with collapsed whitespace.
func StripHTML(s string) string {
	s = blockTags.ReplaceAllString(s, " ")
	s = anyTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// splitAt cuts text after n runes, preferring a word boundary in the last 40 runes.
func splitAt(text string, n int) (head, tail string) {
	r := []rune(text)
	if len(r) <= n {
		return text, ""
	}
	cut := n
	for i := n; i > n-40 && i > 0; i-- {
		if r[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimSpace(string(r[:cut])), strings.TrimSpace(string(r[cut:]))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
