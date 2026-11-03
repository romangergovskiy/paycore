VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/romangergovskiy/paycore/internal/app.Version=$(VERSION)
STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || echo $(HOME)/go/bin/staticcheck)

.PHONY: build run test race vet fmt lint check migrate-bootstrap migrate-up migrate-down migrate-status

build:
	go build -ldflags "$(LDFLAGS)" -o bin/paycore-api ./cmd/api
	go build -o bin/paycore-migrate ./cmd/migrate

# Loads .env when present; the application itself reads only the environment.
run:
	@set -a; [ -f .env ] && . ./.env; set +a; go run -ldflags "$(LDFLAGS)" ./cmd/api

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint:
	@if [ -x "$(STATICCHECK)" ]; then $(STATICCHECK) ./...; else echo "staticcheck not installed, skipping"; fi

check: fmt vet lint race

migrate-bootstrap:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/migrate bootstrap

migrate-up:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/migrate up

migrate-down:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/migrate down

migrate-status:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/migrate status
