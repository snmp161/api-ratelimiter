// SPDX-License-Identifier: Apache-2.0

package limiter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"api-ratelimiter/internal/counter"
	"api-ratelimiter/internal/metrics"
)

type fakeStore struct {
	limits map[string]int64
	err    error
}

func (f *fakeStore) LookupLimit(_ context.Context, k string) (int64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	v, ok := f.limits[k]
	return v, ok, nil
}

func newTestLimiter(t *testing.T, store LimitLookup, globalLimit, burst, abuseMul int64) *Limiter {
	t.Helper()
	c := &clk{t: time.Unix(100, 0)}
	known := counter.NewKnownMap(1, c.now)
	unknown := counter.NewUnknownMap(1, c.now)
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(known, unknown, store, m, logger, globalLimit, burst, abuseMul)
}

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func TestRouting_KnownKeyUsesIndividualLimit(t *testing.T) {
	store := &fakeStore{limits: map[string]int64{"abc": 3}}
	l := newTestLimiter(t, store, 100, 0, 10)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if !l.Decide(ctx, "abc", "1.1.1.1") {
			t.Fatalf("req %d: should be allowed (within individual limit 3)", i)
		}
	}
	if l.Decide(ctx, "abc", "1.1.1.1") {
		t.Fatal("4th request must be blocked by individual limit")
	}
	if l.known.Len() != 1 {
		t.Fatal("expected counter in KnownMap")
	}
	if l.unknown.Len() != 0 {
		t.Fatal("UnknownMap must remain empty for known key")
	}
}

func TestRouting_UnknownKeyUsesGlobalLimit(t *testing.T) {
	store := &fakeStore{limits: map[string]int64{}}
	l := newTestLimiter(t, store, 2, 0, 10)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if !l.Decide(ctx, "xyz", "1.1.1.1") {
			t.Fatalf("req %d should be allowed (within global limit 2)", i)
		}
	}
	if l.Decide(ctx, "xyz", "1.1.1.1") {
		t.Fatal("3rd request must be blocked by global limit")
	}
	if l.known.Len() != 0 {
		t.Fatal("KnownMap must remain empty when key not in redisDB1")
	}
	if l.unknown.Len() != 1 {
		t.Fatalf("expected counter in UnknownMap, got %d", l.unknown.Len())
	}
}

func TestRouting_NoKeyFallsBackToIP(t *testing.T) {
	store := &fakeStore{limits: map[string]int64{}}
	l := newTestLimiter(t, store, 1, 0, 10)
	ctx := context.Background()
	if !l.Decide(ctx, "", "1.2.3.4") {
		t.Fatal("first IP-based request should be allowed")
	}
	if l.Decide(ctx, "", "1.2.3.4") {
		t.Fatal("second IP-based request should be blocked by global limit 1")
	}
}

func TestRouting_RedisErrorFallsBackToGlobal(t *testing.T) {
	store := &fakeStore{err: errors.New("connection refused")}
	l := newTestLimiter(t, store, 1, 0, 10)
	ctx := context.Background()
	if !l.Decide(ctx, "abc", "1.1.1.1") {
		t.Fatal("first request should be allowed (fallback to global)")
	}
	if l.Decide(ctx, "abc", "1.1.1.1") {
		t.Fatal("second request should be blocked by global limit 1 (fallback path)")
	}
	if l.known.Len() != 0 {
		t.Fatal("KnownMap must remain empty when Redis lookup fails")
	}
	if l.unknown.Len() != 1 {
		t.Fatal("expected one counter in UnknownMap after Redis failure")
	}
}

func TestRouting_NoKeyNoIPFailOpenWithoutKey(t *testing.T) {
	store := &fakeStore{limits: map[string]int64{}}
	l := newTestLimiter(t, store, 1, 0, 10)
	for i := 0; i < 5; i++ {
		if !l.Decide(context.Background(), "", "") {
			t.Fatalf("req %d: must be allowed (fail open without key)", i)
		}
	}
}
