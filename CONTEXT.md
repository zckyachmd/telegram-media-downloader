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
