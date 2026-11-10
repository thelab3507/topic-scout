// Package store keeps the scout's state in a data directory outside the repo:
//
//	seen.json       every candidate already handled, so each run scores only new items
//	results.jsonl   one record per scoring outcome, appended as it arrives
//	novelty.json    GitHub search evidence for shortlisted candidates
//	decisions.json  the human's picked / dismissed marks
//	last_run.json   how the most recent run ended
//	profile.md      optional, hand-written: who the builder is (capped, see MaxProfileBytes)
//	clusters.json   the weekly patterns pass
//	settings.json   optional machine settings (the export folder), see Settings
//	briefs/<id>.md  deep-dive briefs written by `scout brief`
//	run.lock        present only while a run is in progress
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thelab3507/topic-scout/internal/candidate"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/score"
)

const (
	// MaxAttempts is how many runs may try an unscored candidate before it is
	// parked for manual review.
	MaxAttempts = 3

	// MaxProfileBytes caps profile.md. The profile rides in every prompt and the
	// model's context is 4096 tokens, so it cannot be allowed to grow.
	MaxProfileBytes = 1500

	staleLock    = 2 * time.Hour
	seenRetain   = 90 * 24 * time.Hour
	dedupeWindow = 30 * 24 * time.Hour
)

// Decision values a human can record.
const (
	Picked    = "picked"
	Dismissed = "dismissed"
)

// Store is a handle on the data directory.
type Store struct{ Dir string }

// Open creates the data directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

// DigestPath is where the landing page is written.
func (s *Store) DigestPath() string { return s.path("digest.html") }

// --- profile ----------------------------------------------------------------

// ProfileName is the optional builder profile in the data directory.
const ProfileName = "profile.md"

// LoadProfile reads profile.md. A missing file is not an error: it returns "".
// The text is capped at MaxProfileBytes, cut on a rune boundary, and cut is
// true when something was dropped so the caller can warn.
func (s *Store) LoadProfile() (text string, cut bool, err error) {
	b, err := os.ReadFile(s.path(ProfileName))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	text = strings.ToValidUTF8(string(b), "")
	text = strings.TrimPrefix(text, "\ufeff") // Notepad's byte-order mark
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) > MaxProfileBytes {
		n := MaxProfileBytes
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		text, cut = text[:n], true
	}
	return strings.TrimSpace(text), cut, nil
}

// --- run lock ---------------------------------------------------------------

// Lock prevents two runs (for example the scheduled task and a manual run)
// from clobbering each other's state. A lock older than two hours is treated as
// left over from a crash.
func (s *Store) Lock() (unlock func(), err error) {
	p := s.path("run.lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(p)
		if statErr == nil && time.Since(info.ModTime()) < staleLock {
			return nil, errors.New("another scout run is in progress (run.lock exists)")
		}
		os.Remove(p)
	}
	return nil, errors.New("could not acquire run.lock")
}

// Locked reports whether a run holds run.lock right now (a stale lock does not
// count). It only looks; it never takes or clears the lock.
func (s *Store) Locked() bool {
	info, err := os.Stat(s.path("run.lock"))
	return err == nil && time.Since(info.ModTime()) < staleLock
}

// --- settings ---------------------------------------------------------------

// SettingsName is the optional settings file in the data directory.
const SettingsName = "settings.json"

// Settings holds the few machine-level choices that must survive without
// command line flags, because the scheduled task starts scout with none.
type Settings struct {
	// ExportDir is a folder outside any git repository that receives a copy of
	// the digest after every run (see package export).
	ExportDir string `json:"export_dir,omitempty"`
}

// LoadSettings reads settings.json. A missing file gives zero Settings.
func (s *Store) LoadSettings() (Settings, error) {
	var v Settings
	err := readJSON(s.path(SettingsName), &v)
	return v, err
}

// --- seen -------------------------------------------------------------------

// Seen is what the scout remembers about a handled candidate.
type Seen struct {
	Title     string    `json:"title"`
	FirstSeen time.Time `json:"first_seen"`
	Status    string    `json:"status"` // score.StatusScored or score.StatusUnscored
	Attempts  int       `json:"attempts"`
}

func (s *Store) LoadSeen() (map[string]Seen, error) {
	m := map[string]Seen{}
	return m, readJSON(s.path("seen.json"), &m)
}

// SaveSeen writes the map, dropping entries older than 90 days.
func (s *Store) SaveSeen(m map[string]Seen, now time.Time) error {
	for id, v := range m {
		if now.Sub(v.FirstSeen) > seenRetain {
			delete(m, id)
		}
	}
	return writeJSON(s.path("seen.json"), m)
}

// NeedsScoring is true for new candidates and for unscored ones that have not
// yet used up their attempts.
func NeedsScoring(seen map[string]Seen, id string) bool {
	v, ok := seen[id]
	return !ok || (v.Status == score.StatusUnscored && v.Attempts < MaxAttempts)
}

// Record merges a scoring outcome into the seen map.
func Record(seen map[string]Seen, c candidate.Candidate, r score.Result, now time.Time) {
	v, ok := seen[c.ID]
	if !ok {
		v = Seen{Title: c.Title, FirstSeen: now}
	}
	v.Status = r.Status
	v.Attempts += r.Attempts
	seen[c.ID] = v
}

// DedupeKnown lists the seen items that dedupe should compare against: those
// first seen in the last 30 days and not waiting for a retry.
func DedupeKnown(seen map[string]Seen, now time.Time) map[string]string {
	out := map[string]string{}
	for id, v := range seen {
		if now.Sub(v.FirstSeen) <= dedupeWindow && !NeedsScoring(seen, id) {
			out[id] = v.Title
		}
	}
	return out
}

// --- results ----------------------------------------------------------------

// RecordLine is one scoring outcome as stored in results.jsonl.
type RecordLine struct {
	RunAt         time.Time           `json:"run_at"`
	RubricVersion string              `json:"rubric_version"`
	Candidate     candidate.Candidate `json:"candidate"`
	Result        score.Result        `json:"result"`
}

// AppendResult writes one line immediately so a crash loses nothing.
func (s *Store) AppendResult(r RecordLine) error {
	f, err := os.OpenFile(s.path("results.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Results returns the latest record per candidate whose run is at or after
// since, oldest first. A later retry of an unscored candidate replaces the
// earlier line.
func (s *Store) Results(since time.Time) ([]RecordLine, error) {
	f, err := os.Open(s.path("results.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	latest := map[string]int{}
	var all []RecordLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var r RecordLine
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // a torn final line after a crash must not break the digest
		}
		latest[r.Candidate.ID] = len(all)
		all = append(all, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []RecordLine
	for i, r := range all {
		if latest[r.Candidate.ID] == i && !r.RunAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// --- novelty ----------------------------------------------------------------

func (s *Store) LoadNovelty() (map[string]novelty.Evidence, error) {
	m := map[string]novelty.Evidence{}
	return m, readJSON(s.path("novelty.json"), &m)
}

func (s *Store) SaveNovelty(m map[string]novelty.Evidence) error {
	return writeJSON(s.path("novelty.json"), m)
}

// --- decisions --------------------------------------------------------------

// DecisionRecord is a human mark on a candidate.
type DecisionRecord struct {
	Decision string    `json:"decision"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
}

func (s *Store) LoadDecisions() (map[string]DecisionRecord, error) {
	m := map[string]DecisionRecord{}
	return m, readJSON(s.path("decisions.json"), &m)
}

// SetDecision records picked or dismissed; "clear" removes a mark.
func (s *Store) SetDecision(id, decision, note string, now time.Time) error {
	m, err := s.LoadDecisions()
	if err != nil {
		return err
	}
	switch decision {
	case Picked, Dismissed:
		m[id] = DecisionRecord{Decision: decision, Note: note, At: now}
	case "clear":
		delete(m, id)
	default:
		return fmt.Errorf("decision must be picked, dismissed or clear, got %q", decision)
	}
	return writeJSON(s.path("decisions.json"), m)
}

// ResolveID accepts a full ID or a unique prefix of one seen in results.
func (s *Store) ResolveID(prefix string) (string, error) {
	recs, err := s.Results(time.Time{})
	if err != nil {
		return "", err
	}
	var match string
	for _, r := range recs {
		id := r.Candidate.ID
		if len(prefix) > 0 && len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			if match != "" && match != id {
				return "", fmt.Errorf("%q matches more than one candidate", prefix)
			}
			match = id
		}
	}
	if match == "" {
		return "", fmt.Errorf("no candidate with id %q in results", prefix)
	}
	return match, nil
}

// --- last run ---------------------------------------------------------------

// LastRun summarises how the most recent run ended.
type LastRun struct {
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Outcome     string    `json:"outcome"` // ok, skipped, aborted, error
	Message     string    `json:"message,omitempty"`
	Fetched     int       `json:"fetched"`
	Duplicates  int       `json:"duplicates"`
	Scored      int       `json:"scored"`
	Unscored    int       `json:"unscored"`
	Shortlisted int       `json:"shortlisted"`
	Warnings    []string  `json:"warnings,omitempty"`
}

func (s *Store) WriteLastRun(l LastRun) error { return writeJSON(s.path("last_run.json"), l) }

func (s *Store) ReadLastRun() (*LastRun, error) {
	var l LastRun
	err := readJSON(s.path("last_run.json"), &l)
	if err != nil || l.StartedAt.IsZero() {
		return nil, err
	}
	return &l, nil
}

// --- other state files --------------------------------------------------------

// LoadJSON reads a state file in the data directory into v. A missing file
// leaves v untouched.
func (s *Store) LoadJSON(name string, v any) error { return readJSON(s.path(name), v) }

// SaveJSON replaces a state file in the data directory atomically.
func (s *Store) SaveJSON(name string, v any) error { return writeJSON(s.path(name), v) }

// --- helpers ----------------------------------------------------------------

// readJSON leaves v untouched when the file does not exist yet.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // a hand-edited file may start with a BOM
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeJSON replaces the file atomically so a crash never leaves half a file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
