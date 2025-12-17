# Go Distributed Queue

![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)
![Redis](https://img.shields.io/badge/Redis-7.0+-DC382D?style=flat&logo=redis)
![Docker](https://img.shields.io/badge/Docker-v24+-2496ED?style=flat&logo=docker)
![REST API](https://img.shields.io/badge/API-REST-4CAF50?style=flat)

**A high-throughput, fault-tolerant distributed task processing system implementing the Reliable Queue Pattern.**

Designed to handle concurrent workloads with zero data loss, ensuring robustness in distributed environments through atomic state transitions and graceful lifecycle management.

## Architecture

The system decouples task ingestion from processing using a persistent Redis layer, managed by a scalable pool of concurrent workers.

```mermaid
flowchart LR
    Client[Client / External Service] --> |POST /task| API[REST API Producer]
    API --> |LPUSH JSON| Redis[(Redis Queue)]
    
    subgraph "Worker Cluster"
        W1[Worker 1]
        W2[Worker 2]
        W3[Worker N]
    end
    
    Redis <--> |Atomic BRPopLPush| W1
    Redis <--> |Atomic BRPopLPush| W2
    Redis <--> |Atomic BRPopLPush| W3
```

## Key Engineering Concepts

### Reliable Queue Pattern (Atomic RPOPLPUSH)
Implements the **Reliable Queue Pattern** to guarantee **at-least-once processing**.
- Tasks are atomically moved from `tasks:pending` to `tasks:processing` using `BRPopLPush`.
- This ensures that if a worker crashes mid-execution, the task remains in the `processing` list and is not lost, adhering to strict data integrity requirements.

### Concurrency & Parallelism
Leverages Go's scheduler and efficient goroutines to maximize throughput.
- **Worker Pools**: Spawns multiple concurrent processors (default: 5) managed via `sync.WaitGroup`.
- **Non-blocking I/O**: Efficiently handles idle waiting on Redis connections without consuming OS threads.

### Graceful Shutdowns
Implements robust signal handling (`SIGINT`, `SIGTERM`) using `os/signal` and context cancellation.
- Ensures all in-flight tasks complete execution before the process terminates.
- Prevents data corruption and "half-processed" states during deployments or scaling events.

## Quick Start

### Prerequisites
- Docker & Docker Compose
- go 1.23+

### 1. Start the System
Initialize the Redis infrastructure.
```bash
docker-compose up -d redis
```

### 2. Start the Services
Run each service in a separate terminal:

**Producer API (Port 8085)**
```bash
go run cmd/producer/main.go
```

**Monitor Dashboard (Port 8082)**
```bash
go run cmd/monitor/main.go
```

**Worker Pool**
```bash
go run cmd/worker/main.go
```

*Services available:*
- **Producer API**: `http://localhost:8085`
- **Monitor Dashboard**: `http://localhost:8082`
- **Redis**: `localhost:6379`

### 3. Dispatch Tasks
You can send tasks manually or run the stress test script.

**Using the Stress Test Script:**
```bash
go run scripts/stress_load/main.go
```

**Using cURL:**
```bash
curl -X POST http://localhost:8085/task \
     -H "Content-Type: application/json" \
     -d '{"type": "email-notification", "payload": "user@example.com"}'
```

**Response:**
```json
{
  "status": "queued",
  "task_id": "1734301548123456789"
}
```

## Future Improvements
- **Dead Letter Queue (DLQ)**: For processing repeatedly failed tasks.
- **Metrics Export**: Prometheus integration for queue depth and latency monitoring.
- **Dynamic Scaling**: Horizontal Pod Autoscaling (HPA) based on queue lag.
