#!/usr/bin/env python3
"""Validate and stage a Zoe backup for a new, dispatch-disabled instance."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import sqlite3
import stat
import sys
import tempfile

from data_lifecycle.common import fail, independent_deletion_journal_metadata, private_dir, read_key, unpack_verified


MANAGED_ROLES = {"service_config_paths", "artifact_paths", "persistent_volume_paths"}


def verify_latest_deletion_head(latest_path: Path, head_path: Path, snapshot_path: Path, snapshot_head: dict) -> dict:
    latest_stat = latest_path.lstat()
    if not stat.S_ISREG(latest_stat.st_mode) or latest_stat.st_uid != os.getuid() or stat.S_IMODE(latest_stat.st_mode) != 0o600:
        fail("latest deletion journal must be a current-user-owned regular file with mode 0600")
    head_stat = head_path.lstat()
    if not stat.S_ISREG(head_stat.st_mode) or head_stat.st_uid != os.getuid() or stat.S_IMODE(head_stat.st_mode) != 0o600:
        fail("latest deletion head must be a current-user-owned regular file with mode 0600")
    try:
        external_head = json.loads(head_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        fail(f"latest deletion head is invalid: {exc}")
    if not isinstance(external_head, dict) or set(external_head) != {"lineage_id", "generation", "record_generation", "sequence", "head_hash", "file_sha256"}:
        fail("latest deletion head has an unsupported schema")
    latest_bytes = latest_path.read_bytes()
    snapshot_bytes = snapshot_path.read_bytes()
    if not latest_bytes.startswith(snapshot_bytes):
        fail("independent latest journal does not extend the selected backup journal")
    if snapshot_head is None or external_head.get("lineage_id") != snapshot_head.get("lineage_id"):
        fail("independent latest deletion head belongs to a different lineage")
    try:
        latest_sequence = len(latest_bytes.splitlines())
        latest_generation = external_head["generation"]
        record_generation = external_head["record_generation"]
        if isinstance(latest_generation, bool) or not isinstance(latest_generation, int) or latest_generation < 1:
            fail("independent latest deletion generation is invalid")
        if isinstance(record_generation, bool) or not isinstance(record_generation, int) or not 0 <= record_generation <= latest_generation:
            fail("independent latest deletion record generation is invalid")
        metadata = independent_deletion_journal_metadata(
            latest_path,
            {"lineage_id": snapshot_head["snapshot_lineage_id"], "generation": latest_generation,
             "deletion_sequence": latest_sequence, "event_count": 0, "event_cursor_sum": 0},
        )
    except Exception as exc:
        fail(f"independent latest deletion journal is invalid: {exc}")
    expected = {
        "lineage_id": metadata["lineage_id"], "generation": metadata["generation"],
        "record_generation": metadata["record_generation"],
        "sequence": metadata["sequence"], "head_hash": metadata["head_hash"],
        "file_sha256": metadata["file_sha256"],
    }
    if external_head != expected or metadata["sequence"] < snapshot_head["snapshot_sequence"]:
        fail("independent latest deletion head does not match the retained journal or is older than the snapshot")
    return metadata


def restore_managed_paths(payload: Path, staged: Path, manifest: dict) -> int:
    roots = manifest.get("roots")
    entries = manifest.get("managed_paths")
    if not isinstance(roots, dict) or not isinstance(entries, list):
        fail("manifest managed path inventory is malformed")
    root_ids = set(roots)
    if not all(isinstance(root_id, str) and root_id.isidentifier() for root_id in root_ids):
        fail("manifest contains an unsafe managed root ID")
    seen = set()
    payload_root = payload.resolve(strict=True)
    staged_root = staged.resolve(strict=True)
    for item in entries:
        if not isinstance(item, dict) or set(item) != {"role", "root_id", "path"}:
            fail("manifest contains a malformed managed path")
        role, root_id = item["role"], item["root_id"]
        if role not in MANAGED_ROLES or not isinstance(root_id, str) or not root_id.isidentifier() or root_id not in root_ids:
            fail("manifest contains an unsafe managed role or root ID")
        rel = PurePosixPath(item["path"] if isinstance(item["path"], str) else "")
        if rel.is_absolute() or not rel.parts or any(p in ("", ".", "..") for p in rel.parts):
            fail("manifest contains an unsafe managed path")
        identity = (role, root_id, rel.as_posix())
        if identity in seen:
            fail("manifest contains a duplicate managed path")
        seen.add(identity)
        source = payload_root / role / root_id / Path(*rel.parts)
        try:
            source = source.resolve(strict=True)
            source.relative_to(payload_root)
        except (OSError, ValueError):
            fail("manifest managed source escapes the verified payload")
        dest = staged_root / "managed" / role / root_id / Path(*rel.parts)
        try:
            dest.resolve(strict=False).relative_to(staged_root)
        except ValueError:
            fail("manifest managed destination escapes the staging directory")
        if source.is_file():
            dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            try:
                dest.parent.resolve(strict=True).relative_to(staged_root)
            except ValueError:
                fail("managed destination parent escapes the staging directory")
            dest.write_bytes(source.read_bytes())
            os.chmod(dest, 0o600)
        elif source.is_dir():
            dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            try:
                dest.parent.resolve(strict=True).relative_to(staged_root)
            except ValueError:
                fail("managed destination parent escapes the staging directory")
            shutil.copytree(source, dest, dirs_exist_ok=True)
        else:
            fail("manifest managed path is missing from payload")
    return len(entries)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact", type=Path, required=True)
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--latest-deletion-ledger", type=Path, required=True, help="independently retained current journal, not copied from the selected backup")
    parser.add_argument("--latest-deletion-head-file", type=Path, required=True, help="0600 maintenance-owned JSON attesting the retained journal head and file hash")
    parser.add_argument("--restore-directory", type=Path, required=True, help="new path; must not already exist")
    args = parser.parse_args()
    key = read_key(args.key_file)
    target = args.restore_directory.absolute()
    if target.exists() or target.is_symlink():
        fail("restore target must be a new path; existing targets are never overwritten")
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="zoe-restore-", dir=target.parent) as tmp:
        payload, manifest = unpack_verified(args.artifact, key, Path(tmp))
        # Check the deletion ledger is present and authentic before making the staged copy
        # available to a server or any derived retrieval service.
        database_manifest = manifest.get("database", {})
        snapshot_head = database_manifest.get("trusted_deletion_head")
        source_db = payload / "authority" / "zoe.sqlite"
        source_journal = payload / "authority" / "zoe.sqlite.deletion-ledger"
        latest_path = args.latest_deletion_ledger
        head_path = args.latest_deletion_head_file
        metadata = verify_latest_deletion_head(latest_path, head_path, source_journal, snapshot_head)
        trusted_head = {
            "snapshot_lineage_id": snapshot_head["snapshot_lineage_id"],
            "snapshot_generation": snapshot_head["snapshot_generation"],
            "snapshot_sequence": snapshot_head["snapshot_sequence"],
            "snapshot_event_count": snapshot_head["snapshot_event_count"],
            "snapshot_event_cursor_sum": snapshot_head["snapshot_event_cursor_sum"],
            "journal_lineage_id": metadata["lineage_id"],
            "journal_generation": metadata["generation"],
            "journal_record_generation": metadata["record_generation"],
            "journal_sequence": metadata["sequence"],
            "journal_head_sha256": metadata["head_hash"],
        }
        staged = Path(tmp) / "new-instance"
        staged.mkdir(mode=0o700)
        (staged / "data").mkdir(mode=0o700)
        shutil.copyfile(source_db, staged / "data" / "zoe.sqlite.restore-pending")
        os.chmod(staged / "data" / "zoe.sqlite.restore-pending", 0o600)
        shutil.copyfile(latest_path, staged / "data" / "zoe.sqlite.restore-pending.deletion-ledger")
        os.chmod(staged / "data" / "zoe.sqlite.restore-pending.deletion-ledger", 0o600)
        # Keep the restore-only closed state inside the database so renaming
        # the staged file cannot bypass the adjacent restore-state.json.
        with sqlite3.connect(staged / "data" / "zoe.sqlite.restore-pending") as database:
            database.execute(
                "CREATE TABLE IF NOT EXISTS zoe_restore_stage_guard ("
                "singleton INTEGER PRIMARY KEY CHECK(singleton=1), "
                "pending INTEGER NOT NULL CHECK(pending IN (0,1)))"
            )
            database.execute(
                "INSERT INTO zoe_restore_stage_guard(singleton,pending) VALUES(1,1) "
                "ON CONFLICT(singleton) DO UPDATE SET pending=1"
            )
        managed_count = restore_managed_paths(payload, staged, manifest)
        restored = dict(manifest)
        restored["restore_state"] = {
            "instance_id": "",
            "dispatch_enabled": False,
            "requires_old_instance_isolation": True,
            "requires_deletion_reconciliation_before_derived_reads": True,
            "trusted_deletion_head": {
                **trusted_head,
                "journal_file_sha256": metadata["file_sha256"],
            },
        }
        (staged / "restore-state.json").write_text(json.dumps(restored, sort_keys=True, indent=2) + "\n", encoding="utf-8")
        os.chmod(staged / "restore-state.json", 0o600)
        os.replace(staged, target)
        parent_fd = os.open(target.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    print(f"restore staged: {target}")
    print("dispatch enabled: false (install the database with `zoe-maintenance prepare-restore` before starting a server)")
    print(f"managed paths: {managed_count}; deletion ledger tables: {len(deletion or {})}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"zoe-restore: {exc}", file=sys.stderr)
        raise SystemExit(1)
