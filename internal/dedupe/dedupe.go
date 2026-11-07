// Package dedupe removes repeat candidates using exact canonical URLs and
// lexical title similarity (token-set Jaccard). There are no embeddings.
package dedupe

import (
	"strings"
	"unicode"

	"github.com/thelab3507/topic-scout/internal/candidate"
)

// Threshold is the Jaccard similarity at or above which two titles are the
// same story. Numeric tokens are kept, so "v0.5.1" and "v0.5.2" stay distinct.
const Threshold = 0.7

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "is": true, "are": true, "with": true, "at": true,
	"by": true, "from": true, "as": true, "its": true, "it": true, "this": true, "that": true,
}

type tokenSet map[string]struct{}

// Drop is a candidate that was removed and why.
type Drop struct {
	Candidate candidate.Candidate
	Reason    string
}

// Index remembers what has been seen so far, across runs and within this one.
type Index struct {
	ids    map[string]bool
	titles []entry
}

type entry struct {
	id     string
	tokens tokenSet
}

func NewIndex() *Index { return &Index{ids: map[string]bool{}} }

// AddKnown records an item from an earlier run.
func (ix *Index) AddKnown(id, title string) {
	ix.ids[id] = true
	ix.titles = append(ix.titles, entry{id, tokens(title)})
}

// Filter keeps the first of any group of duplicates, in the order given, and
// adds every kept candidate to the index so later items compare against it.
func (ix *Index) Filter(cands []candidate.Candidate) (kept []candidate.Candidate, dropped []Drop) {
	for _, c := range cands {
		if ix.ids[c.ID] {
			dropped = append(dropped, Drop{c, "already seen"})
			continue
		}
		toks := tokens(c.Title)
		if dup := ix.nearDuplicate(toks); dup != "" {
			dropped = append(dropped, Drop{c, "near-duplicate of " + dup})
			continue
		}
		ix.ids[c.ID] = true
		ix.titles = append(ix.titles, entry{c.ID, toks})
		kept = append(kept, c)
	}
	return kept, dropped
}

func (ix *Index) nearDuplicate(t tokenSet) string {
	for _, e := range ix.titles {
		if jaccard(t, e.tokens) >= Threshold {
			return e.id
		}
	}
	return ""
}

func tokens(title string) tokenSet {
	fields := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	set := tokenSet{}
	for _, f := range fields {
		if !stopwords[f] {
			set[f] = struct{}{}
		}
	}
	return set
}

func jaccard(a, b tokenSet) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}
