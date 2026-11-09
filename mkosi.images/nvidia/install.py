#!/usr/bin/python3
"""Install hash-pinned NVIDIA redistributables; never execute the vendor installer."""

import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

BASE = Path(sys.argv[3]) if len(sys.argv) > 3 else Path('/work/src/mkosi.images/nvidia')
VERSION = (BASE / 'version').read_text().strip()
BUILD = Path(sys.argv[1])
DEST = Path(sys.argv[2])
ARCH = platform.machine()
ARTIFACTS = json.loads((BASE / 'artifacts.json').read_text())[ARCH]
SOURCE = BUILD / 'usr/src/nvidia'
META = DEST / 'usr/share/nvidia'
META.mkdir(parents=True, exist_ok=True)
(META / 'container-runtime').mkdir(exist_ok=True)
(META / 'version').write_text(VERSION + '\n')


def download(url, filename, digest):
    cache = Path('/work/src/.cache/nvidia') / filename
    target = cache if cache.exists() else Path('/tmp') / filename
    if not target.exists():
        with urllib.request.urlopen(url, timeout=120) as response, target.open('wb') as output:
            shutil.copyfileobj(response, output)
    with target.open('rb') as source:
        actual = hashlib.file_digest(source, 'sha256').hexdigest()
    if actual != digest:
        raise RuntimeError('SHA256 mismatch for ' + filename)
    return target


redist_arch = {'x86_64': 'linux-x86_64', 'aarch64': 'linux-sbsa'}[ARCH]
filename = f'nvidia_driver-{redist_arch}-{VERSION}-archive.tar.xz'
url = f'https://developer.download.nvidia.com/compute/nvidia-driver/redist/nvidia_driver/{redist_arch}/{filename}'
archive = download(url, filename, ARTIFACTS['driver_sha256'])
libraries = ('libcuda.', 'libnvidia-ml.', 'libnvidia-cfg.', 'libnvidia-ptxjitcompiler.',
             'libnvidia-nvvm.', 'libnvidia-gpucomp.', 'libnvidia-allocator.')
libdir = DEST / 'usr/lib' / {'x86_64': 'x86_64-linux-gnu', 'aarch64': 'aarch64-linux-gnu'}[ARCH]
libdir.mkdir(parents=True, exist_ok=True)
licences = DEST / 'usr/share/licenses/nvidia'
licences.mkdir(parents=True, exist_ok=True)
shutil.copyfile('/work/src/COPYING', licences / 'Apache-2.0')
with tarfile.open(archive) as source:
    prefix = source.getnames()[0].rstrip('/') + '/'
    for member in source:
        name = member.name.removeprefix(prefix)
        target = None
        if name.startswith('src/kernel-module-source/'):
            member.name = name.removeprefix('src/kernel-module-source/')
            if member.name:
                source.extract(member, SOURCE, filter='data')
            continue
        if not member.isfile():
            continue
        if name.startswith('firmware/'):
            target = DEST / 'usr/lib/firmware/nvidia' / VERSION / Path(name).name
        elif name.startswith('lib/') and Path(name).name.startswith(libraries):
            target = libdir / Path(name).name
        elif name in ('sbin/nvidia-smi', 'bin/nvidia-modprobe'):
            target = DEST / 'usr/bin' / Path(name).name
        elif name == 'src/nvidia-modprobe/COPYING':
            target = licences / 'COPYING-nvidia-modprobe'
        elif name == 'LICENSE':
            target = licences / 'LICENSE'
        if target:
            target.parent.mkdir(parents=True, exist_ok=True)
            with source.extractfile(member) as content, target.open('wb') as output:
                shutil.copyfileobj(content, output)
            target.chmod(member.mode)

# Generate aliases from each ELF's SONAME, avoiding version assumptions for CUDA libraries.
for library in list(libdir.iterdir()):
    dynamic = subprocess.check_output(['mkosi-chroot', 'readelf', '-d', library], text=True)
    match = re.search(r'\(SONAME\).*\[(.+?)\]', dynamic)
    if match and match[1] != library.name:
        (libdir / match[1]).symlink_to(library.name)

readme = (SOURCE / 'README.md').read_text()
ids = sorted(set(re.findall(r'^\|[^|]*\|\s*([0-9A-Fa-f]{4})\b', readme, re.MULTILINE)))
if not ids:
    raise RuntimeError('Missing open-driver GPU compatibility table')
(META / 'supported-gpus').write_text('\n'.join(value.lower() for value in ids) + '\n')
shutil.copyfile(SOURCE / 'COPYING', licences / 'COPYING-open-modules')

for package in ARTIFACTS['toolkit']:
    deb_arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}[ARCH]
    url = 'https://nvidia.github.io/libnvidia-container/stable/deb/' + deb_arch + '/' + package['file']
    deb = download(url, package['file'], package['sha256'])
    with tempfile.TemporaryDirectory() as directory:
        subprocess.run(['dpkg-deb', '-x', deb, directory], check=True)
        shutil.copytree(Path(directory) / 'usr', DEST / 'usr', dirs_exist_ok=True, symlinks=True)
        config = Path(directory) / 'etc/nvidia-container-runtime'
        if config.exists():
            shutil.copytree(config, META / 'container-runtime', dirs_exist_ok=True)
