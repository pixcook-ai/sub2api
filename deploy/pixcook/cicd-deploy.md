# Development deployment entry point

This entry point is deliberately bound to the existing development inventory.
Production is rejected before configuration reads or mutations. The production
workflow remains disabled separately. Do not install this tool on the production
host.

Install `deploy.py` and `validate.py` together under `/opt/pixcook-cicd`, owned by
root and not writable by the SSH deployment user. Install `cicd-dev.example.json`
as `/etc/pixcook-cicd/dev.json`, root:root mode 0600. Every parent directory and
the original Compose/environment files must also be root-owned without group
or world write permission. Keep the original private environment file intact.

The restricted SSH entry point accepts only:

```text
python3 /opt/pixcook-cicd/deploy.py --environment dev --image ghcr.io/pixcook-ai/sub2api@sha256:<64 lowercase hex> --source-sha <40 lowercase hex>
```

## Deployment boundary

1. Lock the fixed development state directory; validate the original Compose,
   running container labels, data mount, loopback port, database identity and
   existing TOTP state. A persisted key, when present, is read solely for
   validation, never written to the environment or rotated. An existing unset
   TOTP key stays unset; a fresh installation still requires a fixed key.
2. Pull the exact digest, check the OCI revision and binary `-version` commit.
   Read `/app/migration-checksums.json` without application startup or networking.
   Compare all applied database migration checksums before stopping the old app.
   Older first-deployment images may use a root-approved manifest installed as
   `/opt/pixcook-cicd/migrations/<source-sha>.json`; this is an operator-controlled
   exception, not fetched input. Keep an explicit tag on the old image.
3. Gracefully stop only the development application. Back up its mounted files,
   archived file ownership, original Compose/environment/overlay and runtime
   inventory, then create a custom-format database dump. Backups contain private
   account data and secrets and stay in the protected backup directory.
4. Restore that dump into a disposable database using the running database's
   exact image ID. Start a separate Redis using its running image ID and start
   the candidate with copied app files and replacement database/cache addresses.
   Every preflight container attaches only to a newly created internal network,
   without published ports or live mounts. Check health and the complete migrated
   checksum set before touching the live database with the candidate.
5. Write a persistent overlay containing only the image and
   `SUB2API_UPDATE_MODE=image`; run `compose up --no-deps --pull never sub2api`.
   Database/cache/other services, volumes, project and network definitions remain
   in the original Compose file. Verify health, running image ID/digest, OCI
   revision, binary revision and the complete live migration set.
6. Write protected deployment evidence and emit exactly the agreed proof JSON
   to stdout. Progress/failure messages use stderr. Remove only preflight
   resources carrying this run's label; never prune images or unrelated data.

Failure before the live switch restarts the original container and waits for
health. Once the live switch has begun, migration changes may exist even if
Compose exits unsuccessfully: the script stops the candidate and retains the
backup. It never runs old code against a potentially migrated database and never
automatically restores/down-migrates the database. Operator recovery must decide
whether database plus files may be restored together, considering any writes
already accepted. A process killed with SIGKILL or a host crash requires operator
inspection.

The first isolated restore and real deployment are operational acceptance gates;
the Python tests exercise command boundaries with an injected runner and do not
replace them. The parent deployment runbook owns actual host installation, SSH
restriction, remote acceptance and recovery decisions.
