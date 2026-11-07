package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/config"
)

type hdxFetcher struct {
	src  config.Source
	hc   *http.Client
	base string
}

// Fetch lists the newest datasets on the Humanitarian Data Exchange. A dataset
// being listed is itself evidence that open data exists for the topic.
func (f *hdxFetcher) Fetch(ctx context.Context, _ time.Time) ([]candidate.Candidate, error) {
	q := url.Values{"rows": {fmt.Sprint(f.src.MaxItems)}, "sort": {"metadata_created desc"}}
	body, err := get(ctx, f.hc, f.base+"/api/3/action/package_search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Results []struct {
				Name            string `json:"name"`
				Title           string `json:"title"`
				Notes           string `json:"notes"`
				MetadataCreated string `json:"metadata_created"`
				Organization    struct {
					Title string `json:"title"`
				} `json:"organization"`
			} `json:"results"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("package_search: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("package_search reported failure")
	}
	var out []candidate.Candidate
	for _, d := range resp.Result.Results {
		desc := d.Notes
		if d.Organization.Title != "" {
			desc = "Published by " + d.Organization.Title + " on HDX. " + desc
		}
		out = append(out, candidate.New(f.src.Name, f.src.Category, d.Title,
			f.base+"/dataset/"+d.Name, desc, "", parseTime(d.MetadataCreated)))
	}
	return out, nil
}
