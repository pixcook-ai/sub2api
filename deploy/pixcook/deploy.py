#!/usr/bin/env python3
"""Root-only, pinned development deployment. Never deploys production."""
import argparse
import copy
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import signal
import shutil
import stat
import subprocess
import sys
import tarfile
import time
import urllib.request

import validate

CONFIG = Path("/etc/pixcook-cicd/dev.json")
SOURCE = re.compile(r"[0-9a-f]{40}\Z")
FIELDS = {"environment", "compose_files", "env_file", "project_name", "expected_app_container", "database_service", "redis_service", "health_url", "backup_dir", "state_dir"}


class DeployError(Exception):
    pass


def validate_request(environment, image, source_sha):
    # Must precede file access, locks, pulls, or any other side effect.
    if environment != "dev":
        raise DeployError("only the dev environment is supported")
    if not validate.IMAGE.fullmatch(image):
        raise DeployError("an immutable Pixcook GHCR digest is required")
    if not SOURCE.fullmatch(source_sha):
        raise DeployError("a full lowercase source SHA is required")


def secure_file(path, require_root=True):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o022 or (require_root and info.st_uid != 0):
        raise DeployError("configuration must be a root-owned regular file without group/world write access")
    if require_root:
        for parent in path.absolute().parents:
            info = parent.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
                raise DeployError("configuration parent directories must be root-owned and protected")
    return path


def protected_directory(path):
    path = Path(path)
    if not path.is_absolute() or path.is_symlink():
        raise DeployError("state and backup paths must be absolute protected directories")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    for parent in (path, *path.parents):
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise DeployError("state and backup directories must be root-owned and protected")
    return path


def load_config():
    if os.geteuid() != 0:
        raise DeployError("deployment requires the restricted root entry point")
    config = json.loads(secure_file(CONFIG).read_text())
    if set(config) != FIELDS or config["environment"] != "dev":
        raise DeployError("invalid development configuration fields")
    # The operator cannot select a different site using environment variables.
    fixed = {"project_name": "sub2api", "expected_app_container": "sub2api", "database_service": "postgres", "redis_service": "redis", "health_url": "http://127.0.0.1:18080/health", "backup_dir": "/var/backups/pixcook-dev", "state_dir": "/var/lib/pixcook-cicd/dev", "env_file": "/opt/bps-gateway/services/sub2api/.env", "compose_files": ["/opt/bps-gateway/services/sub2api/docker-compose.yml"]}
    if any(config.get(key) != value for key, value in fixed.items()):
        raise DeployError("configuration does not match the pinned dev deployment")
    for filename in [config["env_file"], *config["compose_files"]]:
        secure_file(filename)
    return config


class Runner:
    def run(self, args, *, input=None, stdin=None, output=None, timeout=600, check=True):
        try:
            result = subprocess.run(args, input=input, stdin=stdin, stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, env={"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/root", "LC_ALL": "C"})
        except (OSError, subprocess.TimeoutExpired) as error:
            raise DeployError("deployment command failed or timed out; details suppressed") from error
        if check and result.returncode:
            raise DeployError("deployment command failed; credentials and command output suppressed")
        return result.stdout or b""


def atomic_json(path, value):
    temporary = path.with_name(path.name + ".tmp")
    with open(temporary, "x", encoding="utf-8") as stream:
        os.chmod(temporary, 0o600)
        json.dump(value, stream, sort_keys=True)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def fixed_totp(text):
    """Read only a scalar under the existing totp block; never rewrite YAML."""
    block = re.search(r"(?m)^totp:\s*(?:#.*)?\n((?:[ \t]+.*\n?)*)", text)
    match = re.search(r"(?m)^ +encryption_key: *([^\r\n#]+)", block.group(1)) if block else None
    value = match.group(1).strip().strip("\"'") if match else ""
    if not value:
        return None
    if len(value) < 32 or value.startswith("CHANGE_ME") or any(character.isspace() for character in value):
        raise DeployError("existing fixed TOTP key is invalid; identity changes require an explicit operator task")
    return value


class Deployment:
    def __init__(self, config, image, source_sha, runner=None):
        self.config, self.image, self.source_sha = config, image, source_sha
        self.runner = runner or Runner()
        self.state = protected_directory(config["state_dir"])
        self.backups = protected_directory(config["backup_dir"])
        self.run_id = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(4)
        self.backup = self.backups / self.run_id
        self.old_stopped = False
        self.candidate_started = False
        self.created_containers = []
        self.created_network = None
        self.preflight_dir = None
        self.overlay = self.state / "current.json"

    def command(self, args, **kwargs):
        return self.runner.run(args, **kwargs)

    def docker_json(self, *args):
        return json.loads(self.command(["docker", *args]))

    def compose(self, extra_files=()):
        args = ["docker", "compose", "--env-file", self.config["env_file"], "--project-name", self.config["project_name"]]
        for filename in [*self.config["compose_files"], *extra_files]:
            args.extend(["-f", str(filename)])
        return args

    def current_files(self):
        if self.overlay.exists():
            secure_file(self.overlay)
            return [self.overlay]
        return []

    def container(self, service):
        value = self.command(self.compose(self.current_files()) + ["ps", "-q", service]).decode().strip()
        if not re.fullmatch(r"[0-9a-f]{12,64}", value):
            raise DeployError("expected exactly one running dev service container")
        info = self.docker_json("inspect", value)[0]
        labels = info["Config"].get("Labels") or {}
        if labels.get("com.docker.compose.project") != self.config["project_name"] or labels.get("com.docker.compose.service") != service or not info["State"]["Running"]:
            raise DeployError("dev service container identity mismatch")
        return info

    def inspect_target(self):
        self.baseline = json.loads(self.command(self.compose() + ["config", "--format", "json"]))
        self.previous = json.loads(self.command(self.compose(self.current_files()) + ["config", "--format", "json"]))
        self.old = self.container("sub2api")
        if self.old["Name"].lstrip("/") != self.config["expected_app_container"]:
            raise DeployError("application container is not the pinned dev target")
        self.db = self.container(self.config["database_service"])
        self.redis = self.container(self.config["redis_service"])
        app = self.baseline["services"]["sub2api"]
        ports = app.get("ports", [])
        if len(ports) != 1 or ports[0].get("host_ip") != "127.0.0.1" or str(ports[0].get("published")) != "18080" or int(ports[0].get("target", 0)) != 8080:
            raise DeployError("dev application must use only 127.0.0.1:18080")
        mounts = self.old.get("Mounts", [])
        if len(mounts) != 1 or mounts[0].get("Destination") != "/app/data" or mounts[0].get("Type") not in ("bind", "volume"):
            raise DeployError("unsupported app mounts; explicit backup review required")
        self.runtime_env = dict(item.split("=", 1) for item in self.old["Config"]["Env"] if "=" in item)
        db_env = self.db_environment()
        if (self.runtime_env.get("DATABASE_HOST") != self.config["database_service"] or
                self.runtime_env.get("DATABASE_DBNAME") != db_env.get("POSTGRES_DB", db_env.get("POSTGRES_USER", "postgres")) or
                self.runtime_env.get("DATABASE_USER") != db_env.get("POSTGRES_USER", "postgres")):
            raise DeployError("app database identity does not match the pinned local database")
        self.candidate = copy.deepcopy(self.baseline)
        self.candidate["services"]["sub2api"]["image"] = self.image
        self.candidate["services"]["sub2api"].setdefault("environment", {})["SUB2API_UPDATE_MODE"] = "image"
        old_validation, new_validation = copy.deepcopy(self.baseline), copy.deepcopy(self.candidate)
        if not app.get("environment", {}).get("TOTP_ENCRYPTION_KEY"):
            persisted = self.command(["docker", "exec", self.old["Id"], "cat", "/app/data/config.yaml"]).decode()
            value = fixed_totp(persisted)
            if value is not None:
                for model in (old_validation, new_validation):
                    model["services"]["sub2api"]["environment"]["TOTP_ENCRYPTION_KEY"] = value
        if validate.validate(new_validation, "existing", old_validation):
            raise DeployError("dev deployment failed existing-site preservation validation")
        # Also protect an already-installed overlay from unreviewed changes.
        previous = copy.deepcopy(self.previous)
        baseline = copy.deepcopy(self.baseline)
        for model in (previous, baseline):
            model["services"]["sub2api"].pop("image", None)
            model["services"]["sub2api"].get("environment", {}).pop("SUB2API_UPDATE_MODE", None)
        if previous != baseline:
            raise DeployError("current overlay changes settings beyond image and update mode")
        self.config_hashes = {str(path): file_sha256(path) for path in [*self.config["compose_files"], self.config["env_file"], *self.current_files()]}

    def pull_verify(self):
        self.command(["docker", "pull", self.image], timeout=900)
        self.image_info = self.docker_json("image", "inspect", self.image)[0]
        if self.image not in self.image_info.get("RepoDigests", []) or (self.image_info["Config"].get("Labels") or {}).get("org.opencontainers.image.revision") != self.source_sha:
            raise DeployError("candidate digest/revision metadata mismatch")
        # The binary writes version with log.Printf (stderr), verified separately
        # by redirecting within the network-isolated one-shot container.
        version = self.command(["docker", "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh", self.image, "-c", "exec /app/sub2api -version 2>&1"], timeout=30).decode()
        if not re.search(r"commit: " + re.escape(self.source_sha) + r"(?:,|\))", version):
            raise DeployError("candidate binary source SHA mismatch")
        try:
            self.migrations = json.loads(self.command(["docker", "run", "--rm", "--network", "none", "--entrypoint", "/bin/cat", self.image, "/app/migration-checksums.json"], timeout=30))
        except DeployError:
            manifest = Path("/opt/pixcook-cicd/migrations") / (self.source_sha + ".json")
            self.migrations = json.loads(secure_file(manifest).read_text())
        if not self.migrations or any(not re.fullmatch(r"[A-Za-z0-9_.-]+\.sql", name) or not re.fullmatch(r"[0-9a-f]{64}", checksum) for name, checksum in self.migrations.items()):
            raise DeployError("invalid root-approved source migration manifest")
        self.check_migrations(self.db["Id"], self.db_environment())
        self.command(["docker", "image", "tag", self.old["Image"], "pixcook-dev-rollback:" + self.run_id.lower()])

    def db_environment(self):
        return dict(item.split("=", 1) for item in self.db["Config"]["Env"] if "=" in item)

    def sql(self, container, environment, query):
        return self.command(["docker", "exec", container, "psql", "-U", environment.get("POSTGRES_USER", "postgres"), "-d", environment.get("POSTGRES_DB", environment.get("POSTGRES_USER", "postgres")), "-v", "ON_ERROR_STOP=1", "-At", "-c", query]).decode()

    def check_migrations(self, container, environment, complete=False):
        rows = self.sql(container, environment, "SELECT filename || '|' || checksum FROM schema_migrations ORDER BY filename")
        applied = dict(line.split("|", 1) for line in rows.splitlines() if line)
        if not applied or any(self.migrations.get(name) != checksum for name, checksum in applied.items()) or (complete and applied != self.migrations):
            raise DeployError("migration checksums do not match approved source")

    def stop_old(self):
        self.check_config_hashes()
        self.old_stopped = True
        self.command(["docker", "stop", "--time", "45", self.old["Id"]], timeout=60)

    def snapshot(self):
        self.backup.mkdir(mode=0o700)
        atomic_json(self.backup / "old-runtime.json", {"container": self.old, "database_image": self.db["Image"], "redis_image": self.redis["Image"], "image": self.old["Image"]})
        files = [*self.config["compose_files"], self.config["env_file"], *self.current_files()]
        for index, path in enumerate(files):
            shutil.copy2(path, self.backup / ("config-" + str(index)))
            os.chmod(self.backup / ("config-" + str(index)), 0o600)
        self.command(["docker", "cp", "-a", self.old["Id"] + ":/app/data", str(self.backup / "app-data")])
        with tarfile.open(self.backup / "app-data.tar", "w", dereference=False) as archive:
            archive.add(self.backup / "app-data", arcname="app-data", recursive=True)
        environment = self.db_environment()
        with open(self.backup / "database.dump", "xb") as output:
            self.command(["docker", "exec", self.db["Id"], "pg_dump", "-U", environment.get("POSTGRES_USER", "postgres"), "-d", environment.get("POSTGRES_DB", environment.get("POSTGRES_USER", "postgres")), "--format=custom", "--no-owner", "--no-acl"], output=output, timeout=1800)
        self.check_migrations(self.db["Id"], environment)
        atomic_json(self.backup / "backup.json", {"environment": "dev", "source_sha": self.source_sha, "image": self.image, "database_sha256": file_sha256(self.backup / "database.dump"), "app_data_sha256": file_sha256(self.backup / "app-data.tar"), "status": "complete"})

    def preflight(self):
        self.preflight_dir = self.backup / "preflight"
        self.preflight_dir.mkdir(mode=0o700)
        self.created_network = "pixcook-preflight-" + self.run_id.lower()
        self.command(["docker", "network", "create", "--internal", "--label", "pixcook.cicd.preflight=" + self.run_id, self.created_network])
        db_name = self.start_clone("db", self.db["Image"], {"POSTGRES_USER": "preflight", "POSTGRES_PASSWORD": secrets.token_hex(32), "POSTGRES_DB": "preflight"})
        db_env = {"POSTGRES_USER": "preflight", "POSTGRES_DB": "preflight"}
        self.wait_command(["docker", "exec", db_name, "pg_isready", "-U", "preflight", "-d", "preflight"])
        with open(self.backup / "database.dump", "rb") as source:
            self.command(["docker", "exec", "-i", db_name, "pg_restore", "-U", "preflight", "-d", "preflight", "--no-owner", "--no-acl", "--exit-on-error"], stdin=source, timeout=1800)
        self.check_migrations(db_name, db_env)
        redis_name = self.start_clone("redis", self.redis["Image"], {})
        shutil.copytree(self.backup / "app-data", self.preflight_dir / "app-data", symlinks=True)
        app_env = dict(self.runtime_env)
        app_env.update({"DATABASE_HOST": db_name, "DATABASE_PORT": "5432", "DATABASE_USER": "preflight", "DATABASE_PASSWORD": self.clone_password, "DATABASE_DBNAME": "preflight", "DATABASE_SSLMODE": "disable", "REDIS_HOST": redis_name, "REDIS_PORT": "6379", "REDIS_USERNAME": "", "REDIS_PASSWORD": "", "REDIS_DB": "0", "SERVER_HOST": "0.0.0.0", "SERVER_PORT": "8080", "SUB2API_UPDATE_MODE": "image", "AUTO_SETUP": "false"})
        app_name = self.start_clone("app", self.image, app_env, ["--mount", "type=bind,src=" + str(self.preflight_dir / "app-data") + ",dst=/app/data"])
        self.wait_command(["docker", "exec", app_name, "wget", "-q", "-T", "3", "-O", "/dev/null", "http://127.0.0.1:8080/health"], attempts=120)
        self.check_migrations(db_name, db_env, complete=True)
        self.command(["docker", "stop", "--time", "30", app_name])

    def start_clone(self, suffix, image, environment, extra=()):
        name = "pixcook-preflight-" + self.run_id.lower() + "-" + suffix
        env_file = self.preflight_dir / (suffix + ".env")
        if any("\n" in str(value) or "\r" in str(value) for value in environment.values()):
            raise DeployError("multiline environment values require explicit preflight review")
        env_file.write_text("".join(key + "=" + str(value) + "\n" for key, value in environment.items()))
        env_file.chmod(0o600)
        if suffix == "db":
            self.clone_password = environment["POSTGRES_PASSWORD"]
        # Names are reserved before creation so a partially created container is cleaned.
        self.created_containers.append(name)
        self.command(["docker", "run", "-d", "--name", name, "--label", "pixcook.cicd.preflight=" + self.run_id, "--network", self.created_network, "--pull", "never", "--env-file", str(env_file), *extra, image])
        return name

    def wait_command(self, command, attempts=60):
        for _ in range(attempts):
            try:
                self.command(command, timeout=10)
                return
            except DeployError:
                time.sleep(2)
        raise DeployError("isolated preflight did not become healthy")

    def switch(self):
        self.check_config_hashes()
        # Set before invoking Compose: an error can follow partial migrations.
        self.candidate_started = True
        atomic_json(self.backup / "switch-started.json", {"image": self.image, "source_sha": self.source_sha})
        atomic_json(self.overlay, {"services": {"sub2api": {"image": self.image, "environment": {"SUB2API_UPDATE_MODE": "image"}}}})
        self.command(self.compose([self.overlay]) + ["up", "-d", "--no-deps", "--pull", "never", "sub2api"], timeout=300)

    def verify_live(self):
        self.wait_health()
        current = self.container("sub2api")
        if current["Image"] != self.image_info["Id"] or current["Config"]["Image"] != self.image:
            raise DeployError("running container digest does not match candidate")
        labels = current["Config"].get("Labels") or {}
        if labels.get("org.opencontainers.image.revision") != self.source_sha:
            raise DeployError("running source revision does not match candidate")
        version = self.command(["docker", "exec", current["Id"], "/bin/sh", "-c", "exec /app/sub2api -version 2>&1"]).decode()
        if not re.search(r"commit: " + re.escape(self.source_sha) + r"(?:,|\))", version):
            raise DeployError("running binary revision does not match candidate")
        self.check_migrations(self.db["Id"], self.db_environment(), complete=True)
        return {"runtime_source_sha": self.source_sha, "runtime_image": self.image}

    def check_config_hashes(self):
        if any(file_sha256(path) != checksum for path, checksum in self.config_hashes.items()):
            raise DeployError("deployment configuration changed during this run")

    def wait_health(self):
        for _ in range(120):
            try:
                with urllib.request.urlopen(self.config["health_url"], timeout=3) as response:
                    if response.status == 200:
                        break
            except (OSError, ValueError):
                pass
            time.sleep(2)
        else:
            raise DeployError("dev health check failed after candidate switch")

    def save_success(self, proof):
        atomic_json(self.backup / "deployed.json", proof)
        atomic_json(self.state / "last-success.json", proof)

    def resume_old(self):
        self.command(["docker", "start", self.old["Id"]])
        self.wait_health()

    def quarantine_candidate(self):
        # Never reattach an old binary to a potentially migrated live database.
        self.command(self.compose([self.overlay]) + ["stop", "--timeout", "45", "sub2api"], timeout=60)

    def cleanup_preflight(self):
        for name in reversed(self.created_containers):
            try:
                info = self.docker_json("inspect", name)[0]
                if (info["Config"].get("Labels") or {}).get("pixcook.cicd.preflight") == self.run_id:
                    self.command(["docker", "rm", "-f", "-v", name], check=False)
            except DeployError:
                print("Preflight resource cleanup requires operator inspection.", file=sys.stderr)
        if self.created_network:
            try:
                network = self.docker_json("network", "inspect", self.created_network)[0]
                if (network.get("Labels") or {}).get("pixcook.cicd.preflight") == self.run_id:
                    self.command(["docker", "network", "rm", self.created_network], check=False)
            except DeployError:
                print("Preflight network cleanup requires operator inspection.", file=sys.stderr)
        if self.preflight_dir:
            shutil.rmtree(self.preflight_dir)

    def execute(self):
        try:
            self.inspect_target()
            self.pull_verify()
            self.stop_old()
            self.snapshot()
            self.preflight()
            self.switch()
            runtime = self.verify_live()
            proof = {"environment": "dev", "status": "deployed", "image": self.image, "source_sha": self.source_sha, "health": "ok", **runtime}
            self.save_success(proof)
            return proof
        except Exception:
            if self.candidate_started:
                self.quarantine_candidate()
                print("Candidate stopped; database may have migrated. Manual recovery is required; backup retained.", file=sys.stderr)
            elif self.old_stopped:
                self.resume_old()
                print("Pre-switch failure: original dev app restarted; live database was not migrated.", file=sys.stderr)
            raise
        finally:
            self.cleanup_preflight()


def file_sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--environment", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--source-sha", required=True)
    args = parser.parse_args(argv)
    try:
        validate_request(args.environment, args.image, args.source_sha)
        os.umask(0o077)
        def interrupted(signum, frame):
            raise DeployError("deployment interrupted")
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGINT, interrupted)
        config = load_config()
        directory = protected_directory(config["state_dir"])
        descriptor = os.open(directory / "deploy.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise DeployError("another dev deployment holds the lock") from error
            proof = Deployment(config, args.image, args.source_sha).execute()
        print(json.dumps(proof, sort_keys=True))
        return 0
    except DeployError as error:
        print("Dev deployment failed: " + str(error), file=sys.stderr)
        return 1
    except Exception:
        print("Dev deployment failed; command output and credentials suppressed. Inspect the protected deployment backup locally.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
