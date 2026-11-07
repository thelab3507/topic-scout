package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/run"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

func seeded(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.AppendResult(store.RecordLine{
		RunAt:     time.Now(),
		Candidate: candidate.Candidate{ID: "abc123def456", Title: "Seeded candidate", URL: "https://e.com/x", Published: time.Now()},
		Result: score.Result{ID: "abc123def456", Status: score.StatusScored, Shortlisted: true,
			Scores: &score.Scores{FirstOrDifferent: 5, TestableQuestion: 5, FitsTimebox: 5, ShipsPublicly: 5, HasCommunity: 5, Pass: true, Reason: "r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, st
}

func TestMarkRecordsDecisionAndRefreshesDigest(t *testing.T) {
	dir, st := seeded(t)
	if code := realMain([]string{"mark", "--data-dir", dir, "abc", "picked", "worth", "a", "try"}); code != run.ExitOK {
		t.Fatalf("exit %d", code)
	}
	dec, _ := st.LoadDecisions()
	if dec["abc123def456"].Decision != "picked" || dec["abc123def456"].Note != "worth a try" {
		t.Fatalf("decisions: %+v", dec)
	}
	html, err := os.ReadFile(st.DigestPath())
	if err != nil || !strings.Contains(string(html), "worth a try") {
		t.Fatalf("digest not refreshed with the note: %v", err)
	}
}

func TestMarkRejectsBadInput(t *testing.T) {
	dir, _ := seeded(t)
	for name, args := range map[string][]string{
		"unknown id":       {"mark", "--data-dir", dir, "zzz", "picked"},
		"bad decision":     {"mark", "--data-dir", dir, "abc", "maybe"},
		"missing decision": {"mark", "--data-dir", dir, "abc"},
	} {
		if code := realMain(args); code != run.ExitError {
			t.Errorf("%s: exit %d, want %d", name, code, run.ExitError)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	if realMain(nil) != run.ExitError || realMain([]string{"bogus"}) != run.ExitError {
		t.Fatal("bad invocations must exit 1")
	}
	if realMain([]string{"help"}) != run.ExitOK {
		t.Fatal("help must exit 0")
	}
}

func TestRunWithMissingConfigFails(t *testing.T) {
	if code := realMain([]string{"run", "--config", filepath.Join(t.TempDir(), "nope.json"), "--dry-run"}); code != run.ExitError {
		t.Fatalf("exit %d", code)
	}
}

func TestDataDirPrecedence(t *testing.T) {
	t.Setenv("THELAB_SCOUT_DATA", "/from/env")
	if d, _ := dataDir("/from/flag"); d != "/from/flag" {
		t.Errorf("flag should win, got %s", d)
	}
	if d, _ := dataDir(""); d != "/from/env" {
		t.Errorf("env should be next, got %s", d)
	}
	t.Setenv("THELAB_SCOUT_DATA", "")
	if d, err := dataDir(""); err != nil || !strings.HasSuffix(d, "thelab-scout") {
		t.Errorf("default should end in thelab-scout, got %s (%v)", d, err)
	}
}
