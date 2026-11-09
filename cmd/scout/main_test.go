package main

import (
	"io"
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

// captureStdout runs f and returns what it printed to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

func taggedRecord(id, title string, tags ...string) store.RecordLine {
	return store.RecordLine{
		RunAt:     time.Now(),
		Candidate: candidate.Candidate{ID: id, Title: title, URL: "https://e.com/" + id, Source: "src", Category: "tech", Published: time.Now()},
		Result: score.Result{ID: id, Status: score.StatusScored, Scores: &score.Scores{
			Gist: "Gist of " + title, Tags: tags, Reason: "r", FirstOrDifferent: 4, TestableQuestion: 4, FitsTimebox: 4, ShipsPublicly: 4, HasCommunity: 4, Pass: true,
			Ideas: []score.Idea{{Kind: score.KindBuild, Title: "What if a?", Detail: "Make a."}, {Kind: score.KindContribute, Title: "What if b?", Detail: "Make b."}},
		}},
	}
}

func TestBriefWritesTheFileAndPrintsTheSameText(t *testing.T) {
	dir, st := seeded(t)
	if err := st.AppendResult(taggedRecord("feed00000001", "Main item", "local llm")); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendResult(taggedRecord("feed00000002", "Related item", "local llm")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.md"), []byte("Go preferred."), 0o644); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() { code = realMain([]string{"brief", "--data-dir", dir, "feed00000001"}) })
	if code != run.ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"# Deep dive: Main item", "Gist of Main item", "Related item", "Go preferred.", "stronger assistant"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed brief lacks %q", want)
		}
	}
	saved, err := os.ReadFile(filepath.Join(dir, "briefs", "feed00000001.md"))
	if err != nil || string(saved) != out {
		t.Fatalf("briefs/<id>.md must hold exactly what was printed: %v", err)
	}

	// An id prefix is enough, and an old record without gist or tags still works.
	out = captureStdout(t, func() { code = realMain([]string{"brief", "--data-dir", dir, "abc"}) })
	if code != run.ExitOK || !strings.Contains(out, "Seeded candidate") || !strings.Contains(out, "scored before the gist existed") {
		t.Fatalf("exit %d, output: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "briefs", "abc123def456.md")); err != nil {
		t.Fatal("prefix lookup must save under the full id")
	}
}

func TestBriefRejectsBadInput(t *testing.T) {
	dir, _ := seeded(t)
	for name, args := range map[string][]string{
		"no id":      {"brief", "--data-dir", dir},
		"two ids":    {"brief", "--data-dir", dir, "abc", "def"},
		"unknown id": {"brief", "--data-dir", dir, "zzz"},
	} {
		if code := realMain(args); code != run.ExitError {
			t.Errorf("%s: exit %d, want %d", name, code, run.ExitError)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "briefs")); len(entries) != 0 {
		t.Errorf("a failed brief left files behind: %v", entries)
	}
}

func TestPatternsRejectsBadFlagsWithoutTouchingTheServer(t *testing.T) {
	if code := realMain([]string{"patterns", "--no-such-flag"}); code != run.ExitError {
		t.Fatalf("exit %d", code)
	}
}

func TestMarkRendersTheDigestWithTheProfileInTheRubricVersion(t *testing.T) {
	dir, st := seeded(t)
	if err := os.WriteFile(filepath.Join(dir, "profile.md"), []byte("Go preferred."), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := realMain([]string{"mark", "--data-dir", dir, "abc", "picked"}); code != run.ExitOK {
		t.Fatalf("exit %d", code)
	}
	html, _ := os.ReadFile(st.DigestPath())
	if !strings.Contains(string(html), "rubric "+score.RubricVersion("Go preferred.")) {
		t.Fatal("the digest's rubric version must include the profile")
	}
	if !strings.Contains(string(html), "scout brief abc123def456") {
		t.Fatal("cards must offer the brief command")
	}
}

func TestUsageMentionsTheNewCommands(t *testing.T) {
	for _, want := range []string{"scout patterns", "scout brief", "profile.md"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}
