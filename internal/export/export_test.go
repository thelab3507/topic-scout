package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func page(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "digest.html")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSnapshotWritesLatestAndADatedArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "exports") // does not exist yet
	at := time.Date(2026, 11, 9, 9, 5, 7, 0, time.Local)

	name, err := Snapshot(page(t, "<html>one</html>"), dir, at, true)
	if err != nil {
		t.Fatal(err)
	}
	if name != "digest-20261109-090507.html" {
		t.Fatalf("archive name %q", name)
	}
	for _, f := range []string{LatestName, name} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || string(b) != "<html>one</html>" {
			t.Fatalf("%s: %q %v", f, b, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestSnapshotWithoutArchiveOnlyRefreshesLatest(t *testing.T) {
	dir := t.TempDir()
	if _, err := Snapshot(page(t, "old"), dir, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	name, err := Snapshot(page(t, "new"), dir, time.Now().Add(time.Hour), false)
	if err != nil || name != "" {
		t.Fatalf("name %q err %v", name, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, LatestName)); string(b) != "new" {
		t.Fatalf("latest.html = %q", b)
	}
	if reports, _ := List(dir); len(reports) != 1 {
		t.Fatalf("want the one earlier archive, got %v", reports)
	}
}

func TestSnapshotNeverOverwritesAnEarlierArchive(t *testing.T) {
	dir := t.TempDir()
	a := time.Date(2026, 11, 8, 9, 0, 0, 0, time.Local)
	b := time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)
	Snapshot(page(t, "day one"), dir, a, true)
	Snapshot(page(t, "day two"), dir, b, true)
	got, _ := os.ReadFile(filepath.Join(dir, "digest-20261108-090000.html"))
	if string(got) != "day one" {
		t.Fatalf("the earlier archive changed: %q", got)
	}
}

func TestSnapshotMissingSourceIsAnError(t *testing.T) {
	if _, err := Snapshot(filepath.Join(t.TempDir(), "nope.html"), t.TempDir(), time.Now(), true); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCheckRefusesAFolderInsideAGitRepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repo, "deep", "exports") // need not exist
	err := Check(inside)
	if err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Snapshot(page(t, "x"), inside, time.Now(), true); err == nil {
		t.Fatal("Snapshot must refuse too")
	}
	if _, err := os.Stat(inside); err == nil {
		t.Fatal("a refused export must not create the folder")
	}
	if err := Check(t.TempDir()); err != nil {
		t.Fatalf("a plain folder must pass: %v", err)
	}
	if Check("") == nil {
		t.Fatal("an empty folder name must fail")
	}
}

func TestListIsNewestFirstAndIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"digest-20261107-090000.html", "digest-20261109-090000.html", "digest-20261108-090000.html",
		LatestName, "notes.txt", "digest-bad.html", "digest-20261109-090000.html.tmp",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(dir, "digest-20261110-090000.html"), 0o755) // a folder with an archive's name

	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
		if r.Size != 5 {
			t.Errorf("%s size %d", r.Name, r.Size)
		}
	}
	want := "digest-20261109-090000.html digest-20261108-090000.html digest-20261107-090000.html"
	if strings.Join(names, " ") != want {
		t.Fatalf("got %v", names)
	}
	if got[0].At.Year() != 2026 || got[0].At.Day() != 9 || got[0].At.Hour() != 9 {
		t.Fatalf("time parsed from the name: %v", got[0].At)
	}
}

func TestListOfAMissingFolderIsEmpty(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPathAcceptsOnlyReportNames(t *testing.T) {
	dir := t.TempDir()
	for _, ok := range []string{LatestName, "digest-20261109-090000.html"} {
		if p, good := Path(dir, ok); !good || p != filepath.Join(dir, ok) {
			t.Errorf("%q: %q %v", ok, p, good)
		}
	}
	for _, bad := range []string{
		"", "..", "../secret.html", `..\secret.html`, "digest-20261109-090000.html/../x", "/etc/passwd",
		"digest-2026110-090000.html", "digest-20261109-090000.htm", "scout.log", "latest.html.tmp",
	} {
		if _, good := Path(dir, bad); good {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
