package reaper

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go-queue/internal/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupReaperTest(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	return mr, client
}

// ---------------------------------------------------------------------------
// Unit test 1: orphaned task (no lease) is reclaimed back to pending.
// ---------------------------------------------------------------------------

func TestReaper_OrphanedTaskReclaimed(t *testing.T) {
	mr, client := setupReaperTest(t)
	ctx := context.Background()

	// Insert a task into processing with NO lease
	task := queue.Task{ID: "01HTEST000000000000000001", Type: "email", Payload: "hello"}
	taskData := marshalTask(task)
	client.LPush(ctx, queue.QueueProcessing, taskData)

	// Run one sweep
	r := &Reaper{client: client, interval: time.Second}
	r.sweep(ctx)

	// Task should be back in pending
	pendingLen := client.LLen(ctx, queue.QueuePending).Val()
	if pendingLen != 1 {
		t.Fatalf("expected pending queue length 1, got %d", pendingLen)
	}

	// Processing should be empty
	procLen := client.LLen(ctx, queue.QueueProcessing).Val()
	if procLen != 0 {
		t.Fatalf("expected processing queue length 0, got %d", procLen)
	}

	// Counter should be 1
	if r.Reclaimed() != 1 {
		t.Fatalf("expected reclaimed count 1, got %d", r.Reclaimed())
	}

	_ = mr
}

// ---------------------------------------------------------------------------
// Unit test 2: task with a valid lease is NOT moved.
// ---------------------------------------------------------------------------

func TestReaper_TaskWithValidLeaseNotMoved(t *testing.T) {
	mr, client := setupReaperTest(t)
	ctx := context.Background()

	// Insert a task into processing WITH a valid lease
	task := queue.Task{ID: "01HTEST000000000000000002", Type: "email", Payload: "world"}
	taskData := marshalTask(task)
	client.LPush(ctx, queue.QueueProcessing, taskData)
	client.Set(ctx, LeaseKey(task.ID), "worker-abc", 30*time.Second)

	// Run one sweep
	r := &Reaper{client: client, interval: time.Second}
	r.sweep(ctx)

	// Task should still be in processing
	procLen := client.LLen(ctx, queue.QueueProcessing).Val()
	if procLen != 1 {
		t.Fatalf("expected processing queue length 1, got %d", procLen)
	}

	// Pending should be empty
	pendingLen := client.LLen(ctx, queue.QueuePending).Val()
	if pendingLen != 0 {
		t.Fatalf("expected pending queue length 0, got %d", pendingLen)
	}

	// Counter should be 0
	if r.Reclaimed() != 0 {
		t.Fatalf("expected reclaimed count 0, got %d", r.Reclaimed())
	}

	_ = mr
}

// ---------------------------------------------------------------------------
// Unit test 3: Lua script prevents double-reclaim by concurrent reapers.
// ---------------------------------------------------------------------------

func TestReaper_ConcurrentSweepNoDoubleReclaim(t *testing.T) {
	mr, client := setupReaperTest(t)
	ctx := context.Background()

	// Insert one orphaned task
	task := queue.Task{ID: "01HTEST000000000000000003", Type: "email", Payload: "race"}
	taskData := marshalTask(task)
	client.LPush(ctx, queue.QueueProcessing, taskData)

	// Run two sweeps concurrently
	r := &Reaper{client: client, interval: time.Second}

	done := make(chan struct{})
	go func() {
		r.sweep(ctx)
		close(done)
	}()
	r.sweep(ctx)
	<-done

	// Only one reaper should have reclaimed the task
	pendingLen := client.LLen(ctx, queue.QueuePending).Val()
	if pendingLen != 1 {
		t.Fatalf("expected pending queue length 1, got %d", pendingLen)
	}

	// The Lua script ensures at most one reaper moves the task
	if r.Reclaimed() != 1 {
		t.Fatalf("expected exactly 1 total reclaim, got %d", r.Reclaimed())
	}

	_ = mr
}

// ---------------------------------------------------------------------------
// Unit test 4: AcquireLease / ReleaseLease round-trip.
// ---------------------------------------------------------------------------

func TestLease_AcquireAndRelease(t *testing.T) {
	mr, client := setupReaperTest(t)
	ctx := context.Background()

	taskID := "01HTEST000000000000000004"

	// Acquire
	if err := AcquireLease(ctx, client, taskID, "w1"); err != nil {
		t.Fatalf("AcquireLease failed: %v", err)
	}

	// Lease key should exist
	exists := client.Exists(ctx, LeaseKey(taskID)).Val()
	if exists != 1 {
		t.Fatal("expected lease key to exist after acquire")
	}

	// Release
	if err := ReleaseLease(ctx, client, taskID); err != nil {
		t.Fatalf("ReleaseLease failed: %v", err)
	}

	// Lease key should be gone
	exists = client.Exists(ctx, LeaseKey(taskID)).Val()
	if exists != 0 {
		t.Fatal("expected lease key to be deleted after release")
	}

	_ = mr
}

// ---------------------------------------------------------------------------
// Unit test 5: StartLeaseRefresh extends TTL, and cancel stops refresh.
// ---------------------------------------------------------------------------

func TestLease_RefreshKeepsLeaseAlive(t *testing.T) {
	mr, client := setupReaperTest(t)
	ctx := context.Background()

	// Set a 2-second lease TTL for this test
	t.Setenv("LEASE_TTL_SECONDS", "2")

	taskID := "01HTEST000000000000000005"

	if err := AcquireLease(ctx, client, taskID, "w1"); err != nil {
		t.Fatalf("AcquireLease failed: %v", err)
	}

	// Record TTL immediately after acquire
	ttl0 := client.TTL(ctx, LeaseKey(taskID)).Val()
	t.Logf("TTL right after acquire: %v", ttl0)

	// Start the refresh goroutine (interval = max(2/3,1) = 1s)
	cancel := StartLeaseRefresh(ctx, client, taskID, "w1")

	// Wait 2.5s — longer than the 2s TTL — the refresh should have renewed it
	time.Sleep(2500 * time.Millisecond)

	// The key must still exist (without refresh it would have expired at 2s)
	exists := client.Exists(ctx, LeaseKey(taskID)).Val()
	if exists != 1 {
		t.Fatal("expected lease key to still exist — refresh should have renewed it")
	}

	// The remaining TTL should be close to the full 2s (it was just refreshed)
	ttl1 := client.TTL(ctx, LeaseKey(taskID)).Val()
	t.Logf("TTL after 2.5s with refresh: %v", ttl1)
	if ttl1 < 1*time.Second {
		t.Fatalf("expected TTL to have been refreshed (close to 2s), got %v", ttl1)
	}

	// Stop the refresh goroutine
	cancel()

	// Give the goroutine time to exit, then advance miniredis clock well past TTL
	time.Sleep(100 * time.Millisecond)
	mr.FastForward(5 * time.Second)

	exists = client.Exists(ctx, LeaseKey(taskID)).Val()
	if exists != 0 {
		t.Fatal("expected lease key to expire after refresh stopped and clock advanced")
	}

	_ = mr
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func marshalTask(t queue.Task) string {
	data, err := json.Marshal(t)
	if err != nil {
		panic("marshalTask: " + err.Error())
	}
	return string(data)
}
