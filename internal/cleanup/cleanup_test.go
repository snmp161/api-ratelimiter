package cleanup

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"ratelimiter/internal/counter"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

type fakeStore struct {
	mu        sync.Mutex
	exists    map[string]bool
	abuseK    map[string]store.AbuseRecord
	abuseIP   map[string]store.AbuseRecord
	keyTTL    map[string]time.Duration
	ipTTL     map[string]time.Duration
	dbErr     error
	upsertEr  error
	unhealthy bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		exists:  map[string]bool{},
		abuseK:  map[string]store.AbuseRecord{},
		abuseIP: map[string]store.AbuseRecord{},
		keyTTL:  map[string]time.Duration{},
		ipTTL:   map[string]time.Duration{},
	}
}

func (f *fakeStore) IsHealthy() bool { return !f.unhealthy }

func (f *fakeStore) LimitExists(_ context.Context, k string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[k], nil
}
func (f *fakeStore) UpsertAbuseKey(_ context.Context, k string, r store.AbuseRecord, ttl time.Duration) error {
	if f.upsertEr != nil {
		return f.upsertEr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abuseK[k] = r
	f.keyTTL[k] = ttl
	return nil
}
func (f *fakeStore) UpsertAbuseIP(_ context.Context, ip string, r store.AbuseRecord, ttl time.Duration) error {
	if f.upsertEr != nil {
		return f.upsertEr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abuseIP[ip] = r
	f.ipTTL[ip] = ttl
	return nil
}
func (f *fakeStore) DBSize(_ context.Context) (int64, int64, int64, error) {
	if f.dbErr != nil {
		return 0, 0, 0, f.dbErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return 0, int64(len(f.abuseK)), int64(len(f.abuseIP)), nil
}

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func newSetup(t *testing.T) (*counter.KnownMap, *counter.UnknownMap, *fakeStore, *Cleanup, *clk) {
	t.Helper()
	c := &clk{t: time.Unix(1000, 0)}
	known := counter.NewKnownMap(1, c.now)
	unknown := counter.NewUnknownMap(1, c.now)
	fs := newFakeStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := New(known, unknown, fs, metrics.New(), logger, 3, 15*time.Minute)
	cl.now = c.now
	return known, unknown, fs, cl, c
}

func TestCleanup_KnownInactiveDeleted(t *testing.T) {
	known, _, fs, cl, c := newSetup(t)
	fs.exists["k1"] = true
	known.RecordRequest("k1", 10, 0)
	c.t = time.Unix(1100, 0) // far in the future, > one slot
	cl.Run(context.Background())
	if known.Len() != 0 {
		t.Fatalf("KnownCounters should be empty, got %d", known.Len())
	}
}

func TestCleanup_KnownDroppedWhenLimitVanished(t *testing.T) {
	known, _, fs, cl, _ := newSetup(t)
	known.RecordRequest("k1", 10, 0)
	fs.exists["k1"] = false
	cl.Run(context.Background())
	if known.Len() != 0 {
		t.Fatal("KnownCounter must be deleted when key vanished from redisDB1")
	}
}

func TestCleanup_KnownActiveKept(t *testing.T) {
	known, _, fs, cl, _ := newSetup(t)
	fs.exists["k1"] = true
	known.RecordRequest("k1", 10, 0)
	cl.Run(context.Background())
	if known.Len() != 1 {
		t.Fatal("active KnownCounter must remain")
	}
}

func TestCleanup_UnknownInactiveBelowThresholdDeleted(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	c.t = time.Unix(1100, 0)
	cl.Run(context.Background())
	if unknown.Len() != 0 {
		t.Fatal("inactive unknown counter below threshold must be deleted")
	}
	if len(fs.abuseIP) != 0 {
		t.Fatal("must not write to redis when below threshold")
	}
}

func TestCleanup_UnknownInactiveAboveThresholdTransferredAndDeleted(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	// drive AbuseHits above threshold by triggering 4 slot changes after going over
	for slot := 0; slot < 4; slot++ {
		c.t = time.Unix(int64(1000+slot), 0)
		for i := 0; i < 105; i++ {
			unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
		}
	}
	// trigger one more slot to bake the last AbuseHit
	c.t = time.Unix(1004, 0)
	unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)

	got, _ := unknown.Get("ip:1.1.1.1")
	if got.AbuseHits < 3 {
		t.Fatalf("setup wrong: AbuseHits=%d", got.AbuseHits)
	}

	c.t = time.Unix(1100, 0) // make inactive
	cl.Run(context.Background())

	if unknown.Len() != 0 {
		t.Fatal("inactive unknown counter above threshold must be deleted from memory")
	}
	if _, ok := fs.abuseIP["1.1.1.1"]; !ok {
		t.Fatal("must be transferred to redisDB3")
	}
	if fs.ipTTL["1.1.1.1"] != 15*time.Minute {
		t.Fatalf("TTL=%v want 15m", fs.ipTTL["1.1.1.1"])
	}
}

func TestCleanup_UnknownActiveAboveThresholdTransferredKept(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	for slot := 0; slot < 4; slot++ {
		c.t = time.Unix(int64(1000+slot), 0)
		for i := 0; i < 105; i++ {
			unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
		}
	}
	c.t = time.Unix(1004, 0)
	unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10) // makes counter active in current slot
	// Don't bump c.t — counter is still active.
	cl.Run(context.Background())

	if unknown.Len() != 1 {
		t.Fatal("active unknown counter must remain in memory")
	}
	if _, ok := fs.abuseIP["1.1.1.1"]; !ok {
		t.Fatal("active counter above threshold must be upserted to redis")
	}
}

func TestCleanup_UpsertPayloadMatchesCounter(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	// build a single ip:1.1.1.1 counter past threshold
	for slot := 0; slot < 4; slot++ {
		c.t = time.Unix(int64(1000+slot), 0)
		for i := 0; i < 105; i++ {
			unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
		}
	}
	c.t = time.Unix(1004, 0)
	unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
	got, _ := unknown.Get("ip:1.1.1.1")

	c.t = time.Unix(1100, 0)
	cl.Run(context.Background())

	r, ok := fs.abuseIP["1.1.1.1"]
	if !ok {
		t.Fatal("missing record")
	}
	if r.FirstSeen != got.FirstRequest.Unix() {
		t.Errorf("first_seen mismatch")
	}
	if r.LastSeen != got.LastRequest.Unix() {
		t.Errorf("last_seen mismatch")
	}
	if r.TotalRequests != got.Total {
		t.Errorf("total mismatch: %d vs %d", r.TotalRequests, got.Total)
	}
	if r.BurstHits != got.BurstHits {
		t.Errorf("burst mismatch")
	}
	if r.AbuseHits != got.AbuseHits {
		t.Errorf("abuse mismatch")
	}
}

func TestCleanup_SkipsTransferWhenRedisUnhealthy(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	fs.unhealthy = true

	// Build a counter past the AbuseHits threshold.
	for slot := 0; slot < 4; slot++ {
		c.t = time.Unix(int64(1000+slot), 0)
		for i := 0; i < 105; i++ {
			unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)
		}
	}
	c.t = time.Unix(1004, 0)
	unknown.RecordRequest("ip:1.1.1.1", 10, 0, 10)

	c.t = time.Unix(1100, 0) // active counter, would normally be deleted
	cl.Run(context.Background())

	if len(fs.abuseIP) != 0 {
		t.Fatal("must not upsert to redis when unhealthy")
	}
	// Counter is inactive at this point but redis is down, so we still
	// drop it from memory (the bookkeeping path stays the same; only the
	// network write is skipped).
	if unknown.Len() != 0 {
		t.Fatalf("inactive counter should still be GC'd, got %d", unknown.Len())
	}
}

func TestCleanup_KnownNeverTransferred(t *testing.T) {
	known, _, fs, cl, _ := newSetup(t)
	fs.exists["k1"] = true
	for i := 0; i < 1000; i++ {
		known.RecordRequest("k1", 5, 0)
	}
	cl.Run(context.Background())
	if len(fs.abuseK) != 0 {
		t.Fatal("KnownCounters must never be written to redisDB2")
	}
}

func TestCleanup_TransferKeyPrefix(t *testing.T) {
	_, unknown, fs, cl, c := newSetup(t)
	for slot := 0; slot < 4; slot++ {
		c.t = time.Unix(int64(1000+slot), 0)
		for i := 0; i < 105; i++ {
			unknown.RecordRequest("key:abc", 10, 0, 10)
		}
	}
	c.t = time.Unix(1004, 0)
	unknown.RecordRequest("key:abc", 10, 0, 10)
	c.t = time.Unix(1100, 0)
	cl.Run(context.Background())

	if _, ok := fs.abuseK["abc"]; !ok {
		t.Fatal("key: prefix must transfer to redisDB2 (abuseKey)")
	}
	if _, ok := fs.abuseIP["abc"]; ok {
		t.Fatal("key: prefix must NOT land in redisDB3 (abuseIP)")
	}
}
