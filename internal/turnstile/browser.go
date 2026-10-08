package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// browserDebugHook, when set (tests / troubleshooting), receives a trace of
// every solver step. Production leaves it nil.
var browserDebug func(format string, args ...any)

// browserDiagHook, when set (tests / troubleshooting), receives the CDP
// connection and page session once a browser attempt is attached.
// Production leaves it nil.
var browserDiagHook func(conn *cdp, sid string)

// browserSetupHook, when set (tests / troubleshooting), runs after setup and
// before the target navigation — a place to inject extra test scripts.
// Production leaves it nil.
var browserSetupHook func(conn *cdp, sid string)

func debugf(format string, args ...any) {
	if browserDebug != nil {
		browserDebug(format, args...)
	}
}

// solveTokenBrowser gets a Turnstile token by driving the local browser named
// in [turnstile].browser_path over the Chrome DevTools Protocol — the same
// recipe as heartmore/cloudflare-solver (FlareSolverr): open the site, inject
// the widget, click it with trusted input and poll the callback. No Playwright
// driver, no Python, no Docker — the binary talks CDP directly.
func (s *Solver) solveTokenBrowser(ctx context.Context, site *config.Site, sitekey string, c *core) (string, error) {
	cfg := s.Config()
	if strings.TrimSpace(cfg.BrowserPath) == "" {
		return "", &Error{
			Msg:       `turnstile.browser_path is empty — point it at Chrome/Edge (provider = "browser")`,
			Retryable: false,
		}
	}
	if _, err := os.Stat(cfg.BrowserPath); err != nil {
		return "", &Error{Msg: "turnstile.browser_path: " + err.Error(), Retryable: false}
	}

	// one browser window at a time, whatever the sites do
	s.browserMu.Lock()
	defer s.browserMu.Unlock()

	attempts := cfg.Retries
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		token, err := s.browserAttempt(ctx, site, sitekey, cfg, c)
		if err == nil {
			return token, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = err
		if !Retryable(last) {
			return "", last
		}
		backoff := time.Duration(cfg.RetryBackoffSeconds*float64(attempt+1)) * time.Second
		if backoff <= 0 {
			backoff = time.Second
		}
		if !sleepCtx(ctx, backoff) {
			return "", ctx.Err()
		}
	}
	return "", last
}

func (s *Solver) browserAttempt(ctx context.Context, site *config.Site, sitekey string, cfg config.Turnstile, c *core) (string, error) {
	proxy := ""
	if c.proxy != "" && c.proxy != "direct" {
		if u, err := url.Parse(c.proxy); err == nil && u.User != nil {
			return "", &Error{
				Msg:       "browser solver: proxy credentials are not supported by Chrome — use an IP allow-list proxy",
				Retryable: false,
			}
		}
		proxy = c.proxy
	}
	locale := strings.TrimSpace(cfg.BrowserLocale)
	if locale == "" {
		locale = firstLocale(site.Language)
	}

	br, err := launchBrowser(ctx, strings.TrimSpace(cfg.BrowserPath), cfg, locale, proxy)
	if err != nil {
		return "", &Error{Msg: "browser launch failed: " + err.Error(), Retryable: true}
	}
	conn, err := dialCDP(ctx, br.wsURL, fmt.Sprintf("http://127.0.0.1:%d", br.port))
	if err != nil {
		br.Close()
		return "", &Error{Msg: "cdp dial failed: " + err.Error(), Retryable: true}
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = conn.call(shutCtx, "", "Browser.close", nil)
		cancel()
		conn.Close()
		br.Close()
	}()

	sid, err := attachPage(ctx, conn)
	if err != nil {
		return "", &Error{Msg: "cdp attach failed: " + err.Error(), Retryable: true}
	}
	if h := browserDiagHook; h != nil {
		h(conn, sid)
	}

	// Everything below runs before Page.navigate so the stealth script is in
	// place before the site's own scripts execute.
	metrics := map[string]any{
		"width": 1280, "height": 900, "deviceScaleFactor": 1, "mobile": false,
	}
	if cfg.BrowserHeadless {
		// headless reports a 800x600 screen — smaller than our own viewport,
		// an instant bot tell; present a normal desktop screen instead.
		metrics["screenWidth"] = 1920
		metrics["screenHeight"] = 1080
	}
	setup := []struct {
		method string
		params any
		fatal  bool
	}{
		{"Page.enable", nil, false},
		{"Runtime.enable", nil, false},
		{"Network.enable", nil, false},
		{"Emulation.setDeviceMetricsOverride", metrics, false},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": stealthJS}, false},
	}
	if tz := strings.TrimSpace(cfg.BrowserTimezone); tz != "" {
		setup = append(setup, struct {
			method string
			params any
			fatal  bool
		}{"Emulation.setTimezoneOverride", map[string]any{"timezoneId": tz}, true})
	}
	for _, step := range setup {
		if _, err := conn.call(ctx, sid, step.method, step.params); err != nil && step.fatal {
			return "", &Error{Msg: step.method + ": " + err.Error(), Retryable: false}
		}
	}
	if h := browserSetupHook; h != nil {
		h(conn, sid)
	}
	var uaParams map[string]any
	if cfg.BrowserHeadless {
		// Chromium reports "HeadlessChrome" (bot tell). Fix it via CDP before
		// the real navigation: UA-CH metadata is only readable in a secure
		// context, so hop to a neutral non-Cloudflare HTTPS endpoint first.
		var uerr error
		uaParams, uerr = fixHeadlessUA(ctx, conn, sid)
		if uerr != nil {
			debugf("headless UA fix skipped: %v", uerr)
		}
		// The Turnstile challenge runs inside a cross-origin iframe (OOPIF)
		// whose renderer does NOT inherit the main frame's device emulation —
		// the headless screen stays at 800x600 in there, an instant bot tell
		// (600010). Auto-attach to child frames and equip each one before it
		// is allowed to run.
		stopSub := watchSubTargets(ctx, conn, sid, uaParams, strings.TrimSpace(cfg.BrowserTimezone))
		defer stopSub()
	}
	navCtx, cancelNav := context.WithTimeout(ctx, 30*time.Second)
	target := strings.TrimRight(site.BaseURL, "/") + "/"
	_, err = conn.call(navCtx, sid, "Page.navigate", map[string]any{"url": target})
	cancelNav()
	if err != nil {
		return "", &Error{Msg: "navigate to " + target + ": " + err.Error(), Retryable: true}
	}
	return s.pollForToken(ctx, conn, sid, sitekey, cfg)
}

// fixHeadlessUA applies the UA rewrite only when the browser actually
// reports HeadlessChrome, bootstrapping via a neutral HTTPS origin first so
// navigator.userAgentData (secure-context only) becomes readable. It returns
// the Network.setUserAgentOverride params that were applied (nil when no
// rewrite was needed) so sub-frames can reuse them.
func fixHeadlessUA(ctx context.Context, conn *cdp, sid string) (map[string]any, error) {
	raw, err := conn.evaluate(ctx, sid, `navigator.userAgent`)
	if err != nil {
		return nil, err
	}
	var ua string
	if json.Unmarshal(raw, &ua) != nil {
		return nil, fmt.Errorf("bad userAgent probe: %s", raw)
	}
	if !strings.Contains(ua, "HeadlessChrome") {
		return nil, nil
	}
	// Read UA-CH metadata in a SEPARATE bootstrap tab: a secure context is
	// required for navigator.userAgentData, but the main tab must stay on
	// about:blank so the real navigation gets history.length=2 (headed
	// parity) and an empty document.referrer.
	for _, boot := range []string{
		"https://www.baidu.com/",
		"https://www.microsoft.com/",
	} {
		bootSid, closeBoot, err := openBootstrap(ctx, conn, boot, 8*time.Second)
		if err != nil {
			debugf("ua bootstrap %s: %v", boot, err)
			continue
		}
		params, err := overrideHeadlessUA(ctx, conn, sid, bootSid)
		closeBoot()
		if err != nil {
			return nil, err
		}
		return params, nil
	}
	return nil, errors.New("no secure bootstrap origin reachable")
}

// openBootstrap opens url in a throwaway tab, waits for its https document
// to commit and returns the attached session id plus a closer func.
func openBootstrap(ctx context.Context, conn *cdp, url string, wait time.Duration) (string, func(), error) {
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := conn.call(tctx, "", "Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return "", nil, err
	}
	var ct struct {
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(raw, &ct) != nil || ct.TargetID == "" {
		return "", nil, fmt.Errorf("bad createTarget reply: %s", raw)
	}
	closeBoot := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.call(cctx, "", "Target.closeTarget", map[string]any{"targetId": ct.TargetID})
		ccancel()
	}
	raw, err = conn.call(tctx, "", "Target.attachToTarget", map[string]any{
		"targetId": ct.TargetID, "flatten": true,
	})
	if err != nil {
		closeBoot()
		return "", nil, err
	}
	var at struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &at) != nil || at.SessionID == "" {
		closeBoot()
		return "", nil, fmt.Errorf("bad attachToTarget reply: %s", raw)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if raw, err := conn.evaluate(ctx, at.SessionID, `location.href`); err == nil {
			var href string
			if json.Unmarshal(raw, &href) == nil && strings.HasPrefix(href, "https://") {
				return at.SessionID, closeBoot, nil
			}
		}
		if !sleepCtx(ctx, 200*time.Millisecond) {
			closeBoot()
			return "", nil, ctx.Err()
		}
	}
	closeBoot()
	return "", nil, errors.New("document did not commit")
}

// overrideHeadlessUA rewrites "HeadlessChrome" to "Chrome" in the page's
// navigator.userAgent and in outgoing HTTP headers (via CDP), because
// Cloudflare treats the headless token as a bot signal. The Client-Hint
// metadata must be carried over explicitly: a bare UA override makes Chrome
// drop the sec-ch-ua* headers entirely, which is itself a bot tell.
// The applied override params are returned (nil when no rewrite happened).
func overrideHeadlessUA(ctx context.Context, conn *cdp, sid, bootSid string) (map[string]any, error) {
	raw, err := conn.evaluate(ctx, sid, `navigator.userAgent`)
	if err != nil {
		return nil, err
	}
	var ua string
	if json.Unmarshal(raw, &ua) != nil {
		return nil, fmt.Errorf("bad userAgent probe: %s", raw)
	}
	if !strings.Contains(ua, "HeadlessChrome") {
		return nil, nil
	}
	fixed := strings.Replace(ua, "HeadlessChrome", "Chrome", 1)
	params := map[string]any{"userAgent": fixed}
	if mraw, err := conn.evaluate(ctx, bootSid, uaMetadataExpr); err == nil {
		debugf("ua metadata raw: %s", mraw)
		var s string
		if json.Unmarshal(mraw, &s) == nil {
			var meta map[string]any
			if json.Unmarshal([]byte(s), &meta) == nil {
				params["userAgentMetadata"] = meta
			}
		}
	} else {
		debugf("ua metadata expr failed: %v", err)
	}
	if _, err := conn.call(ctx, sid, "Network.setUserAgentOverride", params); err != nil {
		delete(params, "userAgentMetadata")
		if _, err2 := conn.call(ctx, sid, "Network.setUserAgentOverride", params); err2 != nil {
			return nil, err
		}
		debugf("headless UA metadata override rejected (%v), plain override applied", err)
	}
	debugf("headless UA rewritten: %s", fixed)
	return params, nil
}

// screenPatchJS gives the challenge iframe a desktop-sized screen. CDP's
// device-metrics override is top-level-only ("Command can only be executed
// on top-level targets"), so the cross-origin OOPIF keeps the headless
// native 800x600 — an instant bot tell (600010). Values mirror what the same
// iframe reads headed: 1920x1080 with a 48px taskbar.
const screenPatchJS = `(() => {
  const fix = (name, val) => {
    try {
      const d = Object.getOwnPropertyDescriptor(Screen.prototype, name);
      if (d && d.get) {
        Object.defineProperty(Screen.prototype, name, {
          get: function() { return val; },
          configurable: true
        });
      }
    } catch (e) {}
  };
  fix("width", 1920);
  fix("height", 1080);
  fix("availWidth", 1920);
  fix("availHeight", 1032);
  try { window.__dsSp = 1; } catch (e) {}
})()`

// watchSubTargets auto-attaches to targets related to the page session —
// chiefly the cross-origin Turnstile challenge iframe — with
// waitForDebuggerOnStart so each child pauses BEFORE its first document
// script runs. The handler then applies what the main frame setup gives the
// top-level document (screen, UA, TZ — the OOPIF's own renderer never sees
// the main frame's emulation) and resumes it via runIfWaitingForDebugger.
// Returns a stop func.
func watchSubTargets(ctx context.Context, conn *cdp, sid string, uaParams map[string]any, tz string) func() {
	ch := make(chan subTargetEvent, 64)
	conn.setSubTargets(ch)
	if _, err := conn.call(ctx, sid, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
	}); err != nil {
		debugf("sub-target auto-attach: %v", err)
		conn.setSubTargets(nil)
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case ev := <-ch:
				if ev.method != "Target.attachedToTarget" {
					continue
				}
				var p struct {
					SessionID          string `json:"sessionId"`
					WaitingForDebugger bool   `json:"waitingForDebugger"`
					TargetInfo         struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"targetInfo"`
				}
				if json.Unmarshal(ev.params, &p) != nil || p.SessionID == "" {
					continue
				}
				cctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				if p.TargetInfo.Type == "iframe" {
					steps := []struct {
						method string
						params any
					}{
						{"Runtime.enable", nil},
						{"Network.enable", nil},
						{"Page.enable", nil},
						{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": screenPatchJS, "runImmediately": true}},
					}
					if uaParams != nil {
						steps = append(steps, struct {
							method string
							params any
						}{"Network.setUserAgentOverride", uaParams})
					}
					if tz != "" {
						steps = append(steps, struct {
							method string
							params any
						}{"Emulation.setTimezoneOverride", map[string]any{"timezoneId": tz}})
					}
					for _, st := range steps {
						if _, err := conn.call(cctx, p.SessionID, st.method, st.params); err != nil {
							debugf("sub-frame %s: %v", st.method, err)
						}
					}
					debugf("sub-frame patched (url=%.64s)", p.TargetInfo.URL)
				}
				if p.WaitingForDebugger {
					if _, err := conn.call(cctx, p.SessionID, "Runtime.runIfWaitingForDebugger", nil); err != nil {
						debugf("sub-target resume: %v", err)
					} else {
						debugf("sub-target resumed (type=%s)", p.TargetInfo.Type)
					}
				}
				cancel()
			}
		}
	}()
	return func() {
		conn.setSubTargets(nil)
		close(done)
	}
}

// uaMetadataExpr snapshots navigator.userAgentData (incl. high-entropy
// values) in the shape CDP expects for Emulation.UserAgentMetadata.
const uaMetadataExpr = `(() => {
  const base = {brands: [], fullVersionList: [], platform: "", platformVersion: "",
                architecture: "", bitness: "", model: "", mobile: false, wow64: false};
  const pick = h => JSON.stringify({
    brands: (h.brands || []).map(b => ({brand: String(b.brand), version: String(b.version)})),
    fullVersionList: (h.fullVersionList || []).map(b => ({brand: String(b.brand), version: String(b.version)})),
    platform: String(h.platform || ""), platformVersion: String(h.platformVersion || ""),
    architecture: String(h.architecture || ""), bitness: String(h.bitness || ""),
    model: String(h.model || ""), mobile: !!h.mobile, wow64: !!h.wow64});
  try {
    const uad = navigator.userAgentData;
    if (!uad) return JSON.stringify(base);
    if (!uad.getHighEntropyValues) {
      return JSON.stringify(Object.assign({}, base, {brands: uad.brands || [], mobile: !!uad.mobile}));
    }
    return uad.getHighEntropyValues(["architecture", "bitness", "model", "platform",
                                     "platformVersion", "fullVersionList", "wow64"])
      .then(h => pick(Object.assign({}, h, {brands: h.brands || uad.brands || [], mobile: !!uad.mobile})))
      .catch(() => JSON.stringify(Object.assign({}, base, {brands: uad.brands || [], mobile: !!uad.mobile})));
  } catch (e) { return JSON.stringify(base); }
})()`

// pollForToken waits for page load, deals with a Cloudflare interstitial,
// warms the page like a human, renders the widget and clicks until a token
// lands or the deadline passes.
func (s *Solver) pollForToken(ctx context.Context, conn *cdp, sid, sitekey string, cfg config.Turnstile) (string, error) {
	timeout := time.Duration(cfg.SolveTimeoutValue()) * time.Second
	deadline := time.Now().Add(timeout)

	var preDone, inited bool
	var clicks, reinitsUsed int
	var lastClick, lastReinit, loadingSince time.Time
	var lastState, lastTitle string
	var lastErr error // last poll/evaluate failure, surfaced on timeout

	const (
		firstClickDelay = 3500 * time.Millisecond
		clickSpacing    = 2500 * time.Millisecond
		maxClicks       = 6
		maxReinits      = 3
	)

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", &Error{
				Msg: fmt.Sprintf("browser solver timed out after %s (state=%q title=%q clicks=%d last_err=%v)",
					timeout, lastState, lastTitle, clicks, lastErr),
				Retryable: true,
			}
		}

		raw, err := conn.evaluate(ctx, sid, pollExpr)
		if err != nil {
			// frame detach / navigation races are normal — keep polling
			lastErr = err
			if !sleepCtx(ctx, 400*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}
		var st widgetState
		if json.Unmarshal(raw, &st) != nil {
			if !sleepCtx(ctx, 400*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}
		lastState, lastTitle = st.State, st.Title
		debugf("poll state=%q ready=%q url=%q challenge=%v hasInit=%v renderedAt=%d clicks=%d focus=%v vw=%d vh=%d rect=%+v err=%v lastErr=%v gl=%q",
			st.State, st.Ready, st.URL, st.Challenge, st.HasInit, st.RenderedAt, clicks, st.Focus, st.VW, st.VH, st.Rect, st.Error, lastErr, st.GL)

		// a token is definitive — check it before any other gate
		if st.Token != "" {
			return st.Token, nil
		}

		// Cloudflare interstitial: a clean browser walks through it alone.
		if st.Challenge {
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}
		// about:blank (the launch tab) is not the target page yet
		if !strings.HasPrefix(st.URL, "http") {
			if !sleepCtx(ctx, 300*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}
		// A document stuck on "loading" (slow proxy resources) should not
		// stall the solve forever — proceed once the DOM had 20s.
		if st.Ready != "interactive" && st.Ready != "complete" {
			if loadingSince.IsZero() {
				loadingSince = time.Now()
			}
			if time.Since(loadingSince) < 20*time.Second {
				if !sleepCtx(ctx, 300*time.Millisecond) {
					return "", ctx.Err()
				}
				continue
			}
		} else {
			loadingSince = time.Time{}
		}
		if !preDone {
			debugf("pre-interaction warm-up")
			preInteraction(ctx, conn, sid)
			preDone = true
			continue
		}
		if !inited || !st.HasInit {
			if _, err := conn.evaluate(ctx, sid, initExpr(sitekey, cfg.Action)); err == nil {
				if !inited {
					inited = true
					lastErr = nil
				} else {
					lastErr = errors.New("init evaluated but window.__tsInitDone is not visible in poll")
				}
			} else {
				lastErr = fmt.Errorf("init: %w", err)
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}

		// Widget rejected the challenge — render a fresh one a few times.
		if st.State == "error" || st.State == "render_failed" {
			if time.Since(lastReinit) < 1500*time.Millisecond {
				if !sleepCtx(ctx, 400*time.Millisecond) {
					return "", ctx.Err()
				}
				continue
			}
			ok, rerr := conn.evaluate(ctx, sid, `window.__tsReinit ? window.__tsReinit() : false`)
			if rerr == nil && string(ok) == "true" {
				reinitsUsed++
				lastReinit = time.Now()
			} else if st.State == "render_failed" || reinitsUsed >= maxReinits {
				return "", &Error{
					Msg:       fmt.Sprintf("turnstile widget failed (state=%q error=%q)", st.State, st.Error),
					Retryable: true,
				}
			} else {
				lastReinit = time.Now()
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return "", ctx.Err()
			}
			continue
		}

		// Trusted click on the checkbox, throttled so we never fight the widget.
		if st.Rect != nil && clicks < maxClicks && st.RenderedAt > 0 {
			sinceRender := time.Since(time.UnixMilli(st.RenderedAt))
			visible := st.Rect.X >= 0 && st.Rect.Y >= 0 && st.VW > 0 &&
				st.Rect.X+st.Rect.Width <= float64(st.VW)+2 &&
				st.Rect.Y+st.Rect.Height <= float64(st.VH)+2
			if visible && sinceRender >= firstClickDelay && time.Since(lastClick) >= clickSpacing {
				if err := trustedClick(ctx, conn, sid, st.Rect.X+30, st.Rect.Y+st.Rect.Height/2); err == nil {
					clicks++
					lastClick = time.Now()
				}
			}
		}
		if !sleepCtx(ctx, 400*time.Millisecond) {
			return "", ctx.Err()
		}
	}
}

type widgetState struct {
	Challenge  bool   `json:"challenge"`
	Ready      string `json:"ready"`
	HasInit    bool   `json:"hasInit"`
	State      string `json:"state"`
	Token      string `json:"token"`
	Error      string `json:"error"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	VW         int    `json:"vw"`
	VH         int    `json:"vh"`
	Focus      bool   `json:"focus"`
	GL         string `json:"gl"`
	RenderedAt int64  `json:"renderedAt"`
	Rect       *struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"rect"`
}

// pollExpr reads widget state; it also drives __tsEnsure so the widget is
// (re)created after a challenge redirect wiped the document body.
const pollExpr = `(() => {
  const o = {challenge:false, ready:"", hasInit:false, state:"", token:"", error:"",
             title:document.title, url:location.href, vw:window.innerWidth|0, vh:window.innerHeight|0,
             renderedAt:0, rect:null, focus:false};
  try {
    // interstitial markers only — the widget's own DOM must not trip this
    o.challenge = !!window._cf_chl_opt || !!document.querySelector('#challenge-form');
    o.ready = document.readyState;
    o.focus = document.hasFocus();
    try {
      const gc = document.createElement("canvas").getContext("webgl");
      if (gc) {
        const dbg = gc.getExtension("WEBGL_debug_renderer_info");
        o.gl = dbg ? String(gc.getParameter(dbg.UNMASKED_RENDERER_WEBGL)) : "";
      }
    } catch (e) { o.gl = "err"; }
    o.hasInit = !!window.__tsInitDone;
    if (o.hasInit) {
      o.state = String(window.__tsState || "");
      o.token = String(window.__tsToken || "");
      o.error = String(window.__tsError || "");
      o.renderedAt = window.__tsRenderedAt || 0;
      const el = document.getElementById("__ds_ts_widget");
      if (el) {
        const r = el.getBoundingClientRect();
        o.rect = {x:r.x, y:r.y, width:r.width, height:r.height};
      }
      if (!o.challenge && !o.token && o.state !== "error" && o.state !== "render_failed" &&
          o.ready !== "loading" && typeof window.__tsEnsure === "function") {
        window.__tsEnsure();
      }
    }
  } catch (e) { o.state = "poll_failed"; o.error = String(e); }
  return o;
})()`

// initExpr defines __tsEnsure/__tsReinit and renders the widget once
// challenges.cloudflare.com is loaded.
func initExpr(sitekey, action string) string {
	sk, _ := json.Marshal(sitekey)
	act, _ := json.Marshal(action)
	return strings.NewReplacer("__SITEKEY__", string(sk), "__ACTION__", string(act)).Replace(initTpl)
}

const initTpl = `(() => {
  if (window.__tsInitDone) return {ok:true, already:true};
  window.__tsInitDone = true;
  window.__tsState = "init";
  window.__tsToken = "";
  window.__tsError = "";
  window.__tsReinits = 0;
  window.__tsScript = null;
  window.__tsWidget = null;
  window.__tsWidgetReady = false;
  window.__tsApiJsFailedAt = 0;
  window.__tsRenderedAt = 0;
  window.__tsEnsure = function() {
    try {
      if (!window.turnstile) {
        const failedAt = window.__tsApiJsFailedAt || 0;
        const retried = failedAt && (Date.now() - failedAt > 2000);
        if (!window.__tsScript || retried) {
          if (window.__tsScript && window.__tsScript.parentNode) {
            window.__tsScript.parentNode.removeChild(window.__tsScript);
          }
          window.__tsScript = null;
          window.__tsApiJsFailedAt = 0;
          const s = document.createElement("script");
          s.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
          s.async = true;
          s.onerror = function() { window.__tsApiJsFailedAt = Date.now(); window.__tsState = "apijs_failed"; };
          (document.head || document.documentElement).appendChild(s);
          window.__tsScript = s;
          window.__tsState = "loading_apijs";
        }
        return;
      }
      if (window.__tsWidgetReady && !document.getElementById("__ds_ts_widget")) {
        window.__tsWidgetReady = false;
        window.__tsWidget = null;
        window.__tsState = "init";
      }
      if (window.__tsWidgetReady || !document.body) return;
      const holder = document.createElement("div");
      holder.id = "__ds_ts_widget";
      holder.style.cssText = "position:fixed;top:12px;left:12px;z-index:2147483647;" +
        "width:300px;height:65px;background:#fff;";
      document.body.appendChild(holder);
      window.__tsWidget = window.turnstile.render(holder, {
        sitekey: __SITEKEY__,
        action: __ACTION__,
        callback: function(t) { window.__tsToken = t; window.__tsState = "success"; },
        "error-callback": function(e) { window.__tsError = String(e); window.__tsState = "error"; },
        "expired-callback": function() { window.__tsToken = ""; window.__tsState = "expired"; }
      });
      window.__tsWidgetReady = true;
      window.__tsState = "rendered";
      window.__tsRenderedAt = Date.now();
    } catch (e) {
      window.__tsError = String(e);
      window.__tsState = "render_failed";
    }
  };
  window.__tsReinit = function() {
    if (window.__tsReinits >= 3) return false;
    window.__tsReinits++;
    try {
      if (window.__tsWidget != null && window.turnstile) window.turnstile.remove(window.__tsWidget);
    } catch (e) {}
    const old = document.getElementById("__ds_ts_widget");
    if (old && old.parentNode) old.parentNode.removeChild(old);
    window.__tsWidget = null;
    window.__tsWidgetReady = false;
    window.__tsToken = "";
    window.__tsError = "";
    window.__tsState = "init";
    window.__tsEnsure();
    return true;
  };
  window.__tsEnsure();
  return {ok:true};
})()`

// stealthJS runs before any page script (Page.addScriptToEvaluateOnNewDocument).
const stealthJS = `(() => {
  try {
    Object.defineProperty(Navigator.prototype, "webdriver",
      {get: function() { return undefined; }, configurable: true});
  } catch (e) {}
  try {
    if (!window.chrome) window.chrome = {};
    if (!window.chrome.runtime) window.chrome.runtime = {};
  } catch (e) {}
	  try {
	    const q = navigator.permissions && navigator.permissions.query;
	    if (q) {
	      navigator.permissions.query = function(desc) {
	        return q.call(navigator.permissions, desc).then(function(r) {
	          try {
	            if (r && r.state === "prompt" && desc && desc.name === "notifications") r.state = "granted";
	          } catch (e) {}
	          return r;
	        });
	      };
	    }
	  } catch (e) {}
	  try {
	    // headless/screen-emulated environments report availHeight === height;
	    // a real desktop has a taskbar, so emulate one (no-op when present).
	    const avDesc = Object.getOwnPropertyDescriptor(Screen.prototype, "availHeight");
	    if (avDesc && avDesc.get) {
	      Object.defineProperty(Screen.prototype, "availHeight", {
	        get: function() {
	          const av = avDesc.get.call(this);
	          const h = this.height;
	          return (h && av >= h) ? h - 48 : av;
	        },
	        configurable: true
	      });
	    }
	  } catch (e) {}
	})()`

// preInteraction moves the pointer and nudges the scroll wheel before the
// widget appears, so the page sees a real interaction history first.
func preInteraction(ctx context.Context, conn *cdp, sid string) {
	for _, p := range [][2]float64{{640, 320}, {420, 470}, {790, 255}} {
		if ctx.Err() != nil {
			return
		}
		_ = dispatchMouse(conn.call, ctx, sid, "mouseMoved", p[0], p[1], "none", 0, 0)
		if !sleepCtx(ctx, 150*time.Millisecond) {
			return
		}
	}
	_ = dispatchMouseWheelY(ctx, conn, sid, 640, 450, 200)
	if !sleepCtx(ctx, 250*time.Millisecond) {
		return
	}
	_ = dispatchMouseWheelY(ctx, conn, sid, 640, 450, -200)
}

func trustedClick(ctx context.Context, conn *cdp, sid string, x, y float64) error {
	if err := dispatchMouse(conn.call, ctx, sid, "mouseMoved", x, y, "none", 0, 0); err != nil {
		return err
	}
	if !sleepCtx(ctx, 80*time.Millisecond) {
		return ctx.Err()
	}
	if err := dispatchMouse(conn.call, ctx, sid, "mousePressed", x, y, "left", 1, 1); err != nil {
		return err
	}
	if !sleepCtx(ctx, 60*time.Millisecond) {
		return ctx.Err()
	}
	return dispatchMouse(conn.call, ctx, sid, "mouseReleased", x, y, "left", 0, 1)
}

type cdpCaller func(context.Context, string, string, any) (json.RawMessage, error)

func dispatchMouse(call cdpCaller, ctx context.Context, sid, kind string, x, y float64, button string, buttons, clickCount int) error {
	params := map[string]any{"type": kind, "x": x, "y": y, "pointerType": "mouse"}
	if kind != "mouseWheel" {
		params["button"] = button
		params["buttons"] = buttons
		params["clickCount"] = clickCount
	}
	_, err := call(ctx, sid, "Input.dispatchMouseEvent", params)
	return err
}

func dispatchMouseWheelY(ctx context.Context, conn *cdp, sid string, x, y float64, deltaY float64) error {
	_, err := conn.call(ctx, sid, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": deltaY, "pointerType": "mouse",
	})
	return err
}

// attachPage finds the browser's page target and attaches a flattened session.
func attachPage(ctx context.Context, conn *cdp) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		raw, err := conn.call(ctx, "", "Target.getTargets", nil)
		if err != nil {
			return "", err
		}
		var out struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfos"`
		}
		if err := json.Unmarshal(raw, &out); err == nil {
			for _, t := range out.TargetInfos {
				if t.Type != "page" {
					continue
				}
				attached, err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{
					"targetId": t.TargetID, "flatten": true,
				})
				if err != nil {
					return "", err
				}
				var res struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(attached, &res) == nil && res.SessionID != "" {
					return res.SessionID, nil
				}
			}
		}
		lastErr = errors.New("no page target yet")
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no page target appeared")
	}
	return "", lastErr
}

// ── browser process ────────────────────────────────────────────────

type browserProc struct {
	cmd     *exec.Cmd
	port    int
	wsURL   string
	tempDir string
	exited  chan struct{}
	waitErr error
	stderr  *tailBuffer
}

func launchBrowser(ctx context.Context, exe string, cfg config.Turnstile, locale, proxy string) (*browserProc, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	profile := strings.TrimSpace(cfg.BrowserUserDataDir)
	tempDir := ""
	if profile == "" {
		tempDir = filepath.Join(os.TempDir(), fmt.Sprintf("dsfree2api-ts-%d-%d", os.Getpid(), time.Now().UnixNano()))
		if err := os.MkdirAll(tempDir, 0o700); err != nil {
			return nil, err
		}
		profile = tempDir
	}
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		// Chrome ≥ 136 rejects the DevTools WebSocket handshake unless the
		// caller's origin is allow-listed (otherwise: "bad status" 403).
		"--remote-allow-origins=http://127.0.0.1:" + strconv.Itoa(port),
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		// Chromium/Edge re-launch themselves de-elevated when the parent runs
		// as Administrator, orphaning the real browser and breaking CDP;
		// keep the browser inside our process tree instead.
		"--do-not-de-elevate",
		"--disable-blink-features=AutomationControlled",
		"--disable-background-networking",
		"--disable-sync",
		// IsolateOrigins/site-per-process: without site isolation the
		// cross-origin Turnstile iframe may share the top-level renderer,
		// inheriting its device emulation (headless OOPIFs otherwise keep a
		// native 800x600 screen that Cloudflare reads as a bot tell).
		"--disable-features=Translate,MediaRouter,OptimizationHints,IsolateOrigins,site-per-process",
		"--disable-session-crashed-bubble",
		"--disable-restore-session-state",
		"--window-size=1280,900",
	}
	if locale != "" {
		args = append(args, "--lang="+locale)
	}
	if cfg.BrowserHeadless {
		args = append(args, "--headless=new")
	}
	if proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	args = append(args, "about:blank")

	cmd := exec.Command(exe, args...)
	stderr := &tailBuffer{max: 4096}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		if tempDir != "" {
			_ = os.RemoveAll(tempDir)
		}
		return nil, fmt.Errorf("start %q: %w", exe, err)
	}
	b := &browserProc{cmd: cmd, port: port, tempDir: tempDir, exited: make(chan struct{}), stderr: stderr}
	go func() {
		b.waitErr = cmd.Wait()
		close(b.exited)
	}()
	if err := b.waitDevTools(ctx); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *browserProc) waitDevTools(ctx context.Context) error {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A Windows browser launcher may hand off to a longer-lived process
		// and exit 0; that is not a failure while the DevTools endpoint is
		// still coming up, so the exit check happens only at timeout.
		resp, err := client.Get(endpoint)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var info struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &info) == nil && info.WebSocketDebuggerURL != "" {
				b.wsURL = info.WebSocketDebuggerURL
				return nil
			}
			lastErr = fmt.Errorf("devtools http %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return ctx.Err()
		}
	}
	select {
	case <-b.exited:
		return fmt.Errorf("browser exited early (%v): %s", b.waitErr, b.stderr.String())
	default:
	}
	return fmt.Errorf("devtools never came up: %v; stderr: %s", lastErr, b.stderr.String())
}

// Close kills the browser if it is still alive and drops the temp profile.
// On Windows the spawned launcher may have exited while the real browser
// lives on, so a DevTools close is attempted before giving up.
func (b *browserProc) Close() {
	select {
	case <-b.exited:
	default:
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		select {
		case <-b.exited:
		case <-time.After(3 * time.Second):
		}
	}
	b.closeViaDevTools(2 * time.Second)
	if b.tempDir != "" {
		for i := 0; i < 3; i++ {
			if err := os.RemoveAll(b.tempDir); err == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// closeViaDevTools asks the browser to shut down over the DevTools endpoint.
// It is the only handle left when the original launcher process has exited.
func (b *browserProc) closeViaDevTools(timeout time.Duration) {
	ws := b.wsURL
	if ws == "" {
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var info struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		if json.Unmarshal(body, &info) != nil || info.WebSocketDebuggerURL == "" {
			return
		}
		ws = info.WebSocketDebuggerURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := dialCDP(ctx, ws, fmt.Sprintf("http://127.0.0.1:%d", b.port))
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.call(ctx, "", "Browser.close", nil)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func firstLocale(acceptLanguage string) string {
	lang := strings.TrimSpace(strings.SplitN(acceptLanguage, ",", 2)[0])
	return lang
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// tailBuffer keeps the browser's stderr bounded for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := t.max - t.buf.Len(); room > 0 {
		if len(p) > room {
			t.buf.Write(p[:room])
		} else {
			t.buf.Write(p)
		}
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.Join(strings.Fields(t.buf.String()), " ")
	if len(s) > 600 {
		s = "..." + s[len(s)-600:]
	}
	return s
}
