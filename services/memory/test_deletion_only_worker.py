from __future__ import annotations

import os
import io
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

from .config import WorkerConfig
from .control_api import ControlAPI, ControlAPIError
from .worker import DeletionOnlyAdapters, MemoryWorker, PermanentJobError


class DeletionOnlyWorkerTests(unittest.TestCase):
    def config(self) -> WorkerConfig:
        return WorkerConfig(
            control_url="http://control.invalid",
            runner_token="fixture-token",
            qdrant_url="http://qdrant.invalid",
            qdrant_api_key=None,
            qdrant_collection="qdrant-fixture",
            mem0_collection="mem0-fixture",
            embedder_model="unused",
            embedder_revision="unused",
            llm_provider="openai",
            llm_model="",
            llm_base_url=None,
            llm_api_key=None,
            reasoning_effort="",
            poll_seconds=0.1,
            batch_size=1,
            extractor_version="fixture",
            deletion_only=True,
        )

    def test_environment_mode_does_not_require_or_load_model_configuration(self) -> None:
        env = {
            "ZOE_CONTROL_URL": "http://control.invalid",
            "ZOE_RUNNER_TOKEN": "fixture-token",
            "ZOE_MEMORY_DELETION_ONLY": "1",
        }
        with patch.dict(os.environ, env, clear=True), patch("services.memory.worker.LocalEmbedder") as embedder, patch(
            "services.memory.worker.ControlledMem0"
        ) as mem0:
            config = WorkerConfig.from_env()
            worker = MemoryWorker(config)

        self.assertTrue(config.deletion_only)
        self.assertEqual(config.llm_model, "")
        self.assertIsNone(worker.embedder)
        self.assertIsNone(worker.qdrant)
        self.assertIsNone(worker.mem0)
        embedder.assert_not_called()
        mem0.assert_not_called()

    def test_control_api_selects_only_deletion_routes_in_maintenance_mode(self) -> None:
        api = ControlAPI("http://control.invalid", "fixture-token", deletion_only=True)
        with patch.object(api, "_post", return_value={"items": []}) as post:
            self.assertEqual(api.claim(2), [])

        self.assertEqual(post.call_args_list[0].args[0], "/runner/v1/memory/deletion-jobs/claim")

    def test_unknown_cleanup_target_is_left_as_adapter_missing(self) -> None:
        class FakeAPI:
            failed: tuple[dict, str, bool] | None = None

            def validate(self, job: dict) -> dict:
                return {"allowed": True}

            def fail(self, job: dict, code: str, permanent: bool = False) -> dict:
                self.failed = (job, code, permanent)
                return {}

            def complete(self, job: dict) -> dict:
                raise AssertionError("unsupported target must not be completed")

        worker = MemoryWorker.__new__(MemoryWorker)
        worker.config = self.config()
        worker.api = FakeAPI()
        worker.deletion_adapters = None
        worker.run_job({"id": "cleanup-1", "kind": "delete_cleanup", "target": "unknown_target"})

        self.assertIsNotNone(worker.api.failed)
        self.assertEqual(worker.api.failed[1:], ("adapter_missing", True))

    def test_backup_manifest_target_completes_only_after_control_api_success(self) -> None:
        class FakeAPI:
            completed = False
            failed = None
            calls = []

            def validate(self, job):
                return {"allowed": True}

            def cleanup_backup_delete_manifest(self, job):
                self.calls.append(job)
                return {"status": "completed"}

            def complete(self, job):
                self.completed = True
                return {}

            def fail(self, job, code, permanent=False):
                self.failed = (code, permanent)
                return {}

        worker = MemoryWorker.__new__(MemoryWorker)
        worker.config = self.config()
        worker.api = FakeAPI()
        worker.deletion_adapters = None
        job = {
            "id": "cleanup-backup", "attempt": 1, "kind": "delete_cleanup", "target": "backup_delete_manifest",
            "workspace_id": "default", "entity_id": "claim", "entity_version": 2,
        }
        worker.run_job(job)
        self.assertEqual(worker.api.calls, [job])
        self.assertFalse(worker.api.completed)
        self.assertIsNone(worker.api.failed)

    def test_cleanup_target_controls_whether_worker_calls_generic_complete(self) -> None:
        for deletion_only in (False, True):
            for target, method, adapter_completes in (
                ("export_temp", "cleanup_export_temp", False),
                ("backup_delete_manifest", "cleanup_backup_delete_manifest", True),
            ):
                with self.subTest(deletion_only=deletion_only, target=target):
                    class FakeAPI:
                        def __init__(self):
                            self.calls = []

                        def __getattr__(self, name):
                            if name in ("cleanup_export_temp", "cleanup_backup_delete_manifest"):
                                def cleanup(job):
                                    self.calls.append(name)
                                    return {"status": "completed"}
                                return cleanup
                            raise AssertionError(f"unexpected control API call: {name}")

                        def complete(self, job):
                            self.calls.append("complete")
                            return {}

                        def fail(self, job, code, permanent=False):
                            raise AssertionError(f"cleanup unexpectedly failed: {code}")

                    worker = MemoryWorker.__new__(MemoryWorker)
                    worker.config = replace(self.config(), deletion_only=deletion_only)
                    worker.api = FakeAPI()
                    worker.deletion_adapters = None
                    worker.qdrant = None
                    worker.mem0 = None
                    worker.embedder = None
                    worker.run_job({
                        "id": f"cleanup-{target}", "attempt": 1, "kind": "delete_cleanup", "target": target,
                        "workspace_id": "default", "entity_id": "claim", "entity_version": 2,
                    })
                    expected_calls = [method] + ([] if adapter_completes else ["complete"])
                    self.assertEqual(worker.api.calls, expected_calls)

    def test_pending_cleanup_response_retries_instead_of_completing(self) -> None:
        class FakeAPI:
            completed = False
            failed = None

            def validate(self, job):
                return {"allowed": True}

            def cleanup_export_temp(self, job):
                raise ControlAPIError(202, "export_active")

            def complete(self, job):
                self.completed = True
                return {}

            def fail(self, job, code, permanent=False):
                self.failed = (code, permanent)
                return {}

        worker = MemoryWorker.__new__(MemoryWorker)
        worker.config = self.config()
        worker.api = FakeAPI()
        worker.deletion_adapters = None
        worker.run_job({
            "id": "cleanup-export", "attempt": 1, "kind": "delete_cleanup", "target": "export_temp",
            "workspace_id": "default", "entity_id": "claim", "entity_version": 2,
        })
        self.assertFalse(worker.api.completed)
        self.assertEqual(worker.api.failed, ("export_active", False))

    def test_export_registry_errors_remain_pending_with_stable_code(self) -> None:
        class FakeAPI:
            completed = False
            failed = None

            def validate(self, job):
                return {"allowed": True}

            def cleanup_export_temp(self, job):
                raise ControlAPIError(503, "registry_unavailable")

            def complete(self, job):
                self.completed = True
                return {}

            def fail(self, job, code, permanent=False):
                self.failed = (code, permanent)
                return {}

        worker = MemoryWorker.__new__(MemoryWorker)
        worker.config = self.config()
        worker.api = FakeAPI()
        worker.deletion_adapters = None
        worker.run_job({
            "id": "cleanup-export", "attempt": 1, "kind": "delete_cleanup", "target": "export_temp",
            "workspace_id": "default", "entity_id": "claim", "entity_version": 2,
        })
        self.assertFalse(worker.api.completed)
        self.assertEqual(worker.api.failed, ("registry_unavailable", False))

    def test_unregistered_export_artifact_retries_with_stable_code(self) -> None:
        class FakeAPI:
            completed = False
            failed = None

            def validate(self, job):
                return {"allowed": True}

            def cleanup_export_temp(self, job):
                raise ControlAPIError(503, "unregistered_artifact")

            def complete(self, job):
                self.completed = True
                return {}

            def fail(self, job, code, permanent=False):
                self.failed = (code, permanent)
                return {}

        worker = MemoryWorker.__new__(MemoryWorker)
        worker.config = self.config()
        worker.api = FakeAPI()
        worker.deletion_adapters = None
        worker.run_job({
            "id": "cleanup-export", "attempt": 1, "kind": "delete_cleanup", "target": "export_temp",
            "workspace_id": "default", "entity_id": "claim", "entity_version": 2,
        })
        self.assertFalse(worker.api.completed)
        self.assertEqual(worker.api.failed, ("unregistered_artifact", False))

    def test_backup_manifest_endpoint_payload_is_job_bound(self) -> None:
        api = ControlAPI("http://control.invalid", "fixture-token", deletion_only=True)
        job = {
            "id": "job-id", "attempt": 4, "workspace_id": "default",
            "entity_id": "claim-id", "entity_version": 5,
        }
        with patch.object(api, "_post", return_value={"status": "completed"}) as post:
            api.cleanup_backup_delete_manifest(job)
        self.assertEqual(post.call_args.args[0], "/runner/v1/memory/backup-delete-manifest/cleanup")
        self.assertEqual(post.call_args.args[1], {
            "workspace_id": "default", "entity_id": "claim-id", "entity_version": 5,
            "attempt": 4, "job_id": "job-id",
        })

    def test_control_api_preserves_pending_response_code(self) -> None:
        api = ControlAPI("http://control.invalid", "fixture-token", deletion_only=True)
        response = io.BytesIO(b'{"status":"pending","code":"export_active"}')
        response.status = 202
        with patch("services.memory.control_api.urllib.request.urlopen", return_value=response):
            with self.assertRaises(ControlAPIError) as raised:
                api.cleanup_export_temp({
                    "id": "job-id", "attempt": 1, "workspace_id": "default",
                    "entity_id": "claim", "entity_version": 2,
                })
        self.assertEqual((raised.exception.status, raised.exception.code), (202, "export_active"))

    def test_cleanup_requires_exact_http_200_and_completed_json(self) -> None:
        api = ControlAPI("http://control.invalid", "fixture-token", deletion_only=True)
        job = {
            "id": "job-id", "attempt": 1, "workspace_id": "default",
            "entity_id": "claim", "entity_version": 2,
        }
        for status, body in ((201, b'{"status":"completed"}'), (204, b""), (200, b"not-json")):
            response = io.BytesIO(body)
            response.status = status
            with self.subTest(status=status), patch("services.memory.control_api.urllib.request.urlopen", return_value=response):
                with self.assertRaises((ControlAPIError, ValueError)) as raised:
                    api.cleanup_backup_delete_manifest(job)
                if status != 200:
                    self.assertIsInstance(raised.exception, ControlAPIError)
                    self.assertEqual(raised.exception.status, status)

        response = io.BytesIO(b'{"status":"completed"}')
        response.status = 200
        with patch("services.memory.control_api.urllib.request.urlopen", return_value=response):
            self.assertEqual(api.cleanup_backup_delete_manifest(job), {"status": "completed"})

        response = io.BytesIO(b'{"status":"pending"}')
        response.status = 200
        with patch("services.memory.control_api.urllib.request.urlopen", return_value=response):
            with self.assertRaises(ControlAPIError) as raised:
                api.cleanup_backup_delete_manifest(job)
        self.assertEqual((raised.exception.status, raised.exception.code), (503, "cleanup_incomplete"))

    def test_missing_mem0_history_store_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            adapter = DeletionOnlyAdapters.__new__(DeletionOnlyAdapters)
            class FakeQdrant:
                deletes = 0

                def delete(self, **kwargs) -> None:
                    self.deletes += 1

            adapter.client = FakeQdrant()
            adapter.mem0_collection = "mem0-fixture"
            adapter.history_path = Path(directory) / "missing-history.sqlite"

            class FakeAPI:
                failed: tuple[dict, str, bool] | None = None

                def validate(self, job: dict) -> dict:
                    return {"allowed": True}

                def fail(self, job: dict, code: str, permanent: bool = False) -> dict:
                    self.failed = (job, code, permanent)
                    return {}

                def complete(self, job: dict) -> dict:
                    raise AssertionError("missing history store must not complete cleanup")

            worker = MemoryWorker.__new__(MemoryWorker)
            worker.config = self.config()
            worker.api = FakeAPI()
            worker.deletion_adapters = adapter
            worker.run_job({
                "id": "cleanup-mem0", "attempt": 1, "kind": "delete_cleanup", "target": "mem0",
                "workspace_id": "workspace", "entity_id": "claim", "entity_version": 2,
            })

            self.assertEqual(adapter.client.deletes, 1)
            self.assertIsNotNone(worker.api.failed)
            self.assertEqual(worker.api.failed[1:], ("adapter_missing", True))


if __name__ == "__main__":
    unittest.main()
