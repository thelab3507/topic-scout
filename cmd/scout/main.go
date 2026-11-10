// Command scout finds, scores and shortlists topic candidates for The Lab.
//
//	scout run  [--config sources.json] [--data-dir DIR] [--export-dir DIR] [--limit N] [--dry-run]
//	scout open [--data-dir DIR] [--export-dir DIR]
//	scout mark [--data-dir DIR] [--export-dir DIR] <id> <picked|dismissed|clear> [note]
//	scout patterns [--data-dir DIR] [--export-dir DIR]
//	scout brief [--data-dir DIR] <id>
//	scout serve [--addr 127.0.0.1:8787] [--export-dir DIR] [--config sources.json] [--data-dir DIR] [--open]
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/brief"
	"github.com/thelab3507/topic-scout/internal/cluster"
	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/run"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

const usage = `scout: topic scout for The Lab

  scout run  [--config sources.json] [--data-dir DIR] [--export-dir DIR] [--limit N] [--dry-run]
      Fetch, dedupe, score on the local inference server, check GitHub, write the digest.
  scout open [--data-dir DIR] [--export-dir DIR]
      Rebuild the digest and open it in your browser.
  scout mark [--data-dir DIR] [--export-dir DIR] <id> <picked|dismissed|clear> [note]
      Record your decision on a candidate (an id prefix is enough).
  scout patterns [--data-dir DIR] [--export-dir DIR]
      Run the weekly pattern pass now (it also runs at the end of "scout run" once a week).
  scout brief [--data-dir DIR] <id>
      Write briefs/<id>.md in the data directory and print it: a deep-dive brief for a stronger assistant.
  scout serve [--addr 127.0.0.1:8787] [--export-dir DIR] [--config sources.json] [--data-dir DIR] [--open]
      Start the local web console: run the scout, see the next scheduled run, read the log, open earlier reports.

An optional profile.md in the data directory (max 1500 bytes) describes you; it is added to every prompt.
An export folder (--export-dir, THELAB_SCOUT_EXPORTS, or export_dir in settings.json) receives a copy of
every finished digest; it must be outside any git repository.

Environment: THELAB_SCOUT_DATA (data dir), THELAB_SCOUT_EXPORTS (export folder), THELAB_GITHUB_TOKEN (optional).
Exit codes: 0 ok, 1 error, 2 inference server unavailable or run aborted.
`

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return run.ExitError
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "open":
		return cmdOpen(args[1:])
	case "mark":
		return cmdMark(args[1:])
	case "patterns":
		return cmdPatterns(args[1:])
	case "brief":
		return cmdBrief(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return run.ExitOK
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
	return run.ExitError
}

func dataDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("THELAB_SCOUT_DATA"); v != "" {
		return v, nil
	}
	base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "thelab-scout"), nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "sources.json", "path to sources.json")
	dir := fs.String("data-dir", "", "data directory (default: THELAB_SCOUT_DATA or %LOCALAPPDATA%\\thelab-scout)")
	expFlag := fs.String("export-dir", "", "folder (outside any git repo) that receives a copy of the digest")
	limit := fs.Int("limit", 0, "score at most N candidates (trial runs)")
	dry := fs.Bool("dry-run", false, "fetch and dedupe only; never call the server or write state")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return run.ExitError
	}
	d, err := dataDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	st, err := store.Open(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}

	logw := newLog(d, !*dry)
	defer logw.Close()
	exp := exportDirOrWarn(*expFlag, st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return run.Run(ctx, run.Options{
		Config:  cfg,
		Store:   st,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Scorer:  score.New(),
		Novelty: novelty.New(os.Getenv("THELAB_GITHUB_TOKEN")),
		Now:     time.Now,
		Limit:   *limit,
		DryRun:  *dry,
		Log:     logw,

		ExportDir: exp,
	})
}

func openStore(dir string) (*store.Store, error) {
	d, err := dataDir(dir)
	if err != nil {
		return nil, err
	}
	return store.Open(d)
}

func cmdOpen(args []string) int {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory")
	expFlag := fs.String("export-dir", "", "export folder to refresh")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	st, err := openStore(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	profile, err := loadProfile(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	if err := run.RenderDigest(st, time.Now(), "", profile); err != nil {
		fmt.Fprintln(os.Stderr, "digest:", err)
		return run.ExitError
	}
	exportLatest(st, exportDirOrWarn(*expFlag, st))
	fmt.Println(st.DigestPath())
	if err := openInBrowser(st.DigestPath()); err != nil {
		fmt.Fprintln(os.Stderr, "could not open the browser:", err)
		return run.ExitError
	}
	return run.ExitOK
}

func cmdMark(args []string) int {
	fs := flag.NewFlagSet("mark", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory")
	expFlag := fs.String("export-dir", "", "export folder to refresh")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	if fs.NArg() < 2 {
		fmt.Fprint(os.Stderr, "usage: scout mark [--data-dir DIR] <id> <picked|dismissed|clear> [note]\n")
		return run.ExitError
	}
	st, err := openStore(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	id, err := st.ResolveID(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	note := strings.Join(fs.Args()[2:], " ")
	if err := st.SetDecision(id, fs.Arg(1), note, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	profile, err := loadProfile(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	if err := run.RenderDigest(st, time.Now(), "", profile); err != nil {
		fmt.Fprintln(os.Stderr, "digest:", err)
		return run.ExitError
	}
	exportLatest(st, exportDirOrWarn(*expFlag, st))
	fmt.Printf("%s: %s. Reload the digest.\n", id, fs.Arg(1))
	return run.ExitOK
}

// loadProfile reads profile.md, warning on stderr when it had to be cut.
func loadProfile(st *store.Store) (string, error) {
	return run.LoadProfile(st, func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) })
}

func cmdPatterns(args []string) int {
	fs := flag.NewFlagSet("patterns", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory")
	expFlag := fs.String("export-dir", "", "folder (outside any git repo) that receives a copy of the digest")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	d, err := dataDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	st, err := store.Open(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	logw := newLog(d, true)
	defer logw.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return run.Patterns(ctx, run.Options{Store: st, Scorer: score.New(), Now: time.Now, Log: logw, ExportDir: exportDirOrWarn(*expFlag, st)})
}

func cmdBrief(args []string) int {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	if fs.NArg() != 1 {
		fmt.Fprint(os.Stderr, "usage: scout brief [--data-dir DIR] <id>\n")
		return run.ExitError
	}
	st, err := openStore(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	id, err := st.ResolveID(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	all, err := st.Results(time.Time{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	var target *store.RecordLine
	for i := range all {
		if all[i].Candidate.ID == id {
			target = &all[i]
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "no candidate with id %q in results\n", id)
		return run.ExitError
	}
	profile, err := loadProfile(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	patterns, err := cluster.Load(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	text := brief.Build(brief.Input{Record: *target, All: all, Clusters: patterns, Profile: profile})
	path, err := brief.Write(st, id, text)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	fmt.Print(text)
	fmt.Fprintln(os.Stderr, "saved to", path) // stderr, so the brief on stdout can be piped as is
	return run.ExitOK
}

func openInBrowser(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// scoutLog writes timestamped lines to scout.log (when enabled) and, best
// effort, to stderr. The scheduled task has no console, so the file is where
// an unattended run's story lives.
type scoutLog struct{ f *os.File }

const maxLogBytes = 1 << 20

func newLog(dir string, toFile bool) *scoutLog {
	if !toFile {
		return &scoutLog{}
	}
	p := filepath.Join(dir, "scout.log")
	if info, err := os.Stat(p); err == nil && info.Size() > maxLogBytes {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return &scoutLog{}
	}
	return &scoutLog{f: f}
}

func (l *scoutLog) Write(p []byte) (int, error) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + string(p)
	if l.f != nil {
		l.f.WriteString(line)
	}
	os.Stderr.WriteString(line)
	return len(p), nil
}

func (l *scoutLog) Close() {
	if l.f != nil {
		l.f.Close()
	}
}
