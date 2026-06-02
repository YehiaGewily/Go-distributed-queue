package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// QueueDepth tracks the number of tasks in each queue.
var QueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "queue_depth",
	Help: "Current number of tasks in each queue",
}, []string{"queue"})

// TaskDuration records how long tasks take to process.
var TaskDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "task_duration_seconds",
	Help:    "Time taken to process a task",
	Buckets: prometheus.DefBuckets,
}, []string{"type", "status"})

// TaskEnqueueTotal counts tasks enqueued by type.
var TaskEnqueueTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "task_enqueue_total",
	Help: "Total number of tasks enqueued",
}, []string{"type"})

// TaskRetriesTotal counts task retries by type.
var TaskRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "task_retries_total",
	Help: "Total number of task retries",
}, []string{"type"})

// TaskReclaimedTotal counts orphaned tasks reclaimed by the reaper.
var TaskReclaimedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "task_reclaimed_total",
	Help: "Total number of orphaned tasks reclaimed by the reaper",
})

// Handler returns an http.Handler that serves the Prometheus metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}
