"""Shared safety and archive helpers for backup, restore, and migration."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import secrets
import shutil
import sqlite3
import stat
import subprocess
import struct
import tarfile
import tempfile
from typing import Any, Iterator

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.kdf.scrypt import Scrypt


FORMAT = "zoe-encrypted-backup-v1"
MAGIC = b"ZOEBK1\0"
SCRYPT_N = 2**18
SCRYPT_R = 8
SCRYPT_P = 1
CHUNK = 1024 * 1024



def fail(message: str) -> None:
    raise RuntimeError(message)


def private_dir(path: Path) -> None:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    st = path.lstat()
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid():
        fail(f"directory must be owned by the current user: {path}")
    path.chmod(0o700)


def read_key(path: Path) -> bytes:
    st = path.lstat()
    if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o600:
        fail("encryption key must be a regular, current-user-owned file with mode 0600")
    data = path.read_bytes()
    if len(data) != 32:
        fail("encryption key file must contain exactly 32 random bytes")
    return data


@contextlib.contextmanager
def database_fence(path: Path) -> Iterator[Path]:
    """Acquire Go storage.Store's exclusive flock on the canonical DB inode."""
    if not path.exists() or path.is_symlink():
        fail("database must exist as a regular non-symlink file")
    resolved = path.resolve(strict=True)
    fd = os.open(resolved, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            fail("database path must be a regular file")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            fail("database writer lock is held; stop the server before backup or migration")
        yield resolved
    finally:
        os.close(fd)


def sqlite_snapshot(source: Path, destination: Path) -> dict[str, Any]:
    """Use SQLite's backup API while fenced from Zoe's single writer."""
    uri = f"file:{source.as_posix()}?mode=ro"
    src = sqlite3.connect(uri, uri=True, timeout=5)
    dst = sqlite3.connect(destination)
    try:
        src.execute("PRAGMA query_only=ON")
        src.backup(dst, pages=256, sleep=0.01)
        check = dst.execute("PRAGMA integrity_check").fetchone()
        if not check or check[0] != "ok":
            fail("SQLite integrity_check failed on snapshot")
        tables = {r[0] for r in dst.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        migrations = []
        if "schema_migrations" in tables:
            columns = {r[1] for r in dst.execute("PRAGMA table_info(schema_migrations)")}
            namespace = "namespace" if "namespace" in columns else "''"
            migrations = [
                {"version": int(v), "name": str(n), "namespace": str(ns)}
                for v, n, ns in dst.execute(f"SELECT version,name,{namespace} FROM schema_migrations ORDER BY version")
            ]
        deletion = {}
        for table in sorted(tables):
            if "tombstone" in table.lower() or "deletion_ledger" in table.lower():
                safe = table.replace('"', '""')
                deletion[table] = int(dst.execute(f'SELECT count(*) FROM "{safe}"').fetchone()[0])
        instance_state = None
        if "zoe_instance_state" in tables:
            columns = {r[1] for r in dst.execute("PRAGMA table_info(zoe_instance_state)")}
            required = {"instance_id", "epoch", "dispatch_enabled", "derived_retrieval_enabled", "fence_reason", "fenced_at", "lineage_id", "generation", "deletion_sequence"}
            if not required <= columns:
                fail("instance deletion baseline schema is unavailable")
            row = dst.execute("SELECT instance_id,epoch,dispatch_enabled,derived_retrieval_enabled,fence_reason,fenced_at,lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1").fetchone()
            if row is not None:
                event_count, event_cursor_sum = 0, 0
                if "events" in tables:
                    event_count, event_cursor_sum = map(int, dst.execute("SELECT count(*),coalesce(sum(cursor),0) FROM events").fetchone())
                instance_state = {"instance_id": row[0], "epoch": int(row[1]), "dispatch_enabled": bool(row[2]), "derived_retrieval_enabled": bool(row[3]), "fence_reason": row[4], "fenced_at": row[5], "lineage_id": row[6], "generation": int(row[7]), "deletion_sequence": int(row[8]), "event_count": event_count, "event_cursor_sum": event_cursor_sum}
            else:
                fail("instance deletion baseline singleton is missing")
        return {"integrity": "ok", "migrations": migrations, "deletion_record_counts": deletion, "instance_state": instance_state}
    finally:
        dst.close()
        src.close()


def independent_deletion_journal_metadata(path: Path, instance_state: dict[str, Any] | None) -> dict[str, Any]:
    """Validate the sidecar's continuous identity and bind it to SQLite's head."""
    if instance_state is None:
        fail("backup has no verifiable instance deletion baseline")
    if path.is_symlink():
        fail("independent deletion journal cannot be a symlink")
    if path.exists():
        st = path.stat()
        if not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600 or st.st_uid != os.getuid():
            fail("independent deletion journal must be a private current-user-owned regular file")
        raw = path.read_bytes()
    else:
        raw = b""
    records = []
    previous_hash = ""
    lineage = ""
    generation = 0
    for number, line in enumerate(raw.splitlines(), 1):
        try:
            record = json.loads(line)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            fail(f"independent deletion journal record {number} is invalid: {exc}")
        if not isinstance(record, dict) or record.get("sequence") != number or record.get("previous_hash", "") != previous_hash or not isinstance(record.get("hash"), str) or not record["hash"]:
            fail(f"independent deletion journal sequence or hash chain is invalid at record {number}")
        if not isinstance(record.get("lineage_id"), str) or not record["lineage_id"] or not isinstance(record.get("generation"), int) or record["generation"] < 1:
            fail(f"independent deletion journal identity is invalid at record {number}")
        if lineage and (record["lineage_id"] != lineage or record["generation"] < generation):
            fail(f"independent deletion journal generation is discontinuous at record {number}")
        body_keys = ("sequence", "lineage_id", "generation", "workspace_id", "entity_kind", "entity_id", "entity_version", "actor_id", "reason_code", "deleted_at", "cleanup_targets", "previous_hash")
        if set(record) != set(body_keys) | {"hash"} or not isinstance(record.get("cleanup_targets"), list):
            fail(f"independent deletion journal record {number} has an unsupported schema")
        body = {key: record[key] for key in body_keys}
        encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
        encoded = encoded.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode("utf-8")
        if hashlib.sha256(encoded).hexdigest() != record["hash"]:
            fail(f"independent deletion journal hash mismatch at record {number}")
        lineage = record["lineage_id"]
        generation = record["generation"]
        previous_hash = record["hash"]
        records.append(record)
    expected_sequence = int(instance_state["deletion_sequence"])
    authority_generation = int(instance_state["generation"])
    if expected_sequence != len(records):
        fail("SQLite authority sequence does not match the independent deletion journal head")
    if records and (lineage != instance_state["lineage_id"] or generation > authority_generation):
        fail("SQLite authority identity does not match the independent deletion journal")
    return {
        "path": "authority/zoe.sqlite.deletion-ledger",
        "sequence": len(records),
        "lineage_id": lineage or instance_state["lineage_id"],
        "generation": authority_generation,
        "record_generation": generation,
        "head_hash": previous_hash,
        "file_sha256": hashlib.sha256(raw).hexdigest(),
        "snapshot_lineage_id": instance_state["lineage_id"],
        "snapshot_generation": int(instance_state["generation"]),
        "snapshot_sequence": expected_sequence,
        "snapshot_event_count": int(instance_state["event_count"]),
        "snapshot_event_cursor_sum": int(instance_state["event_cursor_sum"]),
    }


def load_inventory(path: Path | None) -> dict[str, Any]:
    if path is None:
        return {"roots": {}, "service_config_paths": [], "artifact_paths": [], "persistent_volume_paths": [], "secret_refs": []}
    raw = json.loads(path.read_text(encoding="utf-8"))
    allowed = {"schema_version", "roots", "service_config_paths", "artifact_paths", "persistent_volume_paths", "secret_refs"}
    if set(raw) - allowed or raw.get("schema_version", 1) != 1:
        fail("inventory has unsupported fields or schema_version")
    roots = raw.get("roots", {})
    if not isinstance(roots, dict) or not all(isinstance(k, str) and k.isidentifier() and isinstance(v, str) and v for k, v in roots.items()):
        fail("inventory roots must map identifier names to absolute directory paths")
    result: dict[str, Any] = {"roots": roots}
    for key in ("service_config_paths", "artifact_paths", "persistent_volume_paths"):
        values = raw.get(key, [])
        if not isinstance(values, list) or not all(isinstance(x, dict) and set(x) == {"root_id", "path"} and isinstance(x["root_id"], str) and isinstance(x["path"], str) for x in values):
            fail(f"inventory {key} must be an array of root_id/path objects")
        result[key] = values
    refs = raw.get("secret_refs", [])
    if not isinstance(refs, list) or not all(isinstance(x, dict) and set(x) <= {"name", "provider", "version_ref"} and "name" in x and "provider" in x for x in refs):
        fail("secret_refs may contain only name, provider, and optional version_ref")
    result["secret_refs"] = refs
    return result


def reject_symlink_components(path: Path) -> None:
    current = Path(path.anchor)
    for part in path.parts[1:] if path.is_absolute() else path.parts:
        current = current / part
        if current.is_symlink():
            fail(f"symlink path component is not accepted: {current}")


def copy_payload_path(root: Path, source: Path, role: str, root_id: str, payload: Path) -> list[dict[str, Any]]:
    reject_symlink_components(source.expanduser().absolute())
    source = source.expanduser().resolve(strict=True)
    root = root.resolve(strict=True)
    try:
        relative = source.relative_to(root)
    except ValueError:
        fail(f"included path must be within its declared root: {source}")
    if not relative.parts or any(p in (".", "..") for p in relative.parts):
        fail("unsafe included path")
    target = payload / role / root_id / relative
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if source.is_symlink():
        fail(f"symlink data is not accepted in backup inputs: {source}")
    records = []
    if source.is_file():
        shutil.copyfile(source, target, follow_symlinks=False)
        target.chmod(stat.S_IMODE(source.stat().st_mode) & 0o700)
        records.append(file_record(target, role))
    elif source.is_dir():
        target.mkdir(mode=0o700, exist_ok=True)
        for base, dirs, files in os.walk(source, followlinks=False):
            dirs.sort()
            files.sort()
            base_path = Path(base)
            for name in list(dirs):
                item = base_path / name
                if item.is_symlink():
                    fail(f"symlink data is not accepted in backup inputs: {item}")
                (target / item.relative_to(source)).mkdir(mode=0o700, parents=True, exist_ok=True)
            for name in files:
                item = base_path / name
                if item.is_symlink() or not item.is_file():
                    fail(f"only regular files are accepted in backup inputs: {item}")
                dest = target / item.relative_to(source)
                dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                shutil.copyfile(item, dest, follow_symlinks=False)
                dest.chmod(stat.S_IMODE(item.stat().st_mode) & 0o700)
                records.append(file_record(dest, role))
    else:
        fail(f"only regular files and directories can be backed up: {source}")
    return records


def file_record(path: Path, role: str, archive_path: str | None = None) -> dict[str, Any]:
    h = hashlib.sha256()
    with path.open("rb") as src:
        for block in iter(lambda: src.read(CHUNK), b""):
            h.update(block)
    return {"path": archive_path or path.as_posix(), "role": role, "size": path.stat().st_size, "sha256": h.hexdigest()}


def create_archive(payload: Path, archive_path: Path) -> list[dict[str, Any]]:
    records = []
    with tarfile.open(archive_path, "w", format=tarfile.PAX_FORMAT) as tar:
        for path in sorted(payload.rglob("*")):
            if path.is_symlink():
                fail("staged payload contains symlink")
            rel = PurePosixPath(path.relative_to(payload).as_posix())
            if path.is_dir():
                continue
            rec = file_record(path, rel.parts[0], rel.as_posix())
            records.append(rec)
            info = tar.gettarinfo(str(path), arcname=rel.as_posix())
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = 0
            with path.open("rb") as src:
                tar.addfile(info, src)
    return records


def encrypt_file(source: Path, destination: Path, key: bytes) -> None:
    salt, nonce = secrets.token_bytes(16), secrets.token_bytes(12)
    derived = Scrypt(salt=salt, length=32, n=SCRYPT_N, r=SCRYPT_R, p=SCRYPT_P).derive(key)
    encryptor = Cipher(algorithms.AES(derived), modes.GCM(nonce)).encryptor()
    with destination.open("xb") as out, source.open("rb") as src:
        os.chmod(destination, 0o600)
        out.write(MAGIC + salt + nonce)
        for block in iter(lambda: src.read(CHUNK), b""):
            out.write(encryptor.update(block))
        out.write(encryptor.finalize())
        out.write(encryptor.tag)
        out.flush()
        os.fsync(out.fileno())


def decrypt_file(source: Path, destination: Path, key: bytes) -> None:
    size = source.stat().st_size
    if size < len(MAGIC) + 16 + 12 + 16:
        fail("encrypted artifact is truncated")
    with source.open("rb") as src:
        header = src.read(len(MAGIC) + 28)
        if header[:len(MAGIC)] != MAGIC:
            fail("unknown encrypted artifact format")
        salt = header[len(MAGIC):len(MAGIC) + 16]
        nonce = header[len(MAGIC) + 16:]
        src.seek(-16, os.SEEK_END)
        tag = src.read(16)
        ciphertext_size = size - len(header) - 16
        src.seek(len(header))
        derived = Scrypt(salt=salt, length=32, n=SCRYPT_N, r=SCRYPT_R, p=SCRYPT_P).derive(key)
        decryptor = Cipher(algorithms.AES(derived), modes.GCM(nonce, tag)).decryptor()
        with destination.open("xb") as out:
            os.chmod(destination, 0o600)
            remaining = ciphertext_size
            while remaining:
                block = src.read(min(CHUNK, remaining))
                if not block:
                    fail("encrypted artifact is truncated")
                remaining -= len(block)
                out.write(decryptor.update(block))
            out.write(decryptor.finalize())
            out.flush()
            os.fsync(out.fileno())


def safe_extract(archive: Path, destination: Path) -> None:
    with tarfile.open(archive, "r:") as tar:
        for member in tar.getmembers():
            p = PurePosixPath(member.name)
            if p.is_absolute() or not p.parts or any(part in ("", ".", "..") for part in p.parts):
                fail("archive contains an unsafe path")
            if not member.isfile():
                fail("archive contains a non-regular file")
            target = destination.joinpath(*p.parts)
            if not target.resolve().is_relative_to(destination.resolve()):
                fail("archive path escapes restore staging directory")
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            src = tar.extractfile(member)
            if src is None:
                fail("archive file payload is missing")
            with src, target.open("xb") as out:
                shutil.copyfileobj(src, out, CHUNK)
            target.chmod(0o600)


def verify_manifest(payload: Path) -> dict[str, Any]:
    manifest_path = payload / "manifest.json"
    raw = json.loads(manifest_path.read_text(encoding="utf-8"))
    if raw.get("format") != FORMAT or raw.get("schema_version") != 1:
        fail("backup manifest format or schema version is unsupported")
    expected = raw.get("files")
    if not isinstance(expected, list) or not expected:
        fail("backup manifest has no file inventory")
    seen = set()
    for record in expected:
        rel = PurePosixPath(record.get("path", ""))
        if rel.is_absolute() or any(p in ("", ".", "..") for p in rel.parts) or rel.as_posix() in seen:
            fail("backup manifest contains an unsafe or duplicate path")
        seen.add(rel.as_posix())
        file = payload.joinpath(*rel.parts)
        if not file.is_file() or file.is_symlink():
            fail("backup payload is missing a manifest file")
        actual = file_record(file, record.get("role", ""))
        if actual["size"] != record.get("size") or actual["sha256"] != record.get("sha256"):
            fail(f"backup hash or size mismatch: {rel.as_posix()}")
    actual_paths = {p.relative_to(payload).as_posix() for p in payload.rglob("*") if p.is_file()}
    if actual_paths != seen | {"manifest.json"}:
        fail("backup payload contains unmanifested or missing files")
    db = payload / "authority" / "zoe.sqlite"
    if not db.is_file():
        fail("backup does not contain authoritative SQLite database")
    sidecar = payload / "authority" / "zoe.sqlite.deletion-ledger"
    if not sidecar.is_file():
        fail("backup does not contain the independent deletion journal")
    conn = sqlite3.connect(f"file:{db.as_posix()}?mode=ro", uri=True)
    try:
        database_manifest = raw.get("database")
        if not isinstance(database_manifest, dict):
            fail("backup database metadata is malformed")
        if conn.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            fail("restored SQLite database failed integrity_check")
        rows = []
        if conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='schema_migrations'").fetchone():
            columns = {r[1] for r in conn.execute("PRAGMA table_info(schema_migrations)")}
            namespace = "namespace" if "namespace" in columns else "''"
            rows = [{"version": int(v), "name": str(n), "namespace": str(ns)} for v, n, ns in conn.execute(f"SELECT version,name,{namespace} FROM schema_migrations ORDER BY version")]
        if rows != database_manifest.get("migrations"):
            fail("schema migration inventory does not match SQLite database")
        if any(migration.get("namespace") == "memory" for migration in rows):
            fail("backup uses an unsupported schema namespace")
        tables = {r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        if "zoe_instance_state" not in tables:
            fail("backup has no verifiable instance deletion baseline")
        state = conn.execute("SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1").fetchone()
        if state is None:
            fail("backup instance deletion baseline singleton is missing")
        event_count, event_sum = map(int, conn.execute("SELECT count(*),coalesce(sum(cursor),0) FROM events").fetchone()) if "events" in tables else (0, 0)
        instance_state = {"lineage_id": state[0], "generation": int(state[1]), "deletion_sequence": int(state[2]), "event_count": event_count, "event_cursor_sum": event_sum}
        trusted_head = database_manifest.get("trusted_deletion_head")
        actual_head = independent_deletion_journal_metadata(sidecar, instance_state)
        if trusted_head != actual_head:
            fail("backup trusted deletion head does not match the SQLite snapshot and sidecar")
    finally:
        conn.close()
    return raw


def unpack_verified(artifact: Path, key: bytes, parent: Path) -> tuple[Path, dict[str, Any]]:
    plain = parent / "backup.tar"
    decrypt_file(artifact, plain, key)
    payload = parent / "payload"
    payload.mkdir(mode=0o700)
    safe_extract(plain, payload)
    return payload, verify_manifest(payload)


def sha256_file(path: Path) -> str:
    return file_record(path, "")["sha256"]


def run_volume_quiesce_adapter(command: Path, inventory_path: Path, entries: list[dict[str, Any]]) -> dict[str, Any]:
    if not command.is_absolute() or not command.is_file() or not os.access(command, os.X_OK):
        fail("volume quiesce adapter must be an absolute executable file")
    expected = sorted((item["root_id"], item["path"]) for item in entries)
    completed = subprocess.run([str(command), "--check-and-quiesce", "--inventory", str(inventory_path)], check=True, capture_output=True, text=True)
    try:
        evidence = json.loads(completed.stdout)
    except json.JSONDecodeError as exc:
        fail(f"volume quiesce adapter returned invalid evidence: {exc}")
    observed = evidence.get("quiesced_paths")
    if evidence.get("state") != "quiesced" or not isinstance(evidence.get("adapter_id"), str) or not evidence["adapter_id"].strip() or not isinstance(observed, list):
        fail("volume quiesce adapter did not return state=quiesced, adapter_id, and quiesced_paths")
    actual = []
    for item in observed:
        if not isinstance(item, dict) or set(item) != {"root_id", "path"}:
            fail("volume quiesce adapter returned malformed path evidence")
        actual.append((item["root_id"], item["path"]))
    if sorted(actual) != expected:
        fail("volume quiesce adapter did not acknowledge every required persistent volume path")
    return {"adapter_id": evidence["adapter_id"], "verified_at": evidence.get("verified_at", ""), "quiesced_paths": [{"root_id": k, "path": v} for k, v in sorted(actual)]}
