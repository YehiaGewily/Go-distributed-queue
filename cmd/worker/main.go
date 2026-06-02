package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"go-queue/internal/metrics"
	"go-queue/internal/queue"
	"go-queue/internal/reaper"
)

func main() {
	ctx := context.Background()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := queue.NewClient(addr)
	defer client.Close()

	workerID := queue.NewID()
	slog.Info("worker started", "worker_id", workerID, "redis", addr)

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

	// Configurable processing parameters
	sleepMs := envInt("WORKER_SLEEP_MS", 1000)
	failurePct := envInt("WORKER_FAILURE_PCT", 25)

	for {
		// 1. ATOMIC MOVE: Pop from 'pending', push to 'processing'
		result, err := client.BLMove(ctx, queue.QueuePending, queue.QueueProcessing, "RIGHT", "LEFT", 0).Result()

		if err != nil {
			slog.Error("error connecting to Redis", "err", err)
			time.Sleep(3 * time.Second) // Retry delay
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
				// Case A: Retry
				task.RetryCount++
				slog.Info("retrying task", "task_id", task.ID, "attempt", task.RetryCount)
				metrics.TaskRetriesTotal.WithLabelValues(task.Type).Inc()

				// Serialize and push back to Pending
				data, _ := json.Marshal(task)
				client.RPush(ctx, queue.QueuePending, data)
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
