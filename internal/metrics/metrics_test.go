package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordTracksSiteTokensAndQuota(t *testing.T) {
	r := New(t.TempDir())
	r.Record(Record{Model: "deepseek-v4-flash-de", Site: "de", PromptTokens: 120, CompletionTokens: 80, Status: StatusOK})
	r.Record(Record{Model: "deepseek-v4-flash-de", Site: "de", PromptTokens: 15, Status: StatusError, Error: "sse quota exhausted"})

	got := r.SiteTotals("de")
	if got.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", got.Requests)
	}
	if got.PromptTokens != 135 || got.CompletionTokens != 80 {
		t.Errorf("tokens = %d/%d, want 135/80", got.PromptTokens, got.CompletionTokens)
	}

	r.IncSiteQuota("de")
	if got := r.SiteTotals("de"); got.Quota != 1 {
		t.Errorf("Quota = %d, want 1", got.Quota)
	}

	if empty := r.SiteTotals("fr"); empty.Requests != 0 || empty.Quota != 0 {
		t.Errorf("unknown site totals = %+v, want zero", empty)
	}
}

// 回归：落盘失败必须能被调用方看到。之前 Save 把 mkdir/写文件/rename
// 的错误全部吞掉，磁盘满或权限出问题时指标悄悄丢光、日志毫无痕迹。
func TestSaveSurfacesPersistFailure(t *testing.T) {
	dir := t.TempDir()
	// 把目标 metrics.json 建成目录 → rename 必然失败
	if err := os.Mkdir(filepath.Join(dir, "metrics.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := New(dir)
	if err := r.Save(); err == nil {
		t.Fatal("Save() = nil, want rename failure to surface")
	}
}

// 成功路径返回 nil，且文件真实落盘。
func TestSaveSucceedsNormally(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(Record{Model: "m", Site: "de", PromptTokens: 1, Status: StatusOK})
	if err := r.Save(); err != nil {
		t.Fatalf("Save() = %v, want nil", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "metrics.json"))
	if err != nil {
		t.Fatalf("read persisted metrics: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("metrics.json is empty")
	}
}
