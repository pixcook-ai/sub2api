#!/usr/bin/env python3
"""Launch an isolated, real Sub2API server for local SDK conformance."""

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
from urllib.error import URLError
from urllib.request import urlopen
from urllib.parse import urlsplit


def loopback_url(value, field):
    try:
        parsed = urlsplit(value)
        address = ipaddress.ip_address(parsed.hostname or "")
        valid = parsed.scheme == "http" and address.is_loopback and parsed.port
    except (ValueError, TypeError):
        valid = False
    if not valid or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError(f"{field} must be an HTTP literal loopback URL with an explicit port")
    return value.rstrip("/")


def private_file(value, field):
    path = Path(value).expanduser().absolute()
    mode = path.lstat().st_mode
    if not stat.S_ISREG(mode) or stat.S_IMODE(mode) & 0o077:
        raise ValueError(f"{field} must be a private regular file (mode 0600)")
    return path.resolve(strict=True)


def gateway_inputs(manifest_path):
    value = json.loads(private_file(manifest_path, "gateway_manifest").read_text())
    if value.get("contract_version") != 1 or value.get("source") != "local":
        raise ValueError("gateway manifest must be a version 1 local fixture")
    if value.get("seam") != "gateway-diagnostic":
        raise ValueError("gateway manifest must describe the owned diagnostic fixture")
    gateway = loopback_url(value.get("gateway_base_url"), "gateway_base_url")
    if urlsplit(gateway).path not in ("", "/v1"):
        raise ValueError("gateway_base_url must use the root or /v1 path")
    metrics = loopback_url(value.get("bps_metrics_url"), "bps_metrics_url")
    key = private_file(value.get("api_key_file"), "api_key_file")
    return value | {"gateway_base_url": gateway, "bps_metrics_url": metrics, "api_key_file": str(key)}


def command(*args, cwd=None, env=None):
    result = subprocess.run(["rtk", "proxy", *map(str, args)], cwd=cwd, env=env,
                            text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed; exit={result.returncode}: {result.stderr[-1500:]}")
    return result.stdout.strip()


def private_write(path, value):
    payload = json.dumps(value, ensure_ascii=False, indent=2).encode() + b"\n"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(payload)
    return path.stat().st_ino


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def process_environment(data_dir):
    allowed = {"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "GOCACHE", "GOPATH", "GOTOOLCHAIN", "DOCKER_HOST", "DOCKER_CONTEXT"}
    environment = {k: v for k, v in os.environ.items() if k in allowed}
    environment.update(DATA_DIR=str(data_dir), CONFIG_FILE=str(data_dir / "config.yaml"), RUN_MODE="standard", TZ="UTC")
    return environment


def wait_until(description, predicate, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {description}")


def docker_ready(container, *probe):
    result = subprocess.run(["rtk", "proxy", "docker", "exec", container, *probe],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return result.returncode == 0


def mapped_port(container, target):
    binding = command("docker", "port", container, target).splitlines()[0]
    if not binding.startswith("127.0.0.1:"):
        raise RuntimeError("container port was not bound exclusively to loopback")
    return int(binding.rsplit(":", 1)[1])


def healthy(server, port):
    if server.poll() is not None:
        raise RuntimeError("real main server exited; inspect the private main.log")
    try:
        with urlopen(f"http://127.0.0.1:{port}/health", timeout=1) as response:
            return response.status == 200
    except (OSError, URLError):
        return False


def stop_process_group(process):
    if process is None:
        return
    # A group can outlive its leader; always signal the owned process group.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        pass
    # The leader exiting does not prove its children exited. Reap stragglers.
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait(timeout=5)


def source_state(repository):
    return {
        "commit": command("git", "rev-parse", "HEAD", cwd=repository),
        "tracked_dirty": bool(command("git", "status", "--porcelain", "--untracked-files=no", cwd=repository)),
        "source_dirty": bool(command("git", "status", "--porcelain", "--untracked-files=all", cwd=repository)),
    }


def binary_digest(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def build_gateway_fixture(repository, data_dir):
    module = repository / "bps-local"
    binary = data_dir / "gateway-fixture"
    with tempfile.TemporaryDirectory(prefix="sdk-fixture-", dir=module) as source_dir:
        source = Path(source_dir) / "main.go"
        shutil.copyfile(repository / "scripts/sdk-conformance/gateway_fixture.go.txt", source)
        command("go", "build", "-mod=vendor", "-o", binary, source, cwd=module)
    return binary


def launch(args, gateway):
    repository = Path(args.main_repo).resolve(strict=True)
    if not (repository / "backend/cmd/server/main.go").is_file():
        raise ValueError("main-repo is not a Sub2API source checkout")
    output = Path(args.out).expanduser().absolute()
    if not output.parent.is_dir():
        raise ValueError("output manifest parent directory must exist")
    data_dir = Path(tempfile.mkdtemp(prefix="bps-sdk-main-"))
    data_dir.chmod(0o700)
    run_id = secrets.token_hex(8)
    containers = []
    server = None
    gateway_process = None
    gateway_log = None
    output_inode = None
    stop = False
    restart_requested = False

    def stopping(_signum, _frame):
        nonlocal stop
        stop = True

    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, stopping)
    def restarting(_signum, _frame):
        nonlocal restart_requested
        restart_requested = True
    signal.signal(signal.SIGUSR1, restarting)
    try:
        private_write(data_dir / ".bps-sdk-fixture.json", {"contract_version": 1, "run_id": run_id})
        if args.gateway_repo:
            gateway_repository = Path(args.gateway_repo).resolve(strict=True)
            fixture_script = gateway_repository / "scripts/sdk-conformance/fixture.py"
            if not fixture_script.is_file():
                raise ValueError("gateway-repo has no SDK fixture launcher")
            gateway_manifest = data_dir / "gateway.json"
            gateway_log = (data_dir / "gateway.log").open("w")
            os.chmod(gateway_log.name, 0o600)
            gateway_source = source_state(gateway_repository)
            gateway_binary = build_gateway_fixture(gateway_repository, data_dir)
            gateway_process = subprocess.Popen([sys.executable, str(fixture_script), "--binary", str(gateway_binary), "--out", str(gateway_manifest)], stdout=gateway_log, stderr=subprocess.STDOUT, start_new_session=True, env=process_environment(data_dir))
            def gateway_ready():
                if gateway_process.poll() is not None:
                    raise RuntimeError("owned gateway fixture exited; inspect private gateway.log")
                return gateway_manifest.exists()
            wait_until("owned gateway fixture", gateway_ready)
            gateway = gateway_inputs(gateway_manifest)
            gateway_after_build = source_state(gateway_repository)
            gateway["gateway_commit"] = gateway_source["commit"]
            gateway["gateway_tracked_dirty"] = gateway_source["tracked_dirty"] or gateway_after_build["tracked_dirty"]
            gateway["gateway_source_clean"] = not gateway_source["source_dirty"] and gateway_after_build == gateway_source
            gateway["gateway_binary_sha256"] = binary_digest(gateway_binary)
            # Replace only our private diagnostic manifest, before the seed reads it.
            gateway_manifest.write_text(json.dumps(gateway))
            args.gateway_manifest = str(gateway_manifest)
        database_name = "bps_sdk_" + run_id
        password = secrets.token_hex(24)
        for kind, image, inner_port, extra in [
            ("pg", args.postgres_image, 5432, ["-e", "POSTGRES_USER=postgres", "-e", f"POSTGRES_PASSWORD={password}", "-e", f"POSTGRES_DB={database_name}"]),
            ("redis", args.redis_image, 6379, []),
        ]:
            # Never pull mutable or absent images behind a test run.
            command("docker", "image", "inspect", image)
            name = f"bps-sdk-{run_id}-{kind}"
            container = command("docker", "run", "--rm", "-d", "--name", name,
                                "--label", f"com.pixcook.bps.sdk.fixture={run_id}",
                                "-p", f"127.0.0.1::{inner_port}", *extra, image)
            containers.append(container)
        pg, redis = containers
        wait_until("isolated PostgreSQL", lambda: docker_ready(pg, "pg_isready", "-U", "postgres", "-d", database_name))
        wait_until("isolated Redis", lambda: docker_ready(redis, "redis-cli", "ping"))
        main_port = free_port()
        configuration = {
            "run_mode": "standard", "timezone": "UTC",
            "server": {"host": "127.0.0.1", "port": main_port, "mode": "release"},
            "database": {"host": "127.0.0.1", "port": mapped_port(pg, "5432/tcp"), "user": "postgres", "password": password, "dbname": database_name, "sslmode": "disable", "max_open_conns": 20, "max_idle_conns": 5},
            "redis": {"host": "127.0.0.1", "port": mapped_port(redis, "6379/tcp"), "db": 0},
            "jwt": {"secret": secrets.token_hex(32), "expire_hour": 1},
            "security": {"url_allowlist": {"enabled": False, "allow_insecure_http": True}},
            "pricing": {"remote_url": "", "hash_url": "", "data_dir": str(data_dir / "pricing"), "fallback_file": str(repository / "backend/resources/model-pricing/model_prices_and_context_window.json")},
            "gateway": {"files": {"enabled": True, "root": str(data_dir / "files"), "max_file_bytes": 209715200, "user_quota_bytes": 2147483648, "global_quota_bytes": 4294967296, "free_space_reserve_bytes": 1, "lease_seconds": 300}},
        }
        # JSON is valid YAML, so no additional configuration serializer is needed.
        private_write(data_dir / "config.yaml", configuration)
        environment = process_environment(data_dir)
        main_source = source_state(repository)
        commit = main_source["commit"]
        main_tracked_dirty = main_source["tracked_dirty"]
        main_binary = Path(args.main_binary).resolve(strict=True) if args.main_binary else data_dir / "sub2api"
        seed_binary = Path(args.seed_binary).resolve(strict=True) if args.seed_binary else data_dir / "sdk-fixture-seed"
        backend = repository / "backend"
        if not args.seed_binary:
            command("go", "build", "-o", seed_binary, "./cmd/sdk-fixture-seed", cwd=backend)
        if not args.main_binary:
            command("go", "build", "-ldflags", f"-X main.Commit={commit}", "-o", main_binary, "./cmd/server", cwd=backend)
        main_after_build = source_state(repository)
        seed_manifest = data_dir / "seed.json"
        with (data_dir / "seed.log").open("w") as log:
            os.chmod(log.name, 0o600)
            result = subprocess.run(["rtk", "proxy", str(seed_binary), "--fixture-dir", str(data_dir),
                                     "--gateway-manifest", str(Path(args.gateway_manifest).resolve()),
                                     "--model", args.model, "--main-commit", commit,
                                     "--fixture-out", str(seed_manifest)], env=environment,
                                    stdout=log, stderr=subprocess.STDOUT)
        if result.returncode:
            raise RuntimeError("fixture seed failed; inspect private seed.log")
        def start_main():
            with (data_dir / "main.log").open("a") as log:
                os.chmod(log.name, 0o600)
                return subprocess.Popen([str(main_binary)], env=environment, stdout=log,
                                        stderr=subprocess.STDOUT, start_new_session=True, cwd=backend)
        server = start_main()
        wait_until("real main HTTP health (not scheduler acceptance)", lambda: healthy(server, main_port))
        restart_path = data_dir / "main-restart.json"
        restart_count = 0
        private_write(restart_path, {"restart_count": restart_count, "main_pid": server.pid})
        manifest = json.loads(seed_manifest.read_text())
        manifest.update({
            "main_tracked_dirty": main_tracked_dirty,
            "gateway_tracked_dirty": gateway.get("gateway_tracked_dirty"),
            "main_source_clean": not main_source["source_dirty"] and main_after_build == main_source,
            "gateway_source_clean": gateway.get("gateway_source_clean", False),
            "source_provenance_verified": not (args.main_binary or args.seed_binary) and bool(args.gateway_repo) and not main_source["source_dirty"] and main_after_build == main_source and gateway.get("gateway_source_clean") is True,
            "binary_source": "prebuilt-diagnostic" if args.main_binary or args.seed_binary else "launcher-built",
            "main_binary_sha256": binary_digest(main_binary),
            "seed_binary_sha256": binary_digest(seed_binary),
            "gateway_binary_sha256": gateway.get("gateway_binary_sha256"),
            "container_images": {"postgres": args.postgres_image, "redis": args.redis_image},
            "launcher_pid": os.getpid(), "main_pid": server.pid,
            "main_restart_file": str(restart_path),
            "readiness": "http-health-only; SDK scheduling must still pass",
        })
        output_inode = private_write(output, manifest)
        print(str(output), flush=True)
        while not stop:
            if restart_requested:
                old_pid = server.pid
                stop_process_group(server)
                server = start_main()
                wait_until("restarted real main HTTP health", lambda: healthy(server, main_port))
                restart_count += 1
                snapshot = data_dir / "main-restart-next.json"
                private_write(snapshot, {"restart_count": restart_count, "previous_main_pid": old_pid, "main_pid": server.pid})
                snapshot.replace(restart_path)
                restart_requested = False
            if server.poll() is not None:
                raise RuntimeError("real main server stopped before fixture shutdown")
            time.sleep(0.25)
    finally:
        cleanup_errors = []
        def cleanup(label, action):
            try:
                action()
            except Exception as error:
                cleanup_errors.append(f"{label}: {type(error).__name__}")
        cleanup("main process", lambda: stop_process_group(server))
        for container in reversed(containers):
            cleanup("container", lambda container=container: subprocess.run(["rtk", "proxy", "docker", "rm", "-f", container],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True))
        cleanup("gateway process", lambda: stop_process_group(gateway_process))
        if gateway_log:
            cleanup("gateway log", gateway_log.close)
        def remove_owned_output():
            try:
                info = output.lstat()
            except FileNotFoundError:
                return
            if output_inode is not None and info.st_ino == output_inode:
                output.unlink()
        cleanup("output manifest", remove_owned_output)
        if args.keep_private_logs:
            # Synthetic fixture credentials remain here; caller explicitly owns cleanup.
            print(f"private fixture logs retained: {data_dir}", file=sys.stderr)
        else:
            cleanup("private directory", lambda: shutil.rmtree(data_dir))
        if cleanup_errors:
            print("fixture cleanup incomplete: " + "; ".join(cleanup_errors), file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    gateway_group = parser.add_mutually_exclusive_group(required=True)
    gateway_group.add_argument("--gateway-manifest")
    gateway_group.add_argument("--gateway-repo")
    parser.add_argument("--out", required=True)
    parser.add_argument("--model", default="gpt-6-astra")
    parser.add_argument("--main-repo", default=str(Path(__file__).resolve().parents[2]))
    parser.add_argument("--main-binary")
    parser.add_argument("--seed-binary")
    parser.add_argument("--postgres-image", default="postgres:18.1-alpine3.23")
    parser.add_argument("--redis-image", default="redis:8.4-alpine")
    parser.add_argument("--keep-private-logs", action="store_true")
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()
    try:
        gateway = gateway_inputs(args.gateway_manifest) if args.gateway_manifest else None
        if args.gateway_repo and not (Path(args.gateway_repo) / "scripts/sdk-conformance/fixture.py").is_file():
            raise ValueError("gateway-repo has no SDK fixture launcher")
        if Path(args.out).exists():
            raise ValueError("output manifest already exists; refuse to overwrite")
        if args.validate_only:
            print("validated local fixture inputs")
            return 0
        launch(args, gateway)
        return 0
    except (OSError, ValueError, TypeError, RuntimeError, json.JSONDecodeError) as error:
        print(str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
