package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"go-queue/internal/queue"
)

func main() {
	ctx := context.Background()
	client := queue.NewClient("localhost:6379")

	// Seed random number generator
	// rand.Seed(time.Now().UnixNano()) // Deprecated in Go 1.20, but safe to ignore or use NewSource if needed.
	// For simplicity, we'll assume the default global source is fine or seeded elsewhere if strict randomness needed.

	fmt.Println("👷 Worker started. Waiting for tasks...")

	for {
		// 1. ATOMIC MOVE: Pop from 'pending', push to 'processing'
		result, err := client.BRPopLPush(ctx, queue.QueuePending, queue.QueueProcessing, 0).Result()

		if err != nil {
			log.Println("Error connecting to Redis:", err)
			time.Sleep(3 * time.Second) // Retry delay
			continue
		}

		fmt.Printf("🚀 Processing task: %s\n", result)

		// Parse the task
		task, err := queue.BytesToTask([]byte(result))
		if err != nil {
			log.Printf("❌ Failed to parse task: %v\n", err)
			client.LRem(ctx, queue.QueueProcessing, 1, result) // Discard bad data
			continue
		}

		// 2. Process the task with simulated failure
		err = processTask(task)

		// 3. Handle Result
		if err != nil {
			log.Printf("⚠️ Task failed: %v\n", err)

			// Remove from processing queue regardless of next step (we re-add it if needed)
			client.LRem(ctx, queue.QueueProcessing, 1, result)

			if task.RetryCount < 3 {
				// Case A: Retry
				task.RetryCount++
				fmt.Printf("🔄 Retrying task [%s]... Attempt %d\n", task.ID, task.RetryCount)

				// Serialize and push back to Pending
				data, _ := json.Marshal(task)
				client.RPush(ctx, queue.QueuePending, data)
			} else {
				// Case B: Dead Letter Queue
				fmt.Printf("💀 Task [%s] moved to DLQ\n", task.ID)

				// Serialize and push to DLQ
				data, _ := json.Marshal(task)
				client.LPush(ctx, queue.QueueDeadLetter, data)
			}
		} else {
			// Success
			fmt.Println("✨ Task done.")
			client.LRem(ctx, queue.QueueProcessing, 1, result)
		}
	}
}

// processTask simulates work and random failures
func processTask(t queue.Task) error {
	// Simulate work
	time.Sleep(1 * time.Second)

	// Simulate 25% failure chance
	if rand.Intn(4) == 0 {
		return fmt.Errorf("random simulated failure for task %s", t.ID)
	}

	return nil
}
