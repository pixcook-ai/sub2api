import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('ssh_entry', Path(__file__).with_name('ssh_entry.py'))
entry = importlib.util.module_from_spec(spec)
spec.loader.exec_module(entry)


class RestrictedEntryTests(unittest.TestCase):
    def command(self, environment='dev'):
        return 'python3 /opt/pixcook-cicd/deploy.py --environment ' + environment + ' --image ghcr.io/pixcook-ai/sub2api@sha256:' + 'a' * 64 + ' --source-sha ' + 'b' * 40

    def test_development_command_is_accepted(self):
        self.assertEqual(entry.parse_command(self.command()), ['--environment', 'dev', '--image', 'ghcr.io/pixcook-ai/sub2api@sha256:' + 'a' * 64, '--source-sha', 'b' * 40])

    def test_production_and_arbitrary_shell_are_rejected(self):
        for command in [self.command('production'), '', 'bash', 'cat /etc/passwd', self.command() + '; id', self.command() + ' --extra bad', self.command().replace('sha256:', 'latest-'), self.command().replace('/opt/pixcook-cicd/deploy.py', '/tmp/deploy.py')]:
            with self.subTest(command=command), self.assertRaises(ValueError):
                entry.parse_command(command)


if __name__ == '__main__':
    unittest.main()
