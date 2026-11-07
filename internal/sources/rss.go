package sources

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/config"
)

type rssFetcher struct {
	src config.Source
	hc  *http.Client
}

type rssFeed struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
		PubDate     string `xml:"pubDate"`
		DCDate      string `xml:"http://purl.org/dc/elements/1.1/ date"`
	} `xml:"channel>item"`
}

type atomFeed struct {
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
	} `xml:"entry"`
}

// Fetch parses an RSS 2.0 or Atom feed. Items without a parseable date are
// skipped because recency cannot be judged.
func (f *rssFetcher) Fetch(ctx context.Context, _ time.Time) ([]candidate.Candidate, error) {
	body, err := get(ctx, f.hc, f.src.URL)
	if err != nil {
		return nil, err
	}
	root, err := rootElement(body)
	if err != nil {
		return nil, fmt.Errorf("not a feed: %w", err)
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	var out []candidate.Candidate
	switch root {
	case "rss":
		var feed rssFeed
		if err := dec.Decode(&feed); err != nil {
			return nil, err
		}
		for _, it := range feed.Items {
			t := parseTime(first(it.PubDate, it.DCDate))
			out = append(out, candidate.New(f.src.Name, f.src.Category, f.src.TitlePrefix+it.Title, it.Link, it.Description, it.Content, t))
		}
	case "feed":
		var feed atomFeed
		if err := dec.Decode(&feed); err != nil {
			return nil, err
		}
		for _, e := range feed.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			t := parseTime(first(e.Published, e.Updated))
			out = append(out, candidate.New(f.src.Name, f.src.Category, f.src.TitlePrefix+e.Title, link, e.Summary, e.Content, t))
		}
	default:
		return nil, fmt.Errorf("unsupported feed format <%s>", root)
	}
	return out, nil
}

func rootElement(b []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", errors.New("no root element")
			}
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

var timeLayouts = []string{
	time.RFC3339, time.RFC3339Nano, time.RFC1123Z, time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700", "2006-01-02T15:04:05", "2006-01-02T15:04:05.999999", "2006-01-02",
}

// parseTime returns the zero time when no known layout matches.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
