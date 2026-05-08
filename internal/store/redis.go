package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	DBLimits   = 1
	DBAbuseKey = 2
	DBAbuseIP  = 3
)

// Store wraps three go-redis clients, one per logical database. go-redis
// pools and reconnects automatically.
type Store struct {
	limits  *redis.Client // SELECT 1
	abuseK  *redis.Client // SELECT 2
	abuseIP *redis.Client // SELECT 3
}

func New(addr, password string) *Store {
	mk := func(db int) *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     password,
			DB:           db,
			DialTimeout:  500 * time.Millisecond,
			ReadTimeout:  200 * time.Millisecond,
			WriteTimeout: 200 * time.Millisecond,
			PoolSize:     32,
		})
	}
	return &Store{
		limits:  mk(DBLimits),
		abuseK:  mk(DBAbuseKey),
		abuseIP: mk(DBAbuseIP),
	}
}

func (s *Store) Close() {
	_ = s.limits.Close()
	_ = s.abuseK.Close()
	_ = s.abuseIP.Close()
}

// LookupLimit returns (limit, found, error). limit is the per-key request
// limit per window. If err != nil the caller should treat the key as not
// present (fail open).
func (s *Store) LookupLimit(ctx context.Context, apiKey string) (int64, bool, error) {
	v, err := s.limits.HGet(ctx, "rate:limit:"+apiKey, "limit").Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	limit, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, false, perr
	}
	return limit, true, nil
}

// LimitExists checks just for existence (cleanup loop).
func (s *Store) LimitExists(ctx context.Context, apiKey string) (bool, error) {
	n, err := s.limits.Exists(ctx, "rate:limit:"+apiKey).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AbuseRecord is the payload written into redisDB2/redisDB3 by cleanup.
type AbuseRecord struct {
	FirstSeen     int64
	LastSeen      int64
	TotalRequests int64
	BurstHits     int64
	AbuseHits     int64
}

func (s *Store) UpsertAbuseKey(ctx context.Context, apiKey string, r AbuseRecord, ttl time.Duration) error {
	return s.upsertAbuse(ctx, s.abuseK, "rate:abuse:key:"+apiKey, r, ttl)
}

func (s *Store) UpsertAbuseIP(ctx context.Context, ip string, r AbuseRecord, ttl time.Duration) error {
	return s.upsertAbuse(ctx, s.abuseIP, "rate:abuse:ip:"+ip, r, ttl)
}

func (s *Store) upsertAbuse(ctx context.Context, c *redis.Client, key string, r AbuseRecord, ttl time.Duration) error {
	pipe := c.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"first_seen":     r.FirstSeen,
		"last_seen":      r.LastSeen,
		"total_requests": r.TotalRequests,
		"burst_hits":     r.BurstHits,
		"abuse_hits":     r.AbuseHits,
	})
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// AbuseEntry is what the admin UI reads from redisDB2/redisDB3.
type AbuseEntry struct {
	Key           string
	FirstSeen     time.Time
	LastSeen      time.Time
	TotalRequests int64
	BurstHits     int64
	AbuseHits     int64
	TTL           time.Duration
}

func (s *Store) ScanAbuseKeys(ctx context.Context) ([]AbuseEntry, error) {
	return scanAbuse(ctx, s.abuseK, "rate:abuse:key:*", "rate:abuse:key:")
}

func (s *Store) ScanAbuseIPs(ctx context.Context) ([]AbuseEntry, error) {
	return scanAbuse(ctx, s.abuseIP, "rate:abuse:ip:*", "rate:abuse:ip:")
}

func scanAbuse(ctx context.Context, c *redis.Client, pattern, prefix string) ([]AbuseEntry, error) {
	var (
		out    []AbuseEntry
		cursor uint64
		err    error
	)
	for {
		var keys []string
		keys, cursor, err = c.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			vals, err := c.HGetAll(ctx, k).Result()
			if err != nil {
				continue
			}
			ttl, _ := c.TTL(ctx, k).Result()
			entry := AbuseEntry{
				Key:           k[len(prefix):],
				FirstSeen:     time.Unix(parseInt64(vals["first_seen"]), 0),
				LastSeen:      time.Unix(parseInt64(vals["last_seen"]), 0),
				TotalRequests: parseInt64(vals["total_requests"]),
				BurstHits:     parseInt64(vals["burst_hits"]),
				AbuseHits:     parseInt64(vals["abuse_hits"]),
				TTL:           ttl,
			}
			out = append(out, entry)
		}
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// LimitEntry is what the admin UI reads from redisDB1.
type LimitEntry struct {
	APIKey    string
	Limit     int64
	CreatedAt time.Time
}

func (s *Store) ScanLimits(ctx context.Context) ([]LimitEntry, error) {
	const prefix = "rate:limit:"
	var (
		out    []LimitEntry
		cursor uint64
	)
	for {
		var (
			keys []string
			err  error
		)
		keys, cursor, err = s.limits.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			vals, err := s.limits.HGetAll(ctx, k).Result()
			if err != nil {
				continue
			}
			out = append(out, LimitEntry{
				APIKey:    k[len(prefix):],
				Limit:     parseInt64(vals["limit"]),
				CreatedAt: time.Unix(parseInt64(vals["created_at"]), 0),
			})
		}
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// DBSize returns the number of keys per database.
func (s *Store) DBSize(ctx context.Context) (db1, db2, db3 int64, err error) {
	if db1, err = s.limits.DBSize(ctx).Result(); err != nil {
		return
	}
	if db2, err = s.abuseK.DBSize(ctx).Result(); err != nil {
		return
	}
	db3, err = s.abuseIP.DBSize(ctx).Result()
	return
}

func (s *Store) Ping(ctx context.Context) error {
	return s.limits.Ping(ctx).Err()
}

// ───── delete / purge (admin actions) ──────────────────────────────────

// DeleteLimits removes the given api keys from redisDB1.
func (s *Store) DeleteLimits(ctx context.Context, apiKeys []string) (int64, error) {
	return delByPrefix(ctx, s.limits, "rate:limit:", apiKeys)
}

// DeleteAbuseKeys removes the given api keys from redisDB2.
func (s *Store) DeleteAbuseKeys(ctx context.Context, apiKeys []string) (int64, error) {
	return delByPrefix(ctx, s.abuseK, "rate:abuse:key:", apiKeys)
}

// DeleteAbuseIPs removes the given IPs from redisDB3.
func (s *Store) DeleteAbuseIPs(ctx context.Context, ips []string) (int64, error) {
	return delByPrefix(ctx, s.abuseIP, "rate:abuse:ip:", ips)
}

func delByPrefix(ctx context.Context, c *redis.Client, prefix string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = prefix + id
	}
	return c.Del(ctx, keys...).Result()
}

// PurgeLimits / PurgeAbuseKeys / PurgeAbuseIPs flush the corresponding DB.
// Spec mandates a dedicated Redis instance, so FLUSHDB is safe — there are
// no foreign keys to lose.
func (s *Store) PurgeLimits(ctx context.Context) error    { return s.limits.FlushDB(ctx).Err() }
func (s *Store) PurgeAbuseKeys(ctx context.Context) error { return s.abuseK.FlushDB(ctx).Err() }
func (s *Store) PurgeAbuseIPs(ctx context.Context) error  { return s.abuseIP.FlushDB(ctx).Err() }
