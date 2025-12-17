package queue

import (
	"encoding/json"
	"time"
)

// Task represents the work to be done.
type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Payload    string    `json:"payload"`
	RetryCount int       `json:"retry_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// Helper to deserialize
func BytesToTask(b []byte) (Task, error) {
	var t Task
	err := json.Unmarshal(b, &t)
	return t, err
}
