package reaper

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"go-queue/internal/metrics"
	"go-queue/internal/queue"

	"github.com/redis/go-redis/v9"
)

// reclaimScript atomically moves a task from processing back to pending
// if and only if the lease key has expired. This prevents concurrent
// reapers from double-reclaiming the same task.
//
// KEYS[1] = lease key  (task:lease:<id>)
// KEYS[2] = processing list (tasks:processing)
// KEYS[3] = pending list    (tasks:pending)
// ARGV[1] = task JSON data
// Returns 1 if reclaimed, 0 otherwise.
var reclaimScript = redis.NewScript(`
local leaseKey      = KEYS[1]
local processingKey = KEYS[2]
local pendingKey    = KEYS[3]
local taskData      = ARGV[1]

if redis.call("EXISTS", leaseKey) == 1 then
    return 0
end

local removed = redis.call("LREM", processingKey, 1, taskData)
if removed > 0 then
    redis.call("LPUSH", pendingKey, taskData)
    return 1
end

return 0
`)

// Reaper scans the processing queue for orphaned tasks (those with
// expired leases) and moves them back to the pending queue.
type Reaper struct {
	client    *redis.Client
	interval  time.Duration
	reclaimed atomic.Int64
}

// NewReaper creates a Reaper. The sweep interval defaults to 5 s and can be
// overridden with REAPER_INTERVAL_SECONDS.
func NewReaper(client *redis.Client) *Reaper {
	interval := 5 * time.Second
	if v := os.Getenv("REAPER_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	return &Reaper{
		client:   client,
		interval: interval,
	}
}

// Interval returns the configured sweep interval (useful in tests).
func (r *Reaper) Interval() time.Duration { return r.interval }

// Run starts the reaper loop. It blocks until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	slog.Info("reaper started", "interval", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reaper stopped")
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep performs one pass over the processing queue, reclaiming any task
// whose lease has expired.
func (r *Reaper) sweep(ctx context.Context) {
	tasks, err := r.client.LRange(ctx, queue.QueueProcessing, 0, -1).Result()
	if err != nil {
		slog.Error("reaper: failed to list processing tasks", "err", err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	for _, taskData := range tasks {
		task, err := queue.BytesToTask([]byte(taskData))
		if err != nil {
			slog.Warn("reaper: failed to parse task", "err", err)
			continue
		}

		leaseKey := LeaseKey(task.ID)
		pendingQueue := queue.GetPendingQueue(task.Priority)

		reclaimed, err := reclaimScript.Run(
			ctx, r.client,
			[]string{leaseKey, queue.QueueProcessing, pendingQueue},
			taskData,
		).Int64()
		if err != nil {
			slog.Error("reaper: reclaim script failed", "task_id", task.ID, "err", err)
			continue
		}

		if reclaimed > 0 {
			r.reclaimed.Add(1)
			metrics.TaskReclaimedTotal.Inc()
			slog.Info("reaper: reclaimed orphaned task", "task_id", task.ID)
		}
	}
}

// Reclaimed returns the total number of tasks reclaimed since start.
func (r *Reaper) Reclaimed() int64 { return r.reclaimed.Load() }

// ---------------------------------------------------------------------------
// Lease helpers — used by the worker to acquire, refresh, and release leases.
// ---------------------------------------------------------------------------

// LeaseKey returns the Redis key used for a task's lease.
func LeaseKey(taskID string) string {
	return "task:lease:" + taskID
}

// LeaseTTL returns the configured lease TTL (default 30 s, env LEASE_TTL_SECONDS).
func LeaseTTL() time.Duration {
	d := 30 * time.Second
	if v := os.Getenv("LEASE_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d = time.Duration(n) * time.Second
		}
	}
	return d
}

// AcquireLease sets the lease key for a task with the configured TTL.
func AcquireLease(ctx context.Context, client *redis.Client, taskID, workerID string) error {
	return client.Set(ctx, LeaseKey(taskID), workerID, LeaseTTL()).Err()
}

// RefreshLease renews the lease key for a task.
func RefreshLease(ctx context.Context, client *redis.Client, taskID, workerID string) error {
	return client.Set(ctx, LeaseKey(taskID), workerID, LeaseTTL()).Err()
}

// ReleaseLease deletes the lease key for a task.
func ReleaseLease(ctx context.Context, client *redis.Client, taskID string) error {
	return client.Del(ctx, LeaseKey(taskID)).Err()
}

// StartLeaseRefresh spawns a goroutine that refreshes the lease every
// LeaseTTL()/3 while the worker is processing a task.  Returns a cancel
// function that must be called when processing completes.
func StartLeaseRefresh(ctx context.Context, client *redis.Client, taskID, workerID string) context.CancelFunc {
	refreshCtx, cancel := context.WithCancel(ctx)
	interval := LeaseTTL() / 3
	if interval < time.Second {
		interval = time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				if err := RefreshLease(refreshCtx, client, taskID, workerID); err != nil && refreshCtx.Err() == nil {
					slog.Error("lease refresh failed", "task_id", taskID, "err", err)
				}
			}
		}
	}()

	return cancel
}

// IsReaperEnabled returns whether the reaper should run (default true,
// configurable via REAPER_ENABLED).
func IsReaperEnabled() bool {
	v := os.Getenv("REAPER_ENABLED")
	if v == "" {
		return true
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return enabled
}
