# claudecost design notes

Date: 2026-08-11. Status: v0.1.0, pilot.

## What this is

A standalone Go port of the my-usage-dashboard Cowork plugin: the
`claude_usage_extract.py` extractor plus the `dashboard_template.html` page,
compiled into one portable exe so anyone in the organization can run it
without Cowork, without the mirror setup, and without scheduled tasks.

## Decisions

1. **Direct transcript reads replace the mirror.** The plugin needs a mirror
   folder because Cowork refuses to mount Claude's own app data. A native
   process has no such restriction, so `setup_mirror.cmd`, the hourly copy
   task and the staleness window all disappear. The transcript locations are
   the same list `default_sources()` uses in the Python extractor.
2. **Schema 1 is preserved exactly.** The Go code produces the same JSON
   payload the extractor produces (same keys, same dedup rule: one entry per
   requestId keeping the largest output_tokens, same synthetic-turn filter,
   same output-driven subscription model). This means the plugin's template
   renders it unmodified, and numbers are comparable between the exe and the
   Cowork artifact on the same machine.
3. **The template is a byte copy, not a rewrite.** Copied from the plugin on
   2026-08-11 with two text edits: the "how to refresh" paragraph and one JS
   fallback string said "type /usage-dashboard in Cowork", which is wrong for
   an exe. The refresh button machinery is inert because refresh_task_id is
   always empty. Chart.js stays vendored inline; the page makes no network
   calls.
4. **Calibration compiled in, overridable by file.** The PRICES and
   SUBSCRIPTION blocks are embedded as defaults; a claudecost.json next to
   the exe overrides any subset. Quarterly recalibration therefore means
   distributing one small file. The plugin remains the canonical home of
   these numbers; this project mirrors them.
5. **Standard library only.** No bubbletea, no gopsutil: the terminal output
   is a plain summary table, the dashboard is the HTML page. This keeps the
   build dependency-free and the exe small.
6. **Deliberately personal.** Reads only the current user's folders. No
   aggregation across people, by design; individual usage is
   personnel-adjacent data.

## Differences from the Python extractor, all intentional

- Rounding uses round-half-away-from-zero instead of Python's banker's
  rounding: differences are below a tenth of a cent and only in display.
- JSON key order inside objects differs (Go sorts map keys); the template
  reads by key, so rendering is identical.
- The exe always writes the HTML report; `--json` is opt-in, where the Python
  script always writes JSON and makes HTML opt-in.
- generated_at carries "+00:00" like Python's isoformat, kept for template
  compatibility.

## Verification plan

Tree-sitter syntax check passed on all sources (the Cowork sandbox has no Go
toolchain; builds happen on Wilco's machine). Real verification: build, run,
and compare month totals against the Claude Cost (User) artifact on the same
machine the same day. They should agree to the cent; the only legitimate
drift is sessions written between the mirror's last hourly copy and now.

## Keep in sync with the plugin

- `internal/pricing/pricing.go` mirrors PRICES and SUBSCRIPTION.
- `internal/report/template.html` is a copy; re-copy and re-apply the two
  text patches when the plugin template changes.
- If the extractor's schema moves past 1, port the change or the copied
  template will stop matching the data.

## App mode (v0.2.0)

Second binary, `claudecost-app.exe`: a single WebView2 window instead of the
server-plus-tray design floated earlier. Decisions:

1. **One window, no server.** No localhost port, no tray icon, no browser
   tab. `github.com/jchv/go-webview2` hosts Edge's WebView2 engine directly
   inside our own native window; the finished dashboard template renders in
   it unchanged. Fewest moving parts for an org-wide rollout: double-click,
   window opens, numbers appear, close the window, app gone.
2. **`internal/dataset` is the shared core.** Both binaries now call the same
   `Cache.Collect`, so source resolution, parsing, dedup and aggregation
   cannot drift between them. `Cache` adds an mtime/size-keyed reuse of
   already-parsed sessions, so the app's periodic re-collects only re-parse
   transcript files that actually changed, not every file every time.
3. **Collects on startup, on a timer, and on demand.** Default interval 30
   minutes, floor 5 minutes. One shared rebuild path, guarded so re-entrant
   calls (a tick landing mid-refresh) are skipped rather than queued.
4. **No open ports, no admin rights.** `asInvoker` in the manifest is the
   no-admin guarantee. Data, cache and log live under
   `%LOCALAPPDATA%\claudecost\`, never next to the exe, so the exe can run
   from a read-only share.
5. **Icons via go-winres**, embedded as resource group `"#1"` in both
   `winres\cli.json` and `winres\app.json`, so the window can reuse the same
   resource id (`IconId: 1` in go-webview2's `WindowOptions`) instead of a
   separate manual `WM_SETICON` call.
6. **Single instance via a named mutex.** A second launch finds the existing
   window by title and brings it to the front instead of starting a second
   copy.
7. **WebView2 missing is handled, not ignored**, even though it should not
   happen on a managed Windows 10/11 machine: fall back to writing the report
   once and opening it in the default browser, with a message box explaining
   why there is no app window.

## Fast startup (v0.3.0)

Every start re-parsing all ~1700 transcript files took minutes; three
changes make every start after the first feel instant.

1. **Persistent parse cache.** `internal/dataset.Cache` now has `Load` and
   `Save`, gob-encoding the mtime/size-keyed entries map to
   `%LOCALAPPDATA%\claudecost\parsecache.gob`. A `Fingerprint(appVersion,
   cfg)` string (app version plus a SHA-256 of the pricing config) is stored
   alongside the entries and checked on `Load`; a mismatch, a missing file,
   or a decode error all just leave the cache empty; there is nothing worth
   recovering from a stale or corrupt cache file. `Save` writes only entries
   for files still in `c.Files`, so deleted transcripts do not accumulate,
   and writes via a `.tmp` file plus `os.Rename` so a crash mid-write never
   corrupts the real file.
2. **Instant navigation to the last dashboard.** The app window checks for
   an existing `dashboard.html` on startup: if there is one, it navigates to
   it immediately (its own "Snapshot taken" stamp is already honest about
   its age) and rebuilds in the background, reloading the page on success.
   A true first run, with no dashboard yet, keeps the original warming page
   with the progress bar.
3. **Parallel first parse.** `Collect` now splits files into cache hits and
   misses in one sequential stat pass, then parses the misses across a
   worker pool of `min(runtime.NumCPU(), 8)` goroutines writing into a
   pre-sized, index-addressed slice, so ordering stays deterministic without
   a lock. Progress is reported through an atomic done counter with calls to
   the callback serialized by a mutex, since it is not documented as
   concurrency-safe. The parsed entries are folded back into `c.entries`
   single-goroutine, once the pool has fully drained.

The CLI shares the same cache file and the same `Fingerprint`, gated behind
a new `-no-cache` flag that skips both `Load` and `Save`. The first launch
after any release (new `appVersion`) or a `claudecost.json` edit (new config
hash) still takes the full path once; that is accepted and expected.

## v0.3.1: startup polish

Two small fixes after the first day running v0.3.0:

1. **Scrollbar on the warming page.** The page's `body` had `height:100vh`
   but no `margin:0`, so the browser's default body margin pushed the
   rendered height past the viewport by a few pixels and drew a vertical
   scrollbar for content that otherwise fit exactly. Added `margin:0` and
   `overflow:hidden` to the inline style.
2. **"Refreshing" wording in the About tab.** `appRebuildNotice` said the
   window "re-reads your session transcripts when it opens", accurate for
   v0.2.0 but misleading after the fast-startup change: the window now opens
   straight to the last snapshot and the re-read happens invisibly
   afterward. Reworded to say that explicitly. `template.html` itself was
   not touched, this is still a string-replacement patch in
   `applyAppChrome`, same pattern as before.

No data-shape or behavior change beyond these two UI fixes.

## v0.4.0: in-app settings screen

Goal: recalibrating after an invoice or a seat change should not require
hand-editing `claudecost.json`. Adds a Settings screen inside the app
window itself.

1. **Scope: the `Subscription` block only.** `Prices` (Anthropic's published
   per-token rates) stays JSON-only: it changes rarely, and a UI for editing
   per-model rates would mostly just add surface area. Everything a person
   actually recalibrates quarterly, the invoice total, seat counts, seat
   prices, usage credits, company consumption, is covered. `OutputCostFactor`,
   `CalibratedOn` and `Window` are present too, behind an "Advanced"
   `<details>` disclosure, since they change far less often than the rest.
2. **No template.html edit.** Same pattern as the warming page and the
   Refresh button: `applyAppChrome` (now a method on `*app`, so it can read
   `a.cfg` and `a.seat` directly) injects a hidden modal overlay and its
   script just before `</body>`. The shared dashboard template stays
   untouched.
3. **Pre-filled from the running config, every rebuild.** The modal's field
   values are rendered server-side from `a.cfg.Subscription` and `a.seat` at
   the point `applyAppChrome` runs, not fetched separately when the modal
   opens. Opening it always shows what the dashboard is actually using.
4. **Saving is a two-step round trip, mirroring Refresh now.** The bound
   `ccSaveSettings(payload)` call does the fast, synchronous part
   (`applySettings`: validate, write `claudecost.json`, reset the in-memory
   cache) and returns immediately so the JS promise resolves quickly; the
   actual rebuild runs in a background goroutine exactly like `ccRefresh`,
   dispatching `location.reload()` on success. A bound function that blocks
   on a slow rebuild would freeze the WebView2 message loop, since
   `w.Bind` callbacks run synchronously on the same thread `w.Run()` pumps.
5. **Saving resets the whole parse cache, not just the fingerprint.**
   Every already-cached session carries costs computed with the old
   `Subscription`, and there is no cheap way to tell which cached entries
   would compute differently. Treated exactly like a version or config-file
   change: `a.cache = dataset.Cache{}` forces a full re-parse on the next
   `Collect`.
6. **`writeSubscriptionConfig` merges, it does not overwrite.** Reads the
   existing `claudecost.json` (if any) into a generic
   `map[string]json.RawMessage`, replaces only the `"subscription"` key, and
   writes the whole map back via a `.tmp` file plus `os.Rename`, same atomic
   pattern as the parse cache. An unusual hand-added `Prices` override in
   that file survives a Settings save untouched.
7. **Known edge case, accepted.** If a Settings save lands while a ticker
   refresh is already mid-flight, the immediate post-save rebuild can return
   `errRebuildBusy` and get skipped; the numbers are still saved correctly,
   the dashboard just does not refresh until the next tick or a manual
   Refresh now. Not worth a retry queue for how rarely the two would
   actually collide.

Also bumped both `cmd` mains and both `winres` files to 0.4.0; the CLI
itself has no settings screen (nothing long-running to put one in), the
version bump is just for keeping one number per release across both
binaries, as before.

Also reworded the "Refreshing" notice (About tab) to mention the Settings
save as a third rebuild trigger, alongside the interval and Refresh now.


## v0.5.0: tool usage and cross-session dedup (2026-08-12)

Goal: stop double-counting a conversation recorded under two session IDs, and
start counting which connectors, plugins and skills sessions actually call,
so "which of my plugins are dead weight" has a permanent answer.

1. **Cross-session dedup key.** A forked or resumed Cowork session's child
   transcript replays the parent's records, so both parse to identical turn
   sets under two different session IDs. `scan.FindJSONL` dedups by file name
   and `scan.ParseSession` dedups requestIds inside one file; neither catches
   this. `dataset.Collect` adds a post-parse pass keyed on five fields already
   on `scan.Session`: start, end, call count, output tokens, cache-read
   tokens. Two genuinely different conversations would need identical start
   and end timestamps to the millisecond plus identical token counts to
   collide, so the key is safe without any requestId bookkeeping across
   files. On a collision the session whose `SessionID` sorts first is kept,
   deterministic across runs and independent of file mtime. The count is
   exposed as `Cache.DroppedDuplicates` and logged once per rebuild.
2. **Tool counting, largest-per-requestId.** `scan.ParseSession` walks every
   assistant message's `content` blocks for `type == "tool_use"` and counts
   `name` occurrences, on all assistant lines rather than only ones with a
   `usage` block. A streamed call repeats the same `tool_use` block across
   partial lines, so counts are kept per requestId/uuid and only the largest
   per-name count per key survives, mirroring the existing
   largest-`output_tokens`-wins rule. `Session.Tools` stays nil when empty.
3. **Grouping by connector, not raw tool name.** `scan.ToolGroup` maps
   `mcp__<server>__<tool>` to `<server>`, and built-ins (`Read`, `Write`,
   `Bash`, ...) to `"(built in)"`. UUID-shaped server names pass through;
   the dashboard shortens them for display.
4. **Rollup and payload schema.** `agg.Bucket` gains
   `ByTool map[string]*ToolAgg` (`calls`, `sessions`) per month, week and
   day. `Sessions` counts distinct sessions that used the group, matching
   `SurfaceAgg.Sessions`. `dataset.Payload.Schema` moves from 1 to 2.
5. **Cache invalidation is automatic.** `dataset.Fingerprint` folds in the
   app version, so the version bump alone forces one full re-parse; no
   separate cache-format version was needed.
6. **Tools tab.** New tab in `internal/report/template.html`: a horizontal
   bar chart of the top groups by call count plus a table of every group
   with calls, sessions and share of calls, merged across the whole window.

## v0.6.0: source list catches storage migrations (2026-08-12)

Triggered by the first field report: a dashboard that showed last month but
nothing for the current one. Two facts drove the change:

1. **Claude Desktop moved Code sessions to `claude-code-sessions`**
   (anthropics/claude-code #29373). Verified that this folder holds only
   sidebar state (`local_*.json`, no jsonl); the actual Desktop Code
   transcripts land in `~\.claude\projects\<cwd-slug>\<cliSessionId>.jsonl`
   via the embedded CLI runtime, which was already scanned. The folder is
   scanned anyway in case transcripts follow the state files later.
2. **Anthropic documents a Roaming-to-Local AppData move on Windows** for
   Claude Desktop. Where Roaming is redirected the history is copied, not
   moved, which produces exactly the last-month-only symptom against a
   Roaming-only scan.

Changes: `scan.DefaultSources` now scans both session folder names across
both AppData roots, the MSIX LocalCache Roaming and Local views, and the
macOS/Linux equivalents. The rebuild logs one line per scanned source with
its transcript count plus a total, so a "stopped at last month" report is
diagnosable from the log alone.

## v0.6.1: skill breakdown and a readable Tools chart (2026-08-12)

The "(built in)" group was swallowing every Skill call along with the genuine
built-ins, so the one thing the Tools tab was built to answer, which skills
and plugins actually get used, was invisible.

1. **Skill calls are re-tagged at parse time.** A `Skill` tool_use block's
   own `name` is just `"Skill"`; the actual skill sits in the call's
   `input.skill` argument (already `"plugin:skill"` for a plugin skill).
   `scan.ParseSession` reads that field when `name == "Skill"` and counts the
   call under `"skill:<name>"`. An unidentifiable Skill call falls back to
   plain `"Skill"`, so nothing is silently dropped.
2. **`scan.ToolGroup` gained one more case.** `"skill:<name>"` groups as
   `<name>` unchanged, so a plugin skill's plugin prefix carries through into
   the group label. Additive to `scan` alone; `by_tool` totals still sum to a
   session's real call count.
3. **Tools chart readability.** The chart's box grew and the y-axis ticks
   render smaller with `autoSkip:false`, so every row gets a legible label.

## v0.6.2: 0.6.1 verified and its known gap closed (2026-08-12)

1. `go vet ./...` and a full build verified the 0.5.0 and 0.6.0 change sets
   together after they were written by different sessions and merged.
2. **First-click Tools chart fix.** `showTab` special-cased only
   overview/months/weeks for re-render on entry, so the Tools chart, created
   at load while its canvas was `display:none`, sized to zero on a first
   direct click into the tab. `showTab` now also calls `renderTools()` on
   entry; safe because `renderTools` destroys any existing chart before
   recreating.
3. Concurrency note: with two sessions editing the repo at once, a bulk
   in-place rewrite can silently skip a locked file. Make edits file by
   file, verify each by read-back, and end with a version consistency sweep.

## v0.6.3: Tools tab feedback from first real use (2026-08-12)

1. **Chart shows top 8, not top 15.** With one dominant "(built in)" bar the
   long tail was unreadable noise; the chartbox shrank from 420px to 320px to
   match. The table below still lists every group.
2. **Full names in the table.** `groupDisplay` shortens UUID-shaped connector
   ids to 8 characters for chart labels; the table was using the same short
   form, leaving the real id only in a hover tooltip. Table rows now render
   the full name with `word-break:break-all`; the chart keeps the short form.
3. **Plugins named everywhere.** Chart title, table header ("Connector,
   plugin or skill") and the tab lede now say connectors, plugins and skills.

UUID-named rows are claude.ai connector servers that only expose an opaque id
in the transcript; mapping them to friendly names would need a hand-maintained
lookup table, noted as a possible future nicety, not done.

Note for this public repo: the CLI is not retired here (the internal copy
dropped it in its own 0.3.1); both binaries build and both carry each version
bump.
