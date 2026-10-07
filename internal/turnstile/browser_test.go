package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// fpExpr dumps an exhaustive JS-environment fingerprint (main world) so
// headless vs headed runs can be diffed field by field.
const fpExpr = `(() => {
  if (!location.href.startsWith("https://deepseek.")) return "";
  const out = {};
  const j = (v) => { try { const s = JSON.stringify(v); return s === undefined ? "undef" : s; } catch (e) { return "ERR"; } };
  const n = navigator;
  out.nav = {};
  ["appCodeName","appName","platform","vendor","vendorSub","product","productSub","language","webdriver","hardwareConcurrency","deviceMemory","maxTouchPoints","cookieEnabled","doNotTrack","pdfViewerEnabled","onLine","userAgent"].forEach(function(k){
    try { out.nav[k] = j(n[k]); } catch (e) { out.nav[k] = "ERR"; }
  });
  try { out.nav.languages = j(Array.from(n.languages || [])); } catch (e) {}
  try { out.nav.plugins = Array.from(n.plugins || []).map(function(p){ return p.name + "|" + p.filename; }).join(";"); } catch (e) { out.nav.plugins = "ERR"; }
  try { out.nav.mimeTypes = Array.from(n.mimeTypes || []).map(function(m){ return m.type + "|" + m.suffixes; }).join(";"); } catch (e) { out.nav.mimeTypes = "ERR"; }
  try { out.nav.connection = j(n.connection ? {e:n.connection.effectiveType, rtt:n.connection.rtt, dl:n.connection.downlink} : null); } catch (e) {}
  try { out.nav.getBattery = typeof n.getBattery; } catch (e) {}
  out.screen = {};
  ["width","height","availWidth","availHeight","colorDepth","pixelDepth"].forEach(function(k){
    try { out.screen[k] = j(screen[k]); } catch (e) {}
  });
  try { out.screen.orientation = j(screen.orientation && screen.orientation.type); } catch (e) {}
  out.win = {
    dpr: j(window.devicePixelRatio), iw: j(innerWidth), ih: j(innerHeight), ow: j(outerWidth), oh: j(outerHeight),
    sx: j(screenX), sy: j(screenY), chrome: j(typeof window.chrome),
    chromeRuntime: j(window.chrome ? typeof window.chrome.runtime : "none"),
    notif: (typeof Notification !== "undefined") ? Notification.permission : "none",
    histo: j(history.length), ref: j(document.referrer.slice(0,60)),
    vis: j(document.visibilityState), isTop: j(window.top === window),
    docMode: j(document.documentMode), compat: j(document.compatMode)
  };
  try { out.intl = j(Intl.DateTimeFormat().resolvedOptions()); } catch (e) { out.intl = "ERR"; }
  try { out.tzOffset = j(new Date().getTimezoneOffset()); } catch (e) {}
  try {
    const c = document.createElement("canvas");
    c.width = 32; c.height = 32;
    const g = c.getContext("2d");
    g.fillStyle = "#f0f"; g.fillRect(0,0,32,32);
    g.font = "14px Arial"; g.fillText("fp", 2, 20);
    out.canvas = c.toDataURL();
  } catch (e) { out.canvas = "ERR"; }
  try {
    const gc = document.createElement("canvas").getContext("webgl");
    const dbg = gc.getExtension("WEBGL_debug_renderer_info");
    out.gl = {
      vendor: String(gc.getParameter(dbg.UNMASKED_VENDOR_WEBGL)),
      rend: String(gc.getParameter(dbg.UNMASKED_RENDERER_WEBGL)),
      ver: String(gc.getParameter(gc.VERSION)),
      rendStr: String(gc.getParameter(gc.RENDERER)),
      maxTex: j(gc.getParameter(gc.MAX_TEXTURE_SIZE)),
      maxVB: j(gc.getParameter(gc.MAX_VERTEX_ATTRIBS)),
      exts: j((gc.getSupportedExtensions() || []).length)
    };
  } catch (e) { out.gl = "ERR"; }
  try {
    const ac = new (window.AudioContext || window.webkitAudioContext)();
    out.audio = { sr: j(ac.sampleRate), base: j(ac.baseLatency), out: j(ac.outputLatency) };
    ac.close();
  } catch (e) { out.audio = "ERR"; }
  try { out.fonts = j(document.fonts.size); } catch (e) {}
  try { out.voices = j((window.speechSynthesis.getVoices() || []).length); } catch (e) {}
  try {
    out.mm = {
      coarse: matchMedia("(pointer: coarse)").matches,
      anyCoarse: matchMedia("(any-pointer: coarse)").matches,
      dark: matchMedia("(prefers-color-scheme: dark)").matches,
      reduce: matchMedia("(prefers-reduced-motion: reduce)").matches,
      hover: matchMedia("(any-hover: hover)").matches
    };
  } catch (e) { out.mm = "ERR"; }
  try {
    out.leaks = Object.getOwnPropertyNames(window).filter(function(k){
      return /_cdc_|cdc_|webdriver|selenium|puppeteer|phantom|playwright|__nightmare|callPhantom|domAutomation|__selenium/.test(k);
    });
  } catch (e) { out.leaks = "ERR"; }
  try { out.cookie = j(document.cookie.slice(0,80)); } catch (e) {}
  try { out.lsLen = j(localStorage.length); } catch (e) { out.lsLen = "ERR"; }
  return JSON.stringify(out);
})()`

// netTapJS logs challenge-platform request/response bodies to the console so
// the TRACE hook can print the full rch conversation.
const netTapJS = `(() => {
  const ok = u => /cdn-cgi|challenges\.cloudflare/.test(String(u));
  const log = (tag, u, extra) => { try { console.log("NET|" + tag + "|" + u + "|" + extra); } catch (e) {} };
  try {
    const odce = Document.prototype.createElement;
    Document.prototype.createElement = function(t) {
      const r = odce.apply(this, arguments);
      try {
        const tt = String(t).toLowerCase();
        if (tt === "iframe" || tt === "object" || tt === "embed") {
          const src = (typeof arguments[1] === "object" && arguments[1] && arguments[1].src) || "";
          log("create" + tt, src, new Error().stack ? String(new Error().stack).split("\n").slice(1,4).join(" <- ") : "");
          if (tt === "iframe") {
            const d = Object.getOwnPropertyDescriptor(HTMLIFrameElement.prototype, "src");
            if (d && d.set) {
              Object.defineProperty(r, "src", {
                configurable: true,
                get: function() { return d.get.call(this); },
                set: function(v) { try { log("iframesrc", String(v).slice(0,140), ""); } catch (e) {} d.set.call(this, v); }
              });
            }
          }
        }
      } catch (e) {}
      return r;
    };
  } catch (e) {}
  try {
    const oal = window.addEventListener;
    window.addEventListener = function(type, fn, opts) {
      if (String(type) === "message" && typeof fn === "function") {
        const wrapped = function(ev) {
          try {
            const d = ev.data;
            if (d && typeof d === "object") {
              log("msg", (d.event || d.type || "obj") + "|" + JSON.stringify(d).slice(0, 600), String(ev.origin || "").slice(0, 80));
            } else if (typeof d === "string" && d.length < 400) {
              log("msg", d.slice(0, 400), String(ev.origin || "").slice(0, 80));
            }
          } catch (e) {}
          return fn.apply(this, arguments);
        };
        return oal.call(this, type, wrapped, opts);
      }
      return oal.apply(this, arguments);
    };
  } catch (e) {}
  try {
    const of = window.fetch;
    if (of) {
      window.fetch = function(input, init) {
        const u = (typeof input === "string") ? input : (input && input.url) || "";
        if (!ok(u)) return of.apply(this, arguments);
        try {
          let hdr = "";
          if (init && init.headers) { hdr = JSON.stringify(init.headers); }
          let body = "";
          if (init && init.body) { body = String(init.body).slice(0, 700); }
          log("freq", (init && init.method || "GET") + "|" + hdr.slice(0, 400) + "|" + body, u);
        } catch (e) {}
        const p = of.apply(this, arguments);
        try {
          p.then(resp => {
            const c = resp.clone();
            c.text().then(t => log("fresp", resp.status + "|" + t.slice(0, 900)), () => {});
            try { log("fhdr", String(c.headers).slice(0, 300), u); } catch (e) {}
          }, () => {});
        } catch (e) {}
        return p;
      };
    }
  } catch (e) {}
  try {
    const XP = XMLHttpRequest.prototype;
    const oopen = XP.open, osend = XP.send;
    XP.open = function(m, u) { this.__tm = m; this.__tu = u; return oopen.apply(this, arguments); };
    XP.send = function(b) {
      if (ok(this.__tu)) {
        try {
          this.setRequestHeader = function(k, v) {
            try { log("xhdr", this.__tu, k + ": " + v); } catch (e) {}
            return XMLHttpRequest.prototype.setRequestHeader.apply(this, arguments);
          };
        } catch (e) {}
        try {
          this.addEventListener("load", () => {
            try {
              log("xresp", this.status + "|" + this.getAllResponseHeaders().replace(/\n/g, ";").slice(0, 300), this.__tu);
              log("xbody", String(this.responseText).slice(0, 900), this.__tu);
            } catch (e) {}
          });
          log("xreq", this.__tm + "|" + (b ? String(b).slice(0, 700) : ""), this.__tu);
        } catch (e) {}
      }
      return osend.apply(this, arguments);
    };
  } catch (e) {}
})()`

// domExpr reports the widget/iframe situation in the main document (shadow
// DOM included) so we can tell where the challenge actually executes.
const domExpr = `(() => {
  if (!location.href.startsWith("https://deepseek.")) return "";
  const out = { winLen: window.length, iframes: [], shadowRoots: [], body: "" };
  const walkRoot = (root, depth) => {
    if (depth > 6 || !root.querySelectorAll) return;
    root.querySelectorAll("iframe").forEach(f => {
      let u = f.src || "";
      if (!u) { try { u = f.contentWindow.location.href; } catch (e) { u = "no-src-xo"; } }
      out.iframes.push(String(u).slice(0, 120));
    });
    root.querySelectorAll("*").forEach(el => {
      if (el.shadowRoot) {
        out.shadowRoots.push((el.tagName + (el.id ? "#" + el.id : "")) + ":open");
        walkRoot(el.shadowRoot, depth + 1);
      } else if (el.attachShadow && Object.keys(el).some(k => k.indexOf("shadow") >= 0)) {
        out.shadowRoots.push((el.tagName + (el.id ? "#" + el.id : "")) + ":closed?");
      }
    });
  };
  walkRoot(document, 0);
  try {
    const w = document.getElementById("__ds_ts_widget");
    if (w) {
      const own = Object.getOwnPropertyNames(w).filter(k => /shadow/i.test(k));
      out.widget = (w.shadowRoot ? "open" : (own.length ? own.join(",") : "none"));
    } else { out.widget = "no-widget"; }
  } catch (e) { out.widget = "err:" + e; }
  try { out.body = Array.from(document.body.children).map(e => e.tagName + (e.id ? "#" + e.id : "")).join(","); } catch (e) {}
  return JSON.stringify(out);
})()`

func providerSite() *config.Site {
	return &config.Site{
		Code: "de", BaseURL: "https://deepseek.de",
		Language: "de-DE,de;q=0.9,en-US;q=0.8,en;q=0.7",
	}
}

func TestSolveTokenProviderManual(t *testing.T) {
	solver := New(config.Turnstile{Enabled: true, Provider: "manual", Retries: 1}, httpx.DefaultUserAgent)
	_, err := solver.solveToken(context.Background(), providerSite(), "0xdead", solver.coreFor(""))
	if err == nil {
		t.Fatal("manual provider must not solve")
	}
	if Retryable(err) {
		t.Errorf("manual failure should be terminal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "manual") {
		t.Errorf("error should mention the provider: %v", err)
	}
}

func TestSolveTokenProviderBrowserNeedsPath(t *testing.T) {
	solver := New(config.Turnstile{Enabled: true, Provider: "browser", Retries: 1}, httpx.DefaultUserAgent)
	_, err := solver.solveToken(context.Background(), providerSite(), "0xdead", solver.coreFor(""))
	if err == nil || Retryable(err) {
		t.Fatalf("empty browser_path should fail fast, got: %v", err)
	}
	if !strings.Contains(err.Error(), "browser_path") {
		t.Errorf("error should mention browser_path: %v", err)
	}

	solver.SetConfig(config.Turnstile{
		Enabled: true, Provider: "browser", Retries: 1,
		BrowserPath: filepath.Join(t.TempDir(), "missing.exe"),
	})
	_, err = solver.solveToken(context.Background(), providerSite(), "0xdead", solver.coreFor(""))
	if err == nil || Retryable(err) {
		t.Fatalf("missing browser executable should fail fast, got: %v", err)
	}
}

func TestSolveTokenProviderUnknown(t *testing.T) {
	solver := New(config.Turnstile{Enabled: true, Provider: "captcha", Retries: 1}, httpx.DefaultUserAgent)
	_, err := solver.solveToken(context.Background(), providerSite(), "0xdead", solver.coreFor(""))
	if err == nil || Retryable(err) {
		t.Fatalf("unknown provider should fail fast, got: %v", err)
	}
	if !strings.Contains(err.Error(), "captcha") {
		t.Errorf("error should echo the provider: %v", err)
	}
}

// TestBrowserSolveLive drives the real browser against deepseek.de. Opt in:
//
//	DSFREE_BROWSER_E2E=1 go test ./internal/turnstile -run Live -v
//
// Optional: DSFREE_BROWSER_E2E_EXE (defaults to Edge, then Chrome) and the
// proxy/site come from ../../config.toml.
func TestBrowserSolveLive(t *testing.T) {
	if os.Getenv("DSFREE_BROWSER_E2E") == "" {
		t.Skip("set DSFREE_BROWSER_E2E=1 to run the live browser solve")
	}
	cfg, err := config.Load("../../config.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	exe := os.Getenv("DSFREE_BROWSER_E2E_EXE")
	if exe == "" {
		exe = cfg.Turnstile.BrowserPath
	}
	for _, candidate := range []string{
		exe,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	} {
		if candidate != "" {
			if _, err := os.Stat(candidate); err == nil {
				exe = candidate
				break
			}
		}
	}
	if exe == "" {
		t.Skip("no browser executable found — set DSFREE_BROWSER_E2E_EXE")
	}

	ts := cfg.Turnstile
	ts.Enabled = true
	ts.Provider = config.ProviderBrowser
	ts.BrowserPath = exe
	ts.Retries = 1
	ts.TimeoutSeconds = 30 // faster feedback while diagnosing
	ts.BrowserHeadless = os.Getenv("DSFREE_BROWSER_E2E_HEADLESS") != ""
	solver := New(ts, cfg.Upstream.UserAgent)

	browserDebug = func(format string, args ...any) {
		t.Logf(format, args...)
	}
	defer func() { browserDebug = nil }()

	if os.Getenv("DSFREE_BROWSER_E2E_TRACE") != "" {
		type diag struct {
			c   *cdp
			sid string
		}
		var logMu sync.Mutex
		finished := false
		logf := func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			if finished {
				return
			}
			t.Logf(format, args...)
		}
		defer func() {
			logMu.Lock()
			finished = true
			logMu.Unlock()
		}()
		done := make(chan struct{})
		defer close(done)
		type respRef struct{ id, url, sess string }
		pendingResp := make(chan respRef, 16)
		type fetchRef struct{ id, url, sess, method string }
		pendingFetch := make(chan fetchRef, 32)
		rchFrames := make(chan string, 8)

		diagCh := make(chan diag, 1)
		browserDiagHook = func(c *cdp, s string) {
			select {
			case diagCh <- diag{c: c, sid: s}:
			default:
			}
		}
		defer func() { browserDiagHook = nil }()
		browserSetupHook = func(c *cdp, s string) {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			c.call(cctx, s, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": netTapJS})
			_, ferr := c.call(cctx, s, "Fetch.enable", map[string]any{
				"patterns": []any{
					map[string]any{"urlPattern": "*cdn-cgi/challenge-platform*", "requestStage": "Response"},
				},
			})
			if ferr != nil {
				logf("Fetch.enable err=%v", ferr)
			}
			ccancel()
		}
		defer func() { browserSetupHook = nil }()
		seenCtx := sync.Map{}
		cfCtx := make(chan int64, 16)
		cdpEventHook = func(session, method string, params json.RawMessage) {
			switch method {
			case "Network.requestWillBeSent":
				var p struct {
					Request struct {
						URL     string            `json:"url"`
						Method  string            `json:"method"`
						Headers map[string]string `json:"headers"`
					} `json:"request"`
					Type        string `json:"type"`
					FrameID     string `json:"frameId"`
					DocumentURL string `json:"documentURL"`
					Initiator   struct {
						Type string `json:"type"`
					} `json:"initiator"`
				}
				if json.Unmarshal(params, &p) == nil {
					ua := p.Request.Headers["User-Agent"]
					if strings.Contains(p.Request.URL, "cdn-cgi/challenge-platform") || strings.Contains(ua, "HeadlessChrome") {
						keys := make([]string, 0, len(p.Request.Headers))
						for k := range p.Request.Headers {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						var b strings.Builder
						for _, k := range keys {
							fmt.Fprintf(&b, "%s=%q ", k, p.Request.Headers[k])
						}
						logf("CHAL %s %s headlessUA=%v type=%s init=%s frame=%s doc=%s %s",
							p.Request.Method, p.Request.URL, strings.Contains(ua, "HeadlessChrome"),
							p.Type, p.Initiator.Type, p.FrameID, p.DocumentURL, b.String())
						if p.Type == "Document" && p.FrameID != "" && strings.Contains(p.Request.URL, "/rch/") {
							select {
							case rchFrames <- p.FrameID:
							default:
							}
						}
					}
				}
			case "Network.responseReceived":
				var p struct {
					RequestID string `json:"requestId"`
					Response  struct {
						URL          string            `json:"url"`
						Status       int               `json:"status"`
						Protocol     string            `json:"protocol"`
						ConnectionID any               `json:"connectionId"`
						Headers      map[string]string `json:"headers"`
						SetCookie    string            `json:"setCookie"`
					} `json:"response"`
				}
				if json.Unmarshal(params, &p) == nil && (strings.Contains(p.Response.URL, "cdn-cgi") || strings.Contains(p.Response.URL, "challenges.cloudflare")) {
					logf("RESP %d proto=%s conn=%v sess=%s setCookie=%q %s", p.Response.Status, p.Response.Protocol, p.Response.ConnectionID, session, p.Response.SetCookie, p.Response.URL)
					keys := make([]string, 0, len(p.Response.Headers))
					for k := range p.Response.Headers {
						if !strings.EqualFold(k, "Content-Security-Policy") && !strings.EqualFold(k, "Date") {
							keys = append(keys, k)
						}
					}
					sort.Strings(keys)
					var b strings.Builder
					for _, k := range keys {
						v := p.Response.Headers[k]
						if len(v) > 200 {
							v = v[:200] + "..."
						}
						fmt.Fprintf(&b, "%s=%q ", k, v)
					}
					logf("RESPH %s %s", p.Response.URL, b.String())
					select {
					case pendingResp <- respRef{id: p.RequestID, url: p.Response.URL, sess: session}:
					default:
					}
				}
			case "Runtime.executionContextCreated":
				var p struct {
					ExecutionContextID int64  `json:"executionContextId"`
					Origin             string `json:"origin"`
				}
				if json.Unmarshal(params, &p) == nil && strings.Contains(p.Origin, "cloudflare") {
					if _, loaded := seenCtx.LoadOrStore(p.ExecutionContextID, true); !loaded {
						logf("CFCTX id=%d origin=%s", p.ExecutionContextID, p.Origin)
						select {
						case cfCtx <- p.ExecutionContextID:
						default:
						}
					}
				}
			case "Fetch.requestPaused":
				var p struct {
					RequestID string `json:"requestId"`
					Request   struct {
						URL    string `json:"url"`
						Method string `json:"method"`
					} `json:"request"`
					ResponseStatusCode int `json:"responseStatusCode"`
				}
				if json.Unmarshal(params, &p) == nil && p.RequestID != "" {
					select {
					case pendingFetch <- fetchRef{id: p.RequestID, url: p.Request.URL, sess: session, method: p.Request.Method}:
					default:
						logf("FETCH channel full, dropping %s", p.Request.URL)
					}
				}
			case "Runtime.consoleAPICalled":
				var p struct {
					Type string `json:"type"`
					Args []struct {
						Value json.RawMessage `json:"value"`
					} `json:"args"`
				}
				if json.Unmarshal(params, &p) == nil && len(p.Args) > 0 {
					var s string
					if json.Unmarshal(p.Args[0].Value, &s) == nil && strings.HasPrefix(s, "NET|") {
						logf("CONSOLE %s", s)
					}
				}
			case "Target.attachedToTarget":
				logf("ATTACH params=%s", params)
			}
		}
		defer func() { cdpEventHook = nil }()
		type frT struct {
			Frame struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"frame"`
			ChildFrames []frT `json:"childFrames"`
		}
		go func() {
			var d diag
			select {
			case d = <-diagCh:
			case <-done:
				return
			}
			probed := map[string]bool{}
			logf("MAINSID %s", d.sid)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case f := <-pendingFetch:
					func() {
						fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer fcancel()
						raw, err := d.c.call(fctx, f.sess, "Fetch.getResponseBody", map[string]any{"requestId": f.id})
						if err != nil {
							logf("FCHBODY %s %s err=%v", f.method, f.url, err)
						} else {
							var bo struct {
								Body     string `json:"body"`
								Base64   bool   `json:"base64Encoded"`
								MIMEType string `json:"mimeType"`
							}
							if json.Unmarshal(raw, &bo) == nil {
								body := bo.Body
								if bo.Base64 {
									body = "(b64)" + body
								}
								if strings.Contains(f.url, "/rch/") && bo.Body != "" && !probed["rchjs:"+f.url] {
									probed["rchjs:"+f.url] = true
									n := 0
									for k := range probed {
										if strings.HasPrefix(k, "rchfile:") {
											n++
										}
									}
									probed[fmt.Sprintf("rchfile:%d", n)] = true
									dst := filepath.Join(os.TempDir(), fmt.Sprintf("turnstile_rch_%s_%02d.txt", map[bool]string{true: "hl", false: "hd"}[os.Getenv("DSFREE_BROWSER_E2E_HEADLESS") != ""], n))
									if err := os.WriteFile(dst, []byte(bo.Body), 0o644); err == nil {
										logf("RCHFILE %d bytes -> %s", len(bo.Body), dst)
									}
								}
								logf("FCHBODY mime=%s %s %s %s", bo.MIMEType, f.method, f.url, body[:min(600, len(body))])
							}
						}
						rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer rcancel()
						if _, err := d.c.call(rctx, f.sess, "Fetch.continueResponse", map[string]any{"requestId": f.id}); err != nil {
							if _, err2 := d.c.call(rctx, f.sess, "Fetch.continueRequest", map[string]any{"requestId": f.id}); err2 != nil {
								logf("FETCH continue failed id=%s: %v / %v", f.id, err, err2)
							}
						}
					}()
				case r := <-pendingResp:
					time.Sleep(300 * time.Millisecond)
					useSid := r.sess
					if useSid == "" {
						useSid = d.sid
					}
					bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
					braw, berr := d.c.call(bctx, useSid, "Network.getResponseBody", map[string]any{"requestId": r.id})
					bcancel()
					if berr != nil {
						logf("RCHBODY %s err=%v", r.url, berr)
						continue
					}
					var bo struct {
						Body     string `json:"body"`
						Base64   bool   `json:"base64Encoded"`
						MIMEType string `json:"mimeType"`
					}
					if json.Unmarshal(braw, &bo) == nil {
						if strings.Contains(r.url, "api.js") && !probed["apijs"] && bo.Body != "" {
							probed["apijs"] = true
							dst := filepath.Join(os.TempDir(), "turnstile_api_js.txt")
							if err := os.WriteFile(dst, []byte(bo.Body), 0o644); err == nil {
								logf("APIJS saved %d bytes -> %s", len(bo.Body), dst)
							}
						}
						body := bo.Body
						if bo.Base64 {
							body = "(b64)" + body[:min(400, len(body))]
						}
						logf("RCHBODY mime=%s %s %s", bo.MIMEType, r.url, body[:min(900, len(body))])
					}
				case fid := <-rchFrames:
					go func() {
						var last string
						for i := 0; i < 15; i++ {
							select {
							case <-done:
								return
							default:
							}
							time.Sleep(700 * time.Millisecond)
							func() {
								wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
								defer wcancel()
								raw, err := d.c.call(wctx, d.sid, "Page.createIsolatedWorld", map[string]any{
									"frameId": fid, "worldName": "dsrchprobe",
								})
								if err != nil {
									if i == 0 {
										logf("RCHFRAME %s world err=%v", fid, err)
									}
									return
								}
								var w struct {
									ExecutionContextID int64 `json:"executionContextId"`
								}
								if json.Unmarshal(raw, &w) != nil || w.ExecutionContextID == 0 {
									return
								}
								eraw, eerr := d.c.call(wctx, d.sid, "Runtime.evaluate", map[string]any{
									"expression": `JSON.stringify({href:(location.href||"").slice(0,95), rs:document.readyState,
										focus:document.hasFocus(), vis:document.visibilityState,
										inner:[innerWidth,innerHeight], win:[window.outerWidth,window.outerHeight],
										wd:String(navigator.webdriver), ua:navigator.userAgent.slice(-24)})`,
									"returnByValue": true,
									"contextId":     w.ExecutionContextID,
								})
								if eerr != nil {
									return
								}
								var out struct {
									Result struct {
										Value json.RawMessage `json:"value"`
									} `json:"result"`
								}
								if json.Unmarshal(eraw, &out) == nil {
									s := string(out.Result.Value)
									if s != last {
										last = s
										logf("RCHFRAME i=%d %s -> %s", i, fid, s)
									}
								}
							}()
						}
					}()
				case id := <-cfCtx:
					cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					raw, err := d.c.call(cctx, d.sid, "Runtime.evaluate", map[string]any{
						"expression":    `JSON.stringify({ua:navigator.userAgent, wd:String(navigator.webdriver), focus:document.hasFocus(), sl:screen.height+"x"+screen.availHeight, cw:document.documentElement.clientWidth, href:window.location.href.slice(0,70)})`,
						"returnByValue": true,
						"contextId":     id,
					})
					cancel()
					if err != nil {
						logf("CFCTX id=%d evaluate err=%v", id, err)
						continue
					}
					var out struct {
						Result struct {
							Value json.RawMessage `json:"value"`
						} `json:"result"`
					}
					if json.Unmarshal(raw, &out) == nil {
						logf("CFCTX id=%d -> %s", id, out.Result.Value)
					}
				case <-ticker.C:
					func() {
						rctx2, rcancel2 := context.WithTimeout(context.Background(), 4*time.Second)
						defer rcancel2()
						traw, terr := d.c.call(rctx2, "", "Target.getTargets", nil)
						if terr != nil {
							return
						}
						var tl struct {
							TargetInfos []struct {
								TargetID string `json:"targetId"`
								URL      string `json:"url"`
								Type     string `json:"type"`
							} `json:"targetInfos"`
						}
						if json.Unmarshal(traw, &tl) != nil {
							return
						}
						for _, ti := range tl.TargetInfos {
							if !strings.Contains(ti.URL, "challenges.cloudflare.com") || probed["tg:"+ti.TargetID] {
								continue
							}
							if probed["tga:"+ti.TargetID] {
								// already attached+probed once; only retry if still about:blank
							}
							alog, aerr := d.c.call(rctx2, "", "Target.attachToTarget", map[string]any{
								"targetId": ti.TargetID, "flatten": true,
							})
							if aerr != nil {
								probed["tg:"+ti.TargetID] = true
								logf("RCHTGT attach err=%v", aerr)
								continue
							}
							var atg struct {
								SessionID string `json:"sessionId"`
							}
							if json.Unmarshal(alog, &atg) != nil || atg.SessionID == "" {
								probed["tg:"+ti.TargetID] = true
								continue
							}
							eraw, eerr := d.c.call(rctx2, atg.SessionID, "Runtime.evaluate", map[string]any{
								"expression": `JSON.stringify({href:(location.href||"").slice(0,95), rs:document.readyState,
									focus:document.hasFocus(), vis:document.visibilityState, hist:history.length,
									ref:(document.referrer||"").slice(0,60),
									inner:[innerWidth,innerHeight], outer:[outerWidth,outerHeight], dpr:devicePixelRatio,
									wd:String(navigator.webdriver), ua:navigator.userAgent,
									tz:Intl.DateTimeFormat().resolvedOptions().timeZone,
									dsp:String(window.__dsSp),
									swd:String((Object.getOwnPropertyDescriptor(Screen.prototype,"width")||{}).get||"none"),
									screen:[screen.width,screen.height,screen.availWidth,screen.availHeight],
									chrome:typeof window.chrome, plugs:(navigator.plugins||[]).length,
									topWin:(function(){try{return window.top===window}catch(e){return "xo"}})(),
									leaks:Object.getOwnPropertyNames(window).filter(function(k){return /cdc_|webdriver|selenium|phantom/i.test(k)}).join(",")})`,
								"returnByValue": true,
							})
							if eerr != nil {
								logf("RCHTGT eval err=%v", eerr)
								continue
							}
							var out struct {
								Result struct {
									Value json.RawMessage `json:"value"`
								} `json:"result"`
							}
							if json.Unmarshal(eraw, &out) == nil {
								val := string(out.Result.Value)
								if !strings.Contains(val, "about:blank") {
									probed["tg:"+ti.TargetID] = true
								}
								logf("RCHTGT type=%s %s -> %s", ti.Type, ti.URL[:min(70, len(ti.URL))], val)
							}
						}
					}()
					rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					raw, err := d.c.call(rctx, d.sid, "Page.getFrameTree", nil)
					if err != nil {
						cancel()
						logf("FRAMETREE err=%v", err)
						return
					}
					cancel()
					var ftRes struct {
						FrameTree frT `json:"frameTree"`
					}
					if json.Unmarshal(raw, &ftRes) != nil {
						continue
					}
					if !probed["_log"] {
						probed["_log"] = true
						var n int
						var collect func(f frT)
						collect = func(f frT) {
							n++
							for _, c := range f.ChildFrames {
								collect(c)
							}
						}
						collect(ftRes.FrameTree)
						logf("FRAMETREE n=%d", n)
					}
					var walk func(f frT)
					walk = func(f frT) {
						if f.Frame.URL != "" {
							ukey := "u:" + f.Frame.URL
							if !probed[ukey] {
								probed[ukey] = true
								logf("FRAME %s", f.Frame.URL[:min(70, len(f.Frame.URL))])
							}
						}
						if strings.Contains(f.Frame.URL, "challenges.cloudflare") && f.Frame.ID != "" && !probed[f.Frame.ID] {
							probed[f.Frame.ID] = true
							wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
							wraw, werr := d.c.call(wctx, d.sid, "Page.createIsolatedWorld", map[string]any{
								"frameId": f.Frame.ID, "worldName": "dsprobe",
							})
							if werr == nil {
								var w struct {
									ExecutionContextID int64 `json:"executionContextId"`
								}
								if json.Unmarshal(wraw, &w) == nil && w.ExecutionContextID != 0 {
									eraw, eerr := d.c.call(wctx, d.sid, "Runtime.evaluate", map[string]any{
										"expression":    `JSON.stringify({ua:navigator.userAgent, wd:String(navigator.webdriver), focus:document.hasFocus(), sl:screen.height+"x"+screen.availHeight, cw:document.documentElement.clientWidth, href:location.href.slice(0,70)})`,
										"returnByValue": true,
										"contextId":     w.ExecutionContextID,
									})
									if eerr == nil {
										var out struct {
											Result struct {
												Value json.RawMessage `json:"value"`
											} `json:"result"`
										}
										if json.Unmarshal(eraw, &out) == nil {
											logf("CFFRAME %s -> %s", f.Frame.URL[:min(60, len(f.Frame.URL))], out.Result.Value)
										}
									} else {
										logf("CFFRAME eval err=%v", eerr)
									}
								}
							} else {
								logf("CFFRAME world err=%v", werr)
							}
							wcancel()
						}
						for _, c := range f.ChildFrames {
							walk(c)
						}
					}
					walk(ftRes.FrameTree)
					dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
					draw, derr := d.c.call(dctx, d.sid, "Runtime.evaluate", map[string]any{
						"expression": domExpr, "returnByValue": true,
					})
					dcancel()
					if derr == nil {
						var dout struct {
							Result struct {
								Value json.RawMessage `json:"value"`
							} `json:"result"`
						}
						if json.Unmarshal(draw, &dout) == nil {
							var s string
							if json.Unmarshal(dout.Result.Value, &s) == nil && s != "" && !probed["dom:"+s] {
								probed["dom:"+s] = true
								logf("DOM %s", s)
							}
						}
					}
					if !probed["fp"] {
						fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
						fraw, ferr := d.c.call(fctx, d.sid, "Runtime.evaluate", map[string]any{
							"expression": fpExpr, "returnByValue": true,
						})
						fcancel()
						if ferr == nil {
							var fout struct {
								Result struct {
									Value json.RawMessage `json:"value"`
								} `json:"result"`
							}
							if json.Unmarshal(fraw, &fout) == nil {
								var s string
								if json.Unmarshal(fout.Result.Value, &s) == nil && s != "" {
									probed["fp"] = true
									logf("FP %s", s)
								}
							}
						}
					}
				}
			}
		}()
	}

	site := cfg.Sites["de"]
	c := solver.coreFor(cfg.Proxy.URL)
	t.Logf("browser=%s proxy=%s site=%s", exe, cfg.Proxy.URL, site.BaseURL)

	ctx := context.Background()
	token, err := solver.solveToken(ctx, site, site.SiteKey, c)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if len(token) < 20 {
		t.Fatalf("token too short (%d chars): %q", len(token), token)
	}
	t.Logf("token ok: %d chars, prefix %q", len(token), token[:min(12, len(token))])

	// the token is only worth something if the site accepts it
	sess, err := httpx.NewSession(httpx.Options{ProxyURL: cfg.Proxy.URL, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	if err := solver.verifyToken(ctx, sess, site, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
	t.Logf("verify ok: %d cookies for %s", len(sess.Cookies()), site.Code)
	for name := range sess.Cookies() {
		t.Logf("  cookie: %s", name)
	}
}
