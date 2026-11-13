"""Independent current-journal evidence must extend the selected backup."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from zoe_restore import verify_latest_deletion_head


def record_line(sequence: int, generation: int, previous_hash: str) -> tuple[bytes, str]:
    body = {
        "sequence": sequence,
        "lineage_id": "lineage-fixture",
        "generation": generation,
        "workspace_id": "default",
        "entity_kind": "test_record",
        "entity_id": f"claim-{sequence}",
        "entity_version": 2,
        "actor_id": "owner",
        "reason_code": "user_requested",
        "deleted_at": "2026-10-22T00:00:00Z",
        "cleanup_targets": ["claim", "qdrant"],
        "previous_hash": previous_hash,
    }
    encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
    encoded = encoded.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()
    digest = hashlib.sha256(encoded).hexdigest()
    return (json.dumps({**body, "hash": digest}, ensure_ascii=False, separators=(",", ":")).encode() + b"\n", digest)


class LatestDeletionHeadTests(unittest.TestCase):
    def invoke(self, directory: Path, snapshot_bytes: bytes, latest_bytes: bytes, head: dict) -> dict:
        snapshot = directory / "snapshot.ledger"
        latest = directory / "latest.ledger"
        head_path = directory / "latest-head.json"
        for path, content in ((snapshot, snapshot_bytes), (latest, latest_bytes)):
            path.write_bytes(content)
            path.chmod(0o600)
        head_path.write_text(json.dumps(head), encoding="utf-8")
        head_path.chmod(0o600)
        return verify_latest_deletion_head(
            latest, head_path, snapshot,
            {"lineage_id": "lineage-fixture", "snapshot_lineage_id": "lineage-fixture", "snapshot_sequence": len(snapshot_bytes.splitlines())},
        )

    def test_current_head_extends_snapshot_and_validates_file_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            line, digest = record_line(1, 3, "")
            file_hash = hashlib.sha256(line).hexdigest()
            head = {"lineage_id": "lineage-fixture", "generation": 3, "record_generation": 3, "sequence": 1, "head_hash": digest, "file_sha256": file_hash}
            metadata = self.invoke(base, b"", line, head)
            self.assertEqual(metadata["sequence"], 1)

    def test_restore_generation_without_new_deletion_is_attested_separately(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            line, first_hash = record_line(1, 2, "")
            second, last_hash = record_line(2, 2, first_hash)
            latest = line + second
            head = {
                "lineage_id": "lineage-fixture", "generation": 5, "record_generation": 2,
                "sequence": 2, "head_hash": last_hash, "file_sha256": hashlib.sha256(latest).hexdigest(),
            }
            metadata = self.invoke(base, line, latest, head)
            self.assertEqual(metadata["generation"], 5)
            self.assertEqual(metadata["record_generation"], 2)

    def test_stale_head_cannot_attest_a_later_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            line, digest = record_line(1, 2, "")
            stale_head = {"lineage_id": "lineage-fixture", "generation": 1, "record_generation": 0, "sequence": 0, "head_hash": "", "file_sha256": hashlib.sha256(b"").hexdigest()}
            with self.assertRaisesRegex(RuntimeError, "does not match"):
                self.invoke(base, b"", line, stale_head)

    def test_latest_journal_must_retain_exact_snapshot_prefix(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            line, digest = record_line(1, 2, "")
            head = {"lineage_id": "lineage-fixture", "generation": 2, "record_generation": 2, "sequence": 1, "head_hash": digest, "file_sha256": hashlib.sha256(line).hexdigest()}
            with self.assertRaisesRegex(RuntimeError, "does not extend"):
                self.invoke(base, b"not-the-retained-prefix\n", line, head)


if __name__ == "__main__":
    unittest.main()
