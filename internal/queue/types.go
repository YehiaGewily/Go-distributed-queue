package queue

import (
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"
)

// Task represents the work to be done.
type Task struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	Payload        string     `json:"payload"`
	Priority       string     `json:"priority,omitempty"`
	ExecuteAt      *time.Time `json:"execute_at,omitempty"`
	RetryCount     int        `json:"retry_count"`
	CreatedAt      time.Time  `json:"created_at"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

// NewID generates a unique, time-sortable task ID using ULID.
func NewID() string {
	return ulid.Make().String()
}

// Helper to deserialize
func BytesToTask(b []byte) (Task, error) {
	var t Task
	err := json.Unmarshal(b, &t)
	return t, err
}
