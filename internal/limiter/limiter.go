package limiter

import (
	"context"
	"log/slog"
	"time"

	"ratelimiter/internal/counter"
	"ratelimiter/internal/metrics"
)

// LimitLookup matches store.Store.LookupLimit. Pulled out to allow tests to
// inject fakes without depending on a real Redis.
type LimitLookup interface {
	LookupLimit(ctx context.Context, apiKey string) (limit int64, found bool, err error)
}

type Limiter struct {
	known           *counter.KnownMap
	unknown         *counter.UnknownMap
	store           LimitLookup
	metrics         *metrics.Metrics
	logger          *slog.Logger
	globalLimit     int64
	burst           int64
	abuseMultiplier int64
	lookupTimeout   time.Duration
}

func New(
	known *counter.KnownMap,
	unknown *counter.UnknownMap,
	store LimitLookup,
	m *metrics.Metrics,
	logger *slog.Logger,
	globalLimit, burst, abuseMultiplier int64,
) *Limiter {
	return &Limiter{
		known:           known,
		unknown:         unknown,
		store:           store,
		metrics:         m,
		logger:          logger,
		globalLimit:     globalLimit,
		burst:           burst,
		abuseMultiplier: abuseMultiplier,
		lookupTimeout:   100 * time.Millisecond,
	}
}

// Decide is the entry point invoked by the HTTP handler. It returns true when
// the request should be allowed. Never panics — caller still has fail-open
// recover, but Decide itself is defensive too.
func (l *Limiter) Decide(ctx context.Context, apiKey, ip string) bool {
	// Edge case: nothing to key on. Fail safe → allow.
	if apiKey == "" && ip == "" {
		l.metrics.RequestsAllowed.Inc()
		return true
	}

	if apiKey != "" {
		return l.decideWithKey(ctx, apiKey)
	}
	return l.decideWithIP(ip)
}

func (l *Limiter) decideWithKey(ctx context.Context, apiKey string) bool {
	lookupCtx, cancel := context.WithTimeout(ctx, l.lookupTimeout)
	defer cancel()

	limit, found, err := l.store.LookupLimit(lookupCtx, apiKey)
	if err != nil {
		l.metrics.RedisErrorsTotal.Inc()
		l.logger.Warn("redis lookup failed, falling back to global limit", "err", err)
		found = false
	}

	if found {
		d := l.known.RecordRequest(apiKey, limit, l.burst)
		if d.Allowed {
			l.metrics.RequestsAllowed.Inc()
			return true
		}
		l.metrics.RequestsBlocked.Inc()
		l.metrics.RequestsBlockedIndividual.Inc()
		return false
	}

	mapKey := counter.UnknownKeyPrefix + apiKey
	d := l.unknown.RecordRequest(mapKey, l.globalLimit, l.burst, l.abuseMultiplier)
	if d.Allowed {
		l.metrics.RequestsAllowed.Inc()
		return true
	}
	l.metrics.RequestsBlocked.Inc()
	l.metrics.RequestsBlockedGlobal.Inc()
	return false
}

func (l *Limiter) decideWithIP(ip string) bool {
	mapKey := counter.UnknownIPPrefix + ip
	d := l.unknown.RecordRequest(mapKey, l.globalLimit, l.burst, l.abuseMultiplier)
	if d.Allowed {
		l.metrics.RequestsAllowed.Inc()
		return true
	}
	l.metrics.RequestsBlocked.Inc()
	l.metrics.RequestsBlockedGlobal.Inc()
	return false
}
