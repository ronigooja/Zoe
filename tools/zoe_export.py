#!/usr/bin/env python3
"""Export the owner's logical archive using a private instance-key file."""
from __future__ import annotations
import argparse
import base64
import json
import os
from pathlib import Path
import stat
import urllib.parse
import urllib.request

MAX_ARCHIVE_BYTES = 1 << 30

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("export redirects are refused")

def read_instance_key(path: Path, key_format: str) -> str:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid() or info.st_size > 16384:
            raise ValueError("key file must be a private regular file owned by the operator")
        with os.fdopen(fd, "r", closefd=False) as stream:
            value = stream.read(16385).strip()
        if key_format == "json":
            value = json.loads(value)["key"]
        raw = base64.urlsafe_b64decode(value + "=")
        if len(raw) != 32 or base64.urlsafe_b64encode(raw).decode().rstrip("=") != value:
            raise ValueError("invalid instance key file")
        return value
    finally:
        os.close(fd)

def export_archive(server: str, key_path: Path, key_format: str, output: Path, scope: str, opener=None) -> None:
    parsed = urllib.parse.urlsplit(server)
    if parsed.scheme not in ("http", "https") or not parsed.netloc or not parsed.hostname or parsed.username is not None or any(c in server for c in "?#\\") or parsed.netloc.endswith(":"):
        raise ValueError("server must be an HTTP(S) URL without credentials, query, or fragment")
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError("server port is invalid")
    escaped = parsed.path.lower()
    if "%2f" in escaped or "%5c" in escaped or any(segment in (".", "..") for segment in urllib.parse.unquote(parsed.path).split("/")):
        raise ValueError("server path has ambiguous separators or dot segments")
    if scope not in ("personal", "instance"):
        raise ValueError("scope must be personal or instance")
    key = read_instance_key(key_path, key_format)
    request = urllib.request.Request(server.rstrip("/") + "/v1/export?" + urllib.parse.urlencode({"scope": scope}), headers={"Authorization": "Bearer " + key, "Accept": "application/vnd.zoe.export+zip"})
    opener = opener or urllib.request.build_opener(NoRedirect())
    fd = os.open(output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    complete = False
    try:
        with os.fdopen(fd, "wb") as destination:
            with opener.open(request, timeout=120) as response:
                if response.status != 200 or response.headers.get_content_type() != "application/vnd.zoe.export+zip":
                    raise ValueError("server did not return an authoritative export archive")
                total = 0
                while True:
                    chunk = response.read(1 << 20)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > MAX_ARCHIVE_BYTES:
                        raise ValueError("export exceeds the bounded 1 GiB limit")
                    destination.write(chunk)
                if total == 0:
                    raise ValueError("empty export archive")
                destination.flush()
                os.fsync(destination.fileno())
        complete = True
    finally:
        if not complete:
            output.unlink(missing_ok=True)

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--key-format", choices=("json", "plain"), default="json", help="json is the zoe key init/rotate export format")
    parser.add_argument("--output", type=Path, required=True, help="new private archive file; never overwrite")
    parser.add_argument("--scope", choices=("personal", "instance"), default="instance")
    args = parser.parse_args()
    export_archive(args.server, args.key_file, args.key_format, args.output, args.scope)
    print("Logical authority archive saved; external runtime volumes and deployment secrets are outside this export.")
    return 0

if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        # HTTP errors and malformed credential files must never echo headers or key material.
        raise SystemExit("export failed; check the private key file, server availability, and output destination")
