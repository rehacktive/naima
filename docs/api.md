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

## Personal Knowledge Base

Get PKB graph:

```sh
curl -sS "http://localhost:8080/api/pkb/graph" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN"
```

Ingest URL into existing topic:

```sh
curl -sS -X POST "http://localhost:8080/api/pkb/ingest" \
  -H "Authorization: Bearer $NAIMA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"topic_id":1,"url":"https://example.com/article"}'
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
