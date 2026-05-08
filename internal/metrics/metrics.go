package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	Registry *prometheus.Registry

	RequestsAllowed           prometheus.Counter
	RequestsBlocked           prometheus.Counter
	RequestsBlockedIndividual prometheus.Counter
	RequestsBlockedGlobal     prometheus.Counter

	CountersKnownActive   prometheus.Gauge
	CountersUnknownActive prometheus.Gauge
	MemoryBytes           prometheus.Gauge

	CleanupRunsTotal           prometheus.Counter
	CleanupDeletedTotal        prometheus.Counter
	CleanupTransferredTotal    prometheus.Counter
	CleanupLastDurationSeconds prometheus.Gauge

	RedisErrorsTotal prometheus.Counter
	RedisDB1Keys     prometheus.Gauge
	RedisDB2Keys     prometheus.Gauge
	RedisDB3Keys     prometheus.Gauge

	CheckDurationSeconds prometheus.Histogram
}

func New() *Metrics {
	reg := prometheus.NewRegistry()

	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ratelimit_requests_total",
		Help: "Total ratelimit requests by result",
	}, []string{"result"})

	m := &Metrics{
		Registry:                  reg,
		RequestsAllowed:           requestsTotal.WithLabelValues("allowed"),
		RequestsBlocked:           requestsTotal.WithLabelValues("blocked"),
		RequestsBlockedIndividual: requestsTotal.WithLabelValues("blocked_individual"),
		RequestsBlockedGlobal:     requestsTotal.WithLabelValues("blocked_global"),

		CountersKnownActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_counters_known_active",
			Help: "Active counters in KnownCounters map",
		}),
		CountersUnknownActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_counters_unknown_active",
			Help: "Active counters in UnknownCounters map",
		}),
		MemoryBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_memory_bytes",
			Help: "Approximate memory used by both counter maps",
		}),

		CleanupRunsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_cleanup_runs_total",
			Help: "Number of cleanup cycles run",
		}),
		CleanupDeletedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_cleanup_deleted_total",
			Help: "Counters deleted by cleanup",
		}),
		CleanupTransferredTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_cleanup_transferred_total",
			Help: "Counters transferred to redis by cleanup",
		}),
		CleanupLastDurationSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_cleanup_last_duration_seconds",
			Help: "Duration of the last cleanup cycle",
		}),

		RedisErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_redis_errors_total",
			Help: "Redis errors observed",
		}),
		RedisDB1Keys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_redis_db1_keys",
			Help: "Keys in redisDB1",
		}),
		RedisDB2Keys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_redis_db2_keys",
			Help: "Keys in redisDB2",
		}),
		RedisDB3Keys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ratelimit_redis_db3_keys",
			Help: "Keys in redisDB3",
		}),

		CheckDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ratelimit_check_duration_seconds",
			Help:    "Duration of /check handler",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14),
		}),
	}

	reg.MustRegister(
		requestsTotal,
		m.CountersKnownActive,
		m.CountersUnknownActive,
		m.MemoryBytes,
		m.CleanupRunsTotal,
		m.CleanupDeletedTotal,
		m.CleanupTransferredTotal,
		m.CleanupLastDurationSeconds,
		m.RedisErrorsTotal,
		m.RedisDB1Keys,
		m.RedisDB2Keys,
		m.RedisDB3Keys,
		m.CheckDurationSeconds,
	)
	return m
}
