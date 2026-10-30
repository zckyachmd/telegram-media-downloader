# Local MCP control

The Go server starts MCP on `127.0.0.1:3001`. This listener is separate from the dashboard listener and is not reachable from other machines. Set `TGDL_MCP_PORT` to change its local port, `TGDL_MCP_ISSUER` when a local reverse proxy provides the externally visible issuer URL, or `TGDL_MCP_DISABLED=1` to turn MCP off.

Manage agents in **Settings → MCP access**. The admin API below remains available for provisioning automation:

New agents created in the web UI default to all connected accounts and chats. The wildcard includes accounts and chats added later. Clear either **All** grant to restrict that resource type. API clients must send `"*"` explicitly; omitted or empty grants deny access.

```http
POST /api/mcp/agents
Content-Type: application/json

{
  "name": "queue-operator",
  "role": "operator",
  "accountIds": ["<account-id>"],
  "chatIds": ["<chat-id>"]
}
```

The response contains a client ID and one-time client secret. Store the secret in the agent's credential store. The secret is never returned again; rotate it to replace a lost credential. Agent scopes may be narrowed or customized in the same request. Each access token lasts one hour and is revoked when its agent is updated, rotated, or revoked.

The OAuth token endpoint is `POST http://127.0.0.1:3001/oauth/token`, using HTTP Basic client authentication and `grant_type=client_credentials`. The MCP Streamable HTTP endpoint is `http://127.0.0.1:3001/mcp`. A client may request any subset of the scopes assigned to its identity.

## Available tools

- `agent.self`, `monitor.status`, `groups.list`, `groups.upsert`, `groups.remove`
- `queue.snapshot`, `queue.retry`, `queue.retry_all`, `forward.retry`
- `topics.list`, `topics.create`, `topics.update`, `topics.bulk_create`, `topics.bulk_update`
- `messages.send`, `messages.edit`
- `approvals.list`, `approvals.decide`

Topic and message writes, monitored-chat removal, and retry-all create an approval with a ten-minute expiry. Topic updates can change title, closed state, icon, and General-topic visibility. The approval contains the exact operation and encrypted payload. Approvers must be a different active operator/admin identity, hold the operation scope, and be allowed for its account and chat. Approval can be completed with the MCP tools; the dashboard admin API remains available.

MCP uses only connected Telegram accounts while the monitor is running. It does not expose account login, session credentials, filesystem paths, backups, or Telegram deletion operations. Queue results are bounded and omit local file paths. Bulk topic actions cap each request at 50 items and return per-item outcomes.

## Audit and approvals

Audit records contain actor, action, target, outcome, approval ID, and a payload digest. They omit message bodies and credentials and are retained for one year. Pending approval payloads are encrypted with the data-directory key file `mcp.key`; keep that file protected and preserve it with the application's protected data when moving the installation.

Approvals bind a hash of the exact encrypted payload. The web UI shows the payload and digest before enabling the approval action; approval executes the exact approved operation. At decision time the requester must still be active and retain the required chat/account scope. An agent cannot approve its own request. Bulk approvals report each item's result.
