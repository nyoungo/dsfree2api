// Package logbuf keeps a ring buffer of log entries and fans them out to
// live subscribers for the web console.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Entry struct {
	TS    time.Time         `json:"ts"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	subs    map[int]chan Entry
	nextID  int
	limit   int
}

func New(limit int) *Buffer {
	if limit <= 0 {
		limit = 500
	}
	return &Buffer{subs: map[int]chan Entry{}, limit: limit}
}

// Add appends an entry and fans it out. The sends stay under the lock: an
// unlocked send races Subscribe's unsubscribe closure (which closes the
// channel under the same lock) and "send on closed channel" is a panic that
// takes down whatever goroutine was logging. The sends never block, so the
// lock is only held for a bounded number of non-blocking sends.
func (b *Buffer) Add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, e)
	if len(b.entries) > b.limit {
		b.entries = b.entries[len(b.entries)-b.limit:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// slow consumer: drop rather than block the logger
		}
	}
}

// Recent returns up to n entries, optionally filtered by minimum level.
func (b *Buffer) Recent(minLevel string, n int) []Entry {
	min := levelValue(minLevel)
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, n)
	for i := len(b.entries) - 1; i >= 0 && len(out) < n; i-- {
		if levelValue(b.entries[i].Level) >= min {
			out = append(out, b.entries[i])
		}
	}
	// reverse to chronological order
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Subscribe returns a channel of new entries plus an unsubscribe func.
func (b *Buffer) Subscribe() (<-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Entry, 128)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

func levelValue(l string) int {
	switch l {
	case "DEBUG", "debug":
		return -4
	case "INFO", "info":
		return 0
	case "WARN", "WARNING", "warn", "warning":
		return 4
	case "ERROR", "error":
		return 8
	default:
		return -100
	}
}

// Handler is an slog.Handler that mirrors records into the buffer.
type Handler struct {
	parent slog.Handler
	buf    *Buffer
	attrs  []slog.Attr
	group  string
}

func NewHandler(parent slog.Handler, buf *Buffer) *Handler {
	return &Handler{parent: parent, buf: buf}
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	return h.parent.Enabled(context.Background(), l)
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	for _, a := range h.attrs {
		attrs[a.Key] = fmtValue(a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = fmtValue(a.Value)
		return true
	})
	if h.group != "" {
		grouped := map[string]string{}
		for k, v := range attrs {
			grouped[h.group+"."+k] = v
		}
		attrs = grouped
	}
	level := "INFO"
	switch {
	case r.Level >= slog.LevelError:
		level = "ERROR"
	case r.Level >= slog.LevelWarn:
		level = "WARN"
	case r.Level >= slog.LevelInfo:
		level = "INFO"
	default:
		level = "DEBUG"
	}
	h.buf.Add(Entry{TS: r.Time, Level: level, Msg: r.Message, Attrs: attrs})
	return h.parent.Handle(context.Background(), r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &cp
}

func (h *Handler) WithGroup(name string) slog.Handler {
	cp := *h
	if h.group == "" {
		cp.group = name
	} else {
		cp.group = h.group + "." + name
	}
	return &cp
}

func fmtValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return itoa(v.Int64())
	case slog.KindUint64:
		return utoa(v.Uint64())
	case slog.KindFloat64:
		return ftoa(v.Float64())
	case slog.KindBool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		return anyString(v.Any())
	default:
		return v.String()
	}
}

func anyString(v any) string {
	switch t := v.(type) {
	case error:
		if t == nil {
			return ""
		}
		return t.Error()
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func utoa(v uint64) string { return itoa(int64(v)) }

func ftoa(f float64) string {
	return fmt.Sprintf("%g", f)
}
