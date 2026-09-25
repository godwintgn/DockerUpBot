# DockerUpBot debug commands (`dubot`) and Diun

Run these from the host, in the compose directory.

## DockerUpBot (`dubot`)

```bash
docker compose exec dockerupbot dubot <command>
```

| Command | What it does |
|---------|----------------|
| `dubot health` | Process health JSON (version + bot username) |
| `dubot telegram` | Live Telegram `getMe` check (no chat message) |
| `dubot ping` | Send a test Telegram message to notify targets |
| `dubot diun --image … --container …` | Manually fire a Diun-style webhook (bot only) |
| `dubot help` | Show help |

## Everyday testing

```bash
# watch logs
docker compose logs -f dockerupbot
docker compose logs -f diun

# 1) bot API token OK?
docker compose exec dockerupbot dubot telegram

# 2) can we message you?
docker compose exec dockerupbot dubot ping
# Open Telegram, find the bot, press Start (/start), then ping again.
```

### Prefer: trigger real Diun

Diun and DockerUpBot share the `du-tunnel` network. Diun has **no** Telegram notifier — only DockerUpBot messages you. Diun webhooks the bot **only** when an image is `new` or `update`.

Current Diun (v4.33+) has **no** `diun run` command. To force a scan, restart Diun (it runs a check on startup):

```bash
docker compose restart diun
docker compose logs -f diun
```

Test notifiers (Diun → webhook only). DockerUpBot **ignores** Diun `notif test` payloads (`diun/testnotif` / no `ctn_names`) so they no longer enter the UPDATE queue:

```bash
docker compose exec diun diun notif test
# Expect bot log: webhook ignored: test payload or missing ctn_names
```

To exercise a real UPDATE flow, use a real container:

```bash
docker compose exec dockerupbot dubot diun \
  --image YOUR_IMAGE:tag \
  --container YOUR_CONTAINER_NAME
```

Inspect what Diun already knows:

```bash
docker compose exec diun diun image list
```

If a restart produces no Telegram message, there is no update (Diun already knows current digests). That is expected. To force first-seen alerts once for testing: temporarily set `DIUN_WATCH_FIRSTCHECKNOTIF=true` in Compose, clear `./data/diun`, recreate, then set it back to `false`.

### Fallback: webhook-only (`dubot diun`)

Tests DockerUpBot without waiting for Diun. Requires a real container:

```bash
docker ps --format '{{.Names}}\t{{.Image}}'

docker compose exec dockerupbot dubot diun \
  --image YOUR_IMAGE:tag \
  --container YOUR_CONTAINER_NAME
```

**Important:** `ALLOWED_USERS` must be *your* numeric Telegram user ID, not the bot id from `dubot telegram`.

After a successful webhook you should see logs: `webhook received` → `telegram notify sent`, and an UPDATE/SKIP message in Telegram.

## Startup check

```bash
docker compose up -d
docker compose logs -f
```

Good start includes:

```text
msg="telegram connected" username=@YourBot
msg="telegram polling started"
```

and Diun serving / watching on schedule.

## Inspect state

```bash
docker compose exec dockerupbot ls -la /data
docker compose exec dockerupbot cat /data/config.yml
docker compose exec diun ls -la /data
docker compose ps
```

## Empty-scan heartbeat (optional)

To confirm Diun’s cron is alive even when nothing changed, set in `.env`:

```env
NOTIFY_EMPTY_SCAN=true
# DIUN_CONTAINER=diun
```

DockerUpBot watches the Diun container logs for `Jobs completed … updated=0` and sends a short Telegram ping (no UPDATE buttons). Leave it `false` (default) for update-only alerts.

## Notification and queue behavior

A Diun burst is held about 8 seconds, then **one** Telegram card is sent. The previous card is deleted (not left as “superseded”). The idle card only has **UPDATE ALL**, **SKIP ALL**, and **SELECT**.

If one image fails, use **SKIP & CONTINUE** to drop that image and run the rest. **RETRY** runs the failed image again. **STOP** leaves the unfinished images pending, so `/updates` shows them again with UPDATE ALL / SKIP ALL. A missing container name is skipped instead of pausing the queue. Health waits until `health_timeout_seconds`; a brief `unhealthy` during startup does not fail the update. `du-tunnel` is not part of the health check.

## New ops features (slice A–D)

| Feature | How |
|---------|-----|
| Rollback / Keep | After a successful queue, or `/history` |
| Startup recovery | Telegram CONTINUE/STOP if a job was interrupted |
| Skip policy | `SKIP_IMAGES` / `SKIP_CONTAINERS` in `.env` |
| Quiet hours | `QUIET_HOURS=23:00-07:00` — digest when the window ends |
| Stricter health | `REQUIRE_HEALTHY=true` |
| Prune dangling | `PRUNE_AFTER_SUCCESS=true` |
| GHCR auth | Uncomment `~/.docker/config.json` mount in Compose |

## Restart

```bash
docker compose pull
docker compose up -d --force-recreate
```
