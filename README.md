<img src="internal/httpapi/ui/logo2.png" alt="Naima Logo" width="320">

Naima is a Go-based AI agent with:
- persistent conversation memory via [Memorya](https://github.com/rehacktive/memorya)
- pgvector-backed recall
- a built-in streaming web UI
- Telegram integration
- a personal knowledge base with 3D visualization
- tool calling, scheduling, browser automation, and web search

## Overview

Main capabilities:
- chat through web UI, REST API, or Telegram
- stream model output to the web UI and optionally to Telegram drafts
- persist conversation memory in PostgreSQL/pgvector
- ingest URLs and files into a personal knowledge base
- browse/search the web through local SearxNG
- extract document text through Apache Tika
- automate websites through Playwright
- schedule alarms or future agent tasks persisted in PostgreSQL
- send results to Telegram

## Run

### Full stack with Docker Compose

```sh
cp .env.example .env
# Edit .env and set at least:
# - OPENAI_API_KEY
# - OPENAI_MODEL
# - OPENAI_EMBEDDING_MODEL
# - NAIMA_API_TOKEN and/or TELEGRAM_BOT_TOKEN
# - DOMAIN if you want Caddy/TLS

docker compose up -d --build
```

This starts:
- `naima`
- `caddy`
- `pgvector`
- `redis`
- `searxng`
- `tika`

### Local development services only

If you want to run Naima directly on your host and keep only dependencies in Docker:

```sh
docker compose -f docker-compose.dev.yml up -d
```

This starts:
- `pgvector` on `localhost:5432`
- `redis` on `localhost:6379`
- `searxng` on `localhost:8081`
- `tika` on `localhost:9998`

In this mode, `naima` and `caddy` are not started.

### Run only the Naima container

```sh
docker build -t naima:latest .
docker run --rm -it --env-file .env -p 8080:8080 --name naima naima:latest
```

## Telegram

Set `TELEGRAM_BOT_TOKEN` to enable Telegram.

On first run, Naima prints a link code in the terminal. Send that code to the bot to bind your Telegram account to the running agent. When using Docker Compose, the Telegram session is persisted in the `naima_data` volume at `/data/.naima_session.json`.

Telegram features:
- normal text chat
- optional draft streaming via Telegram Bot API draft updates
- audio/voice input transcription through OpenAI `/audio/transcriptions`
- text reply plus generated voice reply through OpenAI `/audio/speech`
- `/new` or `/reset` clears the current memory

## REST API

Set `NAIMA_API_TOKEN` to enable the REST API. Optionally set `NAIMA_API_ADDR` to change the listen address. Default is `:8080`.

### Standard chat endpoint

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello"}'
```

To force a fresh conversation:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello again","new_conversation":true}'
```

### Streaming chat endpoint

```sh
curl -N -X POST "http://localhost:8080/api/messages/stream" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello"}'
```

SSE events:
- `start`
- `delta`
- `done`
- `error`
- `op`

### Memory endpoints

Reset memory:

```sh
curl -sS -X POST "http://localhost:8080/api/memory/reset" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Get current memory status:

```sh
curl -sS "http://localhost:8080/api/memory/status" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

### Tools endpoints

List tools:

```sh
curl -sS "http://localhost:8080/api/tools" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Enable or disable a tool:

```sh
curl -sS -X POST "http://localhost:8080/api/tools" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"web_search","enabled":false}'
```

### Personal Knowledge Base endpoints

Get PKB graph:

```sh
curl -sS "http://localhost:8080/api/pkb/graph" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Ingest a URL into an existing topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"topic_id":1,"url":"https://example.com/article"}'
```

Ingest a URL while creating a new topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"new_topic":"Golang","url":"https://go.dev/doc/"}'
```

Upload a file into an existing topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest/file" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -F "topic_id=1" \
  -F "file=@/absolute/path/to/file.pdf"
```

Upload a file while creating a new topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest/file" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -F "new_topic=Research" \
  -F "file=@/absolute/path/to/file.pdf"
```

Delete a topic:

```sh
curl -sS -X DELETE "http://localhost:8080/api/pkb/topics/1" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Delete a document:

```sh
curl -sS -X DELETE "http://localhost:8080/api/pkb/documents/12" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

## Web UI

Naima serves a built-in web UI directly from disk at:
- [https://YOUR_DOMAIN/](https://YOUR_DOMAIN/)
- or `http://localhost:8080/` when running without Caddy

Current web UI features:
- streaming chat
- live markdown rendering while streaming
- operations panel showing agent steps and tool activity
- theme selector with `Void`, `Void Light`, `Quantum`, `Quantum Light`
- optional UI Basic Auth
- collapsible `Settings`, `Tools`, `Memory`, and `Operations` panels
- personal knowledge base 3D view
- file and URL ingestion dialog
- topic/document delete actions
- topic-scoped document selection for chat

### UI Basic Auth

Optional env vars:
- `NAIMA_UI_BASIC_AUTH_USER`
- `NAIMA_UI_BASIC_AUTH_PASS`

`NAIMA_UI_BASIC_AUTH_PASS` must be the lowercase SHA256 hex digest of the password that the browser user types.

Generate it with:

```sh
./hash_ui_basic_auth_pass.sh "your-password"
```

## Tools

Naima injects tool guidance dynamically:
- base prompt from `prompt.txt`
- per-tool guidance from `internal/tools/<tool_name>.md`
- only enabled tools are injected into the model system prompt

### Registered tools

| Tool | Description |
| --- | --- |
| `time` | current local and UTC time |
| `weather` | weather + 7-day forecast for a location |
| `web_search` | generic search over local SearxNG |
| `news_digest` | curated news digest over SearxNG news results |
| `personal_knowledge_base` | CRUD over topics/documents plus ingestion and temporal search |
| `playwright` | browser automation and page extraction |
| `task_scheduler` | persistent alarms and scheduled agent tasks |
| `long_memory` | semantic recall and LLM summary of past conversation |
| `memory_dump` | debug dump of current in-memory conversation state |
| `telegram_send` | send a Telegram message to the linked account when Telegram is configured |

### Tool notes

#### `weather`
- input: `location`
- returns current conditions and 7-day forecast
- uses Open-Meteo

#### `web_search`
- generic query tool for web/news/images
- backed by local SearxNG

#### `news_digest`
- focused news-summary tool
- deduplicates and summarizes SearxNG news results
- useful for scheduled digests

#### `personal_knowledge_base`
Supports:
- topic CRUD
- document CRUD
- URL ingestion
- note ingestion
- temporal search over ingested material

URL ingestion modes:
- `hybrid` default
- `tika`
- `playwright`
- `fetch`

Hybrid mode combines:
- direct fetch
- Tika extraction
- Playwright extraction
- deterministic cleanup into normalized markdown/text

File ingestion from the web UI:
- stores uploaded files locally
- extracts text through Tika
- saves extracted content as a PKB document under the selected topic

#### `playwright`
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

#### `task_scheduler`
Supports:
- one-shot alarms
- one-shot agent tasks
- cron-style recurring tasks
- PostgreSQL persistence across restarts
- Telegram delivery when enabled

#### `long_memory`
- retrieves related prior conversation through embeddings
- summarizes the recalled messages with the LLM

#### `memory_dump`
- debugging tool
- returns current in-memory messages as JSON

## Personal Knowledge Base

The personal knowledge base stores:
- topics in `pkb_topics`
- documents in `pkb_documents`

Each document belongs to one topic.

### Ingestion

Supported sources:
- URL
- manual note
- uploaded file

Stored documents include `ingest_method`, for example:
- `hybrid_markdown`
- `tika_markdown`
- `playwright_markdown`
- `fallback_text`
- `direct_text`
- `manual_note`
- `tika_file_markdown`

### PKB UI

The web UI includes a 3D PKB view powered by Three.js.

Features:
- topics visualized as buildings
- documents visualized as related nodes
- topic/document inspection in the right panel
- clickable document source links
- topic delete and document delete actions
- document selection mode for scoped chat

### Scoped chat over selected documents

From a topic in the PKB UI:
1. click `Select documents for chat`
2. select one or more documents visually
3. click `Chat with selected document(s)`

Naima returns to the main chat UI and shows a scope banner.
While the scope is active:
- chat requests are built only on top of the selected documents
- clearing the scope resets memory immediately
- applying a scope also resets memory immediately

## Conversation Memory

Naima uses [Memorya](https://github.com/rehacktive/memorya) with PostgreSQL/pgvector storage.

Behavior:
- active memory starts empty on process restart
- embeddings are generated for incoming messages before persistence
- semantic recall is available through pgvector
- LLM summarization is used to compact memory when context reaches capacity
- compacted memory becomes: `pinned messages + one summary message`
- the web UI shows current memory status in the `Memory` panel

## Logging

Naima uses structured colored terminal logs through `logrus`.

Logs include steps such as:
- message received
- embeddings generated
- message saved
- tool execution
- reply completed

The web UI `Operations` panel mirrors the important high-level operations.

## Configuration

Important environment variables:

- `TELEGRAM_BOT_TOKEN`: enable Telegram integration
- `NAIMA_TELEGRAM_STREAM`: enable Telegram draft streaming, default `false`
- `NAIMA_TRANSCRIPTION_MODEL`: OpenAI transcription model, default `whisper-1`
- `NAIMA_TTS_MODEL`: OpenAI speech model, default `tts-1`
- `NAIMA_TTS_VOICE`: OpenAI voice, default `alloy`
- `NAIMA_TTS_FORMAT`: OpenAI speech format, default `mp3`
- `NAIMA_SESSION_FILE`: Telegram session file path
- `NAIMA_API_ADDR`: REST listen address, default `:8080`
- `NAIMA_API_TOKEN`: REST/UI bearer token
- `NAIMA_UI_BASIC_AUTH_USER`: optional UI Basic Auth username
- `NAIMA_UI_BASIC_AUTH_PASS`: optional UI Basic Auth password hash
- `OPENAI_API_KEY`: OpenAI-compatible API key
- `OPENAI_MODEL`: chat model
- `OPENAI_EMBEDDING_MODEL`: embedding model
- `OPENAI_BASE_URL`: optional OpenAI-compatible base URL
- `NAIMA_MEMORY_MAX_CONTEXT`: max in-memory context messages
- `NAIMA_MEMORY_SUMMARY_TIMEOUT_MS`: memory summarizer timeout
- `NAIMA_PGVECTOR_DSN`: PostgreSQL DSN
- `NAIMA_PGVECTOR_SEARCH_LIMIT`: recall search limit
- `NAIMA_PGVECTOR_EMBEDDING_DIMS`: embedding dimensions, use `0` to avoid ivfflat index creation
- `NAIMA_SEARX_URL`: local SearxNG URL
- `NAIMA_TIKA_URL`: Tika server URL
- `NAIMA_TIKA_ALLOW_FALLBACK`: fallback to plain extraction if Tika fails
- `NAIMA_TIKA_FILE_TIMEOUT_MS`: timeout for file extraction through Tika
- `NAIMA_PKB_INGEST_MODE`: `hybrid`, `tika`, `playwright`, or `fetch`
- `NAIMA_PKB_UPLOAD_DIR`: local storage directory for uploaded PKB files
- `NAIMA_PLAYWRIGHT_HEADLESS`: Playwright headless mode
- `NAIMA_PLAYWRIGHT_TIMEOUT_MS`: Playwright timeout
- `NAIMA_TASK_TIMEZONE`: timezone for scheduled tasks
- `NAIMA_UI_DIR`: UI directory, default `./internal/httpapi/ui`
- `NAIMA_TOOL_PROMPTS_DIR`: tool prompt directory, default `./internal/tools`

See [.env.example](/Users/aw4y/dev/naima/.env.example) for the full template.

## Command line

```sh
go run . -name "Naima"
```
