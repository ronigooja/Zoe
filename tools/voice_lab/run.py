#!/usr/bin/env python3
"""One explicit, operator-selected voice-lab experiment per invocation."""

import argparse
import asyncio
import json
import math
from pathlib import Path

from lab.errors import LabError

ROOT = Path(__file__).resolve().parent


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument('--experiment', required=True, choices=[f'E{i}' for i in range(8)])
    result.add_argument('--account-id', required=True, type=int)
    for name in ('db-copy', 'key-file', 'persona-file'):
        result.add_argument('--' + name, required=True)
    result.add_argument('--results-dir', type=Path, default=Path('results'))
    result.add_argument('--audio', type=Path)
    result.add_argument('--instructions-file', type=Path, default=ROOT / 'cases' / 'instructions.txt')
    result.add_argument('--timeout-s', type=float, default=45.0)
    result.add_argument('--dry-run', action='store_true')
    return result


def validate(args, argument_parser):
    if args.account_id <= 0:
        argument_parser.error('--account-id must be positive')
    if not math.isfinite(args.timeout_s) or not 0 < args.timeout_s <= 300:
        argument_parser.error('--timeout-s must be finite, positive, and at most 300')
    if args.experiment != 'E0' and args.audio is None:
        argument_parser.error('--audio is required except for E0')
    if args.audio is not None:
        if args.experiment == 'E1' and args.audio.suffix.lower() != '.wav':
            argument_parser.error('E1 requires a WAV path')
        if args.experiment not in ('E0', 'E1') and args.audio.suffix.lower() == '.wav':
            argument_parser.error('E2..E7 require a directory of numbered WAV files')


def plan(args):
    instructions = args.instructions_file.read_text(encoding='utf-8')
    utterances = (ROOT / 'cases' / 'utterances.txt').read_text(encoding='utf-8').splitlines()
    return {'experiment': args.experiment, 'utterances': utterances,
            'instructions_file': {'name': args.instructions_file.name, 'length': len(instructions)},
            'timeout_s': args.timeout_s,
            'session': {'instructions_length': 0 if args.experiment in ('E0', 'E1') else len(instructions),
                        'delegation': {'type': 'client', **({'ack_filler': [True, False]} if args.experiment == 'E7' else {})},
                        'audio': {'output': {'voice': 'cove'}}, 'model': 'gpt-live-1-codex',
                        'initial_items': 'one fact, user then developer' if args.experiment == 'E6' else []},
            'prerequisite': 'E2..E7 require an earlier E1 delegation observation; operator must verify.'}


async def execute(args):
    # Import optional dependencies only after argument validation and the dry-run exit.
    from lab.account import load_account
    from lab.record import Recorder, make_run_dir
    from lab import upstream
    from lab.sideband import Sideband, context_frames, user_text_frame
    from lab.webrtc import Peer
    from lab.experiments import Context, EXPERIMENTS

    account = load_account(args.db_copy, args.key_file, args.account_id)
    persona = json.loads(Path(args.persona_file).read_text(encoding='utf-8'))
    if not isinstance(persona, dict):
        raise ValueError('persona must be an object')
    recorder = Recorder(make_run_dir(args.results_dir, args.experiment),
                        [account.token, account.proxy, account.device_id] +
                        [value for key, value in persona.items() if key.lower() == 'oai-device-id'])
    context = Context(account=account, persona=persona, peer_factory=Peer,
                      transport=None, connector=None, recorder=recorder, upstream=upstream,
                      sideband_factory=Sideband, context_frames=context_frames,
                      user_text_frame=user_text_frame,
                      paths={'cases': ROOT / 'cases', 'audio': args.audio,
                             'instructions': args.instructions_file}, timeout_s=args.timeout_s)
    return await EXPERIMENTS[args.experiment](context)


def main(argv=None):
    argument_parser = parser()
    args = argument_parser.parse_args(argv)
    validate(args, argument_parser)
    if args.dry_run:
        print(json.dumps(plan(args), ensure_ascii=True, indent=2))
        return 0
    try:
        observations = asyncio.run(execute(args))
    except (LabError, ValueError):
        print('Experiment failed; sensitive error details suppressed.')
        return 1
    except Exception:
        # Exception messages may contain request URLs or credentials.
        print('Experiment failed; sensitive error details suppressed.')
        return 1
    print(args.experiment + ': ' + ('PASS' if observations['pass'] else 'FAIL'))
    return 0 if observations['pass'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
