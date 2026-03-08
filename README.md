<img src="internal/httpapi/ui/logo2.png" alt="Naima Logo" width="320">

Naima is a Go-based AI agent.

## Run

```sh
cp .env.example .env
# Edit .env and set OPENAI_API_KEY, OPENAI_MODEL, and OPENAI_EMBEDDING_MODEL
# Set TELEGRAM_BOT_TOKEN to enable Telegram, or NAIMA_API_TOKEN to enable the REST API
# Optionally set OPENAI_BASE_URL for a local or OpenAI-compatible endpoint
docker compose up -d --build
```

### Run only Naima container

If `pgvector` and `searxng` are already running elsewhere, you can build/run just
the Naima image:

```sh
docker build -t naima:latest .
docker run --rm -it --env-file .env -p 8080:8080 --name naima naima:latest
```

On first run, the app prints a link code in the terminal. Send that code to the bot
in Telegram to bind the agent to your user ID. After that, only your user can chat
with the agent.
When using Docker Compose, this link session is persisted in the `naima_data`
volume (`/data/.naima_session.json`) so you do not need to relink on each restart.

Telegram audio messages are supported:
- voice/audio messages are transcribed via OpenAI `/audio/transcriptions`
- transcription is processed as a normal agent message
- reply is sent as text plus generated speech via OpenAI `/audio/speech`

## REST API

Set `NAIMA_API_TOKEN` to enable the REST endpoint. Optionally set `NAIMA_API_ADDR`
to change the listen address (default `:8080`).

## Web UI

Naima serves a built-in chat UI at:

- [http://localhost:8080/](http://localhost:8080/)

Optional Basic Auth for UI:
- `NAIMA_UI_BASIC_AUTH_USER`
- `NAIMA_UI_BASIC_AUTH_PASS`

When both are set, the browser asks for username/password before loading the UI.

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

## Tools

Naima can call tools during model responses. You can enable/disable tools at
runtime via `/api/tools` or from the web UI.

Tool prompt guidance is dynamic:
- Base prompt is loaded from `prompt.txt`.
- Per-tool guidance is loaded from `internal/tools/<tool_name>.md`.
- Only enabled tools have their guidance injected into the model system prompt.

### Available tools

| Tool | What it does | Typical use |
| --- | --- | --- |
| `time` | Returns current local/UTC timestamp | "What time is it?" |
| `weather` | Returns current weather and 7-day forecast for a location | "Weather in Milan today and this week" |
| `web_search` | Searches web/news/images via local SearxNG | Fresh facts, current events, citations |
| `news_digest` | Builds concise topic-based news digest from SearxNG news results | "Give me a digest on AI regulation this week" |
| `personal_knowledge_base` | CRUD for personal topics and associated documents/notes | "Create topic Golang", "add this URL to topic 3" |
| `playwright` | Automates a browser session and extracts page data | Navigate pages, click/type/press, scrape content |
| `telegram_send` | Sends a text message to your linked Telegram account | "Do X and send the result to Telegram" |
| `task_scheduler` | Creates persistent scheduled tasks (one-time/cron) | "Set an alarm in 5 minutes", "Send me news every day at 10" |
| `long_memory` | Recalls relevant past conversations and summarizes them | "What did we decide about X?" |

### `time`

- No parameters required.
- Returns JSON with local and UTC timestamps.

### `weather`

Required:
- `location` (`string`) location to lookup (example: `Rome, Italy`)

Behavior:
- Uses Open-Meteo geocoding + forecast APIs
- Returns:
  - current conditions (temperature, wind, weather code/description)
  - 7-day forecast (min/max and condition per day)

### `web_search`

Required:
- `query` (`string`)

Optional:
- `categories` (`[]string`) examples: `["web"]`, `["news"]`, `["images"]`
- `engines` (`[]string`) examples: `["duckduckgo"]`
- `time_range` (`string`) one of `day|month|year`
- `language` (`string`) example: `en-US`
- `limit` (`int`) max results to return

### `news_digest`

Required:
- `topic` (`string`)

Optional:
- `region` (`string`) regional focus
- `time_range` (`string`) one of `day|week|month|year`
- `language` (`string`) Searx language code
- `max_items` (`int`) number of headlines in digest (1-15)

Behavior:
- Queries SearxNG news results
- Deduplicates/ranks headlines
- Returns:
  - `digest` (compact textual summary)
  - `items` (structured list with title/url/source/snippet)

### `personal_knowledge_base`

Operations:
- Topics:
  - `create_topic` (`topic`)
  - `list_topics`
  - `update_topic` (`topic_id`, `topic`)
  - `delete_topic` (`topic_id`)
- Documents:
  - `add_content` (`topic_id` + `url` and/or `note`, optional `title`/`content`)
  - `list_documents` (`topic_id`)
  - `update_document` (`document_id`, `content`, optional `title`/`url`)
  - `delete_document` (`document_id`)

Behavior:
- Stores data in PostgreSQL (`pkb_topics`, `pkb_documents`)
- Each document belongs to one topic
- `add_content` supports:
  - URL ingestion (fetches webpage text and stores it)
  - manual notes (stores provided note/content)

### `playwright`

Browser automation tool backed by `playwright-go`.

Required:
- `operation` (`string`)

Supported operations:
- `goto|navigate`: open URL and return scrape output
- `scrape`: return current page text/title
- `click`: click selector, then scrape
- `type`: fill selector with text, then scrape
- `press`: key press on selector (defaults to `Enter` if no value), then scrape
- `evaluate`: run JavaScript and return result
- `screenshot`: return base64 PNG
- `snapshot_for_ai`: best-effort call to hidden runtime helper if available
- `close|reset`: close Playwright session

Parameters:
- `url` (`string`): required for first call and for `goto|navigate`
- `selector` (`string`): required for `click|type|press`
- `value` (`string`): text for `type`, key for `press`
- `script` (`string`): required for `evaluate`
- `wait_ms` (`int`): optional post-action wait
- `full_page` (`bool`): optional for `screenshot`

Recommended flow:
1. `goto` with `url`
2. Run one or more actions (`click`, `type`, `press`)
3. Use `scrape`/`evaluate`/`screenshot` as needed
4. `close` when done

### `long_memory`

Required:
- `something` (`string`) short topic to recall

Behavior:
- Finds related past messages via embeddings search
- Produces a summary (LLM-based, with fallback)

### `telegram_send`

Required:
- `message` (`string`) text to send

Behavior:
- Sends message to the user linked via Telegram session (`.naima_session.json` or `NAIMA_SESSION_FILE`)
- Tool is available only when `TELEGRAM_BOT_TOKEN` is configured

### `task_scheduler`

Operations:
- `create`: create a task
- `list`: list tasks
- `cancel`: disable task by id

Create parameters:
- `kind`: `alarm` or `agent`
  - `alarm`: sends fixed `content` when triggered
  - `agent`: runs `content` as a prompt through Naima at trigger time
- `title`: short label
- `content` (or `prompt`/`message`): task payload
- one-time schedule:
  - `in`: relative duration (`5m`, `2h`)
  - or `run_at`: RFC3339 timestamp
- recurring schedule:
  - `cron`: 5-field cron expression (`minute hour day month weekday`)
- `send_telegram`: default `true`

Examples:
- Alarm in 5 minutes:
```json
{"operation":"create","kind":"alarm","title":"Alarm","content":"Time is up","in":"5m","send_telegram":true}
```
- Daily news at 10:00:
```json
{"operation":"create","kind":"agent","title":"Daily news","content":"Get latest world news summary","cron":"0 10 * * *","send_telegram":true}
```
- List tasks:
```json
{"operation":"list"}
```
- Cancel task:
```json
{"operation":"cancel","id":12}
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

Get current Memorya runtime status:

```sh
curl -sS "http://localhost:8080/api/memory/status" \
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
- `NAIMA_TRANSCRIPTION_MODEL`: OpenAI transcription model for Telegram audio
  inputs (default `whisper-1`).
- `NAIMA_TTS_MODEL`: OpenAI speech model for Telegram voice replies (default
  `tts-1`; allowed: `tts-1`, `tts-1-hd`, `canary-tts`).
- `NAIMA_TTS_VOICE`: OpenAI TTS voice (default `alloy`; allowed: `alloy`,
  `echo`, `fable`, `onyx`, `nova`, `shimmer`).
- `NAIMA_TTS_FORMAT`: OpenAI speech response format (default `mp3`; allowed:
  `mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`).
- `NAIMA_MEMORY_MAX_CONTEXT`: max number of active context messages kept in
  Memorya (default `20`).
- `NAIMA_MEMORY_SUMMARY_TIMEOUT_MS`: timeout for LLM-based Memorya summarizer
  calls in milliseconds (default `8000`).
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
- `NAIMA_TASK_TIMEZONE`: timezone used for cron interpretation (default `UTC`).
- `NAIMA_UI_DIR`: directory containing `index.html` for the built-in web UI
  (default `./internal/httpapi/ui`).
- `NAIMA_TOOL_PROMPTS_DIR`: directory containing per-tool prompt files
  (`<tool_name>.md`) used for dynamic tool guidance injection
  (default `./internal/tools`).

Notes:

- Memorya active context starts empty on every process restart.
- In Telegram, send `/new` or `/reset` to clear the current Memorya context.
- Telegram draft streaming is optional and disabled by default.
- On each new incoming message, Naima computes embeddings before storing it in Memorya.
- Memorya uses an LLM summarizer to compress older context when the in-memory
  context exceeds `NAIMA_MEMORY_MAX_CONTEXT`.
- Compaction policy: when active context reaches max size, Naima compacts
  memory to `pinned messages + one summary message` (so size becomes `1` if
  there are no pinned messages).
- The web UI includes a collapsible Memory panel (between Tools and Operations)
  showing Memorya `GetStatus()` fields.
- Scheduled tasks are persisted in PostgreSQL (`scheduled_tasks`) and restored
  automatically on restart.
- `docker/searxng/settings.yml` is mounted into the SearxNG container and
  enables `json` output so the `web_search` tool can parse results.
