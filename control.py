import argparse
import os
import shlex
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser(description='Control FckCensor on a connected Station')
    parser.add_argument('--device', default=os.environ.get('ANDROID_SERIAL'))
    commands = parser.add_subparsers(dest='command', required=True)
    play = commands.add_parser('play')
    play.add_argument('track', help='Yandex Music track link or numeric ID')
    search = commands.add_parser('search')
    search.add_argument('query', nargs='+')
    commands.add_parser('status')
    args = parser.parse_args()
    serial = args.device
    if not serial:
        result = subprocess.run(['adb', 'devices'], check=True, capture_output=True, text=True)
        devices = [line.split()[0] for line in result.stdout.splitlines()[1:]
                   if len(line.split()) >= 2 and line.split()[1] == 'device']
        if len(devices) != 1:
            parser.error('Select a connected Station with --device SERIAL')
        serial = devices[0]
    command = ['/data/quazar/yam/data/plugins/fckcensor/bin/fckcensor', '-mode', args.command]
    if args.command == 'play':
        command += ['-track', args.track]
    elif args.command == 'search':
        command += ['-query', ' '.join(args.query)]
    result = subprocess.run(['adb', '-s', serial, 'shell', shlex.join(command)], check=False)
    return result.returncode


if __name__ == '__main__':
    sys.exit(main())
