BINARY     := hyperping-exporter
IMAGE_NAME := hyperping-exporter
IMAGE_TAG  := dev
COMPOSE    := docker compose -f deploy/docker-compose.yml

.PHONY: build test lint docker-build docker-run compose-up compose-down clean

build:
	go build -o $(BINARY) .

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

docker-build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-run:
	docker run --rm -p 9312:9312 \
		-e HYPERPING_API_KEY="$$HYPERPING_API_KEY" \
		$(IMAGE_NAME):$(IMAGE_TAG)

compose-up:
	$(COMPOSE) up -d

compose-down:
	$(COMPOSE) down

clean:
	rm -f $(BINARY) coverage.out
