package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ── Configuration ──────────────────────────────────────────────────────────

type config struct {
	BaseURL     string
	Duration    time.Duration
	Concurrency int
	RPS         int // target requests per second (0 = unlimited)
	BatchSize   int // tasks per batch request (0 = single-task mode)
}

// ── Counters ───────────────────────────────────────────────────────────────

type stats struct {
	sent      atomic.Int64
	success   atomic.Int64
	failed    atomic.Int64
	latencyNs atomic.Int64 // cumulative latency in nanoseconds
}

// ── Task payloads ──────────────────────────────────────────────────────────

type task struct {
	Type           string  `json:"type"`
	Payload        string  `json:"payload"`
	Priority       string  `json:"priority,omitempty"`
	ExecuteAt      *string `json:"execute_at,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

var (
	taskTypes  = []string{"email", "resize", "export", "webhook", "report"}
	priorities = []string{"high", "default", "default", "default", "low"} // weighted: 60% default
)

func randomTask(id int) task {
	t := task{
		Type:     taskTypes[rand.Intn(len(taskTypes))],
		Payload:  fmt.Sprintf("stress-data-%d-%d", id, time.Now().UnixNano()),
		Priority: priorities[rand.Intn(len(priorities))],
	}

	// 10% of tasks are scheduled 2–8 seconds in the future
	if rand.Intn(10) == 0 {
		future := time.Now().Add(time.Duration(2+rand.Intn(7)) * time.Second).UTC().Format(time.RFC3339)
		t.ExecuteAt = &future
	}

	// 15% of tasks carry an idempotency key (some will collide on purpose)
	if rand.Intn(100) < 15 {
		t.IdempotencyKey = fmt.Sprintf("idem-%d", rand.Intn(50)) // only 50 distinct keys → collisions
	}

	return t
}

// ── HTTP helpers ───────────────────────────────────────────────────────────

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     30 * time.Second,
	},
}

func sendSingle(url string, t task) (int, error) {
	body, _ := json.Marshal(t)
	resp, err := httpClient.Post(url+"/task", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func sendBatch(url string, tasks []task) (int, error) {
	body, _ := json.Marshal(tasks)
	resp, err := httpClient.Post(url+"/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// ── Worker loop ────────────────────────────────────────────────────────────

func worker(cfg config, s *stats, stop <-chan struct{}, throttle <-chan time.Time) {
	seq := 0
	for {
		select {
		case <-stop:
			return
		default:
		}

		// Throttle if RPS target is set
		if throttle != nil {
			select {
			case <-throttle:
			case <-stop:
				return
			}
		}

		seq++
		start := time.Now()

		var code int
		var err error

		if cfg.BatchSize > 0 {
			// Batch mode
			batch := make([]task, cfg.BatchSize)
			for i := range batch {
				batch[i] = randomTask(seq*1000 + i)
			}
			code, err = sendBatch(cfg.BaseURL, batch)
			s.sent.Add(int64(cfg.BatchSize))
		} else {
			// Single-task mode
			t := randomTask(seq)
			code, err = sendSingle(cfg.BaseURL, t)
			s.sent.Add(1)
		}

		elapsed := time.Since(start).Nanoseconds()
		s.latencyNs.Add(elapsed)

		if err != nil || code >= 400 {
			s.failed.Add(1)
		} else {
			s.success.Add(1)
		}
	}
}

// ── Live stats reporter ────────────────────────────────────────────────────

func reporter(s *stats, stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastSent int64
	lastTime := time.Now()

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			sent := s.sent.Load()
			ok := s.success.Load()
			fail := s.failed.Load()
			dt := now.Sub(lastTime).Seconds()
			rps := float64(sent-lastSent) / dt

			avgLatMs := float64(0)
			if ok+fail > 0 {
				avgLatMs = float64(s.latencyNs.Load()) / float64(ok+fail) / 1e6
			}

			fmt.Printf("  ⚡ %6d tasks sent | %5.0f tasks/s | ✅ %d ok | ❌ %d fail | avg %6.1fms\n",
				sent, rps, ok, fail, avgLatMs)

			lastSent = sent
			lastTime = now
		}
	}
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	url := flag.String("url", "http://localhost:8085", "Producer base URL")
	duration := flag.Duration("duration", 2*time.Minute, "Test duration (e.g. 30s, 2m, 5m)")
	concurrency := flag.Int("concurrency", 20, "Number of concurrent request goroutines")
	rps := flag.Int("rps", 100, "Target requests per second (0 = unlimited)")
	batchSize := flag.Int("batch", 0, "Batch size per request (0 = single-task mode)")
	flag.Parse()

	cfg := config{
		BaseURL:     *url,
		Duration:    *duration,
		Concurrency: *concurrency,
		RPS:         *rps,
		BatchSize:   *batchSize,
	}

	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║           DISTRIBUTED QUEUE STRESS TEST             ║")
	fmt.Println("╠══════════════════════════════════════════════════════╣")
	fmt.Printf("║  Target:      %-38s ║\n", cfg.BaseURL)
	fmt.Printf("║  Duration:    %-38s ║\n", cfg.Duration)
	fmt.Printf("║  Concurrency: %-38d ║\n", cfg.Concurrency)
	if cfg.RPS > 0 {
		fmt.Printf("║  Target RPS:  %-38d ║\n", cfg.RPS)
	} else {
		fmt.Printf("║  Target RPS:  %-38s ║\n", "unlimited")
	}
	if cfg.BatchSize > 0 {
		fmt.Printf("║  Batch Size:  %-38d ║\n", cfg.BatchSize)
	} else {
		fmt.Printf("║  Mode:        %-38s ║\n", "single-task")
	}
	fmt.Println("╠══════════════════════════════════════════════════════╣")
	fmt.Println("║  Task mix:  email, resize, export, webhook, report  ║")
	fmt.Println("║  Priority:  20% high · 60% default · 20% low       ║")
	fmt.Println("║  Delayed:   ~10% scheduled 2-8s in the future       ║")
	fmt.Println("║  Idempotency: ~15% with keys (intentional collisions)║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")
	fmt.Println()

	// Rate limiter (shared ticker across all workers)
	var throttle <-chan time.Time
	if cfg.RPS > 0 {
		interval := time.Second / time.Duration(cfg.RPS)
		t := time.NewTicker(interval)
		defer t.Stop()
		throttle = t.C
	}

	s := &stats{}
	stop := make(chan struct{})

	// Launch reporter
	go reporter(s, stop)

	// Launch workers
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(cfg, s, stop, throttle)
		}()
	}

	// Run for the configured duration
	time.Sleep(cfg.Duration)
	close(stop)
	wg.Wait()

	// ── Final report ───────────────────────────────────────────────────
	sent := s.sent.Load()
	ok := s.success.Load()
	fail := s.failed.Load()
	totalRequests := ok + fail

	avgLatMs := float64(0)
	if totalRequests > 0 {
		avgLatMs = float64(s.latencyNs.Load()) / float64(totalRequests) / 1e6
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║                    FINAL RESULTS                    ║")
	fmt.Println("╠══════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total tasks sent:     %-29d ║\n", sent)
	fmt.Printf("║  Successful requests:  %-29d ║\n", ok)
	fmt.Printf("║  Failed requests:      %-29d ║\n", fail)
	fmt.Printf("║  Avg latency:          %-25.1f ms  ║\n", avgLatMs)
	fmt.Printf("║  Throughput:           %-22.1f tasks/s  ║\n", float64(sent)/cfg.Duration.Seconds())
	fmt.Printf("║  Duration:             %-29s ║\n", cfg.Duration)
	fmt.Println("╚══════════════════════════════════════════════════════╝")
}
