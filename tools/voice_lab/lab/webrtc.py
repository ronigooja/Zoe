"""Test-only WAV framing and a lazily loaded WebRTC peer; no product imports."""

import asyncio
import importlib
import struct
import wave
from dataclasses import dataclass
from fractions import Fraction
from typing import Iterator

from .errors import LabError


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    sample_width: int
    frames: int
    duration_ms: float


def read_wav(path: str) -> WavInfo:
    """Inspect uncompressed 16-bit PCM WAV; mono and stereo are accepted."""
    try:
        with wave.open(str(path), 'rb') as source:
            rate, channels = source.getframerate(), source.getnchannels()
            width, frames = source.getsampwidth(), source.getnframes()
            if source.getcomptype() != 'NONE' or width != 2:
                raise LabError('WAV must contain uncompressed 16-bit PCM audio')
            if channels not in (1, 2):
                raise LabError('WAV must be mono or stereo')
            if rate <= 0:
                raise LabError('WAV sample rate must be positive')
            return WavInfo(rate, channels, width, frames, frames * 1000 / rate)
    except (wave.Error, EOFError):
        raise LabError('Unable to read WAV; expected uncompressed 16-bit PCM audio') from None
    except OSError:
        raise LabError('Unable to open WAV input') from None


def _frame_samples(frame_ms: int, rate: int) -> int:
    if not isinstance(frame_ms, int) or not isinstance(rate, int):
        raise LabError('Frame duration and sample rate must be integers')
    if frame_ms <= 0 or rate <= 0 or frame_ms * rate % 1000:
        raise LabError('Frame duration and rate must produce a positive whole sample count')
    return frame_ms * rate // 1000


def pcm_frames(path: str, frame_ms: int = 20,
               target_rate: int = 48000) -> Iterator[bytes]:
    """Yield fixed-size little-endian mono s16 frames, padding the last with silence.

    This test tool averages stereo channels and uses stdlib-only linear
    interpolation (no anti-alias filter), not a production-quality resampler.
    Prepared utterances are short; the source samples are held in memory.
    """
    count = _frame_samples(frame_ms, target_rate)
    info = read_wav(path)
    with wave.open(str(path), 'rb') as source:
        raw = source.readframes(info.frames)
    if len(raw) != info.frames * info.channels * 2:
        raise LabError('WAV PCM data is truncated')
    samples = [sum(frame) // info.channels
               for frame in struct.iter_unpack('<' + 'h' * info.channels, raw)]
    total = (len(samples) * target_rate + info.sample_rate - 1) // info.sample_rate
    for start in range(0, total, count):
        output = []
        for index in range(start, min(start + count, total)):
            left, fraction = divmod(index * info.sample_rate, target_rate)
            a = samples[left]
            b = samples[min(left + 1, len(samples) - 1)]
            output.append(round(a + (b - a) * fraction / target_rate))
        output.extend([0] * (count - len(output)))
        yield struct.pack('<' + 'h' * count, *output)


def silence_frames(duration_ms: int, frame_ms: int = 20,
                   rate: int = 48000) -> Iterator[bytes]:
    """Yield silence rounded up to whole frames, including a padded final frame."""
    count = _frame_samples(frame_ms, rate)
    if not isinstance(duration_ms, int) or duration_ms < 0:
        raise LabError('Silence duration must be a non-negative integer')
    for _ in range((duration_ms + frame_ms - 1) // frame_ms):
        yield bytes(count * 2)


def _dependencies():
    modules = []
    for name in ('aiortc', 'av'):
        try:
            modules.append(importlib.import_module(name))
        except ImportError:
            raise LabError(f'WebRTC requires the optional package {name}; '
                           'install the lab requirements in its virtual environment') from None
    return tuple(modules)


class Peer:
    def __init__(self, recorder=None):
        self.recorder = recorder
        self._connection = None
        self._track = None
        self._tasks = set()
        self._received_audio_ms = 0.0
        self._closed = False

    @property
    def received_audio_ms(self):
        return self._received_audio_ms

    async def create_offer(self, wav_path: str | None) -> str:
        rtc, av = _dependencies()
        if self._closed or self._connection is not None:
            raise LabError('Peer is closed or already has an offer')
        if wav_path is not None:
            read_wav(wav_path)

        class WavTrack(rtc.AudioStreamTrack):
            def __init__(self):
                super().__init__()
                self.frames = iter(pcm_frames(wav_path)) if wav_path is not None else iter(())
                self.pts = 0
                self.started = None

            async def recv(self):
                loop = asyncio.get_running_loop()
                if self.started is None:
                    self.started = loop.time()
                await asyncio.sleep(max(0, self.started + self.pts / 48000 - loop.time()))
                payload = next(self.frames, None)
                if payload is None:
                    payload = bytes(960 * 2)
                frame = av.AudioFrame(format='s16', layout='mono', samples=960)
                frame.planes[0].update(payload)
                frame.sample_rate = 48000
                frame.time_base = Fraction(1, 48000)
                frame.pts = self.pts
                self.pts += 960
                return frame

        # No implicit public STUN service. Proxy media/ICE support remains unverified.
        connection = rtc.RTCPeerConnection(rtc.RTCConfiguration(iceServers=[]))
        self._connection = connection
        self._track = WavTrack()
        connection.addTransceiver(self._track, direction='sendrecv')
        # Codex voice-host/src/transport.rs:113 and README.md:20 require this
        # ordered channel for transport readiness; control events use sideband.
        connection.createDataChannel('oai-events', ordered=True)

        @connection.on('track')
        def on_track(track):
            if track.kind == 'audio':
                task = asyncio.create_task(self._receive(track, rtc.mediastreams.MediaStreamError))
                self._tasks.add(task)
                task.add_done_callback(self._tasks.discard)

        gathered = asyncio.Event()

        @connection.on('icegatheringstatechange')
        def on_gathering():
            if connection.iceGatheringState == 'complete':
                gathered.set()

        try:
            await asyncio.wait_for(
                connection.setLocalDescription(await connection.createOffer()), timeout=30)
            if connection.iceGatheringState != 'complete':
                await asyncio.wait_for(gathered.wait(), timeout=30)
            return connection.localDescription.sdp
        except BaseException:
            await self.close()
            raise

    async def _receive(self, track, ended_error):
        try:
            while True:
                frame = await track.recv()
                if frame.sample_rate:
                    self._received_audio_ms += frame.samples * 1000 / frame.sample_rate
                del frame  # Count duration only; never persist remote audio.
        except ended_error:
            pass

    async def set_answer(self, sdp: str):
        rtc, _ = _dependencies()
        if self._connection is None or self._closed:
            raise LabError('Create an offer before applying an answer')
        await self._connection.setRemoteDescription(rtc.RTCSessionDescription(sdp=sdp, type='answer'))

    async def close(self):
        """Close idempotently; cleanup without optional packages is always safe."""
        self._closed = True
        tasks = list(self._tasks)
        for task in tasks:
            task.cancel()
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)
        if self._track is not None:
            self._track.stop()
        if self._connection is not None:
            await self._connection.close()


def peer_factory(recorder=None):
    """Replace/inject this callable in the runner for offline orchestration tests."""
    return Peer(recorder=recorder)
