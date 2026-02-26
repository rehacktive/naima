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

Example request:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
	-H "Authorization: Bearer $NAIMA_API_TOKEN" \
	-H "Content-Type: application/json" \
	-d '{"message":"Hello"}'
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

Notes:

- Memorya active context starts empty on every process restart.
- In Telegram, send `/new` or `/reset` to clear the current Memorya context.
- On each new incoming message, Naima computes embeddings before storing it in Memorya.
- Tools available to the model: `time` and `web_search`.
- `docker/searxng/settings.yml` is mounted into the SearxNG container and
  enables `json` output so the `web_search` tool can parse results.
