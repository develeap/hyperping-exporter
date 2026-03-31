VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
REVISION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

BINARY     := hyperping-exporter
IMAGE_NAME := hyperping-exporter
IMAGE_TAG  := dev
COMPOSE    := docker compose -f deploy/docker-compose.yml

.PHONY: build test lint docker-build docker-run compose-up compose-down clean fmt vet coverage govulncheck release-dry-run all

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION) -X main.revision=$(REVISION)" -o $(BINARY) .

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

docker-build: build
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-run:
	docker run --rm -p 9312:9312 \
		-e HYPERPING_API_KEY="$$HYPERPING_API_KEY" \
		$(IMAGE_NAME):$(IMAGE_TAG)

compose-up: build
	$(COMPOSE) up -d --build

compose-down:
	$(COMPOSE) down

clean:
	rm -f $(BINARY) coverage.out coverage.html

fmt:
	gofmt -l -w .

vet:
	go vet ./...

coverage: test
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

release-dry-run:
	goreleaser release --snapshot --clean

all: fmt vet lint test build
