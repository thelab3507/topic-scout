package dedupe

import (
	"testing"

	"github.com/thelab3507/topic-scout/internal/candidate"
)

func c(id, title string) candidate.Candidate { return candidate.Candidate{ID: id, Title: title} }

func titles(cs []candidate.Candidate) []string {
	var out []string
	for _, x := range cs {
		out = append(out, x.Title)
	}
	return out
}

func TestSameIDIsDropped(t *testing.T) {
	ix := NewIndex()
	kept, dropped := ix.Filter([]candidate.Candidate{c("1", "Alpha beta gamma"), c("1", "Totally different")})
	if len(kept) != 1 || len(dropped) != 1 || dropped[0].Reason != "already seen" {
		t.Fatalf("kept=%v dropped=%v", titles(kept), dropped)
	}
}

func TestNearIdenticalTitlesMergeFirstWins(t *testing.T) {
	ix := NewIndex()
	kept, dropped := ix.Filter([]candidate.Candidate{
		c("1", "OpenAI launches GPT-6 model"),
		c("2", "OpenAI launches GPT-6 model today"),
	})
	if len(kept) != 1 || kept[0].ID != "1" || len(dropped) != 1 {
		t.Fatalf("kept=%v dropped=%v", titles(kept), dropped)
	}
}

func TestDistinctTitlesSharingFewWordsAreKept(t *testing.T) {
	ix := NewIndex()
	kept, _ := ix.Filter([]candidate.Candidate{
		c("1", "Ollama adds support for new vision models"),
		c("2", "Ollama pricing for cloud models announced"),
	})
	if len(kept) != 2 {
		t.Fatalf("kept=%v", titles(kept))
	}
}

func TestConsecutiveReleasesOfOneProjectAreKept(t *testing.T) {
	ix := NewIndex()
	kept, _ := ix.Filter([]candidate.Candidate{c("1", "Ollama v0.5.1"), c("2", "Ollama v0.5.2")})
	if len(kept) != 2 {
		t.Fatalf("version bumps must not merge: %v", titles(kept))
	}
}

func TestItemsFromEarlierRunsAreDropped(t *testing.T) {
	ix := NewIndex()
	ix.AddKnown("old", "UN warns of rising flood risk across South Asia")
	kept, dropped := ix.Filter([]candidate.Candidate{
		c("old", "anything"),
		c("new1", "UN warns of rising flood risk across South Asia region"),
		c("new2", "A fresh unrelated story about compilers"),
	})
	if len(kept) != 1 || kept[0].ID != "new2" || len(dropped) != 2 {
		t.Fatalf("kept=%v dropped=%v", titles(kept), dropped)
	}
}

func TestEmptyTitlesNeverMatch(t *testing.T) {
	ix := NewIndex()
	kept, _ := ix.Filter([]candidate.Candidate{c("1", "!!!"), c("2", "???")})
	if len(kept) != 2 {
		t.Fatalf("kept=%v", titles(kept))
	}
}
