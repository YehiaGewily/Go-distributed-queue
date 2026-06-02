.PHONY: up down test test-race test-integration test-chaos stress lint build

up:
	docker compose up -d --build

down:
	docker compose down -v

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

test-integration:
	go test -tags=integration -race ./... -count=1 -timeout 60s

test-chaos:
	go test -tags=chaos -race ./internal/reaper/... -count=1 -timeout 120s

stress:
	go run scripts/stress_load/main.go

lint:
	golangci-lint run

build:
	mkdir -p bin
	go build -o bin/producer cmd/producer/main.go
	go build -o bin/worker cmd/worker/main.go
	go build -o bin/monitor cmd/monitor/main.go
