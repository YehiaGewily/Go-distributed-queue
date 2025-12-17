package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"go-queue/internal/queue"
)

func main() {
	// Allow configuration via env var (useful for Docker)
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	// 1. Initialize Global Redis Client
	client := queue.NewClient(addr)
	defer client.Close()

	log.Printf("🚀 Producer API starting on :8085 (Redis: %s)", addr)

	http.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		// Structured Logging: Request Received
		log.Printf("Request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var t queue.Task
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			log.Printf("Error decoding JSON: %v", err)
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}

		// Enrich Data
		if t.ID == "" {
			t.ID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		if t.Type == "" {
			t.Type = "default"
		}
		t.CreatedAt = time.Now()

		// Serialize
		data, err := json.Marshal(t)
		if err != nil {
			log.Printf("Error marshalling task: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Push to Redis
		err = client.LPush(r.Context(), queue.QueuePending, data).Err()
		if err != nil {
			log.Printf("Redis error: %v", err)
			http.Error(w, "Failed to enqueue task", http.StatusInternalServerError)
			return
		}

		// Structured Logging: Task Queued
		log.Printf("✅ Task Queued: ID=%s Type=%s", t.ID, t.Type)

		// Response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // 202 Accepted
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "queued",
			"task_id": t.ID,
		})
	})

	if err := http.ListenAndServe(":8085", nil); err != nil {
		log.Fatal(err)
	}
}
