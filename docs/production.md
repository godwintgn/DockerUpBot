# Production deployment

Run one DockerUpBot instance per Telegram bot token and state directory on a trusted Docker host. Use Python 3.8+, Docker Engine 25 or newer, and the Compose plugin. DockerUpBot can change containers through the host Docker socket. Keep the deployment, bot token, webhook secret, and managed Compose files under administrator control.

## Install

1. Use a tagged release checkout. Set `DOCKERUPBOT_IMAGE` in `.env` to the `ghcr.io/godwintgn/dockerupbot@sha256:…` reference in that release’s notes.
2. Create the state directories and restrict access to the environment file.

   ```bash
   umask 077
   cp .env.example .env
   mkdir -p data/bot data/diun
   ```

3. Set `TELEGRAM_TOKEN`, `ALLOWED_USERS`, and `COMPOSE_ROOT`. Use an existing absolute directory that contains the managed Compose files. Generate the shared `WEBHOOK_SECRET` with `openssl rand -hex 32`. Start the bot in Telegram before installing. Operators can use their private chats or the one group configured in `TELEGRAM_CHAT_ID`.
4. Label each service you want Diun to watch and reapply its Compose configuration. The initial Diun inventory is quiet.

   ```yaml
   labels:
     diun.enable: "true"
   ```

5. Start the stack and wait for readiness.

   ```bash
   ./deploy.sh --mode production start
   ./deploy.sh ready
   ```

The script selects production automatically from the pinned digest. Explicit `--mode production` also checks that the image reference is pinned. In a terminal, `start` and `update` prompt for missing or invalid values and verify the bot token with Telegram. Enter `:q` or press **Ctrl+C** to cancel setup. The script saves changes and starts Docker after you confirm. Cancelling after Docker begins does not roll back changes Docker has already applied.

For automation, supply a complete `.env` and use `./deploy.sh --mode production --non-interactive start`. Invalid exported variables must be corrected or unset in the invoking shell because Compose gives exports precedence over `.env`.

The production configuration keeps the bot filesystem read-only outside its state mount and temporary filesystem, publishes no host ports, and starts Diun after the bot and socket proxy are healthy. Keep private-registry Docker configuration in a separately mounted, read-only directory with `DOCKER_CONFIG` set to its path inside the bot. The built-in pull client reads credentials stored in `config.json` for the exact registry host and port. Credentials held only by a Docker credential helper require a separate setup.

## Operate and recover

- `dubot health` checks the HTTP process. `dubot ready` also checks the Docker API and a successful Telegram poll in the past 90 seconds. If readiness fails, run `./deploy.sh status` and inspect `./deploy.sh logs` and verify only one process polls the bot token.
- Check Telegram `/updates` for persisted jobs and `/history` for interrupted rollbacks. An interrupted update waits for operator input on restart. If DockerUpBot was unavailable during a Diun scan, inspect the Diun logs and use the [manual digest webhook](debug.md#fallback-webhook-only-dubot-diun) to queue any missed update.
- Application rollback restores an image. Use the managed application’s backup and recovery procedure for database and volume changes. Retained `dockerupbot-rollback` tags consume disk space and can be removed individually once their rollback is no longer needed.

## Back up and upgrade

Run `./deploy.sh stop` before backing up `.env`, `data/bot`, and `data/diun`. Store the backup privately because it may contain secrets and service logs. Save the current DockerUpBot image digest alongside it.

To upgrade, check out the target release, update `DOCKERUPBOT_IMAGE` to its published digest, and run `./deploy.sh --mode production update`. To switch between development and production, change the configured image and use `start` or `update`. The project and state directories stay the same. To revert the bot, restore the previous release configuration and image digest. Restore the matching state backup while the bot and Diun are stopped if the state format has changed.
