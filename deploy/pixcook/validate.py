#!/usr/bin/env python3
"""Validate rendered Compose without printing credentials or starting containers."""
import argparse
import copy
import json
import re
import subprocess
import sys

IMAGE = re.compile(r"ghcr\.io/pixcook-ai/sub2api@sha256:[0-9a-f]{64}\Z")
IDENTITY = ("JWT_SECRET", "TOTP_ENCRYPTION_KEY", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "DATABASE_SSLMODE", "REDIS_HOST", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB")


def validate(model, mode, baseline=None):
    errors = []
    services = model.get("services", {})
    app = services.get("sub2api", {})
    env = app.get("environment", {})
    if not IMAGE.fullmatch(app.get("image", "")):
        errors.append("sub2api image must be a Pixcook GHCR sha256 digest")
    if env.get("SUB2API_UPDATE_MODE") != "image":
        errors.append("SUB2API_UPDATE_MODE must be image")
    for key in ("JWT_SECRET", "TOTP_ENCRYPTION_KEY", "DATABASE_PASSWORD"):
        value = str(env.get(key) or "")
        if key == "TOTP_ENCRYPTION_KEY" and mode == "existing" and baseline is not None and not value and not baseline.get("services", {}).get("sub2api", {}).get("environment", {}).get(key):
            # Preserve an existing unconfigured feature without creating identity.
            continue
        if len(value) < (32 if key != "DATABASE_PASSWORD" else 1) or value.startswith("CHANGE_ME"):
            errors.append(key + " must be explicitly configured")
    ports = app.get("ports", [])
    if not ports or any(port.get("host_ip") not in ("127.0.0.1", "::1") for port in ports):
        errors.append("sub2api must publish only on an explicit loopback address")
    if not any(v.get("target") == "/app/data" and v.get("type") in ("volume", "bind") for v in app.get("volumes", [])):
        errors.append("sub2api /app/data must be persistent")
    if mode == "fresh":
        for name, target in (("postgres", "/var/lib/postgresql/data"), ("redis", "/data")):
            service = services.get(name, {})
            if service.get("ports"):
                errors.append(name + " must not publish host ports")
            if not any(v.get("target") == target and v.get("type") in ("volume", "bind") for v in service.get("volumes", [])):
                errors.append(name + " storage must be persistent")
        if any(s.get("container_name") for s in services.values()):
            errors.append("fresh services must use project-scoped container names")
    elif mode == "existing":
        if baseline is None:
            errors.append("existing deployment requires its original Compose baseline")
            return errors
        old_services = baseline.get("services", {})
        old_app = old_services.get("sub2api", {})
        old_env = old_app.get("environment", {})
        # The existing-site overlay is intentionally limited to image/update
        # transport. Preserve every other app setting, not just DB identity.
        current_app = copy.deepcopy(app)
        previous_app = copy.deepcopy(old_app)
        for value in (current_app, previous_app):
            value.pop("image", None)
            value.get("environment", {}).pop("SUB2API_UPDATE_MODE", None)
        if current_app != previous_app:
            errors.append("application settings changed beyond image/update mode")
        for key in IDENTITY:
            if env.get(key) != old_env.get(key):
                errors.append(key + " changed from the original deployment")
        if app.get("volumes", []) != old_app.get("volumes", []):
            errors.append("sub2api data mounts changed from the original deployment")
        for key in ("volumes", "networks", "secrets", "configs", "name"):
            if model.get(key) != baseline.get(key):
                errors.append("Compose " + key + " changed from the original deployment")
        if set(services) != set(old_services):
            errors.append("deployment service set changed from the original deployment")
        for name, service in old_services.items():
            if name != "sub2api" and services.get(name) != service:
                errors.append(name + " changed; preserve database/cache versions and configuration")
    else:
        errors.append("unsupported validation mode")
    return errors


def render(files, env_file=None, project_name=None):
    command = ["docker", "compose"]
    if env_file:
        command += ["--env-file", env_file]
    if project_name:
        command += ["--project-name", project_name]
    for path in files:
        command += ["-f", path]
    command += ["config", "--format", "json"]
    result = subprocess.run(command, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError("Compose could not be rendered; inspect required variable names locally (credentials suppressed)")
    return json.loads(result.stdout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("fresh", "existing"), required=True)
    parser.add_argument("--compose-file", action="append", required=True)
    parser.add_argument("--baseline-file", action="append", help="Original Compose files, without the Pixcook override")
    parser.add_argument("--env-file")
    parser.add_argument("--project-name", help="Existing deployment: use its original Compose project name")
    args = parser.parse_args()
    if args.mode == "existing" and not args.baseline_file:
        parser.error("--baseline-file is required for existing deployments")
    try:
        model = render(args.compose_file, args.env_file, args.project_name)
        baseline = render(args.baseline_file, args.env_file, args.project_name) if args.baseline_file else None
        errors = validate(model, args.mode, baseline)
    except (RuntimeError, OSError, ValueError):
        print("Deployment validation failed: cannot render Compose; no credentials were printed", file=sys.stderr)
        return 2
    if errors:
        for error in errors:
            print("ERROR: " + error, file=sys.stderr)
        return 1
    print("Deployment configuration validated; no containers started and no credentials printed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
