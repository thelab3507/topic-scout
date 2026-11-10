package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func writeProfile(t *testing.T, s *Store, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, ProfileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingProfileIsNotAnError(t *testing.T) {
	text, cut, err := open(t).LoadProfile()
	if text != "" || cut || err != nil {
		t.Fatalf("text=%q cut=%v err=%v", text, cut, err)
	}
}

func TestProfileUnderTheCapIsReturnedWhole(t *testing.T) {
	s := open(t)
	writeProfile(t, s, string(rune(0xFEFF))+"One person.\r\nGo preferred.\r\n\r\n")
	text, cut, err := s.LoadProfile()
	if err != nil || cut || text != "One person.\nGo preferred." {
		t.Fatalf("text=%q cut=%v err=%v", text, cut, err)
	}
}

func TestProfileIsCappedAtARuneBoundaryAndFlagged(t *testing.T) {
	// "é" is two bytes. Starting one ASCII byte in, every rune begins at an odd
	// offset, so byte 1500 falls in the middle of a rune.
	s := open(t)
	writeProfile(t, s, "x"+strings.Repeat("é", 1000))
	text, cut, err := s.LoadProfile()
	if err != nil || !cut {
		t.Fatalf("cut=%v err=%v", cut, err)
	}
	if len(text) > MaxProfileBytes || !utf8.ValidString(text) {
		t.Fatalf("len=%d valid=%v", len(text), utf8.ValidString(text))
	}
	if len(text) != MaxProfileBytes-1 {
		t.Fatalf("cut at %d bytes, want %d (the last whole rune)", len(text), MaxProfileBytes-1)
	}

	writeProfile(t, s, strings.Repeat("a", MaxProfileBytes))
	if text, cut, _ := s.LoadProfile(); cut || len(text) != MaxProfileBytes {
		t.Fatalf("a profile of exactly the cap must not be cut: len=%d cut=%v", len(text), cut)
	}
	writeProfile(t, s, strings.Repeat("a", MaxProfileBytes+1))
	if text, cut, _ := s.LoadProfile(); !cut || len(text) != MaxProfileBytes {
		t.Fatalf("len=%d cut=%v", len(text), cut)
	}
}

func TestProfileRepoExampleFitsTheCap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "profile.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || len(b) > MaxProfileBytes {
		t.Fatalf("profile.example.md is %d bytes; it must be 1..%d so it is never cut", len(b), MaxProfileBytes)
	}
	for _, want := range []string{"Go preferred", "RTX 4060 Ti", "llama.cpp", "lab-site", "calorie tracker", "OpenStreetMap"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("example profile lacks %q", want)
		}
	}
}

func TestLoadAndSaveJSONRoundTrip(t *testing.T) {
	s := open(t)
	var got map[string]int
	if err := s.LoadJSON("x.json", &got); err != nil || got != nil {
		t.Fatalf("a missing file must leave the value alone: %v %v", got, err)
	}
	if err := s.SaveJSON("x.json", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.LoadJSON("x.json", &got); err != nil || got["a"] != 1 {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestLockedFollowsTheRunLock(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "d"))
	if st.Locked() {
		t.Fatal("a fresh store is not locked")
	}
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Locked() {
		t.Fatal("must be locked while a run holds the lock")
	}
	unlock()
	if st.Locked() {
		t.Fatal("must be unlocked after")
	}
	// A lock left behind by a crash does not count once it is stale.
	lock := filepath.Join(st.Dir, "run.lock")
	os.WriteFile(lock, []byte("1\n"), 0o600)
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(lock, old, old)
	if st.Locked() {
		t.Fatal("a stale lock must not read as a run in progress")
	}
}

func TestSettingsRoundTripAndMissingFile(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "d"))
	if s, err := st.LoadSettings(); err != nil || s.ExportDir != "" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := st.SaveJSON(SettingsName, Settings{ExportDir: `C:\exports`}); err != nil {
		t.Fatal(err)
	}
	if s, err := st.LoadSettings(); err != nil || s.ExportDir != `C:\exports` {
		t.Fatalf("%+v %v", s, err)
	}
	os.WriteFile(filepath.Join(st.Dir, SettingsName), []byte("{oops"), 0o644)
	if _, err := st.LoadSettings(); err == nil {
		t.Fatal("a broken settings.json must be an error")
	}
}

func TestSettingsFileWithAByteOrderMarkStillLoads(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "d"))
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"export_dir":"E:/x"}`)...)
	os.WriteFile(filepath.Join(st.Dir, SettingsName), body, 0o644)
	if s, err := st.LoadSettings(); err != nil || s.ExportDir != "E:/x" {
		t.Fatalf("%+v %v", s, err)
	}
}
