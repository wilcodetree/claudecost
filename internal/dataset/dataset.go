// Package dataset builds the usage payload shared by the claudecost CLI and
// the claudecost-app window: source resolution, JSONL parsing with an
// mtime/size cache, aggregation, and the JSON schema injected into the
// dashboard template.
package dataset

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claudecost/internal/agg"
	"claudecost/internal/pricing"
	"claudecost/internal/scan"
)

// CoverageNote explains what claudecost can and cannot see. Unchanged from
// v0.1.0.
const CoverageNote = "Cowork and Claude Code only, from this machine. claude.ai browser and " +
	"mobile chats keep no local transcript and are deliberately not collected: the only " +
	"source for those would be an organisation-wide export of everyone's activity."

// Distinct errors Collect can return. Callers map each to their own message
// and exit code.
var (
	ErrNoSources  = errors.New("no source folders found")
	ErrNoFiles    = errors.New("no transcript files found")
	ErrNoSessions = errors.New("no sessions inside the reporting window")
)

// SeatError reports an unknown seat tier, with the valid options attached so
// callers can print them without recomputing the list.
type SeatError struct {
	Seat  string
	Valid []string
}

func (e *SeatError) Error() string {
	return fmt.Sprintf("unknown seat tier %q, valid: %s", e.Seat, strings.Join(e.Valid, ", "))
}

func validateSeat(cfg *pricing.Config, seat string) error {
	if _, ok := cfg.Subscription.SeatPriceUSD[seat]; ok {
		return nil
	}
	var valid []string
	for k := range cfg.Subscription.SeatPriceUSD {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	return &SeatError{Seat: seat, Valid: valid}
}

// ---------------------------------------------------------------------------
// Payload, schema 1 of claude_usage_extract.py
// ---------------------------------------------------------------------------

type priceOut struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cache_write"`
	CacheRead  float64 `json:"cache_read"`
}

type subOut struct {
	MonthlySubscriptionUSD float64            `json:"monthly_subscription_usd"`
	MonthlySubscriptionEUR float64            `json:"monthly_subscription_eur"`
	SeatsPurchased         int                `json:"seats_purchased"`
	UsageCreditsBalanceEUR float64            `json:"usage_credits_balance_eur"`
	CompanyConsumptionUSD  float64            `json:"company_consumption_usd"`
	Seats                  map[string]int     `json:"seats"`
	SeatPriceUSD           map[string]float64 `json:"seat_price_usd"`
	OutputCostFactor       float64            `json:"output_cost_factor"`
	CalibratedOn           string             `json:"calibrated_on"`
	Window                 string             `json:"window"`
	YourSeat               string             `json:"your_seat"`
	YourSeatPriceUSD       float64            `json:"your_seat_price_usd"`
}

type totalsOut struct {
	Sessions int     `json:"sessions"`
	Calls    int64   `json:"calls"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	CostSub  float64 `json:"cost_sub"`
}

// Payload is the full dataset injected into the dashboard template.
type Payload struct {
	Schema           int                    `json:"schema"`
	GeneratedAt      string                 `json:"generated_at"`
	WindowFrom       string                 `json:"window_from"`
	WindowTo         string                 `json:"window_to"`
	Prices           map[string]priceOut    `json:"prices_usd_per_mtok"`
	RefreshTaskID    string                 `json:"refresh_task_id"`
	Subscription     subOut                 `json:"subscription"`
	SurfaceLabels    map[string]string      `json:"surface_labels"`
	LongSessionCalls int                    `json:"long_session_calls"`
	Totals           totalsOut              `json:"totals"`
	Months           map[string]*agg.Bucket `json:"months"`
	Weeks            map[string]*agg.Bucket `json:"weeks"`
	Days             map[string]*agg.Bucket `json:"days"`
	Sessions         []*scan.Session        `json:"sessions"`
	CoverageNote     string                 `json:"coverage_note"`
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

func monthStart(t time.Time, back int) time.Time {
	y, m := t.Year(), int(t.Month())-back
	for m <= 0 {
		m += 12
		y--
	}
	return time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
}

// BuildPayload assembles the schema-1 payload from already-aggregated data.
func BuildPayload(cfg *pricing.Config, seat string, cutoff, today time.Time,
	months, weeks, days map[string]*agg.Bucket, kept []*scan.Session) Payload {

	prices := map[string]priceOut{}
	for _, fam := range cfg.Families() {
		p := cfg.Prices[fam]
		prices[p.Label] = priceOut{
			Input:      p.In,
			Output:     p.Out,
			CacheWrite: round4(p.In * cfg.CacheWriteMult),
			CacheRead:  round4(p.In * cfg.CacheReadMult),
		}
	}

	t := totalsOut{Sessions: len(kept)}
	for _, m := range months {
		t.Calls += m.Calls
		t.Tokens += m.Tokens
		t.Cost += m.Cost
		t.CostSub += m.CostSub
	}
	t.Cost = round4(t.Cost)
	t.CostSub = round4(t.CostSub)

	sub := cfg.Subscription
	return Payload{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format("2006-01-02T15:04:05") + "+00:00",
		WindowFrom:  cutoff.Format("2006-01-02"),
		WindowTo:    today.Format("2006-01-02"),
		Prices:      prices,
		Subscription: subOut{
			MonthlySubscriptionUSD: sub.MonthlySubscriptionUSD,
			MonthlySubscriptionEUR: sub.MonthlySubscriptionEUR,
			SeatsPurchased:         sub.SeatsPurchased,
			UsageCreditsBalanceEUR: sub.UsageCreditsBalanceEUR,
			CompanyConsumptionUSD:  sub.CompanyConsumptionUSD,
			Seats:                  sub.Seats,
			SeatPriceUSD:           sub.SeatPriceUSD,
			OutputCostFactor:       sub.OutputCostFactor,
			CalibratedOn:           sub.CalibratedOn,
			Window:                 sub.Window,
			YourSeat:               seat,
			YourSeatPriceUSD:       sub.SeatPriceUSD[seat],
		},
		SurfaceLabels:    scan.SurfaceLabel,
		LongSessionCalls: scan.LongSessionCalls,
		Totals:           t,
		Months:           months,
		Weeks:            weeks,
		Days:             days,
		Sessions:         kept,
		CoverageNote:     CoverageNote,
	}
}

// ---------------------------------------------------------------------------
// Cache: mtime/size-aware parsing shared by the CLI and the app window
// ---------------------------------------------------------------------------

type cacheEntry struct {
	mtime   time.Time
	size    int64
	session *scan.Session // nil means the file parsed to no usable session
}

// Cache reuses parsed sessions across calls to Collect when a transcript
// file's mtime and size are unchanged since the last call. The zero value is
// ready to use.
type Cache struct {
	entries map[string]cacheEntry

	// Sources and Files hold the folder and file lists resolved by the most
	// recent Collect call. Collect's progress callback only carries a
	// done/total file count, so a caller that wants to report the folder
	// count too (as the CLI's "Scanning N folder(s)..." line does) reads it
	// from here inside that same callback.
	Sources []string
	Files   []string
}

// ---------------------------------------------------------------------------
// Disk persistence: parsed sessions survive restarts, keyed by an app
// version + pricing config fingerprint so a release or recalibration forces
// a full re-parse instead of serving stale computed costs.
// ---------------------------------------------------------------------------

// diskEntry and diskCache are the gob-serializable shapes of cacheEntry and
// Cache.entries. gob only ever sees exported fields, so these are separate
// types rather than exporting cacheEntry itself. scan.Session and
// scan.PerModel hold only exported fields, so they round-trip through gob
// correctly even though CWD and Daily carry a json:"-" tag; gob does not
// look at json tags at all.
type diskEntry struct {
	MTime   time.Time
	Size    int64
	Session *scan.Session // nil = file parsed to no usable session
}

type diskCache struct {
	Fingerprint string
	Entries     map[string]diskEntry
}

// Fingerprint identifies everything that invalidates cached parse results:
// the app version (parse logic may change between releases) and the pricing
// config (cached sessions store computed costs).
func Fingerprint(appVersion string, cfg *pricing.Config) string {
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	return appVersion + "|" + hex.EncodeToString(sum[:])
}

// Load reads a previously saved cache from path, keeping it only if its
// stored fingerprint matches fingerprint. Any problem opening or decoding
// the file, or a fingerprint mismatch, is silent: c is simply left with an
// empty cache, and the next Collect re-parses everything, same as a first
// run. There is nothing sensible to recover from a stale or corrupt cache
// file.
func (c *Cache) Load(path, fingerprint string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var dc diskCache
	if err := gob.NewDecoder(f).Decode(&dc); err != nil {
		return
	}
	if dc.Fingerprint != fingerprint {
		return
	}

	entries := make(map[string]cacheEntry, len(dc.Entries))
	for k, v := range dc.Entries {
		entries[k] = cacheEntry{mtime: v.MTime, size: v.Size, session: v.Session}
	}
	c.entries = entries
}

// Save writes the cache to path, keeping only entries whose file path is in
// c.Files (the most recent Collect's file list), so transcripts that no
// longer exist do not accumulate in the file forever. It writes path+".tmp"
// and renames it over path, so a crash mid-write, or a concurrent reader,
// never sees a half-written cache file.
func (c *Cache) Save(path, fingerprint string) error {
	entries := make(map[string]diskEntry, len(c.Files))
	for _, f := range c.Files {
		e, ok := c.entries[f]
		if !ok {
			continue
		}
		entries[f] = diskEntry{MTime: e.mtime, Size: e.size, Session: e.session}
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(diskCache{Fingerprint: fingerprint, Entries: entries}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Collect resolves sources (the given list, else scan.DefaultSources()),
// lists transcript files, parses any that are new or changed since the last
// call on this Cache (reusing the cached session otherwise), aggregates the
// result and returns the dashboard payload for seat and monthsN.
//
// progress, if non-nil, is called once with (0, total) before parsing starts
// and again after every file with (doneSoFar, total).
func (c *Cache) Collect(cfg *pricing.Config, seat string, monthsN int, sources []string,
	progress func(done, total int)) (Payload, error) {

	if err := validateSeat(cfg, seat); err != nil {
		return Payload{}, err
	}

	srcs := sources
	if len(srcs) == 0 {
		srcs = scan.DefaultSources()
	}
	c.Sources = srcs
	if len(srcs) == 0 {
		return Payload{}, ErrNoSources
	}

	files := scan.FindJSONL(srcs)
	c.Files = files
	total := len(files)
	if progress != nil {
		progress(0, total)
	}
	if total == 0 {
		return Payload{}, ErrNoFiles
	}

	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}

	// Sequential pass: stat every file and split into cache hits (reuse the
	// session already held) and misses that need parsing. A miss whose stat
	// itself failed (listed a moment ago, gone now) is parsed but never
	// cached, same as before.
	type miss struct {
		index int
		path  string
		mtime time.Time
		size  int64
		stat  bool
	}
	results := make([]*scan.Session, len(files))
	var misses []miss
	hits := 0
	for i, f := range files {
		fi, statErr := os.Stat(f)
		if statErr == nil {
			if e, ok := c.entries[f]; ok && e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
				results[i] = e.session
				hits++
				continue
			}
			misses = append(misses, miss{index: i, path: f, mtime: fi.ModTime(), size: fi.Size(), stat: true})
			continue
		}
		misses = append(misses, miss{index: i, path: f})
	}

	// Cache hits are reported in one batch up front (they count as instantly
	// done); the workers below report the rest, one file at a time. progress
	// is not documented as concurrency-safe, so calls to it are serialized.
	var done atomic.Int64
	done.Store(int64(hits))
	var progressMu sync.Mutex
	report := func() {
		if progress == nil {
			return
		}
		progressMu.Lock()
		progress(int(done.Load()), total)
		progressMu.Unlock()
	}
	report()

	workers := min(runtime.NumCPU(), 8)
	if workers > len(misses) {
		workers = len(misses)
	}
	if workers < 1 {
		workers = 1
	}
	if len(misses) > 0 {
		jobs := make(chan miss)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for m := range jobs {
					// results is pre-sized and each index is written by
					// exactly one goroutine, so no two workers ever touch
					// the same slot.
					results[m.index] = scan.ParseSession(m.path, cfg)
					done.Add(1)
					report()
				}
			}()
		}
		go func() {
			for _, m := range misses {
				jobs <- m
			}
			close(jobs)
		}()
		wg.Wait()
	}

	// Insert the newly parsed entries back into c.entries: single goroutine,
	// after the pool has fully drained, so the entries map itself is never
	// touched concurrently.
	for _, m := range misses {
		if !m.stat {
			continue
		}
		c.entries[m.path] = cacheEntry{mtime: m.mtime, size: m.size, session: results[m.index]}
	}

	var sessions []*scan.Session
	for _, sess := range results {
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}

	today := time.Now()
	cutoff := monthStart(today, max(0, monthsN-1))
	months, weeks, days, kept := agg.Build(sessions, cutoff)
	if len(kept) == 0 {
		return Payload{}, ErrNoSessions
	}

	return BuildPayload(cfg, seat, cutoff, today, months, weeks, days, kept), nil
}
