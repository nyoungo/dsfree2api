package api

import (
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

// responseStore is an in-process, bounded, TTL cache backing the Responses
// API's previous_response_id and GET /v1/responses/{id}. It is intentionally
// small: the gateway is stateless and only needs to reconstruct context for
// the next turn, so it keeps the minimum needed, never the raw request body.
type responseStore struct {
	mu       sync.Mutex
	entries  map[string]responseEntry
	order    []string
	capacity int
	ttl      time.Duration
}

type responseEntry struct {
	turn     openai.StoredTurn
	inserted time.Time
}

func newResponseStore(capacity int, ttl time.Duration) *responseStore {
	if capacity < 1 {
		capacity = 1
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &responseStore{
		entries:  map[string]responseEntry{},
		capacity: capacity,
		ttl:      ttl,
	}
}

// insert saves a turn, evicting the oldest entry when over capacity.
func (s *responseStore) insert(id string, turn openai.StoredTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[id]; exists {
		s.order = removeString(s.order, id)
	}
	s.entries[id] = responseEntry{turn: turn, inserted: time.Now()}
	s.order = append(s.order, id)
	for len(s.order) > s.capacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// get returns a live turn; an expired entry is treated as missing.
func (s *responseStore) get(id string) (openai.StoredTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return openai.StoredTurn{}, false
	}
	if time.Since(entry.inserted) > s.ttl {
		delete(s.entries, id)
		s.order = removeString(s.order, id)
		return openai.StoredTurn{}, false
	}
	return entry.turn, true
}

func (s *responseStore) getResponse(id string) (map[string]any, bool) {
	turn, ok := s.get(id)
	if !ok {
		return nil, false
	}
	return turn.Response, true
}

func (s *responseStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func removeString(list []string, value string) []string {
	out := list[:0]
	for _, v := range list {
		if v != value {
			out = append(out, v)
		}
	}
	return out
}
