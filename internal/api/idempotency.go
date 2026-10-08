package api

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Idempotency-Key semantics (Stripe / OpenAI conventions): a retried POST that
// carries the same key and body must not hit the upstream twice.
//
//   - the key's scope is (API key, method+path, Idempotency-Key);
//   - same key + different body        → 400 idempotency_error;
//   - same key, first request          → executes, registered as in-flight;
//   - same key while in-flight         → 409 idempotency_error;
//   - same key, completed              → byte-for-byte replay with
//     `idempotent-replayed: true`;
//   - response larger than the per-entry cap, or a write that failed → the
//     entry becomes unreplayable (409) or is dropped so the retry can re-run.
//
// The cache is in-process, bounded and TTL'd; it never touches disk.
const (
	idemTTL       = 24 * time.Hour
	idemCapacity  = 1024
	idemMaxBody   = 1 << 20  // 1 MiB per recorded response
	idemMaxTotal  = 64 << 20 // 64 MiB across all recorded responses
	idemKeyMaxLen = 255      // header length cap
)

var idempotentPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/responses":        true,
	"/v1/messages":         true,
}

type idemKind int

const (
	idemFresh idemKind = iota
	idemReplay
	idemInProgress
	idemConflict
	idemUnreplayable
)

type idemRecorded struct {
	status      int
	contentType string
	body        []byte
}

type idemEntry struct {
	fp      [32]byte
	created time.Time
	mu      sync.Mutex
	state   idemState
	rec     idemRecorded
}

type idemState int

const (
	idemStateInFlight idemState = iota
	idemStateCompleted
	idemStateUnreplayable
)

func (e *idemEntry) recordedSize() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == idemStateCompleted {
		return len(e.rec.body)
	}
	return 0
}

func (e *idemEntry) isCompleted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == idemStateCompleted
}

type idemBeginResult struct {
	kind     idemKind
	recorded idemRecorded
	guard    *idemGuard
}

type idempotencyStore struct {
	mu            sync.Mutex
	entries       map[string]*idemEntry
	order         []string
	recordedBytes int
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{entries: map[string]*idemEntry{}}
}

func (s *idempotencyStore) begin(scope, key string, fp [32]byte) idemBeginResult {
	cacheKey := scope + "\x01" + key

	s.mu.Lock()
	s.evictExpiredLocked()
	if e, ok := s.entries[cacheKey]; ok {
		s.mu.Unlock()
		if e.fp != fp {
			return idemBeginResult{kind: idemConflict}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		switch e.state {
		case idemStateInFlight:
			return idemBeginResult{kind: idemInProgress}
		case idemStateCompleted:
			rec := e.rec
			rec.body = append([]byte(nil), e.rec.body...)
			return idemBeginResult{kind: idemReplay, recorded: rec}
		default:
			return idemBeginResult{kind: idemUnreplayable}
		}
	}
	e := &idemEntry{fp: fp, created: time.Now(), state: idemStateInFlight}
	s.entries[cacheKey] = e
	s.order = append(s.order, cacheKey)
	s.evictOverflowLocked()
	s.mu.Unlock()

	return idemBeginResult{kind: idemFresh, guard: &idemGuard{store: s, cacheKey: cacheKey, entry: e}}
}

func (s *idempotencyStore) commit(cacheKey string, e *idemEntry, rec idemRecorded) {
	rec.body = append([]byte(nil), rec.body...)
	e.mu.Lock()
	e.state = idemStateCompleted
	e.rec = rec
	e.mu.Unlock()

	s.mu.Lock()
	s.recordedBytes += len(rec.body)
	s.enforceBudgetLocked(cacheKey)
	s.mu.Unlock()
}

func (s *idempotencyStore) markUnreplayable(cacheKey string, e *idemEntry) {
	e.mu.Lock()
	e.state = idemStateUnreplayable
	e.rec = idemRecorded{}
	e.mu.Unlock()
}

func (s *idempotencyStore) remove(cacheKey string, e *idemEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.entries[cacheKey]; ok && cur == e {
		delete(s.entries, cacheKey)
		s.order = removeString(s.order, cacheKey)
		s.recordedBytes -= e.recordedSize()
		if s.recordedBytes < 0 {
			s.recordedBytes = 0
		}
	}
}

func (s *idempotencyStore) evictExpiredLocked() {
	now := time.Now()
	for key, e := range s.entries {
		if now.Sub(e.created) > idemTTL {
			delete(s.entries, key)
			s.order = removeString(s.order, key)
			s.recordedBytes -= e.recordedSize()
		}
	}
	if s.recordedBytes < 0 {
		s.recordedBytes = 0
	}
}

func (s *idempotencyStore) evictOverflowLocked() {
	for len(s.entries) > idemCapacity {
		if len(s.order) == 0 {
			return
		}
		oldest := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.entries[oldest]; ok {
			delete(s.entries, oldest)
			s.recordedBytes -= e.recordedSize()
		}
	}
	if s.recordedBytes < 0 {
		s.recordedBytes = 0
	}
}

// enforceBudgetLocked drops the oldest completed entries until the recorded
// body budget fits, never touching the entry just committed.
func (s *idempotencyStore) enforceBudgetLocked(keep string) {
	for s.recordedBytes > idemMaxTotal {
		idx := -1
		for i, key := range s.order {
			if key == keep {
				continue
			}
			if e, ok := s.entries[key]; ok && e.isCompleted() {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		key := s.order[idx]
		e := s.entries[key]
		delete(s.entries, key)
		s.order = append(s.order[:idx], s.order[idx+1:]...)
		s.recordedBytes -= e.recordedSize()
	}
	if s.recordedBytes < 0 {
		s.recordedBytes = 0
	}
}

// idemGuard owns a fresh in-flight entry. finish commits the recorded response;
// release (deferred) removes the placeholders if finish never ran (panic or an
// early return), so a retry can genuinely re-run.
type idemGuard struct {
	store    *idempotencyStore
	cacheKey string
	entry    *idemEntry
	finished bool
}

func (g *idemGuard) finish(status int, contentType string, body []byte, overflow, writeErr bool) {
	if g.finished {
		return
	}
	g.finished = true
	switch {
	case overflow:
		g.store.markUnreplayable(g.cacheKey, g.entry)
	case writeErr:
		g.store.remove(g.cacheKey, g.entry)
	default:
		g.store.commit(g.cacheKey, g.entry, idemRecorded{status: status, contentType: contentType, body: body})
	}
}

func (g *idemGuard) release() {
	if !g.finished {
		g.finished = true
		g.store.remove(g.cacheKey, g.entry)
	}
}

func idemFingerprint(scope, key string, body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// idempotency wraps POST endpoints with Idempotency-Key support. Requests
// without the header pass straight through.
func (s *Server) idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !idempotentPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > idemKeyMaxLen {
			idemFail(w, r.URL.Path, http.StatusBadRequest, "Idempotency-Key must be at most 255 characters")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
		if err != nil {
			idemFail(w, r.URL.Path, http.StatusBadRequest, "failed to read request body")
			return
		}
		_ = r.Body.Close()
		if len(body) > maxRequestBodyBytes {
			idemFail(w, r.URL.Path, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// A request that will fail auth must not occupy a key: the scope
		// includes the API key, so only engage for authenticated callers.
		token := extractKey(r)
		s.cfg.RLock()
		keys := append([]string(nil), s.cfg.Security.APIKeys...)
		s.cfg.RUnlock()
		if len(keys) > 0 && (token == "" || !matchesAny(keys, token)) {
			next.ServeHTTP(w, r)
			return
		}

		scope := token + "|" + r.Method + " " + r.URL.Path
		fp := idemFingerprint(scope, key, body)

		result := s.idem.begin(scope, key, fp)
		switch result.kind {
		case idemReplay:
			rec := result.recorded
			if rec.contentType != "" {
				w.Header().Set("Content-Type", rec.contentType)
			}
			w.Header().Set("idempotent-replayed", "true")
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.body)
			return
		case idemInProgress:
			idemFail(w, r.URL.Path, http.StatusConflict, "a request with this Idempotency-Key is still in progress; retry later or use a new key")
			return
		case idemConflict:
			idemFail(w, r.URL.Path, http.StatusBadRequest, "Idempotency-Key was reused with a different request body; use a new key")
			return
		case idemUnreplayable:
			idemFail(w, r.URL.Path, http.StatusConflict, "the response for this Idempotency-Key could not be cached for replay; use a new key")
			return
		}

		guard := result.guard
		defer guard.release()
		rw := &recordingWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		guard.finish(rw.statusCode(), rw.contentType(), rw.buf, rw.overflow, rw.writeErr)
	})
}

func idemFail(w http.ResponseWriter, path string, code int, msg string) {
	if path == "/v1/messages" {
		writeAnthropicError(w, code, msg, "invalid_request_error")
		return
	}
	writeError(w, code, msg, "idempotency_error")
}

// recordingWriter tees the handler's response into a bounded buffer while
// forwarding it to the real writer.
type recordingWriter struct {
	http.ResponseWriter
	status   int
	wroteHdr bool
	ct       string
	buf      []byte
	overflow bool
	writeErr bool
}

func (rw *recordingWriter) WriteHeader(code int) {
	if !rw.wroteHdr {
		rw.wroteHdr = true
		rw.status = code
		rw.ct = rw.Header().Get("Content-Type")
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	if !rw.wroteHdr {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	if err != nil {
		rw.writeErr = true
	}
	if !rw.overflow {
		if len(rw.buf)+len(b) > idemMaxBody {
			rw.overflow = true
			rw.buf = nil
		} else {
			rw.buf = append(rw.buf, b...)
		}
	}
	return n, err
}

func (rw *recordingWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *recordingWriter) statusCode() int {
	if rw.status == 0 {
		return http.StatusOK
	}
	return rw.status
}

func (rw *recordingWriter) contentType() string { return rw.ct }
