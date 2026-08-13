# claudecost

Your own Claude usage and cost dashboard, as a portable Windows app or a
single-file CLI. It reads the session transcripts Cowork and Claude Code
already write on your machine, prices them, and shows a self-contained HTML
dashboard: current and previous month, by month, week, day and session, in
euros, plus a Tools tab showing which connectors, plugins and skills your
sessions actually call.

This is the standalone sibling of the "Claude Cost (User)" Cowork dashboard
plugin: same data, same dedup rules, same dashboard page, but no Cowork
session, no mirror folder, no scheduled tasks. Everything reads the
transcript folders directly on this device.

Everything stays on this device. No API key, no admin rights, no network
calls, no install.

## The app (recommended)

Double-click `claudecost-app.exe`. One window opens, no console, no tray
icon, no browser tab. It collects your usage on startup, again every 30
minutes while the window stays open, and whenever you press "Refresh now" in
the bottom-right corner. Close the window and the app is gone; nothing keeps
running in the background.

- No open ports, no admin rights (`asInvoker`).
- Needs the WebView2 runtime, which ships with Edge on Windows 10/11. If it
  is somehow missing, the app falls back to writing the report once and
  opening it in your default browser instead.
- Data, cache and log live under `%LOCALAPPDATA%\claudecost\`, never next to
  the exe, so it can run from a read-only share.
- Launching it a second time brings the existing window to the front instead
  of opening a second copy.
- Flags: `-interval` (default 15m, floor 5m), `-months` (default 2), `-seat`,
  `-config`, `-source` (repeatable). Same meaning as the CLI flags below.
- Parsed transcripts are cached in `%LOCALAPPDATA%\claudecost\parsecache.gob`,
  so only the first start (and the first after an update or a config change)
  reads every transcript; later starts show the previous dashboard right
  away and quietly refresh in the background. Delete that file to force a
  full re-read.
- Click the gear icon (top right, next to Refresh now) to edit your
  subscription numbers, seat counts and seat prices from inside the window,
  no more hand-editing `claudecost.json`. Saving re-reads everything, since
  cached sessions carry costs computed with the old numbers.

Windows will likely show a blue "Windows protected your PC" screen the first
time, since this is an unsigned binary. That is expected: click **More info**,
then **Run anyway**, or build it yourself from source (see Build below).

## The CLI

For power users and scripting. Run it, and it writes a timestamped HTML
report and opens it once, then exits.

    claudecost                  build the dashboard and open it in the browser
    claudecost -months 3        widen the window to three months
    claudecost -seat Premium    price your seat tier correctly (default Standard)
    claudecost -no-open         write the report without opening it
    claudecost -json data.json  also write the raw dataset
    claudecost -source DIR      scan DIR instead of the auto-detected folders
    claudecost -no-cache        skip the parse cache, always re-parse everything

Reports land in a `reports` folder next to the exe if writable, otherwise in
`%LOCALAPPDATA%\claudecost\reports`. Override with `-out DIR`.

## What it does

- Finds transcripts in the known locations (Claude Code `~\.claude\projects`,
  Cowork under `%APPDATA%` or the MSIX `%LOCALAPPDATA%\Packages` path, plus
  the macOS and Linux equivalents).
- Deduplicates streamed API calls (one entry per requestId, largest
  output_tokens), drops synthetic turns, and prices every call two ways:
  subscription share (what a flat monthly plan actually costs, allocated by
  consumption) and API list price (the comparison).
- Renders the dashboard with Chart.js vendored inline: no network access.
- The CLI also prints a terminal summary with per-month totals.

Coverage is Cowork and Claude Code only. claude.ai in the browser keeps no
local transcript, so it cannot appear here; check claude.ai/settings/usage
for those limit bars instead.

## Build

Requires Go (winget install GoLang.Go). Then, in PowerShell, from this
folder:

    .\build.ps1

First run needs internet access: it fetches `go-winres` for the icons and
`go mod tidy` resolves the WebView2 binding. Produces `claudecost.exe` (the
CLI) and `claudecost-app.exe` (the app), both portable single files. Copy
them anywhere; no install.

## Configuration

Prices and the subscription calibration are compiled in as illustrative
defaults (see `internal\pricing\pricing.go`), not a real invoice. Plug in
your own numbers one of two ways.

In the app, click the gear icon and fill in the form: your subscription
total, seat counts, seat prices, usage credits, and (behind an Advanced
section, since it changes far less often) the output cost factor and
calibration date. Saving writes `claudecost.json` for you and re-reads
everything.

Or edit the file directly, useful for the CLI, for scripting, or for
anything outside the subscription block, such as an unusual per-model price
override: drop a `claudecost.json` next to the exe (CLI) or pass `-config`
(app), overriding any subset of the defaults. See `claudecost.example.json`.
The recommended calibration source is your real invoice total divided by
consumption, not seats times list price, since a flat subscription rarely
maps cleanly to per-seat usage.

## Status

Personal project, actively used daily. The dashboard is deliberately
personal: it reads only your own transcript folders and never aggregates or
compares across people. Costs shown are allocated shares of a flat monthly
invoice, not money owed by anyone.
