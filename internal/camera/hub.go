package camera

import (
	"bytes"
	"sync"
)

// Event is a named snapshot, sent to clients as an SSE event.
type Event struct {
	Name string
	Data []byte
}

// hub fans snapshots out to subscribers. Only the latest snapshot per name
// matters, so a slow subscriber gets the newest one instead of a backlog.
type hub struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
	last map[string][]byte
	// order keeps snapshots in first-publish order for new subscribers.
	order []string
}

func newHub() *hub {
	return &hub{subs: map[*Subscription]struct{}{}, last: map[string][]byte{}}
}

// publish sends data to every subscriber unless it equals the last
// snapshot of that name. It reports whether anything was sent.
func (h *hub) publish(name string, data []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.last[name]; ok && bytes.Equal(old, data) {
		return false
	}
	if _, ok := h.last[name]; !ok {
		h.order = append(h.order, name)
	}
	h.last[name] = data
	for s := range h.subs {
		s.put(Event{Name: name, Data: data})
	}
	return true
}

func (h *hub) subscribe() *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &Subscription{notify: make(chan struct{}, 1), h: h}
	for _, name := range h.order {
		s.put(Event{Name: name, Data: h.last[name]})
	}
	h.subs[s] = struct{}{}
	return s
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Subscription receives snapshots. Wait on Ready, then call Take.
type Subscription struct {
	notify chan struct{}
	h      *hub

	mu      sync.Mutex
	pending []Event
}

// Ready is signaled when Take has something.
func (s *Subscription) Ready() <-chan struct{} { return s.notify }

// Take returns the snapshots that arrived since the last call, at most one
// per name.
func (s *Subscription) Take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	return p
}

// Close unsubscribes.
func (s *Subscription) Close() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	delete(s.h.subs, s)
}

func (s *Subscription) put(e Event) {
	s.mu.Lock()
	replaced := false
	for i := range s.pending {
		if s.pending[i].Name == e.Name {
			s.pending[i] = e
			replaced = true
		}
	}
	if !replaced {
		s.pending = append(s.pending, e)
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}
