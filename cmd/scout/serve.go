package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/thelab3507/topic-scout/internal/config"
	"github.com/thelab3507/topic-scout/internal/export"
	"github.com/thelab3507/topic-scout/internal/novelty"
	"github.com/thelab3507/topic-scout/internal/run"
	"github.com/thelab3507/topic-scout/internal/score"
	"github.com/thelab3507/topic-scout/internal/serve"
	"github.com/thelab3507/topic-scout/internal/store"
)

const (
	defaultAddr     = "127.0.0.1:8787"
	defaultTaskName = "TheLabScout"
)

// exportEnv names the environment variable that sets the export folder.
const exportEnv = "THELAB_SCOUT_EXPORTS"

// exportDir picks the export folder: the flag, then THELAB_SCOUT_EXPORTS, then
// export_dir in settings.json in the data directory (which is how the scheduled
// task, started with no flags, finds it). "" means exports are off.
func exportDir(flagValue string, st *store.Store) (string, error) {
	dir := flagValue
	if dir == "" {
		dir = os.Getenv(exportEnv)
	}
	if dir == "" {
		s, err := st.LoadSettings()
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", store.SettingsName, err)
		}
		dir = s.ExportDir
	}
	if dir == "" {
		return "", nil
	}
	return filepath.Abs(dir) // the console and the log should show where files really go
}

// exportDirOrWarn is exportDir for commands that must work without it: a broken
// settings.json is reported, and exports are simply off.
func exportDirOrWarn(flagValue string, st *store.Store) string {
	dir, err := exportDir(flagValue, st)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err, "(exports are off)")
		return ""
	}
	return dir
}

// exportLatest refreshes latest.html after a command that rebuilt the digest
// (open, mark). Problems are warnings: the digest itself is already written.
func exportLatest(st *store.Store, dir string) {
	if dir == "" {
		return
	}
	if _, err := export.Snapshot(st.DigestPath(), dir, time.Now(), false); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not export the digest:", err)
	}
}

var taskNameOK = regexp.MustCompile(`^[A-Za-z0-9_. -]{1,100}$`)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "sources.json", "path to sources.json")
	dir := fs.String("data-dir", "", "data directory")
	expFlag := fs.String("export-dir", "", "folder (outside any git repo) that receives a copy of every digest")
	addr := fs.String("addr", defaultAddr, "address to listen on (loopback only)")
	task := fs.String("task-name", defaultTaskName, "Windows scheduled task to read the next run time from")
	open := fs.Bool("open", false, "open the console in your browser")
	if err := fs.Parse(args); err != nil {
		return run.ExitError
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, "usage: scout serve [--addr 127.0.0.1:8787] [--export-dir DIR] [--config sources.json] [--data-dir DIR] [--open]\n")
		return run.ExitError
	}
	if err := serve.ValidAddr(*addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	if !taskNameOK.MatchString(*task) {
		fmt.Fprintf(os.Stderr, "task name %q has characters a scheduled task name does not\n", *task)
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
	exp, err := exportDir(*expFlag, st)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitError
	}
	if exp != "" {
		if err := export.Check(exp); err != nil {
			fmt.Fprintln(os.Stderr, "export folder:", err)
			return run.ExitError
		}
	}
	if _, err := config.Load(*cfgPath); err != nil {
		// Fail early, not on the first click; the file is read again for every run.
		fmt.Fprintln(os.Stderr, "config:", err)
		return run.ExitError
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot listen:", err)
		return run.ExitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	srv := &serve.Server{
		Store:     st,
		ExportDir: exp,
		Runner:    consoleRunner(st, *cfgPath, exp),
		Now:       time.Now,
		NextRun:   scheduledNextRun(*task),
	}
	srv.Start(ctx)
	go srv.WatchNext(ctx)

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()

	url := "http://" + ln.Addr().String() + "/"
	fmt.Println("Scout console:", url)
	if exp != "" {
		fmt.Println("Exporting digests to:", exp)
	} else {
		fmt.Println("No export folder set (use --export-dir, THELAB_SCOUT_EXPORTS or settings.json).")
	}
	fmt.Println("Press Ctrl+C to stop.")
	if *open {
		if err := openInBrowser(url); err != nil {
			fmt.Fprintln(os.Stderr, "could not open the browser:", err)
		}
	}

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		stop()
		srv.Wait()
		return run.ExitError
	}
	srv.Wait() // a job in progress was cancelled with ctx; let it release run.lock
	return run.ExitOK
}

// consoleRunner runs the jobs the web console starts through exactly the code
// `scout run` and `scout patterns` use, writing to scout.log so the page can
// show it. sources.json is read again for every run, so edits apply at once.
func consoleRunner(st *store.Store, cfgPath, exportDir string) serve.Runner {
	return func(ctx context.Context, kind string, req serve.JobRequest) int {
		logw := newLog(st.Dir, true)
		defer logw.Close()
		switch kind {
		case serve.KindRun:
			cfg, err := config.Load(cfgPath)
			if err != nil {
				fmt.Fprintf(logw, "config: %v\n", err)
				return run.ExitError
			}
			return run.Run(ctx, run.Options{
				Config:    cfg,
				Store:     st,
				HTTP:      &http.Client{Timeout: 30 * time.Second},
				Scorer:    score.New(),
				Novelty:   novelty.New(os.Getenv("THELAB_GITHUB_TOKEN")),
				Now:       time.Now,
				Limit:     req.Limit,
				DryRun:    req.DryRun,
				Log:       logw,
				ExportDir: exportDir,
			})
		case serve.KindPatterns:
			return run.Patterns(ctx, run.Options{Store: st, Scorer: score.New(), Now: time.Now, Log: logw, ExportDir: exportDir})
		}
		fmt.Fprintf(logw, "unknown job %q\n", kind)
		return run.ExitError
	}
}

// --- next scheduled run ---------------------------------------------------------

// nextRunScript reads the task name from the environment, so a task name is
// never spliced into script text.
const nextRunScript = `$ErrorActionPreference = 'Stop'
$info = Get-ScheduledTaskInfo -TaskName $env:SCOUT_TASK
if (-not $info.NextRunTime -or $info.NextRunTime.Year -lt 2000) { 'none' } else { $info.NextRunTime.ToString('o') }`

// scheduledNextRun returns a function that asks Windows Task Scheduler when the
// named task fires next, or nil where there is no Task Scheduler.
func scheduledNextRun(task string) func(ctx context.Context) (time.Time, error) {
	if runtime.GOOS != "windows" {
		return nil
	}
	return func(ctx context.Context) (time.Time, error) {
		cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", nextRunScript)
		cmd.Env = append(os.Environ(), "SCOUT_TASK="+task)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return time.Time{}, errors.New("the scheduler did not answer in time")
			}
			return time.Time{}, fmt.Errorf("task %q was not found; install it with scripts\\install-task.ps1", task)
		}
		return parseNextRun(string(out), task)
	}
}

// parseNextRun reads the script's output: an ISO 8601 time, or "none".
func parseNextRun(out, task string) (time.Time, error) {
	s := strings.TrimSpace(out)
	if s == "" || s == "none" {
		return time.Time{}, fmt.Errorf("task %q has no next run (is it disabled?)", task)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("could not read the next run time %q", s)
	}
	return t, nil
}
