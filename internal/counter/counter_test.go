package counter

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestKnown_IncrementsWindowAndTotal(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)

	for i := 1; i <= 5; i++ {
		d := m.RecordRequest("k", 10, 0)
		if !d.Allowed {
			t.Fatalf("request %d: expected allowed", i)
		}
		if d.WindowCount != int64(i) {
			t.Fatalf("request %d: WindowCount=%d", i, d.WindowCount)
		}
	}
	got, _ := m.Get("k")
	if got.Total != 5 {
		t.Fatalf("Total=%d want 5", got.Total)
	}
	if got.WindowCount != 5 {
		t.Fatalf("WindowCount=%d want 5", got.WindowCount)
	}
}

func TestKnown_SlotChangeResetsWindowKeepsTotal(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)

	for i := 0; i < 3; i++ {
		m.RecordRequest("k", 10, 0)
	}
	c.t = time.Unix(101, 0)
	d := m.RecordRequest("k", 10, 0)
	if d.WindowCount != 1 {
		t.Fatalf("after slot change WindowCount=%d want 1", d.WindowCount)
	}
	got, _ := m.Get("k")
	if got.Total != 4 {
		t.Fatalf("Total=%d want 4", got.Total)
	}
}

func TestKnown_AllowedAtLimit(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	for i := 0; i < 10; i++ {
		d := m.RecordRequest("k", 10, 0)
		if !d.Allowed {
			t.Fatalf("at WindowCount=%d expected allowed", d.WindowCount)
		}
	}
}

func TestKnown_BlockedOverLimitNoBurst(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	for i := 0; i < 10; i++ {
		m.RecordRequest("k", 10, 0)
	}
	d := m.RecordRequest("k", 10, 0)
	if d.Allowed {
		t.Fatal("11th request must be blocked")
	}
}

func TestKnown_BurstZoneAllowedAndCounted(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	// limit=10, burst=5 → effective 15
	for i := 0; i < 10; i++ {
		m.RecordRequest("k", 10, 5)
	}
	for i := 0; i < 5; i++ {
		d := m.RecordRequest("k", 10, 5)
		if !d.Allowed {
			t.Fatalf("burst req %d should be allowed", i)
		}
		if !d.InBurst {
			t.Fatalf("burst req %d should be marked InBurst", i)
		}
	}
	d := m.RecordRequest("k", 10, 5)
	if d.Allowed {
		t.Fatal("16th request must be blocked")
	}
	got, _ := m.Get("k")
	if got.BurstHits != 5 {
		t.Fatalf("BurstHits=%d want 5", got.BurstHits)
	}
}

func TestKnown_ViolationHitsOnSlotChange(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	// fill above limit (rely on burst to allow)
	for i := 0; i < 14; i++ {
		m.RecordRequest("k", 10, 5)
	}
	got, _ := m.Get("k")
	if got.ViolationHits != 0 {
		t.Fatalf("ViolationHits=%d want 0 before slot change", got.ViolationHits)
	}
	c.t = time.Unix(101, 0)
	m.RecordRequest("k", 10, 5)
	got, _ = m.Get("k")
	if got.ViolationHits != 1 {
		t.Fatalf("ViolationHits=%d want 1 after slot change with prev WindowCount > limit", got.ViolationHits)
	}
}

func TestKnown_ViolationHitsNotIncrementedWhenAtOrBelowLimit(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	for i := 0; i < 10; i++ {
		m.RecordRequest("k", 10, 5)
	}
	c.t = time.Unix(101, 0)
	m.RecordRequest("k", 10, 5)
	got, _ := m.Get("k")
	if got.ViolationHits != 0 {
		t.Fatalf("ViolationHits=%d want 0 (prev slot was at limit)", got.ViolationHits)
	}
}

func TestKnown_FirstAndLastRequest(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	m.RecordRequest("k", 10, 0)
	c.t = time.Unix(150, 0)
	m.RecordRequest("k", 10, 0)
	got, _ := m.Get("k")
	if got.FirstRequest.Unix() != 100 {
		t.Fatalf("FirstRequest=%d want 100", got.FirstRequest.Unix())
	}
	if got.LastRequest.Unix() != 150 {
		t.Fatalf("LastRequest=%d want 150", got.LastRequest.Unix())
	}
}

func TestUnknown_AbuseHitsOnSlotChange(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(1, c.now)
	// global=10, multiplier=10, threshold=100
	// fill 105 requests but burst is small so most blocked but WindowCount keeps growing
	for i := 0; i < 105; i++ {
		m.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	}
	c.t = time.Unix(101, 0)
	m.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	got, _ := m.Get("ip:1.1.1.1")
	if got.AbuseHits != 1 {
		t.Fatalf("AbuseHits=%d want 1", got.AbuseHits)
	}
}

func TestUnknown_AbuseHitsNotIncrementedAtThreshold(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(1, c.now)
	for i := 0; i < 100; i++ {
		m.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	}
	c.t = time.Unix(101, 0)
	m.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	got, _ := m.Get("ip:1.1.1.1")
	if got.AbuseHits != 0 {
		t.Fatalf("AbuseHits=%d want 0 (prev was at threshold, not over)", got.AbuseHits)
	}
}

func TestUnknown_BlockedOverLimitPlusBurst(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(1, c.now)
	for i := 0; i < 12; i++ {
		m.RecordRequest("ip:1.1.1.1", 10, 2, 10)
	}
	d := m.RecordRequest("ip:1.1.1.1", 10, 2, 10)
	if d.Allowed {
		t.Fatal("13th request must be blocked when limit=10 burst=2")
	}
}

func TestUnknown_BurstHits(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(1, c.now)
	// limit=10, burst=2 — burst zone is requests 11 and 12
	for i := 0; i < 10; i++ {
		m.RecordRequest("ip:1.1.1.1", 10, 2, 10)
	}
	for i := 0; i < 2; i++ {
		d := m.RecordRequest("ip:1.1.1.1", 10, 2, 10)
		if !d.Allowed || !d.InBurst {
			t.Fatalf("burst req %d: allowed=%v inBurst=%v", i, d.Allowed, d.InBurst)
		}
	}
	got, _ := m.Get("ip:1.1.1.1")
	if got.BurstHits != 2 {
		t.Fatalf("BurstHits=%d want 2", got.BurstHits)
	}
}

func TestUnknown_NamespaceKeysDoNotCollide(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(1, c.now)
	m.RecordRequest("key:abc", 10, 0, 10)
	m.RecordRequest("ip:abc", 10, 0, 10)
	if m.Len() != 2 {
		t.Fatalf("expected 2 distinct counters, got %d", m.Len())
	}
}

func TestKnown_IsInactive(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now)
	m.RecordRequest("k", 10, 0)
	got, _ := m.Get("k")
	if m.IsInactive(got, time.Unix(100, 0)) {
		t.Fatal("counter should be active in current slot")
	}
	if !m.IsInactive(got, time.Unix(102, 0)) {
		t.Fatal("counter should be inactive 2s later")
	}
}
