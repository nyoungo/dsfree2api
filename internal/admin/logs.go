package admin

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nyoungo/dsfree2api/internal/logbuf"
)

// handleLogs serves the ring buffer either as a one-shot JSON payload or as a
// live SSE stream (?stream=1).
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	level := q.Get("level")
	if level == "" {
		level = "INFO"
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	if q.Get("stream") != "1" {
		writeJSON(w, http.StatusOK, map[string]any{"entries": s.logs.Recent(level, limit)})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, e := range s.logs.Recent(level, limit) {
		writeLog(w, e)
	}
	flusher.Flush()

	ch, unsub := s.logs.Subscribe()
	defer unsub()
	minLevel := level
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !levelOK(e.Level, minLevel) {
				continue
			}
			writeLog(w, e)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeLog(w http.ResponseWriter, e logbuf.Entry) {
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", raw)
}

func levelOK(actual, min string) bool {
	return levelVal(actual) >= levelVal(min)
}

func levelVal(l string) int {
	switch l {
	case "DEBUG":
		return -4
	case "INFO":
		return 0
	case "WARN":
		return 4
	case "ERROR":
		return 8
	default:
		return -100
	}
}

func randRead(b []byte) (int, error) { return rand.Read(b) }
