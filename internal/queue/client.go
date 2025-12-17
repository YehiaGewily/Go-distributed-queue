package queue

import (
	"github.com/redis/go-redis/v9"
)

// NewClient creates a new Redis client.
func NewClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: addr,
	})
}

const (
	QueuePending    = "tasks:pending"
	QueueProcessing = "tasks:processing"
	QueueDeadLetter = "tasks:dead_letter"
)
