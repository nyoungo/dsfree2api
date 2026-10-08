package metrics

import "testing"

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
