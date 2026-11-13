from __future__ import annotations

import json
import os
from pathlib import Path
import contextlib
import importlib
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

from data_lifecycle.retention import read_backup_delete_manifest


class BackupDeleteManifestTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.journal = self.root / "journal"
        self.manifest = self.root / "manifest"
        self.record = {
            "sequence": 1, "lineage_id": "fixture-lineage", "generation": 2,
            "workspace_id": "default", "entity_kind": "test_record", "entity_id": "claim-fixture",
            "entity_version": 3, "actor_id": "fixture-owner", "reason_code": "user_requested",
            "deleted_at": "2026-10-21T00:00:00Z", "cleanup_targets": ["qdrant", "backup_delete_manifest"],
            "previous_hash": "", "hash": "a" * 64,
        }
        self.journal.write_text(json.dumps(self.record) + "\n")
        os.chmod(self.journal, 0o600)
        self.metadata = {"snapshot_lineage_id": "fixture-lineage", "snapshot_sequence": 1}

    def tearDown(self):
        self.temp.cleanup()

    def write_manifest(self, deletion=None):
        row = deletion or {
            "version": 1, "sequence": 1, "baseline": "fixture-lineage",
            "targets": self.record["cleanup_targets"], "source_hash": self.record["hash"],
        }
        self.manifest.write_text(json.dumps(row) + "\n")
        os.chmod(self.manifest, 0o600)

    def test_manifest_must_match_frozen_authority_journal(self):
        self.write_manifest()
        result = read_backup_delete_manifest(self.manifest, self.journal, self.metadata)
        self.assertEqual(result, {
            "baseline": "fixture-lineage", "sequence": 1,
            "targets": ["backup_delete_manifest", "qdrant"], "version": 1,
        })

    def test_missing_or_stale_manifest_fails_when_journal_has_deletions(self):
        with self.assertRaisesRegex(RuntimeError, "not configured"):
            read_backup_delete_manifest(None, self.journal, self.metadata)
        with self.assertRaisesRegex(RuntimeError, "missing"):
            read_backup_delete_manifest(self.root / "absent", self.journal, self.metadata)
        self.write_manifest()
        self.manifest.write_text("")
        os.chmod(self.manifest, 0o600)
        with self.assertRaisesRegex(RuntimeError, "not complete"):
            read_backup_delete_manifest(self.manifest, self.journal, self.metadata)

    def test_symlink_manifest_fails_closed(self):
        self.write_manifest()
        link = self.root / "manifest-link"
        link.symlink_to(self.manifest)
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            read_backup_delete_manifest(link, self.journal, self.metadata)

    def test_backup_cli_rejects_deletion_journal_without_configured_manifest(self):
        database = self.root / "source.sqlite"
        database.touch()
        Path(str(database) + ".deletion-ledger").write_text(json.dumps(self.record) + "\n")
        destination = self.root / "backups"
        destination.mkdir(mode=0o700)
        common_stub = types.ModuleType("data_lifecycle.common")
        for name in (
            "FORMAT", "CHUNK", "copy_payload_path", "create_archive", "database_fence",
            "encrypt_file", "file_record", "load_inventory", "private_dir", "read_key",
            "run_volume_quiesce_adapter", "sqlite_snapshot", "independent_deletion_journal_metadata",
        ):
            setattr(common_stub, name, lambda *args, **kwargs: None)
        arguments = [
            "zoe-backup", "--database", str(database), "--destination", str(destination),
            "--key-file", str(self.root / "key"),
        ]
        head = {"snapshot_lineage_id": "fixture-lineage", "snapshot_sequence": 1}
        with patch.dict(sys.modules, {"data_lifecycle.common": common_stub}):
            zoe_backup = importlib.import_module("zoe_backup")
        with patch.dict(os.environ, {}, clear=True), patch.object(sys, "argv", arguments), \
                patch.object(zoe_backup, "private_dir"), patch.object(zoe_backup, "read_key", return_value=b"k" * 32), \
                patch.object(zoe_backup, "load_inventory", return_value={
                    "roots": {}, "service_config_paths": [], "artifact_paths": [],
                    "persistent_volume_paths": [], "secret_refs": [],
                }), \
                patch.object(zoe_backup, "database_fence", return_value=contextlib.nullcontext(database)), \
                patch.object(zoe_backup, "sqlite_snapshot", return_value={"instance_state": None}), \
                patch.object(zoe_backup, "independent_deletion_journal_metadata", return_value=head):
            with self.assertRaisesRegex(RuntimeError, "not configured"):
                zoe_backup.main()
        self.assertEqual(list(destination.iterdir()), [])

    def test_empty_journal_without_adapter_is_explicitly_not_applicable(self):
        self.journal.write_text("")
        self.metadata["snapshot_sequence"] = 0
        self.assertEqual(read_backup_delete_manifest(None, self.journal, self.metadata), {
            "baseline": "fixture-lineage", "sequence": 0, "targets": [], "version": 1,
        })
        self.metadata["snapshot_sequence"] = 1
        with self.assertRaisesRegex(RuntimeError, "journal is empty"):
            read_backup_delete_manifest(None, self.journal, self.metadata)

    def test_global_sequences_can_arrive_out_of_order_without_identity_fields(self):
        second = {**self.record, "sequence": 2, "lineage_id": "fixture-lineage", "entity_id": "claim-b", "hash": "c" * 64}
        self.journal.write_text(json.dumps(self.record) + "\n" + json.dumps(second) + "\n")
        os.chmod(self.journal, 0o600)
        self.metadata["snapshot_sequence"] = 2
        rows = [
            {"version": 1, "sequence": 2, "baseline": "fixture-lineage", "targets": second["cleanup_targets"], "source_hash": second["hash"]},
            {"version": 1, "sequence": 1, "baseline": "fixture-lineage", "targets": self.record["cleanup_targets"], "source_hash": self.record["hash"]},
        ]
        self.manifest.write_text("".join(json.dumps(row) + "\n" for row in rows))
        os.chmod(self.manifest, 0o600)
        result = read_backup_delete_manifest(self.manifest, self.journal, self.metadata)
        self.assertEqual(result["sequence"], 2)
        self.assertEqual(result["baseline"], "fixture-lineage")


if __name__ == "__main__":
    unittest.main()
