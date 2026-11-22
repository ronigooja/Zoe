"""Prompt lifecycle checks without wording locks on operator-owned instructions."""
import dataclasses
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from services.memory.prompts import MemoryPrompts, NAMES, load_prompts
from services.memory.limits import defaults


class PromptTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        for name in NAMES:
            (self.directory / (name + ".txt")).write_bytes((name + "\n").encode())

    def test_each_failure(self):
        for name in NAMES:
            path = self.directory / (name + ".txt")
            for data, category in ((None, "missing"), (b"", "empty"), (b"\n", "empty"),
                                   (b"\xffprivate", "invalid_utf8"), (b"x" * 16001, "too_large")):
                with self.subTest(name=name, category=category):
                    if data is None:
                        path.unlink()
                    else:
                        path.write_bytes(data)
                    with self.assertRaisesRegex(ValueError, "^prompt_" + category + ": " + name + "$"):
                        load_prompts(self.directory)
                    path.write_bytes(b"fixture\n")
            with patch.object(Path, "open", side_effect=PermissionError):
                with self.assertRaisesRegex(ValueError, "^prompt_unreadable: memory_ingest$"):
                    load_prompts(self.directory)

    def test_bytes_boundary_and_exact_newline(self):
        path = self.directory / "memory_write.txt"
        for data, expected in ((b"x" * 16000, "x" * 16000), (b"text\n\n", "text\n"),
                               (b"text\r\n", "text\r"), (b" ", " "), (b"text", "text")):
            path.write_bytes(data)
            self.assertEqual(load_prompts(self.directory).memory_write, expected)

    def test_override_and_immutable_restart_snapshot(self):
        with patch.dict(os.environ, {"ZOE_PROMPT_DIR": str(self.directory)}):
            prompts = load_prompts()
            self.assertEqual(prompts.memory_ingest, "memory_ingest")
            (self.directory / "memory_ingest.txt").write_bytes(b"changed\n")
            self.assertEqual(prompts.memory_ingest, "memory_ingest")
            self.assertEqual(load_prompts().memory_ingest, "changed")
        with self.assertRaises(dataclasses.FrozenInstanceError):
            prompts.memory_ingest = "replacement"
        with patch.dict(os.environ, {"ZOE_PROMPT_DIR": ""}), patch.object(Path, "open", side_effect=FileNotFoundError) as opened:
            with self.assertRaises(ValueError):
                load_prompts()
            self.assertEqual(opened.call_args, unittest.mock.call("rb"))

    def test_config_identity_excludes_prompts(self):
        from services.memory.memory_api import config_fingerprint
        args = ("https://fixture.invalid", "model", "low", "", "credential")
        before = config_fingerprint(*args)
        with patch.dict(os.environ, {"ZOE_PROMPT_DIR": str(self.directory)}):
            for name in NAMES:
                (self.directory / (name + ".txt")).write_bytes(b"changed\n")
            load_prompts()
            self.assertEqual(config_fingerprint(*args), before)

    def test_worker_fails_before_constructing_backend(self):
        from services.memory import worker
        for name in NAMES:
            path = self.directory / (name + ".txt")
            for data in (None, b"", b"\xff", b"x" * 16001):
                with self.subTest(name=name, data_size=None if data is None else len(data)):
                    if data is None:
                        path.unlink()
                    else:
                        path.write_bytes(data)
                    with patch.dict(os.environ, {"ZOE_PROMPT_DIR": str(self.directory), "ZOE_MEMORY_API_TOKEN_FILE": "fixture"}), \
                            patch.object(worker, "load_limits", return_value=defaults()), \
                            patch.object(worker, "ControlledMem0") as backend, patch.object(worker, "serve_memory_api") as serve:
                        with self.assertRaises(ValueError):
                            worker.main()
                        backend.assert_not_called()
                        serve.assert_not_called()
                    path.write_bytes(b"fixture\n")

    def test_startup_passes_same_snapshot_to_consumers(self):
        from services.memory import worker, memory_api
        prompts = load_prompts(self.directory)
        controlled = Mock(prompts=prompts)
        policy = defaults()
        with patch.object(worker, "load_limits", return_value=policy) as load_limits, \
                patch.object(worker, "load_prompts", return_value=prompts) as loader, \
                patch.object(worker.WorkerConfig, "from_env", return_value="config"), \
                patch.object(worker, "ControlledMem0", return_value=controlled) as backend, \
                patch.object(worker, "create_memory_api", return_value="server") as create, \
                patch.object(worker, "serve_memory_api"), \
                patch.dict(os.environ, {"ZOE_MEMORY_API_TOKEN_FILE": "fixture"}):
            worker.main()
            load_limits.assert_called_once_with()
            loader.assert_called_once_with(limits=policy)
            backend.assert_called_once_with("config", prompts)
            create.assert_called_once_with(controlled, policy)
        with patch.dict(os.environ, {"ZOE_MEMORY_API_TOKEN_FILE": "fixture", "ZOE_MEMORY_API_ADDR": "127.0.0.1:0"}), \
                patch.object(Path, "read_text", return_value="token"), \
                patch.object(memory_api, "PersonMemory") as person, patch.object(memory_api, "MemoryAPIServer"):
            memory_api.create_memory_api(controlled, policy)
            person.assert_called_once_with(controlled.memory, prompts, policy)

    def test_custom_instruction_reaches_mem0(self):
        from services.memory import mem0_backend
        config = Mock(qdrant_url="http://qdrant:6333", qdrant_api_key="", reasoning_effort="")
        prompts = MemoryPrompts("ingest fixture", "write fixture", "custom fixture")
        with patch.object(mem0_backend, "register_pinned_embedder"), \
                patch.object(mem0_backend, "register_responses_provider"), \
                patch.object(mem0_backend.Memory, "from_config") as factory, \
                patch.object(Path, "mkdir"), patch.object(mem0_backend.os, "chmod"):
            backend = mem0_backend.ControlledMem0(config, prompts, embedder=Mock())
            self.assertIs(backend.prompts, prompts)
            self.assertEqual(factory.call_args.args[0]["custom_instructions"], prompts.memory_custom)
