#!/bin/sh
set -eu
trap 'exit 130' INT

usage() {
    cat <<'USAGE'
Usage: ./deploy.sh [--mode auto|dev|production] [--non-interactive] COMMAND

  start          Start or apply the selected configuration and wait for readiness
  stop           Stop the stack and retain its containers and data
  restart        Restart the existing containers with their current configuration
  update         Refresh images, apply the selected configuration, and wait
  status         Show service and health status
  ready          Check Docker and Telegram readiness from inside the bot
  logs [SERVICE] Follow stack or service logs
  check          Validate the selected Compose files without printing secrets

Automatic mode uses production when DOCKERUPBOT_IMAGE is pinned by digest.
Start and update prompt for missing or invalid settings in a terminal.
Enter :q or press Ctrl+C to cancel before applying the deployment.
USAGE
}

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

mode=auto
prompt=yes
while [ "$#" -gt 0 ]; do
    case "$1" in
        --mode)
            case "${2:-}" in
                auto|dev|production) mode=$2 ;;
                *) fail 'Choose --mode auto, dev, or production.' ;;
            esac
            shift 2
            ;;
        --non-interactive) prompt=no; shift ;;
        *) break ;;
    esac
done

command=${1:-help}
case "$command" in
    help|-h|--help) usage; exit 0 ;;
    logs)
        [ "$#" -le 2 ] || fail 'Usage: ./deploy.sh logs [SERVICE]'
        ;;
    start|stop|restart|update|status|ready|check)
        [ "$#" -eq 1 ] || fail "Unexpected arguments for $command."
        ;;
    *) fail "Unknown command $command. Run ./deploy.sh --help." ;;
esac
shift

CDPATH='' cd -P "$(dirname "$0")"
command -v python3 >/dev/null 2>&1 || fail 'Install Python 3 to run the deployment script.'
selection=$(python3 scripts/deploy_setup.py "$mode" "$command" "$prompt")
mode=${selection%% *}
image=${selection#* }
printf 'Selected Compose mode is %s.\n' "$mode"

compose() {
    if [ "$mode" = production ]; then
        docker compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml "$@"
    else
        docker compose --env-file .env -f docker-compose.yml "$@"
    fi
}

case "$command" in
    start|update)
        umask 077
        mkdir -p data/bot data/diun
        if [ "$image" = dockerupbot:local ]; then
            if [ "$command" = update ] || ! docker image inspect "$image" >/dev/null 2>&1; then
                docker build --tag "$image" .
            fi
            if [ "$command" = update ]; then
                compose pull diun diun-docker
            fi
        elif [ "$command" = update ]; then
            compose pull
        fi
        compose up -d --wait --wait-timeout 120
        ;;
    stop|restart) compose "$command" ;;
    status) compose ps --all ;;
    ready) compose exec -T dockerupbot dubot ready ;;
    logs) compose logs --tail 100 --follow -- "$@" ;;
    check) printf 'Deployment configuration is valid.\n' ;;
esac
