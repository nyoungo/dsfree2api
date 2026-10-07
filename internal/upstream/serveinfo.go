package upstream

import "sync"

// ServeInfo records which model/site/proxy route actually answered a request.
// It is safe to read from the request goroutine while the producer goroutine
// is still writing.
type ServeInfo struct {
	mu      sync.Mutex
	modelID string
	site    string
	route   string
}

func (s *ServeInfo) Set(modelID, site, route string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.modelID, s.site, s.route = modelID, site, route
	s.mu.Unlock()
}

// Get returns a snapshot of the fields recorded so far.
func (s *ServeInfo) Get() (modelID, site, route string) {
	if s == nil {
		return "", "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelID, s.site, s.route
}
