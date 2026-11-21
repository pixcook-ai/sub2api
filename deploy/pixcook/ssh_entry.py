#!/usr/bin/env python3
"""Forced-command entry point for the development-only SSH identity."""
import os
import re
import shlex
import sys

DEPLOY = '/opt/pixcook-cicd/deploy.py'


def parse_command(command):
    args = shlex.split(command)
    if len(args) != 8 or args[:4] != ['python3', DEPLOY, '--environment', 'dev']:
        raise ValueError('only the fixed development deployment command is permitted')
    if args[4] != '--image' or args[6] != '--source-sha':
        raise ValueError('invalid development deployment arguments')
    if not re.fullmatch(r'ghcr\.io/pixcook-ai/sub2api@sha256:[0-9a-f]{64}', args[5]):
        raise ValueError('a pinned Pixcook digest is required')
    if not re.fullmatch(r'[0-9a-f]{40}', args[7]):
        raise ValueError('a full source SHA is required')
    return args[2:]


def main():
    try:
        if len(sys.argv) != 1:
            raise ValueError('entry point takes no direct arguments')
        args = parse_command(os.environ.get('SSH_ORIGINAL_COMMAND', ''))
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    os.execve('/usr/bin/python3', ['/usr/bin/python3', DEPLOY, *args],
              {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'HOME': '/root', 'LANG': 'C.UTF-8'})


if __name__ == '__main__':
    sys.exit(main())
