#!/usr/bin/env python3
"""Export retained Docker runtime logs without altering the running services."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--since', default='24h')
    parser.add_argument('--tail', type=int, default=10000)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('services', nargs='*', default=['relaytale'])
    args = parser.parse_args()
    if not 1 <= args.tail <= 100000:
        parser.error('--tail must be between 1 and 100000 per service')
    if not args.services or any(s not in ('relaytale', 'postgres', 'caddy') for s in args.services):
        parser.error('services must be relaytale, postgres or caddy')
    target = args.output.resolve()
    if target.exists():
        parser.error('output already exists; refusing to overwrite')
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    repo = Path(__file__).resolve().parents[1]
    temp_path = None
    try:
        with tempfile.TemporaryFile() as raw:
            subprocess.run(['docker', 'compose', 'logs', '--no-color', '--timestamps',
                            '--since', args.since, '--tail', str(args.tail), *args.services],
                           cwd=repo, stdout=raw, stderr=subprocess.PIPE, check=True, timeout=120)
            raw.seek(0)
            fd, temp_path = tempfile.mkstemp(prefix='.runtime-export-', dir=target.parent)
            with os.fdopen(fd, 'wb') as output:
                with gzip.GzipFile(fileobj=output, mode='wb', mtime=0) as archive:
                    shutil.copyfileobj(raw, archive)
                output.flush()
                os.fsync(output.fileno())
        digest = hashlib.sha256()
        with open(temp_path, 'rb') as exported:
            while chunk := exported.read(65536):
                digest.update(chunk)
        os.link(temp_path, target)
        dir_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
        print(json.dumps({'path': str(target), 'sha256': digest.hexdigest(),
                          'services': args.services, 'since': args.since,
                          'tail_per_service': args.tail}))
    finally:
        if temp_path:
            os.unlink(temp_path)


if __name__ == '__main__':
    try:
        main()
    except (OSError, subprocess.SubprocessError) as error:
        raise SystemExit(f'Runtime log export failed ({type(error).__name__}); check Docker and the output path.')
