import argparse
import json
import re
from pathlib import Path
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

DATA_URL = 'https://raw.githubusercontent.com/Hazzz895/FckCensorData/main/list_v2.json'


def normalize_url(value):
    if not isinstance(value, str) or not value:
        raise ValueError('Missing audio URL')
    url = value if '://' in value else 'https://' + value
    parts = urlsplit(url)
    if parts.scheme != 'https' or not parts.hostname or parts.username or parts.password:
        raise ValueError('Audio URL must use HTTPS without credentials')
    return url


def build_index(document):
    result = {}
    sources = document.get('sources')
    if not isinstance(sources, list):
        raise TypeError('Invalid replacement document')
    for source in sources:
        for storage in source.get('tracksStorages', []):
            template = storage.get('urlTemplate')
            for entry in storage.get('trackIds', []):
                track_id = str(entry['id'] if isinstance(entry, dict) else entry)
                if re.fullmatch(r'\d+', track_id) and template:
                    result.setdefault(track_id, normalize_url(template.replace('%%', track_id)))
            for track_id, entry in storage.get('tracks', {}).items():
                track_id = str(track_id)
                if not re.fullmatch(r'\d+', track_id):
                    continue
                value = entry.get('url') if isinstance(entry, dict) else entry
                if isinstance(value, int) and template:
                    value = template.replace('%%', str(value))
                if isinstance(value, str):
                    result.setdefault(track_id, normalize_url(value))
    return result


def main():
    parser = argparse.ArgumentParser(description='Resolve FckCensor audio replacements; does not control Station playback')
    parser.add_argument('track_id', nargs='?')
    parser.add_argument('--source', type=Path)
    parser.add_argument('--export', type=Path)
    args = parser.parse_args()
    if args.source:
        document = json.loads(args.source.read_text())
    else:
        request = Request(DATA_URL, headers={'User-Agent': 'StationUncensorResearch/0.1'})
        with urlopen(request, timeout=20) as response:
            content = response.read(4 * 1024 * 1024 + 1)
        if len(content) > 4 * 1024 * 1024:
            raise ValueError('Replacement document exceeds size limit')
        document = json.loads(content)
    index = build_index(document)
    if args.export:
        args.export.write_text(json.dumps(index, ensure_ascii=False, indent=2) + '\n')
    if args.track_id:
        track_id = args.track_id.split(':', 1)[0]
        if not re.fullmatch(r'\d+', track_id):
            parser.error('Track ID must be numeric, optionally followed by :album_id')
        print(json.dumps({'track_id': track_id, 'replacement_url': index.get(track_id)}, ensure_ascii=False))
    elif not args.export:
        print(json.dumps({'audio_replacements': len(index)}))


if __name__ == '__main__':
    main()
