package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/config"
)

type hnFetcher struct {
	src  config.Source
	hc   *http.Client
	base string
}

type hnItem struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Text        string `json:"text"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
	Time        int64  `json:"time"`
	Dead        bool   `json:"dead"`
	Deleted     bool   `json:"deleted"`
}

// Fetch reads the best-stories list and then each of the first MaxItems items.
// Points and comments go into the description because they hint at community.
func (f *hnFetcher) Fetch(ctx context.Context, _ time.Time) ([]candidate.Candidate, error) {
	body, err := get(ctx, f.hc, f.base+"/beststories.json")
	if err != nil {
		return nil, err
	}
	var ids []int
	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, fmt.Errorf("beststories: %w", err)
	}
	if len(ids) > f.src.MaxItems {
		ids = ids[:f.src.MaxItems]
	}
	var out []candidate.Candidate
	for _, id := range ids {
		raw, err := get(ctx, f.hc, fmt.Sprintf("%s/item/%d.json", f.base, id))
		if err != nil {
			return out, fmt.Errorf("item %d: %w", id, err)
		}
		var it hnItem
		if err := json.Unmarshal(raw, &it); err != nil || it.Dead || it.Deleted || it.Title == "" {
			continue
		}
		link := it.URL
		if link == "" {
			link = fmt.Sprintf("https://news.ycombinator.com/item?id=%d", it.ID)
		}
		desc := fmt.Sprintf("%d points and %d comments on Hacker News. %s", it.Score, it.Descendants, candidate.StripHTML(it.Text))
		out = append(out, candidate.New(f.src.Name, f.src.Category, it.Title, link, desc, "", time.Unix(it.Time, 0)))
	}
	return out, nil
}
