import argparse
import os
import shutil
import subprocess
import tomllib
import zipfile
from pathlib import Path

root = Path(__file__).resolve().parent
build_dir = root / '.build'
dist = root / 'dist'


def clean():
    for directory in (build_dir, dist):
        if directory.is_symlink():
            directory.unlink()
        elif directory.exists():
            shutil.rmtree(directory)


def build():
    manifest = tomllib.loads((root / 'plugin.toml').read_text())
    staging = build_dir / 'package'
    if staging.exists():
        shutil.rmtree(staging)
    bin_dir = staging / 'bin'
    lib_dir = staging / 'lib'
    bin_dir.mkdir(parents=True)
    lib_dir.mkdir()
    env = dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH='arm64')
    subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(bin_dir / 'fckcensor'), './cmd/fckcensor'], env=env, check=True)
    subprocess.run(['clang', '--target=armv7-linux-gnueabihf', '-mthumb', '-O2', '-fPIC', '-shared', '-nostdlib', '-fuse-ld=lld', '-fomit-frame-pointer', '-Wl,-soname,libfckcensor.so', '-o', str(lib_dir / 'libfckcensor.so'), 'native/hook.c'], check=True)
    for name in ('plugin.toml', 'start.sh'):
        shutil.copy2(root / name, staging / name)
    (staging / 'start.sh').chmod(0o755)
    dist.mkdir(exist_ok=True)
    package = dist / f"{manifest['id']}-v{manifest['version']}.yap"
    with zipfile.ZipFile(package, 'w', zipfile.ZIP_DEFLATED) as archive:
        for name in ('plugin.toml', 'start.sh', 'bin/fckcensor', 'lib/libfckcensor.so'):
            archive.write(staging / name, name)
    print(package)


def main():
    parser = argparse.ArgumentParser(description='Build and verify FckCensor')
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument('--clean', action='store_true')
    modes.add_argument('--rebuild', action='store_true')
    args = parser.parse_args()
    os.chdir(root)
    if args.clean or args.rebuild:
        clean()
    if args.clean:
        return
    build()


if __name__ == '__main__':
    main()
