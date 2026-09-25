# Use BotFather Bot API for protected forwarding

Protected forwarding uses a dedicated BotFather bot token, while GramJS user accounts remain responsible for source monitoring and downloads. Telegram exposes per-message `noforwards` for bot sends, but not as a reliable equivalent for ordinary user-account uploads; keeping the two identities separate preserves existing source access and makes protection explicit per destination.

## Consequences

- Bot tokens are stored encrypted and never returned by configuration APIs.
- A protected destination must select a BotFather bot and an explicit destination ID or username.
- User-account forwarding remains supported for destinations that do not require protection.
