"""Bounded, dependency-injected voice probes. No network clients are imported here."""

import asyncio
import json
from dataclasses import dataclass, field
from pathlib import Path

from .errors import LabError
from .record import slim_event
from .upstream import resolve_device_id

HARD_CAP_S = 1800.0


@dataclass
class Context:
    account: object
    persona: dict
    peer_factory: object
    transport: object
    connector: object
    recorder: object
    upstream: object
    sideband_factory: object
    context_frames: object
    user_text_frame: object
    paths: dict = field(default_factory=dict)
    timeout_s: float = 45.0
    cleanup_timeout_s: float = 15.0
    silence_s: float = 0.25
    observation_s: float = 20.0
    delegation_grace_s: float = 8.0


def inputs(ctx):
    cases = Path(ctx.paths['cases'])
    instructions = Path(ctx.paths.get('instructions', cases / 'instructions.txt'))
    return (instructions.read_text(encoding='utf-8'),
            [line for line in (cases / 'utterances.txt').read_text(encoding='utf-8').splitlines()
             if line.strip()])


def audio(ctx, index=1):
    path = Path(ctx.paths['audio'])
    return path if path.suffix.lower() == '.wav' else path / f'{index:02d}.wav'


def turn(event, role):
    """Unknown event shapes remain in the event list rather than raising."""
    if not isinstance(event, dict) or event.get('type') != 'turn.done':
        return False
    payload = event.get('turn', event)
    return isinstance(payload, dict) and payload.get('role') == role


def turn_text(event):
    payload = event.get('turn', event)
    text = payload.get('transcript', payload.get('text', '')) if isinstance(payload, dict) else ''
    return text if isinstance(text, str) else ''


def delegated_text(event):
    if not isinstance(event, dict) or event.get('type') != 'delegation.created':
        return None
    item = event.get('item')
    content = item.get('content') if isinstance(item, dict) else None
    if isinstance(content, list) and content and isinstance(content[0], dict):
        text = content[0].get('text')
        return text if isinstance(text, str) else None
    return None


def experiment_timeout(ctx, name):
    count = {'E2': 7, 'E3': 3, 'E5': 2, 'E6': 2, 'E7': 14}.get(name, 1)
    if name in ('E2', 'E7'):
        count = len(inputs(ctx)[1]) * (2 if name == 'E7' else 1)
    per_call = ctx.timeout_s + ctx.observation_s + 2 * ctx.cleanup_timeout_s
    return min(count * per_call + 5.0, HARD_CAP_S)


def delegation_id(event):
    if not isinstance(event, dict) or event.get('type') != 'delegation.created':
        return None
    item = event.get('item', event.get('delegation', event))
    return item.get('id') if isinstance(item, dict) else None


async def _frames(ctx, sideband, text, identifier=None):
    for frame in ctx.context_frames(text, delegation_id=identifier):
        await sideband.send(frame)


async def _call(ctx, session, wav, mode=None, sentence=None, probe=None):
    peer = sideband = None
    reader = None
    queue = asyncio.Queue()
    result = {'events': [], 'clean_close': False, 'reader_ended_early': False,
              'audio_input': None if wav is None else Path(wav).name}
    cleanup_errors = []

    async def read_events():
        try:
            async for event in sideband.events():
                # Wrap events so an unknown JSON null is not the end sentinel.
                await queue.put((slim_event(event),))
        except Exception as error:
            result['reader_error'] = type(error).__name__
        finally:
            queue.put_nowait(None)

    try:
        # Validate identity before even creating a peer (ICE can use the network).
        device_id = resolve_device_id(ctx.account.device_id, ctx.persona)
        peer = ctx.peer_factory(recorder=ctx.recorder)
        offer = await peer.create_offer(wav)
        request = ctx.upstream.build_create_request(
            ctx.account.token, device_id, ctx.persona, offer, session)
        created = await ctx.upstream.create_call(
            request, ctx.transport, proxy=ctx.account.proxy,
            secrets=[ctx.account.token, ctx.account.proxy, ctx.account.device_id] +
                    [value for key, value in ctx.persona.items() if key.lower() == 'oai-device-id'])
        result['create_ok'] = 200 <= created.status < 300 and bool(created.call_id)
        if not result['create_ok']:
            return result
        sideband = ctx.sideband_factory(created.call_id, ctx.account.token, ctx.persona,
                                        proxy=ctx.account.proxy, connector=ctx.connector,
                                        recorder=ctx.recorder)
        await sideband.connect()
        result['sideband_connected'] = True
        reader = asyncio.create_task(read_events(), name='voice-lab-sideband-reader')
        await peer.set_answer(created.answer_sdp)
        if probe is not None:
            await probe.ready(sideband, peer, result)
        if mode == 'silence':
            await asyncio.sleep(ctx.silence_s)
        else:
            identifier = None
            sent = False
            answered = False
            delegation_deadline = None
            deadline = asyncio.get_running_loop().time() + ctx.observation_s
            while True:
                now = asyncio.get_running_loop().time()
                if not identifier and delegation_deadline is not None and now >= delegation_deadline:
                    result['delegation_grace_elapsed'] = True
                    break
                end = min(deadline, delegation_deadline) if not identifier and delegation_deadline is not None else deadline
                remaining = end - now
                if remaining <= 0:
                    result['observation_window_elapsed'] = True
                    break
                try:
                    queued = await asyncio.wait_for(queue.get(), min(remaining, 0.25))
                except asyncio.TimeoutError:
                    # Only the queue wait is cancelled; the generator keeps reading.
                    continue
                if queued is None:
                    result['reader_ended_early'] = asyncio.get_running_loop().time() < deadline
                    break
                event = queued[0]
                result['events'].append(event)
                if probe is not None:
                    if await probe.event(event):
                        break
                    continue
                if turn(event, 'user') and delegation_deadline is None:
                    delegation_deadline = asyncio.get_running_loop().time() + ctx.delegation_grace_s
                new_id = delegation_id(event)
                if new_id:
                    identifier = new_id
                    if mode in ('session', 'open', 'answered', 'lifecycle') and not sent:
                        await _frames(ctx, sideband, sentence,
                                      None if mode == 'session' else identifier)
                        result['sent_text'] = sentence
                        result['sent_after_event'] = len(result['events'])
                        sent = True
                if turn(event, 'assistant') and sent and not answered:
                    answered = True
                    if mode == 'answered':
                        await _frames(ctx, sideband, sentence, identifier)
                        result['sent_after_event'] = len(result['events'])
                        result['answered_delegation_reused'] = True
                    elif mode == 'lifecycle':
                        await sideband.send(ctx.user_text_frame(inputs(ctx)[1][0]))
                        result['next_turn_input'] = 'typed; second voice injection is unavailable in Peer contract'
                elif mode == 'lifecycle' and answered and turn(event, 'user'):
                    result['next_user_turn'] = True
                    break
                if turn(event, 'assistant'):
                    if identifier and mode != 'lifecycle':
                        if mode != 'answered' or result.get('second_reply_seen'):
                            break
                        if result.get('answered_delegation_reused'):
                            result['second_reply_seen'] = True
        result['received_audio_ms'] = peer.received_audio_ms
    finally:
        if probe is not None:
            await probe.close()
        if reader is not None:
            reader.cancel()
            await asyncio.gather(reader, return_exceptions=True)
        for name, resource in (('sideband', sideband), ('peer', peer)):
            if resource is not None:
                try:
                    await asyncio.wait_for(resource.close(), timeout=ctx.cleanup_timeout_s)
                except (Exception, asyncio.CancelledError) as error:
                    cleanup_errors.append(name + ':' + type(error).__name__)
        result['clean_close'] = not cleanup_errors
        if cleanup_errors:
            result['cleanup_errors'] = cleanup_errors
        # Preserve partial observations even when cancellation interrupts the call.
        ctx.recorder.event('observation', result)
    events = result['events']
    result['event_order'] = [e.get('type', '<unknown>') if isinstance(e, dict) else '<unknown>' for e in events]
    result['event_types'] = sorted(set(str(t) for t in result['event_order']))
    result['delegation_created'] = any(isinstance(e, dict) and e.get('type') == 'delegation.created' for e in events)
    result['user_transcripts'] = [turn_text(e) for e in events if turn(e, 'user')]
    result['assistant_text'] = [turn_text(e) for e in events if turn(e, 'assistant')]
    result['delegated_text'] = [text for e in events if (text := delegated_text(e)) is not None]
    result['errors'] = [e for e in events if isinstance(e, dict) and 'error' in str(e.get('type', '')).lower()]
    if sentence is not None:
        texts = [turn_text(e) for e in events[result.get('sent_after_event', len(events)):] if turn(e, 'assistant')]
        result['reply_text'] = texts
        result['comparison'] = 'exact' if sentence in texts else ('rewritten' if texts else 'absent')
        result['spoken_audio_observed'] = result['received_audio_ms'] > 0
    return result


async def _execute(ctx, name, body):
    ctx.paths['experiment'] = name
    observations = {'experiment': name, 'runs': []}
    async def bounded_call(session, wav, **kwargs):
        ctx.recorder.write_session(session)
        ctx.recorder.event('session', session)
        result = await asyncio.wait_for(_call(ctx, session, wav, **kwargs),
                                        ctx.timeout_s + ctx.observation_s + 2 * ctx.cleanup_timeout_s)
        observations['runs'].append(result)
        return result
    try:
        await asyncio.wait_for(body(bounded_call), experiment_timeout(ctx, name))
        runs = observations['runs']
        complete = bool(runs) and all(r.get('create_ok') and r.get('clean_close') and
                                     not r.get('reader_error') for r in runs)
        if name == 'E0':
            complete = complete and all(r.get('sideband_connected') for r in runs)
        elif name == 'E1':
            complete = complete and all(r.get('delegation_created') and
                                       any(text.strip() for text in r.get('user_transcripts', [])) for r in runs)
        else:
            complete = complete and all(r.get('delegation_created') for r in runs)
            if name == 'E4':
                complete = complete and all(r.get('next_user_turn') for r in runs)
            elif name in ('E3', 'E5'):
                complete = complete and all(r.get('comparison') != 'absent' for r in runs)
            elif name == 'E6':
                complete = complete and all(r.get('fact_used') for r in runs)
        observations['pass'] = bool(complete)
    except asyncio.TimeoutError:
        observations.update({'pass': False, 'error': 'timeout'})
    except LabError:
        observations.update({'pass': False, 'error': 'LabError'})
    except Exception as error:
        observations.update({'pass': False, 'error': type(error).__name__})
    observations['criterion'] = ('Protocol/evidence collection only; semantic rule compliance, filler meaning, '
                                 'and production acceptance require human review. E1 requires transcript and delegation; '
                                 'E3/E5 require a reply, not necessarily exact wording. E4 requires next user turn. '
                                 'E6 requires the seeded fact in assistant text.')
    ctx.recorder.write_summary('# ' + name + '\n\n' + ('PASS' if observations['pass'] else 'FAIL') +
                               '\n\n```json\n' + json.dumps(observations, ensure_ascii=True, indent=2) + '\n```\n')
    return observations


async def E0(ctx):
    async def body(call):
        await call(ctx.upstream.build_session(instructions=''), None, mode='silence')
    return await _execute(ctx, 'E0', body)


async def E1(ctx):
    async def body(call):
        await call(ctx.upstream.build_session(instructions=''), audio(ctx))
    return await _execute(ctx, 'E1', body)


async def E2(ctx):
    async def body(call):
        instructions, utterances = inputs(ctx)
        for index, text in enumerate(utterances, 1):
            result = await call(ctx.upstream.build_session(instructions=instructions), audio(ctx, index))
            result['utterance'] = text
    return await _execute(ctx, 'E2', body)


async def E3(ctx):
    async def body(call):
        instructions, utterances = inputs(ctx)
        for mode in ('session', 'open', 'answered'):
            result = await call(ctx.upstream.build_session(instructions=instructions), audio(ctx, 2),
                                mode=mode, sentence=utterances[0])
            result['variant'] = mode
    return await _execute(ctx, 'E3', body)


async def E4(ctx):
    async def body(call):
        instructions, utterances = inputs(ctx)
        result = await call(ctx.upstream.build_session(instructions=instructions), audio(ctx, 2),
                            mode='lifecycle', sentence=utterances[0])
        result['completion_like_types'] = [t for t in result['event_types']
                                           if any(word in t.lower() for word in ('complet', 'clos', 'done', 'finish', 'end'))]
        result['codex_comparison'] = 'Manual comparison with CompletedHandoff required; event names alone do not establish lifecycle semantics.'
    return await _execute(ctx, 'E4', body)


async def E5(ctx):
    async def body(call):
        instructions, utterances = inputs(ctx)
        for prefix in ('', 'Read this to the user exactly as written: '):
            result = await call(ctx.upstream.build_session(instructions=instructions), audio(ctx, 2),
                                mode='open', sentence=prefix + utterances[0])
            result['content_comparison'] = ('exact' if utterances[0] in result['reply_text'] else
                                            ('rewritten' if result['reply_text'] else 'absent'))
            result['prefixed'] = bool(prefix)
    return await _execute(ctx, 'E5', body)


async def E6(ctx):
    async def body(call):
        instructions, _ = inputs(ctx)
        fact = 'My license plate is LAB-482.'
        for role in ('user', 'developer'):
            session = ctx.upstream.build_session(instructions=instructions,
                                                initial_items=[ctx.upstream.build_message_item(role, fact)])
            result = await call(session, audio(ctx, 5))
            result['initial_role'] = role
            result['fact_used'] = any('LAB-482' in str(text) for text in result['assistant_text'])
    return await _execute(ctx, 'E6', body)


async def E7(ctx):
    async def body(call):
        instructions, utterances = inputs(ctx)
        for filler in (True, False):
            for index, text in enumerate(utterances, 1):
                result = await call(ctx.upstream.build_session(instructions=instructions, ack_filler=filler), audio(ctx, index))
                result.update({'ack_filler': filler, 'utterance': text})
    return await _execute(ctx, 'E7', body)


EXPERIMENTS = {f'E{i}': globals()[f'E{i}'] for i in range(8)}
