//go:build integration

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

// TestIntegration_ReaperReclaimsOrphanedTasks pushes 50 tasks into a real Redis,
// starts a worker that sleeps 500 ms per task, kills it after 100 ms, then
// starts a fresh worker and asserts all 50 tasks eventually complete.
func TestIntegration_ReaperReclaimsOrphanedTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := setupRedis(t)
	defer client.Close()

	ctx := context.Background()

	// Derive the address string (including DB 15) for worker env vars.
	opts := client.Options()
	workerAddr := opts.Addr

	// --- 1. Build the worker binary ---
	tmpDir := t.TempDir()
	binName := "worker"
	if runtime.GOOS == "windows" {
		binName = "worker.exe"
	}
	workerBin := filepath.Join(tmpDir, binName)

	projectRoot := filepath.Join("..", "..")
	buildCmd := exec.Command("go", "build", "-o", workerBin, "./cmd/worker")
	buildCmd.Dir = projectRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build worker: %v\n%s", err, out)
	}

	// --- 2. Enqueue 50 tasks ---
	const numTasks = 50
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

	// --- 3. Start worker-1 (500 ms sleep, 0 % failure, short lease) ---
	workerEnv := []string{
		"REDIS_ADDR=" + workerAddr,
		"WORKER_SLEEP_MS=500",
		"WORKER_FAILURE_PCT=0",
		"LEASE_TTL_SECONDS=2",
		"REAPER_INTERVAL_SECONDS=1",
		"REAPER_ENABLED=true",
	}

	worker1 := exec.Command(workerBin)
	worker1.Env = append(os.Environ(), workerEnv...)
	worker1.Stdout = os.Stderr
	worker1.Stderr = os.Stderr
	if err := worker1.Start(); err != nil {
		t.Fatalf("failed to start worker1: %v", err)
	}

	// --- 4. Kill worker-1 after 100 ms ---
	time.Sleep(100 * time.Millisecond)
	if err := worker1.Process.Kill(); err != nil {
		t.Fatalf("failed to kill worker1: %v", err)
	}
	worker1.Wait() //nolint:errcheck
	t.Log("worker1 killed after 100 ms")

	// --- 5. Start worker-2 (same config) ---
	worker2 := exec.Command(workerBin)
	worker2.Env = append(os.Environ(), workerEnv...)
	worker2.Stdout = os.Stderr
	worker2.Stderr = os.Stderr
	if err := worker2.Start(); err != nil {
		t.Fatalf("failed to start worker2: %v", err)
	}
	defer func() {
		worker2.Process.Kill() //nolint:errcheck
		worker2.Wait()         //nolint:errcheck
	}()

	// --- 6. Poll until all three queues are empty ---
	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			pending := client.LLen(ctx, queue.QueuePending).Val()
			processing := client.LLen(ctx, queue.QueueProcessing).Val()
			dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()
			t.Fatalf("timeout: pending=%d processing=%d dlq=%d", pending, processing, dlq)
		case <-ticker.C:
			pending := client.LLen(ctx, queue.QueuePending).Val()
			processing := client.LLen(ctx, queue.QueueProcessing).Val()
			dlq := client.LLen(ctx, queue.QueueDeadLetter).Val()
			if pending == 0 && processing == 0 && dlq == 0 {
				t.Log("all 50 tasks completed successfully")
				return
			}
		}
	}
}
