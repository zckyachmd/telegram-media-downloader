# Building on memory-limited ARM64 hosts

The core Go server contains a large generated Telegram client package. Building
multiple packages at once can cause Docker/BuildKit to run out of RAM, producing
`github.com/gotd/td/tg: ... compile: signal: killed` or
`ResourceExhausted ... cannot allocate memory`.

The root Dockerfile cross-compiles on `BUILDPLATFORM` and now defaults to:

- `go build -p 1` — only one Go package compiler process at a time.
- `GOMAXPROCS=2` — lower CPU parallelism inside the compiler.
- `GOMEMLIMIT=1536MiB` — soft limit for Go-managed heap, **not** a hard
  container or host memory limit; native mappings, linker and BuildKit still
  use memory. The host needs additional headroom.
- BuildKit cache mounts for dependencies and compiled objects, reducing work
  on repeat builds without changing executable contents.

## Local build

```sh
# From repo root; default configuration prioritizes low memory usage.
docker compose build telegram-downloader
# Launch only after image build succeeds.
docker compose up -d --no-build telegram-downloader
```

For a very small host, lower the compiler heap target (it may increase garbage
collection work). This is a soft heap target, not a guarantee against OOM:

```sh
TGDL_GO_BUILD_JOBS=1 TGDL_GO_BUILD_MAXPROCS=1 TGDL_GO_BUILD_MEMLIMIT=1024MiB \\
  docker compose build --progress=plain telegram-downloader
```

Avoid running multiple heavy BuildKit builds at once. When more RAM is
available, `TGDL_GO_BUILD_JOBS=2` may speed up builds at the cost of higher
peak memory use.

## Production deployment recommendation

Do **not** compile on a small production host if build latency or memory
pressure risks the running service. Build multi-arch images in dedicated CI
runners (or on a stronger workstation) and deploy them using image-only
Compose. The Synology Compose already pulls a prebuilt GHCR image. Build and
runtime memory are independent: `deploy.resources.limits.memory` controls the
application container, not the ephemeral Docker builder.

## Validation

```sh
docker compose config --quiet
docker compose build --progress=plain telegram-downloader
docker image inspect telegram-media-downloader:local --format '{{.Architecture}}/{{.Os}}'
```

Check target architecture when cross-compiling; successful amd64 builds do
not establish ARM64 compatibility. On ARM64 hosts inspect memory use with
`docker stats` during runtime and system-level metrics during the build.
