	░▒▓███████▓▒░ ░▒▓██████▓▒░░▒▓█▓▒░▒▓██████████████▓▒░ ░▒▓██████▓▒░  
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ 
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ next
	░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░ artificial
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ intelligence
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ modular
	░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░░▒▓█▓▒░ agent      

Naima is a Go-based AI agent.

## Run

```sh
docker compose up -d pgvector redis searxng
cp .env.example .env
# Edit .env and set OPENAI_API_KEY, OPENAI_MODEL, and OPENAI_EMBEDDING_MODEL
# Set TELEGRAM_BOT_TOKEN to enable Telegram, or NAIMA_API_TOKEN to enable the REST API
# Optionally set OPENAI_BASE_URL for a local or OpenAI-compatible endpoint
go run .
```

On first run, the app prints a link code in the terminal. Send that code to the bot
in Telegram to bind the agent to your user ID. After that, only your user can chat
with the agent.

## REST API

Set `NAIMA_API_TOKEN` to enable the REST endpoint. Optionally set `NAIMA_API_ADDR`
to change the listen address (default `:8080`).

## Web UI

Naima serves a built-in chat UI at:

- [http://localhost:8080/](http://localhost:8080/)

Enter your API token in the page, then chat. Responses stream from
`/api/messages/stream`.
The UI file is served from disk (`internal/httpapi/ui/index.html`) so page
changes are picked up without restarting Naima.

Example request:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN" \
	-H "Content-Type: application/json" \
	-d '{"message":"Hello"}'
```

### REST Streaming

For token streaming, use the SSE endpoint:

```sh
curl -N -X POST "http://localhost:8080/api/messages/stream" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN" \
	-H "Content-Type: application/json" \
	-d '{"message":"Hello"}'
```

Stream events:
- `start`
- `delta` (token chunks)
- `done` (final response)
- `error`
- `op` (operation/status messages)

### Tools API

List current tool states (enabled/disabled):

```sh
curl -sS "http://localhost:8080/api/tools" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Enable/disable a tool:

```sh
curl -sS -X POST "http://localhost:8080/api/tools" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN" \
	-H "Content-Type: application/json" \
	-d '{"name":"web_search","enabled":false}'
```

To start a new conversation (clear Memorya context) with REST:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN" \
	-H "Content-Type: application/json" \
	-d '{"message":"Hello again","new_conversation":true}'
```

or:

```sh
curl -sS -X POST "http://localhost:8080/api/memory/reset" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN"
```

## Options

```sh
go run . -name "Naima"
```

## Conversation memory

This project uses [Memorya](https://github.com/rehacktive/memorya) to keep the
active conversation context in memory and persist messages to PostgreSQL with
pgvector.

Optional environment variables:

- `NAIMA_TELEGRAM_STREAM`: enable Telegram draft streaming via
  `sendMessageDraft` (`true`/`1`/`yes`/`on`). Default `false` (normal
  `sendMessage` only).
- `NAIMA_MEMORY_MAX_CONTEXT`: max number of active context messages kept in
  Memorya (default `20`).
- `NAIMA_PGVECTOR_DSN`: PostgreSQL DSN for pgvector storage
  (default `postgres://naima:naima@localhost:5432/naima?sslmode=disable`).
- `NAIMA_PGVECTOR_SEARCH_LIMIT`: max related messages fetched by vector search
  (default `5`).
- `NAIMA_PGVECTOR_EMBEDDING_DIMS`: embedding vector dimensions for pgvector
  indexing. Set to your model dimension (for example `1536`). Use `0` to skip
  ivfflat index creation (default `0`).
- `NAIMA_SEARX_URL`: local Searx base URL used by the `web_search` tool
  (default `http://localhost:8081`).
- `NAIMA_PLAYWRIGHT_HEADLESS`: run Playwright in headless mode (`true`/`false`,
  default `true`).
- `NAIMA_PLAYWRIGHT_TIMEOUT_MS`: Playwright navigation/action timeout in
  milliseconds (default `30000`).
- `NAIMA_UI_DIR`: directory containing `index.html` for the built-in web UI
  (default `./internal/httpapi/ui`).

Notes:

- Memorya active context starts empty on every process restart.
- In Telegram, send `/new` or `/reset` to clear the current Memorya context.
- Telegram draft streaming is optional and disabled by default.
- On each new incoming message, Naima computes embeddings before storing it in Memorya.
- Tools available to the model: `time`, `web_search`, `playwright`, `long_memory`.
- `web_search` supports optional `categories`, `engines`, and `time_range`
  (`day|month|year`) in addition to `query`.
- `playwright` supports browser automation/scraping operations:
  `scrape`, `click`, `type`, `press`, `evaluate`, `screenshot`.
- `long_memory` uses `something` as input and returns a summary of relevant
  previous messages from the memory database.
- `docker/searxng/settings.yml` is mounted into the SearxNG container and
  enables `json` output so the `web_search` tool can parse results.
