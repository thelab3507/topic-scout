// Package export keeps a copy of the digest outside the scout's data directory
// and outside any git repository, so reports can be browsed and kept without
// scores ever reaching a public history.
//
// After a run the scout writes two kinds of file into the export folder:
//
//	latest.html                  always the newest digest
//	digest-YYYYMMDD-HHMMSS.html  one snapshot per finished run (the archive)
//
// Snapshots are never deleted by the scout. A digest is a single self-contained
// page (CSS and font are embedded), so each file opens on its own.
package export

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// LatestName is the always-current copy of the digest.
	LatestName = "latest.html"

	archiveLayout = "20060102-150405"
	archivePrefix = "digest-"
	archiveSuffix = ".html"
)

var archiveName = regexp.MustCompile(`^digest-(\d{8}-\d{6})\.html$`)

// Report is one archived digest.
type Report struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"` // when the snapshot was taken (local time, from the file name)
	Size int64     `json:"size"`
}

// Check reports whether dir is a usable export folder: absolute, and neither
// inside a git working tree nor itself one. A folder that does not exist yet is
// fine; Snapshot creates it.
func Check(dir string) error {
	if dir == "" {
		return errors.New("no export folder set")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return fmt.Errorf("%s is inside a git repository (%s); choose a folder outside every repo", abs, p)
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// Snapshot copies the digest at src into dir as latest.html and, when archive
// is set, also as a new dated file. It returns the archive file name ("" when
// none was written). Both files are replaced atomically. now names the archive.
func Snapshot(src, dir string, now time.Time, archive bool) (string, error) {
	if err := Check(dir); err != nil {
		return "", err
	}
	page, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(dir, LatestName), page); err != nil {
		return "", err
	}
	if !archive {
		return "", nil
	}
	name := archivePrefix + now.Local().Format(archiveLayout) + archiveSuffix
	if err := writeAtomic(filepath.Join(dir, name), page); err != nil {
		return "", err
	}
	return name, nil
}

// List returns the archived digests in dir, newest first. A missing folder
// gives an empty list. Files that do not follow the archive naming are ignored,
// so the folder can hold anything else.
func List(dir string) ([]Report, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Report
	for _, e := range entries {
		m := archiveName.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		at, err := time.ParseInLocation(archiveLayout, m[1], time.Local)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Report{Name: e.Name(), At: at, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// Path resolves a report name (an archive name or latest.html) to a file in
// dir. It accepts nothing else, which is what keeps a web request from reaching
// outside the export folder.
func Path(dir, name string) (string, bool) {
	if name != LatestName && !archiveName.MatchString(name) {
		return "", false
	}
	return filepath.Join(dir, name), true
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
