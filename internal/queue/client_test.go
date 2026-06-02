package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupTestDB(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, client
}

func TestBLMove_AtomicPendingToProcessing(t *testing.T) {
	mr, client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()

	// Seed a task into the pending queue
	taskJSON := `{"id":"01HTEST000000000000000001","type":"email","payload":"hello","retry_count":0}`
	client.LPush(ctx, QueuePending, taskJSON)

	// BLMove should atomically pop from pending (RIGHT) and push to processing (LEFT)
	result, err := client.BLMove(ctx, QueuePending, QueueProcessing, "RIGHT", "LEFT", 5*time.Second).Result()
	if err != nil {
		t.Fatalf("BLMove failed: %v", err)
	}
	if result != taskJSON {
		t.Fatalf("expected %q, got %q", taskJSON, result)
	}

	// Pending queue should now be empty
	pendingLen := client.LLen(ctx, QueuePending).Val()
	if pendingLen != 0 {
		t.Fatalf("expected pending queue length 0, got %d", pendingLen)
	}

	// Processing queue should have exactly one item
	procLen := client.LLen(ctx, QueueProcessing).Val()
	if procLen != 1 {
		t.Fatalf("expected processing queue length 1, got %d", procLen)
	}

	// The item in processing should match the original task
	got := client.LIndex(ctx, QueueProcessing, 0).Val()
	if got != taskJSON {
		t.Fatalf("expected processing item %q, got %q", taskJSON, got)
	}

	_ = mr // keep miniredis alive for the test
}

func TestBLMove_BlockingTimeout(t *testing.T) {
	mr, client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()

	// With an empty pending queue, BLMove should block and then return nil
	// Use a short timeout so the test doesn't hang
	done := make(chan struct{})
	go func() {
		_, err := client.BLMove(ctx, QueuePending, QueueProcessing, "RIGHT", "LEFT", 1*time.Second).Result()
		if err != nil {
			// nil timeout error from miniredis is expected
			t.Logf("BLMove returned error on timeout (expected): %v", err)
		}
		close(done)
	}()

	// Wait a bit longer than the timeout to ensure it completes
	select {
	case <-done:
		// Good — BLMove returned after timeout
	case <-time.After(3 * time.Second):
		t.Fatal("BLMove did not return within expected time")
	}

	_ = mr
}
