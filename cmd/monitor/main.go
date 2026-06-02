package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go-queue/internal/metrics"
	"go-queue/internal/queue"
)

//go:embed templates/index.html
var indexHTML string

type StatsResponse struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
	DeadLetter int64 `json:"dead_letter"`
}

func main() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := queue.NewClient(addr)
	defer client.Close()

	// Background goroutine: update queue_depth gauges every 5 seconds
	go func() {
		ctx := context.Background()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			pending, err := client.LLen(ctx, queue.QueuePending).Result()
			if err != nil {
				slog.Error("monitor: error fetching pending", "err", err)
				pending = 0
			}
			processing, err := client.LLen(ctx, queue.QueueProcessing).Result()
			if err != nil {
				slog.Error("monitor: error fetching processing", "err", err)
				processing = 0
			}
			dlq, err := client.LLen(ctx, queue.QueueDeadLetter).Result()
			if err != nil {
				slog.Error("monitor: error fetching dead_letter", "err", err)
				dlq = 0
			}
			metrics.QueueDepth.WithLabelValues("pending").Set(float64(pending))
			metrics.QueueDepth.WithLabelValues("processing").Set(float64(processing))
			metrics.QueueDepth.WithLabelValues("dlq").Set(float64(dlq))
		}
	}()

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
			slog.Error("error fetching pending", "err", err)
			pending = 0
		}

		processing, err := client.LLen(ctx, queue.QueueProcessing).Result()
		if err != nil {
			slog.Error("error fetching processing", "err", err)
			processing = 0
		}

		deadLetter, err := client.LLen(ctx, queue.QueueDeadLetter).Result()
		if err != nil {
			slog.Error("error fetching dead letter", "err", err)
			deadLetter = 0
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StatsResponse{
			Pending:    pending,
			Processing: processing,
			DeadLetter: deadLetter,
		})
	})

	// Expose Prometheus metrics
	http.Handle("/metrics", metrics.Handler())

	slog.Info("monitor dashboard starting", "addr", ":8082")
	if err := http.ListenAndServe(":8082", nil); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
