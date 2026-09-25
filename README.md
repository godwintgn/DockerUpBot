<div align="center">

<img src="logo.png" alt="DockerUpBot" width="160">

# DockerUpBot

**Diun finds the update. You approve it in Telegram. One service restarts.**

DockerUpBot is a self-hosted Telegram bot for Docker Compose image updates. [Diun](https://github.com/crazy-max/diun) watches your containers and calls the bot only when a digest actually changes. You update, skip, or roll back from the chat. Nothing is published to the host.

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![Image](https://img.shields.io/badge/ghcr.io-godwintgn%2Fdockerupbot-2496ED)](https://github.com/godwintgn/dockerupbot/pkgs/container/dockerupbot)

</div>

## Image

Public image: `ghcr.io/godwintgn/dockerupbot`

```bash
docker pull ghcr.io/godwintgn/dockerupbot:latest
```

| | |
| --- | --- |
| Registry | GitHub Container Registry (public) |
| Tags | `latest` on `main`, plus `X.Y.Z`, `X.Y`, and `X` on a release tag |
| Systems | Linux **amd64** (PCs and most servers) and Linux **arm64** (64-bit Raspberry Pi, ARM cloud VMs) |
| Not included | 32-bit ARM, 32-bit x86, RISC-V, and other Linux CPU types |
| Listens on | `:9467` inside the container. The Compose file does not publish that port. |
| Needs | Docker socket, your Compose project directory, and a Telegram bot token |
| License | GPL-3.0 |

The same page you are reading is what GitHub shows on the package. Diun in the Compose file is the separate public image `crazymax/diun:latest`.

## Features

- **Update cards in Telegram** — one live message with **UPDATE ALL**, **SKIP ALL**, and **SELECT**
- **Confirm before anything pulls** — a start screen, then one image at a time
- **Real progress** — bytes, speed, and layers while the image pulls
- **Compose-aware** — recreates only the service that uses the new image
- **Health wait** — polls until the container is healthy, or until the timeout
- **Failure controls** — **RETRY**, **SKIP & CONTINUE**, **LOGS**, or **STOP**
- **Stopped jobs stay pending** — `/updates` lists them so you can run or skip later
- **Rollback** — tag the previous digest and bring the service back
- **Quiet hours and skip lists** — hold notifications overnight, ignore noisy images
- **Optional empty-scan ping** — a message when Diun’s cron finds zero updates
- **Startup recovery** — if the bot restarts mid-update, it tells you and waits
- **Allow-list** — only your Telegram user IDs can press the buttons
- **No host ports** — Diun and the bot talk on a private Compose network

Commands: `/updates` · `/status` · `/history` · `/help`

## Deploy

You need Docker with Compose, a Telegram bot from [@BotFather](https://t.me/BotFather), and the host path where your Compose projects live.

```bash
git clone https://github.com/godwintgn/dockerupbot.git
cd dockerupbot
cp .env.example .env
mkdir -p data/bot data/diun
```

Fill in `.env`:

| Variable | Required | What to put |
| --- | --- | --- |
| `TELEGRAM_TOKEN` | yes | BotFather token |
| `ALLOWED_USERS` | yes | Your numeric Telegram user ID. Comma-separate more than one. |
| `WEBHOOK_SECRET` | yes | Long random string. Diun sends it as the webhook `Authorization` header. |
| `COMPOSE_ROOT` | yes | Host directory that contains the stacks you want to update |
| `TELEGRAM_CHAT_ID` | no | Chat that receives alerts. If empty, each allowed user is notified. |
| `TZ` | no | Timezone for Diun’s schedule. Compose falls back to `Asia/Muscat`. |
| `NOTIFY_EMPTY_SCAN` | no | `true` to ping when a scan finds nothing new |
| `SKIP_IMAGES` | no | Substrings or `/regex/` to ignore |
| `SKIP_CONTAINERS` | no | Container names to ignore |
| `QUIET_HOURS` | no | `HH:MM-HH:MM`, overnight ranges work (`23:00-07:00`) |
| `REQUIRE_HEALTHY` | no | `true` to wait for a Docker `HEALTHCHECK` of `healthy` |
| `PRUNE_AFTER_SUCCESS` | no | `true` to prune dangling images after a clean queue |

Open the bot in Telegram and press **Start** once, so it is allowed to message you. Then:

```bash
docker compose up -d
```

Diun checks every 6 hours and on startup. It webhooks `http://dockerupbot:9467/webhook/diun` only for a real `new` or `update`. The first inventory stays quiet.

Force a scan:

```bash
docker compose restart diun
```

Compose pulls `ghcr.io/godwintgn/dockerupbot:latest` and `crazymax/diun:latest`.

### `data/bot/config.yml`

On first start the bot writes a short `./data/bot/config.yml` (`/data/config.yml` inside the container) and does not overwrite it later. Replace that file with the full config below and restart the bot. Keep tokens and `WEBHOOK_SECRET` in `.env`. A value set in `.env` overrides the same key here.

```yaml
# Secrets come from .env — do not put tokens here.
# Listen address is set by the image (HTTP_ADDR=:9467).

# Pending updates, queue, and history.
# Host file: ./data/bot/dockerupbot.json
data_path: /data/dockerupbot.json

# How often Telegram progress messages refresh while an image pulls.
refresh_seconds: 2

# Seconds to wait after a recreate before the update is treated as unhealthy.
health_timeout_seconds: 120

# Telegram message when Diun’s cron finishes with zero updates.
notify_empty_scan: true

# Container whose logs are read for that empty-scan message.
diun_container: diun

# true = wait for Docker HEALTHCHECK "healthy".
# false = a running container is enough.
require_healthy: false

# true = prune dangling images after a queue finishes cleanly.
prune_after_success: false

# Hold update cards during this window. Empty = off.
# Overnight ranges work, for example 23:00-07:00.
quiet_hours: ""

# Images to ignore. Comma-separated substrings, or /regex/.
# Example: diun/testnotif,crazymax/diun
skip_images: ""

# Containers to ignore. Comma-separated names, or /regex/.
# Example: diun,dockerupbot
skip_containers: ""
```

### Socket access

The bot mounts `/var/run/docker.sock` read-write so it can pull and recreate services. Diun mounts the same socket read-only. Treat the bot token, webhook secret, and allow-list like root credentials, and do not publish port `9467`.

## License

GPL-3.0. See [LICENSE](LICENSE).
