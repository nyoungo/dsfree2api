package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// 回归：HTTP 服务启动失败（errCh 分支）时 run() 必须返回退出码 1，且所有
// defer 照常执行 —— metrics.json 只有 met.Close() 才会写出来。旧实现
// 在这里直接 os.Exit(1)，defer 全被跳过：指标/日志不落盘，xray 子进程
// 也收不回来。
//
// 触发方式用一个不可绑定的地址（RFC 5737 文档段，任何机器都不可能配在
// 本地网卡上），而不是占用端口：Windows 上 Go 的 listener 带 SO_REUSEADDR，
// 第二个同端口绑定照样成功，根本走不到失败分支。
func TestServerBindFailureRunsDefers(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dsfree2api")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cfg, err := config.Load("../../config.example.toml")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	dataDir := t.TempDir()
	cfg.Runtime.DataDir = dataDir
	cfg.Admin.Enabled = false
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatalf("save temp config: %v", err)
	}

	// CommandContext + 超时：万一子进程不按预期退出，到点强杀，绝不留
	// 孤儿进程（早期版本用裸 exec.Command，遗留过一个跑在随机端口上的
	// 实例直到人工发现）。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-config", cfgPath, "-host", "192.0.2.1").CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("子进程 60s 未退出（已强杀）\noutput:\n%s", out)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want code 1\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "server failed") {
		t.Fatalf("没走到 server failed 分支\noutput:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "metrics.json")); err != nil {
		t.Fatalf("metrics.json 没落盘，defer 被跳过: %v\noutput:\n%s", err, out)
	}
}
