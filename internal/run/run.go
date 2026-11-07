// Package run orchestrates one scout pass in a fixed order:
//
//	fetch -> dedupe -> health check -> score (back to back) -> GitHub novelty -> digest
//
// The order matters. The inference server unloads its model after ~20 s idle
// and pays a 10-12 s reload on the next call, so all slow network work happens
// before scoring starts and GitHub search happens after it ends.
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

	lr := store.LastRun{StartedAt: o.Now()}
	finish := func(outcome, msg string, code int) int {
		lr.Outcome, lr.Message, lr.FinishedAt = outcome, msg, o.Now()
		if err := o.Store.WriteLastRun(lr); err != nil {
			logf("could not write last_run.json: %v", err)
		}
		if err := RenderDigest(o.Store, o.Now()); err != nil {
			logf("could not write digest: %v", err)
		}
		if msg != "" {
			logf("%s: %s", outcome, msg)
		}
		return code
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
		return finish("ok", "", ExitOK)
	}

	// 3. Never start or touch the server: if it is down, stop and say so.
	if err := o.Scorer.Healthy(ctx); err != nil {
		return finish("skipped", err.Error()+" (not starting it; the items will be retried next run)", ExitUnavailable)
	}

	// 4. Score everything back to back.
	rubric := score.RubricVersion()
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

// RenderDigest rebuilds digest.html from the stored state. It is used after
// every run and by the open and mark commands.
func RenderDigest(st *store.Store, now time.Time) error {
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
	return report.Write(st.DigestPath(), report.Input{
		Now: now, LastRun: last, Records: recs, Novelty: nov, Decisions: dec, RubricVersion: score.RubricVersion(),
	})
}
