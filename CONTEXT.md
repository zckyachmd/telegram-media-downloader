# Telegram forwarding context

This context defines the actors and destination semantics used when downloaded media is posted back to Telegram.

## Actors

**Telegram Account**:
A user-authorized GramJS session used to read source chats, download media, and optionally forward without per-message content protection.
_Avoid_: bot, BotFather account

**BotFather Bot**:
A Telegram Bot API identity created through `@BotFather` and authenticated by a bot token. It can send protected messages with Telegram's `noforwards` behavior.
_Avoid_: userbot, Telegram account

## Forwarding

**Source Topic**:
A forum topic in the monitored source chat used to filter incoming media.

**Destination Topic**:
A forum topic in the target chat where forwarded media is posted. It is independent from the source topic and is selected by destination topic ID.

**Protected Content**:
A message sent with Telegram's content-protection flag, disabling forwarding and normal saving/downloading in supported Telegram clients.

**NSFW Spoiler**:
An explicit per-forward presentation option that sends eligible photos/videos behind Telegram's spoiler cover. It is not enabled merely because the NSFW classifier detects a candidate.
