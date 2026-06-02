package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go-queue/internal/metrics"
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

	slog.Info("producer API starting", "addr", ":8085", "redis", addr)

	// Expose Prometheus metrics
	http.Handle("/metrics", metrics.Handler())

	http.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		// Structured Logging: Request Received
		slog.Info("request received", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var t queue.Task
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			slog.Error("error decoding JSON", "err", err)
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}

		// Enrich Data
		if t.ID == "" {
			t.ID = queue.NewID()
		}
		if t.Type == "" {
			t.Type = "default"
		}
		t.CreatedAt = time.Now()

		// Serialize
		data, err := json.Marshal(t)
		if err != nil {
			slog.Error("error marshalling task", "err", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Push to Redis
		err = client.LPush(r.Context(), queue.QueuePending, data).Err()
		if err != nil {
			slog.Error("redis error", "err", err)
			http.Error(w, "Failed to enqueue task", http.StatusInternalServerError)
			return
		}

		// Structured Logging: Task Queued
		slog.Info("task queued", "id", t.ID, "type", t.Type)
		metrics.TaskEnqueueTotal.WithLabelValues(t.Type).Inc()

		// Response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // 202 Accepted
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "queued",
			"task_id": t.ID,
		})
	})

	if err := http.ListenAndServe(":8085", nil); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
