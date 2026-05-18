BINARY   = api-ratelimiter
VERSION  = $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  = -ldflags "-X main.Version=$(VERSION) -s -w"

.PHONY: build run test test-verbose test-cover test-integration clean install lint

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/api-ratelimiter

run: build
	./$(BINARY) \
	  --listen unix:/tmp/ratelimit.sock \
	  --admin-listen 127.0.0.1:8080 \
	  --metrics-listen 127.0.0.1:9091 \
	  --redis-addr 127.0.0.1:6379 \
	  --log-level debug \
	  --log-format text \
	  --global-limit 100 \
	  --burst 20 \
	  --window second

test:
	go test ./...

test-verbose:
	go test -v ./...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

test-integration:
	go test -tags=integration -timeout=10m ./test/integration/...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY) coverage.out coverage.html

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
