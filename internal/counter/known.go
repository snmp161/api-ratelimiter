package counter

import (
	"sync"
	"time"
)

type KnownCounter struct {
	FirstRequest  time.Time
	LastRequest   time.Time
	Total         int64
	WindowCount   int64
	Slot          int64
	BurstHits     int64
	ViolationHits int64
}

type KnownMap struct {
	mu       sync.RWMutex
	counters map[string]*KnownCounter
	now      func() time.Time
	windowS  int64
}

func NewKnownMap(windowSeconds int64, now func() time.Time) *KnownMap {
	if now == nil {
		now = time.Now
	}
	return &KnownMap{
		counters: make(map[string]*KnownCounter),
		now:      now,
		windowS:  windowSeconds,
	}
}

// Decision summarises the outcome for a single request.
type Decision struct {
	Allowed     bool
	WindowCount int64
	Limit       int64
	Burst       int64
	InBurst     bool
}

// RecordRequest registers an incoming request for the given key, applying the
// limit fetched from redisDB1. Returns the decision.
func (m *KnownMap) RecordRequest(key string, limit, burst int64) Decision {
	now := m.now()
	currentSlot := now.Unix() / m.windowS

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.counters[key]
	if !ok {
		c = &KnownCounter{
			FirstRequest: now,
			LastRequest:  now,
			Slot:         currentSlot,
		}
		m.counters[key] = c
	}

	if currentSlot != c.Slot {
		if c.WindowCount > limit {
			c.ViolationHits++
		}
		c.Slot = currentSlot
		c.WindowCount = 0
	}

	c.WindowCount++
	c.Total++
	c.LastRequest = now

	d := Decision{
		WindowCount: c.WindowCount,
		Limit:       limit,
		Burst:       burst,
	}

	if c.WindowCount > limit+burst {
		d.Allowed = false
		return d
	}

	if c.WindowCount > limit {
		c.BurstHits++
		d.InBurst = true
	}
	d.Allowed = true
	return d
}

// Snapshot returns a stable copy for cleanup / admin reads.
type KnownSnapshot struct {
	Key string
	KnownCounter
}

func (m *KnownMap) Snapshot() []KnownSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]KnownSnapshot, 0, len(m.counters))
	for k, c := range m.counters {
		out = append(out, KnownSnapshot{Key: k, KnownCounter: *c})
	}
	return out
}

func (m *KnownMap) Get(key string) (KnownCounter, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.counters[key]
	if !ok {
		return KnownCounter{}, false
	}
	return *c, true
}

func (m *KnownMap) Delete(key string) {
	m.mu.Lock()
	delete(m.counters, key)
	m.mu.Unlock()
}

func (m *KnownMap) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.counters)
}

// SizeBytes is a rough estimate (used for memory metric).
func (m *KnownMap) SizeBytes() int64 {
	const perEntry = 16 + 80 // key string header + struct
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for k := range m.counters {
		total += int64(len(k)) + perEntry
	}
	return total
}
