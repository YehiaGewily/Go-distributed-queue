package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"go-queue/internal/queue"
)

func main() {
	ctx := context.Background()
	client := queue.NewClient("localhost:6379")

	fmt.Println("👷 Worker started. Waiting for tasks...")

	for {
		// 1. ATOMIC MOVE: Pop from 'pending', push to 'processing'
		// 0 means "wait forever until a task arrives"
		result, err := client.BRPopLPush(ctx, queue.QueuePending, queue.QueueProcessing, 0).Result()

		if err != nil {
			log.Println("Error connecting to Redis:", err)
			time.Sleep(3 * time.Second) // Retry delay
			continue
		}

		// 2. Process the task
		fmt.Printf("🚀 Processing task: %s\n", result)

		// Simulate heavy work (e.g., resizing image)
		time.Sleep(1 * time.Second) // Requirement: 1 second sleep

		// 3. Cleanup: Remove from 'processing' queue
		// If the worker crashes BEFORE this line, the task stays in 'processing'
		// and isn't lost. This is what makes it "Reliable".
		client.LRem(ctx, queue.QueueProcessing, 1, result)

		fmt.Println("✨ Task done.")
	}
}
