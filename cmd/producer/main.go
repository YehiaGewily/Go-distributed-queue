package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go-queue/internal/metrics"
	"go-queue/internal/queue"

	"github.com/redis/go-redis/v9"
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

	mux := setupMux(client)

	if err := http.ListenAndServe(":8085", mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func setupMux(client *redis.Client) *http.ServeMux {
	mux := http.NewServeMux()

	// Expose Prometheus metrics
	mux.Handle("/metrics", metrics.Handler())

	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
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

		// Validate priority
		priority, valid := validatePriority(t.Priority)
		if !valid {
			slog.Warn("invalid priority requested", "priority", t.Priority)
			http.Error(w, "Invalid priority. Must be 'high', 'default', or 'low'", http.StatusBadRequest)
			return
		}
		t.Priority = priority

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

		// Push to Redis (delayed or priority pending queue)
		var redisErr error
		if t.ExecuteAt != nil && t.ExecuteAt.After(time.Now()) {
			redisErr = client.ZAdd(r.Context(), queue.QueueDelayed, redis.Z{
				Score:  float64(t.ExecuteAt.Unix()),
				Member: data,
			}).Err()
			slog.Info("task scheduled for future execution", "id", t.ID, "execute_at", t.ExecuteAt)
		} else {
			qName := queue.GetPendingQueue(t.Priority)
			redisErr = client.LPush(r.Context(), qName, data).Err()
			slog.Info("task enqueued", "id", t.ID, "queue", qName)
		}

		if redisErr != nil {
			slog.Error("redis error", "err", redisErr)
			http.Error(w, "Failed to enqueue task", http.StatusInternalServerError)
			return
		}

		// Structured Logging: Task Queued
		slog.Info("task queued successfully", "id", t.ID, "type", t.Type, "priority", t.Priority)
		metrics.TaskEnqueueTotal.WithLabelValues(t.Type).Inc()

		// Response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // 202 Accepted
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":  "queued",
			"task_id": t.ID,
		}); err != nil {
			slog.Error("failed to encode response", "err", err)
		}
	})

	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		// Structured Logging: Request Received
		slog.Info("request received", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var tasks []queue.Task
		if err := json.NewDecoder(r.Body).Decode(&tasks); err != nil {
			slog.Error("error decoding JSON array", "err", err)
			http.Error(w, "Invalid JSON body, expected array of tasks", http.StatusBadRequest)
			return
		}

		// Validate & Enrich all tasks first
		now := time.Now()
		var taskIDs []string
		for i := range tasks {
			t := &tasks[i]
			// Validate priority
			priority, valid := validatePriority(t.Priority)
			if !valid {
				slog.Warn("invalid priority requested in batch", "priority", t.Priority, "index", i)
				http.Error(w, fmt.Sprintf("Invalid priority at index %d. Must be 'high', 'default', or 'low'", i), http.StatusBadRequest)
				return
			}
			t.Priority = priority

			// Enrich
			if t.ID == "" {
				t.ID = queue.NewID()
			}
			if t.Type == "" {
				t.Type = "default"
			}
			t.CreatedAt = now
			taskIDs = append(taskIDs, t.ID)
		}

		// Pipelined Redis Exec
		pipe := client.Pipeline()
		ctx := r.Context()
		for _, t := range tasks {
			data, err := json.Marshal(t)
			if err != nil {
				slog.Error("error marshalling task in batch", "err", err)
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}

			if t.ExecuteAt != nil && t.ExecuteAt.After(now) {
				pipe.ZAdd(ctx, queue.QueueDelayed, redis.Z{
					Score:  float64(t.ExecuteAt.Unix()),
					Member: data,
				})
			} else {
				qName := queue.GetPendingQueue(t.Priority)
				pipe.LPush(ctx, qName, data)
			}
		}

		_, err := pipe.Exec(ctx)
		if err != nil {
			slog.Error("redis batch pipeline execution error", "err", err)
			http.Error(w, "Failed to enqueue batch of tasks", http.StatusInternalServerError)
			return
		}

		// Update metrics & log
		for _, t := range tasks {
			metrics.TaskEnqueueTotal.WithLabelValues(t.Type).Inc()
		}
		slog.Info("batch tasks queued successfully", "count", len(tasks))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // 202 Accepted
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "queued",
			"count":    len(tasks),
			"task_ids": taskIDs,
		}); err != nil {
			slog.Error("failed to encode response", "err", err)
		}
	})

	return mux
}

// validatePriority checks if the priority is valid, defaults to "default" if empty.
func validatePriority(p string) (string, bool) {
	switch p {
	case "":
		return "default", true
	case "high", "default", "low":
		return p, true
	default:
		return "", false
	}
}
