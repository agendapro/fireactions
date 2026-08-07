package cache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// namespace is declared locally rather than imported from package server:
// server imports server/cache, so the reverse import would be a cycle.
const namespace = "fireactions"

var (
	metricCacheUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name:      "cache_up",
		Namespace: namespace,
		Help:      "Is the cache component up",
	})

	metricCacheDiskBytesUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name:      "cache_disk_bytes_used",
		Namespace: namespace,
		Help:      "Total bytes used by the cache data directory",
	})

	metricCacheEvictionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name:      "cache_evictions_total",
		Namespace: namespace,
		Help:      "Total number of cache entries evicted by the sweep",
	})
)
