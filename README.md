# topic-scout

Finds, scores and shortlists news-sparked build ideas for [The Lab](https://github.com/thelab3507/thelab).
It runs on one PC as a daily scheduled task, scores candidates on a **local** inference server (free, private),
and writes a single `digest.html` where a human reviews the shortlist and decides what to build.

**Status: Active (v0).** Issues and PRs welcome.

## How a run works

```
fetch  ->  dedupe  ->  health check  ->  score (back to back)  ->  GitHub novelty  ->  weekly patterns  ->  digest.html
```

1. **Fetch** official feeds and APIs (RSS/Atom, Hacker News API, HDX), 14-day window, per-source caps. No source needs a key.
   A feed that answers HTTP 429 is waited out once (as long as it asks, max 65 s) and retried.
2. **Dedupe**: exact canonical URL, then lexical title similarity (token-set Jaccard >= 0.7, version numbers count so
   `v0.5.1` and `v0.5.2` stay distinct). No embeddings. Items handled in earlier runs are never rescored.
3. **Score** each survivor against the charter's five criteria on `127.0.0.1:8080` (`POST /v1/infer`, strict
   `json_schema`, `reasoning: off`, sequential). All slow network work happens *before* scoring, because the model
   unloads after ~20 s idle and a reload costs 10-12 s. The reply carries more than scores: a plain-English `gist`,
   1-3 topic `tags`, and 2-3 project `ideas` (see below). The [builder profile](#your-profile) is appended to the prompt.
4. **Novelty**: for shortlisted items only, two GitHub repository searches (created in the last 30 days, and all-time by
   stars). This is evidence for you, never an automatic rejection.
5. **Patterns**: if the last pattern pass is 6 or more days old and scoring did not abort, group the last 14 days of
   scored items by tag and ask the model what is forming around the biggest groups (see [Deep dives](#deep-dives)).
   It can never fail the run: a pattern that cannot be written is skipped with a warning in `scout.log`.
6. **Digest**: rebuilds `digest.html`. It is also rewritten while scoring (at the start, after every 10 results and after
   every shortlisted one). Those interim pages say "Run in progress" and refresh themselves every 30 seconds; the final
   write is static. Run `scout open` at any time to see the partial results.

**The rubric (v2)** is built for projects, not just news. The Lab ships a small public project (a tool, app, library,
integration or upstream contribution, plus a writeup) in 2 weeks, 4 at most, mainly from new tech: developer tools,
APIs, models, open datasets and platforms. The five criteria are: *first or different* (released in roughly the last
2 weeks, or the project it sparks does not appear to exist yet), *project hook* (the item sparks a concrete, useful
build; a pure measurement or benchmark with nothing built scores at most 3), *fits the timebox*, *ships publicly* and
*has a community*. The hook is still stored under the JSON key `testable_question`, so older results stay readable.
The ideas must be simple English "What if...?" questions with a kind: **Build**, **Experiment** or **Contribute**.
At least two should be Build or Contribute; if the model returns fewer, the result is kept and carries a warning.

**Shortlisted** means the model said `pass` *and* all five scores are >= 4. A mismatch between `pass` and the scores is
shown as a warning instead of being trusted silently. Non-`tech` categories (humanitarian, global, local) also get the
charter's stricter rules in the prompt (credible source, open data, responder-focused, no personal data).

**Token budget.** The server's context is 4096 tokens. The prompt (rubric, extra rules, profile and candidate) is capped
at 3200 estimated tokens and the reply at 800 (patterns: 600), and a test keeps their sum under 4096 and checks that a
full-size profile still leaves at least 600 tokens for the candidate.

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
scout patterns                   # run the weekly pattern pass now (health check first)
scout brief <id>                 # write briefs/<id>.md in the data directory and print it
```

The digest is the landing place: weekly **patterns**, then the shortlist (picked first), items needing review,
everything scored but not shortlisted (so you can check the model isn't rejecting good items or scoring everything a 4),
and dismissed items. Each card has an "In short:" line, topic tags, the ideas under "What if..." (a kind badge, a bold
title and a muted detail), the model's reason, the five scores (the second pill is the *Hook*), the novelty evidence and
the exact `scout mark` and `scout brief` commands. The "Scored, not shortlisted" table shows the same gist, tags and
ideas. Reload the page after marking.

Exit codes: `0` ok, `1` error, `2` inference server unavailable (down, draining, or the run aborted). If the server
is down the run is *skipped* and its items are retried next time; the scout never starts or restarts the server.

## Your profile

An optional `profile.md` in the data directory tells the model who is building. Copy
[profile.example.md](profile.example.md) there and edit it:

```powershell
Copy-Item profile.example.md "$env:LOCALAPPDATA\thelab-scout\profile.md"
```

- It is appended to **every** system prompt (scoring and patterns) under "About the builder", so the ideas fit your
  time, languages, hardware and interests, and avoid what you want to avoid. The brief includes it too.
- It is capped at **1500 bytes**, cut on a character boundary. When it is cut, `run`, `open`, `mark`, `brief` and
  `patterns` log a warning. Every byte rides in a 4096-token context, which is why the cap exists.
- It is part of the **rubric version**: scores written under different profiles are not comparable, so editing the
  profile changes the hash shown in the digest footer and stored with each result. Old results are not rescored.
- If it exists but cannot be read, `scout run` stops with an error instead of scoring without it.
- It lives in the data directory, not the repo, so personal details never reach a public history.

## Deep dives

**Patterns.** At the end of `scout run`, if the last pass is 6 or more days old and scoring did not abort, the scout
groups scored items from the last 14 days by normalised tag (lower case, spaces collapsed, edge punctuation trimmed). A
tag is a *pattern* when it has 3 or more items from 2 or more different sources; the biggest 3 are kept. For each, one
model call (at most 10 items: source, title, gist; strict schema; 600 tokens; profile appended) returns a name, what is
forming, the open question and 2-3 ideas. It uses the same transport and retry rules as scoring (`Scorer.Ask`). The
result is saved to `clusters.json` and shown under **Patterns** at the top of the digest, with a collapsible item list.
When there are none, the digest says so. `scout patterns` forces a pass (after a health check) and takes the run lock
so it cannot overlap a scheduled run. If every pattern fails, nothing is saved, so the next run tries again.

**Briefs.** The local model only sees a feed snippet. For an item worth a closer look, `scout brief <id>` writes
`briefs/<id>.md` in the data directory and prints it: instructions for a stronger assistant (read the item and related
items, search the last 4 weeks for the same topic, say what is really new, name the open question, give 3-5
"What if...?" builds that fit you, and be critical about what exists, the risks and the 2-week fit), then the item
(title, link, source, date, gist, scores, reason, tags, feed text, the local model's first ideas), up to 8 related items
that share a tag, the pattern it belongs to, and your profile. Paste it into a stronger assistant. Every card in the
digest shows the command.

## Install the daily task (Windows)

```powershell
.\scripts\install-task.ps1          # builds, installs to %LOCALAPPDATA%\thelab-scout, registers "TheLabScout" daily 09:00
Start-ScheduledTask -TaskName TheLabScout
.\scripts\uninstall-task.ps1        # removes the task, keeps your data
```

Runs as you, only while logged on, with no console window, and runs a missed schedule when the PC is next on.
State, `digest.html`, `scout.log`, `profile.md`, `clusters.json` and `briefs\` live in `%LOCALAPPDATA%\thelab-scout` (override with `--data-dir` or `THELAB_SCOUT_DATA`).
That folder is deliberately outside this repo so scores never end up in a public history.

## Configuration

`sources.json` lists sources; per source: `type` (`rss`, `hn`, `hdx`), `category`, `max_items`,
optional `exclude_title` (regex, e.g. to drop release candidates) and `title_prefix` (so a bare `v0.40.15` becomes
`ollama/ollama release v0.40.15`). Adding another RSS/Atom feed is a config edit only.

| Environment variable | Purpose |
| --- | --- |
| `THELAB_GITHUB_TOKEN` | Optional. Fine-grained, public-read only. Raises the search limit from 10 to 30 requests/min. |
| `THELAB_SCOUT_DATA` | Data directory override. |

The rubric lives in `internal/score/rubric/`. Each stored result records a short `rubric_version` hash (rubric, schema
and builder profile) so scores stay comparable across edits.

## Known limitations (honest list)

- **Scoring quality is only lightly checked.** No calibration set, by decision. In the first live runs (8 candidates)
  scores spread 2-5 and nothing was shortlisted, which is plausible for vendor changelogs but means the shortlist will be
  sparse. Spot-check `digest.html` after the first real runs.
- **Rubric v2 and the richer reply have only been tested against fake servers.** Nothing has been run against the real
  inference server yet. The new schema (`minItems`/`maxItems` on arrays, an `enum`, nested objects) is not confirmed to
  be accepted by its grammar compiler; if it is not, the first run stops with `invalid_grammar` and says so. The 800-token
  reply budget is also an estimate (gist + tags + 2-3 ideas + scores + reason); a truncated reply is retried once and
  then shown as unscored, as before. Results scored under v1 have no gist, tags or ideas and are skipped by patterns.
- **Idea quality is the model's, and small.** The local model is told to avoid generic ideas and to fit your profile, but
  nothing checks that an idea is original or feasible; that is what `scout brief` and a stronger assistant are for.
  The Build/Contribute mix is only warned about, never enforced.
- **Patterns lean on tag consistency.** Tags are normalised but not merged, so "local llm" and "local llm inference"
  are different tags. An item can sit under several patterns. A pattern pass needs about a week of tagged results
  before the first one appears.
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
