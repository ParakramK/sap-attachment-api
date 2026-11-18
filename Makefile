GO ?= go
BIN := bin
APP := sap-attachment-api

.PHONY: fmt vet lint test vuln check build run docker-build clean

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

test:
	$(GO) test -race ./...

# Binary-mode scan: checks exact stdlib+module versions embedded in the
# build without type-checking source (immune to x/vuln vs. go1.27 parsing).
vuln: build
	govulncheck -mode binary $(BIN)/$(APP)

# Run before every commit: format, analyze, test, vulnerability-scan.
check: fmt vet lint test vuln

build:
	$(GO) build -trimpath -o $(BIN)/$(APP) ./cmd/server

run:
	$(GO) run ./cmd/server

docker-build:
	docker build -t $(APP) .

clean:
	rm -rf $(BIN) data
