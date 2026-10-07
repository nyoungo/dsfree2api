package api

import (
	"sync"
	"time"
)

// limiter is a simple fixed one-minute window counter per API key.
type limiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func newLimiter() *limiter {
	return &limiter{counts: map[string]int{}, window: time.Now().Truncate(time.Minute)}
}

func (l *limiter) allow(key string) bool {
	return l.allowWithLimit(key, 0)
}

func (l *limiter) allowWithLimit(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Truncate(time.Minute)
	if now.After(l.window) {
		l.window = now
		l.counts = map[string]int{}
	}
	if l.counts[key] >= limit {
		return false
	}
	l.counts[key]++
	return true
}
