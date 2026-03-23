# Naima Quickstart

This guide shows the fastest way to configure, run, and use Naima with Docker Compose.

## 1) Prerequisites

- Docker + Docker Compose plugin
- An LLM endpoint compatible with the OpenAI Chat/Embeddings APIs
- Optional: Telegram bot token (if you want Telegram integration)

## 2) Create your `.env`

```bash
cp .env.example .env
```

Set these required variables first:

```env
# LLM
OPENAI_API_KEY=your_key_here
OPENAI_MODEL=gpt-4o-mini
OPENAI_EMBEDDING_MODEL=text-embedding-3-small
OPENAI_BASE_URL=

# Naima API/UI access
NAIMA_API_TOKEN=choose_a_long_random_token
DOMAIN=localhost
```

Optional but recommended for Web UI Basic Auth:

```env
NAIMA_UI_BASIC_AUTH_USER=admin
# This must be SHA256 hex (not clear text)
NAIMA_UI_BASIC_AUTH_PASS=<sha256_hex>
```

Generate `NAIMA_UI_BASIC_AUTH_PASS` with:

```bash
./hash_ui_basic_auth_pass.sh "your_password"
```

## 3) LLM provider setup

Naima uses OpenAI-compatible APIs. Choose one of these patterns.

### A) OpenAI cloud

```env
OPENAI_API_KEY=sk-...
OPENAI_BASE_URL=
OPENAI_MODEL=gpt-4o-mini
OPENAI_EMBEDDING_MODEL=text-embedding-3-small
```

### B) Other OpenAI-compatible cloud provider

```env
OPENAI_API_KEY=provider_key
OPENAI_BASE_URL=https://your-provider.example.com/v1
OPENAI_MODEL=your-chat-model
OPENAI_EMBEDDING_MODEL=your-embedding-model
```

### C) Local provider (LM Studio/Ollama OpenAI-compatible endpoint)

When Naima runs in Docker, `OPENAI_BASE_URL` must be reachable **from the Naima container**.

- Docker Desktop (Mac/Windows):

```env
OPENAI_BASE_URL=http://host.docker.internal:1234/v1
```

- Linux (typical bridge gateway):

```env
OPENAI_BASE_URL=http://172.17.0.1:1234/v1
```

Also set:

```env
OPENAI_MODEL=your_local_chat_model
OPENAI_EMBEDDING_MODEL=your_local_embedding_model
OPENAI_API_KEY=dummy_or_local_key
```

## 4) Start Naima with Docker Compose

```bash
docker compose up -d --build
```

This starts Naima plus dependencies (pgvector, searxng, redis, tika, bash-tool, caddy).

Check status:

```bash
docker compose ps
```

## 5) Access Naima

- Web UI: `https://<DOMAIN>` (for local test: `https://localhost`)
- API base: `https://<DOMAIN>/api`

If using self-signed/local TLS, your browser may ask to accept the certificate.

## 6) First API test

### Standard response

```bash
curl -sS -X POST "https://localhost/api/messages" \
  -H "Authorization: Bearer ${NAIMA_API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"message":"hello Naima"}'
```

### Streaming response

```bash
curl -N -X POST "https://localhost/api/messages/stream" \
  -H "Authorization: Bearer ${NAIMA_API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"message":"give me a quick summary of your capabilities"}'
```

## 7) Optional Telegram setup

Set in `.env`:

```env
TELEGRAM_BOT_TOKEN=your_bot_token
NAIMA_SESSION_FILE=/data/.naima_session.json
NAIMA_TELEGRAM_STREAM=false
```

Then restart:

```bash
docker compose up -d --build
```

## 8) Useful commands

```bash
# Follow logs
docker compose logs -f naima

# Stop everything
docker compose down

# Stop and remove volumes (fresh reset)
docker compose down -v
```

## 9) Next docs

- API reference: `docs/api.md`
- Tools reference: `docs/tools.md`
