package dispatcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go-queue/internal/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupTestDB(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, client
}

func TestDispatcher_SweepDispatchesDueTasks(t *testing.T) {
	_, client := setupTestDB(t)
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	d := NewDispatcher(client)

	// 1. Create a task scheduled in the past
	pastTask := queue.Task{
		ID:        "past-id",
		Type:      "test",
		Priority:  "default",
		CreatedAt: time.Now(),
	}
	pastJSON, _ := json.Marshal(pastTask)
	pastScore := time.Now().Add(-5 * time.Second).Unix()

	// 2. Create a task scheduled in the future
	futureTask := queue.Task{
		ID:        "future-id",
		Type:      "test",
		Priority:  "default",
		CreatedAt: time.Now(),
	}
	futureJSON, _ := json.Marshal(futureTask)
	futureScore := time.Now().Add(5 * time.Second).Unix()

	// ZAdd both to QueueDelayed
	client.ZAdd(ctx, queue.QueueDelayed, redis.Z{Score: float64(pastScore), Member: pastJSON})
	client.ZAdd(ctx, queue.QueueDelayed, redis.Z{Score: float64(futureScore), Member: futureJSON})

	// Run dispatcher sweep
	d.sweep(ctx)

	// The past task should be moved to tasks:pending:default
	pendingDefaultLen := client.LLen(ctx, queue.QueuePendingDefault).Val()
	if pendingDefaultLen != 1 {
		t.Errorf("expected 1 task in pending default queue, got %d", pendingDefaultLen)
	}

	got := client.LIndex(ctx, queue.QueuePendingDefault, 0).Val()
	var gotTask queue.Task
	if err := json.Unmarshal([]byte(got), &gotTask); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gotTask.ID != "past-id" {
		t.Errorf("expected task past-id to be dispatched, got %s", gotTask.ID)
	}

	// The future task should still be in QueueDelayed
	delayedCount := client.ZCard(ctx, queue.QueueDelayed).Val()
	if delayedCount != 1 {
		t.Errorf("expected 1 task to remain in delayed queue, got %d", delayedCount)
	}
}

func TestDispatcher_SweepRespectsPriorities(t *testing.T) {
	_, client := setupTestDB(t)
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	d := NewDispatcher(client)

	// Create three past tasks with different priorities
	highTask := queue.Task{ID: "high-id", Priority: "high"}
	defaultTask := queue.Task{ID: "def-id", Priority: "default"}
	lowTask := queue.Task{ID: "low-id", Priority: "low"}

	highJSON, _ := json.Marshal(highTask)
	defaultJSON, _ := json.Marshal(defaultTask)
	lowJSON, _ := json.Marshal(lowTask)

	pastScore := time.Now().Add(-1 * time.Second).Unix()

	client.ZAdd(ctx, queue.QueueDelayed, redis.Z{Score: float64(pastScore), Member: highJSON})
	client.ZAdd(ctx, queue.QueueDelayed, redis.Z{Score: float64(pastScore), Member: defaultJSON})
	client.ZAdd(ctx, queue.QueueDelayed, redis.Z{Score: float64(pastScore), Member: lowJSON})

	// Run dispatcher sweep
	d.sweep(ctx)

	// Verify each task ended up in its correct queue
	if client.LLen(ctx, queue.QueuePendingHigh).Val() != 1 {
		t.Error("expected 1 task in high priority queue")
	}
	if client.LLen(ctx, queue.QueuePendingDefault).Val() != 1 {
		t.Error("expected 1 task in default priority queue")
	}
	if client.LLen(ctx, queue.QueuePendingLow).Val() != 1 {
		t.Error("expected 1 task in low priority queue")
	}
}
