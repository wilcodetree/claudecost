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
	version     = "0.3.1"
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
	interval := flag.Duration("interval", 30*time.Minute, "how often to re-read transcripts while the window is open")
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
			w.Dispatch(func() { w.Eval("location.reload()") })
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
			w.Dispatch(func() { w.Eval("location.reload()") })
		}()
	}); err != nil {
		log.Println("could not bind ccRefresh:", err)
	}

	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for range ticker.C {
			built, err := a.rebuild(progressReporter(w))
			if err != nil {
				log.Println("scheduled collection failed:", err)
				continue
			}
			stamp := built.Format("15:04")
			w.Dispatch(func() { w.Eval("ccStale('" + stamp + "')") })
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
	blob, err := json.Marshal(payload)
	if err != nil {
		return time.Time{}, err
	}
	html, err := report.Render(blob)
	if err != nil {
		return time.Time{}, err
	}

	built := time.Now()
	html = applyAppChrome(html, a.interval)
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
	return fmt.Sprintf(`<b>This window keeps itself current.</b> It opens straight to your last snapshot and quietly catches up in the background within moments. It also re-reads your session transcripts every %d minutes while open, and whenever you press Refresh now (top right).`,
		int(interval/time.Minute))
}

// stampAnchor is the exact markup template.html renders for the "Snapshot
// taken" box in the header. Its own script fills it in by id, so it stays
// intact; we just wrap it together with our button so both sit in the same
// spot, top right, instead of adding a second, duplicate timestamp of our
// own at the bottom.
const stampAnchor = `<div class="stamp" id="stamp"></div>`

func stampAreaHTML() string {
	return `<div style="display:flex;align-items:center;gap:12px">
 <span style="color:var(--pine-40);font-size:11px;opacity:.75;white-space:nowrap">v` + version + `</span>
 <span id="cc_progress" style="display:none;color:var(--pine-40);font-size:12px;white-space:nowrap"></span>
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

  window.ccStale = function(t){
    btn.disabled = false;
    btn.textContent = 'New data ('+t+') – Refresh';
    btn.style.borderColor = '#FFDD32';
    btn.style.color = '#FFDD32';
  };

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

func applyAppChrome(html string, interval time.Duration) string {
	if n := strings.Count(html, cliRebuildNotice); n != 1 {
		log.Printf("warning: rebuild notice found %d times in the rendered template, expected 1", n)
	}
	html = strings.Replace(html, cliRebuildNotice, appRebuildNotice(interval), 1)

	if n := strings.Count(html, stampAnchor); n != 1 {
		log.Printf("warning: snapshot stamp found %d times in the rendered template, expected 1; Refresh now not placed", n)
	} else {
		html = strings.Replace(html, stampAnchor, stampAreaHTML(), 1)
	}

	if !strings.Contains(html, "</body>") {
		log.Println("warning: no </body> in the rendered template; chrome not injected")
		return html
	}
	return strings.Replace(html, "</body>", appChromeStyle+appChromeScript+"</body>", 1)
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
