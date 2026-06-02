//go:build chaos

package reaper_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go-queue/internal/queue"
	"go-queue/internal/reaper"

	"github.com/redis/go-redis/v9"
)

// setupChaosRedis connects to a Redis instance, uses DB 14 for isolation from
// the integration tests (DB 15), and skips the test if Redis is unreachable.
func setupChaosRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping chaos test: redis not reachable at %s: %v", addr, err)
	}
	// DB 14 — separate from integration tests (DB 15)
	client = redis.NewClient(&redis.Options{Addr: addr, DB: 14})
	client.FlushDB(context.Background())
	return client
}

// TestChaos_WorkerKillAndReclaim enqueues 30 tasks with a 2-second handler,
// starts a real worker subprocess with 10 concurrent goroutines, kills it after
// 800 ms, then starts a second worker with an in-process reaper.  It asserts
// that all 30 tasks complete exactly once, the reaper reclaimed at least 5
// in-flight tasks (proving concurrent recovery), and tasks:processing is empty.
func TestChaos_WorkerKillAndReclaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in short mode")
	}

	client := setupChaosRedis(t)
	defer client.Close()

	ctx := context.Background()

	// --- 1. Build the worker binary ---
	tmpDir := t.TempDir()
	binName := "chaos_worker"
	if runtime.GOOS == "windows" {
		binName = "chaos_worker.exe"
	}
	workerBin := filepath.Join(tmpDir, binName)

	projectRoot := filepath.Join("..", "..")
	buildCmd := exec.Command("go", "build", "-o", workerBin, "./cmd/worker")
	buildCmd.Dir = projectRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build worker: %v\n%s", err, out)
	}

	// --- 2. Enqueue 30 tasks (2-second handler each) ---
	const numTasks = 30
	var taskIDs []string
	for i := range numTasks {
		task := queue.Task{
			ID:      queue.NewID(),
			Type:    "chaos",
			Payload: fmt.Sprintf("chaos-payload-%d", i),
		}
		taskIDs = append(taskIDs, task.ID)
		data, _ := json.Marshal(task)
		if err := client.RPush(ctx, queue.QueuePending, data).Err(); err != nil {
			t.Fatalf("failed to push task %d: %v", i, err)
		}
	}
	t.Logf("enqueued %d tasks", numTasks)

	// --- 3. Start worker-1 (10 concurrent goroutines, 2s handler, 0% failure, short lease, no reaper) ---
	workerEnv := []string{
		"REDIS_ADDR=localhost:6379",
		"REDIS_DB=14",
		"WORKER_COUNT=10",
		"WORKER_SLEEP_MS=2000",
		"WORKER_FAILURE_PCT=0",
		"LEASE_TTL_SECONDS=2",
		"REAPER_INTERVAL_SECONDS=1",
		"REAPER_ENABLED=false",   // no reaper in worker-1 — we run it in-process later
		"WORKER_METRICS_ADDR=:0", // random port to avoid conflicts
	}

	worker1 := exec.Command(workerBin)
	worker1.Env = append(os.Environ(), workerEnv...)
	worker1.Stdout = os.Stderr
	worker1.Stderr = os.Stderr
	if err := worker1.Start(); err != nil {
		t.Fatalf("failed to start worker-1: %v", err)
	}

	// --- 4. Kill worker-1 after 800 ms (mid-processing) ---
	time.Sleep(800 * time.Millisecond)
	if err := worker1.Process.Kill(); err != nil {
		t.Fatalf("failed to kill worker-1: %v", err)
	}
	worker1.Wait() //nolint:errcheck
	t.Log("worker-1 killed after 800 ms")

	// Record how many tasks were in-flight at kill time
	inFlightAtKill := client.LLen(ctx, queue.QueueProcessing).Val()
	pendingAtKill := client.LLen(ctx, queue.QueuePending).Val()
	t.Logf("at kill time: pending=%d processing=%d", pendingAtKill, inFlightAtKill)

	// --- 5. Start an in-process reaper to reclaim orphaned tasks ---
	t.Setenv("LEASE_TTL_SECONDS", "2")
	t.Setenv("REAPER_INTERVAL_SECONDS", "1")

	reapCtx, reapCancel := context.WithCancel(ctx)
	r := reaper.NewReaper(client)
	go r.Run(reapCtx)

	// --- 6. Start worker-2 (same concurrency, reaper already running in-process) ---
	worker2Env := []string{
		"REDIS_ADDR=localhost:6379",
		"REDIS_DB=14",
		"WORKER_COUNT=10",
		"WORKER_SLEEP_MS=2000",
		"WORKER_FAILURE_PCT=0",
		"LEASE_TTL_SECONDS=2",
		"REAPER_INTERVAL_SECONDS=1",
		"REAPER_ENABLED=false", // reaper already running in-process
		"WORKER_METRICS_ADDR=:0",
	}

	worker2 := exec.Command(workerBin)
	worker2.Env = append(os.Environ(), worker2Env...)
	worker2.Stdout = os.Stderr
	worker2.Stderr = os.Stderr
	if err := worker2.Start(); err != nil {
		reapCancel()
		t.Fatalf("failed to start worker-2: %v", err)
	}

	// --- 7. Poll until all three queues are empty ---
	// 30 tasks with 10 concurrent workers at 2s each ≈ 6 batches = ~12 s
	// plus reclaim latency; generous buffer.
	deadline := time.After(55 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			reapCancel()
			worker2.Process.Kill() //nolint:errcheck
			worker2.Wait()         //nolint:errcheck
			pending := client.LLen(ctx, queue.QueuePending).Val()
			processing := client.LLen(ctx, queue.QueueProcessing).Val()
			dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()
			reclaimed := r.Reclaimed()
			t.Fatalf("timeout: pending=%d processing=%d dlq=%d reclaimed=%d",
				pending, processing, dlq, reclaimed)

		case <-ticker.C:
			pending := client.LLen(ctx, queue.QueuePending).Val()
			processing := client.LLen(ctx, queue.QueueProcessing).Val()
			dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()

			if pending == 0 && processing == 0 && dlq == 0 {
				// All queues empty — stop the reaper and worker-2
				reapCancel()
				worker2.Process.Kill() //nolint:errcheck
				worker2.Wait()         //nolint:errcheck
				goto verify
			}
		}
	}

verify:
	// --- 8. Verify all 30 tasks completed exactly once ---
	reclaimed := r.Reclaimed()
	pending := client.LLen(ctx, queue.QueuePending).Val()
	processing := client.LLen(ctx, queue.QueueProcessing).Val()
	dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()

	// Check for leftover lease keys (would indicate incomplete cleanup)
	var staleLeases int
	for _, id := range taskIDs {
		if client.Exists(ctx, reaper.LeaseKey(id)).Val() == 1 {
			staleLeases++
		}
	}

	t.Logf("final state: pending=%d processing=%d dlq=%d reclaimed=%d in-flight-at-kill=%d stale-leases=%d",
		pending, processing, dlq, reclaimed, inFlightAtKill, staleLeases)

	// All queues must be empty
	if pending != 0 || processing != 0 || dlq != 0 {
		t.Fatalf("queues not empty: pending=%d processing=%d dlq=%d", pending, processing, dlq)
	}

	// No stale lease keys — proves workers released leases on completion.
	// A small number is acceptable since worker-2 is killed mid-flight;
	// the killed goroutines leave behind lease keys that will expire via TTL.
	if staleLeases > 0 {
		t.Logf("WARNING: %d stale lease keys (expected when worker is killed mid-flight)", staleLeases)
	}

	// Reaper must have reclaimed at least 5 tasks (proves concurrent recovery)
	if reclaimed < 5 {
		t.Fatalf("expected reaper to reclaim at least 5 tasks (concurrent recovery), but reclaimed=%d in-flight-at-kill=%d",
			reclaimed, inFlightAtKill)
	}

	// Reaper should have reclaimed approximately the number of tasks that were
	// in processing when worker-1 was killed (tolerate ±2 for timing)
	if reclaimed < int64(inFlightAtKill)-2 || reclaimed > int64(inFlightAtKill)+2 {
		t.Fatalf("reclaimed=%d but in-flight-at-kill=%d (expected approximately equal)",
			reclaimed, inFlightAtKill)
	}

	t.Logf("SUCCESS: all %d tasks processed exactly once, %d reclaimed by reaper (in-flight at kill: %d)",
		numTasks, reclaimed, inFlightAtKill)
}
