package proxypool

import (
	"encoding/base64"
	"testing"
)

func TestParseVless(t *testing.T) {
	line := "vless://11111111-2222-3333-4444-555555555555@example.com:443" +
		"?type=ws&security=tls&sni=sni.example.com&path=%2Fws&host=cdn.example.com&fp=chrome#node"
	n, err := ParseNode(line, "http")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Kind != "xray" || n.Scheme != "vless" || n.Display != "example.com:443" {
		t.Fatalf("node = %+v", n)
	}
	if n.Outbound["protocol"] != "vless" {
		t.Fatalf("protocol = %v", n.Outbound["protocol"])
	}
	settings := n.Outbound["settings"].(map[string]any)
	vnext := settings["vnext"].([]any)[0].(map[string]any)
	if vnext["address"] != "example.com" || vnext["port"] != 443 {
		t.Fatalf("vnext = %+v", vnext)
	}
	users := vnext["users"].([]any)[0].(map[string]any)
	if users["id"] != "11111111-2222-3333-4444-555555555555" || users["encryption"] != "none" {
		t.Fatalf("users = %+v", users)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Fatalf("stream = %+v", ss)
	}
	ws := ss["wsSettings"].(map[string]any)
	if ws["path"] != "/ws" {
		t.Fatalf("ws = %+v", ws)
	}
	tls := ss["tlsSettings"].(map[string]any)
	if tls["serverName"] != "sni.example.com" {
		t.Fatalf("tls = %+v", tls)
	}
}

func TestParseVmess(t *testing.T) {
	payload := `{"v":"2","ps":"n","add":"vm.example.com","port":"443","id":"uuid-1",` +
		`"aid":"0","scy":"auto","net":"ws","type":"none","host":"vm.example.com",` +
		`"path":"/vm","tls":"tls","sni":"vm.example.com"}`
	line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
	n, err := ParseNode(line, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Kind != "xray" || n.Scheme != "vmess" || n.Display != "vm.example.com:443" {
		t.Fatalf("node = %+v", n)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Fatalf("stream = %+v", ss)
	}
	if ss["wsSettings"].(map[string]any)["path"] != "/vm" {
		t.Fatalf("ws = %+v", ss["wsSettings"])
	}
}

func TestParseTrojan(t *testing.T) {
	n, err := ParseNode("trojan://secret@tr.example.com:443?security=tls&sni=tr.example.com#t", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Scheme != "trojan" || n.Display != "tr.example.com:443" {
		t.Fatalf("node = %+v", n)
	}
	settings := n.Outbound["settings"].(map[string]any)
	srv := settings["servers"].([]any)[0].(map[string]any)
	if srv["address"] != "tr.example.com" || srv["port"] != 443 || srv["password"] != "secret" {
		t.Fatalf("server = %+v", srv)
	}
}

func TestParseShadowsocks(t *testing.T) {
	// SIP002: base64(method:password)@host:port
	ui := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass:word"))
	n, err := ParseNode("ss://"+ui+"@ss.example.com:8388#x", "")
	if err != nil {
		t.Fatalf("sip002 parse: %v", err)
	}
	srv := n.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv["method"] != "aes-256-gcm" || srv["password"] != "pass:word" || srv["address"] != "ss.example.com" {
		t.Fatalf("server = %+v", srv)
	}

	// legacy: base64(method:password@host:port)
	legacy := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret@ss.example.com:8388"))
	n2, err := ParseNode("ss://"+legacy, "")
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	srv2 := n2.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv2["password"] != "secret" || srv2["address"] != "ss.example.com" || srv2["port"] != 8388 {
		t.Fatalf("legacy server = %+v", srv2)
	}
}

func TestParsePlainEndpoints(t *testing.T) {
	cases := []struct {
		line, def, wantURL, wantDisplay string
	}{
		{"socks5://u:p@1.2.3.4:1080", "http", "socks5://u:p@1.2.3.4:1080", "1.2.3.4:1080"},
		{"http://1.2.3.4:8080", "socks5", "http://1.2.3.4:8080", "1.2.3.4:8080"},
		{"1.2.3.4:8080", "http", "http://1.2.3.4:8080", "1.2.3.4:8080"},
		{"1.2.3.4:8080", "socks5", "socks5://1.2.3.4:8080", "1.2.3.4:8080"},
		{"u:p@1.2.3.4:1080", "http", "http://u:p@1.2.3.4:1080", "1.2.3.4:1080"},
		{"1.2.3.4:8080:u:p", "http", "http://u:p@1.2.3.4:8080", "1.2.3.4:8080"},
	}
	for _, c := range cases {
		n, err := ParseNode(c.line, c.def)
		if err != nil {
			t.Fatalf("%q: %v", c.line, err)
		}
		if n.Kind != "url" || n.URL != c.wantURL || n.Display != c.wantDisplay {
			t.Errorf("%q => kind=%s url=%q display=%q", c.line, n.Kind, n.URL, n.Display)
		}
	}
}

func TestParseSubscriptionBody(t *testing.T) {
	plain := "vless://a@b:1\n# comment\nsocks5://x:y@1.2.3.4:5\n\n"
	got := ParseSubscriptionBody([]byte(plain))
	if len(got) != 2 || got[0] != "vless://a@b:1" || got[1] != "socks5://x:y@1.2.3.4:5" {
		t.Fatalf("plain parse = %v", got)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(plain))
	got2 := ParseSubscriptionBody([]byte(enc))
	if len(got2) != 2 || got2[0] != got[0] {
		t.Fatalf("base64 parse = %v", got2)
	}
}

func TestParseNodeRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "hello world", "vless://no-port", "ss://@@@", "1.2.3.4", "ftp://1.2.3.4:21"} {
		if _, err := ParseNode(bad, "http"); err == nil {
			t.Errorf("%q should fail to parse", bad)
		}
	}
}
