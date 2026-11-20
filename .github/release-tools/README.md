# Pixcook release pipeline

The long-lived application branch is `pixcook` (overridable with the repository variable `PIXCOOK_INTEGRATION_BRANCH`). Upstream tracking and candidate PR creation are separate from image publication. Neither candidate CI nor the release workflow deploys a server or rewrites a shared branch.

## Publication boundaries

Only `vX.Y.Z-pixcook.N` tags can publish. The selected tag must resolve to the checked-out commit, and both that commit and the executing workflow revision must be ancestors of the current Pixcook integration branch. Manual runs default to `dry_run=true`; any branch/ref can be selected for a nonpublishing dry run. Publishing a nonintegration commit or an upstream `vX.Y.Z[-klno.N]` tag is rejected.

The source owns `.github/upstream-baseline.json`, containing exactly `repository`, `tag`, and full lowercase `sha`. The release planner accepts only `KlN-4096/sub2api`, verifies the SHA is an ancestor of the selected source, and the workflow fetches that exact remote tag to confirm the SHA. `release-source.json` records upstream and source provenance. Parent runbooks cover seeding the first baseline and reviewing subsequent sync PRs.

Release preparation resolves one source commit; candidate checks, frontend and all binary builds consume that same SHA. Candidate checks run full backend unit tests, real migrated integration tests (`CI=1` rejects missing services), Tibo/scheduling race tests, frontend validation/build and release/sync helper contracts. There is no production environment or registry write permission in candidate checks.

## Images and rollback references

GHCR publishes under `ghcr.io/<repository-owner-lowercase>/sub2api` (Pixcook: `ghcr.io/pixcook-ai/sub2api`). Full mode builds Linux amd64/arm64 and publishes a combined manifest; simple mode is amd64 only. The only top-level tags are the selected version and `sha-<full-source-sha>`. `latest`, major and minor tags are not updated. Existing tags are refused; network/authentication failures do not count as tag absence. This is a workflow guard, not a registry immutability policy: deploy by recorded digest and restrict package write access.

Each image carries OCI source/revision/version and upstream repository/tag/revision labels. `published-image-provenance` retains manifest digest metadata and source provenance for 90 days; full GitHub releases also contain `release-source.json`. Record the final manifest digest in the deployment manifest; a version tag alone is not a rollback record. Database compatibility and quality isolation must be checked before switching to an old image.

The frontend builds once; GoReleaser OSS runs one configured target per runner. Archive source SHA, version, target and checksum are verified before images are built. Builds use existing lockfiles and do not run `go mod tidy`. Upstream packaging configurations are reused with image generation disabled; runtime image construction consumes verified binaries. Full mode retains optional DockerHub support when its credentials exist. No release job modifies `main`, syncs VERSION back to a branch, sends notifications, or deploys production.

## Nonpublishing validation

```bash
rtk gh workflow run release.yml --ref <workflow-branch> \
  -f tag=<candidate-branch-or-sha> -f dry_run=true -f simple_release=true
```

Dry runs run the candidate checks and build verified archives and OCI runtime images without registry logins, image pushes or GitHub Release publication. Source provenance and build metadata are uploaded as `release-dry-run-report`; a local contract test is not evidence that this remote build has completed. Validate full mode separately when arm64 or desktop archives are needed.

Local helper checks:

```bash
rtk python -m pip install -r .github/release-tools/requirements-release.txt
rtk python -m unittest discover -s .github/release-tools -p 'test_*.py'
rtk bash -n .github/release-tools/release-images.sh
```
