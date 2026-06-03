//go:build integration

package reaper_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"go-queue/internal/dispatcher"
	"go-queue/internal/queue"
	"go-queue/internal/reaper"

	"github.com/redis/go-redis/v9"
)

func setupRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping integration test: redis not reachable at %s: %v", addr, err)
	}
	// flush a dedicated test DB
	client = redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	client.FlushDB(context.Background())
	return client
}

// TestIntegration_ReaperReclaimsOrphanedTasks enqueues tasks, starts an
// in-process worker that acquires leases but then "crashes" (context cancelled
// without releasing leases or acking), waits for the reaper to reclaim the
// orphaned tasks, and finally verifies a second worker completes them all.
func TestIntegration_ReaperReclaimsOrphanedTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Short intervals so the test completes quickly
	t.Setenv("LEASE_TTL_SECONDS", "2")
	t.Setenv("REAPER_INTERVAL_SECONDS", "1")

	client := setupRedis(t)
	defer func() { _ = client.Close() }()

	ctx := context.Background()

	// --- 1. Enqueue 10 tasks ---
	const numTasks = 10
	for i := range numTasks {
		task := queue.Task{
			ID:      queue.NewID(),
			Type:    "integration",
			Payload: fmt.Sprintf("payload-%d", i),
		}
		data, _ := json.Marshal(task)
		if err := client.RPush(ctx, queue.QueuePending, data).Err(); err != nil {
			t.Fatalf("failed to push task %d: %v", i, err)
		}
	}

	// --- 2. Start "worker-1" that picks up tasks but crashes mid-processing ---
	//
	// The worker goroutine uses BLMove with a short timeout in a loop.  For
	// each task it moves to processing, it acquires a lease and starts a
	// refresh goroutine, then immediately tries to grab the next task.
	// When the context is cancelled all refresh goroutines stop, no leases
	// are released, and no tasks are acked — simulating a hard crash.
	worker1Ctx, worker1Cancel := context.WithCancel(ctx)
	worker1Done := make(chan struct{})
	go func() {
		defer close(worker1Done)
		for {
			result, err := client.BLMove(worker1Ctx, queue.QueuePending, queue.QueueProcessing, "RIGHT", "LEFT", 2*time.Second).Result()
			if err != nil {
				return // context cancelled or timeout
			}
			task, err := queue.BytesToTask([]byte(result))
			if err != nil {
				continue
			}
			if err := reaper.AcquireLease(worker1Ctx, client, task.ID, "worker-1"); err != nil {
				return
			}
			_ = reaper.StartLeaseRefresh(worker1Ctx, client, task.ID, "worker-1")
			// Do NOT ack or release — simulate a worker that is still
			// processing when it crashes.
		}
	}()

	// Give worker-1 time to pick up all tasks, then "crash" it.
	time.Sleep(3 * time.Second)
	worker1Cancel()
	<-worker1Done
	t.Log("worker-1 crashed (context cancelled)")

	// --- 3. Wait for leases to expire, then start the reaper ---
	time.Sleep(3 * time.Second)

	reapCtx, reapCancel := context.WithCancel(ctx)
	r := reaper.NewReaper(client)
	go r.Run(reapCtx)

	// Wait for at least one reaper sweep
	time.Sleep(2 * time.Second)
	reapCancel()

	reclaimed := r.Reclaimed()
	t.Logf("reaper reclaimed %d tasks", reclaimed)
	if reclaimed == 0 {
		t.Fatal("expected reaper to reclaim at least one task, but reclaimed=0")
	}

	// --- 4. Start "worker-2" that completes all remaining tasks ---
	completed := 0
	for completed < numTasks {
		result, err := client.BLMove(ctx, queue.QueuePending, queue.QueueProcessing, "RIGHT", "LEFT", 5*time.Second).Result()
		if err != nil {
			t.Fatalf("worker-2: BLMove error after %d completions: %v", completed, err)
		}
		task, err := queue.BytesToTask([]byte(result))
		if err != nil {
			t.Fatalf("worker-2: parse error: %v", err)
		}

		// Normal lifecycle: acquire → process → ack + release
		reaper.AcquireLease(ctx, client, task.ID, "worker-2")
		reaper.ReleaseLease(ctx, client, task.ID)
		client.LRem(ctx, queue.QueueProcessing, 1, result)
		completed++
		t.Logf("worker-2 completed task %s (%d/%d)", task.ID, completed, numTasks)
	}

	// --- 5. Verify all queues are empty ---
	pending := client.LLen(ctx, queue.QueuePending).Val()
	processing := client.LLen(ctx, queue.QueueProcessing).Val()
	dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()

	if pending != 0 || processing != 0 || dlq != 0 {
		t.Fatalf("queues not empty: pending=%d processing=%d dlq=%d", pending, processing, dlq)
	}

	t.Logf("SUCCESS: all %d tasks completed, %d reclaimed by reaper", numTasks, reclaimed)
}

// TestIntegration_DelayedTaskExecutionAccuracy schedules a task 2s in the future and asserts it executes within ±500ms of target.
func TestIntegration_DelayedTaskExecutionAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Setup fast dispatcher intervals via env
	t.Setenv("DISPATCHER_INTERVAL_MS", "100")

	client := setupRedis(t)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the dispatcher in background
	disp := dispatcher.NewDispatcher(client)
	go disp.Run(ctx)

	// Align to next second boundary to avoid sub-second rounding errors with second-precision Unix timestamps
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))

	// Create and schedule task 2 seconds in the future
	targetDelay := 2 * time.Second
	startTime := time.Now()
	executeAt := startTime.Add(targetDelay)

	task := queue.Task{
		ID:        queue.NewID(),
		Type:      "integration-delay",
		ExecuteAt: &executeAt,
	}

	data, _ := json.Marshal(task)
	err := client.ZAdd(ctx, queue.QueueDelayed, redis.Z{
		Score:  float64(executeAt.Unix()),
		Member: data,
	}).Err()
	if err != nil {
		t.Fatalf("failed to schedule task: %v", err)
	}

	// We will simulate a worker that polls pending queue.
	// Since the dispatcher moves it to pending:default, we poll pending:default.
	res, err := client.BLMove(ctx, queue.QueuePendingDefault, queue.QueueProcessing, "RIGHT", "LEFT", 5*time.Second).Result()
	if err != nil {
		t.Fatalf("BLMove failed to receive dispatched task: %v", err)
	}

	executionTime := time.Now()
	actualDelay := executionTime.Sub(startTime)

	// Clean up task from processing list
	client.LRem(ctx, queue.QueueProcessing, 1, res)

	var receivedTask queue.Task
	json.Unmarshal([]byte(res), &receivedTask)
	if receivedTask.ID != task.ID {
		t.Fatalf("expected task ID %s, got %s", task.ID, receivedTask.ID)
	}

	// Assert: executed within ±500ms of target (2 seconds)
	diff := actualDelay - targetDelay
	absDiff := time.Duration(math.Abs(float64(diff)))

	t.Logf("Start time: %s", startTime.Format(time.RFC3339Nano))
	t.Logf("Target execution time: %s", executeAt.Format(time.RFC3339Nano))
	t.Logf("Actual execution time: %s", executionTime.Format(time.RFC3339Nano))
	t.Logf("Target delay: %s, Actual delay: %s, Difference: %s", targetDelay, actualDelay, diff)

	if absDiff > 500*time.Millisecond {
		t.Errorf("expected task to execute within 500ms of target delay (2s), but difference was %s", diff)
	}
}
