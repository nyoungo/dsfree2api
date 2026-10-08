package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

func authHeaders(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
}

// ── H2: Responses store ──────────────────────────────────────────

func TestGetResponseUnknownIs404(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := get(t, srv.URL+"/v1/responses/resp_missing", map[string]string{"Authorization": "Bearer " + testKey})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", resp.StatusCode, body)
	}
	if openaiErrType(body) == "" {
		t.Errorf("expected an error body, got %s", body)
	}
}

func TestPreviousResponseIDUnknownIs400(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := post(t, srv.URL+"/v1/responses", authHeaders(testKey),
		`{"model":"deepseek-v4-flash-de","input":"hi","previous_response_id":"resp_missing"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
}

func TestResponseStoreEvictsAndExpires(t *testing.T) {
	s := newResponseStore(2, time.Minute)
	s.insert("a", openai.StoredTurn{InputText: "1"})
	s.insert("b", openai.StoredTurn{InputText: "2"})
	s.insert("c", openai.StoredTurn{InputText: "3"})
	if _, ok := s.get("a"); ok {
		t.Fatal("oldest entry must be evicted")
	}
	if turn, ok := s.get("b"); !ok || turn.InputText != "2" {
		t.Fatalf("b = %+v ok=%v", turn, ok)
	}
	if s.len() != 2 {
		t.Fatalf("len = %d, want 2", s.len())
	}

	// Expire c manually and confirm get treats it as missing.
	s.mu.Lock()
	e := s.entries["c"]
	e.inserted = time.Now().Add(-time.Hour)
	s.entries["c"] = e
	s.mu.Unlock()
	if _, ok := s.get("c"); ok {
		t.Fatal("expired entry must be missing")
	}
	if s.len() != 1 {
		t.Fatalf("len after expiry = %d, want 1", s.len())
	}
}

func TestResponseStoreGetResponse(t *testing.T) {
	s := newResponseStore(4, time.Minute)
	s.insert("r1", openai.StoredTurn{Response: map[string]any{"id": "r1", "object": "response"}})
	snap, ok := s.getResponse("r1")
	if !ok || snap["object"] != "response" {
		t.Fatalf("snapshot = %v ok=%v", snap, ok)
	}
	if _, ok := s.getResponse("nope"); ok {
		t.Fatal("unknown id must miss")
	}
}

// ── H3: Idempotency ──────────────────────────────────────────────

func TestIdempotencyReplaysCompletedResponse(t *testing.T) {
	srv := newTestServer(t, nil)
	h := authHeaders(testKey)
	h["Idempotency-Key"] = "idem-replay"
	body := `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`

	resp, first := post(t, srv.URL+"/v1/chat/completions", h, body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("first status = %d body=%s, want 404", resp.StatusCode, first)
	}
	if resp.Header.Get("idempotent-replayed") != "" {
		t.Fatalf("first response must not be marked replayed")
	}

	resp, second := post(t, srv.URL+"/v1/chat/completions", h, body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replay status = %d body=%s, want 404", resp.StatusCode, second)
	}
	if resp.Header.Get("idempotent-replayed") != "true" {
		t.Fatalf("replay header missing; headers=%v", resp.Header)
	}
	if second != first {
		t.Fatalf("replayed body differs:\n first=%s\nsecond=%s", first, second)
	}
}

func TestIdempotencyConflictDifferentBody(t *testing.T) {
	srv := newTestServer(t, nil)
	h := authHeaders(testKey)
	h["Idempotency-Key"] = "idem-conflict"

	if resp, _ := post(t, srv.URL+"/v1/chat/completions", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"a"}]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("first status = %d, want 404", resp.StatusCode)
	}
	resp, body := post(t, srv.URL+"/v1/chat/completions", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"b"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflict status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if openaiErrType(body) != "idempotency_error" {
		t.Fatalf("error type = %q, want idempotency_error; body=%s", openaiErrType(body), body)
	}
}

func TestIdempotencyRejectsLongKey(t *testing.T) {
	srv := newTestServer(t, nil)
	h := authHeaders(testKey)
	h["Idempotency-Key"] = strings.Repeat("k", 256)
	resp, body := post(t, srv.URL+"/v1/chat/completions", h, `{"model":"x","messages":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
}

func TestIdempotencyAnthropicErrorShape(t *testing.T) {
	srv := newTestServer(t, nil)
	h := authHeaders(testKey)
	h["Idempotency-Key"] = "idem-anthropic"
	h["Content-Type"] = "application/json"

	// Two different bodies → conflict, rendered in Anthropic error shape.
	if resp, _ := post(t, srv.URL+"/v1/messages", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"a"}]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("first status = %d, want 404", resp.StatusCode)
	}
	resp, body := post(t, srv.URL+"/v1/messages", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"b"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if got := anthropicErrType(body); got != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error; body=%s", got, body)
	}
}

func TestIdempotencyStoreLifecycle(t *testing.T) {
	s := newIdempotencyStore()
	fp := idemFingerprint("scope", "k", []byte("body"))

	first := s.begin("scope", "k", fp)
	if first.kind != idemFresh || first.guard == nil {
		t.Fatalf("first begin = %+v, want fresh", first)
	}
	if got := s.begin("scope", "k", fp); got.kind != idemInProgress {
		t.Fatalf("second begin = %v, want in-progress", got.kind)
	}
	if got := s.begin("scope", "k", idemFingerprint("scope", "k", []byte("other"))); got.kind != idemConflict {
		t.Fatalf("different body = %v, want conflict", got.kind)
	}

	first.guard.finish(200, "application/json", []byte(`{"ok":true}`), false, false)
	replay := s.begin("scope", "k", fp)
	if replay.kind != idemReplay || replay.recorded.status != 200 || string(replay.recorded.body) != `{"ok":true}` {
		t.Fatalf("replay = %+v", replay)
	}

	// Overflow marks the entry unreplayable (409 on the next try).
	ov := s.begin("scope", "ov", idemFingerprint("scope", "ov", []byte("x")))
	ov.guard.finish(200, "application/json", []byte("big"), true, false)
	if got := s.begin("scope", "ov", idemFingerprint("scope", "ov", []byte("x"))); got.kind != idemUnreplayable {
		t.Fatalf("overflow begin = %v, want unreplayable", got.kind)
	}

	// A dropped (unfinished) guard frees the key so a retry can re-run.
	dropped := s.begin("scope", "drop", idemFingerprint("scope", "drop", []byte("y")))
	dropped.guard.release()
	if got := s.begin("scope", "drop", idemFingerprint("scope", "drop", []byte("y"))); got.kind != idemFresh {
		t.Fatalf("after release begin = %v, want fresh", got.kind)
	}
}
