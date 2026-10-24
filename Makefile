.PHONY: up down logs test itest lint loadtest seed build

COMPOSE = docker compose -f deploy/docker-compose.yml --env-file .env

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f

build:
	go build ./...

test:
	go test ./... -short

itest:
	go test ./tests/integration/... -run . -v

lint:
	go vet ./...
	gofmt -l .

loadtest:
	k6 run loadtest/e1_overhead.js
	k6 run loadtest/e2_split_vs_nosplit.js
	k6 run loadtest/e5_cache.js

seed:
	go run ./cmd/msclient -key mk_demo_a -X POST -idem seed-order-1 \
		-d '{"amount_minor":50000,"currency":"BDT"}' /v1/payments
