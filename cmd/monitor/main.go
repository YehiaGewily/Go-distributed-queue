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
	PendingHigh    int64 `json:"pending_high"`
	PendingDefault int64 `json:"pending_default"`
	PendingLow     int64 `json:"pending_low"`
	Processing     int64 `json:"processing"`
	DeadLetter     int64 `json:"dead_letter"`
	Delayed        int64 `json:"delayed"`
}

func main() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := queue.NewClient(addr)
	defer func() { _ = client.Close() }()

	// Background goroutine: update queue_depth gauges every 5 seconds
	go func() {
		ctx := context.Background()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			pendingHigh, err := client.LLen(ctx, queue.QueuePendingHigh).Result()
			if err != nil {
				slog.Error("monitor: error fetching pending high", "err", err)
				pendingHigh = 0
			}
			pendingDefault, err := client.LLen(ctx, queue.QueuePendingDefault).Result()
			if err != nil {
				slog.Error("monitor: error fetching pending default", "err", err)
				pendingDefault = 0
			}
			pendingLow, err := client.LLen(ctx, queue.QueuePendingLow).Result()
			if err != nil {
				slog.Error("monitor: error fetching pending low", "err", err)
				pendingLow = 0
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
			delayed, err := client.ZCard(ctx, queue.QueueDelayed).Result()
			if err != nil {
				slog.Error("monitor: error fetching delayed", "err", err)
				delayed = 0
			}
			metrics.QueueDepth.WithLabelValues("pending_high").Set(float64(pendingHigh))
			metrics.QueueDepth.WithLabelValues("pending_default").Set(float64(pendingDefault))
			metrics.QueueDepth.WithLabelValues("pending_low").Set(float64(pendingLow))
			metrics.QueueDepth.WithLabelValues("processing").Set(float64(processing))
			metrics.QueueDepth.WithLabelValues("dlq").Set(float64(dlq))
			metrics.QueueDepth.WithLabelValues("delayed").Set(float64(delayed))
		}
	}()

	// Serve the static HTML page
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write([]byte(indexHTML)); err != nil {
			slog.Error("failed to write response", "err", err)
		}
	})

	// JSON Stats Endpoint
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		pendingHigh, err := client.LLen(ctx, queue.QueuePendingHigh).Result()
		if err != nil {
			slog.Error("error fetching pending high", "err", err)
			pendingHigh = 0
		}

		pendingDefault, err := client.LLen(ctx, queue.QueuePendingDefault).Result()
		if err != nil {
			slog.Error("error fetching pending default", "err", err)
			pendingDefault = 0
		}

		pendingLow, err := client.LLen(ctx, queue.QueuePendingLow).Result()
		if err != nil {
			slog.Error("error fetching pending low", "err", err)
			pendingLow = 0
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

		delayed, err := client.ZCard(ctx, queue.QueueDelayed).Result()
		if err != nil {
			slog.Error("error fetching delayed", "err", err)
			delayed = 0
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(StatsResponse{
			PendingHigh:    pendingHigh,
			PendingDefault: pendingDefault,
			PendingLow:     pendingLow,
			Processing:     processing,
			DeadLetter:     deadLetter,
			Delayed:        delayed,
		}); err != nil {
			slog.Error("failed to encode response", "err", err)
		}
	})

	// Expose Prometheus metrics
	http.Handle("/metrics", metrics.Handler())

	slog.Info("monitor dashboard starting", "addr", ":8082")
	if err := http.ListenAndServe(":8082", nil); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
