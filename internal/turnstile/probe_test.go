package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// TestHeadlessProbe prints the environment signals Cloudflare can use to
// detect a headless browser, mirroring the production setup path.
// Opt in: DSFREE_PROBE=1 go test ./internal/turnstile -run Probe -v
// (DSFREE_PROBE_HEADED=1 runs the headed baseline instead)
func TestHeadlessProbe(t *testing.T) {
	if os.Getenv("DSFREE_PROBE") == "" {
		t.Skip("set DSFREE_PROBE=1 to probe the headless environment")
	}
	cfg, err := config.Load("../../config.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	headless := os.Getenv("DSFREE_PROBE_HEADED") == ""
	exe := `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	br, err := launchBrowser(ctx, exe, config.Turnstile{BrowserHeadless: headless}, "de-DE,de;q=0.9", cfg.Proxy.URL)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer br.Close()
	conn, err := dialCDP(ctx, br.wsURL, fmt.Sprintf("http://127.0.0.1:%d", br.port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	sid, err := attachPage(ctx, conn)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	metrics := map[string]any{"width": 1280, "height": 900, "deviceScaleFactor": 1, "mobile": false}
	if headless {
		metrics["screenWidth"] = 1920
		metrics["screenHeight"] = 1080
	}
	for _, step := range [][2]any{
		{"Page.enable", nil},
		{"Runtime.enable", nil},
		{"Emulation.setDeviceMetricsOverride", metrics},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": stealthJS}},
	} {
		if _, err := conn.call(ctx, sid, step[0].(string), step[1]); err != nil {
			t.Fatalf("%v: %v", step[0], err)
		}
	}
	if headless {
		bootSid, closeBoot, err := openBootstrap(ctx, conn, "https://www.baidu.com/", 8*time.Second)
		if err != nil {
			t.Logf("bootstrap: %v", err)
		} else {
			if _, err := overrideHeadlessUA(ctx, conn, sid, bootSid); err != nil {
				t.Logf("overrideHeadlessUA: %v", err)
			}
			closeBoot()
		}
	}
	if _, err := conn.call(ctx, sid, "Page.navigate", map[string]any{"url": "https://deepseek.de/"}); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := conn.evaluate(ctx, sid, `document.readyState`)
		if err == nil && string(raw) == `"complete"` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("page never completed: raw=%s err=%v", raw, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	probe := `(() => {
	  const o = {headless: %(HEADLESS)s, ua: navigator.userAgent,
	    screen: [screen.width, screen.height, screen.availWidth, screen.availHeight, screen.colorDepth, screen.pixelDepth],
	    inner: [window.innerWidth, window.innerHeight],
	    outer: [window.outerWidth, window.outerHeight, window.screenX, window.screenY],
	    docClientW: document.documentElement.clientWidth,
	    mq: {hover: matchMedia("(hover: hover)").matches, anyHover: matchMedia("(any-hover: hover)").matches,
	         pfine: matchMedia("(pointer: fine)").matches, pcoarse: matchMedia("(pointer: coarse)").matches,
	         pnone: matchMedia("(pointer: none)").matches, anyFine: matchMedia("(any-pointer: fine)").matches,
	         srgb: matchMedia("(color-gamut: srgb)").matches, dark: matchMedia("(prefers-color-scheme: dark)").matches},
	    chrome: Object.keys(window.chrome || {}).sort(),
	    notif: (typeof Notification !== "undefined") ? Notification.permission : "no-Notification",
	    plugins: navigator.plugins.length,
	    conn: navigator.connection ? [navigator.connection.effectiveType, navigator.connection.downlink, navigator.connection.rtt] : null,
	    orient: screen.orientation ? [screen.orientation.type, screen.orientation.angle] : null,
	    vv: window.visualViewport ? [window.visualViewport.width, window.visualViewport.height, window.visualViewport.scale] : null,
	    audio: null, voices: null, kbd: null, raf: null, gamepads: null};
	  return (async () => {
	    try { const ac = new AudioContext(); o.audio = [ac.sampleRate, ac.state, ac.baseLatency || 0]; } catch (e) { o.audio = "err:" + e; }
	    try {
	      o.voices = await new Promise(res => {
	        const ss = window.speechSynthesis; if (!ss) return res(-1);
	        const v = ss.getVoices(); if (v.length) return res(v.length);
	        const t = setTimeout(() => res(ss.getVoices().length), 1500);
	        ss.addEventListener("voiceschanged", () => { clearTimeout(t); res(ss.getVoices().length); }, {once:true});
	      });
	    } catch (e) { o.voices = "err:" + e; }
	    try {
	      const m = await navigator.keyboard.getLayoutMap();
	      o.kbd = [m.size, Array.from(m.values()).slice(0, 8).join("")];
	    } catch (e) { o.kbd = "err:" + e; }
	    try {
	      o.raf = await new Promise(res => {
	        let n = 0, t0 = 0;
	        const f = t => { if (!n) { t0 = t; } if (++n >= 5) { res(Math.round((t - t0) / 4)); } else requestAnimationFrame(f); };
	        requestAnimationFrame(f);
	        setTimeout(() => res("timeout"), 2000);
	      });
	    } catch (e) { o.raf = "err:" + e; }
	    try { o.gamepads = (navigator.getGamepads ? Array.from(navigator.getGamepads()).length : -1); } catch (e) { o.gamepads = "err"; }
	    return JSON.stringify(o);
	  })();
	})()`
	raw, err := conn.evaluate(ctx, sid, strings.Replace(probe, "%(HEADLESS)s", map[bool]string{true: "true", false: "false"}[headless], 1))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var payload string
	if json.Unmarshal(raw, &payload) != nil {
		payload = string(raw)
	}
	fmt.Printf("PROBE(headless=%v): %s\n", headless, payload)
}
