# Scan routing and forward queue

Each monitored group can use `autoForward.scanRoute`:

- `download` (default) keeps the existing flow: scan queues a local download; a configured auto-forward runs after ingest.
- `direct` queues the scan result to the persistent forward bucket. It does not create download work or a local media file.
- `both` atomically queues direct forwarding and local download from the same scan result. The normal post-download forward is suppressed for that message, so each destination receives one configured forward job.

Direct-only jobs use the saved group routes, account, captions, and protection settings from enqueue time. Changing group configuration does not rewrite a waiting job's destinations. Direct mode cannot satisfy routes that require protected re-upload or a local file; such jobs fail visibly instead of silently downloading.

Legacy jobs without a saved destination snapshot fail closed before sending. An operator can confirm a legacy uncertain job as delivered, or explicitly request a duplicate-risk resend; that resend records the current target snapshot before it is queued.

The forward worker has its own durable pause flag. The Queue page's global pause and resume actions control both download and forward buckets. Forward bucket controls affect only forward workers. The Queue page's More menu also controls the minimum gap between forward starts (default 1 second; 0 disables the added delay). Per-account FloodWait deadlines are shared by forward workers and persisted; retries keep their due time in SQLite and survive restart. A forward that is still `processing` during migration becomes `uncertain`; it is never automatically resent.

When a Telegram request times out or loses its connection, the response does not prove whether Telegram accepted the forward. The destination becomes `uncertain`, and retry-all excludes it. An operator can confirm delivery after checking the destination. Resend requires explicit duplicate-risk confirmation and writes a row to `tgdl_forward_queue_audit`. A cancellation before a Telegram request is issued stays retryable; a claimed job cancelled while waiting for its pacing slot is returned to `pending`. Telegram's API has no idempotency key or reliable search receipt here, so delivery is not exactly once.

The MCP `queue.snapshot` tool accepts an optional one-based `page` for forward queue results. `forward.pause` and `forward.resume` (scope `forward.control`) control only that bucket; `download.pause` and `download.resume` (scope `queue.control`) control only downloads. `forward.reconcile` requires an idempotency key, accepts a destination index from the snapshot, and checks authorization for the source chat, source account, and every saved destination. Mutations are audited. Local paths and session credentials are not included in snapshots.
