package dispatcher

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"go-queue/internal/queue"

	"github.com/redis/go-redis/v9"
)

// dispatchScript atomically retrieves, priority-routes, enqueues, and removes
// due tasks from the delayed sorted set.
//
// KEYS[1] = delayed tasks sorted set (tasks:delayed)
// KEYS[2] = pending high list        (tasks:pending:high)
// KEYS[3] = pending default list     (tasks:pending:default)
// KEYS[4] = pending low list         (tasks:pending:low)
// ARGV[1] = current time (Unix timestamp in seconds)
// Returns the number of tasks successfully dispatched.
var dispatchScript = redis.NewScript(`
local delayedKey     = KEYS[1]
local pendingHigh    = KEYS[2]
local pendingDefault = KEYS[3]
local pendingLow     = KEYS[4]
local now            = tonumber(ARGV[1])

local tasks = redis.call("ZRANGEBYSCORE", delayedKey, "-inf", now)
local count = 0

for _, taskData in ipairs(tasks) do
    local task = cjson.decode(taskData)
    local priority = task["priority"]
    local targetQueue = pendingDefault
    if priority == "high" then
        targetQueue = pendingHigh
    elseif priority == "low" then
        targetQueue = pendingLow
    end
    
    redis.call("LPUSH", targetQueue, taskData)
    redis.call("ZREM", delayedKey, taskData)
    count = count + 1
end

return count
`)

type Dispatcher struct {
	client   *redis.Client
	interval time.Duration
}

// NewDispatcher creates a new Dispatcher. The sweep interval defaults to 1 s and can
// be overridden with DISPATCHER_INTERVAL_MS or DISPATCHER_INTERVAL_SECONDS.
func NewDispatcher(client *redis.Client) *Dispatcher {
	interval := 1 * time.Second
	if v := os.Getenv("DISPATCHER_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Millisecond
		}
	} else if v := os.Getenv("DISPATCHER_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	return &Dispatcher{
		client:   client,
		interval: interval,
	}
}

// Run starts the dispatcher loop. It blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	slog.Info("delayed dispatcher started", "interval", d.interval)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("delayed dispatcher stopped")
			return
		case <-ticker.C:
			d.sweep(ctx)
		}
	}
}

// sweep executes one pass of task dispatching using the Lua script.
func (d *Dispatcher) sweep(ctx context.Context) {
	now := time.Now().Unix()
	keys := []string{
		queue.QueueDelayed,
		queue.QueuePendingHigh,
		queue.QueuePendingDefault,
		queue.QueuePendingLow,
	}
	dispatched, err := dispatchScript.Run(ctx, d.client, keys, now).Int64()
	if err != nil {
		slog.Error("dispatcher: failed to run dispatch script", "err", err)
		return
	}

	if dispatched > 0 {
		slog.Info("dispatcher: successfully dispatched delayed tasks", "count", dispatched)
	}
}

// IsDispatcherEnabled returns whether the dispatcher should run (default true,
// configurable via DISPATCHER_ENABLED).
func IsDispatcherEnabled() bool {
	v := os.Getenv("DISPATCHER_ENABLED")
	if v == "" {
		return true
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return enabled
}
