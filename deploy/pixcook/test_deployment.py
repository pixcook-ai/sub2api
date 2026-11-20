import importlib.util
from pathlib import Path
import unittest
import subprocess
import tempfile
import sys

ROOT = Path(__file__).parent
spec = importlib.util.spec_from_file_location("deployment", ROOT / "validate.py")
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)

DIGEST = "ghcr.io/pixcook-ai/sub2api@sha256:" + "a" * 64

class DeploymentValidationTests(unittest.TestCase):
    def fresh(self):
        return {"services": {
            "sub2api": {"image": DIGEST, "environment": {
                "SUB2API_UPDATE_MODE": "image", "DATABASE_HOST": "postgres", "DATABASE_PORT": "5432",
                "DATABASE_USER": "sub2api", "DATABASE_DBNAME": "sub2api", "DATABASE_PASSWORD": "local-test-db-only",
                "JWT_SECRET": "j" * 32, "TOTP_ENCRYPTION_KEY": "t" * 32,
            }, "ports": [{"host_ip": "127.0.0.1", "published": "8080", "target": 8080}],
            "volumes": [{"type": "volume", "source": "app-data", "target": "/app/data"}]},
            "postgres": {"image": "postgres:18-alpine", "environment": {"PGDATA": "/var/lib/postgresql/data"},
                         "volumes": [{"type": "volume", "source": "pg-data", "target": "/var/lib/postgresql/data"}]},
            "redis": {"image": "redis:8-alpine", "volumes": [{"type": "volume", "source": "redis-data", "target": "/data"}]},
        }}

    def test_fresh_requires_self_owned_immutable_image(self):
        model = self.fresh()
        self.assertEqual([], deployment.validate(model, "fresh"))
        for image in ("weishaw/sub2api:latest", "ghcr.io/pixcook-ai/sub2api:latest", DIGEST[:-1], DIGEST.replace("pixcook-ai", "other")):
            with self.subTest(image=image):
                model["services"]["sub2api"]["image"] = image
                self.assertTrue(deployment.validate(model, "fresh"))

    def test_rejects_binary_update_or_missing_secrets(self):
        for field in ("JWT_SECRET", "TOTP_ENCRYPTION_KEY", "DATABASE_PASSWORD", "SUB2API_UPDATE_MODE"):
            with self.subTest(field=field):
                model = self.fresh()
                model["services"]["sub2api"]["environment"].pop(field)
                self.assertTrue(deployment.validate(model, "fresh"))

    def test_rejects_non_loopback_listener_and_unpersisted_data(self):
        model = self.fresh()
        model["services"]["sub2api"]["ports"][0]["host_ip"] = "0.0.0.0"
        self.assertTrue(deployment.validate(model, "fresh"))
        model = self.fresh()
        model["services"]["sub2api"]["volumes"] = []
        self.assertTrue(deployment.validate(model, "fresh"))

    def test_fresh_rejects_host_database_exposure(self):
        model = self.fresh()
        model["services"]["postgres"]["ports"] = [{"published": "5432", "target": 5432}]
        self.assertTrue(deployment.validate(model, "fresh"))

    def test_existing_preserves_database_major_and_volumes(self):
        before = self.fresh()
        before["services"]["sub2api"]["image"] = "weishaw/sub2api:v0.2.23"
        before["services"]["postgres"]["image"] = "postgres:16-alpine"
        after = self.fresh()
        after["services"]["postgres"] = before["services"]["postgres"]
        self.assertEqual([], deployment.validate(after, "existing", baseline=before))
        after["services"]["postgres"] = self.fresh()["services"]["postgres"]
        self.assertTrue(deployment.validate(after, "existing", baseline=before))
        after["services"]["postgres"] = before["services"]["postgres"]
        after["services"]["sub2api"]["volumes"][0]["source"] = "different-data"
        self.assertTrue(deployment.validate(after, "existing", baseline=before))

    def test_existing_rejects_rotated_identity_and_database_endpoint(self):
        before = self.fresh()
        for field in ("JWT_SECRET", "TOTP_ENCRYPTION_KEY", "DATABASE_PASSWORD", "DATABASE_HOST", "DATABASE_USER", "DATABASE_DBNAME"):
            with self.subTest(field=field):
                after = self.fresh()
                after["services"]["sub2api"]["environment"][field] = "different-value-that-is-long-enough"
                self.assertTrue(deployment.validate(after, "existing", baseline=before))

    def test_existing_preserves_other_application_settings(self):
        before = self.fresh()
        after = self.fresh()
        after["services"]["sub2api"]["command"] = ["other-entrypoint"]
        self.assertTrue(deployment.validate(after, "existing", baseline=before))

    def test_existing_requires_baseline(self):
        self.assertTrue(deployment.validate(self.fresh(), "existing"))

    def test_error_text_does_not_include_secret_values(self):
        before = self.fresh()
        after = self.fresh()
        after["services"]["sub2api"]["environment"]["JWT_SECRET"] = "do-not-print-this-secret-value-123456"
        errors = deployment.validate(after, "existing", baseline=before)
        self.assertNotIn("do-not-print", "\n".join(errors))

class ComposeCommandTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        try:
            result = subprocess.run(["docker", "compose", "version"], capture_output=True)
        except FileNotFoundError:
            raise unittest.SkipTest("Docker Compose CLI is required")
        if result.returncode:
            raise unittest.SkipTest("Docker Compose CLI is required")

    def run_validator(self, temp, mode, files, baseline=None, image=DIGEST):
        env = Path(temp) / ".env"
        env.write_text("\n".join([
            "PIXCOOK_IMAGE=" + image,
            "POSTGRES_PASSWORD=private-database-test-marker",
            "REDIS_PASSWORD=private-cache-test-marker",
            "JWT_SECRET=" + "j" * 32,
            "TOTP_ENCRYPTION_KEY=" + "t" * 32,
            "ADMIN_EMAIL=admin@example.invalid",
            "ADMIN_PASSWORD=private-admin-test-marker",
            "",
        ]))
        env.chmod(0o600)
        command = [sys.executable, str(ROOT / "validate.py"), "--mode", mode, "--env-file", str(env)]
        for path in files:
            command += ["--compose-file", str(path)]
        for path in baseline or []:
            command += ["--baseline-file", str(path)]
        result = subprocess.run(command, capture_output=True, text=True)
        self.assertNotIn("private-", result.stdout + result.stderr)
        return result

    def test_actual_fresh_compose_and_invalid_image(self):
        with tempfile.TemporaryDirectory() as temp:
            result = self.run_validator(temp, "fresh", [ROOT / "compose.yaml"])
            self.assertEqual(0, result.returncode, result.stderr)
            result = self.run_validator(temp, "fresh", [ROOT / "compose.yaml"], image="ghcr.io/pixcook-ai/sub2api:latest")
            self.assertEqual(1, result.returncode)

    def test_actual_overlay_keeps_existing_postgres16_and_data(self):
        with tempfile.TemporaryDirectory() as temp:
            original = Path(temp) / "original.yaml"
            text = (ROOT / "compose.yaml").read_text()
            text = text.replace("postgres:18-alpine", "postgres:16-alpine")
            text = text.replace("image: ${PIXCOOK_IMAGE:?Set the approved ghcr.io/pixcook-ai/sub2api@sha256 digest}", "image: weishaw/sub2api:v0.2.23")
            original.write_text(text)
            result = self.run_validator(temp, "existing", [original, ROOT / "compose.existing.yaml"], [original])
            self.assertEqual(0, result.returncode, result.stderr)
            wrong = Path(temp) / "wrong.yaml"
            wrong.write_text("services:\n  postgres:\n    image: postgres:18-alpine\n")
            result = self.run_validator(temp, "existing", [original, ROOT / "compose.existing.yaml", wrong], [original])
            self.assertEqual(1, result.returncode)


if __name__ == "__main__":
    unittest.main()
