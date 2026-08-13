//go:build windows

// Command claudecost-app is the single-window desktop face of claudecost: a
// WebView2 window that collects your Claude usage on startup, on an
// interval, and on demand, and shows the same dashboard template as the CLI,
// live, in place. No server, no open port, no tray, no browser tab.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"claudecost/internal/dataset"
	"claudecost/internal/pricing"
	"claudecost/internal/report"
)

const (
	version     = "0.6.3"
	windowTitle = "Claude Cost"
	mutexName   = `Local\claudecost-app`
	minInterval = 5 * time.Minute
)

func init() {
	// go-webview2 pumps a Win32 message loop tied to the thread that created
	// the window; keep that on one OS thread for the life of the process.
	runtime.LockOSThread()
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	interval := flag.Duration("interval", 15*time.Minute, "how often to re-read transcripts while the window is open")
	monthsN := flag.Int("months", 2, "how many months to include, counting the current one")
	seat := flag.String("seat", "Standard", "your own seat tier (Standard or Premium)")
	cfgPath := flag.String("config", "", "config file overriding the compiled-in prices and subscription (default: claudecost.json in the app's data folder, if present)")
	var sources multiFlag
	flag.Var(&sources, "source", "extra folder to scan; repeatable, overrides auto-detect")
	flag.Parse()

	if *interval < minInterval {
		*interval = minInterval
	}

	dataDir := appDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		// No window, no console, and now nowhere to log either: nothing
		// sensible left to do.
		return
	}
	setupLog(dataDir)
	log.Printf("claudecost-app %s starting", version)

	// Same idea as the CLI's exe-adjacent lookup, just rooted at the app's
	// own data folder instead: nothing is written or read next to the exe,
	// so it can run from a read-only share. This is what makes a quarterly
	// price/subscription recalibration a "drop one file" update instead of
	// a rebuild: place claudecost.json in %LOCALAPPDATA%\claudecost\ and
	// restart the app.
	cfgFile := *cfgPath
	if cfgFile == "" {
		if cand := filepath.Join(dataDir, "claudecost.json"); fileExists(cand) {
			cfgFile = cand
		}
	}
	cfg, err := pricing.Load(cfgFile)
	if err != nil {
		log.Println("config error:", err)
		return
	}
	if cfgFile != "" {
		log.Println("using config overrides from", cfgFile)
	}

	cachePath := filepath.Join(dataDir, "parsecache.gob")
	fingerprint := dataset.Fingerprint(version, &cfg)

	// Where Settings saves land: the file that was actually loaded, or the
	// default location next to the parse cache if none existed yet, so the
	// very first save from the window creates it rather than erroring.
	cfgSavePath := cfgFile
	if cfgSavePath == "" {
		cfgSavePath = filepath.Join(dataDir, "claudecost.json")
	}

	if alreadyRunning() {
		log.Println("another instance is already running; bringing it to the front")
		bringExistingToFront()
		return
	}

	wv2Dir := filepath.Join(dataDir, "wv2")
	_ = os.MkdirAll(wv2Dir, 0o755)

	a := &app{
		cfg:         cfg,
		seat:        *seat,
		monthsN:     *monthsN,
		sources:     []string(sources),
		htmlPath:    filepath.Join(dataDir, "dashboard.html"),
		interval:    *interval,
		cachePath:   cachePath,
		fingerprint: fingerprint,
		cfgPath:     cfgSavePath,
	}
	a.cache.Load(a.cachePath, a.fingerprint)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath: wv2Dir,
		WindowOptions: webview2.WindowOptions{
			Title:  windowTitle,
			Width:  1280,
			Height: 860,
			Center: true,
			IconId: 1, // matches the "#1" icon group winres/app.json embeds
		},
	})
	if w == nil {
		log.Println("could not create the WebView2 window; falling back to the default browser")
		runWithoutWindow(a)
		return
	}

	// A dashboard.html from a previous run is shown immediately, stale
	// "Snapshot taken" stamp and all: that stamp is already honest about
	// its own age, so no extra "refreshing" banner is needed on top of it.
	// A true first run has nothing to show yet, so it keeps the warming
	// page with the progress bar instead.
	hadDashboard := fileExists(a.htmlPath)
	if hadDashboard {
		w.Navigate(toFileURL(a.htmlPath))
	} else {
		w.SetHtml(warmingPageHTML())
	}

	// First collection happens off the UI goroutine so the page already on
	// screen (warming or the stale dashboard) can paint immediately. With a
	// warm parse cache this finishes in seconds, well before anyone digs
	// into a tab.
	go func() {
		if _, err := a.rebuild(progressReporter(w)); err != nil {
			log.Println("initial collection failed:", err)
			return
		}
		if hadDashboard {
			w.Dispatch(func() { w.Eval("window.ccReload ? ccReload() : location.reload()") })
			return
		}
		fileURL := toFileURL(a.htmlPath)
		w.Dispatch(func() { w.Navigate(fileURL) })
	}()

	if err := w.Bind("ccRefresh", func() {
		go func() {
			if _, err := a.rebuild(progressReporter(w)); err != nil {
				log.Println("refresh failed:", err)
				w.Dispatch(func() { w.Eval("window.ccRefreshFailed && window.ccRefreshFailed()") })
				return
			}
			w.Dispatch(func() { w.Eval("window.ccReload ? ccReload() : location.reload()") })
		}()
	}); err != nil {
		log.Println("could not bind ccRefresh:", err)
	}

	if err := w.Bind("ccSaveSettings", func(p settingsPayload) error {
		if err := a.applySettings(p); err != nil {
			return err
		}
		go func() {
			if _, err := a.rebuild(progressReporter(w)); err != nil {
				log.Println("rebuild after settings save failed:", err)
				w.Dispatch(func() { w.Eval("window.ccSettingsRebuildFailed && window.ccSettingsRebuildFailed()") })
				return
			}
			w.Dispatch(func() { w.Eval("window.ccReload ? ccReload() : location.reload()") })
		}()
		return nil
	}); err != nil {
		log.Println("could not bind ccSaveSettings:", err)
	}

	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := a.rebuild(progressReporter(w)); err != nil {
				log.Println("scheduled collection failed:", err)
				continue
			}
			// Reload the page so the new snapshot shows without a click.
			// ccReload (app chrome) stashes the active tab first so the
			// reload lands back on the same tab, not on Overview.
			w.Dispatch(func() { w.Eval("window.ccReload ? ccReload() : location.reload()") })
		}
	}()

	w.Run()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// progressReporter turns a Collect-shaped (done,total) callback into live
// updates on the page via ccProgress(done, total, etaSeconds|null). Call it
// fresh immediately before each rebuild so its elapsed-time clock, which the
// estimate is based on, starts at zero every time.
func progressReporter(w webview2.WebView) func(done, total int) {
	start := time.Now()
	return func(done, total int) {
		if total <= 0 {
			return
		}
		eta := "null"
		if done > 0 {
			perFile := time.Since(start) / time.Duration(done)
			remaining := perFile * time.Duration(total-done)
			eta = strconv.Itoa(int(remaining.Seconds()))
		}
		d, t := done, total
		w.Dispatch(func() {
			w.Eval(fmt.Sprintf("window.ccProgress && window.ccProgress(%d,%d,%s)", d, t, eta))
		})
	}
}

// ---------------------------------------------------------------------------
// app: the one rebuild path shared by startup, the ticker and Refresh now
// ---------------------------------------------------------------------------

type app struct {
	cfg      pricing.Config
	seat     string
	monthsN  int
	sources  []string
	htmlPath    string
	interval    time.Duration
	cachePath   string
	fingerprint string
	cfgPath     string

	cache    dataset.Cache
	mu       sync.Mutex
	building atomic.Bool
}

var errRebuildBusy = errors.New("a collection is already running")

// rebuild collects, renders and writes dashboard.html, reporting progress
// through progress if non-nil. It is guarded so only one collection runs at
// a time; a call that lands while another is already in flight (a tick
// firing mid-refresh) is skipped, not queued, and reports errRebuildBusy.
func (a *app) rebuild(progress func(done, total int)) (time.Time, error) {
	if !a.building.CompareAndSwap(false, true) {
		return time.Time{}, errRebuildBusy
	}
	defer a.building.Store(false)

	a.mu.Lock()
	defer a.mu.Unlock()

	payload, err := a.cache.Collect(&a.cfg, a.seat, a.monthsN, a.sources, progress)
	if err != nil {
		return time.Time{}, err
	}
	logSourceScan(a.cache.Sources, a.cache.Files)
	blob, err := json.Marshal(payload)
	if err != nil {
		return time.Time{}, err
	}
	html, err := report.Render(blob)
	if err != nil {
		return time.Time{}, err
	}

	built := time.Now()
	html = a.applyAppChrome(html)
	if err := os.WriteFile(a.htmlPath, []byte(html), 0o600); err != nil {
		return time.Time{}, err
	}
	// Persisted for every trigger (startup, the interval ticker, Refresh
	// now) since they all share this one rebuild path. A save failure never
	// fails the rebuild itself: the dashboard already wrote fine, and the
	// next run just falls back to a full re-parse.
	if err := a.cache.Save(a.cachePath, a.fingerprint); err != nil {
		log.Println("could not save parse cache:", err)
	}
	return built, nil
}

// logSourceScan writes one line per scanned source folder with the number of
// transcript files found under it, plus a total. Kept deliberately terse: it
// exists so a "my dashboard stopped at last month" report can be diagnosed
// from app.log alone, by seeing which folders were scanned and which were
// empty, without touching the user's machine.
func logSourceScan(sources, files []string) {
	for _, src := range sources {
		n := 0
		prefix := src + string(os.PathSeparator)
		for _, f := range files {
			if strings.HasPrefix(f, prefix) {
				n++
			}
		}
		log.Printf("source %s: %d transcript files", src, n)
	}
	log.Printf("sources scanned: %d, transcript files total: %d", len(sources), len(files))
}

// ---------------------------------------------------------------------------
// Settings: the Subscription block of the pricing config, editable from the
// window itself instead of by hand-editing claudecost.json.
// ---------------------------------------------------------------------------

// settingsPayload is the shape ccSaveSettings receives from the settings
// modal's Save button. Field names match the JS object literal exactly;
// go-webview2 unmarshals the JS argument straight into this struct.
type settingsPayload struct {
	YourSeat                  string  `json:"yourSeat"`
	MonthlySubscriptionEUR    float64 `json:"monthlySubscriptionEUR"`
	MonthlySubscriptionUSD    float64 `json:"monthlySubscriptionUSD"`
	SeatsPurchased            int     `json:"seatsPurchased"`
	StandardSeats             int     `json:"standardSeats"`
	PremiumSeats              int     `json:"premiumSeats"`
	StandardSeatPriceUSD      float64 `json:"standardSeatPriceUSD"`
	PremiumSeatPriceUSD       float64 `json:"premiumSeatPriceUSD"`
	UsageCreditsBalanceEUR    float64 `json:"usageCreditsBalanceEUR"`
	UsageCreditsSpentEUR      float64 `json:"usageCreditsSpentEUR"`
	UsageCreditsMonthlyCapEUR float64 `json:"usageCreditsMonthlyCapEUR"`
	CompanyConsumptionUSD     float64 `json:"companyConsumptionUSD"`
	OutputCostFactor          float64 `json:"outputCostFactor"`
	CalibratedOn              string  `json:"calibratedOn"`
	Window                    string  `json:"window"`
}

// applySettings writes p to claudecost.json (preserving any other keys
// already in that file, such as an unusual Prices override), then updates
// the running config and seat in memory. It resets the parse cache outright
// rather than just bumping the fingerprint: every already-cached session
// carries costs computed with the old Subscription, and there is no cheap
// way to tell which ones actually changed, so the honest fix is to treat
// this exactly like a version or config-file change and re-parse
// everything. The caller triggers the actual rebuild afterward.
func (a *app) applySettings(p settingsPayload) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	sub := pricing.Subscription{
		MonthlySubscriptionEUR:    p.MonthlySubscriptionEUR,
		MonthlySubscriptionUSD:    p.MonthlySubscriptionUSD,
		SeatsPurchased:            p.SeatsPurchased,
		Seats:                     map[string]int{"Standard": p.StandardSeats, "Premium": p.PremiumSeats},
		SeatPriceUSD:              map[string]float64{"Standard": p.StandardSeatPriceUSD, "Premium": p.PremiumSeatPriceUSD},
		UsageCreditsBalanceEUR:    p.UsageCreditsBalanceEUR,
		UsageCreditsSpentEUR:      p.UsageCreditsSpentEUR,
		UsageCreditsMonthlyCapEUR: p.UsageCreditsMonthlyCapEUR,
		CompanyConsumptionUSD:     p.CompanyConsumptionUSD,
		OutputCostFactor:          p.OutputCostFactor,
		CalibratedOn:              p.CalibratedOn,
		Window:                    p.Window,
	}
	if err := writeSubscriptionConfig(a.cfgPath, sub); err != nil {
		return err
	}

	a.cfg.Subscription = sub
	if p.YourSeat == "Standard" || p.YourSeat == "Premium" {
		a.seat = p.YourSeat
	}
	a.fingerprint = dataset.Fingerprint(version, &a.cfg)
	a.cache = dataset.Cache{}
	return nil
}

// writeSubscriptionConfig merges sub into the "subscription" key of the
// JSON file at path, leaving any other keys (an unusual Prices override,
// say) exactly as they were. A missing or unreadable existing file is
// treated as empty, not an error: this is very likely the first time
// anyone has saved settings from the window. Written via a .tmp file plus
// os.Rename, same pattern as the parse cache, so a crash mid-write never
// corrupts the real file.
func writeSubscriptionConfig(path string, sub pricing.Subscription) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &raw)
	}
	subBytes, err := json.MarshalIndent(sub, "", "  ")
	if err != nil {
		return err
	}
	raw["subscription"] = subBytes

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// escapeAttr is a minimal HTML attribute escape for the couple of text
// fields (CalibratedOn, Window) that land inside a value="..." attribute.
// Not a general-purpose escaper, just enough for values the user themselves
// typed into this same form a moment ago.
func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ---------------------------------------------------------------------------
// HTML chrome: turn the CLI's rendered page into the app window's page.
// The embedded template.html itself is never touched.
// ---------------------------------------------------------------------------

const warmingPageTemplate = `<!doctype html><title>Claude Cost</title><body style="font-family:sans-serif;background:#1f2733;
color:#eee;display:grid;place-items:center;height:100vh;margin:0;overflow:hidden">
<div style="text-align:center;min-width:320px">
 <div>Reading your session transcripts&hellip;</div>
 <div style="margin:14px auto 0;width:320px;height:8px;background:#334455;border-radius:4px;overflow:hidden">
  <div id="cc_warm_fill" style="width:0%;height:100%;background:#FFDD32;transition:width .2s"></div>
 </div>
 <div id="cc_warm_text" style="margin-top:8px;font-size:13px;color:#9fb4bd">Starting&hellip;</div>
 <div style="margin-top:22px;font-size:11px;color:#5b6b74">Claude Cost vAPP_VERSION</div>
</div>
<script>
function ccProgress(done,total,eta){
  if(total<=0) return;
  var pct = Math.round(done/total*100);
  var fill = document.getElementById('cc_warm_fill');
  if(fill) fill.style.width = pct+'%';
  var t = document.getElementById('cc_warm_text');
  if(t) t.textContent = done+' / '+total+' files ('+pct+'%)'+(eta!=null && eta>0 ? ', about '+eta+'s left' : '');
}
</script>
</body>`

func warmingPageHTML() string {
	return strings.Replace(warmingPageTemplate, "APP_VERSION", version, 1)
}

const cliRebuildNotice = `<b>This page is rebuilt every time you run claudecost.exe.</b> It reads your session transcripts live at each run, so refreshing is simply running it again: a new report is written and opened for you.`

func appRebuildNotice(interval time.Duration) string {
	return fmt.Sprintf(`<b>This window keeps itself current.</b> It opens straight to your last snapshot and quietly catches up in the background within moments. It also re-reads your session transcripts every %d minutes while open, whenever you press Refresh now (top right), and right after you save changes in Settings (the gear icon).`,
		int(interval/time.Minute))
}

// stampAnchor is the exact markup template.html renders for the "Snapshot
// taken" box in the header. Its own script fills it in by id, so it stays
// intact; we just wrap it together with our button so both sit in the same
// spot, top right, instead of adding a second, duplicate timestamp of our
// own at the bottom.
const stampAnchor = `<div class="stamp" id="stamp"></div>`

func (a *app) stampAreaHTML() string {
	return `<div style="display:flex;align-items:center;gap:12px">
 <span style="color:var(--pine-40);font-size:11px;opacity:.75;white-space:nowrap">v` + version + `</span>
 <span id="cc_progress" style="display:none;color:var(--pine-40);font-size:12px;white-space:nowrap"></span>
 <button id="cc_settings_btn" title="Subscription settings" style="padding:6px 10px;border:1px solid var(--pine-40);border-radius:6px;background:transparent;color:#fff;font:14px 'Inter','Segoe UI',sans-serif;cursor:pointer;white-space:nowrap">&#9881;</button>
 <button id="cc_btn" style="padding:6px 12px;border:1px solid var(--pine-40);border-radius:6px;background:transparent;color:#fff;font:13px 'Inter','Segoe UI',sans-serif;cursor:pointer;white-space:nowrap">Refresh now</button>
 ` + stampAnchor + `
</div>`
}

// appChromeStyle is a small CSS override, not a template edit: the Sessions
// tab's own scrollable table (".scroll.tall.with-filters", template.html's
// class, unique to that tab) is sized for a generic browser viewport. In our
// fixed 1280x860 window that leaves the page slightly taller than the
// window, so a second, outer scrollbar appears alongside the table's own
// one. Shrinking just that box's max-height keeps everything on one screen.
// Only the Sessions tab is affected; Months/Weeks/Days are untouched.
const appChromeStyle = `
<style>
.scroll.tall.with-filters{max-height:calc(100vh - 500px) !important}
</style>`

const appChromeScript = `
<script>
(function(){
  var btn = document.getElementById('cc_btn');
  if(!btn) return;
  var progressEl = document.getElementById('cc_progress');
  var hideTimer = null;

  window.ccProgress = function(done, total, eta){
    if(!progressEl || total<=0) return;
    var pct = Math.round(done/total*100);
    progressEl.style.display = 'inline';
    progressEl.textContent = pct+'% ('+done+'/'+total+')'+(eta!=null && eta>0 ? ', ~'+eta+'s left' : '');
    if(hideTimer) clearTimeout(hideTimer);
    if(done>=total){
      hideTimer = setTimeout(function(){ progressEl.style.display='none'; }, 800);
    }
  };

  // ccReload: reload the page for a fresh snapshot, landing back on the
  // tab the user was on. Tab state is in-memory only (see template.html's
  // tab navigation notes), so it is stashed in localStorage across the
  // reload. Used by the interval ticker, Refresh now, and Settings save.
  window.ccReload = function(){
    try{ localStorage.setItem('cc_tab', (typeof currentTab === 'function') ? currentTab() : 'overview'); }catch(e){}
    location.reload();
  };

  // Restore the stashed tab after a ccReload-driven reload.
  try{
    var savedTab = localStorage.getItem('cc_tab');
    if(savedTab){
      localStorage.removeItem('cc_tab');
      if(savedTab !== 'overview' && typeof showTab === 'function') showTab(savedTab);
    }
  }catch(e){}

  window.ccRefreshFailed = function(){
    btn.disabled = false;
    btn.textContent = 'Refresh failed – try again';
    btn.style.borderColor = '#ff8a65';
    btn.style.color = '#ff8a65';
  };

  btn.onclick = function(){
    btn.disabled = true;
    btn.style.borderColor = 'var(--pine-40)';
    btn.style.color = '#fff';
    btn.textContent = 'Rebuilding…';
    window.ccRefresh();
  };
})();
</script>`

func (a *app) applyAppChrome(html string) string {
	if n := strings.Count(html, cliRebuildNotice); n != 1 {
		log.Printf("warning: rebuild notice found %d times in the rendered template, expected 1", n)
	}
	html = strings.Replace(html, cliRebuildNotice, appRebuildNotice(a.interval), 1)

	if n := strings.Count(html, stampAnchor); n != 1 {
		log.Printf("warning: snapshot stamp found %d times in the rendered template, expected 1; Refresh now not placed", n)
	} else {
		html = strings.Replace(html, stampAnchor, a.stampAreaHTML(), 1)
	}

	if !strings.Contains(html, "</body>") {
		log.Println("warning: no </body> in the rendered template; chrome not injected")
		return html
	}
	return strings.Replace(html, "</body>", appChromeStyle+appChromeScript+a.settingsModalHTML()+"</body>", 1)
}

// settingsModalHTML renders the Settings overlay, pre-filled with the
// currently loaded subscription numbers and seat, so opening it always
// shows what the dashboard is actually using right now, not stale form
// defaults. Saving posts to ccSaveSettings (bound in main), which writes
// claudecost.json, resets the parse cache, and rebuilds in the background.
func (a *app) settingsModalHTML() string {
	sub := a.cfg.Subscription
	std := sub.Seats["Standard"]
	prem := sub.Seats["Premium"]
	stdPrice := sub.SeatPriceUSD["Standard"]
	premPrice := sub.SeatPriceUSD["Premium"]
	selected := func(seat string) string {
		if a.seat == seat {
			return " selected"
		}
		return ""
	}
	f := func(v float64) string { return fmt.Sprintf("%v", v) }
	n := func(v int) string { return fmt.Sprintf("%d", v) }

	return `
<div id="cc_settings_overlay" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,.55);z-index:1000;align-items:center;justify-content:center;font-family:'Inter','Segoe UI',sans-serif">
 <div style="background:#1f2733;color:#eee;border-radius:10px;padding:22px 26px;width:480px;max-height:86vh;overflow:auto;box-shadow:0 12px 40px rgba(0,0,0,.5)">
  <h3 style="margin:0 0 4px;font-size:16px">Subscription settings</h3>
  <p style="margin:0 0 16px;color:#9fb4bd;font-size:12px">Saved to claudecost.json in this window's data folder. Saving triggers a full re-read: cached sessions carry costs computed with the old numbers.</p>
  <div id="cc_settings_error" style="display:none;margin-bottom:12px;color:#ff8a65;font-size:12px"></div>
  <style>
   #cc_settings_overlay label{display:flex;flex-direction:column;gap:4px;font-size:12px;color:#c8d4d9}
   #cc_settings_overlay input,#cc_settings_overlay select{background:#111a24;border:1px solid var(--pine-40);border-radius:6px;color:#fff;padding:6px 8px;font-size:13px}
   #cc_settings_overlay .grid{display:grid;grid-template-columns:1fr 1fr;gap:10px 14px}
  </style>
  <div class="grid">
   <label>Your seat
    <select id="cc_s_yourSeat">
     <option value="Standard"` + selected("Standard") + `>Standard</option>
     <option value="Premium"` + selected("Premium") + `>Premium</option>
    </select></label>
   <div></div>
   <label>Monthly subscription (EUR)<input id="cc_s_subEUR" type="number" step="0.01" min="0" value="` + f(sub.MonthlySubscriptionEUR) + `"></label>
   <label>Monthly subscription (USD)<input id="cc_s_subUSD" type="number" step="0.01" min="0" value="` + f(sub.MonthlySubscriptionUSD) + `"></label>
   <label>Seats purchased<input id="cc_s_seatsTotal" type="number" step="1" min="0" value="` + n(sub.SeatsPurchased) + `"></label>
   <div></div>
   <label>Standard seats<input id="cc_s_seatsStd" type="number" step="1" min="0" value="` + n(std) + `"></label>
   <label>Premium seats<input id="cc_s_seatsPrem" type="number" step="1" min="0" value="` + n(prem) + `"></label>
   <label>Standard seat price (USD)<input id="cc_s_priceStd" type="number" step="0.01" min="0" value="` + f(stdPrice) + `"></label>
   <label>Premium seat price (USD)<input id="cc_s_pricePrem" type="number" step="0.01" min="0" value="` + f(premPrice) + `"></label>
   <label>Usage credits balance (EUR)<input id="cc_s_creditsBal" type="number" step="0.01" min="0" value="` + f(sub.UsageCreditsBalanceEUR) + `"></label>
   <label>Usage credits spent (EUR)<input id="cc_s_creditsSpent" type="number" step="0.01" min="0" value="` + f(sub.UsageCreditsSpentEUR) + `"></label>
   <label>Usage credits monthly cap (EUR)<input id="cc_s_creditsCap" type="number" step="0.01" min="0" value="` + f(sub.UsageCreditsMonthlyCapEUR) + `"></label>
   <label>Company consumption (USD)<input id="cc_s_companyUSD" type="number" step="0.01" min="0" value="` + f(sub.CompanyConsumptionUSD) + `"></label>
  </div>
  <details style="margin-top:14px">
   <summary style="cursor:pointer;color:#9fb4bd;font-size:12px">Advanced (recalibration)</summary>
   <div class="grid" style="margin-top:10px">
    <label>Output cost factor<input id="cc_s_factor" type="number" step="0.0001" min="0" value="` + f(sub.OutputCostFactor) + `"></label>
    <div></div>
    <label>Calibrated on<input id="cc_s_calibratedOn" type="text" value="` + escapeAttr(sub.CalibratedOn) + `"></label>
    <label>Window<input id="cc_s_window" type="text" value="` + escapeAttr(sub.Window) + `"></label>
   </div>
  </details>
  <div style="display:flex;justify-content:flex-end;gap:10px;margin-top:20px">
   <button id="cc_s_cancel" style="padding:6px 14px;border:1px solid var(--pine-40);border-radius:6px;background:transparent;color:#fff;font:13px 'Inter','Segoe UI',sans-serif;cursor:pointer">Cancel</button>
   <button id="cc_s_save" style="padding:6px 14px;border:1px solid #FFDD32;border-radius:6px;background:transparent;color:#FFDD32;font:13px 'Inter','Segoe UI',sans-serif;cursor:pointer">Save and reload</button>
  </div>
 </div>
</div>
<script>
(function(){
  var btn = document.getElementById('cc_settings_btn');
  var overlay = document.getElementById('cc_settings_overlay');
  if(!btn || !overlay) return;
  var errEl = document.getElementById('cc_settings_error');
  var saveBtn = document.getElementById('cc_s_save');

  btn.onclick = function(){ if(errEl) errEl.style.display='none'; overlay.style.display='flex'; };
  var cancelBtn = document.getElementById('cc_s_cancel');
  if(cancelBtn) cancelBtn.onclick = function(){ overlay.style.display='none'; };

  window.ccSettingsRebuildFailed = function(msg){
    document.body.style.opacity = '';
    if(saveBtn){ saveBtn.disabled = false; saveBtn.textContent = 'Save and reload'; }
    if(errEl){ errEl.textContent = msg || 'Rebuild failed after saving; your numbers were kept, try Refresh now.'; errEl.style.display = 'block'; }
  };

  if(saveBtn) saveBtn.onclick = function(){
    var num = function(id){ var v = parseFloat(document.getElementById(id).value); return isNaN(v) ? 0 : v; };
    var int = function(id){ var v = parseInt(document.getElementById(id).value, 10); return isNaN(v) ? 0 : v; };
    var payload = {
      yourSeat: document.getElementById('cc_s_yourSeat').value,
      monthlySubscriptionEUR: num('cc_s_subEUR'),
      monthlySubscriptionUSD: num('cc_s_subUSD'),
      seatsPurchased: int('cc_s_seatsTotal'),
      standardSeats: int('cc_s_seatsStd'),
      premiumSeats: int('cc_s_seatsPrem'),
      standardSeatPriceUSD: num('cc_s_priceStd'),
      premiumSeatPriceUSD: num('cc_s_pricePrem'),
      usageCreditsBalanceEUR: num('cc_s_creditsBal'),
      usageCreditsSpentEUR: num('cc_s_creditsSpent'),
      usageCreditsMonthlyCapEUR: num('cc_s_creditsCap'),
      companyConsumptionUSD: num('cc_s_companyUSD'),
      outputCostFactor: num('cc_s_factor'),
      calibratedOn: document.getElementById('cc_s_calibratedOn').value,
      window: document.getElementById('cc_s_window').value
    };
    saveBtn.disabled = true;
    saveBtn.textContent = 'Saving…';
    if(errEl) errEl.style.display = 'none';
    window.ccSaveSettings(payload).then(function(){
      saveBtn.textContent = 'Rebuilding…';
      document.body.style.opacity = '.6';
    }).catch(function(err){
      saveBtn.disabled = false;
      saveBtn.textContent = 'Save and reload';
      if(errEl){ errEl.textContent = String(err); errEl.style.display = 'block'; }
    });
  };
})();
</script>`
}

func toFileURL(path string) string {
	u := &url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}
	return u.String()
}

// ---------------------------------------------------------------------------
// Data dir, logging
// ---------------------------------------------------------------------------

func appDataDir() string {
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		return filepath.Join(lad, "claudecost")
	}
	// Very unlikely on a managed Windows machine; fall back rather than
	// write next to a possibly read-only exe.
	return filepath.Join(os.TempDir(), "claudecost")
}

func setupLog(dataDir string) {
	f, err := os.OpenFile(filepath.Join(dataDir, "claudecost-app.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

// ---------------------------------------------------------------------------
// Single instance and the WebView2-missing fallback
// ---------------------------------------------------------------------------

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// alreadyRunning claims a named mutex for the life of the process. Windows
// reports ERROR_ALREADY_EXISTS when another instance already holds it, even
// though the returned handle is otherwise valid; that is the documented way
// to detect this without a helper class.
func alreadyRunning() bool {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return false
	}
	_, err = windows.CreateMutex(nil, false, name)
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func bringExistingToFront() {
	title, err := windows.UTF16PtrFromString(windowTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	procSetForegroundWindow.Call(hwnd)
}

const (
	mbOK          = 0x00000000
	mbIconWarning = 0x00000030
)

// runWithoutWindow covers the very unlikely case of a managed machine
// missing the WebView2 runtime: collect once, open the result in the
// default browser, and say why there is no app window.
func runWithoutWindow(a *app) {
	if _, err := a.rebuild(nil); err != nil {
		log.Println("fallback collection failed:", err)
	}
	openInBrowser(a.htmlPath)
	showWebView2MissingMessage()
}

func openInBrowser(path string) {
	_ = exec.Command("cmd", "/c", "start", "", path).Start()
}

func showWebView2MissingMessage() {
	text, _ := windows.UTF16PtrFromString(
		"Claude Cost could not open its window because the WebView2 runtime is missing.\n" +
			"The dashboard was opened in your default browser instead. Installing the\n" +
			"WebView2 runtime (part of Microsoft Edge) will let the app window open normally.")
	caption, _ := windows.UTF16PtrFromString(windowTitle)
	_, _ = windows.MessageBox(0, text, caption, mbOK|mbIconWarning)
}
