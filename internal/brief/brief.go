// Package brief writes a deep-dive brief for one item: a Markdown document to
// hand to a stronger assistant. The local model only reads a snippet from a
// feed; the brief packs what it found, the items around it and the builder's
// profile, and tells the stronger assistant what to go and check.
package brief

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

// MaxRelated is how many related items a brief lists.
const MaxRelated = 8

// Input is everything a brief is made from.
type Input struct {
	Record   store.RecordLine   // the item itself
	All      []store.RecordLine // every stored record, to find related items
	Clusters cluster.File       // the latest pattern pass, if any
	Profile  string             // the builder profile, already capped
}

// Related is an item that shares at least one tag with the brief's item.
type Related struct {
	Record store.RecordLine
	Shared []string // the shared tags, alphabetical
}

// FindRelated returns up to max scored items that share a tag with target, most
// shared tags first, then newest published, then by id so the order is stable.
// The item itself is never included.
func FindRelated(target store.RecordLine, all []store.RecordLine, max int) []Related {
	mine := tagSet(target)
	if len(mine) == 0 {
		return nil
	}
	var out []Related
	for _, r := range all {
		if r.Candidate.ID == target.Candidate.ID || r.Result.Scores == nil {
			continue
		}
		var shared []string
		for tag := range tagSet(r) {
			if mine[tag] {
				shared = append(shared, tag)
			}
		}
		if len(shared) == 0 {
			continue
		}
		sort.Strings(shared)
		out = append(out, Related{Record: r, Shared: shared})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if len(a.Shared) != len(b.Shared) {
			return len(a.Shared) > len(b.Shared)
		}
		if !a.Record.Candidate.Published.Equal(b.Record.Candidate.Published) {
			return a.Record.Candidate.Published.After(b.Record.Candidate.Published)
		}
		return a.Record.Candidate.ID < b.Record.Candidate.ID
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func tagSet(r store.RecordLine) map[string]bool {
	set := map[string]bool{}
	if r.Result.Scores == nil {
		return set
	}
	for _, t := range r.Result.Scores.Tags {
		if n := score.NormalizeTag(t); n != "" {
			set[n] = true
		}
	}
	return set
}

const instructions = `You are a stronger assistant helping one person decide whether a news item is worth turning into a small public project. The project is a tool, an app, a library, an integration or an upstream contribution, plus a writeup, finished in 2 weeks, 4 at the very most. A small local model screened the item from a short feed snippet, so treat its notes below as a starting point, not as fact.

Do this, in order:

1. Read the item below and the related items.
2. Search the last 4 weeks for the same topic: releases, repositories, discussions and competing announcements.
3. Say what is really new, in simple English.
4. Name the open question: what is still unanswered.
5. Give 3-5 "What if...?" builds that fit the builder profile at the end. For each, say what it is, why it fits, and the smallest version that ships in 2 weeks.
6. Be critical. Say what already exists, what the risks are, and whether it really fits in 2 weeks. If the item is not worth building from, say so plainly.`

var scoreNames = [5]string{"First or different", "Project hook", "Fits the timebox", "Ships publicly", "Reaches the right people"}

// Build renders the brief as Markdown.
func Build(in Input) string {
	var b strings.Builder
	c, r := in.Record.Candidate, in.Record.Result

	fmt.Fprintf(&b, "# Deep dive: %s\n\n## What to do\n\n%s\n\n", score.OneLine(c.Title), instructions)

	b.WriteString("## The item\n\n")
	fmt.Fprintf(&b, "- Title: %s\n- Link: %s\n- Source: %s (%s)\n- Published: %s\n- Item id: %s\n",
		score.OneLine(c.Title), c.URL, c.Source, c.Category, day(c.Published), c.ID)

	s := r.Scores
	if s != nil && s.Gist != "" {
		fmt.Fprintf(&b, "- In short: %s\n", score.OneLine(s.Gist))
	} else {
		b.WriteString("- In short: (not recorded; scored before the gist existed)\n")
	}
	if s != nil {
		vals := [5]int{s.FirstOrDifferent, s.TestableQuestion, s.FitsTimebox, s.ShipsPublicly, s.HasCommunity}
		var parts []string
		for i, v := range vals {
			parts = append(parts, fmt.Sprintf("%s %d", scoreNames[i], v))
		}
		fmt.Fprintf(&b, "- Local model's scores (1-5): %s; total %d/25; it said pass=%v\n", strings.Join(parts, ", "), s.Total(), s.Pass)
		fmt.Fprintf(&b, "- Local model's reason: %s\n", score.OneLine(s.Reason))
		if len(s.Tags) > 0 {
			fmt.Fprintf(&b, "- Tags: %s\n", strings.Join(s.Tags, ", "))
		} else {
			b.WriteString("- Tags: (none recorded)\n")
		}
	} else {
		fmt.Fprintf(&b, "- Local model: no verdict (status %q)\n", r.Status)
	}

	if text := strings.TrimSpace(c.Summary + " " + c.Snippet); text != "" {
		fmt.Fprintf(&b, "\nFeed text (all the local model saw):\n\n> %s\n", score.OneLine(text))
	} else {
		b.WriteString("\nFeed text: (the feed gave none)\n")
	}

	if s != nil && len(s.Ideas) > 0 {
		b.WriteString("\nThe local model's first ideas:\n\n")
		writeIdeas(&b, s.Ideas)
	}

	b.WriteString("\n## Related items\n\n")
	related := FindRelated(in.Record, in.All, MaxRelated)
	if len(related) == 0 {
		b.WriteString("None share a tag with this item.\n")
	}
	for _, rel := range related {
		rc := rel.Record.Candidate
		fmt.Fprintf(&b, "- %s (%s, %s)\n  Link: %s\n  Shared tags: %s\n", score.OneLine(rc.Title), rc.Source, day(rc.Published), rc.URL, strings.Join(rel.Shared, ", "))
		if g := score.OneLine(rel.Record.Result.Scores.Gist); g != "" {
			fmt.Fprintf(&b, "  In short: %s\n", g)
		}
	}

	b.WriteString("\n## Pattern it belongs to\n\n")
	patterns := in.Clusters.Containing(c.ID)
	if len(patterns) == 0 {
		b.WriteString("None yet. It is not part of the latest weekly pattern pass.\n")
	}
	for _, p := range patterns {
		fmt.Fprintf(&b, "### %s\n\n- Tag: %s (%d items from %d sources)\n- What is forming: %s\n- Open question: %s\n\n", score.OneLine(p.Name), p.Tag, p.Count, p.Sources, score.OneLine(p.Forming), score.OneLine(p.OpenQuestion))
		writeIdeas(&b, p.Ideas)
		b.WriteString("\n")
	}

	b.WriteString("\n## About the builder\n\n")
	if p := strings.TrimSpace(in.Profile); p != "" {
		b.WriteString(p + "\n")
	} else {
		b.WriteString("(no profile.md in the data directory)\n")
	}
	return b.String()
}

func writeIdeas(b *strings.Builder, ideas []score.Idea) {
	for _, i := range ideas {
		fmt.Fprintf(b, "- [%s] %s %s\n", i.Kind, score.OneLine(i.Title), score.OneLine(i.Detail))
	}
}

func day(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}

// Write saves the brief to briefs/<id>.md in the data directory, replacing any
// earlier one, and returns the path.
func Write(st *store.Store, id, text string) (string, error) {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("not a usable item id: %q", id)
	}
	dir := filepath.Join(st.Dir, "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".md")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}
