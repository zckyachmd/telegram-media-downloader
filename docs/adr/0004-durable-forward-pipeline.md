# ADR 0004: Durable forward pipeline

## Status

Accepted

## Context

Forwarding needs to choose a delivery method for each destination, retain the effective protection and caption rules for queued work, and retry failed destinations without duplicating successful sends. A single local download should serve every destination that needs an upload.

## Decision

Keep one durable forward job per source media item. Snapshot resolved destination settings at enqueue time and persist each destination's method, stage, and outcome in the job. Use native Telegram forwarding when the source caption can be retained and protection is off. Use the library file when caption transformation or protection requires a new upload. Manual forwards of library media enter the same queue.

## Consequences

- Successful destinations are skipped on retry.
- Route and protection edits do not rewrite existing jobs.
- Native forwarding avoids a local media download when every destination qualifies.
- Telegram's `noforwards` parameter is documented as bot-only by GoTD; user sessions may reject it, so the application must show that failure rather than claim protection succeeded.
