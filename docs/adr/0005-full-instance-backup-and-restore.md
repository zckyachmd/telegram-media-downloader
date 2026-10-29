# Full-instance backup and restore

Provide an application-local full recovery archive, separate from media mirror destinations. An administrator enters a password to create one encrypted package containing application state, Telegram sessions, downloaded media (including media stored under the configured external downloads directory), and persistent application logs/history. The Web UI downloads it automatically when creation finishes. Restore uploads a selected package, asks for its password and explicit confirmation, creates a local rollback package with the same password, and stages replacement. The package carries a random salt, so restore works on a different server without a server-stored key. A single package removes category choices and reduces mismatched restore points, while staged restore protects active data.

See [full backup and restore design](../BACKUP-RESTORE-DESIGN.md) for scope and flow.
