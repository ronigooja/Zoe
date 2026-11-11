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
    result.add_argument('--experiment', required=True, choices=[f'E{i}' for i in range(8)] + [f'P{i}' for i in range(1, 11)])
    result.add_argument('--account-id', required=True, type=int)
    for name in ('db-copy', 'key-file', 'persona-file'):
        result.add_argument('--' + name, required=True)
    result.add_argument('--results-dir', type=Path, default=Path('results'))
    result.add_argument('--audio', type=Path)
    result.add_argument('--instructions-file', type=Path, default=ROOT / 'cases' / 'instructions.txt')
    result.add_argument('--timeout-s', type=float, default=45.0)
    result.add_argument('--dry-run', action='store_true')
    result.add_argument('--fingerprint', choices=('codex', 'legacy-browser'), default='codex')
    result.add_argument('--repeat', type=int, default=3)
    result.add_argument('--delay-s', type=float, default=1.0)
    result.add_argument('--gap-s', type=float, default=3.0)
    result.add_argument('--update-after-s', type=float, default=3.0)
    result.add_argument('--update-trigger', choices=('event', 'timer'), default='event')
    result.add_argument('--second-audio', type=Path)
    result.add_argument('--channel', choices=('instructions', 'thinking', 'commentary'), default='instructions')
    result.add_argument('--bind', choices=('none', 'open'), default='none')
    result.add_argument('--text-file', type=Path, default=ROOT / 'cases' / 'append_override.txt')
    result.add_argument('--spoken-window-s', type=float, default=6.0)
    result.add_argument('--ack-delay-ms', type=int)
    result.add_argument('--empty-thinking', action='store_true')
    result.add_argument('--with-context', choices=('state',))
    result.add_argument('--context-channel', choices=('speakable', 'commentary'), default='speakable', help='Channel of the P1 state context append.')
    result.add_argument('--create-delay-s', type=float, default=1.0)
    result.add_argument('--shape', choices=('A', 'B', 'C'), default='A')
    result.add_argument('--state-file', type=Path, default=ROOT / 'cases' / 'state_cancel.txt')
    result.add_argument('--items-file', type=Path, default=ROOT / 'cases' / 'items_plate_home.txt')
    result.add_argument('--p6-items-file', type=Path, help='Optional initial_items for P6 only; without it P6 seeds none.')
    result.add_argument('--update-instructions-file', type=Path, default=ROOT / 'cases' / 'instructions_override.txt')
    result.add_argument('--update-items-file', type=Path, default=ROOT / 'cases' / 'items_plate_home.txt')
    return result


def validate(args, argument_parser):
    if args.account_id <= 0:
        argument_parser.error('--account-id must be positive')
    if not math.isfinite(args.timeout_s) or not 0 < args.timeout_s <= 300:
        argument_parser.error('--timeout-s must be finite, positive, and at most 300')
    if args.experiment != 'E0' and args.audio is None:
        argument_parser.error('--audio is required except for E0')
    if args.experiment.startswith('P'):
        if not 1 <= args.repeat <= 100:
            argument_parser.error('--repeat must be between 1 and 100')
        for value in (args.delay_s, args.gap_s, args.update_after_s, args.spoken_window_s, args.create_delay_s):
            if not math.isfinite(value) or not 0 <= value <= 300:
                argument_parser.error('Probe delays must be finite and between 0 and 300')
        if args.ack_delay_ms is not None and args.ack_delay_ms < 0:
            argument_parser.error('--ack-delay-ms must be non-negative')
        if args.experiment in ('P3', 'P4', 'P5', 'P8') and args.second_audio is None:
            argument_parser.error('--second-audio is required for update probes')
        if args.experiment == 'P1' and args.audio.suffix.lower() == '.wav':
            argument_parser.error('P1 requires a directory containing 01.wav, 02.wav, 03.wav')
        return
    if args.audio is not None:
        if args.experiment == 'E1' and args.audio.suffix.lower() != '.wav':
            argument_parser.error('E1 requires a WAV path')
        if args.experiment not in ('E0', 'E1') and args.audio.suffix.lower() == '.wav':
            argument_parser.error('E2..E7 require a directory of numbered WAV files')


def plan(args):
    if args.experiment.startswith('P'):
        from lab.probes import plan as probe_plan
        return probe_plan(args)
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
    from lab.account import load_account, load_persona
    from lab.record import Recorder, make_run_dir
    from lab import upstream
    from lab.sideband import Sideband, context_frames, user_text_frame
    from lab.webrtc import Peer
    from lab.experiments import Context, EXPERIMENTS

    account = load_account(args.db_copy, args.key_file, args.account_id)
    persona = load_persona(args.persona_file, args.fingerprint)
    if args.fingerprint == 'codex':
        persona.account_id = account.codex_account_id()
    recorder = Recorder(make_run_dir(args.results_dir, args.experiment),
                        [account.token, account.proxy, account.device_id] +
                        (list(persona.values()) + [persona.account_id] if args.fingerprint == 'codex' else
                         [value for key, value in persona.items() if key.lower() == 'oai-device-id']))
    context = Context(account=account, persona=persona, peer_factory=Peer,
                      transport=None, connector=None, recorder=recorder, upstream=upstream,
                      sideband_factory=Sideband, context_frames=context_frames,
                      user_text_frame=user_text_frame,
                      paths={'cases': ROOT / 'cases', 'audio': args.audio,
                             'instructions': args.instructions_file}, timeout_s=args.timeout_s)
    if args.experiment.startswith('P'):
        from lab.probes import run_probe
        return await run_probe(context, args)
    return await EXPERIMENTS[args.experiment](context)


async def repeated(args):
    results = [await execute(args) for _ in range(args.repeat)]
    return {'pass': all(result['pass'] for result in results),
            'counts': {'runs': len(results), 'completed': sum(bool(r['pass']) for r in results),
                       'delegated': sum(bool(r.get('delegated')) for r in results),
                        'errors': sum(bool(r.get('error') or r.get('reader_error') or r.get('post_update_errors') or r.get('errors'))
                                     for r in results)}}


def main(argv=None):
    argument_parser = parser()
    args = argument_parser.parse_args(argv)
    validate(args, argument_parser)
    if args.dry_run:
        print(json.dumps(plan(args), ensure_ascii=True, indent=2))
        return 0
    try:
        observations = asyncio.run(repeated(args) if args.experiment.startswith('P') else execute(args))
    except (LabError, ValueError):
        print('Experiment failed; sensitive error details suppressed.')
        return 1
    except Exception:
        # Exception messages may contain request URLs or credentials.
        print('Experiment failed; sensitive error details suppressed.')
        return 1
    if args.experiment.startswith('P'):
        print(' '.join(key + '=' + str(value) for key, value in observations['counts'].items()))
    else:
        print(args.experiment + ': ' + ('PASS' if observations['pass'] else 'FAIL'))
    return 0 if observations['pass'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
