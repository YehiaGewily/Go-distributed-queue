package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func runWorkerIdempotentLogic(t *testing.T, ctx context.Context, client *redis.Client, task Task, ttlSeconds int, handler func() error) (bool, error) {
	if task.IdempotencyKey != "" {
		claimTTL := 5 * time.Minute
		claimed, err := client.SetNX(ctx, "task:done:"+task.IdempotencyKey, "in_flight", claimTTL).Result()
		if err != nil {
			// log and proceed without dedup
		} else if !claimed {
			// Already in flight or completed
			return false, nil
		}
	}

	err := handler()
	if err != nil {
		if task.IdempotencyKey != "" {
			_ = client.Del(ctx, "task:done:"+task.IdempotencyKey).Err()
		}
		return true, err
	}

	if task.IdempotencyKey != "" {
		if err := client.Set(ctx, "task:done:"+task.IdempotencyKey, "done", time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
			t.Fatalf("set done marker: %v", err)
		}
	}
	return true, nil
}

func TestIdempotency_DuplicateKeySkipped(t *testing.T) {
	mr, client := setupTestDB(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	task1 := Task{
		ID:             "task-1",
		Type:           "default",
		IdempotencyKey: "unique-key-1",
	}
	task2 := Task{
		ID:             "task-2",
		Type:           "default",
		IdempotencyKey: "unique-key-1",
	}

	handlerCount := 0
	handler := func() error {
		handlerCount++
		return nil
	}

	// First execution should succeed and run handler
	executed1, err := runWorkerIdempotentLogic(t, ctx, client, task1, 86400, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed1 {
		t.Fatal("expected first task to be executed")
	}

	// Second execution with same key should be skipped
	executed2, err := runWorkerIdempotentLogic(t, ctx, client, task2, 86400, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if executed2 {
		t.Fatal("expected second task to be skipped")
	}

	if handlerCount != 1 {
		t.Fatalf("expected handler to be called exactly once, got %d", handlerCount)
	}
	_ = mr
}

func TestIdempotency_NoKeyAlwaysRuns(t *testing.T) {
	mr, client := setupTestDB(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	task1 := Task{
		ID:             "task-1",
		Type:           "default",
		IdempotencyKey: "",
	}
	task2 := Task{
		ID:             "task-2",
		Type:           "default",
		IdempotencyKey: "",
	}

	handlerCount := 0
	handler := func() error {
		handlerCount++
		return nil
	}

	executed1, err := runWorkerIdempotentLogic(t, ctx, client, task1, 86400, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed1 {
		t.Fatal("expected task1 to run")
	}

	executed2, err := runWorkerIdempotentLogic(t, ctx, client, task2, 86400, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed2 {
		t.Fatal("expected task2 to run")
	}

	if handlerCount != 2 {
		t.Fatalf("expected handler to be called twice, got %d", handlerCount)
	}
	_ = mr
}

func TestIdempotency_FailureAllowsRetry(t *testing.T) {
	mr, client := setupTestDB(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	task := Task{
		ID:             "task-1",
		Type:           "default",
		IdempotencyKey: "unique-key-retry",
	}

	handlerCount := 0
	handler := func() error {
		handlerCount++
		if handlerCount == 1 {
			return errors.New("simulated failure")
		}
		return nil
	}

	// First execution fails
	executed1, err := runWorkerIdempotentLogic(t, ctx, client, task, 86400, handler)
	if err == nil {
		t.Fatal("expected error from first execution")
	}
	if !executed1 {
		t.Fatal("expected first execution attempt to run")
	}

	// Second execution should succeed because failure deleted the claim
	executed2, err := runWorkerIdempotentLogic(t, ctx, client, task, 86400, handler)
	if err != nil {
		t.Fatalf("unexpected error on second run: %v", err)
	}
	if !executed2 {
		t.Fatal("expected second execution to run")
	}

	if handlerCount != 2 {
		t.Fatalf("expected handler to be called twice, got %d", handlerCount)
	}
	_ = mr
}

func TestIdempotency_DoneKeyHasLongTTL(t *testing.T) {
	mr, client := setupTestDB(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	task := Task{
		ID:             "task-1",
		Type:           "default",
		IdempotencyKey: "unique-key-ttl",
	}

	handler := func() error {
		return nil
	}

	ttlSeconds := 86400
	executed, err := runWorkerIdempotentLogic(t, ctx, client, task, ttlSeconds, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed {
		t.Fatal("expected task to be executed")
	}

	ttl := client.TTL(ctx, "task:done:unique-key-ttl").Val()
	// TTL should be close to 86400 seconds (allowing for a small delay/precision margin)
	if ttl < 86300*time.Second || ttl > 86405*time.Second {
		t.Fatalf("expected TTL to be close to %d seconds, got %v", ttlSeconds, ttl)
	}
	_ = mr
}
