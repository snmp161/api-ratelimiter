package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"ratelimiter/internal/counter"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

// Store is the subset of store.Store used by cleanup. Defining the interface
// here keeps the cleanup package testable with fakes.
type Store interface {
	LimitExists(ctx context.Context, apiKey string) (bool, error)
	UpsertAbuseKey(ctx context.Context, apiKey string, r store.AbuseRecord, ttl time.Duration) error
	UpsertAbuseIP(ctx context.Context, ip string, r store.AbuseRecord, ttl time.Duration) error
	DBSize(ctx context.Context) (int64, int64, int64, error)
}

type Cleanup struct {
	known             *counter.KnownMap
	unknown           *counter.UnknownMap
	store             Store
	metrics           *metrics.Metrics
	logger            *slog.Logger
	transferThreshold int64
	abuseTTL          time.Duration
	now               func() time.Time
}

func New(
	known *counter.KnownMap,
	unknown *counter.UnknownMap,
	s Store,
	m *metrics.Metrics,
	logger *slog.Logger,
	transferThreshold int64,
	abuseTTL time.Duration,
) *Cleanup {
	return &Cleanup{
		known:             known,
		unknown:           unknown,
		store:             s,
		metrics:           m,
		logger:            logger,
		transferThreshold: transferThreshold,
		abuseTTL:          abuseTTL,
		now:               time.Now,
	}
}

// Run performs one cleanup cycle. Safe to call from a ticker or directly
// during shutdown.
func (c *Cleanup) Run(ctx context.Context) {
	start := c.now()
	c.metrics.CleanupRunsTotal.Inc()

	deleted, transferred := c.runKnown(ctx, start)
	d2, t2 := c.runUnknown(ctx, start)
	deleted += d2
	transferred += t2

	c.metrics.CleanupDeletedTotal.Add(float64(deleted))
	c.metrics.CleanupTransferredTotal.Add(float64(transferred))

	dur := time.Since(start)
	c.metrics.CleanupLastDurationSeconds.Set(dur.Seconds())

	c.metrics.CountersKnownActive.Set(float64(c.known.Len()))
	c.metrics.CountersUnknownActive.Set(float64(c.unknown.Len()))
	c.metrics.MemoryBytes.Set(float64(c.known.SizeBytes() + c.unknown.SizeBytes()))

	if db1, db2, db3, err := c.store.DBSize(ctx); err == nil {
		c.metrics.RedisDB1Keys.Set(float64(db1))
		c.metrics.RedisDB2Keys.Set(float64(db2))
		c.metrics.RedisDB3Keys.Set(float64(db3))
	} else {
		c.metrics.RedisErrorsTotal.Inc()
	}

	c.logger.Info("cleanup finished",
		"deleted", deleted,
		"transferred", transferred,
		"duration_ms", dur.Milliseconds(),
	)
}

func (c *Cleanup) runKnown(ctx context.Context, now time.Time) (deleted, transferred int) {
	for _, snap := range c.known.Snapshot() {
		exists, err := c.store.LimitExists(ctx, snap.Key)
		if err != nil {
			c.metrics.RedisErrorsTotal.Inc()
			c.logger.Warn("redisDB1 exists check failed", "key", snap.Key, "err", err)
			continue
		}
		if !exists {
			c.logger.Warn("api_key removed from redisDB1, dropping in-memory KnownCounter", "key", snap.Key)
			c.known.Delete(snap.Key)
			deleted++
			continue
		}
		if c.known.IsInactive(snap.KnownCounter, now) {
			c.known.Delete(snap.Key)
			deleted++
		}
	}
	return
}

func (c *Cleanup) runUnknown(ctx context.Context, now time.Time) (deleted, transferred int) {
	for _, snap := range c.unknown.Snapshot() {
		inactive := c.unknown.IsInactive(snap.UnknownCounter, now)
		shouldTransfer := snap.AbuseHits >= c.transferThreshold

		if shouldTransfer {
			if err := c.transfer(ctx, snap); err != nil {
				c.metrics.RedisErrorsTotal.Inc()
				c.logger.Warn("upsert to abuse db failed", "key", snap.Key, "err", err)
				// Even on error: keep in memory if active, drop if inactive.
				// This matches the "просто удалить из памяти" branch when we
				// can't write the record — preferable to leaking memory
				// indefinitely on persistent Redis failure.
				if inactive {
					c.unknown.Delete(snap.Key)
					deleted++
				}
				continue
			}
			transferred++
			if inactive {
				c.unknown.Delete(snap.Key)
				deleted++
			}
			continue
		}

		if inactive {
			c.unknown.Delete(snap.Key)
			deleted++
		}
	}
	return
}

func (c *Cleanup) transfer(ctx context.Context, snap counter.UnknownSnapshot) error {
	rec := store.AbuseRecord{
		FirstSeen:     snap.FirstRequest.Unix(),
		LastSeen:      snap.LastRequest.Unix(),
		TotalRequests: snap.Total,
		BurstHits:     snap.BurstHits,
		AbuseHits:     snap.AbuseHits,
	}
	switch {
	case strings.HasPrefix(snap.Key, counter.UnknownKeyPrefix):
		return c.store.UpsertAbuseKey(ctx, strings.TrimPrefix(snap.Key, counter.UnknownKeyPrefix), rec, c.abuseTTL)
	case strings.HasPrefix(snap.Key, counter.UnknownIPPrefix):
		return c.store.UpsertAbuseIP(ctx, strings.TrimPrefix(snap.Key, counter.UnknownIPPrefix), rec, c.abuseTTL)
	default:
		return errors.New("unknown counter key prefix: " + snap.Key)
	}
}

// Loop runs cleanup cycles until ctx is cancelled. The first run happens
// after one interval — same cadence the spec describes.
func (c *Cleanup) Loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						c.logger.Error("panic in cleanup loop, continuing", "recover", rec)
					}
				}()
				c.Run(ctx)
			}()
		}
	}
}
