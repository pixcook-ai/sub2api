import contextlib
import io
import json
import pathlib
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import deploy

IMAGE = "ghcr.io/pixcook-ai/sub2api@sha256:" + "a" * 64
SHA = "b" * 40


class StagedDeployment(deploy.Deployment):
    def __init__(self, fail=None):
        self.events = []
        self.fail = fail
        self.image = IMAGE
        self.source_sha = SHA
        self.backup = pathlib.Path("/private/dev-backup")
        self.old_stopped = False
        self.candidate_started = False

    def stage(self, name):
        self.events.append(name)
        if self.fail == name:
            raise deploy.DeployError("controlled failure")

    def inspect_target(self): self.stage("inspect")
    def pull_verify(self): self.stage("pull_verify")
    def snapshot(self):
        self.stage("snapshot")
    def stop_old(self):
        self.stage("stop")
        self.old_stopped = True
    def preflight(self): self.stage("preflight")
    def switch(self):
        self.candidate_started = True
        self.stage("switch")
    def verify_live(self):
        self.stage("verify")
        return {"runtime_source_sha": SHA, "runtime_image": IMAGE}
    def save_success(self, proof): self.stage("save")
    def resume_old(self): self.stage("resume_old")
    def quarantine_candidate(self): self.stage("quarantine")
    def cleanup_preflight(self): self.stage("cleanup")


class DeployTests(unittest.TestCase):
    def test_nondev_and_invalid_inputs_rejected_before_config_read(self):
        for environment, image, sha in [("production", IMAGE, SHA), ("dev", "ghcr.io/pixcook-ai/sub2api:latest", SHA), ("dev", IMAGE, "abc")]:
            with self.assertRaises(deploy.DeployError):
                deploy.validate_request(environment, image, sha)
        with patch.object(deploy, "load_config") as config, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(deploy.main(["--environment", "production", "--image", IMAGE, "--source-sha", SHA]), 1)
            config.assert_not_called()

    def test_fixed_totp_reads_existing_key_without_generating_identity(self):
        key = "a" * 64
        self.assertEqual(deploy.fixed_totp("totp:\n  encryption_key: \"" + key + "\" # persisted\nserver:\n  port: 8080\n"), key)
        for text in ("totp: {}\n", "jwt:\n  encryption_key: " + key, "totp:\n  encryption_key: \"\"\n"):
            self.assertIsNone(deploy.fixed_totp(text))
        for text in ("totp:\n  encryption_key: CHANGE_ME", "totp:\n  encryption_key: short"):
            with self.assertRaises(deploy.DeployError): deploy.fixed_totp(text)

    def test_inspect_preserves_absent_totp_in_both_models_and_runtime(self):
        from test_deployment import DeploymentValidationTests
        model = DeploymentValidationTests().fresh()
        app = model["services"]["sub2api"]
        app["environment"].pop("TOTP_ENCRYPTION_KEY")
        app["ports"][0]["published"] = "18080"
        instance = deploy.Deployment.__new__(deploy.Deployment)
        instance.image = IMAGE
        instance.config = {"project_name": "sub2api", "expected_app_container": "sub2api", "database_service": "postgres", "redis_service": "redis", "compose_files": ["/original.yml"], "env_file": "/original.env"}
        instance.old = {"Id": "app", "Name": "/sub2api", "Mounts": [{"Destination": "/app/data", "Type": "volume"}], "Config": {"Env": [key + "=" + value for key, value in app["environment"].items()]}}
        database = {"Config": {"Env": ["POSTGRES_DB=sub2api", "POSTGRES_USER=sub2api"]}}
        instance.container = lambda service: instance.old if service == "sub2api" else database
        instance.current_files = lambda: []
        instance.command = lambda args: b"server: {}\n" if args[:2] == ["docker", "exec"] else json.dumps(model).encode()
        original_validate = deploy.validate.validate
        with patch.object(deploy, "file_sha256", return_value="fixed"), patch.object(deploy.validate, "validate", wraps=original_validate) as validator:
            instance.inspect_target()
        after, mode, before = validator.call_args.args
        self.assertEqual(mode, "existing")
        for value in (before, after, instance.candidate):
            self.assertNotIn("TOTP_ENCRYPTION_KEY", value["services"]["sub2api"]["environment"])
        self.assertNotIn("TOTP_ENCRYPTION_KEY", instance.runtime_env)

    def test_migration_preflight_rejects_unknown_modified_and_missing_applied_rows(self):
        instance = deploy.Deployment.__new__(deploy.Deployment)
        instance.migrations = {"001.sql": "a" * 64, "002.sql": "b" * 64}
        instance.sql = lambda *args: "001.sql|" + "a" * 64 + "\n"
        instance.check_migrations("clone", {})
        with self.assertRaises(deploy.DeployError): instance.check_migrations("clone", {}, complete=True)
        for rows in ("", "unknown.sql|" + "a" * 64, "001.sql|" + "b" * 64):
            instance.sql = lambda *args, value=rows: value
            with self.assertRaises(deploy.DeployError): instance.check_migrations("clone", {})

    def test_real_preflight_commands_only_attach_isolated_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            instance = deploy.Deployment.__new__(deploy.Deployment)
            instance.backup = pathlib.Path(directory)
            (instance.backup / "app-data").mkdir()
            (instance.backup / "app-data" / "config.yaml").write_text("server: {}\n")
            (instance.backup / "database.dump").write_bytes(b"custom-format-backup")
            instance.db, instance.redis = {"Image": "sha256:pg17"}, {"Image": "sha256:redis7"}
            instance.runtime_env = {"DATABASE_HOST": "LIVE_DB", "DATABASE_PASSWORD": "LIVE_SECRET", "REDIS_HOST": "LIVE_REDIS", "JWT_SECRET": "fixed-jwt"}
            instance.image, instance.run_id = IMAGE, "test-run"
            instance.created_containers = []
            calls, migrations = [], []
            def command(args, **kwargs):
                calls.append(args)
                if "pg_restore" in args:
                    self.assertEqual(kwargs["stdin"].read(), b"custom-format-backup")
                return b""
            instance.command = command
            instance.check_migrations = lambda container, env, complete=False: migrations.append((container, complete))
            instance.preflight()
            runs = [call for call in calls if call[:3] == ["docker", "run", "-d"]]
            self.assertEqual(len(runs), 3)
            self.assertIn("--internal", calls[0])
            for call in runs:
                self.assertEqual(call[call.index("--network") + 1], "pixcook-preflight-test-run")
                self.assertEqual(call[call.index("--pull") + 1], "never")
                self.assertNotIn("-p", call)
                self.assertNotIn("--publish", call)
            app_environment = (instance.preflight_dir / "app.env").read_text()
            self.assertIn("DATABASE_HOST=pixcook-preflight-test-run-db\n", app_environment)
            self.assertIn("REDIS_HOST=pixcook-preflight-test-run-redis\n", app_environment)
            self.assertNotIn("LIVE_", app_environment)
            self.assertIn("JWT_SECRET=fixed-jwt\n", app_environment)
            self.assertEqual(migrations, [("pixcook-preflight-test-run-db", False), ("pixcook-preflight-test-run-db", True)])

    def test_runner_suppresses_secret_command_failure(self):
        result = type("Result", (), {"returncode": 1, "stdout": b"SECRET", "stderr": b"SECRET"})()
        with patch.object(deploy.subprocess, "run", return_value=result):
            with self.assertRaises(deploy.DeployError) as caught:
                deploy.Runner().run(["docker", "SECRET"])
            self.assertNotIn("SECRET", str(caught.exception))

    def test_candidate_verifies_digest_binary_and_embedded_manifest_before_stopping(self):
        instance = deploy.Deployment.__new__(deploy.Deployment)
        instance.image, instance.source_sha, instance.run_id = IMAGE, SHA, "test-run"
        instance.old = {"Image": "sha256:old"}
        instance.db = {"Id": "live-db", "Config": {"Env": []}}
        instance.docker_json = lambda *args: [{"RepoDigests": [IMAGE], "Config": {"Labels": {"org.opencontainers.image.revision": SHA}}}]
        calls, checked = [], []
        def command(args, **kwargs):
            calls.append(args)
            if "/app/migration-checksums.json" in args:
                return json.dumps({"001.sql": "c" * 64}).encode()
            if "exec /app/sub2api -version 2>&1" in args:
                return ("Sub2API test (commit: " + SHA + ", built: now)").encode()
            return b""
        instance.command = command
        instance.check_migrations = lambda *args: checked.append(args)
        instance.pull_verify()
        self.assertEqual(checked, [("live-db", {})])
        self.assertEqual(instance.migrations, {"001.sql": "c" * 64})
        for call in calls:
            if call[:2] == ["docker", "run"]:
                self.assertEqual(call[call.index("--network") + 1], "none")
            self.assertNotIn("stop", call)
        self.assertEqual(calls[-1], ["docker", "image", "tag", "sha256:old", "pixcook-dev-rollback:test-run"])
        instance.docker_json = lambda *args: [{"RepoDigests": [], "Config": {"Labels": {"org.opencontainers.image.revision": SHA}}}]
        with self.assertRaises(deploy.DeployError): instance.pull_verify()

    def test_configuration_drift_fails_before_real_switch(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "compose.yaml"
            path.write_text("original")
            instance = deploy.Deployment.__new__(deploy.Deployment)
            instance.config_hashes = {str(path): deploy.file_sha256(path)}
            instance.check_config_hashes()
            path.write_text("changed")
            with self.assertRaises(deploy.DeployError): instance.check_config_hashes()

    def test_success_has_consistent_backup_then_clone_before_switch(self):
        instance = StagedDeployment()
        proof = instance.execute()
        self.assertEqual(instance.events, ["inspect", "pull_verify", "stop", "snapshot", "preflight", "switch", "verify", "save", "cleanup"])
        self.assertEqual(proof, {"environment": "dev", "status": "deployed", "image": IMAGE, "source_sha": SHA, "health": "ok", "runtime_source_sha": SHA, "runtime_image": IMAGE})

    def test_preflight_failure_resumes_old_without_real_switch(self):
        instance = StagedDeployment("preflight")
        with self.assertRaises(deploy.DeployError): instance.execute()
        self.assertIn("resume_old", instance.events)
        self.assertNotIn("switch", instance.events)
        self.assertNotIn("quarantine", instance.events)
        self.assertEqual(instance.events[-1], "cleanup")

    def test_backup_failure_resumes_old(self):
        instance = StagedDeployment("snapshot")
        with self.assertRaises(deploy.DeployError): instance.execute()
        self.assertIn("resume_old", instance.events)
        self.assertNotIn("preflight", instance.events)

    def test_real_candidate_failure_never_automatically_restores_old_database_or_app(self):
        for stage in ("switch", "verify", "save"):
            instance = StagedDeployment(stage)
            with self.assertRaises(deploy.DeployError): instance.execute()
            self.assertIn("quarantine", instance.events)
            self.assertNotIn("resume_old", instance.events)

    def test_pull_failure_never_stops_old(self):
        instance = StagedDeployment("pull_verify")
        with self.assertRaises(deploy.DeployError): instance.execute()
        self.assertNotIn("stop", instance.events)
        self.assertNotIn("resume_old", instance.events)

    def test_secure_files_reject_symlinks_and_writable_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "config.json"
            path.write_text("{}")
            path.chmod(0o666)
            with self.assertRaises(deploy.DeployError): deploy.secure_file(path, require_root=False)
            path.chmod(0o600)
            deploy.secure_file(path, require_root=False)
            link = pathlib.Path(directory) / "link"
            link.symlink_to(path)
            with self.assertRaises(deploy.DeployError): deploy.secure_file(link, require_root=False)


if __name__ == "__main__":
    unittest.main()
