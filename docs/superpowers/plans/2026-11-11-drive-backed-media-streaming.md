# Drive-backed media streaming implementation plan

**Goal:** Let tgdl stream mirrored media from Google Drive after safely evicting its local copy, while keeping local disk use bounded and supporting smooth preview and seeking in tgdl.

**Architecture:** Reuse the existing Google Drive OAuth destination and mirror objects. Keep stable remote-source metadata when local files are evicted. Extend the existing media-serving path to read only requested byte ranges from Drive and return standard range responses. Keep a small, bounded disk cache for actively streamed data; do not introduce rclone or Jellyfin.

**Scope assumption:** This plan covers Google Drive continuous mirror media, not snapshot archives, encrypted mirror objects, other backup providers, or a general-purpose streaming server. First release requires one unambiguous readable Drive mirror per media object; multi-destination fallback can follow if current catalog semantics require it.

## Existing code to reuse

- `core-service/internal/backup.Provider` already has `Download`, and `gdriveProvider` already downloads mirror objects. Add a range-capable read operation to the provider contract rather than buffering whole objects through `Download`.
- `core-service/internal/app/file_cleanup.go` already evicts local media after a completed mirror and keeps catalog rows. Preserve/extend that eligibility flow rather than adding another deletion path.
- `core-service/internal/app/media_api.go` has the media delivery path and HTTP range behavior for local files. Use the same authenticated route and response semantics for Drive-backed reads.
- Existing per-destination backup configuration, job state, and UI should remain the source of truth for mirror availability.

## Constraints and safety

- Never delete a local file until its upload is complete and its remote object is verified against expected size and checksum.
- Never evict media that has no usable, enabled, unencrypted Google Drive mirror. Preserve existing protection for active transfers and other references to the same path.
- Keep catalog identity, filename, size, MIME type, and remote path after local eviction; local `file_path` alone cannot remain the source locator.
- Validate paths and destination/object association at the trust boundary. Streaming endpoints must use normal tgdl authentication/authorization; never expose raw Drive tokens or public links.
- Remote reads must honor request cancellation, use bounded buffers, and avoid temporary full-file downloads. Bound and clean up cache data; expose a conservative default cache limit.
- If Drive is unavailable, fail the media request clearly and retain the remote catalog entry for retry. Do not silently remove it or claim it is playable.
- Thumbnail generation and background analysis must not trigger full media downloads after eviction. Existing local derived assets may continue to be served.

## Tasks

- [x] Trace mirror job records, destination eligibility, encryption state, shared-file references, and the current media route; settle the smallest durable schema for `(download, destination, remote path, verified size/checksum)` and migration behavior. The durable locator is the destination ID on the download; the completed backup job remains the source of the remote object path and verification record.
- [x] Add a streaming/range reader to the backup provider boundary. Implement Google Drive object metadata lookup and ranged media reads; handle servers that ignore a Range request safely, and do not buffer an entire object in RAM.
- [x] Persist the verified remote destination locator as part of safe eviction after a completed mirror job. Eviction clears only local-file presence while retaining catalog metadata; destination disablement, edits, and removal are blocked while it is the only copy.
- [x] Extend the existing media endpoint to select local file first, otherwise the verified Drive source. Implement `HEAD`, single byte ranges, `206`, `Content-Range`, `Content-Length`, `Accept-Ranges`, `416`, MIME/disposition headers, and cancellation. Keep auth and route compatibility for current dashboard preview URLs.
- [x] Add bounded range cache for active/nearby byte windows, with a conservative fixed cap, eviction policy, and cleanup on process restart. Do not cache full files by default. Existing proxy/compression path was not changed.
- [x] Prevent thumbnail, seekbar, AI, integrity, dedup, faststart, and other local-only jobs from accidentally downloading remote media. They select only rows with a local path (or serve already-generated derived assets); the gallery marks Drive-only rows as `remoteOnly`.
- [x] Update backup UI/status with local vs Drive-only state and cache behavior. Eviction remains opt-in through the existing delete-after-upload setting and is scoped to verified, enabled, unencrypted Google Drive mirror destinations.
- [x] Add tests for provider range behavior, local-first/remote-fallback serving, range edge cases and repeated requests, auth, upload-before-evict ordering, shared references, failed upload, encryption, disabled/removed destination, cancellation, cache bounds, and no full-download background processing.
- [ ] Verify on a low-memory Pi-class host with a large MP4: playback starts before full transfer, seeks work, local disk usage stays within the cache bound, RAM remains bounded, and Drive failure produces a recoverable error. Record Drive request volume for sequential playback and repeated seeks.
- [x] Update `docs/BACKUP.md` and user-facing backup/media help to document Drive-only streaming requirements, limits, cache, and the fact that Google API/network availability affect playback.

**Initial verification status (range-only phase):** `go test ./internal/app ./internal/backup ./internal/store -count=1` passes (649 tests), and a Linux ARM64 server binary cross-build succeeds. Release `v3.4.23-0fda0d4` was first deployed and healthy on `z-server-tunnel`; the later cleanup UI release and playback checks are recorded below. A bounded range-only check requested seven 64 KiB ranges and received seven `206` responses (about 460 KiB total); the cold first response took 4.87s, later uncached reads took 1.01–1.32s, a repeated range took 105ms, and two concurrent ranges completed in 2.03s. At that time the process counters showed 7 Drive attempts and about 6 MiB fetched with no retries. It was not player playback; actual quota, sustained playback, and host load were still unmeasured then.

## Acceptance criteria

- A mirrored video can be previewed and sought from tgdl after its local copy is evicted.
- Range reads fetch only the requested portions (plus bounded read-ahead/cache), never the full file by default.
- No local eviction occurs before verified mirror completion; catalog rows remain available if Drive is temporarily unavailable.
- Local and existing preview behavior remains unchanged when files are present on disk.
- Cache use is bounded and measurable; background processing cannot silently refill the host disk with full remote media.
- Tests cover failure and edge cases above, and low-memory host verification demonstrates the intended disk/RAM tradeoff.

## Main tradeoff

Drive-backed seek depends on internet latency and Google Drive API behavior/quotas. Bounded caching reduces repeated reads but cannot make remote access as predictable as local storage; users must be able to keep local copies or disable eviction when reliability matters more than disk space.

## Follow-up audit plan: latency, quota, and host I/O

### Audit findings

- `gdriveProvider.ReadRange` issues one Drive media API request per 1 MiB block. The current published schedule prices media downloads at 200 quota units/request. The active project's Cloud Console showed 1,000,000 units/minute per project, 325,000 units/minute per user, and 12,000 previous requests/minute per project/user; project-specific schedules differ. The current quota and usage snapshot is recorded below.
- Retryable 403/429 responses now establish a shared cooldown across streams, and media range retries have a 10-second retry-delay budget. The HTTP transport timeout still bounds individual network operations; actual first-byte and retry latency need host measurements.
- The app and backup manager each cap concurrent streams/range reads at two. They do not cap request rate or project egress. A daily hard cap inside tgdl cannot account for other applications using the same Google Cloud project, so it must not be presented as a project-wide quota guarantee.
- Cache writes now require an independent second read of the same block, avoiding persistent writes during one-pass sequential playback; this policy has fixture coverage but still needs Pi measurement. The 32 MiB LRU cap remains.
- The initial `0fda0d4` deployment exposed stale build metadata because `/etc/tgdl/tgdl.env` overrode embedded version values. The later `b2e5be4` cleanup release is active; its checksum and health are recorded below. That first snapshot had 2 downloads and 2 backfills active; the later stream test also overlapped background backup work, so its cgroup I/O cannot be attributed solely to playback.

### Ordered improvements

1. **Measure before tuning.** Done locally: the admin-only `GET /api/backup/media/stats` endpoint reports process-lifetime Drive media attempts/retries/status codes/bytes/retry wait and stream bytes/cache hits/misses/writes/evictions plus average, maximum, and bucket-estimated p50/p95 first-byte latency. Counters reset on restart; the endpoint does not estimate project-wide quota use.
2. **Stop sequential cache write-through.** Done locally: a block is admitted to disk only after a second independent read; an in-memory admission index is bounded to 512 keys. The existing 32 MiB ceiling remains. A one-pass Pi playback has not yet been measured.
3. **Coalesce identical concurrent cache misses.** Done locally with the existing `singleflight` dependency. Tests verify one Drive read for identical concurrent ranges, prompt cancellation of a waiting caller, and successful completion of the leader.
4. **Coordinate quota backoff across streams.** Done locally in the shared cloud API client: retryable 403/429 establish a shared cooldown, media range retries have a 10-second retry-delay budget, and numeric `Retry-After` parsing is overflow-safe. Tests verify a second range waits through a 429 cooldown. Full network request latency is still subject to the HTTP transport timeout.
5. **Evaluate a sequential-read window separately from seek windows.** Pending Pi measurements. Keep the first block small for time-to-first-byte and random seeks; benchmark a larger block for contiguous reads to reduce request count. Change the block size only if measured request reduction outweighs seek overfetch and startup latency on the Pi.
6. **Use project settings for any hard throttling.** The active project's per-minute quotas are now known, but its daily egress value and per-user usage were not visible in the Console page. No hard cap is added: tgdl cannot see usage by other clients sharing the project. Track tgdl's stream counters and check Cloud Console usage; do not claim a project-wide daily egress cap.
7. **Run a controlled Pi-class benchmark.** Partially measured on 2026-11-11: Chrome played a remote-only 875 MB video to 1:50, sought backward 30 seconds, and overlapped it with a second remote-only stream for about 10 seconds. This showed working range playback and seeking, but was not a clean load benchmark because backup uploads/downloads were active. Keep the full test pending until first-frame time, stall count, user-visible seek latency, and attributable cgroup I/O are measured without background transfers.

#### Benchmark protocol

1. Before the run, record the actual Drive project quota in Google Cloud Console and capture `GET /api/backup/media/stats`. Treat the API counters as deltas from this snapshot; they reset on service restart and do not include calls from other clients.
2. Use a dedicated, unencrypted large MP4 already mirrored to Drive. Play it through tgdl for a fixed interval, seek to several distant offsets, then repeat one seek. The bytes are streamed to the player; the benchmark does not create a full local media copy. Record elapsed start-to-first-frame and player stalls.
3. Repeat with two concurrent players. During each run, sample `/sys/fs/cgroup/system.slice/tgdl.service/memory.current`, `memory.peak`, `io.stat`, and `/proc/pressure/io` before and after; also capture host `vmstat` at a fixed interval.
4. Compare counter deltas for media attempts, retries/statuses, fetched/served bytes, cache hits/misses/writes/evictions, and p50/p95 TTFB. Keep `io.stat` deltas and the test interval so disk activity can be rate-normalized. Use the injected slow/429 fixture tests for error-path validation; do not inject faults into the production Drive endpoint.
5. Only compare a larger sequential-read block after capturing the current 1 MiB baseline. Any candidate must reduce Drive request count without worsening first-frame time, seek latency, quota behavior, or Pi memory/I/O. Do not select a project request-rate cap until its configured quota is known.

### Follow-up acceptance checks

- One pass over a large file causes no persistent cache writes for blocks that are never reused; disk cache remains at or below 32 MiB.
- Concurrent requests for the same block cause one Drive media read under normal operation.
- Repeated seeks to a previously admitted block are served from cache; sequential playback preserves first-byte latency and does not generate excessive Drive requests.
- 403/429 bursts trigger one shared cooldown, respect cancellation, avoid retry storms, and return a recoverable error within the media-request retry budget.
- The approved Pi benchmark records p50/p95 time-to-first-byte, Drive API media attempts and bytes, cache stats, process RSS/HWM, cgroup I/O, and system I/O wait for both one and two streams.

**Local implementation validation:** `go test ./internal/app ./internal/backup ./internal/store -count=1` passes (649 tests). A full `go test ./... -count=1` run passed 1,286 tests but failed `internal/hash/TestRootThatIsALink` and `internal/fsx/TestRemoveTreeKeepsSharedFiles`; both also fail when run individually and neither package was changed here. The active project was checked read-only in Google Cloud Console: Drive API quotas displayed 1,000,000 units/minute per project, 325,000 units/minute per user, and a previous request quota of 12,000/minute per project and per user. Console usage at the time showed about 193 units/minute and 2.57 requests/minute; per-user usage was unavailable. The short Chrome playback test added 90 media requests, 91.2 MB fetched from Drive, and 95.6 MB served, with no retries. The process-wide first-byte metrics after the test showed 2.28 s average, 5.12 s maximum, a 2 s p50 bucket and 10 s p95 bucket (10 samples, cumulative since restart). The cache directory was 25 MB under its 32 MiB cap. Cgroup memory rose from 329.4 MB to 333.2 MB; process RSS after the test was 77 MB and `memory.peak` was 339 MB against a 384 MiB limit. Disk I/O deltas are not attributable to streaming because background backup work was active. The published Drive API page lists 200 quota units per `alt=media` request and a 1 TB/day per-project egress threshold; the Cloud Console page did not show daily egress or usable per-user consumption, and tgdl cannot account for other clients using the project. No project-wide hard cap is claimed. The full controlled benchmark remains open because first-frame time, visible stalls, and isolated I/O were not measured.

**References:** [Drive API usage limits](https://developers.google.com/workspace/drive/api/guides/limits), [Drive API error handling](https://developers.google.com/workspace/drive/api/guides/handle-errors).

### Cleanup UI follow-up (2026-11-11)

The Drive mirror card already exposed “Clean uploaded files” for disabled destinations, and the API/backend allow this operation while checking that every enabled mirror has a completed copy. The UI handler incorrectly rejected disabled destinations before showing the safety confirmation. Commit `b2e5be4` removes that guard. “Sync media now” still correctly requires an enabled destination; the disabled card uses “Enable & run now” with confirmation. The ARM64 release at `/opt/tgdl/releases/v3.4.23-b2e5be4` is active and its checksum matches the local build. The service is healthy, and the served `/js/maintenance-backup.js` no longer contains the rejecting guard. A graceful restart returned the in-progress upload to the durable queue; it resumed afterward with 8 pending and 1 uploading at the latest read-only snapshot. `/etc/tgdl/tgdl.env` now has `GIT_SHA=b2e5be4`, but the active process still reports `0fda0d4` until its next restart. The live Drive mirror has `delete_after_upload=1`; no cleanup or manual sync was triggered.

### Chrome playback and quota check (2026-11-11)

In Chrome, the authenticated inline media endpoint played an 875 MB Drive-only MP4. The player timeline advanced to 1:50; a 30-second backward seek succeeded. A separate 543 MB Drive-only MP4 played concurrently for about 10 seconds. Both players were paused afterward. No full-file download was initiated or saved; only playback ranges were fetched. Stream-stat deltas for this run were 90 Drive media requests, 91,226,112 bytes fetched, 95,552,626 bytes served, 6 additional cache hits, 90 misses, 21 writes, zero evictions, and zero retries. The first-byte figures above are coarse process-lifetime metrics, not a direct TTFF or per-seek stopwatch. The 1 MiB read size yielded roughly one media request per MiB; no larger-window variant was tested.

The same test ran while background transfers remained active (latest snapshot: 8 pending, 1 uploading). Cgroup `io.stat` grew by roughly 3.9 GB read and 2.7 GB written during the wider observation window, so those deltas cannot be assigned to playback. `memory.current` changed from 329,363,456 to 333,234,176 bytes, `memory.peak` was 339,021,824 bytes, the server RSS was about 77 MB, and the disk cache was 25 MB. There were no cgroup OOM events; the cgroup memory usage exceeded the 320 MiB `MemoryHigh` threshold during this busy period.
