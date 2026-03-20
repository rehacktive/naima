<img src="internal/httpapi/ui/logo2.png" alt="Naima Logo" width="320">

Naima is a Go-based AI agent with persistent memory, a streaming web UI, Telegram integration, a personal knowledge base, and a tool-based execution model.

## What It Does

Naima combines:
- chat over web UI, REST API, or Telegram
- Memorya-backed conversation memory persisted in PostgreSQL/pgvector
- personal knowledge base ingestion for URLs, notes, and files
- semantic retrieval over PKB document chunks
- browser automation through Playwright
- browser automation through Lightpanda
- Linux shell execution inside an isolated Debian sidecar
- web/news search through SearxNG
- scheduled alarms and agent tasks
- Telegram delivery for results and audio workflows

## Run

### Full stack with Docker Compose

```sh
cp .env.example .env
# Set at least:
# - OPENAI_API_KEY
# - OPENAI_MODEL
# - OPENAI_EMBEDDING_MODEL
# - NAIMA_API_TOKEN and/or TELEGRAM_BOT_TOKEN
# - DOMAIN if using Caddy/TLS

docker compose up -d --build
```

Services started:
- `naima`
- `caddy`
- `pgvector`
- `redis`
- `searxng`
- `tika`
- `bash-tool`
- `lightpanda`

### Local development services only

If you want to run Naima directly on the host:

```sh
docker compose -f docker-compose.dev.yml up -d
```

Exposed ports:
- `pgvector` on `localhost:5432`
- `redis` on `localhost:6379`
- `searxng` on `localhost:8081`
- `tika` on `localhost:9998`
- `bash-tool` on `localhost:8090`
- `lightpanda` on `localhost:9222`

## Main Features

### Web UI
- streaming chat
- live markdown rendering
- operations panel
- theme selector
- optional Basic Auth
- personal knowledge base tab with 3D view
- URL/file ingestion dialog
- scoped chat over selected PKB documents

### Browser extension
- Chrome/Brave popup extension to ingest the current tab URL into Naima
- optional Telegram notification after ingestion completes
- source lives in `browser-extension/naima-ingest`

### Telegram
- account linking via link code
- optional draft streaming
- audio transcription with OpenAI
- generated voice replies with OpenAI
- `/new` and `/reset` to clear memory

### Personal Knowledge Base
- topics and documents stored in PostgreSQL
- full document content stored in `pkb_documents`
- chunk embeddings stored in `pkb_embeddings`
- URL ingestion via hybrid extraction
- file ingestion via Tika
- semantic retrieval used during chat for PKB-like questions

### Memory
- active context managed by Memorya
- embeddings persisted in pgvector
- summarization compacts context when limits are reached

## Configuration

Use [.env.example](/Users/aw4y/dev/naima/.env.example) as the base template.

Important env groups:
- OpenAI-compatible client: `OPENAI_*`
- REST/UI: `NAIMA_API_*`, `NAIMA_UI_*`
- Telegram: `TELEGRAM_BOT_TOKEN`, `NAIMA_TELEGRAM_STREAM`, `NAIMA_TTS_*`
- Memory/pgvector: `NAIMA_MEMORY_*`, `NAIMA_PGVECTOR_*`
- PKB/Tika: `NAIMA_TIKA_*`, `NAIMA_PKB_*`
- Tool defaults: `NAIMA_TOOL_<TOOL_NAME>`
- Lightpanda: `NAIMA_LIGHTPANDA_*`
- Playwright: `NAIMA_PLAYWRIGHT_*`
- Bash tool: `NAIMA_BASH_TOOL_URL`
- Tasks: `NAIMA_TASK_TIMEZONE`

## Documentation

Detailed references:
- [REST API](docs/api.md)
- [Tools](docs/tools.md)

## Browser Extension

Load the unpacked extension from:
- `browser-extension/naima-ingest`

It lets you:
- set local Naima URL and API token
- choose an existing PKB topic or create a new one
- ingest the current browser tab URL
- request Telegram notification when the ingest completes

## Command line

```sh
go run . -name "Naima"
```
