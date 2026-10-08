package proxypool

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// 回归：Status() 必须在持锁状态读取 endpoint 字段。检查循环/Report 会在
// m.mu 下写 ep.healthy/lastErr/latencyMs，锁外读它们就是数据竞争
// （go test -race 会直接报 DATA RACE，线上是控制台刷新时的随机脏数据）。
func TestStatusDoesNotRaceWithEndpointUpdates(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ProxyPool.Enabled = true
	cfg.ProxyPool.Entries = map[string]*config.ProxyEntry{
		"a": {Link: "http://127.0.0.1:9", Enabled: true},
		"b": {Link: "socks5://127.0.0.1:10", Enabled: true},
	}
	m := New(cfg, t.TempDir(), nil)
	m.Reload()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		// 模拟检查循环：只在锁内写 endpoint 字段
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.Lock()
			for _, ep := range m.endpoints {
				ep.checked = true
				ep.healthy = !ep.healthy
				ep.latencyMs++
				ep.lastErr = "probe failed"
				ep.checkedAt = time.Now()
			}
			m.mu.Unlock()
		}
	}()

	for i := 0; i < 300; i++ {
		st := m.Status()
		if len(st.Entries) != 2 {
			close(stop)
			wg.Wait()
			t.Fatalf("entries = %d, want 2", len(st.Entries))
		}
	}
	close(stop)
	wg.Wait()
}

// 供 TestCloseKillsXrayChild 拉起的子进程桩：当成 xray 起着，等着被杀。
func TestXrayChildStub(t *testing.T) {
	if os.Getenv("DSFREE_XRAY_CHILD") != "1" {
		t.Skip("只给 Close 回归测试当子进程用")
	}
	time.Sleep(time.Hour)
}

// 回归：Close 必须同步杀掉 xray 子进程并等后台循环退出。进程退出时
// Windows/Linux 都不会替父进程带走子进程，漏杀就是孤儿 xray。
func TestCloseKillsXrayChild(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	m := New(cfg, t.TempDir(), nil)
	m.Start(context.Background())

	cmd := exec.Command(os.Args[0], "-test.run=^TestXrayChildStub$")
	cmd.Env = append(os.Environ(), "DSFREE_XRAY_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	m.xray.mu.Lock()
	m.xray.proc = cmd
	m.xray.exited = exited
	m.xray.running = true
	m.xray.pid = cmd.Process.Pid
	m.xray.mu.Unlock()

	m.Close()

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Close 没有回收 xray 子进程")
	}
	if st := m.xray.status(); st.Running || st.PID != 0 {
		t.Fatalf("xray 状态没清干净: %+v", st)
	}
	// 幂等：重复 Close 不能 panic 也不能卡死
	m.Close()
}
