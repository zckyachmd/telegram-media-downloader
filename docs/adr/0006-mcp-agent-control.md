---
status: accepted
---

# MCP agent control boundary

Expose application control through a first-party MCP service in the Go core over authenticated Streamable HTTP, initially bound to localhost. Give each agent an individually revocable OAuth 2.1 identity with configurable operation and account/chat/group scopes; keep the existing cluster token separate. Require operator approval for message sends/edits, destructive actions, bulk operations, and retry-all, with approvals bound to the exact operation and payload. Record one year of redacted agent audit metadata. Keep Telegram account sessions, secrets, backup/restore, filesystem access, and deletion of Telegram messages/groups/topics outside the initial agent surface.
