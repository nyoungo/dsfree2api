package proxypool

import (
	"net"
	"os"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// TestXrayCoreSmoke drives the managed core against the locally placed
// binary (data/xray.exe) with a syntactically real VLESS node. It never
// connects upstream — it only proves that ensure() picks up the local
// binary, configure() starts the process and the local socks inbound is
// live. Skipped unless opted in:
//
//	$env:DSFREE_XRAY_SMOKE=1; go test ./internal/proxypool -run Smoke -v
func TestXrayCoreSmoke(t *testing.T) {
	if os.Getenv("DSFREE_XRAY_SMOKE") == "" {
		t.Skip("set DSFREE_XRAY_SMOKE=1 to run the local xray smoke test")
	}
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ProxyPool.Enabled = true
	cfg.ProxyPool.XrayAutoDownload = false // the local binary must be used as-is
	cfg.ProxyPool.Entries = map[string]*config.ProxyEntry{
		"smoke": {
			Link: "vless://11111111-2222-3333-4444-555555555555@example.com:443" +
				"?security=tls&type=tcp&sni=example.com",
			Enabled: true,
		},
	}
	cfg.Sites["de"].Proxies = []string{"smoke"}

	m := New(cfg, "../../data", nil)
	m.Reload()
	st := m.Status()
	if !st.Xray.Running {
		t.Fatalf("xray core did not start: %+v", st.Xray)
	}
	t.Logf("xray running: pid=%d path=%s", st.Xray.PID, st.Xray.Path)

	cands := m.Candidates("de")
	if len(cands) != 1 || !strings.HasPrefix(cands[0].Proxy, "socks5://127.0.0.1:") {
		t.Fatalf("candidates = %+v", cands)
	}
	t.Logf("candidate proxy: %s", cands[0].Proxy)

	m.xray.stop()
	if m.Status().Xray.Running {
		t.Fatal("xray still running after stop")
	}

	// Port stability: a restart re-claims the same socks port, so cached
	// cookies (keyed by the local proxy URL) stay valid.
	m.Reload()
	again := m.Candidates("de")
	if len(again) != 1 || again[0].Proxy != cands[0].Proxy {
		t.Fatalf("port not stable across restart: %q -> %+v", cands[0].Proxy, again)
	}
	m.xray.stop()

	// Race self-heal: steal the assigned port; the next start must move away.
	port := strings.TrimPrefix(cands[0].Proxy, "socks5://127.0.0.1:")
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("occupy port %s: %v", port, err)
	}
	defer ln.Close()
	m.Reload()
	third := m.Candidates("de")
	if len(third) != 1 || third[0].Proxy == cands[0].Proxy {
		t.Fatalf("stolen port was not reassigned: %+v", third)
	}
	t.Logf("port moved %s -> %s", cands[0].Proxy, third[0].Proxy)
	m.xray.stop()
}
