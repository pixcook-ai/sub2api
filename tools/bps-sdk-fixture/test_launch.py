import json
import os
from pathlib import Path
import runpy
import signal
import subprocess
import sys
import tempfile
import unittest
import time

LAUNCHER = runpy.run_path(str(Path(__file__).with_name("launch.py")))


class LauncherBoundaryTest(unittest.TestCase):
    @unittest.skipUnless(hasattr(os, "fork"), "fixture process groups require POSIX")
    def test_shutdown_reaps_child_after_group_leader_exits(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_file = Path(directory) / "child.pid"
            code = """import os, signal, sys, time
child = os.fork()
if child == 0:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    with open(sys.argv[1], 'w') as f:
        f.write(str(os.getpid()))
while True:
    time.sleep(1)
"""
            process = subprocess.Popen([sys.executable, "-c", code, str(pid_file)], start_new_session=True)
            try:
                deadline = time.monotonic() + 5
                while not pid_file.exists() and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(pid_file.exists(), "child did not start")
                child = int(pid_file.read_text())
                LAUNCHER["stop_process_group"](process)
                deadline = time.monotonic() + 3
                state = ""
                while time.monotonic() < deadline:
                    state = subprocess.run(["rtk", "proxy", "ps", "-o", "stat=", "-p", str(child)], capture_output=True, text=True).stdout.strip()
                    if not state or state.startswith("Z"):
                        break
                    time.sleep(0.02)
                self.assertTrue(not state or state.startswith("Z"), f"child still running: {state}")
            finally:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)

    def test_untracked_build_source_is_not_clean_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            for arguments in (["init", "-q", directory], ["-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture"]):
                subprocess.run(["rtk", "proxy", "git", *arguments], check=True, capture_output=True)
            clean = LAUNCHER["source_state"](Path(directory))
            self.assertFalse(clean["source_dirty"])
            (Path(directory) / "untracked.go").write_text("package fixture\n")
            dirty = LAUNCHER["source_state"](Path(directory))
            self.assertFalse(dirty["tracked_dirty"])
            self.assertTrue(dirty["source_dirty"])
            self.assertEqual(clean["commit"], dirty["commit"])

    def invoke(self, directory, *, gateway="http://127.0.0.1:8790", metrics="http://127.0.0.1:8791/metrics", key_mode=0o600, existing_output=False):
        root = Path(directory)
        key = root / "gateway.key"
        key.write_text("synthetic-fixture-key\n")
        key.chmod(key_mode)
        manifest = root / "gateway.json"
        manifest.write_text(json.dumps({
            "contract_version": 1, "source": "local", "seam": "gateway-diagnostic",
            "gateway_base_url": gateway, "api_key_file": str(key), "bps_metrics_url": metrics,
        }))
        manifest.chmod(0o600)
        output = root / "main.json"
        if existing_output:
            output.write_text("user-owned-content")
        result = subprocess.run([
            sys.executable, str(Path(__file__).with_name("launch.py")),
            "--gateway-manifest", str(manifest), "--out", str(output), "--validate-only",
        ], text=True, capture_output=True)
        return result, output

    def test_remote_gateway_is_rejected_before_starting_services(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            key = root / "gateway.key"
            key.write_text("synthetic-fixture-key\n")
            key.chmod(0o600)
            manifest = root / "gateway.json"
            manifest.write_text(json.dumps({
                "contract_version": 1,
                "source": "local",
                "seam": "gateway-diagnostic",
                "gateway_base_url": "https://api.openai.com",
                "api_key_file": str(key),
                "bps_metrics_url": "http://127.0.0.1:8791/metrics",
            }))
            manifest.chmod(0o600)
            output = root / "main.json"
            result = subprocess.run([
                sys.executable, str(Path(__file__).with_name("launch.py")),
                "--gateway-manifest", str(manifest), "--out", str(output),
                "--validate-only",
            ], text=True, capture_output=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("loopback", result.stderr)
            self.assertFalse(output.exists())

    def test_local_validation_does_not_create_services_or_output(self):
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), "validated local fixture inputs")
            self.assertFalse(output.exists())

    def test_external_metrics_endpoint_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(directory, metrics="http://192.0.2.2:8080/metrics")
            self.assertEqual(result.returncode, 2)
            self.assertIn("bps_metrics_url", result.stderr)
            self.assertFalse(output.exists())

    def test_shared_key_permissions_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(directory, key_mode=0o644)
            self.assertEqual(result.returncode, 2)
            self.assertIn("private regular file", result.stderr)
            self.assertFalse(output.exists())

    def test_existing_output_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(directory, existing_output=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("refuse to overwrite", result.stderr)
            self.assertEqual(output.read_text(), "user-owned-content")

    def test_shared_manifest_is_rejected_before_services(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = root / "gateway.json"
            manifest.write_text("{}")
            manifest.chmod(0o644)
            result = subprocess.run([sys.executable, str(Path(__file__).with_name("launch.py")), "--gateway-manifest", str(manifest), "--out", str(root / "out.json"), "--validate-only"], text=True, capture_output=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("gateway_manifest must be a private regular file", result.stderr)


if __name__ == "__main__":
    unittest.main()
