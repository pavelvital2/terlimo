"""Narrow offline checks for STEP036 route and stop boundaries."""
from __future__ import annotations

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import device_route as route
import device_stage as stage


class RouteChecks(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config = Path(self.tmp.name) / "Caddyfile"
        self.config.write_text("original.example { respond ok }\n")
        self.config.chmod(0o640)
        self.original = self.config.read_bytes()
        self.owner = self.config.stat()

    def invoke(self, action, expected, restart):
        args = ["device_route.py", action, "--expect-sha256", expected,
                "--backup-dir", str(Path(self.tmp.name) / "backup")]
        with mock.patch.object(route, "CONFIG", self.config), \
             mock.patch.object(route, "validate"), \
             mock.patch.object(route, "restart", side_effect=restart), \
             mock.patch.object(route.os, "geteuid", return_value=0), \
             mock.patch("sys.argv", args), contextlib.redirect_stdout(io.StringIO()):
            return route.main()

    def test_apply_and_rollback_keep_owner_mode(self):
        self.assertEqual(self.invoke("apply", route.sha(self.original), None), 0)
        self.assertIn(route.BLOCK.encode(), self.config.read_bytes())
        current = self.config.stat()
        self.assertEqual((current.st_uid, current.st_gid, current.st_mode & 0o777),
                         (self.owner.st_uid, self.owner.st_gid, 0o640))
        self.assertEqual(self.invoke("rollback", route.sha(self.config.read_bytes()), None), 0)
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_wrong_pre_hash_does_not_restart_or_write(self):
        restart = mock.Mock()
        with self.assertRaises(SystemExit):
            self.invoke("apply", "0" * 64, restart)
        restart.assert_not_called()
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_failed_restart_restores_preimage(self):
        restart = mock.Mock(side_effect=[RuntimeError("restart failed"), None])
        with self.assertRaisesRegex(RuntimeError, "restart failed"):
            self.invoke("apply", route.sha(self.original), restart)
        self.assertEqual(restart.call_count, 2)
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.config.stat().st_mode & 0o777, 0o640)

    def test_concurrent_change_during_validation_refuses_without_restart(self):
        restart = mock.Mock()
        def change(_path):
            self.config.write_text("other owner edit\n")
        with mock.patch.object(route, "CONFIG", self.config), \
             mock.patch.object(route, "validate", side_effect=change), \
             mock.patch.object(route, "restart", restart), \
             mock.patch.object(route.os, "geteuid", return_value=0), \
             mock.patch("sys.argv", ["device_route.py", "apply", "--expect-sha256",
                                      route.sha(self.original), "--backup-dir", str(Path(self.tmp.name) / "backup")]):
            with self.assertRaisesRegex(RuntimeError, "changed after"):
                route.main()
        restart.assert_not_called()
        self.assertEqual(self.config.read_text(), "other owner edit\n")


class StopChecks(unittest.TestCase):
    def test_one_stop_error_does_not_skip_other_owned_processes(self):
        with tempfile.TemporaryDirectory() as root:
            work = Path(root)
            for name in ("serve", "api", "worker", "gateway"):
                (work / f"{name}.pid.json").write_text("{}")
            with mock.patch.object(stage, "resolve_work", return_value=(work, {"owned": True})), \
                 mock.patch.object(stage, "pid_record_path", side_effect=lambda _work, name: work / f"{name}.pid.json"), \
                 mock.patch.object(stage, "verified_alive", return_value=True), \
                 mock.patch.object(stage, "stop_one", side_effect=[RuntimeError("relay failed"),
                      {"action": "term_exit", "verified": True}, {"action": "term_exit", "verified": True},
                      {"action": "term_exit", "verified": True}]) as stop:
                result = stage.stop_package(work)
            self.assertEqual(stop.call_count, 4)
            self.assertFalse(result["verified_exit"])
            self.assertEqual(result["steps"]["serve"]["action"], "stop_error")

    def test_scoped_postgres_stop_keeps_work(self):
        with tempfile.TemporaryDirectory() as root:
            work = Path(root)
            with mock.patch.object(stage, "resolve_work", return_value=(work, {"owned": True})), \
                 mock.patch.object(stage, "stop_package_postgres", return_value={"ok": True, "verified": True, "stopped": True}) as pg, \
                 mock.patch("sys.argv", ["device_stage.py", "--work", str(work), "--stop-postgres"]), \
                 contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(stage.main(), 0)
            pg.assert_called_once_with(work)
            self.assertTrue(work.exists())
            self.assertTrue(json.loads(output.getvalue())["verified"])


class NodeIdentityChecks(unittest.TestCase):
    def test_phone_node_launch_uses_registered_step036_identity(self):
        with tempfile.TemporaryDirectory() as root:
            work = Path(root)
            creds = work / "creds"
            creds.mkdir()
            (creds / "wl-test-dtls-cert").touch()
            (creds / "wl-test-dtls-key").touch()
            gateway = object.__new__(stage.NetnsPhoneGateway)
            gateway.work = str(work)
            gateway.creds = str(creds)
            gateway.log_path = str(work / "node.log")
            gateway.listen_port = stage.PHONE_DTLS
            gateway.wg_port = stage.PHONE_WG
            with mock.patch.object(stage.subprocess, "Popen") as launch:
                gateway.start()
            gateway.log.close()
            command = launch.call_args.args[0][-1]
            self.assertIn("WL_TEST_NODE_ID=terlimo-036-node ", command)
            self.assertNotIn("WL_TEST_NODE_ID=terlimo-033-node", command)


if __name__ == "__main__":
    unittest.main()
