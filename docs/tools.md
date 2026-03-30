# Tools Reference

Naima loads tool guidance dynamically:
- base prompt from `prompt.txt`
- per-tool guidance from `internal/tools/<tool_name>.md`
- only enabled tools are injected into the model prompt

## Registered tools

| Tool | Description |
| --- | --- |
| `time` | current local and UTC time |
| `weather` | weather + 7-day forecast for a location |
| `web_search` | generic search over local SearxNG |
| `news_digest` | curated news digest over SearxNG news results |
| `personal_knowledge_base` | CRUD over topics/documents plus ingestion and temporal search |
| `pkb_retrieve` | explicit semantic retrieval over ingested PKB documents and chunks |
| `bash` | bash execution inside an isolated Debian sidecar container |
| `playwright` | browser automation and page extraction |
| `task_scheduler` | persistent alarms and scheduled agent tasks |
| `long_memory` | semantic recall and LLM summary of past conversation |
| `memory_dump` | debug dump of current in-memory conversation state |
| `email` | read mail over POP3, wait for confirmation emails, and send mail over SMTP |
| `telegram_send` | send a Telegram message to the linked account when Telegram is configured |

## Notes by tool

### `time`
- returns current local and UTC time

### `weather`
- input: `location`
- returns current conditions and a 7-day forecast
- uses Open-Meteo

### `web_search`
- generic web/news/images search
- backed by local SearxNG

### `news_digest`
- topic-focused news summarization tool
- deduplicates and summarizes SearxNG news results
- useful for recurring digests and scheduled tasks

### `personal_knowledge_base`
Supports:
- topic CRUD
- document CRUD
- URL ingestion
- note ingestion
- temporal search

Storage behavior:
- full document content stays in `pkb_documents`
- chunk embeddings are stored in `pkb_embeddings`
- extracted tags are stored in `pkb_tags` and linked through `pkb_document_tags`
- those embeddings are used for semantic retrieval during chat

Tag extraction:
- runs automatically on document create/update and ingestion
- uses the configured chat model to extract relevant tags from document content
- stores each tag with both text and category
- number of tags per document is controlled by `NAIMA_PKB_TAG_LIMIT` (default `12`)
- on startup, only missing tag rows are backfilled
- on startup, only missing embedding rows are backfilled

URL ingestion modes:
- `hybrid`
- `tika`
- `playwright`
- `fetch`

Hybrid mode combines:
- direct fetch
- Tika extraction
- Playwright extraction
- deterministic cleanup into normalized markdown/text

File ingestion:
- files are stored locally
- text is extracted through Tika
- extracted content is saved as a PKB document

### `pkb_retrieve`
- semantic retrieval over `pkb_embeddings`
- embeds the query and returns nearest PKB documents plus relevant chunks
- use when the answer should come from ingested PKB content rather than conversation memory

### `bash`
- executes bash commands in a Debian sidecar container
- supports package installation and persistent workspace files inside the container
- returns stdout, stderr, exit code, timeout status, and working directory

### `playwright`
Supports operations such as:
- `goto`
- `scrape`
- `click`
- `type`
- `press`
- `evaluate`
- `screenshot`
- `snapshot_for_ai`
- `close`

### `task_scheduler`
Supports:
- one-shot alarms
- one-shot agent tasks
- cron-style recurring tasks
- PostgreSQL persistence across restarts
- Telegram delivery when enabled

### `long_memory`
- retrieves related prior conversation through embeddings
- summarizes the recalled messages with the LLM

### `memory_dump`
- debugging tool
- returns current in-memory messages as JSON

### `email`
- inbox read/poll via POP3
- message send via SMTP
- useful for account signup, email confirmation, password reset, and mailbox automation flows
- configured entirely from `NAIMA_EMAIL_*` env vars

### `telegram_send`
- sends a text message to the linked Telegram account
- available only when Telegram is configured

## Default tool state

At startup you can preconfigure which tools are enabled:
- one env var per tool, using `NAIMA_TOOL_<TOOL_NAME>`
- supported values: `enabled` or `disabled`
- if the env var is empty or unset, the tool starts enabled by default

Example:
- `NAIMA_TOOL_WEB_SEARCH=enabled`
- `NAIMA_TOOL_PLAYWRIGHT=disabled`
- `NAIMA_TOOL_BASH=disabled`
