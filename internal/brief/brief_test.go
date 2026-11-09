package brief

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func rec(id string, ageDays int, tags ...string) store.RecordLine {
	return store.RecordLine{
		RunAt: now,
		Candidate: candidate.Candidate{
			ID: id, Source: "src-" + id, Category: "tech", Title: "Title " + id, URL: "https://e.com/" + id,
			Summary: "Summary " + id, Snippet: "Snippet " + id, Published: now.AddDate(0, 0, -ageDays),
		},
		Result: score.Result{ID: id, Status: score.StatusScored, Scores: &score.Scores{
			Gist: "Gist " + id, Tags: tags, Reason: "Reason " + id,
			FirstOrDifferent: 4, TestableQuestion: 5, FitsTimebox: 3, ShipsPublicly: 4, HasCommunity: 2, Pass: false,
			Ideas: []score.Idea{
				{Kind: score.KindBuild, Title: "What if " + id + " had a CLI?", Detail: "Write the CLI for " + id + "."},
				{Kind: score.KindContribute, Title: "What if " + id + " took a patch?", Detail: "Send a patch for " + id + "."},
			},
		}},
	}
}

func relatedIDs(rs []Related) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Record.Candidate.ID)
	}
	return strings.Join(out, ",")
}

func TestRelatedAreRankedBySharedTagsThenNewestAndCappedAtEight(t *testing.T) {
	target := rec("T", 1, "Local LLM", "open data", "geospatial")
	all := []store.RecordLine{
		target, // never related to itself
		rec("two-old", 9, "local llm", "open data"),
		rec("two-new", 2, "open data.", "local llm"),
		rec("one-new", 1, "geospatial"),
		rec("one-old", 8, "local llm"),
		rec("none", 0, "unrelated"),
		rec("untagged", 0),
	}
	unscored := rec("uns", 0, "local llm")
	unscored.Result = score.Result{ID: "uns", Status: score.StatusUnscored}
	all = append(all, unscored)

	got := FindRelated(target, all, MaxRelated)
	if relatedIDs(got) != "two-new,two-old,one-new,one-old" {
		t.Fatalf("related = %s", relatedIDs(got))
	}
	if strings.Join(got[0].Shared, ",") != "local llm,open data" {
		t.Fatalf("shared tags (normalised, sorted): %v", got[0].Shared)
	}

	for i := 0; i < 20; i++ {
		all = append(all, rec(fmt.Sprintf("many-%02d", i), 3+i%3, "geospatial"))
	}
	if got := FindRelated(target, all, MaxRelated); len(got) != MaxRelated || relatedIDs(got[:2]) != "two-new,two-old" {
		t.Fatalf("want %d related, most shared first: %s", MaxRelated, relatedIDs(got))
	}
	if got := FindRelated(rec("lonely", 0), all, MaxRelated); got != nil {
		t.Fatalf("an item without tags has no related items: %v", got)
	}
}

func TestBriefHasEverySection(t *testing.T) {
	target := rec("T", 1, "local llm")
	in := Input{
		Record:  target,
		All:     []store.RecordLine{target, rec("R1", 2, "local llm")},
		Profile: "Go preferred.",
		Clusters: cluster.File{GeneratedAt: now, Clusters: []cluster.Cluster{{
			Tag: "local llm", Name: "Local runtimes", Forming: "Runtimes ship quant pickers.", OpenQuestion: "Which quant wins on 8 GB?",
			Count: 4, Sources: 3, Items: []cluster.Item{{ID: "T"}},
			Ideas: []score.Idea{{Kind: score.KindBuild, Title: "What if a picker?", Detail: "Make a picker."}, {Kind: score.KindBuild, Title: "What if two?", Detail: "Make two."}},
		}}},
	}
	out := Build(in)
	for _, want := range []string{
		// instructions for the stronger assistant
		"stronger assistant", "Search the last 4 weeks", "what is really new", "open question", `3-5 "What if...?" builds`, "fit the builder profile", "What already exists", "risks", "2 weeks",
		// the item
		"# Deep dive: Title T", "https://e.com/T", "src-T (tech)", "2026-11-08", "Item id: T",
		"In short: Gist T", "First or different 4", "Project hook 5", "Fits the timebox 3", "Ships publicly 4", "Reaches the right people 2", "total 18/25",
		"Reason T", "Tags: local llm", "Summary T Snippet T",
		"[Build] What if T had a CLI? Write the CLI for T.", "[Contribute] What if T took a patch?",
		// related
		"## Related items", "Title R1", "https://e.com/R1", "Shared tags: local llm", "In short: Gist R1",
		// pattern
		"## Pattern it belongs to", "### Local runtimes", "4 items from 3 sources", "Runtimes ship quant pickers.", "Which quant wins on 8 GB?", "What if a picker?",
		// profile
		"## About the builder", "Go preferred.",
	} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if strings.Contains(out, "Title T (") && strings.Count(out, "https://e.com/T") > 1 {
		t.Error("the item must not be listed among its own related items")
	}
}

func TestBriefCopesWithMissingPatternProfileAndOldRecords(t *testing.T) {
	old := rec("O", 1)
	old.Result.Scores = &score.Scores{FirstOrDifferent: 3, TestableQuestion: 3, FitsTimebox: 3, ShipsPublicly: 3, HasCommunity: 3, Reason: "old"}
	out := Build(Input{Record: old, All: []store.RecordLine{old}})
	for _, want := range []string{"scored before the gist existed", "Tags: (none recorded)", "None share a tag", "None yet", "no profile.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "first ideas") {
		t.Error("an item without ideas must not get an ideas heading")
	}

	uns := rec("U", 1)
	uns.Result = score.Result{ID: "U", Status: score.StatusUnscored}
	if out := Build(Input{Record: uns}); !strings.Contains(out, "no verdict") || !strings.Contains(out, "Summary U") {
		t.Errorf("unscored brief: %s", out)
	}
}

func TestWriteSavesUnderBriefsAndRejectsBadIDs(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := Write(st, "abc123", "# hi\n")
	if err != nil || p != filepath.Join(st.Dir, "briefs", "abc123.md") {
		t.Fatalf("path=%q err=%v", p, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "# hi\n" {
		t.Fatalf("content %q", b)
	}
	if _, err := Write(st, "abc123", "# again\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "# again\n" {
		t.Fatal("a second brief must replace the first")
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	for _, bad := range []string{"", "../x", `a\b`, "a/b"} {
		if _, err := Write(st, bad, "x"); err == nil {
			t.Errorf("id %q accepted", bad)
		}
	}
}
