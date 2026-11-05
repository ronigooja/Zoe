from __future__ import annotations

import os
import unittest
from unittest.mock import patch

from .config import WorkerConfig


class ReasoningEffortConfigTests(unittest.TestCase):
    def test_supported_efforts_and_empty_default(self) -> None:
        for effort in ("", "none", "low", "medium", "high", "max"):
            with self.subTest(effort=effort), patch.dict(os.environ, {
                "ZOE_CONTROL_URL": "http://localhost:8080",
                "ZOE_RUNNER_TOKEN": "test-token",
                "ZOE_MEMORY_DELETION_ONLY": "true",
                "ZOE_MEM0_LLM_REASONING_EFFORT": effort,
            }, clear=True):
                self.assertEqual(WorkerConfig.from_env().reasoning_effort, effort)

    def test_minimal_and_unknown_efforts_are_rejected(self) -> None:
        for effort in ("minimal", "xhigh", "invalid"):
            with self.subTest(effort=effort), patch.dict(os.environ, {
                "ZOE_MEMORY_DELETION_ONLY": "true",
                "ZOE_MEM0_LLM_REASONING_EFFORT": effort,
            }, clear=True):
                with self.assertRaisesRegex(ValueError, "ZOE_MEM0_LLM_REASONING_EFFORT"):
                    WorkerConfig.from_env()
