package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), "")
	t.Cleanup(s.Close)
	return s, mr
}

func TestLookupLimit_Found(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:abc", "limit", "500", "created_at", "1700000000")

	limit, found, err := s.LookupLimit(context.Background(), "abc")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !found {
		t.Fatal("expected found")
	}
	if limit != 500 {
		t.Fatalf("limit=%d want 500", limit)
	}
}

func TestLookupLimit_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	_, found, err := s.LookupLimit(context.Background(), "missing")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Fatal("expected not found")
	}
}

func TestLookupLimit_ParseError(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:bad", "limit", "not-a-number")

	_, _, err := s.LookupLimit(context.Background(), "bad")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLimitExists(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:k1", "limit", "100")

	ctx := context.Background()
	if ok, _ := s.LimitExists(ctx, "k1"); !ok {
		t.Fatal("k1 must exist")
	}
	if ok, _ := s.LimitExists(ctx, "k2"); ok {
		t.Fatal("k2 must not exist")
	}
}

func TestUpsertAbuseKey_WritesFieldsAndTTL(t *testing.T) {
	s, mr := newTestStore(t)
	rec := AbuseRecord{
		FirstSeen: 1700000000, LastSeen: 1700001000,
		TotalRequests: 1000, BurstHits: 5, AbuseHits: 3,
	}
	if err := s.UpsertAbuseKey(context.Background(), "k1", rec, 5*time.Minute); err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	mr.Select(DBAbuseKey)
	got := mr.HGet("rate:abuse:key:k1", "total_requests")
	if got != "1000" {
		t.Fatalf("total_requests=%q want 1000", got)
	}
	if mr.HGet("rate:abuse:key:k1", "abuse_hits") != "3" {
		t.Fatal("abuse_hits mismatch")
	}
	ttl := mr.TTL("rate:abuse:key:k1")
	if ttl != 5*time.Minute {
		t.Fatalf("ttl=%v want 5m", ttl)
	}
}

func TestUpsertAbuseIP_WritesFieldsAndTTL(t *testing.T) {
	s, mr := newTestStore(t)
	rec := AbuseRecord{FirstSeen: 1, LastSeen: 2, TotalRequests: 10, BurstHits: 1, AbuseHits: 1}
	if err := s.UpsertAbuseIP(context.Background(), "1.2.3.4", rec, 1*time.Hour); err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	mr.Select(DBAbuseIP)
	if mr.HGet("rate:abuse:ip:1.2.3.4", "total_requests") != "10" {
		t.Fatal("total_requests mismatch")
	}
	if mr.TTL("rate:abuse:ip:1.2.3.4") != time.Hour {
		t.Fatal("ttl mismatch")
	}
}

func TestScanLimits(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:a", "limit", "10", "created_at", "100")
	mr.HSet("rate:limit:b", "limit", "20", "created_at", "200")

	out, err := s.ScanLimits(context.Background())
	if err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d entries want 2", len(out))
	}
	m := map[string]LimitEntry{out[0].APIKey: out[0], out[1].APIKey: out[1]}
	if m["a"].Limit != 10 || m["b"].Limit != 20 {
		t.Fatalf("wrong limits: %+v", m)
	}
}

func TestScanAbuseKeys(t *testing.T) {
	s, mr := newTestStore(t)
	rec := AbuseRecord{FirstSeen: 1, LastSeen: 2, TotalRequests: 100, BurstHits: 4, AbuseHits: 2}
	if err := s.UpsertAbuseKey(context.Background(), "k1", rec, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = mr // keep for symmetry / cleanup is via t.Cleanup
	out, err := s.ScanAbuseKeys(context.Background())
	if err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d entries want 1", len(out))
	}
	e := out[0]
	if e.Key != "k1" {
		t.Fatalf("key=%q want k1", e.Key)
	}
	if e.TotalRequests != 100 || e.BurstHits != 4 || e.AbuseHits != 2 {
		t.Fatalf("counters mismatch: %+v", e)
	}
	if e.TTL <= 0 || e.TTL > 30*time.Minute {
		t.Fatalf("ttl=%v not in (0, 30m]", e.TTL)
	}
}

func TestScanAbuseIPs(t *testing.T) {
	s, _ := newTestStore(t)
	rec := AbuseRecord{FirstSeen: 1, LastSeen: 2, TotalRequests: 50}
	if err := s.UpsertAbuseIP(context.Background(), "1.2.3.4", rec, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	out, err := s.ScanAbuseIPs(context.Background())
	if err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if len(out) != 1 || out[0].Key != "1.2.3.4" {
		t.Fatalf("unexpected entries: %+v", out)
	}
}

func TestDeleteLimits(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")
	mr.HSet("rate:limit:b", "limit", "2")
	mr.HSet("rate:limit:c", "limit", "3")

	n, err := s.DeleteLimits(context.Background(), []string{"a", "c", "missing"})
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted=%d want 2", n)
	}
	if !mr.Exists("rate:limit:b") {
		t.Fatal("b must remain")
	}
	if mr.Exists("rate:limit:a") || mr.Exists("rate:limit:c") {
		t.Fatal("a and c must be gone")
	}
}

func TestDeleteAbuseKeys_And_IPs(t *testing.T) {
	s, mr := newTestStore(t)
	if err := s.UpsertAbuseKey(context.Background(), "k1", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAbuseIP(context.Background(), "1.1.1.1", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}

	if n, _ := s.DeleteAbuseKeys(context.Background(), []string{"k1"}); n != 1 {
		t.Fatalf("abuseK delete=%d want 1", n)
	}
	if n, _ := s.DeleteAbuseIPs(context.Background(), []string{"1.1.1.1"}); n != 1 {
		t.Fatalf("abuseIP delete=%d want 1", n)
	}
	mr.Select(DBAbuseKey)
	if mr.Exists("rate:abuse:key:k1") {
		t.Fatal("k1 must be gone")
	}
	mr.Select(DBAbuseIP)
	if mr.Exists("rate:abuse:ip:1.1.1.1") {
		t.Fatal("ip must be gone")
	}
}

func TestDeleteLimits_EmptyInputIsNoOp(t *testing.T) {
	s, _ := newTestStore(t)
	n, err := s.DeleteLimits(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("empty delete: n=%d err=%v", n, err)
	}
}

func TestPurge(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")
	mr.HSet("rate:limit:b", "limit", "2")

	if err := s.PurgeLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mr.DB(DBLimits).Keys() != nil && len(mr.DB(DBLimits).Keys()) != 0 {
		t.Fatal("DBLimits must be empty")
	}

	if err := s.UpsertAbuseKey(context.Background(), "k", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeAbuseKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mr.DB(DBAbuseKey).Keys()) != 0 {
		t.Fatal("DBAbuseKey must be empty")
	}

	if err := s.UpsertAbuseIP(context.Background(), "ip", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeAbuseIPs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mr.DB(DBAbuseIP).Keys()) != 0 {
		t.Fatal("DBAbuseIP must be empty")
	}
}

func TestDBSize(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Select(DBLimits)
	mr.HSet("rate:limit:a", "limit", "1")
	mr.HSet("rate:limit:b", "limit", "2")
	if err := s.UpsertAbuseKey(context.Background(), "k1", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAbuseIP(context.Background(), "1.1.1.1", AbuseRecord{}, time.Minute); err != nil {
		t.Fatal(err)
	}

	d1, d2, d3, err := s.DBSize(context.Background())
	if err != nil {
		t.Fatalf("dbsize err: %v", err)
	}
	if d1 != 2 || d2 != 1 || d3 != 1 {
		t.Fatalf("dbsize got %d/%d/%d want 2/1/1", d1, d2, d3)
	}
}

func TestPing(t *testing.T) {
	s, mr := newTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("ping ok store: %v", err)
	}
	// Simulate Redis going away.
	mr.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("ping must fail when redis is dead")
	}
}
