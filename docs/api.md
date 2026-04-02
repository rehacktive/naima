# API Reference

## REST API

Set `NAIMA_API_TOKEN` to enable the REST API. Default listen address is `:8080` unless `NAIMA_API_ADDR` is set.

### Chat

Standard request:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello"}'
```

New conversation:

```sh
curl -sS -X POST "http://localhost:8080/api/messages" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello again","new_conversation":true}'
```

Streaming request:

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

## Memory

Reset memory:

```sh
curl -sS -X POST "http://localhost:8080/api/memory/reset" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Get memory status:

```sh
curl -sS "http://localhost:8080/api/memory/status" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

## Tools

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

Note:
- Persona storage is currently accessed through the `persona` tool, not through dedicated REST endpoints

## Deep Research

Submit a new research run:

```sh
curl -sS -X POST "http://localhost:8080/api/research" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "topic":"AI coding agents",
    "note":"Research current AI coding agents, their capabilities, pricing, and tradeoffs. Focus on practical developer usage.",
    "guide_title":"AI coding agents brief",
    "language":"en",
    "time_range":"month",
    "max_sources":6,
    "max_queries":5,
    "notify_telegram":true
  }'
```

List recent research runs:

```sh
curl -sS "http://localhost:8080/api/research?limit=20" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Get one research run with status and logs:

```sh
curl -sS "http://localhost:8080/api/research/1" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Notes:
- research runs can be started either through the `deep_research` tool or `POST /api/research`
- these endpoints are for later inspection after the page is closed
- run logs are persisted in the database and returned by `GET /api/research/:id`

## Personal Knowledge Base

Get PKB graph:

```sh
curl -sS "http://localhost:8080/api/pkb/graph" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

List extracted PKB tags (for Tag navigator):

```sh
curl -sS "http://localhost:8080/api/pkb/tags" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Get documents associated with one tag:

```sh
curl -sS "http://localhost:8080/api/pkb/tags/1/documents" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Ingest URL into existing topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"topic_id":1,"url":"https://example.com/article"}'
```

Ingest URL and notify via Telegram when done:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"topic_id":1,"url":"https://example.com/article","notify_telegram":true}'
```

Ingest URL into new topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"new_topic":"Golang","url":"https://go.dev/doc/"}'
```

Upload file into existing topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest/file" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -F "topic_id=1" \
  -F "file=@/absolute/path/to/file.pdf"
```

Upload file into new topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest/file" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -F "new_topic=Research" \
  -F "file=@/absolute/path/to/file.pdf"
```

Delete topic:

```sh
curl -sS -X DELETE "http://localhost:8080/api/pkb/topics/1" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Delete document:

```sh
curl -sS -X DELETE "http://localhost:8080/api/pkb/documents/12" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Tag behavior:
- tags are extracted automatically whenever a URL/file/note is ingested or updated
- extraction is delegated to the configured chat model
- tags are persisted with text+category and linked to documents in PostgreSQL
- on startup, missing tags/embeddings are backfilled only for documents that do not already have them

## Maintenance

Rebuild mismatched PKB and memory embeddings using the current embedding model:

```sh
./scripts/rebuild_mismatched_pkb_embeddings.sh
./scripts/rebuild_mismatched_pkb_embeddings.sh --apply
./scripts/rebuild_mismatched_pkb_embeddings.sh --apply --restart
```

## Persona

Current behavior:
- explicit persona facts are stored through the `persona` tool
- recent conversation can also be analyzed in background to infer persona facts
- stored facts can be reused by other tools, such as defaulting email recipients or remembering user interests
- when Persona storage is empty, `GET /api/persona/bootstrap` reports that the UI should ask for the user's name
- `POST /api/persona/name` stores the onboarding name explicitly

## Web UI

The built-in UI is available at:
- `http://localhost:8080/`
- `https://YOUR_DOMAIN/` when using Caddy

Optional Basic Auth env vars:
- `NAIMA_UI_BASIC_AUTH_USER`
- `NAIMA_UI_BASIC_AUTH_PASS`

Generate the password hash with:

```sh
./hash_ui_basic_auth_pass.sh "your-password"
```
