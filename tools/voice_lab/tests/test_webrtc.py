"""Small stdlib-only tests; no sockets, native media packages, or TTS processes."""

import importlib.util
import struct
import tempfile
import unittest
import wave
from pathlib import Path
from unittest.mock import patch

from lab.webrtc import LabError, Peer, pcm_frames, read_wav, silence_frames, peer_factory


_SPEC = importlib.util.spec_from_file_location(
    'voice_lab_gen_audio', Path(__file__).resolve().parents[1] / 'cases' / 'gen_audio.py')
gen_audio = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(gen_audio)


class WavTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='voice-lab-wav-')
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / 'input.wav'

    def write_wav(self, rate=16000, channels=1, width=2, samples=None):
        if samples is None:
            samples = [1000] * (rate // 100 * channels)
        with wave.open(str(self.path), 'wb') as output:
            output.setnchannels(channels)
            output.setsampwidth(width)
            output.setframerate(rate)
            output.writeframes(struct.pack('<' + ('h' if width == 2 else 'B') * len(samples),
                                           *samples))
        return str(self.path)

    def test_mono_16k_metadata(self):
        info = read_wav(self.write_wav())
        self.assertEqual((info.sample_rate, info.channels, info.sample_width, info.frames),
                         (16000, 1, 2, 160))
        self.assertEqual(info.duration_ms, 10)

    def test_stereo_44100_metadata(self):
        info = read_wav(self.write_wav(rate=44100, channels=2))
        self.assertEqual((info.sample_rate, info.channels, info.frames), (44100, 2, 441))
        self.assertEqual(info.duration_ms, 10)

    def test_reject_8bit(self):
        with self.assertRaisesRegex(LabError, '16-bit PCM'):
            read_wav(self.write_wav(width=1, samples=[128] * 160))

    def test_reject_invalid_wav(self):
        self.path.write_bytes(b'not a wav')
        with self.assertRaisesRegex(LabError, '16-bit PCM'):
            read_wav(str(self.path))

    def test_frames_rate_length_padding(self):
        frames = list(pcm_frames(self.write_wav(samples=[1234] * 400)))
        self.assertEqual(len(frames), 2)  # 25 ms -> two 20 ms frames.
        self.assertTrue(all(len(frame) == 1920 for frame in frames))
        self.assertEqual(struct.unpack('<960h', frames[0]), (1234,) * 960)
        last = struct.unpack('<960h', frames[1])
        self.assertEqual(last[:240], (1234,) * 240)
        self.assertEqual(last[240:], (0,) * 720)

    def test_stereo_downmix_and_nondefault_rate(self):
        path = self.write_wav(rate=44100, channels=2, samples=[1000, 3000] * 441)
        frames = list(pcm_frames(path, frame_ms=10, target_rate=16000))
        self.assertEqual(len(frames), 1)
        self.assertEqual(struct.unpack('<160h', frames[0]), (2000,) * 160)

    def test_linear_interpolation(self):
        frames = list(pcm_frames(self.write_wav(rate=1000, samples=[0, 1000]),
                                 frame_ms=1, target_rate=2000))
        self.assertEqual([struct.unpack('<2h', frame) for frame in frames],
                         [(0, 500), (1000, 1000)])

    def test_empty_wav(self):
        self.assertEqual(list(pcm_frames(self.write_wav(samples=[]))), [])

    def test_truncated_wav(self):
        path = self.write_wav()
        self.path.write_bytes(self.path.read_bytes()[:-2])
        with self.assertRaisesRegex(LabError, 'truncated'):
            list(pcm_frames(path))

    def test_silence(self):
        self.assertEqual(list(silence_frames(41)), [bytes(1920)] * 3)
        self.assertEqual(list(silence_frames(0)), [])
        self.assertEqual(list(silence_frames(10, frame_ms=10, rate=16000)), [bytes(320)])

    def test_bad_frame_arguments(self):
        for arguments in ({'frame_ms': 0}, {'frame_ms': 1, 'rate': 44100},
                          {'rate': -1}, {'duration_ms': -1}):
            with self.subTest(arguments=arguments), self.assertRaises(LabError):
                list(silence_frames(**({'duration_ms': 20} | arguments)))


class PeerTests(unittest.IsolatedAsyncioTestCase):
    async def test_missing_aiortc_is_clear(self):
        peer = Peer()
        with patch('lab.webrtc.importlib.import_module', side_effect=ImportError):
            with self.assertRaisesRegex(LabError, 'aiortc'):
                await peer.create_offer(None)
            with self.assertRaisesRegex(LabError, 'aiortc'):
                await peer.set_answer('fake')
        self.assertEqual(peer.received_audio_ms, 0)
        await peer.close()
        await peer.close()

    async def test_missing_av_is_clear(self):
        with patch('lab.webrtc.importlib.import_module', side_effect=[object(), ImportError()]):
            with self.assertRaisesRegex(LabError, 'package av'):
                await Peer().create_offer(None)

    async def test_duration_only_and_factory_seam(self):
        class Ended(Exception):
            pass

        class Frame:
            sample_rate = 48000
            samples = 960

        class Track:
            remaining = 3

            async def recv(self):
                if not self.remaining:
                    raise Ended()
                self.remaining -= 1
                return Frame()

        recorder = object()
        peer = peer_factory(recorder=recorder)
        self.assertIs(peer.recorder, recorder)
        await peer._receive(Track(), Ended)
        self.assertEqual(peer.received_audio_ms, 60)
        await peer.close()


class AudioGeneratorTests(unittest.TestCase):
    def test_commands_names_and_index(self):
        with tempfile.TemporaryDirectory(prefix='voice-lab-tts-test-') as temporary:
            root = Path(temporary)
            source, output = root / 'utterances.txt', (root / 'audio').resolve()
            source.write_text('first phrase\n\n  second phrase  \n', encoding='utf-8')
            calls = []

            def runner(command, **kwargs):
                calls.append(command)
                self.assertTrue(kwargs['check'])
                self.assertTrue(Path(command[-2] if command[0] == 'afconvert'
                                     else command[4]).is_relative_to(output))
                Path(command[-1] if command[0] == 'afconvert' else command[4]).touch()

            gen_audio.generate_audio(source, output, runner=runner,
                                     which=lambda name: '/fake/' + name, platform='darwin')
            self.assertEqual(len(calls), 4)
            self.assertEqual(calls[0][:4], ['say', '-v', 'Tingting', '-o'])
            self.assertEqual(calls[0][-1], 'first phrase')
            self.assertEqual(calls[2][-1], 'second phrase')
            self.assertEqual(calls[1][:7],
                             ['afconvert', '-f', 'WAVE', '-d', 'LEI16@16000', '-c', '1'])
            self.assertEqual(Path(calls[1][-1]).name, '01.wav')
            self.assertEqual(Path(calls[3][-1]).name, '02.wav')
            self.assertEqual((output / 'index.txt').read_text(),
                             '01.wav\tfirst phrase\n02.wav\tsecond phrase\n')
            self.assertEqual(sorted(path.name for path in output.iterdir()),
                             ['01.wav', '02.wav', 'index.txt'])

    def test_voice_override(self):
        with patch.object(gen_audio, 'generate_audio') as generate:
            gen_audio.main(['--utterances', 'cases/utterances.txt', '--out', 'audio',
                            '--voice', 'CustomVoice'])
            generate.assert_called_once_with('cases/utterances.txt', 'audio', 'CustomVoice')

    def test_missing_say_refused_before_output_created(self):
        with tempfile.TemporaryDirectory(prefix='voice-lab-tts-test-') as temporary:
            output = Path(temporary) / 'audio'
            with self.assertRaisesRegex(ValueError, 'say'):
                gen_audio.generate_audio('unused.txt', output, platform='darwin',
                                         which=lambda _: None)
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
