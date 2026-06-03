package queue

import (
	"github.com/redis/go-redis/v9"
)

// NewClient creates a new Redis client connected to the given address on DB 0.
func NewClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: addr,
	})
}

// NewClientWithDB creates a new Redis client connected to the given address
// and database number. This is useful for tests that need isolation from the
// default DB.
func NewClientWithDB(addr string, db int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   db,
	})
}

const (
	QueuePendingHigh    = "tasks:pending:high"
	QueuePendingDefault = "tasks:pending:default"
	QueuePendingLow     = "tasks:pending:low"
	QueuePending        = QueuePendingDefault
	QueueProcessing     = "tasks:processing"
	QueueDeadLetter     = "tasks:dead_letter"
	QueueDelayed        = "tasks:delayed"
)

// GetPendingQueue returns the Redis key for the given priority queue.
func GetPendingQueue(priority string) string {
	switch priority {
	case "high":
		return QueuePendingHigh
	case "low":
		return QueuePendingLow
	default:
		return QueuePendingDefault
	}
}
