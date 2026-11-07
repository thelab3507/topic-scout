// Command scout finds, scores and shortlists topic candidates for The Lab.
//
//	scout run  [--config sources.json] [--data-dir DIR] [--limit N] [--dry-run]
//	scout open [--data-dir DIR]
//	scout mark [--data-dir DIR] <id> <picked|dismissed|clear> [note]
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

	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/run"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/store"
)

const usage = `scout: topic scout for The Lab

  scout run  [--config sources.json] [--data-dir DIR] [--limit N] [--dry-run]
      Fetch, dedupe, score on the local inference server, check GitHub, write the digest.
  scout open [--data-dir DIR]
      Rebuild the digest and open it in your browser.
  scout mark [--data-dir DIR] <id> <picked|dismissed|clear> [note]
      Record your decision on a candidate (an id prefix is enough).

Environment: THELAB_SCOUT_DATA (data dir), THELAB_GITHUB_TOKEN (optional).
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
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	st, err := openStore(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	if err := run.RenderDigest(st, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "digest:", err)
		return run.ExitError
	}
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
	if err := run.RenderDigest(st, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "digest:", err)
		return run.ExitError
	}
	fmt.Printf("%s: %s. Reload the digest.\n", id, fs.Arg(1))
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
