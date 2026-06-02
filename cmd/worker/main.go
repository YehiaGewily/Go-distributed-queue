package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"go-queue/internal/dispatcher"
	"go-queue/internal/metrics"
	"go-queue/internal/queue"
	"go-queue/internal/reaper"

	"github.com/redis/go-redis/v9"
)

func main() {
	ctx := context.Background()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	db := envInt("REDIS_DB", 0)
	client := queue.NewClientWithDB(addr, db)
	defer func() { _ = client.Close() }()

	workerID := queue.NewID()
	workerCount := envInt("WORKER_COUNT", 1)
	slog.Info("worker started", "worker_id", workerID, "redis", addr, "concurrency", workerCount)

	// Start metrics HTTP server
	metricsAddr := os.Getenv("WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":8086"
	}
	go func() {
		http.Handle("/metrics", metrics.Handler())
		slog.Info("worker metrics server starting", "addr", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			slog.Error("metrics server error", "err", err)
		}
	}()

	// Start reaper goroutine (gated by REAPER_ENABLED, default true)
	if reaper.IsReaperEnabled() {
		r := reaper.NewReaper(client)
		go r.Run(ctx)
	}

	// Start dispatcher goroutine (gated by DISPATCHER_ENABLED, default true)
	if dispatcher.IsDispatcherEnabled() {
		d := dispatcher.NewDispatcher(client)
		go d.Run(ctx)
	}

	// Configurable processing parameters
	sleepMs := envInt("WORKER_SLEEP_MS", 1000)
	failurePct := envInt("WORKER_FAILURE_PCT", 25)

	// Launch concurrent worker goroutines
	var wg sync.WaitGroup
	for i := range workerCount {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			runWorkerLoop(ctx, client, workerID, sleepMs, failurePct)
		}(i)
	}
	wg.Wait()
}

// pollTask attempts to retrieve a task from the queues in priority order:
// 1. Non-blocking LMove on QueuePendingHigh
// 2. Non-blocking LMove on QueuePendingDefault
// 3. Blocking BLMove on QueuePendingLow with a 1-second timeout.
func pollTask(ctx context.Context, client *redis.Client) (string, error) {
	// 1. High priority
	res, err := client.LMove(ctx, queue.QueuePendingHigh, queue.QueueProcessing, "RIGHT", "LEFT").Result()
	if err == nil {
		return res, nil
	}
	if err != redis.Nil {
		return "", err
	}

	// 2. Default priority
	res, err = client.LMove(ctx, queue.QueuePendingDefault, queue.QueueProcessing, "RIGHT", "LEFT").Result()
	if err == nil {
		return res, nil
	}
	if err != redis.Nil {
		return "", err
	}

	// 3. Low priority (blocking 1s)
	res, err = client.BLMove(ctx, queue.QueuePendingLow, queue.QueueProcessing, "RIGHT", "LEFT", 1*time.Second).Result()
	if err == nil {
		return res, nil
	}
	return "", err
}

// calculateBackoff calculates true exponential backoff with jitter capped at 60s.
func calculateBackoff(attempt int) time.Duration {
	baseDelay := 2.0 // base delay in seconds
	delaySecs := baseDelay * math.Pow(2, float64(attempt))

	// Jitter: ±20%
	jitterRange := delaySecs * 0.2
	jitter := (rand.Float64()*2 - 1) * jitterRange
	finalDelaySecs := delaySecs + jitter

	// Cap at 60 seconds
	if finalDelaySecs > 60.0 {
		finalDelaySecs = 60.0
	}
	if finalDelaySecs < 0.1 {
		finalDelaySecs = 0.1
	}

	return time.Duration(finalDelaySecs * float64(time.Second))
}

// runWorkerLoop is the main task-processing loop for a single worker goroutine.
func runWorkerLoop(ctx context.Context, client *redis.Client, workerID string, sleepMs, failurePct int) {
	for {
		// 1. ATOMIC MOVE: Pop from priority queues, push to 'processing'
		result, err := pollTask(ctx, client)

		if err != nil {
			if err != redis.Nil {
				slog.Error("error connecting to Redis", "err", err)
				time.Sleep(3 * time.Second) // Retry delay
			}
			continue
		}

		// Parse the task
		task, err := queue.BytesToTask([]byte(result))
		if err != nil {
			slog.Error("failed to parse task", "err", err)
			client.LRem(ctx, queue.QueueProcessing, 1, result) // Discard bad data
			continue
		}

		// 2. Acquire lease and start periodic refresh
		if err := reaper.AcquireLease(ctx, client, task.ID, workerID); err != nil {
			slog.Error("failed to acquire lease", "task_id", task.ID, "err", err)
		}
		cancelRefresh := reaper.StartLeaseRefresh(ctx, client, task.ID, workerID)

		// 3. Process the task with simulated failure
		start := time.Now()
		err = processTask(task, sleepMs, failurePct)
		duration := time.Since(start).Seconds()

		// 4. Stop lease refresh and release lease
		cancelRefresh()
		if delErr := reaper.ReleaseLease(ctx, client, task.ID); delErr != nil {
			slog.Error("failed to release lease", "task_id", task.ID, "err", delErr)
		}

		// 5. Handle Result
		if err != nil {
			slog.Warn("task failed", "task_id", task.ID, "err", err)
			metrics.TaskDuration.WithLabelValues(task.Type, "failure").Observe(duration)

			// Remove from processing queue regardless of next step (we re-add it if needed)
			client.LRem(ctx, queue.QueueProcessing, 1, result)

			if task.RetryCount < 3 {
				// Case A: Retry with exponential backoff and jitter
				task.RetryCount++
				backoff := calculateBackoff(task.RetryCount)
				executeAt := time.Now().Add(backoff)
				task.ExecuteAt = &executeAt

				slog.Info("retrying task with backoff", "task_id", task.ID, "attempt", task.RetryCount, "backoff", backoff)
				metrics.TaskRetriesTotal.WithLabelValues(task.Type).Inc()

				// Serialize and push back to delayed queue
				data, _ := json.Marshal(task)
				err = client.ZAdd(ctx, queue.QueueDelayed, redis.Z{
					Score:  float64(executeAt.Unix()),
					Member: data,
				}).Err()
				if err != nil {
					slog.Error("failed to schedule retry", "task_id", task.ID, "err", err)
				}
			} else {
				// Case B: Dead Letter Queue
				slog.Warn("task moved to DLQ", "task_id", task.ID)

				// Serialize and push to DLQ
				data, _ := json.Marshal(task)
				client.LPush(ctx, queue.QueueDeadLetter, data)
			}
		} else {
			// Success
			slog.Info("task done", "task_id", task.ID)
			metrics.TaskDuration.WithLabelValues(task.Type, "success").Observe(duration)
			client.LRem(ctx, queue.QueueProcessing, 1, result)
		}
	}
}

// processTask simulates work and random failures
func processTask(t queue.Task, sleepMs, failurePct int) error {
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	if failurePct > 0 && rand.Intn(100) < failurePct {
		return fmt.Errorf("random simulated failure for task %s", t.ID)
	}

	return nil
}

// envInt reads an integer from an environment variable, falling back to def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
