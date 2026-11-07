# topic-scout

Finds, scores and shortlists news-sparked build ideas for [The Lab](https://github.com/thelab3507/thelab).
It runs on one PC as a daily scheduled task, scores candidates on a **local** inference server (free, private),
and writes a single `digest.html` where a human reviews the shortlist and decides what to build.

**Status: Active (v0).** Issues and PRs welcome.

## How a run works

```
fetch  ->  dedupe  ->  health check  ->  score (back to back)  ->  GitHub novelty  ->  digest.html
```

1. **Fetch** official feeds and APIs (RSS/Atom, Hacker News API, HDX), 14-day window, per-source caps. No source needs a key.
   A feed that answers HTTP 429 is waited out once (as long as it asks, max 65 s) and retried.
2. **Dedupe**: exact canonical URL, then lexical title similarity (token-set Jaccard >= 0.7, version numbers count so
   `v0.5.1` and `v0.5.2` stay distinct). No embeddings. Items handled in earlier runs are never rescored.
3. **Score** each survivor against the charter's five criteria on `127.0.0.1:8080` (`POST /v1/infer`, strict
   `json_schema`, `reasoning: off`, sequential). All slow network work happens *before* scoring, because the model
   unloads after ~20 s idle and a reload costs 10-12 s.
4. **Novelty**: for shortlisted items only, two GitHub repository searches (created in the last 30 days, and all-time by
   stars). This is evidence for you, never an automatic rejection.
5. **Digest**: rebuilds `digest.html`. It is also rewritten while scoring (at the start, after every 10 results and after
   every shortlisted one). Those interim pages say "Run in progress" and refresh themselves every 30 seconds; the final
   write is static. Run `scout open` at any time to see the partial results.

**Shortlisted** means the model said `pass` *and* all five scores are >= 4. A mismatch between `pass` and the scores is
shown as a warning instead of being trusted silently. Non-`tech` categories (humanitarian, global, local) also get the
charter's stricter rules in the prompt (credible source, open data, responder-focused, no personal data).

**Unscored is not failed.** If the server returns an empty, truncated or malformed reply twice, the candidate is kept as
*unscored*, shown under "Needs review", and retried on later runs (3 attempts, then parked). It is never recorded as a fail.

## Use it

```
go build -o scout.exe ./cmd/scout

scout run --dry-run              # fetch + dedupe only; calls nothing local, writes nothing
scout run --limit 4              # trial run: score at most 4
scout run                        # the real thing
scout open                       # rebuild digest.html and open it
scout mark <id> picked "why"     # record a decision (an id prefix is enough); also: dismissed, clear
```

The digest is the landing place: shortlist (picked first), items needing review, everything scored but not shortlisted
(so you can check the model isn't rejecting good items or scoring everything a 4), and dismissed items. Each card has the
model's reason, the five scores, the novelty evidence and the exact `scout mark` command. Reload the page after marking.

Exit codes: `0` ok, `1` error, `2` inference server unavailable (down, draining, or the run aborted). If the server
is down the run is *skipped* and its items are retried next time; the scout never starts or restarts the server.

## Install the daily task (Windows)

```powershell
.\scripts\install-task.ps1          # builds, installs to %LOCALAPPDATA%\thelab-scout, registers "TheLabScout" daily 09:00
Start-ScheduledTask -TaskName TheLabScout
.\scripts\uninstall-task.ps1        # removes the task, keeps your data
```

Runs as you, only while logged on, with no console window, and runs a missed schedule when the PC is next on.
State, `digest.html` and `scout.log` live in `%LOCALAPPDATA%\thelab-scout` (override with `--data-dir` or `THELAB_SCOUT_DATA`).
That folder is deliberately outside this repo so scores never end up in a public history.

## Configuration

`sources.json` lists sources; per source: `type` (`rss`, `hn`, `hdx`), `category`, `max_items`,
optional `exclude_title` (regex, e.g. to drop release candidates) and `title_prefix` (so a bare `v0.40.15` becomes
`ollama/ollama release v0.40.15`). Adding another RSS/Atom feed is a config edit only.

| Environment variable | Purpose |
| --- | --- |
| `THELAB_GITHUB_TOKEN` | Optional. Fine-grained, public-read only. Raises the search limit from 10 to 30 requests/min. |
| `THELAB_SCOUT_DATA` | Data directory override. |

The rubric lives in `internal/score/rubric/`. Each stored result records a short `rubric_version` hash so scores stay
comparable across edits.

## Known limitations (honest list)

- **Scoring quality is only lightly checked.** No calibration set, by decision. In the first live runs (8 candidates)
  scores spread 2-5 and nothing was shortlisted, which is plausible for vendor changelogs but means the shortlist will be
  sparse. Spot-check `digest.html` after the first real runs.
- **Reddit is rate limited to about one request per minute** without credentials, so the four subreddit feeds add a few
  minutes to the fetch phase (it all happens before scoring, so the model is unaffected). Link posts have almost no text
  in the feed, so the model judges them on the title alone.
- **ReliefWeb was dropped.** Its API needs an approved appname. UN News and HDX cover the humanitarian side; add ReliefWeb
  back only if that gap shows up in the digest.
- **A request can hang at the model's idle-unload boundary.** Once, a call made ~22 s after the previous run waited the
  full 150 s client timeout; the retry then succeeded in 11 s. The scout recovers, but that run took 2.5 extra minutes.
  Cause not established; it is a server-side question.
- **Snippets come only from feed fields.** Articles are never fetched, so thin feeds give the model little to judge.
- **Local news (e.g. Singapore press) is not configured.** Add an `rss` source when terms of use are checked.
- `ggml-org/llama.cpp` is deliberately not on the release watchlist: it publishes a release per build.
- No licence file yet; choose one before making the repo public.

## Develop

```
go vet ./... && go test ./...
```

Standard library only (no third-party dependencies). Tests use fake feed, inference and GitHub servers, so they never
touch the real services.

The embedded font is Plus Jakarta Sans (SIL OFL 1.1, see `internal/report/assets/FONT-LICENSE.txt`).
