#!/usr/bin/env python3
"""Create an upstream merge candidate without rewriting shared branches."""
import json
import os
import re
import subprocess
import sys
from pathlib import Path

UPSTREAM = "KlN-4096/sub2api"
SHA = re.compile(r"^[0-9a-f]{40}$")
STABLE_TAG = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:-klno\.(\d+))?$")


def select_release(rows, requested=""):
    stable = [r for r in rows if not r.get("draft") and not r.get("prerelease") and STABLE_TAG.fullmatch(r.get("tag_name", ""))]
    if requested:
        if not STABLE_TAG.fullmatch(requested) or not any(r["tag_name"] == requested for r in stable):
            raise ValueError("requested tag is not a published stable KlN release")
        return requested
    if not stable:
        raise ValueError("no published stable KlN release found")
    return max(stable, key=lambda r: tuple(int(x or 0) for x in STABLE_TAG.fullmatch(r["tag_name"]).groups()))["tag_name"]


def require_sha(sha):
    if not SHA.fullmatch(sha):
        raise ValueError("expected full lowercase commit SHA")
    return sha


def candidate_branch(base, upstream):
    return "pixcook-sync/" + require_sha(base) + "-" + require_sha(upstream)


def command(args, root=None, check=True):
    result = subprocess.run(args, cwd=root, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and result.returncode:
        # Avoid forwarding arbitrary upstream commit data or credentials into logs.
        raise RuntimeError("command failed: " + " ".join(args[:3]))
    return result


def git(root, *args, check=True):
    return command(["git", *args], root, check=check)


def merge_candidate(root, base, upstream, tag):
    require_sha(base)
    require_sha(upstream)
    if not STABLE_TAG.fullmatch(tag):
        raise ValueError("invalid stable release tag")
    if git(root, "status", "--porcelain").stdout.strip():
        raise ValueError("candidate checkout must be clean")
    git(root, "checkout", "--detach", base)
    if git(root, "merge-base", "--is-ancestor", upstream, base, check=False).returncode == 0:
        return {"status": "up_to_date", "base_sha": base, "upstream_sha": upstream}
    git(root, "config", "user.name", "pixcook-sync-bot")
    git(root, "config", "user.email", "pixcook-sync-bot@users.noreply.github.com")
    paths = git(root, "diff", "--name-only", base, upstream, "--", ".github").stdout.splitlines()
    merged = git(root, "merge", "--no-ff", "--no-commit", upstream, check=False)
    # Automation is locally owned: never install upstream sync/release jobs.
    # Restoring all .github paths also resolves conflicts in that scope.
    git(root, "restore", "--source=" + base, "--staged", "--worktree", "--", ".github")
    conflicts = git(root, "diff", "--name-only", "--diff-filter=U").stdout.splitlines()
    if conflicts or merged.returncode not in (0, 1):
        git(root, "merge", "--abort")
        return {"status": "conflict", "base_sha": base, "upstream_sha": upstream, "conflicts": conflicts,
                "preserved_automation": paths}
    baseline = Path(root) / ".github/upstream-baseline.json"
    baseline.write_text(json.dumps({"repository": UPSTREAM, "tag": tag, "sha": upstream}, indent=2) + "\n")
    git(root, "add", ".github/upstream-baseline.json")
    git(root, "commit", "-m", "Merge KlN upstream " + upstream)
    return {"status": "merged", "base_sha": base, "upstream_sha": upstream,
            "candidate_sha": git(root, "rev-parse", "HEAD").stdout.strip(), "preserved_automation": paths}


def gh_json(*args):
    return json.loads(command(["gh", *args]).stdout)


def write_report(report):
    Path("sync-report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a") as f:
            for key in ("status", "candidate_branch", "upstream_tag", "upstream_sha", "base_sha"):
                if key in report:
                    f.write(key + "=" + report[key] + "\n")


def main():
    root = Path.cwd()
    repository = os.environ["GITHUB_REPOSITORY"]
    integration = os.environ.get("INTEGRATION_BRANCH", "pixcook")
    git(root, "check-ref-format", "refs/heads/" + integration)
    if integration.startswith(("pixcook-sync/", "upstream-klno/")):
        raise ValueError("integration branch must be distinct from generated branches")
    releases = gh_json("api", "--paginate", "--slurp", "repos/" + UPSTREAM + "/releases?per_page=100")
    tag = select_release([release for page in releases for release in page], os.environ.get("REQUESTED_UPSTREAM_TAG", ""))
    git(root, "fetch", "--no-tags", "origin", "refs/heads/" + integration)
    base = git(root, "rev-parse", "FETCH_HEAD^{commit}").stdout.strip()
    # Exact remote tag only, no global tag mutation or untrusted ref expansion.
    git(root, "fetch", "--no-tags", "https://github.com/" + UPSTREAM + ".git", "refs/tags/" + tag)
    upstream = git(root, "rev-parse", "FETCH_HEAD^{commit}").stdout.strip()
    branch = candidate_branch(base, upstream)
    if integration == branch:
        raise ValueError("generated refs must be separate from the integration branch")
    report = merge_candidate(root, base, upstream, tag)
    report.update(upstream_tag=tag, upstream_repository=UPSTREAM, candidate_branch=branch,
                  integration_branch=integration)
    Path("upstream-automation.patch").write_text(git(root, "diff", base, upstream, "--", ".github").stdout)
    if report["status"] == "conflict":
        write_report(report)
        return 1
    # Never publish raw upstream trees as branch tips: their Actions may run.
    # The candidate merge parent and report preserve the exact upstream SHA.
    if report["status"] == "up_to_date":
        write_report(report)
        return 0
    # An immutable base/upstream pair has one candidate. Reuse existing PRs.
    existing = git(root, "ls-remote", "--heads", "origin", "refs/heads/" + branch).stdout.strip()
    if existing:
        remote_sha = existing.split()[0]
        git(root, "fetch", "--no-tags", "origin", "refs/heads/" + branch)
        if (git(root, "rev-parse", remote_sha + "^{tree}").stdout != git(root, "rev-parse", "HEAD^{tree}").stdout or
                git(root, "show", "-s", "--format=%P", remote_sha).stdout.split() != [base, upstream]):
            raise ValueError("existing candidate differs; refusing to overwrite")
        report["candidate_sha"] = remote_sha
    else:
        git(root, "push", "origin", "HEAD:refs/heads/" + branch)
    prs = gh_json("pr", "list", "--repo", repository, "--base", integration, "--head", branch, "--state", "all", "--json", "url,state")
    if prs:
        report["pull_request"] = prs[0]["url"]
    else:
        body = ("Merge stable KlN release `" + tag + "` (`" + upstream + "`) into `" + integration + "` at `" + base + "`.\n\n"
                "The integration branch is unchanged until this PR is reviewed and merged. No release or deployment is triggered by this sync job.\n\n"
                "Integration-owned `.github/` is preserved. Review upstream automation differences separately:\n" +
                "\n".join("- `" + p + "`" for p in report["preserved_automation"]) + "\n\n"
                "Run the full integration CI and staging checks before merge; this job does not execute candidate code.\n")
        Path("sync-pr-body.md").write_text(body)
        report["pull_request"] = command(["gh", "pr", "create", "--repo", repository, "--base", integration,
                                         "--head", branch, "--title", "Sync KlN " + tag, "--body-file", "sync-pr-body.md"]).stdout.strip()
    write_report(report)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, KeyError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
