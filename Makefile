GO ?= go

.PHONY: build run dev test check

build:
	CGO_ENABLED=0 $(GO) -C core-service build -trimpath -o tgdl-server ./cmd/tgdl-server

run: build
	./runner.sh

dev:
	cd core-service && PORT=3003 TGDL_BIND_HOST=127.0.0.1 TRUST_PROXY=0 TGDL_DEV_MODE=1 air -c ../.air.toml

test:
	$(GO) -C core-service test ./...

check:
	@test -z "$$(gofmt -l core-service)" || { echo 'Run gofmt on the changed Go files.'; exit 1; }
	$(GO) -C core-service vet ./...
	$(GO) -C core-service test -race ./...
