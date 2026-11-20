# KlN upstream candidate sync

`sync-upstream.yml` selects only a published, non-draft, non-prerelease stable release from **KlN-4096/sub2api**. The version pattern accepts `vX.Y.Z` and `vX.Y.Z-klno.N`. An explicit tag must also identify a published stable release. The fetched remote tag is peeled to its exact commit SHA; local tags are never rewritten.

Repository setup:

- Create the long-lived integration branch `pixcook` from the reviewed local integration commit. Optional variable `PIXCOOK_INTEGRATION_BRANCH` changes that name.
- Configure `SYNC_GITHUB_TOKEN` as a dedicated fine-grained token scoped to this repository, with Contents, Pull requests and Workflows read/write. Merge parents introduce upstream workflow history; normal GITHUB_TOKEN cannot always push that history. Missing credentials stop the job before any mutation.
- Raw upstream commits are fetched locally only. The candidate merge parent, baseline JSON and report retain the exact upstream SHA; raw upstream branch tips are never pushed, so their inherited push workflows cannot be activated by this job.
- Install this workflow on the repository default branch so GitHub runs the schedule. Do not install the old destructive upstream sync workflow. Confirm the repository allows the sync credential to open PRs and that protection rules accept generated branch names.

The job builds a normal merge from one fixed integration commit, retains the integration tree under `.github/` except for updating `.github/upstream-baseline.json` to the selected upstream repository/tag/peeled SHA, and pushes `pixcook-sync/<full base SHA>-<full upstream SHA>`. It opens a PR against the configured integration branch. Existing candidate PRs are reused; divergent existing refs are rejected, never overwritten. The integration branch, default branch and release tags are never pushed by this job. Closed PRs stay closed on rerun; a reviewer may reopen one or generate a new candidate after the base changes.

All non-automation merge conflicts abort the local merge; no candidate branch is pushed. The uploaded `sync-report.json` lists conflicts. `upstream-automation.patch` records `.github/` differences for separate review; preserved automation must not be mistaken for all upstream CI changes having been adopted.

The job runs trusted tooling from the workflow commit and its offline real-Git tests. It does **not** execute upstream candidate code under write credentials and does not trigger a release or deployment. Candidate PR CI plus the integration/staging acceptance remains required before merging. A dedicated token makes candidate pushes eligible to trigger ordinary repository PR workflows; keep those workflows free of privileged untrusted execution.

Tests: `rtk python3 -m unittest discover -s .github/upstream-sync-tools -p 'test_*.py'`.
