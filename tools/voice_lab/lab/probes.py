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


def append_frame(channel, text, identifier=None):
    """GPT-Live live-conversations docs shape; UNVERIFIED on AVAS.

    Source: https://developers.openai.com/api/docs/guides/live-conversations
    Mirror context append's input_text content; the channel is in the type.
    Keep empty text for P7 and send one frame, without context splitting.
    """
    if channel not in ('instructions', 'thinking', 'commentary'):
        raise ValueError('Unknown append channel')
    return {'type': f'session.{channel}.append', 'delegation_id': identifier,
            'content': [{'type': 'input_text', 'text': text}]}


def function_output_frame(shape, identifier, text):
    """For P10, all three shapes are GUESSES; AVAS fields are UNVERIFIED.

    Codex source does not define delegation.function_call_output.create.
    """
    if shape not in ('A', 'B', 'C'):
        raise ValueError('Unknown function output shape')
    frame = {'type': 'delegation.function_call_output.create',
             'call_id' if shape == 'C' else 'delegation_item_id': identifier}
    frame.update({'content': [{'type': 'input_text', 'text': text}]} if shape == 'B'
                 else {'output': text})
    return frame


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


class AppendController(Controller):
    """P8 shares P5 behavior, with fixed instructions/unbound defaults."""

    async def ready(self, sideband, peer, result):
        await super().ready(sideband, peer, result)
        self.window_start = None
        self.second_started = False
        result.update({'frame_sent': False, 'post_append_event_types': [],
                       'post_append_errors': [], 'spoken': False, 'filler': False,
                       'filler_text': [], 'errors': []})

    async def observe_window(self, seconds, second=False):
        await asyncio.sleep(seconds)
        field = 'spoken' if second else 'filler'
        self.result[field] |= self.peer.received_audio_ms > self.audio_start
        self.result[field + '_window_complete'] = True
        if second:
            self.second_started = True
            if self.args.second_audio is not None:
                await self.peer.play_wav(self.args.second_audio)

    async def event(self, event):
        if self.task is not None and self.task.done():
            self.task.result()
        now = asyncio.get_running_loop().time()
        kind = event.get('type', '<unknown>') if isinstance(event, dict) else '<unknown>'
        if kind == 'error':
            self.result['errors'].append(event)
        if self.sent:
            self.result['post_append_event_types'].append(kind)
            if kind == 'error':
                self.result['post_append_errors'].append(event)
        if self.window_start is not None:
            seconds = 5.0 if self.name == 'P7' else self.args.spoken_window_s
            in_window = now - self.window_start < seconds and not self.second_started
            if in_window and (kind in ('session.output_audio.delta', 'output_transcript.added') or
                              turn(event, 'assistant')):
                field = 'filler' if self.name == 'P7' else 'spoken'
                self.result[field] = True
                if self.name == 'P7':
                    text = turn_text(event) if turn(event, 'assistant') else event.get('transcript', event.get('text', ''))
                    if isinstance(text, str) and text:
                        self.result['filler_text'].append(text)
        identifier = delegation_id(event)
        if identifier and self.window_start is None:
            await self.start_window(identifier, now)
        if self.second_started and turn(event, 'assistant'):
            self.result['later_assistant_text'].append(turn_text(event))
        # Keep collecting all post-frame events until the bounded call ends.
        return False


    async def start_window(self, identifier, now):
        self.identifier = identifier
        self.window_start = now
        self.audio_start = self.peer.received_audio_ms
        if self.name != 'P7' or self.args.empty_thinking:
            channel = 'thinking' if self.name == 'P7' else ('instructions' if self.name == 'P8' else self.args.channel)
            bound = identifier if self.name == 'P7' or (self.name != 'P8' and self.args.bind == 'open') else None
            text = '' if self.name == 'P7' else self.args.text_file.read_text(encoding='utf-8')
            frame = append_frame(channel, text, bound)
            await self.sideband.send(frame)
            self.sent = True
            self.result.update({'frame_sent': True, 'sent_frame': frame})
            if self.name != 'P7':
                self.window_start = asyncio.get_running_loop().time()
        self.task = asyncio.create_task(self.observe_window(
            max(0, 5.0 - (asyncio.get_running_loop().time() - self.window_start))
            if self.name == 'P7' else self.args.spoken_window_s, self.name != 'P7'))


class ProtocolController(AppendController):
    """P9/P10 reuse append observation and cleanup without assuming acceptance."""

    async def ready(self, sideband, peer, result):
        await super().ready(sideband, peer, result)
        self.pre_create_start = None
        result.update({'frames_sent': [], 'pre_create_spoken': False,
                       'pre_create_window_complete': False,
                       'pre_create_assistant_text': [], 'spoken_window_assistant_text': [],
                       'post_frame_non_error_event_types': []})

    async def send_frame(self, frame):
        await self.sideband.send(frame)
        self.sent = True
        self.result['frame_sent'] = True
        self.result['frames_sent'].append(frame['type'])

    async def start_window(self, identifier, now):
        if self.identifier is not None:
            return
        self.identifier = identifier
        if self.name == 'P9':
            if self.args.with_context == 'state':
                text = self.args.text_file.read_text(encoding='utf-8')
                frames = self.ctx.context_frames(text)
                if len(frames) != 1:
                    raise LabError('P9 context requires exactly one frame')
                await self.send_frame(frames[0])
            self.pre_create_start = now
            self.pre_create_audio = self.peer.received_audio_ms
        else:
            await self.send_frame(function_output_frame(
                self.args.shape, identifier, self.args.text_file.read_text(encoding='utf-8')))
            self.window_start = asyncio.get_running_loop().time()
            self.audio_start = self.peer.received_audio_ms
        self.task = asyncio.create_task(self.run_window())

    async def run_window(self):
        if self.name == 'P9':
            await asyncio.sleep(self.args.create_delay_s)
            self.result['pre_create_spoken'] |= self.peer.received_audio_ms > self.pre_create_audio
            await self.send_frame({'type': 'response.create'})
            self.result['pre_create_window_complete'] = True
            self.pre_create_start = None
            self.window_start = asyncio.get_running_loop().time()
            self.audio_start = self.peer.received_audio_ms
        await self.observe_window(self.args.spoken_window_s, second=True)

    async def event(self, event):
        kind = event.get('type', '<unknown>') if isinstance(event, dict) else '<unknown>'
        if self.pre_create_start is not None and (kind in (
                'session.output_audio.delta', 'output_transcript.added') or turn(event, 'assistant')):
            self.result['pre_create_spoken'] = True
            if turn(event, 'assistant'):
                self.result['pre_create_assistant_text'].append(turn_text(event))
        if (self.window_start is not None and not self.second_started and
                asyncio.get_running_loop().time() - self.window_start < self.args.spoken_window_s and
                turn(event, 'assistant')):
            self.result['spoken_window_assistant_text'].append(turn_text(event))
        if self.sent and kind != 'error':
            self.result['post_frame_non_error_event_types'].append(kind)
        return await super().event(event)


class SpawnController(Controller):
    async def ready(self, sideband, peer, result):
        await super().ready(sideband, peer, result)
        self.started = asyncio.get_running_loop().time()
        self.timeline = []

    async def event(self, event):
        self.timeline.append((asyncio.get_running_loop().time() - self.started, event))
        return False

    async def close(self):
        await super().close()
        if not hasattr(self, 'timeline'):
            return
        rows = [{'audio': path.name, 'delegation_created': False, 'user_transcript': '',
                 'assistant_text': [], 'delegation_match': []} for path in self.audio_paths]
        current = 0
        users = 0
        ids = {}
        for elapsed, event in self.timeline:
            if turn(event, 'user'):
                current = min(users, len(rows) - 1)
                users += 1
                rows[current]['user_transcript'] = turn_text(event)
                rows[current]['user_t_s'] = elapsed
                identifier = event.get('turn', {}).get('id')
                if identifier:
                    ids[identifier] = current
            elif turn(event, 'assistant'):
                rows[current]['assistant_text'].append(turn_text(event))
        current = 0
        users = 0
        for elapsed, event in self.timeline:
            if turn(event, 'user'):
                current = min(users, len(rows) - 1)
                users += 1
            if delegation_id(event):
                item = event.get('item', {})
                linked = item.get('user_bidi_turn_id', event.get('user_bidi_turn_id'))
                index = ids.get(linked, current)
                rows[index]['delegation_created'] = True
                rows[index]['delegation_match'].append({
                    'id': delegation_id(event), 't_s': elapsed,
                    'method': 'turn_id' if linked in ids else 'order/time'})
        self.result['utterances'] = rows
        self.result['matching_note'] = 'Turn id when available; otherwise latest final user turn (first before any user). Overlapping turns require human review.'


async def run_probe(ctx, args):
    """Each repetition owns exactly one call and one Recorder directory."""
    name = args.experiment
    instructions = args.instructions_file.read_text(encoding='utf-8')
    items = read_items(args.items_file) if name == 'P2' else []
    options = {'acknowledgement_delay_ms': args.ack_delay_ms} if name == 'P7' else {}
    session = ctx.upstream.build_session(instructions=instructions, initial_items=items, **options)
    ctx.recorder.write_session(session)
    controller_type = (ProtocolController if name in ('P9', 'P10') else
                       AppendController if name in ('P5', 'P7', 'P8') else
                       SpawnController if name == 'P6' else Controller)
    controller = controller_type(name, args, ctx)
    result = None
    try:
        with tempfile.TemporaryDirectory(prefix='voice-lab-probe-') as temporary:
            wav = args.audio
            if name == 'P1':
                wav = join_wavs([args.audio / '01.wav', args.audio / '02.wav'],
                                Path(temporary) / 'weather-cancel.wav', args.gap_s)
            if name == 'P6':
                paths = sorted(args.audio.glob('[0-9][0-9].wav')) if args.audio.is_dir() else [args.audio]
                if not paths:
                    raise LabError('P6 requires numbered WAV files')
                if args.audio.is_dir() and [p.name for p in paths] != [f'{i:02d}.wav' for i in range(1, len(paths) + 1)]:
                    raise LabError('P6 WAV numbering must start at 01 without gaps')
                controller.audio_paths = paths
                wav = join_wavs(paths, Path(temporary) / 'utterances.wav', args.gap_s) if args.audio.is_dir() else args.audio
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
    for field in ('instructions_file', 'items_file', 'update_instructions_file', 'update_items_file', 'state_file', 'text_file'):
        path = getattr(args, field, None)
        if path is not None:
            files[field] = {'name': path.name, 'length': len(path.read_text(encoding='utf-8'))}
    for field in ('audio', 'second_audio'):
        path = getattr(args, field, None)
        if path is not None:
            files[field] = {'name': path.name}
    return {'experiment': args.experiment, 'repeat': args.repeat, 'files': files,
            'delay_s': args.delay_s, 'update_trigger': args.update_trigger,
            'update_after_s': args.update_after_s, 'timeout_s': args.timeout_s,
            'channel': 'instructions' if args.experiment == 'P8' else args.channel,
            'bind': 'none' if args.experiment == 'P8' else args.bind,
            'spoken_window_s': args.spoken_window_s, 'ack_delay_ms': args.ack_delay_ms,
            'empty_thinking': args.empty_thinking,
            **({'with_context': args.with_context, 'create_delay_s': args.create_delay_s,
                'shape': args.shape} if args.experiment in ('P9', 'P10') else {})}
