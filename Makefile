GO ?= go
AIR ?= air
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS ?= $(shell $(GO) -C core-service env GOOS)
GOARCH ?= $(shell $(GO) -C core-service env GOARCH)
DIST_DIR ?= dist
RELEASE_NAME = tgdl-server_$(VERSION)_$(GOOS)_$(GOARCH)

.PHONY: build run dev release release-linux release-linux-amd64 release-linux-arm64 test check

build:
	CGO_ENABLED=0 $(GO) -C core-service build -trimpath -o tgdl-server ./cmd/tgdl-server

run: build
	./runner.sh

dev:
	@echo "Open the dashboard at http://127.0.0.1:3002"
	TGDL_DATA_DIR="$${TGDL_DATA_DIR:-$(CURDIR)/data/dev}" PORT=3003 TGDL_BIND_HOST=127.0.0.1 TRUST_PROXY=0 TGDL_DEV_MODE=1 $(AIR) -c .air.toml

release:
	@mkdir -p "$(DIST_DIR)/$(RELEASE_NAME)"
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) -C core-service build -trimpath -ldflags "-X github.com/botnick/telegram-media-downloader/core-service/internal/version.Version=$(VERSION)" -o "$(CURDIR)/$(DIST_DIR)/$(RELEASE_NAME)/tgdl-server" ./cmd/tgdl-server
	cp LICENSE README.md config.example.json "$(DIST_DIR)/$(RELEASE_NAME)/"
	tar -C "$(DIST_DIR)" -czf "$(DIST_DIR)/$(RELEASE_NAME).tar.gz" "$(RELEASE_NAME)"
	rm -rf "$(DIST_DIR)/$(RELEASE_NAME)"

release-linux: release-linux-amd64 release-linux-arm64

release-linux-amd64:
	$(MAKE) release GOOS=linux GOARCH=amd64

release-linux-arm64:
	$(MAKE) release GOOS=linux GOARCH=arm64

test:
	$(GO) -C core-service test ./...

check:
	@test -z "$$(gofmt -l core-service)" || { echo 'Run gofmt on the changed Go files.'; exit 1; }
	$(GO) -C core-service vet ./...
	$(GO) -C core-service test -race ./...
