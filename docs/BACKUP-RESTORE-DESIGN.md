# Full backup and restore

## Goal

Provide two Web UI actions: **Create and download backup** and **Upload and
restore**. The operator enters a password each time. Full recovery archives
are stored locally and are never sent to media backup destinations.

## Package contents

The package contains a consistent SQLite snapshot, app configuration and
secrets, Telegram account/session files, monitored chats and other database
state, queues and history, persistent app logs, all regular files under the app
data directory, and downloaded media. An external `TGDL_DOWNLOADS_DIR` is
included.

SQLite sidecars, lock/session temporary files, partial `.part` downloads, older
backup archives, symlinks, and special files are excluded or rejected. Host and
container operating-system logs are not included. The encrypted package limit
is 64 GiB minus 32 bytes; an oversized backup fails without publishing a
partial archive.

## Setup and backup

1. Enter a password of at least 12 characters.
2. **Create and download backup** creates an authenticated encrypted package
   under `<TGDL_DATA_DIR>/backups/` and automatically downloads it when ready.
   The archive includes its random salt, so the password alone can decrypt it
   on another server.

## Restore

1. Select a `.tgdf` package, enter its password, and confirm **Upload and
   restore**.
2. The server authenticates the uploaded package and creates an encrypted
   local rollback package with the same password before changing active files.
   Failure before activation leaves active files unchanged.
3. It validates archive paths and SQLite integrity in staging. External media
   is staged beside its configured target.
4. A durable journal swaps staged files after the server stops and completes an
   interrupted swap before opening the database on startup.

## Existing backup destinations

The existing destination feature remains for continuous media mirroring and
state-only scheduled/manual snapshots. These jobs are separate from the local
full recovery archive and are not used by **Restore latest**.

## Safety

- Admin authentication and existing CSRF protection apply to download,
  backup, and restore.
- Passwords are not stored as settings. Temporary archives use restrictive
  permissions and are removed after use.
- Restore rejects traversal, links, special files, unsupported entries, and
  extraction-limit violations.
- The source and restored Telegram sessions never run simultaneously.
