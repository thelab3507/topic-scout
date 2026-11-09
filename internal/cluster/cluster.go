// Package cluster finds the topics that keep coming up and asks the local model
// what is forming around each one.
//
// A pattern is a normalised tag that appears on at least MinItems scored items
// from at least MinSources distinct sources in the last WindowDays days. The
// biggest MaxClusters are kept. Each gets one model call, through the score
// package's Ask, so it follows the same transport and retry rules as scoring:
// sequential, strict json_schema, reasoning off, one retry.
//
// Nothing here may fail a run. A pattern whose call fails is skipped with a
// warning; the caller decides what to do about a dead server.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

const (
	// FileName is where the latest pass is kept in the data directory.
	FileName = "clusters.json"

	WindowDays  = 14 // how far back scored items count
	MinItems    = 3  // items a tag needs to be a pattern
	MinSources  = 2  // distinct sources a tag needs to be a pattern
	MaxClusters = 3  // patterns kept per pass

	// MaxItemsSent is how many items one model call sees.
	MaxItemsSent = 10
	// MaxTokens is the reply budget of a pattern call.
	MaxTokens = 600

	// RefreshAfter is how old the last pass must be before a run starts another.
	RefreshAfter = 6 * 24 * time.Hour

	// NoneNote is shown in the digest when a pass found no pattern.
	NoneNote = "No pattern yet: no topic tag has 3 or more items from 2 or more sources in the last 14 days."
)

// Item is one scored item that belongs to a pattern.
type Item struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Source    string    `json:"source"`
	Published time.Time `json:"published"`
}

// Cluster is one pattern with the model's reading of it.
type Cluster struct {
	Tag          string       `json:"tag"`
	Name         string       `json:"name"`
	Forming      string       `json:"forming"`
	OpenQuestion string       `json:"open_question"`
	Ideas        []score.Idea `json:"ideas"`
	Count        int          `json:"count"`   // items under the tag, however many the model saw
	Sources      int          `json:"sources"` // distinct sources among them
	Items        []Item       `json:"items"`
}

// File is clusters.json.
type File struct {
	GeneratedAt time.Time `json:"generated_at"`
	Clusters    []Cluster `json:"clusters"`
	Note        string    `json:"note,omitempty"`
}

// Load reads clusters.json. A missing file gives the zero File.
func Load(st *store.Store) (File, error) {
	var f File
	return f, st.LoadJSON(FileName, &f)
}

// Save replaces clusters.json.
func Save(st *store.Store, f File) error { return st.SaveJSON(FileName, f) }

// Due reports whether a new pass should run: there has never been one, or the
// last is RefreshAfter old or older.
func Due(last File, now time.Time) bool {
	return last.GeneratedAt.IsZero() || now.Sub(last.GeneratedAt) >= RefreshAfter
}

// Group is the set of scored items that share a tag.
type Group struct {
	Tag     string
	Records []store.RecordLine // newest published first
	Sources int
}

// Find groups the scored records of the last WindowDays days by normalised tag
// and returns the biggest MaxClusters tags that clear both thresholds. An item
// counts once per tag however often the tag is repeated, and may appear under
// several tags. Ties go to more sources, then the tag's alphabetical order, so
// the result is stable.
func Find(recs []store.RecordLine, now time.Time) []Group {
	since := now.AddDate(0, 0, -WindowDays)
	byTag := map[string][]store.RecordLine{}
	for _, r := range recs {
		if r.Result.Status != score.StatusScored || r.Result.Scores == nil || r.RunAt.Before(since) {
			continue
		}
		seen := map[string]bool{}
		for _, raw := range r.Result.Scores.Tags {
			tag := score.NormalizeTag(raw)
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			byTag[tag] = append(byTag[tag], r)
		}
	}

	var groups []Group
	for tag, rs := range byTag {
		sources := map[string]bool{}
		for _, r := range rs {
			sources[r.Candidate.Source] = true
		}
		if len(rs) < MinItems || len(sources) < MinSources {
			continue
		}
		sort.SliceStable(rs, func(i, j int) bool {
			a, b := rs[i].Candidate, rs[j].Candidate
			if !a.Published.Equal(b.Published) {
				return a.Published.After(b.Published)
			}
			return a.ID < b.ID
		})
		groups = append(groups, Group{Tag: tag, Records: rs, Sources: len(sources)})
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if len(a.Records) != len(b.Records) {
			return len(a.Records) > len(b.Records)
		}
		if a.Sources != b.Sources {
			return a.Sources > b.Sources
		}
		return a.Tag < b.Tag
	})
	if len(groups) > MaxClusters {
		groups = groups[:MaxClusters]
	}
	return groups
}

// schemaJSON is the strict json_schema of a pattern reply.
const schemaJSON = `{
  "type": "json_schema",
  "json_schema": {
    "name": "topic_pattern",
    "strict": true,
    "schema": {
      "type": "object",
      "properties": {
        "name": {"type": "string"},
        "forming": {"type": "string"},
        "open_question": {"type": "string"},
        "ideas": ` + score.IdeasSchema + `
      },
      "required": ["name","forming","open_question","ideas"],
      "additionalProperties": false
    }
  }
}`

const systemRules = `You look for patterns across recent news items for "The Lab", a series where one person ships a small public project (a tool, an app, a library, an integration or an upstream contribution, plus a writeup) within 2 weeks, 4 at the very most.

You will receive one topic tag and up to 10 items that share it, each with its source, title and gist. Judge only from that text. Do not assume facts it does not state. Reply only with the JSON object the schema requires. Write in simple English.

- name: a short plain name for the pattern, 2-6 words.
- forming: 1-2 sentences on what seems to be forming across the items.
- open_question: 1 sentence on what nobody seems to have answered yet.
- ideas: 2-3 project ideas the pattern sparks. kind is Build (a new tool, app, library or integration), Experiment (a measurement or test) or Contribute (a pull request, plugin or fix to an existing project). title is a "What if...?" question. detail is one sentence on what to make. At least two ideas must be Build or Contribute. Each idea must fit the builder profile when one is given. Avoid generic ideas such as "build a dashboard".`

type reply struct {
	Name         string       `json:"name"`
	Forming      string       `json:"forming"`
	OpenQuestion string       `json:"open_question"`
	Ideas        []score.Idea `json:"ideas"`
}

var replyKeys = []string{"name", "forming", "open_question", "ideas"}

// parseReply demands exactly the four keys, non-empty text and 2-3 valid ideas.
func parseReply(output string) (reply, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &fields); err != nil {
		return reply{}, fmt.Errorf("not a JSON object: %w", err)
	}
	if len(fields) != len(replyKeys) {
		return reply{}, fmt.Errorf("expected exactly %d keys, got %d", len(replyKeys), len(fields))
	}
	for _, k := range replyKeys {
		if _, ok := fields[k]; !ok {
			return reply{}, fmt.Errorf("missing key %q", k)
		}
	}
	dec := json.NewDecoder(strings.NewReader(output))
	dec.DisallowUnknownFields()
	var r reply
	if err := dec.Decode(&r); err != nil {
		return reply{}, err
	}
	r.Name, r.Forming, r.OpenQuestion = strings.TrimSpace(r.Name), strings.TrimSpace(r.Forming), strings.TrimSpace(r.OpenQuestion)
	switch {
	case r.Name == "":
		return reply{}, errors.New("name is empty")
	case r.Forming == "":
		return reply{}, errors.New("forming is empty")
	case r.OpenQuestion == "":
		return reply{}, errors.New("open_question is empty")
	}
	if err := score.ValidateIdeas(r.Ideas); err != nil {
		return reply{}, err
	}
	for i := range r.Ideas {
		r.Ideas[i].Title = strings.TrimSpace(r.Ideas[i].Title)
		r.Ideas[i].Detail = strings.TrimSpace(r.Ideas[i].Detail)
	}
	return r, nil
}

// prompt builds the system and user text for one group. It sends at most
// MaxItemsSent items, newest first, and drops the oldest until the prompt fits
// the scorer's budget.
func prompt(g Group, profile string, budget int) (system, user string) {
	system = score.WithProfile(systemRules, profile)
	n := min(len(g.Records), MaxItemsSent)
	for {
		var b strings.Builder
		fmt.Fprintf(&b, "Tag: %s\nItems (%d shown of %d):\n", g.Tag, n, len(g.Records))
		for i, r := range g.Records[:n] {
			c := r.Candidate
			gist := ""
			if r.Result.Scores != nil {
				gist = r.Result.Scores.Gist
			}
			fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1,
				score.TruncateRunes(score.OneLine(c.Source), 40),
				score.TruncateRunes(score.OneLine(c.Title), 150),
				score.TruncateRunes(score.OneLine(gist), 300))
		}
		user = b.String()
		if n <= 1 || score.EstimateTokens(system)+score.EstimateTokens(user) <= budget {
			return system, user
		}
		n--
	}
}

// Generate asks the model about each group, one call at a time, and returns the
// File to save plus how many groups failed. A group whose call fails twice is
// skipped with a warning through logf. A non-nil error means the pass had to
// stop (server down or draining, context cancelled); the File then holds what
// was finished so far.
func Generate(ctx context.Context, sc *score.Scorer, groups []Group, now time.Time, profile string, logf func(string, ...any)) (File, int, error) {
	f := File{GeneratedAt: now}
	if len(groups) == 0 {
		f.Note = NoneNote
		return f, 0, nil
	}
	failed := 0
	for _, g := range groups {
		system, user := prompt(g, profile, sc.MaxPrompt)
		var parsed reply
		_, err := sc.Ask(ctx, system, user, schemaJSON, MaxTokens, func(out string) error {
			var perr error
			parsed, perr = parseReply(out)
			return perr
		})
		if err != nil {
			var fe *score.FatalError
			if errors.As(err, &fe) || ctx.Err() != nil {
				f.Note = noteForFailures(failed+1, len(groups))
				return f, failed + 1, err
			}
			failed++
			logf("warning: pattern %q skipped: %v", g.Tag, err)
			continue
		}
		if score.BuildOrContribute(parsed.Ideas) < 2 {
			logf("warning: pattern %q has fewer than two Build or Contribute ideas", g.Tag)
		}
		f.Clusters = append(f.Clusters, Cluster{
			Tag: g.Tag, Name: parsed.Name, Forming: parsed.Forming, OpenQuestion: parsed.OpenQuestion, Ideas: parsed.Ideas,
			Count: len(g.Records), Sources: g.Sources, Items: itemsOf(g),
		})
	}
	if failed > 0 {
		f.Note = noteForFailures(failed, len(groups))
	}
	return f, failed, nil
}

func noteForFailures(failed, total int) string {
	return fmt.Sprintf("%d of %d patterns could not be written this time; see scout.log.", failed, total)
}

func itemsOf(g Group) []Item {
	items := make([]Item, 0, len(g.Records))
	for _, r := range g.Records {
		c := r.Candidate
		items = append(items, Item{ID: c.ID, Title: c.Title, URL: c.URL, Source: c.Source, Published: c.Published})
	}
	return items
}

// Containing returns the clusters that list the item, so a brief can say which
// pattern it belongs to.
func (f File) Containing(id string) []Cluster {
	var out []Cluster
	for _, c := range f.Clusters {
		for _, it := range c.Items {
			if it.ID == id {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
