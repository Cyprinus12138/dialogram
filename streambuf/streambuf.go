// Package streambuf persists in-flight streaming events keyed by a
// client-supplied message id so that a client can disconnect (tab switch,
// network drop, mobile backgrounding) and reconnect to resume from where it
// left off — the pattern the major chat products use.
//
// The producer goroutine that runs the model call writes events to the
// store with Append and finalises with Finish. The HTTP handler tails the
// store with Tail, blocking on new events until the stream completes.
// Multiple tails of the same message are allowed; only one Start succeeds
// and owns the producer side.
package streambuf

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is how long a finished stream is retained for resume.
const DefaultTTL = 30 * time.Minute

// Terminal stream statuses. Finish accepts any string; these are the
// conventions the sse package writes and clients switch on.
const (
	StatusDone        = "done"
	StatusWaiting     = "waiting"
	StatusErrorPrefix = "error:"
)

// ErrorStatus formats an error message as a terminal status.
func ErrorStatus(msg string) string { return StatusErrorPrefix + msg }

// IsError reports whether a status is an error status.
func IsError(status string) bool { return strings.HasPrefix(status, StatusErrorPrefix) }

// Event is one serialised stream event (JSON) plus its 1-based sequence
// number within the message. The seq is written on the wire as the SSE
// `id:` field so clients can reconnect with a resume cursor.
type Event struct {
	Seq  int
	Data string
}

// Store is the contract for pluggable persistence. Implementations must be
// safe for concurrent use.
type Store interface {
	// Start atomically claims ownership of msgID for the producer side.
	// Returns true if this caller should run the model call, false if the
	// message already exists — the caller should only Tail.
	Start(ctx context.Context, msgID string) (bool, error)

	// Append persists one event and wakes any blocked Tail calls.
	Append(ctx context.Context, msgID string, data string) error

	// Finish marks the stream terminal. status is StatusDone on a clean
	// end, StatusWaiting on a human-in-the-loop pause, or ErrorStatus(msg)
	// on failure. Safe to call once.
	Finish(ctx context.Context, msgID string, status string) error

	// Tail returns events starting after the `since` offset (0 means from
	// the start). Blocks up to `timeout` for new events when none are
	// available. The returned status is "" while in progress (keep
	// tailing) or the terminal status once finished.
	Tail(ctx context.Context, msgID string, since int, timeout time.Duration) (events []Event, status string, err error)
}

// ---------------- in-memory implementation ----------------

// MemStore keeps buffers in a process-local map. Fine for single-instance
// deployments; use RedisStore when scaling horizontally.
type MemStore struct {
	mu      sync.Mutex
	entries map[string]*memEntry
	ttl     time.Duration
	stop    chan struct{}
}

type memEntry struct {
	mu       sync.Mutex
	events   []string
	status   string
	started  bool
	waiters  []chan struct{}
	expireAt time.Time
}

// NewMemStore creates an in-memory store. Finished buffers expire after
// ttl (DefaultTTL when <= 0); expired entries are reaped in the background
// once a minute. Call Close to stop the reaper.
func NewMemStore(ttl time.Duration) *MemStore {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	s := &MemStore{entries: map[string]*memEntry{}, ttl: ttl, stop: make(chan struct{})}
	go s.reaper(time.Minute)
	return s
}

// Close stops the background reaper.
func (s *MemStore) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return nil
}

func (s *MemStore) reaper(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Reap(time.Now())
		}
	}
}

// Reap drops finished entries whose expiry is before now. Exposed so tests
// and operators can trigger it deterministically.
func (s *MemStore) Reap(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, e := range s.entries {
		e.mu.Lock()
		expired := now.After(e.expireAt) && e.status != ""
		e.mu.Unlock()
		if expired {
			delete(s.entries, id)
			n++
		}
	}
	return n
}

// Len reports the number of live buffers.
func (s *MemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *MemStore) getOrCreate(msgID string) *memEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[msgID]
	if !ok {
		e = &memEntry{expireAt: time.Now().Add(s.ttl)}
		s.entries[msgID] = e
	}
	return e
}

func (s *MemStore) get(msgID string) *memEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[msgID]
}

// Start implements Store.
func (s *MemStore) Start(_ context.Context, msgID string) (bool, error) {
	e := s.getOrCreate(msgID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return false, nil
	}
	e.started = true
	return true, nil
}

// Append implements Store.
func (s *MemStore) Append(_ context.Context, msgID string, data string) error {
	e := s.get(msgID)
	if e == nil {
		return fmt.Errorf("streambuf: no such message %q", msgID)
	}
	e.mu.Lock()
	e.events = append(e.events, data)
	e.expireAt = time.Now().Add(s.ttl)
	waiters := e.waiters
	e.waiters = nil
	e.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
	return nil
}

// Finish implements Store. The first status wins.
func (s *MemStore) Finish(_ context.Context, msgID string, status string) error {
	e := s.get(msgID)
	if e == nil {
		return fmt.Errorf("streambuf: no such message %q", msgID)
	}
	e.mu.Lock()
	if e.status == "" {
		e.status = status
	}
	e.expireAt = time.Now().Add(s.ttl)
	waiters := e.waiters
	e.waiters = nil
	e.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
	return nil
}

// Tail implements Store.
func (s *MemStore) Tail(ctx context.Context, msgID string, since int, timeout time.Duration) ([]Event, string, error) {
	if since < 0 {
		since = 0
	}
	e := s.getOrCreate(msgID)
	for {
		e.mu.Lock()
		if len(e.events) > since {
			out := make([]Event, 0, len(e.events)-since)
			for i := since; i < len(e.events); i++ {
				out = append(out, Event{Seq: i + 1, Data: e.events[i]})
			}
			status := e.status
			e.mu.Unlock()
			return out, status, nil
		}
		if e.status != "" {
			status := e.status
			e.mu.Unlock()
			return nil, status, nil
		}
		ch := make(chan struct{})
		e.waiters = append(e.waiters, ch)
		e.mu.Unlock()

		timer := time.NewTimer(timeout)
		select {
		case <-ch:
			timer.Stop()
			// Woken by Append/Finish — re-check.
		case <-timer.C:
			return nil, "", nil
		case <-ctx.Done():
			timer.Stop()
			return nil, "", ctx.Err()
		}
	}
}
