#!/usr/bin/env python3
"""Offline macOS TTS preparation; no network, no third-party packages."""

import argparse
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


def generate_audio(utterances, out, voice='Tingting', *, runner=subprocess.run,
                   which=shutil.which, platform=sys.platform):
    """Prepare numbered files; all generated and temporary files stay under out."""
    if platform != 'darwin' or which('say') is None or which('afconvert') is None:
        raise ValueError('Audio preparation requires macOS say and afconvert')
    texts = [line.strip() for line in Path(utterances).read_text(encoding='utf-8').splitlines()
             if line.strip()]
    output = Path(out).resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.voice-lab-tts-', dir=output) as temporary:
        source = Path(temporary) / 'speech.aiff'
        for index, text in enumerate(texts, 1):
            runner(['say', '-v', voice, '-o', str(source), text], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            runner(['afconvert', '-f', 'WAVE', '-d', 'LEI16@16000', '-c', '1',
                    str(source), str(output / f'{index:02d}.wav')], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    (output / 'index.txt').write_text(
        ''.join(f'{index:02d}.wav\t{text}\n' for index, text in enumerate(texts, 1)),
        encoding='utf-8')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--voice', default='Tingting')
    parser.add_argument('--utterances', required=True)
    parser.add_argument('--out', required=True)
    args = parser.parse_args(argv)
    try:
        generate_audio(args.utterances, args.out, args.voice)
    except Exception:
        parser.exit(1, 'Local audio preparation failed; check installed voices and tools.\n')


if __name__ == '__main__':
    main()
