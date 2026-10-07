// Package proxypool manages the egress endpoints used to reach the upstream
// sites: Xray share links driven by a managed Xray core, plus plain
// http/https/socks5 endpoints. Selection is sticky per site and health is
// probed in the background.
package proxypool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Node is a parsed proxy node.
type Node struct {
	Kind     string         // "xray" (share link) or "url" (plain endpoint)
	Scheme   string         // vless / vmess / trojan / ss / http / https / socks5
	Display  string         // host:port, for the console
	Link     string         // original input line
	URL      string         // Kind == "url": ready-to-use proxy URL
	Outbound map[string]any // Kind == "xray": Xray outbound object (tag added later)
}

// ParseNode parses one line: an Xray share link (vless/vmess/trojan/ss) or a
// plain endpoint. Accepted plain forms:
//
//	scheme://user:pass@host:port
//	user:pass@host:port            (scheme = defaultScheme)
//	host:port                      (scheme = defaultScheme)
//	host:port:user:pass            (scheme = defaultScheme)
func ParseNode(line, defaultScheme string) (*Node, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("empty node")
	}
	switch {
	case strings.HasPrefix(line, "vless://"):
		return parseVless(line)
	case strings.HasPrefix(line, "vmess://"):
		return parseVmess(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojan(line)
	case strings.HasPrefix(line, "ss://"):
		return parseShadowsocks(line)
	default:
		return parsePlainEndpoint(line, defaultScheme)
	}
}

func parseVless(raw string) (*Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("vless: %w", err)
	}
	id := u.User.Username()
	host, port := u.Hostname(), u.Port()
	if id == "" || host == "" || port == "" {
		return nil, errors.New("vless: missing uuid / host / port")
	}
	q := u.Query()
	user := map[string]any{"id": id, "encryption": "none"}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}
	out := map[string]any{
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []any{map[string]any{
				"address": host, "port": portInt(port), "users": []any{user},
			}},
		},
		"streamSettings": streamSettings(firstNonEmptyStr(q.Get("type"), "tcp"), q.Get("security"), q),
	}
	return &Node{Kind: "xray", Scheme: "vless", Display: net.JoinHostPort(host, port), Link: raw, Outbound: out}, nil
}

func parseVmess(raw string) (*Node, error) {
	body := strings.TrimPrefix(raw, "vmess://")
	if i := strings.IndexAny(body, "#"); i >= 0 {
		body = body[:i]
	}
	dec, err := b64Decode(body)
	if err != nil {
		return nil, fmt.Errorf("vmess: invalid base64 payload: %w", err)
	}
	var v struct {
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Aid  any    `json:"aid"`
		Scy  string `json:"scy"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
		FP   string `json:"fp"`
		ALPN string `json:"alpn"`
	}
	if err := json.Unmarshal([]byte(dec), &v); err != nil {
		return nil, fmt.Errorf("vmess: bad json: %w", err)
	}
	port := toInt(v.Port)
	if v.Add == "" || port == 0 || v.ID == "" {
		return nil, errors.New("vmess: missing add / port / id")
	}
	security := strings.TrimSpace(v.Scy)
	if security == "" {
		security = "auto"
	}
	out := map[string]any{
		"protocol": "vmess",
		"settings": map[string]any{
			"vnext": []any{map[string]any{
				"address": v.Add, "port": port,
				"users": []any{map[string]any{"id": v.ID, "alterId": toInt(v.Aid), "security": security}},
			}},
		},
	}
	network := strings.ToLower(strings.TrimSpace(v.Net))
	if network == "" {
		network = "tcp"
	}
	ss := map[string]any{"network": network}
	switch network {
	case "ws":
		ws := map[string]any{}
		if v.Path != "" {
			ws["path"] = v.Path
		}
		if v.Host != "" {
			ws["headers"] = map[string]any{"Host": v.Host}
		}
		ss["wsSettings"] = ws
	case "h2":
		h2 := map[string]any{}
		if v.Path != "" {
			h2["path"] = v.Path
		}
		if v.Host != "" {
			h2["host"] = []any{v.Host}
		}
		ss["h2Settings"] = h2
	case "grpc":
		g := map[string]any{}
		if v.Path != "" {
			g["serviceName"] = v.Path
		}
		ss["grpcSettings"] = g
	case "tcp":
		if strings.EqualFold(v.Type, "http") {
			req := map[string]any{"path": []any{firstNonEmptyStr(v.Path, "/")}}
			if v.Host != "" {
				req["headers"] = map[string]any{"Host": []any{v.Host}}
			}
			ss["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	case "httpupgrade", "xhttp":
		sub := map[string]any{}
		if v.Path != "" {
			sub["path"] = v.Path
		}
		if v.Host != "" {
			sub["host"] = v.Host
		}
		ss[network+"Settings"] = sub
	}
	if strings.EqualFold(v.TLS, "tls") {
		ss["security"] = "tls"
		t := map[string]any{}
		if sni := firstNonEmptyStr(v.SNI, v.Host); sni != "" {
			t["serverName"] = sni
		}
		if v.FP != "" {
			t["fingerprint"] = v.FP
		}
		if v.ALPN != "" {
			t["alpn"] = strings.Split(v.ALPN, ",")
		}
		ss["tlsSettings"] = t
	}
	out["streamSettings"] = ss
	return &Node{Kind: "xray", Scheme: "vmess", Display: net.JoinHostPort(v.Add, strconv.Itoa(port)), Link: raw, Outbound: out}, nil
}

func parseTrojan(raw string) (*Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("trojan: %w", err)
	}
	pass := u.User.Username()
	host, port := u.Hostname(), u.Port()
	if pass == "" || host == "" || port == "" {
		return nil, errors.New("trojan: missing password / host / port")
	}
	q := u.Query()
	security := q.Get("security")
	if security == "" {
		security = "tls"
	}
	out := map[string]any{
		"protocol": "trojan",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": host, "port": portInt(port), "password": pass}},
		},
		"streamSettings": streamSettings(firstNonEmptyStr(q.Get("type"), "tcp"), security, q),
	}
	return &Node{Kind: "xray", Scheme: "trojan", Display: net.JoinHostPort(host, port), Link: raw, Outbound: out}, nil
}

func parseShadowsocks(raw string) (*Node, error) {
	body := strings.TrimPrefix(raw, "ss://")
	if i := strings.Index(body, "#"); i >= 0 {
		body = body[:i]
	}
	var method, pass, hostport string
	if at := strings.LastIndex(body, "@"); at >= 0 {
		userinfo, hp := body[:at], body[at+1:]
		if i := strings.IndexAny(hp, "?"); i >= 0 {
			hp = hp[:i]
		}
		if strings.Contains(userinfo, ":") {
			// plain method:password (uncommon)
		} else {
			dec, err := b64Decode(userinfo)
			if err != nil {
				return nil, errors.New("ss: cannot decode credentials")
			}
			userinfo = dec
		}
		mp := strings.SplitN(userinfo, ":", 2)
		if len(mp) != 2 || mp[0] == "" || mp[1] == "" {
			return nil, errors.New("ss: bad credentials")
		}
		method, pass, hostport = mp[0], mp[1], hp
	} else {
		// legacy: ss://base64(method:password@host:port)
		dec, err := b64Decode(body)
		if err != nil {
			return nil, errors.New("ss: invalid base64 payload")
		}
		at := strings.LastIndex(dec, "@")
		if at < 0 {
			return nil, errors.New("ss: missing @host")
		}
		mp := strings.SplitN(dec[:at], ":", 2)
		if len(mp) != 2 {
			return nil, errors.New("ss: bad credentials")
		}
		method, pass, hostport = mp[0], mp[1], dec[at+1:]
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("ss: %w", err)
	}
	out := map[string]any{
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": host, "port": port, "method": method, "password": pass}},
		},
	}
	return &Node{Kind: "xray", Scheme: "ss", Display: net.JoinHostPort(host, strconv.Itoa(port)), Link: raw, Outbound: out}, nil
}

func parsePlainEndpoint(line, defaultScheme string) (*Node, error) {
	if defaultScheme = strings.ToLower(strings.TrimSpace(defaultScheme)); defaultScheme == "" {
		defaultScheme = "http"
	}
	build := func(scheme, host, port, user, pass string) (*Node, error) {
		scheme = normalizeScheme(scheme)
		if scheme == "" {
			return nil, fmt.Errorf("unsupported proxy scheme %q (http/https/socks5)", scheme)
		}
		if host == "" || port == "" {
			return nil, errors.New("missing host or port")
		}
		u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}
		if user != "" {
			u.User = url.UserPassword(user, pass)
		}
		return &Node{
			Kind: "url", Scheme: scheme,
			Display: net.JoinHostPort(host, port),
			Link:    line, URL: u.String(),
		}, nil
	}

	switch {
	case strings.Contains(line, "://"):
		u, err := url.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("bad proxy url: %w", err)
		}
		user, pass := "", ""
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		return build(u.Scheme, u.Hostname(), u.Port(), user, pass)
	case strings.Contains(line, "@"):
		at := strings.LastIndex(line, "@")
		userinfo, hostport := line[:at], line[at+1:]
		mp := strings.SplitN(userinfo, ":", 2)
		user, pass := mp[0], ""
		if len(mp) == 2 {
			pass = mp[1]
		}
		host, port, err := splitHostPort(hostport)
		if err != nil {
			return nil, err
		}
		return build(defaultScheme, host, strconv.Itoa(port), user, pass)
	default:
		parts := strings.Split(line, ":")
		switch len(parts) {
		case 2:
			return build(defaultScheme, parts[0], parts[1], "", "")
		case 4:
			// ip:port:user:pass
			return build(defaultScheme, parts[0], parts[1], parts[2], parts[3])
		default:
			return nil, fmt.Errorf("unrecognized endpoint %q", line)
		}
	}
}

// ParseSubscriptionBody turns a subscription response into node lines. The
// body may be plain text (one node per line) or a base64 blob of that text.
func ParseSubscriptionBody(raw []byte) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil
	}
	if !strings.Contains(s, "://") {
		if dec, err := b64Decode(strings.ReplaceAll(s, "\n", "")); err == nil {
			s = dec
		}
	}
	out := make([]string, 0, 16)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// ── helpers ──────────────────────────────────────────────────────

// streamSettings builds Xray streamSettings from share-link query params.
func streamSettings(network, security string, q url.Values) map[string]any {
	network = strings.ToLower(strings.TrimSpace(network))
	if network == "" {
		network = "tcp"
	}
	ss := map[string]any{"network": network}
	switch network {
	case "ws":
		ws := map[string]any{}
		if p := q.Get("path"); p != "" {
			ws["path"] = p
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		ss["wsSettings"] = ws
	case "grpc":
		g := map[string]any{}
		if s := q.Get("serviceName"); s != "" {
			g["serviceName"] = s
		}
		ss["grpcSettings"] = g
	case "httpupgrade":
		h := map[string]any{}
		if p := q.Get("path"); p != "" {
			h["path"] = p
		}
		if hh := q.Get("host"); hh != "" {
			h["host"] = hh
		}
		ss["httpupgradeSettings"] = h
	case "xhttp":
		x := map[string]any{}
		if p := q.Get("path"); p != "" {
			x["path"] = p
		}
		if hh := q.Get("host"); hh != "" {
			x["host"] = hh
		}
		if m := q.Get("mode"); m != "" {
			x["mode"] = m
		}
		ss["xhttpSettings"] = x
	case "tcp":
		if strings.EqualFold(q.Get("headerType"), "http") {
			req := map[string]any{}
			if h := q.Get("host"); h != "" {
				req["headers"] = map[string]any{"Host": []any{h}}
			}
			if p := q.Get("path"); p != "" {
				req["path"] = []any{p}
			}
			ss["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	}
	switch strings.ToLower(security) {
	case "tls":
		t := map[string]any{}
		if sni := firstNonEmptyStr(q.Get("sni"), q.Get("host")); sni != "" {
			t["serverName"] = sni
		}
		if fp := q.Get("fp"); fp != "" {
			t["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			t["alpn"] = strings.Split(alpn, ",")
		}
		ss["security"] = "tls"
		ss["tlsSettings"] = t
	case "reality":
		r := map[string]any{}
		if sni := q.Get("sni"); sni != "" {
			r["serverName"] = sni
		}
		if fp := q.Get("fp"); fp != "" {
			r["fingerprint"] = fp
		}
		if pbk := q.Get("pbk"); pbk != "" {
			r["publicKey"] = pbk
		}
		if sid := q.Get("sid"); sid != "" {
			r["shortId"] = sid
		}
		if spx := q.Get("spx"); spx != "" {
			r["spiderX"] = spx
		}
		ss["security"] = "reality"
		ss["realitySettings"] = r
	}
	return ss
}

func normalizeScheme(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "http":
		return "http"
	case "https":
		return "https"
	case "socks", "socks5", "socks5h":
		return "socks5"
	default:
		return ""
	}
}

func b64Decode(s string) (string, error) {
	s = strings.TrimSpace(s)
	trimmed := strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(trimmed); err == nil {
			return string(b), nil
		}
	}
	return "", errors.New("invalid base64")
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func portInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func splitHostPort(hostport string) (string, int, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(hostport))
	if err != nil {
		return "", 0, fmt.Errorf("bad host:port %q", hostport)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("bad port in %q", hostport)
	}
	return host, n, nil
}
