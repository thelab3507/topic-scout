// Package run orchestrates one scout pass in a fixed order:
//
//	fetch -> dedupe -> health check -> score (back to back) -> GitHub novelty -> weekly patterns -> digest
//
// The order matters. The inference server unloads its model after ~20 s idle
// and pays a 10-12 s reload on the next call, so all slow network work happens
// before scoring starts and GitHub search and the pattern pass happen after it
// ends. The pattern pass runs only when the last one is 6 or more days old and
// scoring did not abort, and nothing it does can fail the run.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/dedupe"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/report"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/sources"
	"github.com/thelab3507/topic-scout/internal/store"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUnavailable = 2 // inference server down, draining, or the run was aborted
)

// previewEvery is how many scored candidates pass between interim digest writes
// (a shortlisted candidate always triggers one).
const previewEvery = 10

// Options carries everything a run needs, so tests can swap in fakes.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	HTTP    *http.Client
	Scorer  *score.Scorer
	Novelty *novelty.Checker
	Now     func() time.Time
	Limit   int  // when > 0, score at most this many candidates (for trial runs)
	DryRun  bool // fetch and dedupe only; touch nothing and never call the server
	Log     io.Writer
}

// Run performs one pass and returns a process exit code.
func Run(ctx context.Context, o Options) int {
	logf := func(format string, args ...any) { fmt.Fprintf(o.Log, format+"\n", args...) }

	if !o.DryRun {
		unlock, err := o.Store.Lock()
		if err != nil {
			logf("cannot start: %v", err)
			return ExitError
		}
		defer unlock()
	}

	profile, profileErr := LoadProfile(o.Store, logf)
	if o.Scorer != nil {
		o.Scorer.Profile = profile
	}

	lr := store.LastRun{StartedAt: o.Now()}
	finish := func(outcome, msg string, code int) int {
		lr.Outcome, lr.Message, lr.FinishedAt = outcome, msg, o.Now()
		if err := o.Store.WriteLastRun(lr); err != nil {
			logf("could not write last_run.json: %v", err)
		}
		if err := RenderDigest(o.Store, o.Now(), "", profile); err != nil {
			logf("could not write digest: %v", err)
		}
		if msg != "" {
			logf("%s: %s", outcome, msg)
		}
		return code
	}

	if profileErr != nil {
		// Scores written under an unknown profile would not be comparable.
		return finish("error", profileErr.Error(), ExitError)
	}
	seen, err := o.Store.LoadSeen()
	if err != nil {
		return finish("error", err.Error(), ExitError)
	}

	// 1. Fetch everything first: slow network calls must not fall between scoring calls.
	fetched := sources.FetchAll(ctx, o.Config, o.HTTP, o.Now())
	lr.Fetched = len(fetched.Candidates)
	lr.Warnings = fetched.Warnings
	for _, w := range fetched.Warnings {
		logf("warning: %s", w)
	}
	if len(fetched.Candidates) == 0 && len(fetched.Warnings) == len(o.Config.Sources) {
		if o.DryRun {
			return ExitError
		}
		return finish("error", "every source failed", ExitError)
	}

	// 2. Dedupe against what was handled before, then within this run.
	var fresh []candidate.Candidate
	for _, c := range fetched.Candidates {
		if _, known := seen[c.ID]; known && !store.NeedsScoring(seen, c.ID) {
			lr.Duplicates++
			continue
		}
		fresh = append(fresh, c)
	}
	idx := dedupe.NewIndex()
	for id, title := range store.DedupeKnown(seen, o.Now()) {
		idx.AddKnown(id, title)
	}
	kept, dropped := idx.Filter(fresh)
	lr.Duplicates += len(dropped)

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Published.After(kept[j].Published) })
	limit := o.Config.MaxPerRun
	if o.Limit > 0 && o.Limit < limit {
		limit = o.Limit
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	logf("fetched %d, %d already handled or duplicate, scoring %d", lr.Fetched, lr.Duplicates, len(kept))

	if o.DryRun {
		for _, c := range kept {
			logf("  would score [%s] %s: %s", c.Category, c.Source, c.Title)
		}
		return ExitOK
	}
	if len(kept) == 0 {
		o.maybePatterns(ctx, profile, logf) // a quiet day is still a fine time for the weekly pass
		return finish("ok", "", ExitOK)
	}

	// 3. Never start or touch the server: if it is down, stop and say so.
	if err := o.Scorer.Healthy(ctx); err != nil {
		return finish("skipped", err.Error()+" (not starting it; the items will be retried next run)", ExitUnavailable)
	}

	// 4. Score everything back to back.
	rubric := score.RubricVersion(profile)
	preview := func(n int) {
		msg := fmt.Sprintf("%d of %d scored.", n, len(kept))
		if err := RenderDigest(o.Store, o.Now(), msg, profile); err != nil {
			logf("could not write digest: %v", err)
		}
	}
	preview(0)
	var shortlisted []candidate.Candidate
	n := 0
	scoreErr := o.Scorer.ScoreAll(ctx, kept, func(c candidate.Candidate, r score.Result) {
		n++
		now := o.Now()
		if err := o.Store.AppendResult(store.RecordLine{RunAt: now, RubricVersion: rubric, Candidate: c, Result: r}); err != nil {
			logf("could not record result: %v", err)
		}
		store.Record(seen, c, r, now)
		if err := o.Store.SaveSeen(seen, now); err != nil {
			logf("could not save seen.json: %v", err)
		}
		switch {
		case r.Status == score.StatusUnscored:
			lr.Unscored++
			logf("[%d/%d] unscored  %s (%s)", n, len(kept), c.Title, r.Error)
		case r.Shortlisted:
			lr.Scored++
			lr.Shortlisted++
			shortlisted = append(shortlisted, c)
			logf("[%d/%d] SHORTLIST %s (%d/25)", n, len(kept), c.Title, r.Scores.Total())
		default:
			lr.Scored++
			logf("[%d/%d] scored    %s (%d/25)", n, len(kept), c.Title, r.Scores.Total())
		}
		if r.Shortlisted || n%previewEvery == 0 {
			preview(n)
		}
	})

	// 5. GitHub novelty runs after scoring so it never interrupts the model.
	o.addNovelty(ctx, shortlisted, logf)

	var fe *score.FatalError
	switch {
	case errors.As(scoreErr, &fe):
		return finish("aborted", fe.Reason, ExitUnavailable)
	case scoreErr != nil:
		return finish("aborted", scoreErr.Error(), ExitUnavailable)
	}

	// 6. Weekly patterns, only after a clean scoring pass.
	o.maybePatterns(ctx, profile, logf)
	return finish("ok", "", ExitOK)
}

func (o Options) addNovelty(ctx context.Context, shortlisted []candidate.Candidate, logf func(string, ...any)) {
	if len(shortlisted) == 0 || o.Novelty == nil {
		return
	}
	known, err := o.Store.LoadNovelty()
	if err != nil {
		logf("could not read novelty.json: %v", err)
		return
	}
	for _, c := range shortlisted {
		if _, done := known[c.ID]; done {
			continue
		}
		known[c.ID] = o.Novelty.Check(ctx, c.Title)
		if known[c.ID].Skipped != "" {
			logf("novelty skipped for %q: %s", c.Title, known[c.ID].Skipped)
		}
	}
	if err := o.Store.SaveNovelty(known); err != nil {
		logf("could not save novelty.json: %v", err)
	}
}

// LoadProfile reads the builder profile from the data directory, warning
// through logf when profile.md had to be cut to its cap. A missing file gives "".
func LoadProfile(st *store.Store, logf func(string, ...any)) (string, error) {
	text, cut, err := st.LoadProfile()
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", store.ProfileName, err)
	}
	if cut {
		logf("warning: %s is over %d bytes; only the first %d are used", store.ProfileName, store.MaxProfileBytes, len(text))
	}
	return text, nil
}

// RenderDigest rebuilds digest.html from the stored state. It is used after
// every run and by the open and mark commands, which pass no progress text.
// While a run is scoring it passes some, which makes the page say so and
// refresh itself. profile is the builder profile, which the page's rubric
// version depends on.
func RenderDigest(st *store.Store, now time.Time, progress, profile string) error {
	recs, err := st.Results(now.AddDate(0, 0, -report.WindowDays))
	if err != nil {
		return err
	}
	nov, err := st.LoadNovelty()
	if err != nil {
		return err
	}
	dec, err := st.LoadDecisions()
	if err != nil {
		return err
	}
	last, err := st.ReadLastRun()
	if err != nil {
		return err
	}
	patterns, err := cluster.Load(st)
	if err != nil {
		return err
	}
	return report.Write(st.DigestPath(), report.Input{
		Now: now, LastRun: last, Records: recs, Novelty: nov, Decisions: dec, RubricVersion: score.RubricVersion(profile),
		Clusters: patterns, Progress: progress,
	})
}
