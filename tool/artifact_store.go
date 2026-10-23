package tool

import (
	"context"
	"sync"
	"time"
)

// ArtifactStore holds runtime data stashed by tools so the rows never need
// to enter the model context. Tools return small ArtifactRef handles in
// their ModelObservation; downstream tools (notably render_from_ref)
// resolve the handle server-side via Get.
//
// The default implementation (NewMemoryArtifactStore) keeps everything
// in-process behind a TTL — adequate for single-instance deployments and
// tests. Multi-instance deployments should plug in a shared implementation
// (Redis, …) keyed by the same id; the neutral tool_use_id flowing through
// the block IR is a natural key family.
type ArtifactStore interface {
	// Put stores value under id. kind is the canonical shape tag
	// (ShapeTableRecord, ShapeTimeSeries, …).
	Put(ctx context.Context, id, kind string, value any) error
	// Get returns the value previously stored under id, or ok=false if it
	// is missing or evicted.
	Get(ctx context.Context, id string) (kind string, value any, ok bool)
}

type memoryArtifactStore struct {
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]memoryArtifactEntry
}

type memoryArtifactEntry struct {
	kind     string
	value    any
	expireAt time.Time
}

// NewMemoryArtifactStore builds an in-process store with the given TTL.
// Pass 0 to default to 30 minutes — long enough to outlive a typical chat
// turn including human-in-the-loop pauses, short enough to bound memory.
func NewMemoryArtifactStore(ttl time.Duration) ArtifactStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &memoryArtifactStore{ttl: ttl, items: map[string]memoryArtifactEntry{}}
}

func (s *memoryArtifactStore) Put(_ context.Context, id, kind string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.items[id] = memoryArtifactEntry{kind: kind, value: value, expireAt: time.Now().Add(s.ttl)}
	return nil
}

func (s *memoryArtifactStore) Get(_ context.Context, id string) (string, any, bool) {
	s.mu.RLock()
	e, ok := s.items[id]
	s.mu.RUnlock()
	if !ok {
		return "", nil, false
	}
	if time.Now().After(e.expireAt) {
		s.mu.Lock()
		delete(s.items, id)
		s.mu.Unlock()
		return "", nil, false
	}
	return e.kind, e.value, true
}

// gcLocked drops expired entries opportunistically on Put. Caller holds the
// write lock. No background sweeper: one Put per tool call is a natural
// cadence and avoids leaking a goroutine per Registry.
func (s *memoryArtifactStore) gcLocked() {
	now := time.Now()
	for k, e := range s.items {
		if now.After(e.expireAt) {
			delete(s.items, k)
		}
	}
}
