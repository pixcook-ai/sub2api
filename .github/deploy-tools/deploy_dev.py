"""Send one validated digest to the restricted development deploy command."""
import argparse
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.parse import urljoin, urlsplit
from urllib.request import Request, urlopen

PUBLIC_ORIGIN = 'https://dev.pixcook.ai'


class PageAssets(HTMLParser):
    def __init__(self):
        super().__init__()
        self.assets = set()

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == 'script' and attrs.get('src'):
            self.assets.add((attrs['src'], 'js'))
        if tag == 'link' and 'stylesheet' in attrs.get('rel', '').lower().split() and attrs.get('href'):
            self.assets.add((attrs['href'], 'css'))


def public_response(url, kinds):
    request = Request(url, headers={'User-Agent': 'Pixcook-Deployment-Check', 'Cache-Control': 'no-cache'})
    with urlopen(request, timeout=20) as response:
        kind = response.headers.get_content_type()
        if response.status != 200 or response.geturl() != url or kind not in kinds:
            raise ValueError('unexpected public response: ' + url)
        body = response.read(8 * 1024 * 1024 + 1)
        if not body or len(body) > 8 * 1024 * 1024:
            raise ValueError('empty or oversized public response: ' + url)
        return body, kind


def check_public():
    body, _ = public_response(PUBLIC_ORIGIN + '/health', {'application/json'})
    health = json.loads(body)
    if not isinstance(health, dict) or health.get('status') != 'ok':
        raise ValueError('public health is not ok')
    checks = {'health': {'status': 'ok', 'http_status': 200}, 'pages': {}, 'assets': {}}
    for path in ('/', '/admin'):
        page_url = PUBLIC_ORIGIN + path
        body, _ = public_response(page_url, {'text/html'})
        parser = PageAssets()
        parser.feed(body.decode('utf-8'))
        if not any(kind == 'js' for _, kind in parser.assets):
            raise ValueError('public page has no application script: ' + path)
        checks['pages'][path] = {'http_status': 200, 'content_type': 'text/html'}
        for reference, kind in sorted(parser.assets):
            url = urljoin(page_url, reference)
            parsed = urlsplit(url)
            if parsed.scheme != 'https' or parsed.netloc != 'dev.pixcook.ai' or parsed.fragment:
                raise ValueError('unexpected asset origin: ' + path)
            if url in checks['assets']:
                continue
            allowed = {'text/css'} if kind == 'css' else {'text/javascript', 'application/javascript', 'application/x-javascript'}
            _, content_type = public_response(url, allowed)
            checks['assets'][url] = {'http_status': 200, 'content_type': content_type}
    return checks


def validate_release(image, source_sha):
    if not re.fullmatch(r'ghcr\.io/pixcook-ai/sub2api@sha256:[0-9a-f]{64}', image):
        raise ValueError('expected a Pixcook image digest')
    if not re.fullmatch(r'[0-9a-f]{40}', source_sha):
        raise ValueError('expected a full source SHA')


def verify_proof(output, image, source_sha):
    proof = json.loads(output)
    expected = dict(environment='dev', status='deployed', image=image, source_sha=source_sha,
                    health='ok', runtime_source_sha=source_sha, runtime_image=image)
    if not isinstance(proof, dict) or any(proof.get(k) != v for k, v in expected.items()):
        raise ValueError('remote runtime proof does not match this deployment')
    return proof


def deploy(image, source_sha, env):
    validate_release(image, source_sha)
    host = env.get('DEV_SSH_HOST', '')
    port = env.get('DEV_SSH_PORT', '22') or '22'
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]{0,252}', host):
        raise ValueError('invalid SSH host')
    if not port.isascii() or not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise ValueError('invalid SSH port')
    if env.get('DEV_SSH_USER') != 'pixcook-deploy':
        raise ValueError('development deploy requires the restricted user')
    key = env.get('DEV_SSH_PRIVATE_KEY', '')
    known_hosts = env.get('DEV_SSH_KNOWN_HOSTS', '')
    if not key.strip() or not known_hosts.strip():
        raise ValueError('SSH identity and pinned known hosts are required')
    with tempfile.TemporaryDirectory(prefix='pixcook-deploy-') as directory:
        identity = Path(directory) / 'identity'
        hosts = Path(directory) / 'known_hosts'
        identity.write_text(key.rstrip() + '\n')
        identity.chmod(0o600)
        hosts.write_text(known_hosts.rstrip() + '\n')
        command = ('python3 /opt/pixcook-cicd/deploy.py --environment dev --image ' + image
                   + ' --source-sha ' + source_sha)
        result = subprocess.run([
            'ssh', '-F', '/dev/null', '-i', str(identity), '-p', port,
            '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile=' + str(hosts),
            '-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=15',
            '-o', 'ServerAliveCountMax=3', 'pixcook-deploy@' + host, command,
        ], check=True, capture_output=True, text=True, timeout=900)
    proof = verify_proof(result.stdout, image, source_sha)
    proof['public_checks'] = check_public()
    return proof


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--image', required=True)
    parser.add_argument('--source-sha', required=True)
    args = parser.parse_args()
    proof = deploy(args.image, args.source_sha, os.environ)
    Path('dev-deployment-proof.json').write_text(json.dumps(proof, indent=2) + '\n')
    print(json.dumps(proof))


if __name__ == '__main__':
    main()
