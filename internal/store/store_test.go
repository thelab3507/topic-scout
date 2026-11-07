package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/score"
)

var t0 = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func line(id, status string, at time.Time) RecordLine {
	return RecordLine{RunAt: at, RubricVersion: "v1",
		Candidate: candidate.Candidate{ID: id, Title: "T" + id}, Result: score.Result{ID: id, Status: status}}
}

func TestSeenSurvivesReloadAndPrunesOldEntries(t *testing.T) {
	s := open(t)
	seen := map[string]Seen{
		"new": {Title: "n", FirstSeen: t0, Status: score.StatusScored},
		"old": {Title: "o", FirstSeen: t0.AddDate(0, 0, -91), Status: score.StatusScored},
	}
	if err := s.SaveSeen(seen, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSeen()
	if err != nil || len(got) != 1 || got["new"].Title != "n" {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "seen.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestMissingFilesReadAsEmpty(t *testing.T) {
	s := open(t)
	if m, err := s.LoadSeen(); err != nil || len(m) != 0 {
		t.Fatal(m, err)
	}
	if r, err := s.Results(time.Time{}); err != nil || len(r) != 0 {
		t.Fatal(r, err)
	}
	if l, err := s.ReadLastRun(); err != nil || l != nil {
		t.Fatal(l, err)
	}
}

func TestNeedsScoringRequeuesUnscoredUntilAttemptCap(t *testing.T) {
	seen := map[string]Seen{
		"scored":   {Status: score.StatusScored, Attempts: 1},
		"retry":    {Status: score.StatusUnscored, Attempts: 2},
		"parked":   {Status: score.StatusUnscored, Attempts: MaxAttempts},
		"exhaust1": {Status: score.StatusUnscored, Attempts: 6},
	}
	for id, want := range map[string]bool{"fresh": true, "scored": false, "retry": true, "parked": false, "exhaust1": false} {
		if got := NeedsScoring(seen, id); got != want {
			t.Errorf("NeedsScoring(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestRecordAccumulatesAttemptsAndKeepsFirstSeen(t *testing.T) {
	seen := map[string]Seen{}
	c := candidate.Candidate{ID: "a", Title: "A"}
	Record(seen, c, score.Result{Status: score.StatusUnscored, Attempts: 2}, t0)
	Record(seen, c, score.Result{Status: score.StatusScored, Attempts: 1}, t0.Add(time.Hour))
	if v := seen["a"]; v.Attempts != 3 || v.Status != score.StatusScored || !v.FirstSeen.Equal(t0) {
		t.Fatalf("%+v", v)
	}
}

func TestDedupeKnownSkipsRetriesAndOldItems(t *testing.T) {
	seen := map[string]Seen{
		"scored": {Title: "s", FirstSeen: t0, Status: score.StatusScored},
		"retry":  {Title: "r", FirstSeen: t0, Status: score.StatusUnscored, Attempts: 1},
		"old":    {Title: "o", FirstSeen: t0.AddDate(0, 0, -40), Status: score.StatusScored},
	}
	got := DedupeKnown(seen, t0)
	if len(got) != 1 || got["scored"] != "s" {
		t.Fatalf("got %v", got)
	}
}

func TestResultsLatestRecordWinsAndTornLineIsIgnored(t *testing.T) {
	s := open(t)
	for _, r := range []RecordLine{line("a", score.StatusUnscored, t0), line("b", score.StatusScored, t0), line("a", score.StatusScored, t0.Add(time.Hour))} {
		if err := s.AppendResult(r); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(filepath.Join(s.Dir, "results.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"run_at": "torn`)
	f.Close()

	got, err := s.Results(time.Time{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	for _, r := range got {
		if r.Candidate.ID == "a" && r.Result.Status != score.StatusScored {
			t.Fatal("retry did not supersede the earlier unscored line")
		}
	}
	recent, _ := s.Results(t0.Add(30 * time.Minute))
	if len(recent) != 1 || recent[0].Candidate.ID != "a" {
		t.Fatalf("window filter wrong: %v", recent)
	}
}

func TestDecisions(t *testing.T) {
	s := open(t)
	if err := s.SetDecision("a", Picked, "good one", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("b", Dismissed, "", t0); err != nil {
		t.Fatal(err)
	}
	m, _ := s.LoadDecisions()
	if m["a"].Decision != Picked || m["a"].Note != "good one" || m["b"].Decision != Dismissed {
		t.Fatalf("%v", m)
	}
	if err := s.SetDecision("a", "clear", "", t0); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.LoadDecisions(); len(m) != 1 {
		t.Fatalf("clear failed: %v", m)
	}
	if err := s.SetDecision("a", "maybe", "", t0); err == nil {
		t.Fatal("invalid decision accepted")
	}
}

func TestResolveID(t *testing.T) {
	s := open(t)
	s.AppendResult(line("abc123def456", score.StatusScored, t0))
	s.AppendResult(line("abd999def456", score.StatusScored, t0))
	if id, err := s.ResolveID("abc"); err != nil || id != "abc123def456" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := s.ResolveID("ab"); err == nil {
		t.Fatal("ambiguous prefix accepted")
	}
	if _, err := s.ResolveID("zzz"); err == nil {
		t.Fatal("unknown prefix accepted")
	}
	if _, err := s.ResolveID(""); err == nil {
		t.Fatal("empty prefix accepted")
	}
}

func TestLockExcludesSecondRunAndReleases(t *testing.T) {
	s := open(t)
	unlock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(); err == nil {
		t.Fatal("second lock acquired while first held")
	}
	unlock()
	unlock2, err := s.Lock()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	unlock2()
}

func TestStaleLockIsReplaced(t *testing.T) {
	s := open(t)
	p := filepath.Join(s.Dir, "run.lock")
	os.WriteFile(p, []byte("999"), 0o600)
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(p, old, old)
	unlock, err := s.Lock()
	if err != nil {
		t.Fatalf("stale lock not replaced: %v", err)
	}
	unlock()
}

func TestLastRunRoundTrip(t *testing.T) {
	s := open(t)
	in := LastRun{StartedAt: t0, FinishedAt: t0.Add(time.Minute), Outcome: "ok", Scored: 4, Warnings: []string{"w"}}
	if err := s.WriteLastRun(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.ReadLastRun()
	if err != nil || out == nil || out.Scored != 4 || out.Outcome != "ok" || len(out.Warnings) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}
