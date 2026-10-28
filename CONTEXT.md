# Glossary

- **Locale**: A language used for user-facing text in the web application.
- **Auto**: Use the browser's preferred language when available; otherwise use English.
- **User guide**: Documentation that helps operators install, configure, use, or troubleshoot the application.
- **Processing attempt**: One recorded attempt to process a source message, including retries of the same message.
- **Unique file**: One distinct media item in the library; duplicate source messages do not increase this count.
- **Processing history**: Durable records of processing attempts, with forwarding and local deletion represented separately from download or duplicate-skip outcomes.
- **Forward route**: A rule that sends files from a source topic to a destination; **All** applies to every topic and can run alongside topic-specific routes.
- **Forward destination**: The Telegram chat, channel, Saved Messages, or Telegram Downloader Storage selected for a route.
- **Active forward**: A file currently being uploaded to its configured destination.
- **Forward job**: One durable queue item that resolves source metadata, downloads media only when required, and delivers it to one or more destinations.
- **Forward method**: The delivery path for one destination: native Telegram forwarding or local download followed by upload.
- **Protected copy**: A destination message sent with Telegram content protection enabled when the account/API permits it.
- **Effective protection**: The resolved protection setting after global, group, topic, and route overrides are applied when a job is queued.
- **Destination outcome**: The durable delivery status for one destination within a forward job; retrying a job skips destinations already sent successfully.
- **Dev asset serving**: In local development, serve frontend files from the working tree so edits appear after a browser refresh without rebuilding the Go binary.
