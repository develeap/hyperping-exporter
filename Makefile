VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
REVISION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

BINARY     := hyperping-exporter
IMAGE_NAME := hyperping-exporter
IMAGE_TAG  := dev
COMPOSE    := docker compose -f deploy/docker-compose.yml

.PHONY: build test lint docker-build docker-run compose-up compose-down clean fmt vet coverage govulncheck release-dry-run all \
        helm-render helm-kubeconform helm-pss helm-pss-clean helm-ci-fast helm-ci

CHART_DIR        := deploy/helm/hyperping-exporter
CHART_TESTS_DIR  := $(CHART_DIR)/tests
PINS_FILE        := $(CHART_TESTS_DIR)/pins.expected.yaml
KUBECONFORM_CATALOG_REF := $(shell awk -F'"' '/^datreeio_crds_catalog_tag:/{print $$2}' $(PINS_FILE))
HELM_RENDER_FIXTURES := \
	default external-secret external-secret-defaults \
	replicas-zero \
	networkpolicy-default networkpolicy-dns-override \
	networkpolicy-cilium-defaults \
	networkpolicy-cilium-with-ingress networkpolicy-cilium-matchexpressions \
	networkpolicy-cilium-mixed cache-ttl-numeric log-level-numeric \
	metrics-path-with-special-chars pss-restricted ascii-regex \
	readme-regex single-quote-regex mcp-url both-flags \
	quote-regex mcp-url-query existing-secret servicemonitor-enabled

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION) -X main.revision=$(REVISION)" -o $(BINARY) .

test:
	go test -race -coverprofile=coverage.out -coverpkg=./internal/... ./...
	@pct=$$(go tool cover -func=coverage.out | awk '/^total:/{gsub(/%/,""); print $$3}'); \
		echo "Coverage: $${pct}%"; \
		awk -v p="$$pct" 'BEGIN { if (p+0 < 90) { print "FAIL: coverage " p "% is below 90%"; exit 1 } }'

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
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

release-dry-run:
	goreleaser release --snapshot --clean

all: fmt vet lint test build

# ---- Helm chart targets ----

helm-render:
	python3 $(CHART_TESTS_DIR)/render_test.py

# Run kubeconform against every fixture's rendered output. Catalog tag
# is pinned via $(KUBECONFORM_CATALOG_REF) (read from pins.expected.yaml)
# so schema drift is a deliberate update, never an upstream surprise.
#
# CiliumNetworkPolicy is skipped because the pinned CRDs-catalog tag
# (v0.0.12) does not include the `spec.enableDefaultDeny` field that
# Cilium 1.14+ shipped and that the chart now relies on for the
# ingress-lockdown parity fix (R8). Schema validation for CNP will be
# re-enabled when the catalog tag is rolled forward to one that ships
# the Cilium v1.14+ schema. Until then, render_test.py keeps strong
# structural assertions on the rendered Cilium spec, and the live
# Cilium CRD accepts the field without complaint.
helm-kubeconform:
	@command -v kubeconform >/dev/null || { echo "kubeconform not on PATH"; exit 2; }
	@if [ -z "$(KUBECONFORM_CATALOG_REF)" ]; then echo "KUBECONFORM_CATALOG_REF empty (pins.expected.yaml)"; exit 2; fi
	@set -e; \
	for f in $(HELM_RENDER_FIXTURES); do \
		echo "=== kubeconform: $$f ==="; \
		helm template testrel $(CHART_DIR) -f $(CHART_TESTS_DIR)/fixtures/$$f.values.yaml \
		  | kubeconform -strict -summary -skip CiliumNetworkPolicy -schema-location default \
		      -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/$(KUBECONFORM_CATALOG_REF)/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
		      -; \
	done

helm-pss:
	bash $(CHART_TESTS_DIR)/admission_test.sh

helm-pss-clean:
	@. $(CHART_TESTS_DIR)/admission_env.sh; kind delete cluster --name "$$KIND_CLUSTER_NAME" 2>/dev/null || true

helm-ci-fast: helm-render helm-kubeconform

helm-ci: helm-ci-fast helm-pss
