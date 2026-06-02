package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestProducer_TaskRoute_PriorityValidation(t *testing.T) {
	_, client := setupTestDB(t)
	defer client.Close()

	mux := setupMux(client)
	server := httptest.NewServer(mux)
	defer server.Close()

	tests := []struct {
		name           string
		payload        map[string]interface{}
		expectedStatus int
		expectedQueue  string
	}{
		{
			name: "Valid high priority",
			payload: map[string]interface{}{
				"type":     "test",
				"payload":  "hello",
				"priority": "high",
			},
			expectedStatus: http.StatusAccepted,
			expectedQueue:  queue.QueuePendingHigh,
		},
		{
			name: "Valid default priority",
			payload: map[string]interface{}{
				"type":     "test",
				"payload":  "hello",
				"priority": "default",
			},
			expectedStatus: http.StatusAccepted,
			expectedQueue:  queue.QueuePendingDefault,
		},
		{
			name: "Empty priority defaults to default",
			payload: map[string]interface{}{
				"type":    "test",
				"payload": "hello",
			},
			expectedStatus: http.StatusAccepted,
			expectedQueue:  queue.QueuePendingDefault,
		},
		{
			name: "Invalid priority rejects",
			payload: map[string]interface{}{
				"type":     "test",
				"payload":  "hello",
				"priority": "urgent",
			},
			expectedStatus: http.StatusBadRequest,
			expectedQueue:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.payload)
			req, _ := http.NewRequest("POST", server.URL+"/task", bytes.NewBuffer(body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}

			if tt.expectedQueue != "" && resp.StatusCode == http.StatusAccepted {
				ctx := context.Background()
				len := client.LLen(ctx, tt.expectedQueue).Val()
				if len == 0 {
					t.Errorf("expected task to be enqueued in %s, but queue is empty", tt.expectedQueue)
				}
				// Clean up
				client.FlushDB(ctx)
			}
		})
	}
}

func TestProducer_TaskRoute_ExecuteAt(t *testing.T) {
	_, client := setupTestDB(t)
	defer client.Close()

	mux := setupMux(client)
	server := httptest.NewServer(mux)
	defer server.Close()

	futureTime := time.Now().Add(10 * time.Second)
	payload := map[string]interface{}{
		"type":       "test",
		"payload":    "delayed-data",
		"execute_at": futureTime.Format(time.RFC3339),
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", server.URL+"/task", bytes.NewBuffer(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d", resp.StatusCode)
	}

	// Verify it was enqueued in tasks:delayed
	ctx := context.Background()
	delayedCount := client.ZCard(ctx, queue.QueueDelayed).Val()
	if delayedCount != 1 {
		t.Errorf("expected 1 task in delayed queue, got %d", delayedCount)
	}

	// Also verify that it was NOT enqueued in pending
	pendingCount := client.LLen(ctx, queue.QueuePendingDefault).Val()
	if pendingCount != 0 {
		t.Errorf("expected 0 tasks in pending, got %d", pendingCount)
	}
}

func TestProducer_BatchTasks(t *testing.T) {
	_, client := setupTestDB(t)
	defer client.Close()

	mux := setupMux(client)
	server := httptest.NewServer(mux)
	defer server.Close()

	futureTime := time.Now().Add(10 * time.Second)
	payload := []map[string]interface{}{
		{
			"type":     "test1",
			"payload":  "p1",
			"priority": "high",
		},
		{
			"type":     "test2",
			"payload":  "p2",
			"priority": "low",
		},
		{
			"type":       "test3",
			"payload":    "p3",
			"execute_at": futureTime.Format(time.RFC3339),
		},
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", server.URL+"/tasks", bytes.NewBuffer(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d", resp.StatusCode)
	}

	ctx := context.Background()
	// High priority task enqueued
	if client.LLen(ctx, queue.QueuePendingHigh).Val() != 1 {
		t.Error("expected 1 task in tasks:pending:high")
	}
	// Low priority task enqueued
	if client.LLen(ctx, queue.QueuePendingLow).Val() != 1 {
		t.Error("expected 1 task in tasks:pending:low")
	}
	// Delayed task enqueued
	if client.ZCard(ctx, queue.QueueDelayed).Val() != 1 {
		t.Error("expected 1 task in tasks:delayed")
	}
}
