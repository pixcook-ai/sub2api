import hashlib
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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread
from types import SimpleNamespace
from unittest.mock import patch

LAUNCHER = runpy.run_path(str(Path(__file__).with_name("launch.py")))


class LauncherBoundaryTest(unittest.TestCase):
    def description(self):
        return {"contract": "bps-files/1", "json_profile": "bps-files-json/1",
                "backend_id": "fixture-test", "subject_fingerprint": "sha256:" + "1" * 64,
                "credential_generation": "generation-1",
                "capabilities": {"batch_admit": True, "ensure_upload": True, "metadata_recovery": False},
                "limits": {"file_bytes": 200 << 20, "message_bytes": 500 << 20,
                           "message_positions": 20, "staging_bytes": 2 << 30,
                           "admission_bytes": 500 << 20, "concurrent_admissions": 4,
                           "live_admissions": 32, "admission_ttl_seconds": 600,
                           "manifest_bytes": 64 << 20, "unique_files": 128,
                           "free_space_reserve_bytes": 1, "admit_timeout_seconds": 300,
                           "ensure_timeout_seconds": 300}}

    def describe_server(self, payload, *, status=200, location=None):
        observed = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                observed.append({"path": self.path, "service_key": self.headers.get("X-Bps-Service-Key"),
                                 "authorization": self.headers.get("Authorization")})
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                if location:
                    self.send_header("Location", location)
                self.end_headers()
                self.wfile.write(payload)
            def log_message(self, *_args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_port}", observed

    def managed_gateway(self, directory, control):
        root = Path(directory)
        service = root / "service.key"
        service.write_text("s" * 48)
        service.chmod(0o600)
        return {"managed_files": {"contract": "bps-files/1", "json_profile": "bps-files-json/1",
                                  "control_url": control, "service_key_file": str(service),
                                  "backend_id": "fixture-test", "credential_generation": "generation-1"}}

    def test_real_describe_uses_independent_key_and_observed_identity(self):
        wanted = self.description()
        control, observed = self.describe_server(json.dumps(wanted).encode())
        with tempfile.TemporaryDirectory() as directory:
            gateway = self.managed_gateway(directory, control)
            result = LAUNCHER["describe_managed_files"](gateway)
            self.assertEqual(result["managed_files"]["description"], wanted)
            self.assertNotIn("description", gateway["managed_files"], "preflight must not mutate its unverified input")
        self.assertEqual(observed, [{"path": "/internal/bps-files/v1/describe", "service_key": "s" * 48,
                                     "authorization": None}])

    def test_describe_redirect_is_not_followed_or_leaked(self):
        target, received = self.describe_server(json.dumps(self.description()).encode())
        source, observed = self.describe_server(b"{}", status=302, location=target + "/leak")
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "managed_files Describe failed"):
                LAUNCHER["describe_managed_files"](self.managed_gateway(directory, source))
        self.assertEqual(len(observed), 1)
        self.assertEqual(received, [])

    def test_describe_rejects_wrong_identity_unknown_fields_and_unbounded_limits(self):
        for mutation in ({"backend_id": "other"}, {"subject_fingerprint": "invalid"},
                         {"extra": "secret"}, {"capabilities": {"batch_admit": True, "ensure_upload": True, "metadata_recovery": True}},
                         {"limits": self.description()["limits"] | {"file_bytes": True}},
                         {"limits": self.description()["limits"] | {"file_bytes": 20 << 20}},
                         {"limits": self.description()["limits"] | {"message_bytes": 40 << 20}},
                         {"limits": self.description()["limits"] | {"admission_bytes": 40 << 20}},
                         {"limits": self.description()["limits"] | {"live_admissions": 1}},
                         {"limits": self.description()["limits"] | {"staging_bytes": 17 << 30}}):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                control, _observed = self.describe_server(json.dumps(self.description() | mutation).encode())
                with self.assertRaisesRegex(ValueError, "managed_files Describe"):
                    LAUNCHER["describe_managed_files"](self.managed_gateway(directory, control))

    def test_describe_rejects_duplicate_json_and_large_bodies(self):
        for payload in (b'{"contract":"bps-files/1","contract":"bps-files/1"}', b" " * (65536 + 1)):
            with self.subTest(length=len(payload)), tempfile.TemporaryDirectory() as directory:
                control, _observed = self.describe_server(payload)
                with self.assertRaisesRegex(ValueError, "managed_files Describe"):
                    LAUNCHER["describe_managed_files"](self.managed_gateway(directory, control))

    def test_gateway_snapshot_keeps_wrapper_cases_and_both_sources_immutable(self):
        paths = ("scripts/sdk-conformance/fixture.py", "scripts/sdk-conformance/gateway_fixture.go.txt",
                 "scripts/sdk-conformance/native_fixture.go.txt", "scripts/sdk-conformance/cases/sse.json")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repository, run = root / "repository", root / "run"
            run.mkdir(mode=0o700)
            expected = {}
            for index, relative in enumerate(paths):
                source = repository / relative
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_bytes(f"source-{index}\n".encode())
                expected[relative] = source.read_bytes()
            snapshot, hashes = LAUNCHER["snapshot_gateway_sources"](repository, run)
            for relative in paths:
                (repository / relative).write_text("changed after snapshot")
            self.assertEqual(set(hashes), set(paths))
            for relative in paths:
                self.assertEqual((snapshot / relative).read_bytes(), expected[relative])
                self.assertEqual(hashes[relative], hashlib.sha256(expected[relative]).hexdigest())
                self.assertEqual((snapshot / relative).stat().st_mode & 0o777, 0o400)
            self.assertEqual(snapshot.stat().st_mode & 0o777, 0o700)

    def test_seed_input_contains_observed_description_without_rewriting_gateway(self):
        wanted = self.description()
        control, observed = self.describe_server(json.dumps(wanted).encode())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            gateway = self.managed_gateway(directory, control)
            original = json.dumps(gateway)
            manifest = root / "gateway.json"
            manifest.write_text(original)
            managed, seed_input = LAUNCHER["prepare_seed_gateway"](gateway, root)
            self.assertEqual(managed["managed_files"]["description"], wanted)
            self.assertEqual(json.loads(seed_input.read_text()), managed)
            self.assertEqual(seed_input.stat().st_mode & 0o777, 0o600)
            self.assertEqual(manifest.read_text(), original)
            self.assertNotIn("description", gateway["managed_files"])
        self.assertEqual(len(observed), 1)

    def test_launch_rejects_failed_describe_before_database_or_build(self):
        control, observed = self.describe_server(b"{}", status=500)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            entrypoint = root / "backend/cmd/server/main.go"
            entrypoint.parent.mkdir(parents=True)
            entrypoint.write_text("package main\n")
            args = SimpleNamespace(main_repo=str(root), out=str(root / "main.json"),
                                   gateway_repo=None, keep_private_logs=False,
                                   postgres_image="unused-postgres", redis_image="unused-redis")
            with patch.dict(LAUNCHER["launch"].__globals__, {"command": lambda *_a, **_kw: self.fail("service or build started before preflight")}):
                with self.assertRaisesRegex(ValueError, "managed_files Describe"):
                    LAUNCHER["launch"](args, self.managed_gateway(directory, control))
            self.assertFalse((root / "main.json").exists())
        self.assertEqual(len(observed), 1)

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

    def test_launcher_provenance_requires_matching_tracked_checkout_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            repository = Path(directory)
            relative = "tools/bps-sdk-fixture/launch.py"
            launcher = repository / relative
            launcher.parent.mkdir(parents=True)
            launcher.write_bytes(b"approved launcher\n")
            subprocess.run(["rtk", "proxy", "git", "init", "-q", directory], check=True, capture_output=True)
            self.assertFalse(LAUNCHER["launcher_matches_checkout"](repository, b"approved launcher\n"))
            subprocess.run(["rtk", "proxy", "git", "-C", directory, "add", relative], check=True, capture_output=True)
            subprocess.run(["rtk", "proxy", "git", "-C", directory, "-c", "user.name=Fixture",
                            "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"],
                           check=True, capture_output=True)
            self.assertTrue(LAUNCHER["launcher_matches_checkout"](repository, b"approved launcher\n"))
            self.assertFalse(LAUNCHER["launcher_matches_checkout"](repository, b"different launcher\n"))

    def invoke(self, directory, *, gateway="http://127.0.0.1:8790", metrics="http://127.0.0.1:8791/metrics", key_mode=0o600, existing_output=False, managed=None, require_managed=False):
        root = Path(directory)
        key = root / "gateway.key"
        key.write_text("synthetic-fixture-key\n")
        key.chmod(key_mode)
        manifest = root / "gateway.json"
        value = {
            "contract_version": 1, "source": "local", "seam": "gateway-diagnostic",
            "gateway_base_url": gateway, "api_key_file": str(key), "bps_metrics_url": metrics,
        }
        if managed is not None:
            value["managed_files"] = managed
        manifest.write_text(json.dumps(value))
        manifest.chmod(0o600)
        output = root / "main.json"
        if existing_output:
            output.write_text("user-owned-content")
        result = subprocess.run([
            sys.executable, str(Path(__file__).with_name("launch.py")),
            "--gateway-manifest", str(manifest), "--out", str(output), "--validate-only",
            *(["--require-managed-files"] if require_managed else []),
        ], text=True, capture_output=True)
        return result, output

    def test_required_managed_contract_is_rejected_before_services(self):
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(directory, require_managed=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("managed_files contract is required", result.stderr)
            self.assertFalse(output.exists())

    def test_duplicate_or_oversized_private_manifest_is_rejected(self):
        for variant in ("duplicate", "oversized"):
            with self.subTest(variant=variant), tempfile.TemporaryDirectory() as directory:
                result, _output = self.invoke(directory)
                self.assertEqual(result.returncode, 0, result.stderr)
                manifest = Path(directory) / "gateway.json"
                payload = manifest.read_text()
                manifest.write_text(payload[:-1] + ',"source":"local"}' if variant == "duplicate"
                                    else payload + ' ' * (1 << 20))
                with self.assertRaises(ValueError):
                    LAUNCHER["gateway_inputs"](manifest)

    def test_managed_contract_checks_control_origin_and_independent_key(self):
        mutations = [({}, 0, ""),
                     ({"control_url": "http://127.0.0.1:8792"}, 2, "same origin"),
                     ({"control_url": "http://127.0.0.1:8790/v1"}, 2, "root"),
                     ({"contract": "bps-files/2"}, 2, "contract"),
                     ({"backend_id": ""}, 2, "identity")]
        for mutation, status, message in mutations:
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                service = Path(directory) / "service.key"
                service.write_text("s" * 48)
                service.chmod(0o600)
                managed = {"contract": "bps-files/1", "json_profile": "bps-files-json/1",
                           "control_url": "http://127.0.0.1:8790", "service_key_file": str(service),
                           "backend_id": "fixture-test", "credential_generation": "generation-1"} | mutation
                result, output = self.invoke(directory, managed=managed, require_managed=True)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertIn(message, result.stderr)
                self.assertFalse(output.exists())

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
