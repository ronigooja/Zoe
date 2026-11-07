"""Reusable bounded probes. Imports never open network clients."""

import asyncio
import json
import tempfile
import wave
from dataclasses import replace
from pathlib import Path

from .errors import LabError
from .experiments import _call, delegation_id, turn, turn_text
from .upstream import build_message_item


def read_items(path):
    items = []
    for line in Path(path).read_text(encoding='utf-8').splitlines():
        if line.strip():
            role, separator, text = line.partition('|')
            if not separator:
                raise LabError('Items require role|text lines')
            items.append(build_message_item(role.strip(), text.strip()))
    return items


def join_wavs(paths, output, gap_s=1.0):
    """Join compatible PCM WAVs with silence; never transcode or invoke tools."""
    format_ = None
    with wave.open(str(output), 'wb') as target:
        for index, path in enumerate(paths):
            with wave.open(str(path), 'rb') as source:
                current = (source.getnchannels(), source.getsampwidth(), source.getframerate())
                if source.getcomptype() != 'NONE' or current[1] != 2:
                    raise LabError('Probe WAVs require uncompressed 16-bit PCM')
                if format_ is None:
                    format_ = current
                    target.setnchannels(current[0])
                    target.setsampwidth(current[1])
                    target.setframerate(current[2])
                if current != format_:
                    raise LabError('Probe WAV formats must match')
                if index:
                    target.writeframes(bytes(round(gap_s * current[2]) * current[0] * current[1]))
                target.writeframes(source.readframes(source.getnframes()))
    return Path(output)


def update_frame(name, args):
    """Codex codex-rs protocol.rs FramelessSessionUpdate uses type/session.

    Codex sends this at startup, not mid-call on AVAS. Mid-call acceptance and
    effects are UNVERIFIED; capture acknowledgments/errors, never infer success.
    """
    session = ({'instructions': args.update_instructions_file.read_text(encoding='utf-8')}
               if name == 'P3' else {'initial_items': read_items(args.update_items_file)})
    return {'type': 'session.update', 'session': session}


class Controller:
    def __init__(self, name, args, ctx):
        self.name, self.args, self.ctx = name, args, ctx
        self.task = None
        self.identifier = None
        self.users = 0
        self.sent = False
        self.later_user = False
        self.frame = update_frame(name, args) if name in ('P3', 'P4') else None

    async def ready(self, sideband, peer, result):
        self.sideband, self.peer, self.result = sideband, peer, result
        result.update({'post_update_event_types': [], 'post_update_errors': [],
                       'later_assistant_text': [], 'progress_answer': []})
        if self.frame and self.args.update_trigger == 'timer':
            self.task = asyncio.create_task(self.send_after(self.args.update_after_s))

    async def send_after(self, delay):
        await asyncio.sleep(delay)
        if self.frame:
            await self.sideband.send(self.frame)
            self.sent = True
            self.result['update_sent'] = True
        else:
            if not self.identifier:
                raise LabError('No open delegation for state sentence')
            text = self.args.state_file.read_text(encoding='utf-8')
            for frame in self.ctx.context_frames(text, delegation_id=self.identifier):
                await self.sideband.send(frame)
            self.sent = True
            self.result['state_sent'] = True
        await self.peer.play_wav(self.args.second_audio if self.frame else self.args.audio / '03.wav')

    async def event(self, event):
        if self.task is not None and self.task.done():
            self.task.result()
        if self.sent and self.frame and isinstance(event, dict):
            self.result['post_update_event_types'].append(event.get('type', '<unknown>'))
            if event.get('type') == 'error':
                self.result['post_update_errors'].append(event)
        self.identifier = delegation_id(event) or self.identifier
        if turn(event, 'user'):
            self.users += 1
            if self.sent:
                self.later_user = True
            if self.name == 'P1' and self.users == 2 and self.task is None:
                self.task = asyncio.create_task(self.send_after(self.args.delay_s))
        if self.frame and self.args.update_trigger == 'event' and self.task is None:
            if delegation_id(event) or turn(event, 'assistant'):
                self.task = asyncio.create_task(self.send_after(0))
        if turn(event, 'assistant'):
            if self.name == 'P2':
                return True
            if self.sent and self.later_user:
                field = 'later_assistant_text' if self.frame else 'progress_answer'
                self.result[field].append(turn_text(event))
                return True
        return False

    async def close(self):
        if self.task is not None:
            if not self.task.done():
                self.task.cancel()
            outcome = await asyncio.gather(self.task, return_exceptions=True)
            if isinstance(outcome[0], Exception):
                self.result['action_error'] = type(outcome[0]).__name__


async def run_probe(ctx, args):
    """Each repetition owns exactly one call and one Recorder directory."""
    name = args.experiment
    instructions = args.instructions_file.read_text(encoding='utf-8')
    items = read_items(args.items_file) if name == 'P2' else []
    session = ctx.upstream.build_session(instructions=instructions, initial_items=items)
    ctx.recorder.write_session(session)
    controller = Controller(name, args, ctx)
    result = None
    try:
        with tempfile.TemporaryDirectory(prefix='voice-lab-probe-') as temporary:
            wav = args.audio
            if name == 'P1':
                wav = join_wavs([args.audio / '01.wav', args.audio / '02.wav'],
                                Path(temporary) / 'weather-cancel.wav', args.gap_s)
            result = await asyncio.wait_for(
                _call(replace(ctx, observation_s=args.timeout_s), session, wav, probe=controller),
                args.timeout_s + 2 * ctx.cleanup_timeout_s + 5)
    except Exception as error:
        result = controller.result if hasattr(controller, 'result') else {'events': []}
        result['error'] = type(error).__name__
    result['pass'] = bool(result.get('clean_close') and not any(
        result.get(key) for key in ('error', 'reader_error', 'action_error')))
    result['delegated'] = any(delegation_id(event) for event in result['events'])
    ctx.recorder.write_summary('# ' + name + '\n\nEvidence only; human judgment required.\n\n```json\n' +
                               json.dumps(result, ensure_ascii=True, indent=2) + '\n```\n')
    return result


def plan(args):
    files = {}
    for field in ('instructions_file', 'items_file', 'update_instructions_file', 'update_items_file', 'state_file'):
        path = getattr(args, field, None)
        if path is not None:
            files[field] = {'name': path.name, 'length': len(path.read_text(encoding='utf-8'))}
    for field in ('audio', 'second_audio'):
        path = getattr(args, field, None)
        if path is not None:
            files[field] = {'name': path.name}
    return {'experiment': args.experiment, 'repeat': args.repeat, 'files': files,
            'delay_s': args.delay_s, 'update_trigger': args.update_trigger,
            'update_after_s': args.update_after_s, 'timeout_s': args.timeout_s}
