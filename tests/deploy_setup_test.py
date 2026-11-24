import contextlib
import json
from io import StringIO
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import deploy_setup


repo = Path(__file__).resolve().parents[1]


class DeploymentSetupTest(unittest.TestCase):
    def setUp(self):
        sandbox = tempfile.TemporaryDirectory()
        self.addCleanup(sandbox.cleanup)
        previous = Path.cwd()
        self.addCleanup(os.chdir, previous)
        os.chdir(sandbox.name)
        for name in [".env.example", "docker-compose.yml", "docker-compose.production.yml"]:
            Path(name).write_bytes((repo / name).read_bytes())
        relevant = [*deploy_setup.SETTINGS, "DOCKERUPBOT_IMAGE"]
        environment = {key: value for key, value in os.environ.items() if key not in relevant}
        self.enterContext(patch.dict(os.environ, environment, clear=True))
        self.enterContext(patch.object(deploy_setup.sys.stdin, "isatty", return_value=True))
        self.output = StringIO()
        self.enterContext(contextlib.redirect_stdout(self.output))
        self.enterContext(contextlib.redirect_stderr(self.output))
        self.root = Path(sandbox.name) / "stacks ' $literal \\"
        self.root.mkdir()

    def test_first_setup_reprompts_and_keeps_secrets_private(self):
        token = "123:fixture"
        secret = 'a shared "$literal" secret \\'
        with (
            patch.object(deploy_setup.getpass, "getpass", side_effect=[token, secret]) as hidden,
            patch("builtins.input", side_effect=["123", "456, 789", "/", str(self.root), "yes"]),
            patch.object(deploy_setup, "telegram_accepts", return_value=True),
        ):
            deploy_setup.main("auto", "start", True)
        self.assertEqual(hidden.call_count, 2)
        self.assertNotIn(token, self.output.getvalue())
        self.assertNotIn(secret, self.output.getvalue())
        saved = deploy_setup.effective_environment(Path(".env"))
        self.assertEqual(saved["TELEGRAM_TOKEN"], token)
        self.assertEqual(saved["WEBHOOK_SECRET"], secret)
        self.assertEqual(saved["ALLOWED_USERS"], "456, 789")
        self.assertEqual(saved["COMPOSE_ROOT"], str(self.root))
        self.assertEqual(Path(".env").stat().st_mode & 0o777, 0o600)

    def test_cancel_leaves_existing_env_unchanged(self):
        path = Path(".env")
        root = json.dumps(str(self.root).replace("$", "$$"))
        path.write_text(f"COMPOSE_ROOT={root}\nTELEGRAM_TOKEN=123:fixture\nALLOWED_USERS=456\nWEBHOOK_SECRET=\n")
        before = path.read_bytes()
        with (
            patch.object(deploy_setup.getpass, "getpass", return_value="replacement"),
            patch("builtins.input", return_value=""),
            patch.object(deploy_setup, "telegram_accepts", return_value=True),
            self.assertRaises(deploy_setup.Cancelled),
        ):
            deploy_setup.main("auto", "update", True)
        self.assertEqual(path.read_bytes(), before)
        self.assertEqual(list(Path(".").glob(".env.deploy.*")), [])

    def test_quit_and_interrupt_leave_no_env_file(self):
        for end in (":q", KeyboardInterrupt()):
            with self.subTest(end=type(end).__name__):
                behavior = {"return_value": end} if isinstance(end, str) else {"side_effect": end}
                with (
                    patch.object(deploy_setup.getpass, "getpass", **behavior),
                    self.assertRaises((deploy_setup.Cancelled, KeyboardInterrupt)),
                ):
                    deploy_setup.main("auto", "start", True)
                self.assertFalse(Path(".env").exists())
                self.assertEqual(list(Path(".").glob(".env.deploy.*")), [])


    def test_rejected_token_is_replaced_before_deployment(self):
        path = Path(".env")
        root = json.dumps(str(self.root).replace("$", "$$"))
        path.write_text(f"COMPOSE_ROOT={root}\nTELEGRAM_TOKEN=123:revoked\nALLOWED_USERS=456\nWEBHOOK_SECRET=random\n")
        with (
            patch.object(deploy_setup.getpass, "getpass", return_value="789:replacement"),
            patch("builtins.input", return_value="yes"),
            patch.object(deploy_setup, "telegram_accepts", side_effect=[False, True]) as verify,
        ):
            deploy_setup.main("auto", "start", True)
        self.assertEqual(verify.call_count, 2)
        self.assertEqual(deploy_setup.effective_environment(path)["TELEGRAM_TOKEN"], "789:replacement")
        self.assertIn("Telegram rejected TELEGRAM_TOKEN", self.output.getvalue())
        self.assertNotIn("123:revoked", self.output.getvalue())
