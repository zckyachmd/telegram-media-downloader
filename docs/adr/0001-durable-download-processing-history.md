# Durable download processing history

Record every processing attempt in durable history, separate from the current media catalog, so retries, duplicate skips, forwarding, and deletion remain auditable and statistics stay accurate after local files are removed. Show the history and statistics in Settings → Tools → Logs; count attempts separately from unique files, and let the existing Group Monitor cleanup action clear history only for the selected group.
