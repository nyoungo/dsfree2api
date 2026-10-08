package config

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 回归：docker-compose 的 `API_KEYS: ${API_KEYS:-}` 让环境变量恒存在（值为空），
// 空值必须视为"不覆盖"，否则 config.toml 里的 api_keys 会被清空，
// api/server.go:120 的 len(keys) > 0 判定随之跳过全部鉴权。
// 对照：TURNSTILE_API_KEY 一直有 != "" 判断。
func TestEmptyAPIKeysEnvKeepsFileKeys(t *testing.T) {
	t.Setenv("API_KEYS", "")

	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Security.APIKeys) == 0 {
		t.Fatalf("空的 API_KEYS 环境变量清空了 config.toml 的 api_keys: %v "+
			"→ api/server.go:120 将跳过全部鉴权", cfg.Security.APIKeys)
	}
	if cfg.Security.APIKeys[0] != "sk-dsfr-local-change-me" {
		t.Fatalf("want file key preserved, got %v", cfg.Security.APIKeys)
	}
}

// 回归：SaveTo 必须持读锁遍历 Sites/Models 等 map —— 与并发的管理端写操作
// 相撞是 "concurrent map iteration and map write"，属不可 recover 的 fatal
// error，进程直接退出。用 -race 运行可稳定检出。
func TestSaveToConcurrentWithMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.path = path

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写者：持锁改 map（与真实 admin action 行为一致）
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			cfg.Lock()
			cfg.Sites["race-probe"] = &Site{Code: "race-probe", Enabled: false}
			if i%4 == 0 {
				delete(cfg.Sites, "race-probe")
			}
			cfg.Unlock()
			i++
		}
	}()

	// SaveTo：必须在读锁下编码（与真实 s.save() 行为一致）
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := cfg.SaveTo(path); err != nil {
			t.Logf("save: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// 回归：PUT 校验失败必须整体回滚，不能把半套配置留在内存里。
func TestReplaceJSONRollsBackOnInvalidPayload(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	before := cfg.Server.Port

	err = cfg.ReplaceJSON([]byte(`{"server":{"port":99999,"host":"127.0.0.1","log_level":"INFO","log_file":""}}`))
	if err == nil {
		t.Fatalf("want validation error for port 99999, got nil")
	}
	if cfg.Server.Port != before {
		t.Fatalf("失败的 PUT 没有回滚: port=%d, want %d", cfg.Server.Port, before)
	}
}

// 回归：控制台 GET 会把两个密钥抹成空串再原样回传，空值必须表示"保持不变"。
func TestReplaceJSONBlankSecretsKeepCurrent(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Admin.Password = "admin123"
	cfg.Turnstile.APIKey = "solver-secret"

	payload := []byte(`{"admin":{"enabled":true,"host":"127.0.0.1","port":8001,"password":""},` +
		`"turnstile":{"enabled":false,"api_key":""},"server":{"host":"127.0.0.1","port":18000,` +
		`"log_level":"INFO","log_file":""},"security":{"api_keys":["sk-a"]}}`)
	if err := cfg.ReplaceJSON(payload); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if cfg.Admin.Password != "admin123" {
		t.Fatalf("空 password 覆盖成了 %q", cfg.Admin.Password)
	}
	if cfg.Turnstile.APIKey != "solver-secret" {
		t.Fatalf("空 api_key 覆盖成了 %q", cfg.Turnstile.APIKey)
	}
	if cfg.Server.Port != 18000 {
		t.Fatalf("port = %d, want 18000", cfg.Server.Port)
	}
}

// 回归：SaveTo 必须原子替换目标文件。裸 os.WriteFile 是"先截断再写"，
// 进程崩在中间、或并发读者正好撞上截断窗口，就留下空/半截 config.toml
// —— 下次启动直接 config error 起不来。顺带把权限从 0644 收紧到 0600
// （config 里有 api_keys 和 admin password）。
func TestSaveToIsAtomicAndPrivate(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	want, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				// 完整文件 ~7KB，撞上截断窗口会读到 0 或半截
				if int64(len(raw)) < want.Size()-8 {
					atomic.AddInt32(&bad, 1)
				}
			}
		}()
	}

	for i := 0; i < 30; i++ {
		cfg.Lock()
		cfg.Server.Port = 8000 + i
		cfg.Unlock()
		if err := cfg.SaveTo(path); err != nil {
			t.Fatalf("save #%d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if bad > 0 {
		t.Fatalf("并发读者读到了写了一半的 config: %d 次", bad)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("临时文件没清理: %s", path+".tmp")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600（config 里有 api_keys / admin password）", st.Mode().Perm())
	}
}
