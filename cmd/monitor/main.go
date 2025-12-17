package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"go-queue/internal/queue"
)

//go:embed templates/index.html
var indexHTML string

type StatsResponse struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
}

func main() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := queue.NewClient(addr)
	defer client.Close()

	// Serve the static HTML page
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(indexHTML))
	})

	// JSON Stats Endpoint
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		pending, err := client.LLen(ctx, queue.QueuePending).Result()
		if err != nil {
			log.Printf("Error fetching pending: %v", err)
			pending = 0
		}

		processing, err := client.LLen(ctx, queue.QueueProcessing).Result()
		if err != nil {
			log.Printf("Error fetching processing: %v", err)
			processing = 0
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatsResponse{
			Pending:    pending,
			Processing: processing,
		})
	})

	log.Println("📊 Monitor Dashboard starting on :8082")
	if err := http.ListenAndServe(":8082", nil); err != nil {
		log.Fatal(err)
	}
}
