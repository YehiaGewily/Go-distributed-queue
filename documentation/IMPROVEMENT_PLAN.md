# Go Distributed Queue — Improvement Plan

A phased roadmap to move the project from "working queue demo" to "production-grade, defensible system." Phases are ordered by leverage: each one is independently shippable, and earlier phases unblock later ones.

---

## Phase 1 — Correctness (Critical)

These are the items where the current code does *not* deliver what the README claims. Ship these first.

### 1.1 Reaper for orphaned tasks in `tasks:processing`

**Problem:** `BRPOPLPUSH` moves a task to the processing list atomically, but if a worker crashes (SIGKILL, OOM, host failure), the task sits there forever. The current system loses the task in practice while claiming reliability.

**Design:**
- Each worker, on picking up a task, sets a lease key: `SET task:lease:<id> <worker_id> EX 30`.
- Worker periodically refreshes the lease while processing (every 10s).
- A separate **reaper** goroutine (run inside the worker binary, or as a sidecar) scans `tasks:processing` every 5s. For each entry, it checks if `task:lease:<id>` still exists. If not → the worker died → `LREM` from processing, `LPUSH` back to pending, increment a `reclaimed_total` counter.

**Acceptance criteria:**
- Integration test: enqueue 100 tasks, SIGKILL worker mid-flight, all 100 eventually complete.
- Reclaim latency ≤ 2× lease TTL.

**Files to add/modify:**
- `internal/reaper/reaper.go` — new
- `internal/worker/worker.go` — add lease refresh goroutine per task
- `cmd/worker/main.go` — start reaper alongside worker pool

---

### 1.2 Replace deprecated `BRPOPLPUSH` with `BLMOVE`

**Problem:** `BRPOPLPUSH` is deprecated as of Redis 6.2. `BLMOVE` is the supported equivalent.

**Change:**
```go
// Before
client.BRPopLPush(ctx, "tasks:pending", "tasks:processing", 0)

// After
client.BLMove(ctx, "tasks:pending", "tasks:processing", "RIGHT", "LEFT", 0)
```

**Acceptance criteria:** No deprecation warnings; behavior identical.

---

### 1.3 Replace nanosecond timestamp IDs with ULIDs

**Problem:** `time.Now().UnixNano()` collides under concurrent load (two goroutines in the same nanosecond produce identical IDs). The stress test will silently overwrite tasks if Redis is keyed by ID anywhere.

**Change:**
- Add `github.com/oklog/ulid/v2`.
- Replace ID generation in producer:
  ```go
  id := ulid.Make().String()
  ```
- ULIDs are lexicographically sortable, so debugging logs stay readable.

**Acceptance criteria:** Stress test with 10k concurrent enqueues produces zero duplicate IDs.

---

### 1.4 Idempotency contract

**Problem:** At-least-once delivery means handlers must tolerate duplicate execution. Currently undocumented and unenforced.

**Design:**
- Add optional `idempotency_key` field to task payload.
- Before executing, worker does `SETNX task:done:<key> 1 EX 86400`. If it returns 0, the task already ran — skip and ack.
- Document this contract in the README so producers know to set the key for non-idempotent operations.

**Acceptance criteria:**
- Integration test: enqueue the same `idempotency_key` 5 times, handler runs exactly once.

---

## Phase 2 — Features

### 2.1 Delayed/scheduled tasks via sorted set

**Problem:** Retries currently use `time.Sleep` inside the worker (assumption based on README), which blocks a worker slot for the entire backoff duration. Also: no way to schedule a task for future execution.

**Design:**
- New Redis key: `tasks:delayed` (sorted set, score = unix timestamp when task should execute).
- On retry-after-failure: `ZADD tasks:delayed <now + backoff> <task_json>`.
- New **dispatcher** goroutine polls every 1s: `ZRANGEBYSCORE tasks:delayed -inf <now>`, moves results to `tasks:pending` via Lua script (atomic).

**Bonus:** producers can now schedule tasks for the future:
```json
POST /task
{"type": "email", "payload": "...", "execute_at": "2026-06-01T12:00:00Z"}
```

**Acceptance criteria:**
- Worker availability is independent of retry backoff duration.
- Tasks scheduled for future execution fire within ±1s of target time.

---

### 2.2 Priority queues

**Problem:** Single FIFO means a flood of low-priority tasks starves high-priority ones.

**Design:**
- Three queues: `tasks:pending:high`, `tasks:pending:default`, `tasks:pending:low`.
- Workers poll via `BLMOVE` in priority order with a short timeout per queue (e.g., 1s on high, then check default, then low with longer timeout).
- Producer accepts `priority` field (default = "default").

**Acceptance criteria:**
- With 1000 low-priority tasks queued, a single high-priority task is picked up within 1s.

---

### 2.3 Batch enqueue endpoint

**Problem:** Stress-testing 10k tasks = 10k HTTP requests, which mostly measures HTTP overhead, not queue throughput.

**Design:**
```
POST /tasks  (plural)
[
  {"type": "...", "payload": "..."},
  {"type": "...", "payload": "..."}
]
```
Implemented as a single Redis pipeline with one `LPUSH` per task, or a single multi-arg `LPUSH`.

**Acceptance criteria:** Batch of 1000 tasks completes in <50ms server-side.

---

## Phase 3 — Observability

### 3.1 Prometheus metrics

Stop deferring this. Expose `/metrics` on producer, worker, and monitor.

**Metrics to expose:**
| Metric | Type | Labels |
|---|---|---|
| `queue_depth` | Gauge | `queue` (pending/processing/dlq/delayed) |
| `task_duration_seconds` | Histogram | `type`, `status` (success/failed/retried) |
| `task_enqueue_total` | Counter | `type`, `priority` |
| `task_retries_total` | Counter | `type` |
| `task_reclaimed_total` | Counter | (from reaper) |
| `worker_active` | Gauge | `worker_id` |

**Acceptance criteria:** Grafana dashboard JSON committed to `deploy/grafana/`.

---

### 3.2 Structured logging with `log/slog`

**Change:** Replace any `log.Printf` with `slog` JSON handler. Every log line includes `task_id`, `attempt`, `worker_id`, `task_type` where applicable.

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
logger.Info("task completed",
    "task_id", task.ID,
    "type", task.Type,
    "duration_ms", elapsed.Milliseconds(),
    "attempt", task.Attempt,
)
```

---

### 3.3 OpenTelemetry tracing (optional, nice-to-have)

Producer creates a span on enqueue, embeds `traceparent` in the task payload, worker extracts it and continues the trace. Lets you trace a request from API call through queue wait through worker execution.

---

## Phase 4 — Security & Operations

### 4.1 Producer API hardening

- **Auth:** Bearer token middleware. Token via env var `PRODUCER_API_TOKEN`.
- **Body size limit:** `http.MaxBytesReader(w, r.Body, 64*1024)` — reject payloads >64KB.
- **Rate limiting:** `golang.org/x/time/rate` per-IP, e.g., 100 req/s burst 200.
- **Input validation:** Reject empty `type`, non-JSON payloads, unknown priority values.

### 4.2 Redis hardening

- Require `REDIS_PASSWORD` env var; refuse to start without it in non-dev mode.
- Document AOF persistence config (`appendonly yes`, `appendfsync everysec`) since "zero data loss" depends on it.

### 4.3 Configuration via env vars

Consolidate all config into a struct loaded from env:
```go
type Config struct {
    RedisAddr        string `env:"REDIS_ADDR" default:"localhost:6379"`
    RedisPassword    string `env:"REDIS_PASSWORD"`
    ProducerPort     int    `env:"PRODUCER_PORT" default:"8085"`
    WorkerCount      int    `env:"WORKER_COUNT" default:"10"`
    MaxRetries       int    `env:"MAX_RETRIES" default:"3"`
    LeaseTTLSeconds  int    `env:"LEASE_TTL_SECONDS" default:"30"`
}
```

Use `github.com/caarlos0/env/v11` or stdlib.

---

## Phase 5 — Testing

This is the credibility gap. None of the reliability claims are verifiable without these.

### 5.1 Unit tests with miniredis

`github.com/alicebob/miniredis/v2` — in-memory Redis for fast unit tests. Cover:
- Queue client: enqueue, dequeue, move-to-processing, ack, retry, DLQ.
- Reaper: detects expired leases, reclaims correctly.
- Delayed dispatcher: moves due tasks, leaves future tasks.

### 5.2 Integration tests with real Redis

Spin up Redis via `testcontainers-go`. Cover:
- 1000 tasks enqueued, all executed exactly the expected number of times.
- Worker SIGKILL mid-flight → reaper recovers → all tasks complete.
- Poison pill task → fails 3 times → lands in DLQ.

### 5.3 Chaos test

Add `scripts/chaos/main.go`:
- Spawn 5 workers as subprocesses.
- Enqueue 10k tasks.
- Every 2s, randomly SIGKILL one worker and restart it.
- Assert: zero tasks lost, zero tasks duplicated (with idempotency keys), DLQ count matches injected poison count.

---

## Phase 6 — Polish

### 6.1 Full docker-compose

Currently only Redis. Add producer, worker, monitor as services:
```yaml
services:
  redis: {...}
  producer:
    build: {context: ., dockerfile: build/producer.Dockerfile}
    depends_on: [redis]
  worker:
    build: {context: ., dockerfile: build/worker.Dockerfile}
    depends_on: [redis]
    deploy: {replicas: 3}
  monitor:
    build: {context: ., dockerfile: build/monitor.Dockerfile}
    depends_on: [redis]
```

Then `docker compose up --scale worker=10` actually demonstrates horizontal scaling.

### 6.2 Makefile / justfile

```makefile
.PHONY: up down test stress lint
up: ; docker compose up -d
down: ; docker compose down -v
test: ; go test -race ./...
stress: ; go run scripts/stress_load/main.go
lint: ; golangci-lint run
```

### 6.3 CI via GitHub Actions

`.github/workflows/ci.yml`:
- `go test -race ./...`
- `golangci-lint run`
- `gosec ./...`
- Build all three binaries
- (Optional) Run integration tests against a Redis service container

### 6.4 README cleanup

- Fix the mermaid diagram: workers don't have bidirectional arrows to Redis.
- Document the idempotency contract.
- Document the reliability guarantees precisely: "at-least-once with reaper-based recovery, lease TTL = 30s, max reclaim latency ≈ 60s."
- Add a "Reliability" section explaining the reaper.

---

## Suggested order if you only have a weekend

Do these four, in order, and the project clears the bar from "demo" to "defensible":

1. **Reaper + lease** (Phase 1.1) — the single biggest correctness fix.
2. **ULIDs** (Phase 1.3) — five minutes, removes a silent bug.
3. **Chaos test that kills workers** (Phase 5.3) — proves the reaper works, gives you a screenshot/log to show in interviews.
4. **Prometheus `/metrics`** (Phase 3.1) — the dashboard is fine for demos, `/metrics` is what makes the project look like it belongs in a real cluster.

Everything else is nice-to-have. Those four are the difference between "I wrote a queue" and "I wrote a queue, here's the chaos test that proves it doesn't lose tasks under worker failure."
