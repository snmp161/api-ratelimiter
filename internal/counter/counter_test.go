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

func TestKnown_SizeBytes_AccountsForStructAndKey(t *testing.T) {
	m := NewKnownMap(1, nil)
	if m.SizeBytes() != 0 {
		t.Fatalf("empty map: SizeBytes=%d want 0", m.SizeBytes())
	}
	m.RecordRequest("abc", 10, 0)
	got := m.SizeBytes()
	// Per-entry: 16 (string header) + 8 (map ptr) + 17 (bucket) + 88 (struct) + 3 (len("abc"))
	wantMin := int64(80 + 3) // generous lower bound
	if got < wantMin {
		t.Fatalf("SizeBytes=%d, expected at least %d for one entry", got, wantMin)
	}
	// Add another entry; total should grow by (perEntry + 5).
	before := got
	m.RecordRequest("xy", 10, 0)
	delta := m.SizeBytes() - before
	if delta < 50 || delta > 200 {
		t.Errorf("delta=%d looks wrong (one entry + 2-char key should be ~130b)", delta)
	}
}

func TestUnknown_SizeBytes_GrowsWithEntries(t *testing.T) {
	m := NewUnknownMap(1, nil)
	if m.SizeBytes() != 0 {
		t.Fatalf("empty map: SizeBytes=%d want 0", m.SizeBytes())
	}
	for i := 0; i < 100; i++ {
		// fmt.Sprintf would pull in fmt; cheap manual unique key.
		m.RecordRequest("key:"+string(rune('a'+i%26))+string(rune('a'+i/26)), 10, 0, 10)
	}
	if m.Len() != 100 {
		t.Fatalf("setup: expected 100 unique counters, got %d", m.Len())
	}
	got := m.SizeBytes()
	// 100 entries × ~130 bytes each = ~13000 minimum
	if got < 10000 {
		t.Errorf("SizeBytes=%d, expected >= 10000 for 100 entries", got)
	}
}

func TestKnown_IsInactive(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewKnownMap(1, c.now) // window = 1s, inactivity threshold = 2*window = 2s

	m.RecordRequest("k", 10, 0)
	got, _ := m.Get("k")

	if m.IsInactive(got, time.Unix(100, 0)) {
		t.Fatal("counter should be active in current slot")
	}
	// 1s later: slot changed but only 1s ≤ 2*window — still active (the
	// 2×window gap smooths over short idle pauses).
	if m.IsInactive(got, time.Unix(101, 0)) {
		t.Fatal("1s after last request: should still be active (< 2*window)")
	}
	// 2s later: hit the 2*window boundary — inactive.
	if !m.IsInactive(got, time.Unix(102, 0)) {
		t.Fatal("2s after last request: should be inactive (>= 2*window)")
	}
}

func TestUnknown_IsInactive_TwoWindowGap(t *testing.T) {
	c := &clock{t: time.Unix(100, 0)}
	m := NewUnknownMap(60, c.now) // window = 60s, threshold = 120s

	m.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	got, _ := m.Get("ip:1.1.1.1")

	// 90s later: slot changed (100/60=1 vs 190/60=3) but only 90s — still active.
	if m.IsInactive(got, time.Unix(190, 0)) {
		t.Fatal("90s after last request (< 2*window=120s): should still be active")
	}
	// 121s later: past 2*window — inactive.
	if !m.IsInactive(got, time.Unix(221, 0)) {
		t.Fatal("121s after last request: should be inactive")
	}
}
