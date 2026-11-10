package run

import (
	"context"
	"fmt"

	"github.com/thelab3507/topic-scout/internal/cluster"
)

// maybePatterns runs the weekly pattern pass at the end of a run when the last
// pass is old enough. Whatever happens here is logged and swallowed: a failure
// must never fail the run, and the digest simply keeps the previous patterns.
func (o Options) maybePatterns(ctx context.Context, profile string, logf func(string, ...any)) {
	if ctx.Err() != nil {
		return
	}
	o.patternPass(ctx, profile, false, logf)
}

// patternPass groups the recent scored items, asks the model about the biggest
// groups and saves clusters.json. force skips the "last pass is 6+ days old"
// check and always health checks the server first. The exit code is only for
// `scout patterns`; a run ignores it.
//
// A pass that found groups but could not write any of them is not saved, so the
// next run tries again instead of waiting another week.
func (o Options) patternPass(ctx context.Context, profile string, force bool, logf func(string, ...any)) int {
	now := o.Now()
	last, err := cluster.Load(o.Store)
	if err != nil {
		logf("warning: could not read %s, starting afresh: %v", cluster.FileName, err)
		last = cluster.File{}
	}
	if !force && !cluster.Due(last, now) {
		return ExitOK
	}

	recs, err := o.Store.Results(now.AddDate(0, 0, -cluster.WindowDays))
	if err != nil {
		logf("pattern pass skipped: could not read results: %v", err)
		return ExitError
	}
	groups := cluster.Find(recs, now)

	// With nothing to ask there is nothing to call; a forced pass still checks
	// the server first so `scout patterns` reports a dead server honestly.
	if force || len(groups) > 0 {
		if err := o.Scorer.Healthy(ctx); err != nil {
			logf("pattern pass skipped: %v", err)
			return ExitUnavailable
		}
	}

	file, failed, genErr := cluster.Generate(ctx, o.Scorer, groups, now, profile, logf)
	code := ExitOK
	switch {
	case genErr != nil:
		logf("pattern pass stopped: %v", genErr)
		code = ExitUnavailable
	case len(groups) > 0 && failed == len(groups):
		logf("pattern pass: none of the %d patterns could be written; keeping the previous result", len(groups))
		code = ExitError
	}
	if len(groups) > 0 && len(file.Clusters) == 0 {
		return code
	}
	if err := cluster.Save(o.Store, file); err != nil {
		logf("could not save %s: %v", cluster.FileName, err)
		return ExitError
	}
	logf("pattern pass: %d pattern(s) from %d candidate tag(s)", len(file.Clusters), len(groups))
	return code
}

// Patterns is the console's "Update patterns": it forces a pattern pass now, health check
// first, then rebuilds the digest. It takes the run lock so it cannot overlap a
// run, since the server handles one call at a time.
func Patterns(ctx context.Context, o Options) int {
	logf := func(format string, args ...any) { fmt.Fprintf(o.Log, format+"\n", args...) }
	unlock, err := o.Store.Lock()
	if err != nil {
		logf("cannot start: %v", err)
		return ExitError
	}
	defer unlock()

	profile, err := LoadProfile(o.Store, logf)
	if err != nil {
		logf("%v", err)
		return ExitError
	}
	o.Scorer.Profile = profile

	code := o.patternPass(ctx, profile, true, logf)
	if ctx.Err() != nil {
		code = ExitStopped
	}
	if err := RenderDigest(o.Store, o.Now(), "", profile); err != nil {
		logf("could not write digest: %v", err)
		if code == ExitOK {
			code = ExitError
		}
		return code
	}
	o.exportDigest(logf, false) // the patterns changed the page, but this is not a new run to archive
	return code
}
