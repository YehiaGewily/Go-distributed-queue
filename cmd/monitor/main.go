package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"go-queue/internal/queue"

	"github.com/redis/go-redis/v9"
)

type DashboardData struct {
	PendingCount    int64
	ProcessingCount int64
	Status          string
	StatusColor     string
	StatusBgColor   string
	LastUpdated     string
}

func main() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := queue.NewClient(addr)
	defer client.Close()

	tmpl, err := template.ParseFiles("cmd/monitor/templates/index.html")
	if err != nil {
		log.Fatalf("Error parsing template: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		// Fetch Stats
		pending, errPending := client.LLen(ctx, queue.QueuePending).Result()
		processing, errProcessing := client.LLen(ctx, queue.QueueProcessing).Result()

		// Determine Status
		status := "System Healthy"
		statusColor := "#4CAF50" // Green

		if errPending != nil || errProcessing != nil {
			status = "Redis Connection Error"
			statusColor = "#F44336" // Red
			if errPending != nil && errPending != redis.Nil {
				log.Printf("Error fetching pending: %v", errPending)
			}
			if errProcessing != nil && errProcessing != redis.Nil {
				log.Printf("Error fetching processing: %v", errProcessing)
			}
		}

		data := DashboardData{
			PendingCount:    pending,
			ProcessingCount: processing,
			Status:          status,
			StatusColor:     statusColor,
			StatusBgColor:   statusColor + "20",
			LastUpdated:     time.Now().Format("15:04:05"),
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "Template error", http.StatusInternalServerError)
		}
	})

	log.Println("📊 Monitor Dashboard starting on :8081")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatal(err)
	}
}
