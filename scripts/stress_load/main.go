package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"go-queue/internal/queue"
)

func main() {
	url := "http://localhost:8085/task"
	count := 50
	types := []string{"Email", "Resize", "Export"}

	fmt.Printf("Starting stress test: Sending %d requests to %s...\n", count, url)

	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			taskType := types[rand.Intn(len(types))]
			payload := map[string]string{
				"id":      queue.NewID(),
				"type":    taskType,
				"payload": fmt.Sprintf("stress-test-data-%d", id),
			}

			jsonData, _ := json.Marshal(payload)
			resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
			if err != nil {
				fmt.Printf("❌ Request %d failed: %v\n", id, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode == http.StatusAccepted {
				// Success (silent to avoid spam)
			} else {
				fmt.Printf("⚠️ Request %d got status: %s\n", id, resp.Status)
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	fmt.Printf("✅ Stress test complete in %v\n", duration)
	fmt.Printf("🚀 Throughput: %.2f req/sec\n", float64(count)/duration.Seconds())
}
