#!/bin/sh
set -eu

CDPATH='' cd -P "$(dirname "$0")/.."
real_docker=$(command -v docker)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cp -R deploy.sh scripts docker-compose.yml docker-compose.production.yml .env.example "$fixture/"
mkdir "$fixture/bin" "$fixture/stacks"
cat > "$fixture/.env" <<ENV
COMPOSE_ROOT=$fixture/stacks
WEBHOOK_SECRET=fixture
TELEGRAM_TOKEN=1234:fixture
ALLOWED_USERS=5678
ENV
unset DOCKERUPBOT_IMAGE COMPOSE_ROOT WEBHOOK_SECRET TELEGRAM_TOKEN ALLOWED_USERS
export REAL_DOCKER="$real_docker" TRACE="$fixture/trace"
cat > "$fixture/bin/docker" <<'DOCKER'
#!/bin/sh
printf '%s\n' "$*" >> "$TRACE"
case " $* " in
    *" config "*) exec "$REAL_DOCKER" "$@" ;;
esac
# Simulate an absent local image and record any deployment operations.
[ "$1 $2" != "image inspect" ]
DOCKER
chmod +x "$fixture/bin/docker"
export PATH="$fixture/bin:$PATH"

run() {
    : > "$TRACE"
    "$fixture/deploy.sh" --non-interactive "$@"
}
expect_call() {
    grep -Fxq -- "$*" "$TRACE"
}

run check
expect_call 'compose --env-file .env -f docker-compose.yml config --no-env-resolution --quiet'
run start
expect_call 'build --tag dockerupbot:local .'
expect_call 'compose --env-file .env -f docker-compose.yml up -d --wait --wait-timeout 120'
[ -d "$fixture/data/bot" ]
[ -d "$fixture/data/diun" ]
run update
expect_call 'compose --env-file .env -f docker-compose.yml pull diun diun-docker'
if run --mode production start; then
    echo 'Production accepted an unpinned image' >&2
    exit 1
fi

cat >> "$fixture/.env" <<'ENV'
DOCKERUPBOT_IMAGE=example.com/dockerupbot@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
ENV
run check
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml config --no-env-resolution --quiet'
run update
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml pull'
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml up -d --wait --wait-timeout 120'
run stop
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml stop'
run restart
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml restart'
run ready
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml exec -T dockerupbot dubot ready'
run logs dockerupbot
expect_call 'compose --env-file .env -f docker-compose.yml -f docker-compose.production.yml logs --tail 100 --follow -- dockerupbot'
run --mode dev status
expect_call 'compose --env-file .env -f docker-compose.yml ps --all'

# Environment exports take precedence over .env when Compose resolves the image.
export DOCKERUPBOT_IMAGE=example.com/dockerupbot:edge
run check
expect_call 'compose --env-file .env -f docker-compose.yml config --no-env-resolution --quiet'

export DOCKERUPBOT_IMAGE=example.com/dockerupbot@sha256:short
if run check; then
    echo 'Automatic mode accepted a malformed digest' >&2
    exit 1
fi

unset DOCKERUPBOT_IMAGE
export ALLOWED_USERS=1234 WEBHOOK_SECRET=CHANGE_ME_TO_A_LONG_RANDOM_SECRET
if output=$(run start 2>&1); then
    echo 'Non-interactive deployment accepted invalid settings' >&2
    exit 1
fi
printf '%s\n' "$output" | grep -Fq 'ALLOWED_USERS is invalid'
printf '%s\n' "$output" | grep -Fq 'WEBHOOK_SECRET is invalid'
if grep -Eq '(^build | up -d )' "$TRACE"; then
    echo 'Docker started changing the deployment before settings were validated' >&2
    exit 1
fi
