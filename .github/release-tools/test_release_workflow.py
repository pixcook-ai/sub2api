"""Release boundaries: a candidate cannot silently become a deployment."""
from pathlib import Path
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]


class ReleaseWorkflowTest(unittest.TestCase):
    def test_publication_requires_candidate_checks_and_pixcook_tag(self):
        text = (ROOT / '.github/workflows/release.yml').read_text()
        workflow = yaml.load(text, Loader=yaml.BaseLoader)
        self.assertEqual(workflow['on']['push']['tags'], ['v*-pixcook.*'])
        self.assertEqual(workflow['on']['workflow_dispatch']['inputs']['dry_run']['default'], 'true')
        self.assertIn('candidate-checks', workflow['jobs']['release']['needs'])
        self.assertEqual(workflow['jobs']['candidate-checks']['with']['source_sha'], '${{ needs.prepare.outputs.sha }}')
        self.assertIn('git merge-base --is-ancestor HEAD FETCH_HEAD', text)
        self.assertIn('git merge-base --is-ancestor "$WORKFLOW_SHA" FETCH_HEAD', text)
        self.assertNotIn('git push', text)
        self.assertNotIn('sync-version-file:', text)
        for job in workflow['jobs'].values():
            for step in job.get('steps', []):
                if step.get('uses', '').startswith('actions/checkout@'):
                    self.assertEqual(step['with']['persist-credentials'], 'false')

    def test_candidate_checks_do_not_publish_or_skip_missing_integration_services(self):
        text = (ROOT / '.github/workflows/pixcook-candidate.yml').read_text()
        workflow = yaml.load(text, Loader=yaml.BaseLoader)
        self.assertEqual(workflow['permissions'], {'contents': 'read'})
        backend = workflow['jobs']['backend']
        tests = next(step for step in backend['steps'] if step.get('name', '').startswith('Full unit'))
        self.assertEqual(tests['env']['CI'], '1')
        self.assertIn('-tags=unit ./...', tests['run'])
        self.assertIn('-tags=integration ./...', tests['run'])
        self.assertNotIn('packages: write', text)
        self.assertNotIn('docker push', text)
        self.assertNotIn('environment: production', text)


if __name__ == '__main__':
    unittest.main()
