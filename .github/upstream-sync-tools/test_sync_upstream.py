import importlib.util
import subprocess
import json
import tempfile
import unittest
from pathlib import Path

SPEC = importlib.util.spec_from_file_location("sync_upstream", Path(__file__).with_name("sync_upstream.py"))
sync = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(sync)


class ReleaseSelectionTests(unittest.TestCase):
    def test_only_stable_non_draft_releases_from_requested_list_are_selected(self):
        rows = [
            {"tag_name": "v9.0.0-klno.1", "draft": True, "prerelease": False},
            {"tag_name": "v8.0.0-klno.1", "draft": False, "prerelease": True},
            {"tag_name": "v0.2.24-klno.1", "draft": False, "prerelease": False},
            {"tag_name": "v0.2.23-klno.4", "draft": False, "prerelease": False},
        ]
        self.assertEqual("v0.2.24-klno.1", sync.select_release(rows))
        with self.assertRaises(ValueError):
            sync.select_release(rows, "v8.0.0-klno.1")
        with self.assertRaises(ValueError):
            sync.select_release(rows, "--upload-pack=bad")

    def test_main_never_publishes_raw_upstream_tracking_branch(self):
        import inspect
        self.assertNotIn("push_immutable(", inspect.getsource(sync.main))

    def test_candidate_ref_is_bound_to_full_commit_identity(self):
        base, upstream = "a" * 40, "b" * 40
        self.assertEqual("pixcook-sync/" + base + "-" + upstream, sync.candidate_branch(base, upstream))
        with self.assertRaises(ValueError):
            sync.candidate_branch("main", upstream)


class RealGitMergeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.run_git("init", "-q", "-b", "base")
        self.run_git("config", "user.name", "Test")
        self.run_git("config", "user.email", "test@example.invalid")
        self.write("app.txt", "base\n")
        self.write(".github/workflows/sync-upstream.yml", "owned workflow\n")
        self.write(".github/upstream-baseline.json", json.dumps({"repository": "KlN-4096/sub2api", "tag": "v0.2.22-klno.1", "sha": "0" * 40}))
        self.commit("base")
        self.base = self.run_git("rev-parse", "HEAD")

    def run_git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True).strip()

    def write(self, path, text):
        p = self.root / path
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    def commit(self, message):
        self.run_git("add", ".")
        self.run_git("commit", "-qm", message)

    def test_merge_keeps_custom_code_and_workflows_but_contains_upstream_parent(self):
        self.run_git("switch", "-qc", "upstream")
        self.write("feature.txt", "upstream feature\n")
        self.write(".github/workflows/sync-upstream.yml", "destructive upstream workflow\n")
        self.write(".github/workflows/release.yml", "upstream automatic release\n")
        self.write(".github/workflows/new-unsafe.yml", "new upstream automation\n")
        self.commit("upstream")
        upstream = self.run_git("rev-parse", "HEAD")
        self.run_git("switch", "-q", "base")
        self.write("custom.txt", "Tibo\n")
        self.write(".github/workflows/sync-upstream.yml", "locally maintained workflow\n")
        self.write(".github/workflows/release.yml", "reviewed Pixcook release\n")
        self.commit("custom")
        base = self.run_git("rev-parse", "HEAD")
        result = sync.merge_candidate(self.root, base, upstream, "v0.2.24-klno.1")
        self.assertEqual("merged", result["status"])
        self.assertEqual("locally maintained workflow\n", (self.root / ".github/workflows/sync-upstream.yml").read_text())
        self.assertEqual("reviewed Pixcook release\n", (self.root / ".github/workflows/release.yml").read_text())
        self.assertFalse((self.root / ".github/workflows/new-unsafe.yml").exists())
        self.assertEqual("Tibo\n", (self.root / "custom.txt").read_text())
        self.assertEqual("upstream feature\n", (self.root / "feature.txt").read_text())
        self.assertEqual({"repository": "KlN-4096/sub2api", "tag": "v0.2.24-klno.1", "sha": upstream},
                         json.loads((self.root / ".github/upstream-baseline.json").read_text()))
        self.assertEqual([base, upstream], self.run_git("show", "-s", "--format=%P").split())
        self.assertIn(".github/workflows/sync-upstream.yml", result["preserved_automation"])
        self.assertEqual("", self.run_git("status", "--porcelain"))
        self.assertEqual(base, self.run_git("rev-parse", "base"))

    def test_application_conflict_aborts_without_changing_shared_branch(self):
        self.run_git("switch", "-qc", "upstream")
        self.write("app.txt", "upstream\n")
        self.commit("upstream")
        upstream = self.run_git("rev-parse", "HEAD")
        self.run_git("switch", "-q", "base")
        self.write("app.txt", "custom\n")
        self.commit("custom")
        base = self.run_git("rev-parse", "HEAD")
        result = sync.merge_candidate(self.root, base, upstream, "v0.2.24-klno.1")
        self.assertEqual("conflict", result["status"])
        self.assertEqual(["app.txt"], result["conflicts"])
        self.assertEqual(base, self.run_git("rev-parse", "base"))
        self.assertEqual(base, self.run_git("rev-parse", "HEAD"))
        self.assertEqual("", self.run_git("status", "--porcelain"))

    def test_dirty_checkout_is_not_modified(self):
        self.write("app.txt", "uncommitted custom work\n")
        with self.assertRaises(ValueError):
            sync.merge_candidate(self.root, self.base, self.base, "v0.2.23-klno.1")
        self.assertEqual("uncommitted custom work\n", (self.root / "app.txt").read_text())

    def test_already_integrated_is_noop(self):
        result = sync.merge_candidate(self.root, self.base, self.base, "v0.2.23-klno.1")
        self.assertEqual("up_to_date", result["status"])
        self.assertEqual(self.base, self.run_git("rev-parse", "HEAD"))


if __name__ == "__main__":
    unittest.main()
