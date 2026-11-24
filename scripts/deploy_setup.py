"""Validate and, when interactive, prepare the dotenv file for deploy.sh."""

import getpass
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import warnings
from urllib.error import HTTPError, URLError
from urllib.request import urlopen


ENV_FILE = Path(".env")
SETTINGS = {
    "TELEGRAM_TOKEN": "Get the bot token from @BotFather. It looks like 123456789:AA...",
    "ALLOWED_USERS": "Enter your numeric Telegram user IDs, separated by commas. Use your user ID, not the bot ID.",
    "WEBHOOK_SECRET": "Enter a shared secret. You can generate one with openssl rand -hex 32.",
    "COMPOSE_ROOT": "Enter an existing absolute host directory containing the managed Compose projects, for example /srv/compose.",
}
IMAGE_HELP = "Use the full image reference from the release notes, including @sha256: followed by 64 lowercase hex digits."
IMAGE_DIGEST = re.compile(r"[^\s@]+@sha256:[0-9a-f]{64}")


class SetupError(Exception):
    pass


class Cancelled(Exception):
    pass


def effective_environment(path):
    # Ask Compose to interpret dotenv and shell precedence without expanding the deployment itself.
    fields = [*SETTINGS, "DOCKERUPBOT_IMAGE"]
    fixture = {"services": {"settings": {
        "image": "scratch",
        "environment": {key: "${" + key + ":-}" for key in fields},
    }}}
    config = subprocess.run(
        ["docker", "compose", "--env-file", str(path), "-f", "-", "config", "--format", "json"],
        input=json.dumps(fixture), text=True, capture_output=True,
    )
    if config.returncode:
        raise SetupError("Compose could not read the deployment settings. Check that Docker Compose is installed and .env contains valid KEY=value entries.")
    values = json.loads(config.stdout)["services"]["settings"]["environment"]
    # Compose serializes literal dollars as $$ so its output can be interpolated again.
    return {key: values[key].replace("$$", "$") for key in fields}


def valid(key, value, values):
    if any(ord(char) < 32 or ord(char) == 127 for char in value):
        return False
    if key == "TELEGRAM_TOKEN":
        return value != "123456789:REPLACE_ME" and bool(re.fullmatch(r"[1-9][0-9]*:[A-Za-z0-9_-]+", value))
    if key == "ALLOWED_USERS":
        users = [user.strip() for user in value.split(",")]
        bot_id = values["TELEGRAM_TOKEN"].partition(":")[0]
        return bot_id not in users and all(
            re.fullmatch(r"[1-9][0-9]{0,18}", user) and int(user) < 2**63
            for user in users
        )
    if key == "WEBHOOK_SECRET":
        return bool(value.strip()) and value == value.strip() and value != "CHANGE_ME_TO_A_LONG_RANDOM_SECRET"
    if key == "COMPOSE_ROOT":
        path = Path(value)
        try:
            return path.is_absolute() and path.is_dir() and path.resolve() != Path("/") and os.access(path, os.R_OK | os.X_OK)
        except (OSError, RuntimeError, ValueError):
            return False
    return bool(IMAGE_DIGEST.fullmatch(value))


def problem(key, value):
    instruction = SETTINGS.get(key, IMAGE_HELP)
    status = "is missing" if not value else "is invalid"
    override = f" Correct or unset the exported {key} before retrying." if key in os.environ else ""
    return f"{key} {status}. {instruction}{override}"


def ask(label, hidden=False):
    try:
        if hidden:
            with warnings.catch_warnings():
                warnings.simplefilter("error", getpass.GetPassWarning)
                value = getpass.getpass(label + " (hidden, :q to cancel) ")
        else:
            print(label + " ", end="", file=sys.stderr, flush=True)
            value = input()
    except EOFError as error:
        raise Cancelled() from error
    except getpass.GetPassWarning:
        raise SetupError("Could not hide secret input. Run deployment from a terminal with echo control.") from None
    if value.strip() == ":q":
        raise Cancelled()
    return value


def prompt_setting(key, values, changes):
    if key in os.environ:
        raise SetupError(problem(key, values[key]))
    while True:
        print(SETTINGS.get(key, IMAGE_HELP), file=sys.stderr)
        value = ask(f"{key} =", hidden=key in {"TELEGRAM_TOKEN", "WEBHOOK_SECRET"})
        if valid(key, value, values):
            values[key] = changes[key] = value
            return
        print(problem(key, value), file=sys.stderr)


def telegram_accepts(token):
    try:
        with urlopen(f"https://api.telegram.org/bot{token}/getMe", timeout=8) as response:
            result = json.load(response)
    except HTTPError as error:
        if error.code in {401, 404}:
            return False
        raise SetupError(f"Telegram returned HTTP {error.code} during token validation. Check Telegram availability and retry.") from None
    except (URLError, OSError, ValueError):
        raise SetupError("Could not verify TELEGRAM_TOKEN with Telegram. Check the connection to api.telegram.org and retry.") from None
    return isinstance(result, dict) and result.get("ok") is True


def validate_settings(mode, values, interactive, changes):
    required = list(SETTINGS)
    image = values["DOCKERUPBOT_IMAGE"]
    if mode == "production" or "@" in image:
        required.append("DOCKERUPBOT_IMAGE")
    issues = []
    for key in required:
        if not valid(key, values[key], values):
            if interactive:
                prompt_setting(key, values, changes)
            else:
                issues.append(problem(key, values[key]))
    if issues:
        raise SetupError("\n".join(issues) + "\nCorrect .env or run start from a terminal to configure it interactively.")


def compose_mode(mode, image):
    if mode != "auto":
        return mode
    return "production" if IMAGE_DIGEST.fullmatch(image) else "dev"


def check_compose(path, mode):
    args = ["docker", "compose", "--env-file", str(path), "-f", "docker-compose.yml"]
    if mode == "production":
        args += ["-f", "docker-compose.production.yml"]
    result = subprocess.run([*args, "config", "--no-env-resolution", "--quiet"], capture_output=True)
    if result.returncode:
        raise SetupError("Compose rejected the deployment configuration. Check .env and the selected Compose files.")


def main(mode, command, allow_prompt):
    deploying = command in {"start", "update"}
    interactive = deploying and allow_prompt and sys.stdin.isatty()
    if interactive:
        print("Enter :q or press Ctrl+C to cancel at any prompt.", file=sys.stderr)

    if ENV_FILE.is_file():
        source = ENV_FILE
    elif interactive:
        source = Path(".env.example")
        if not source.is_file():
            raise SetupError("Missing .env.example. Restore it from the repository and retry.")
    else:
        raise SetupError("Missing .env. Run ./deploy.sh start from a terminal or copy .env.example to .env and fill in the required settings.")

    values = effective_environment(source)
    changes = {}
    if deploying or command == "check":
        validate_settings(mode, values, interactive, changes)
    elif (mode == "production" or "@" in values["DOCKERUPBOT_IMAGE"]) and not valid("DOCKERUPBOT_IMAGE", values["DOCKERUPBOT_IMAGE"], values):
        raise SetupError(problem("DOCKERUPBOT_IMAGE", values["DOCKERUPBOT_IMAGE"]))

    if interactive:
        while not telegram_accepts(values["TELEGRAM_TOKEN"]):
            print("Telegram rejected TELEGRAM_TOKEN. Copy a valid token from @BotFather or regenerate it there.", file=sys.stderr)
            if "TELEGRAM_TOKEN" in os.environ:
                raise SetupError("Correct or unset the exported TELEGRAM_TOKEN before retrying.")
            prompt_setting("TELEGRAM_TOKEN", values, changes)
            if not valid("ALLOWED_USERS", values["ALLOWED_USERS"], values):
                prompt_setting("ALLOWED_USERS", values, changes)

    image = values["DOCKERUPBOT_IMAGE"] or "dockerupbot:local"
    selected = compose_mode(mode, image)
    staged = None
    try:
        env_path = source
        if changes or source != ENV_FILE:
            with tempfile.NamedTemporaryFile(mode="wb", prefix=".env.deploy.", dir=".", delete=False) as file:
                staged = Path(file.name)
                file.write(source.read_bytes())
                # Appending keeps the existing dotenv syntax and optional settings intact.
                for key, value in changes.items():
                    quoted = json.dumps(value.replace("$", "$$"), ensure_ascii=False)
                    file.write(f"\n{key}={quoted}\n".encode())
            env_path = staged
            resolved = effective_environment(env_path)
            if resolved != values:
                raise SetupError("The staged settings did not resolve as entered. Check exported variables and .env quoting.")

        if deploying or command == "check":
            check_compose(env_path, selected)
        if interactive and ask(f"Apply the {selected} deployment? [y/N]").strip().lower() not in {"y", "yes"}:
            raise Cancelled()
        if staged is not None:
            staged.replace(ENV_FILE)
    finally:
        if staged is not None:
            staged.unlink(missing_ok=True)

    print(selected, image)


if __name__ == "__main__":
    try:
        main(sys.argv[1], sys.argv[2], sys.argv[3] == "yes")
    except (Cancelled, KeyboardInterrupt):
        print("\nDeployment cancelled.", file=sys.stderr)
        sys.exit(130)
    except SetupError as error:
        print(error, file=sys.stderr)
        sys.exit(1)
    except (OSError, KeyError, TypeError, ValueError):
        print("Deployment setup failed. Check access to .env and verify that Docker Compose is installed.", file=sys.stderr)
        sys.exit(1)
