BINARY   = api-ratelimiter
VERSION  = $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  = -ldflags "-X main.Version=$(VERSION) -s -w"

# Debian version numbers must not carry the leading "v".
VERSION_NUM = $(patsubst v%,%,$(VERSION))

# nfpm is run straight from the module proxy at a pinned version — no
# separate install step, no tool that can drift. Override NFPM to use a
# locally installed binary instead.
NFPM ?= go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.3

.PHONY: build run test test-verbose test-cover test-integration dist clean install lint

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

# Release artifacts, identical to what .github/workflows/release.yml
# produces: static linux/amd64 binary, tarball, .deb, sha256 for both.
# Exists so a release never depends on GitHub Actions still working —
# see "Building without CI" in README.md.
#
#   make dist VERSION=v0.12.0
#
# Without an explicit VERSION the value comes from `git describe`, which
# on an untagged commit yields something like v0.11.0-3-gdeadbee — fine
# for a scratch build, not for a release.
dist:
	@echo "==> building $(BINARY) $(VERSION) (deb version $(VERSION_NUM))"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath $(LDFLAGS) -o $(BINARY) ./cmd/api-ratelimiter
	mkdir -p dist
	tar czf dist/$(BINARY)-$(VERSION)-linux-amd64.tar.gz $(BINARY)
	cd dist && sha256sum $(BINARY)-$(VERSION)-linux-amd64.tar.gz \
	  > $(BINARY)-$(VERSION)-linux-amd64.tar.gz.sha256
	VERSION=$(VERSION_NUM) $(NFPM) pkg --packager deb \
	  --target dist/$(BINARY)_$(VERSION_NUM)_amd64.deb -f packaging/nfpm.yaml
	cd dist && sha256sum $(BINARY)_$(VERSION_NUM)_amd64.deb \
	  > $(BINARY)_$(VERSION_NUM)_amd64.deb.sha256
	@echo "==> artifacts:" && ls -1 dist

lint:
	golangci-lint run ./...

clean:
	rm -rf dist
	rm -f $(BINARY) coverage.out coverage.html

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
