import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from email.message import Message

SPEC = importlib.util.spec_from_file_location('deploy_dev', Path(__file__).with_name('deploy_dev.py'))
deploy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(deploy)
SHA = 'a' * 40
IMAGE = 'ghcr.io/pixcook-ai/sub2api@sha256:' + 'b' * 64
PROOF = dict(environment='dev', status='deployed', image=IMAGE, source_sha=SHA,
             health='ok', runtime_source_sha=SHA, runtime_image=IMAGE)
ENV = dict(DEV_SSH_HOST='dev.example.com', DEV_SSH_PORT='22', DEV_SSH_USER='pixcook-deploy',
           DEV_SSH_PRIVATE_KEY='private-key', DEV_SSH_KNOWN_HOSTS='known-host')


class Response(io.BytesIO):
    def __init__(self, body, content_type, url, status=200):
        super().__init__(body)
        self.status = status
        self.headers = Message()
        self.headers['Content-Type'] = content_type
        self.url = url

    def geturl(self):
        return self.url


class DeployTest(unittest.TestCase):
    def test_rejects_mutable_foreign_and_injected_references(self):
        for image in ['ghcr.io/pixcook-ai/sub2api:latest', IMAGE + '; id',
                      IMAGE.replace('pixcook-ai', 'other')]:
            with self.subTest(image=image), self.assertRaises(ValueError):
                deploy.validate_release(image, SHA)
        for sha in [SHA[:7], SHA + '\n', '$(id)']:
            with self.subTest(sha=sha), self.assertRaises(ValueError):
                deploy.validate_release(IMAGE, sha)

    def test_rejects_partial_wrong_and_failed_runtime_proofs(self):
        deploy.verify_proof(json.dumps(PROOF), IMAGE, SHA)
        for key in PROOF:
            proof = dict(PROOF)
            proof[key] = 'wrong'
            with self.subTest(key=key), self.assertRaises(ValueError):
                deploy.verify_proof(json.dumps(proof), IMAGE, SHA)
        with self.assertRaises(ValueError):
            deploy.verify_proof('{}', IMAGE, SHA)
        with self.assertRaises(ValueError):
            deploy.verify_proof('progress\n' + json.dumps(PROOF), IMAGE, SHA)

    def test_ssh_pins_host_key_and_runs_only_fixed_remote_command(self):
        result = subprocess.CompletedProcess([], 0, json.dumps(PROOF), '')
        def remote_command(cmd, **kwargs):
            identity = Path(cmd[cmd.index('-i') + 1])
            hosts = Path(next(arg.split('=', 1)[1] for arg in cmd if arg.startswith('UserKnownHostsFile=')))
            self.assertEqual(identity.read_text(), 'private-key\n')
            self.assertEqual(identity.stat().st_mode & 0o777, 0o600)
            self.assertEqual(hosts.read_text(), 'known-host\n')
            return result
        with patch.object(deploy.subprocess, 'run', side_effect=remote_command) as run, patch.object(deploy, 'check_public', return_value={'health': 'ok'}) as public:
            proof = deploy.deploy(IMAGE, SHA, ENV)
        self.assertEqual(proof['public_checks'], {'health': 'ok'})
        public.assert_called_once_with()
        args, kwargs = run.call_args
        cmd = args[0]
        self.assertIn('StrictHostKeyChecking=yes', cmd)
        self.assertIn('IdentitiesOnly=yes', cmd)
        self.assertIn('BatchMode=yes', cmd)
        self.assertEqual(cmd[-2], 'pixcook-deploy@dev.example.com')
        self.assertEqual(cmd[-1], 'python3 /opt/pixcook-cicd/deploy.py --environment dev --image ' + IMAGE + ' --source-sha ' + SHA)
        self.assertNotIn('shell', kwargs)
        self.assertTrue(kwargs['check'])

    def test_workflow_keeps_ci_source_digest_and_dev_boundary(self):
        root = Path(__file__).resolve().parents[2]
        workflow = (root / '.github/workflows/deploy-dev.yml').read_text()
        self.assertIn('uses: ./.github/workflows/pixcook-candidate.yml', workflow)
        self.assertIn('needs: candidate', workflow)
        self.assertIn('needs: image', workflow)
        self.assertIn('refs/heads/pixcook', workflow)
        self.assertIn('docker logout ghcr.io', workflow)
        self.assertIn('git ls-remote', workflow)
        self.assertNotIn('download-artifact', workflow)
        self.assertNotIn('PROD_SSH', workflow)
        production = (root / '.github/workflows/deploy-production.yml').read_text()
        self.assertIn('false && vars.PROD_DEPLOY_ENABLED', production)
        self.assertNotIn('secrets.', production)

    def test_invalid_ssh_settings_never_connect(self):
        for key, value in [('DEV_SSH_HOST', '-oProxyCommand=id'), ('DEV_SSH_PORT', '65536'),
                           ('DEV_SSH_USER', 'root'), ('DEV_SSH_KNOWN_HOSTS', '')]:
            env = dict(ENV, **{key: value})
            with self.subTest(key=key), patch.object(deploy.subprocess, 'run') as run:
                with self.assertRaises(ValueError):
                    deploy.deploy(IMAGE, SHA, env)
                run.assert_not_called()

    def test_failed_remote_command_propagates(self):
        with patch.object(deploy.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, ['ssh'])):
            with self.assertRaises(subprocess.CalledProcessError):
                deploy.deploy(IMAGE, SHA, ENV)

    def test_public_checks_run_only_after_valid_remote_proof_and_are_persisted(self):
        bad = subprocess.CompletedProcess([], 0, '{}', '')
        with patch.object(deploy.subprocess, 'run', return_value=bad), patch.object(deploy, 'check_public') as public:
            with self.assertRaises(ValueError):
                deploy.deploy(IMAGE, SHA, ENV)
            public.assert_not_called()
        proof = dict(PROOF, public_checks={'health': {'status': 'ok'}})
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'proof.json'
            with patch.object(deploy, 'deploy', return_value=proof), patch.object(deploy, 'Path', return_value=output), patch('sys.argv', ['deploy_dev.py', '--image', IMAGE, '--source-sha', SHA]), patch('builtins.print'):
                deploy.main()
            self.assertEqual(json.loads(output.read_text())['public_checks'], proof['public_checks'])

    def public_fixture(self, changes=None):
        fixtures = {
            '/health': (b'{"status":"ok"}', 'application/json', 200),
            '/': (b'<html><script type="module" src="/assets/app.js"></script><link rel="stylesheet" href="/assets/app.css"></html>', 'text/html', 200),
            '/admin': (b'<html><script src="/assets/app.js"></script></html>', 'text/html', 200),
            '/assets/app.js': (b'console.log("ready")', 'text/javascript', 200),
            '/assets/app.css': (b'body {color:black}', 'text/css', 200),
        }
        fixtures.update(changes or {})
        requested = []
        def open_response(request, timeout):
            url = request.full_url
            self.assertEqual(timeout, 20)
            self.assertTrue(url.startswith('https://dev.pixcook.ai/'))
            path = url.removeprefix('https://dev.pixcook.ai')
            requested.append(path)
            body, kind, status = fixtures[path]
            return Response(body, kind, url, status)
        return open_response, requested

    def test_public_checks_fetch_health_both_pages_and_referenced_assets(self):
        opener, requested = self.public_fixture()
        with patch.object(deploy, 'urlopen', side_effect=opener):
            checks = deploy.check_public()
        self.assertEqual(checks['health']['status'], 'ok')
        self.assertEqual(set(checks['pages']), {'/', '/admin'})
        self.assertEqual(set(requested), {'/health', '/', '/admin', '/assets/app.js', '/assets/app.css'})
        self.assertEqual(len(checks['assets']), 2)

    def test_public_checks_fail_on_unhealthy_pages_assets_or_foreign_asset(self):
        changes = [
            {'/health': (b'{"status":"failed"}', 'application/json', 200)},
            {'/health': (b'not json', 'application/json', 200)},
            {'/admin': (b'<html>error</html>', 'text/html', 502)},
            {'/': (b'login error', 'text/plain', 200)},
            {'/admin': (b'<html>empty</html>', 'text/html', 200)},
            {'/assets/app.js': (b'<html>fallback</html>', 'text/html', 200)},
            {'/assets/app.css': (b'', 'text/css', 200)},
            {'/': (b'<script src="https://other.invalid/app.js"></script>', 'text/html', 200)},
        ]
        for change in changes:
            opener, _ = self.public_fixture(change)
            with self.subTest(change=change), patch.object(deploy, 'urlopen', side_effect=opener):
                with self.assertRaises(ValueError):
                    deploy.check_public()


if __name__ == '__main__':
    unittest.main()
