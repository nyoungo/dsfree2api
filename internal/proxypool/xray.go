package proxypool

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// xrayNode is one Xray-backed endpoint, ready to be written into the core
// config.
type xrayNode struct {
	Name     string
	Outbound map[string]any
}

// xrayCore owns the managed Xray process: one socks inbound per pool node,
// each routed to its own outbound.
type xrayCore struct {
	dir string
	log *slog.Logger

	mu      sync.Mutex
	path    string
	proc    *exec.Cmd
	exited  chan struct{}
	running bool
	pid     int
	cfgPath string
	cfgHash string
	ports   map[string]int
	lastErr string
	tail    *logTail
}

// XrayStatus is the console view of the core.
type XrayStatus struct {
	Path    string `json:"path"`
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Error   string `json:"error,omitempty"`
}

func newXrayCore(dir string, log *slog.Logger) *xrayCore {
	if log == nil {
		log = slog.Default()
	}
	return &xrayCore{dir: dir, log: log, ports: map[string]int{}}
}

func (c *xrayCore) status() XrayStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return XrayStatus{Path: c.path, Running: c.running, PID: c.pid, Error: c.lastErr}
}

// ensure resolves the Xray binary: explicit path, then a previously downloaded
// copy, then (optionally) a fresh download. The download runs unlocked so
// status reads never stall behind a slow network.
func (c *xrayCore) ensure(explicitPath string, autoDownload bool, version string) error {
	c.mu.Lock()
	if c.path != "" {
		if _, err := os.Stat(c.path); err == nil {
			c.mu.Unlock()
			return nil
		}
		c.path = ""
	}
	if p := strings.TrimSpace(explicitPath); p != "" {
		if _, err := os.Stat(p); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("xray_path: %w", err)
		}
		c.path = p
		c.mu.Unlock()
		return nil
	}
	target := filepath.Join(c.dir, "xray"+exeSuffix())
	if _, err := os.Stat(target); err == nil {
		c.path = target
		c.mu.Unlock()
		return nil
	}
	if !autoDownload {
		c.mu.Unlock()
		return errors.New("xray binary not found — set [proxypool].xray_path, or enable xray_auto_download")
	}
	c.mu.Unlock()

	if err := c.download(target, version); err != nil {
		c.mu.Lock()
		c.lastErr = err.Error()
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.path = target
	c.mu.Unlock()
	return nil
}

func (c *xrayCore) download(target, version string) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	osName := map[string]string{"windows": "windows", "linux": "linux", "darwin": "macos"}[runtime.GOOS]
	arch := map[string]string{"amd64": "64", "arm64": "arm64-v8a", "386": "32", "arm": "arm32-v7a"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return fmt.Errorf("xray: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	asset := fmt.Sprintf("Xray-%s-%s.zip", osName, arch)
	downloadURL := "https://github.com/XTLS/Xray-core/releases/latest/download/" + asset
	if v := strings.TrimSpace(version); v != "" {
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		downloadURL = fmt.Sprintf("https://github.com/XTLS/Xray-core/releases/download/%s/%s", v, asset)
	}
	c.log.Info("downloading xray core", "asset", asset)
	zipPath := target + ".download"
	if err := httpDownload(downloadURL, zipPath, 180*time.Second); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	if want, err := fetchDigest(downloadURL + ".dgst"); err != nil {
		c.log.Warn("xray checksum unavailable, skipping verification", "error", err)
	} else if want != "" {
		got, err := fileSHA256(zipPath)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("xray checksum mismatch: got %s want %s", got, want)
		}
	}
	if err := extractZipBinary(zipPath, target); err != nil {
		return err
	}
	_ = os.Chmod(target, 0o755)
	c.log.Info("xray core ready", "path", target)
	return nil
}

// configure regenerates the core config for the given nodes and restarts the
// process when something changed. Ports are kept stable across restarts so
// cached cookies stay valid.
func (c *xrayCore) configure(nodes []xrayNode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(nodes) == 0 {
		c.stopLocked()
		c.cfgHash = ""
		return nil
	}
	if c.path == "" {
		return errors.New("xray binary unavailable")
	}
	for _, n := range nodes {
		if c.ports[n.Name] == 0 {
			p, err := freePort()
			if err != nil {
				return err
			}
			c.ports[n.Name] = p
		}
	}
	if c.configHash(nodes) == c.cfgHash && c.running {
		return nil
	}
	// A restart is due. Stop first so our own listeners do not count as
	// "port busy", then reclaim or reassign every assigned port — another
	// local core may have raced us for one, since the OS only guaranteed it
	// at allocation time.
	c.stopLocked()
	for _, n := range nodes {
		if !portAvailable(c.ports[n.Name]) {
			p, err := freePort()
			if err != nil {
				return err
			}
			c.ports[n.Name] = p
		}
	}
	raw, err := json.Marshal(buildXrayConfig(nodes, c.ports))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	c.cfgHash = hex.EncodeToString(sum[:])
	cfgPath := filepath.Join(c.dir, "config.json")
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return err
	}
	c.cfgPath = cfgPath
	return c.startLocked(nodes)
}

// configHash is the digest of the config that would be generated for nodes
// with the current port assignments.
func (c *xrayCore) configHash(nodes []xrayNode) string {
	raw, err := json.Marshal(buildXrayConfig(nodes, c.ports))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// portAvailable reports whether 127.0.0.1:port can still be claimed.
func portAvailable(port int) bool {
	if port <= 0 {
		return false
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func (c *xrayCore) startLocked(nodes []xrayNode) error {
	cmd := exec.Command(c.path, "run", "-c", c.cfgPath)
	c.tail = &logTail{max: 40}
	cmd.Stdout = c.tail
	cmd.Stderr = c.tail
	if err := cmd.Start(); err != nil {
		c.lastErr = "start xray: " + err.Error()
		return errors.New(c.lastErr)
	}
	c.proc = cmd
	c.pid = cmd.Process.Pid
	c.running = true
	c.lastErr = ""
	exited := make(chan struct{})
	c.exited = exited
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		if c.proc == cmd {
			c.running = false
			c.pid = 0
			if t := c.tail.Last(); t != "" {
				c.lastErr = "xray exited: " + t
			} else {
				c.lastErr = "xray exited"
			}
		}
		c.mu.Unlock()
		close(exited)
	}()
	// Wait until the local socks inbounds answer.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, n := range nodes {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.ports[n.Name]), 300*time.Millisecond)
			if err != nil {
				ok = false
				break
			}
			conn.Close()
		}
		if ok {
			return nil
		}
		if !c.running {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !c.running {
		if c.lastErr == "" {
			c.lastErr = "xray failed to start"
		}
		return errors.New(c.lastErr)
	}
	c.lastErr = "xray socks ports did not come up in time"
	return errors.New(c.lastErr)
}

func (c *xrayCore) stopLocked() {
	if c.proc != nil && c.proc.Process != nil {
		_ = c.proc.Process.Kill()
	}
	if c.exited != nil {
		select {
		case <-c.exited:
		case <-time.After(3 * time.Second):
		}
	}
	c.proc = nil
	c.exited = nil
	c.running = false
	c.pid = 0
}

func (c *xrayCore) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// port returns the local socks port assigned to a node name.
func (c *xrayCore) port(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ports[name]
}

func buildXrayConfig(nodes []xrayNode, ports map[string]int) map[string]any {
	inbounds := make([]any, 0, len(nodes))
	outbounds := make([]any, 0, len(nodes)+2)
	rules := make([]any, 0, len(nodes))
	for i, n := range nodes {
		inTag := fmt.Sprintf("in-%d", i)
		outTag := fmt.Sprintf("out-%d", i)
		inbounds = append(inbounds, map[string]any{
			"tag": inTag, "listen": "127.0.0.1", "port": ports[n.Name],
			"protocol": "socks",
			"settings": map[string]any{"udp": false, "auth": "noauth"},
		})
		ob := make(map[string]any, len(n.Outbound)+1)
		for k, v := range n.Outbound {
			ob[k] = v
		}
		ob["tag"] = outTag
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []any{inTag}, "outboundTag": outTag})
	}
	outbounds = append(outbounds,
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{"tag": "blocked", "protocol": "blackhole"},
	)
	return map[string]any{
		"log":       map[string]any{"loglevel": "warning", "access": "none"},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rules},
	}
}

// ── platform / io helpers ────────────────────────────────────────

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func httpDownload(url, dst string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: http %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 256<<20)); err != nil {
		return err
	}
	return f.Sync()
}

func fetchDigest(url string) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.Contains(strings.ToUpper(key), "SHA2-256") || strings.Contains(strings.ToUpper(key), "SHA256") {
			if v := strings.TrimSpace(strings.Fields(val)[0]); v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("no sha256 digest found")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractZipBinary(zipPath, target string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer r.Close()
	want := "xray" + exeSuffix()
	for _, f := range r.File {
		if !strings.EqualFold(filepath.Base(f.Name), want) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	return errors.New("no xray binary inside the archive")
}

// logTail keeps the last few process log lines for status reporting.
type logTail struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (t *logTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		t.lines = append(t.lines, line)
	}
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	return len(p), nil
}

func (t *logTail) Last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return ""
	}
	return t.lines[len(t.lines)-1]
}
