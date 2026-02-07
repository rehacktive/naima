# Naima

Naima is a minimal Go-based AI agent starter.

## Run

```sh
cp .env.example .env
# Edit .env and set TELEGRAM_BOT_TOKEN
go run .
```

On first run, the app prints a link code in the terminal. Send that code to the bot
in Telegram to bind the agent to your user ID. After that, only your user can chat
with the agent.

## Options

```sh
go run . -name "Naima"
```
