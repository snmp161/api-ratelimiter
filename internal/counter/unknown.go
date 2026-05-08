package counter

import (
	"sync"
	"time"
)

const (
	UnknownKeyPrefix = "key:"
	UnknownIPPrefix  = "ip:"
)

type UnknownCounter struct {
	FirstRequest time.Time
	LastRequest  time.Time
	Total        int64
	WindowCount  int64
	Slot         int64
	BurstHits    int64
	AbuseHits    int64
}

type UnknownMap struct {
	mu       sync.RWMutex
	counters map[string]*UnknownCounter
	now      func() time.Time
	windowS  int64
}

func NewUnknownMap(windowSeconds int64, now func() time.Time) *UnknownMap {
	if now == nil {
		now = time.Now
	}
	return &UnknownMap{
		counters: make(map[string]*UnknownCounter),
		now:      now,
		windowS:  windowSeconds,
	}
}

// RecordRequest applies the global limit and counts AbuseHits when the
// previous slot exceeded global_limit * abuseMultiplier.
func (m *UnknownMap) RecordRequest(key string, globalLimit, burst, abuseMultiplier int64) Decision {
	now := m.now()
	currentSlot := now.Unix() / m.windowS

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.counters[key]
	if !ok {
		c = &UnknownCounter{
			FirstRequest: now,
			LastRequest:  now,
			Slot:         currentSlot,
		}
		m.counters[key] = c
	}

	if currentSlot != c.Slot {
		if c.WindowCount > globalLimit*abuseMultiplier {
			c.AbuseHits++
		}
		c.Slot = currentSlot
		c.WindowCount = 0
	}

	c.WindowCount++
	c.Total++
	c.LastRequest = now

	d := Decision{
		WindowCount: c.WindowCount,
		Limit:       globalLimit,
		Burst:       burst,
	}

	if c.WindowCount > globalLimit+burst {
		d.Allowed = false
		return d
	}

	if c.WindowCount > globalLimit {
		c.BurstHits++
		d.InBurst = true
	}
	d.Allowed = true
	return d
}

type UnknownSnapshot struct {
	Key string
	UnknownCounter
}

func (m *UnknownMap) Snapshot() []UnknownSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]UnknownSnapshot, 0, len(m.counters))
	for k, c := range m.counters {
		out = append(out, UnknownSnapshot{Key: k, UnknownCounter: *c})
	}
	return out
}

func (m *UnknownMap) Get(key string) (UnknownCounter, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.counters[key]
	if !ok {
		return UnknownCounter{}, false
	}
	return *c, true
}

func (m *UnknownMap) Delete(key string) {
	m.mu.Lock()
	delete(m.counters, key)
	m.mu.Unlock()
}

func (m *UnknownMap) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.counters)
}

func (m *UnknownMap) SizeBytes() int64 {
	const perEntry = 16 + 80
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for k := range m.counters {
		total += int64(len(k)) + perEntry
	}
	return total
}

// IsInactive reports whether the counter has not been touched in the current slot.
func (m *UnknownMap) IsInactive(c UnknownCounter, now time.Time) bool {
	currentSlot := now.Unix() / m.windowS
	return currentSlot != c.Slot && now.Sub(c.LastRequest) >= time.Duration(m.windowS)*time.Second
}

// IsInactiveKnown is the same predicate for KnownCounter (kept here so both
// share the windowS configuration without callers needing to know about it).
func (m *KnownMap) IsInactive(c KnownCounter, now time.Time) bool {
	currentSlot := now.Unix() / m.windowS
	return currentSlot != c.Slot && now.Sub(c.LastRequest) >= time.Duration(m.windowS)*time.Second
}
