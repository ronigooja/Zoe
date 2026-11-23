"""Regression coverage for operator CLI boundaries; no host operations are run."""
import base64
import email.message
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest import mock
from tools import zoe_destroy, zoe_export

class LifecycleCLI(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def manifest(self):
        deployment = self.root / "instance"
        deployment.mkdir()
        roots = []
        for category in sorted(zoe_destroy.REQUIRED_CATEGORIES):
            path = deployment / category
            path.mkdir()
            (path / "fixture").write_text("owned content")
            roots.append({"category": category, "path": str(path)})
        database = deployment / "data" / "zoe.sqlite"
        with sqlite3.connect(database) as connection:
            connection.execute("CREATE TABLE zoe_instance_state(singleton INTEGER,instance_id TEXT,dispatch_enabled INTEGER)")
            connection.execute("INSERT INTO zoe_instance_state VALUES(1,'instance-fixture',1)")
        database.chmod(0o600)
        adapter = self.root / "cleanup-adapter"
        adapter.write_text("#!/bin/sh\nexit 1\n")
        adapter.chmod(0o700)
        maintenance = self.root / "zoe-maintenance"
        maintenance.write_text("#!/bin/sh\nexit 1\n")
        maintenance.chmod(0o700)
        return {"schema_version": 1, "topology": "linux-systemd-compose", "instance_id": "instance-fixture", "deployment_root": str(deployment), "database": str(database), "roots": roots, "service_units": sorted(zoe_destroy.ALLOWED_UNITS), "external_resources": [{"kind": "opencode_runtime", "id": "owned-runtime"}, {"kind": "qdrant_index", "id": "owned-index"}], "cleanup_adapter": str(adapter), "maintenance_binary": str(maintenance)}

    def executor(self, manifest, *, incomplete=False, active=False):
        calls = []
        def run(command, *, input=None):
            calls.append(command)
            if command[:2] == ["systemctl", "show"]:
                return "active" if active else "inactive"
            if command[0] == manifest["maintenance_binary"]:
                with sqlite3.connect(manifest["database"]) as connection:
                    connection.execute("UPDATE zoe_instance_state SET dispatch_enabled=0")
            if command[0] == manifest["cleanup_adapter"]:
                request = json.loads(input)
                return json.dumps({"instance_id": manifest["instance_id"], "challenge": request["challenge"], "inventory_complete": not incomplete, "unaccounted_resources": [], "active_runtimes": [], "removed_resources": manifest["external_resources"]})
            return ""
        return run, calls

    def test_plan_is_read_only_and_complete_cleanup_deletes_only_owned_roots(self):
        manifest = self.manifest()
        receipt = self.root / "receipt.jsonl"
        runner, calls = self.executor(manifest)
        plan = zoe_destroy.destroy(manifest, receipt, False, "", runner)
        self.assertFalse(plan["executed"])
        self.assertEqual(calls, [])
        self.assertFalse(receipt.exists())
        result = zoe_destroy.destroy(manifest, receipt, True, manifest["instance_id"], runner)
        self.assertTrue(result["executed"])
        self.assertTrue(all(not Path(item["path"]).exists() for item in manifest["roots"]))
        self.assertTrue(Path(manifest["cleanup_adapter"]).exists())
        self.assertFalse(Path(manifest["deployment_root"]).exists())
        self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
        self.assertIn('"status": "destroyed"', receipt.read_text())

    def test_incomplete_proof_or_active_service_never_deletes_owned_files(self):
        for incomplete, active in ((True, False), (False, True)):
            with self.subTest(incomplete=incomplete, active=active):
                # Give each attempt its own independent instance.
                base = self.root
                self.root = base / ("incomplete" if incomplete else "active")
                self.root.mkdir()
                manifest = self.manifest()
                runner, _ = self.executor(manifest, incomplete=incomplete, active=active)
                with self.assertRaises(ValueError):
                    zoe_destroy.destroy(manifest, self.root / "receipt", True, manifest["instance_id"], runner)
                self.assertTrue(all(Path(item["path"]).exists() for item in manifest["roots"]))
                self.root = base

    def test_wrong_identity_unsafe_root_symlinks_and_missing_adapter_fail_closed(self):
        manifest = self.manifest()
        receipt = self.root / "receipt"
        with self.assertRaises(ValueError):
            zoe_destroy.destroy({**manifest, "instance_id": "another-instance"}, receipt, False, "")
        unsafe = [{**item, "path": "/"} if item["category"] == "config" else item for item in manifest["roots"]]
        with self.assertRaises(ValueError):
            zoe_destroy.destroy({**manifest, "roots": unsafe}, receipt, False, "")
        with mock.patch.object(zoe_destroy, "mounted_paths", return_value={Path(manifest["roots"][0]["path"])}):
            with self.assertRaises(ValueError):
                zoe_destroy.destroy(manifest, receipt, False, "")
        link = Path(manifest["roots"][0]["path"]) / "outside"
        link.symlink_to(self.root)
        with self.assertRaises(ValueError):
            zoe_destroy.destroy(manifest, receipt, False, "")
        link.unlink()
        Path(manifest["cleanup_adapter"]).unlink()
        with self.assertRaises(FileNotFoundError):
            zoe_destroy.destroy(manifest, receipt, True, manifest["instance_id"])
        self.assertFalse(receipt.exists())

    def test_export_private_json_and_plain_keys_preserve_credentials_off_url(self):
        key = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")
        class Response(io.BytesIO):
            status = 200
            headers = email.message.Message()
        Response.headers["Content-Type"] = "application/vnd.zoe.export+zip"
        class Opener:
            def open(self, request, timeout):
                self.request = request
                return Response(b"PK-fixture")
        for key_format in ("json", "plain"):
            source = self.root / (key_format + ".key")
            source.write_text(json.dumps({"key": key}) if key_format == "json" else key)
            source.chmod(0o600)
            output = self.root / (key_format + ".zip")
            opener = Opener()
            zoe_export.export_archive("https://instance.example/base", source, key_format, output, "instance", opener)
            self.assertEqual(opener.request.full_url, "https://instance.example/base/v1/export?scope=instance")
            self.assertEqual(output.read_bytes(), b"PK-fixture")
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            self.assertNotIn(key, opener.request.full_url)
            self.assertEqual(opener.request.get_header("Authorization"), "Bearer " + key)
            with self.assertRaises(FileExistsError):
                zoe_export.export_archive("https://instance.example/base", source, key_format, output, "instance", opener)
        source.chmod(0o644)
        with self.assertRaises(ValueError):
            zoe_export.read_instance_key(source, "plain")

    def test_export_rejects_retired_scope_before_transport(self):
        opener = mock.Mock()
        output = self.root / "unsupported.zip"
        with self.assertRaisesRegex(ValueError, "personal or instance"):
            zoe_export.export_archive(
                "https://instance.example", self.root / "unused-key", "plain",
                output, "workspace", opener,
            )
        opener.open.assert_not_called()
        self.assertFalse(output.exists())

    def test_failed_export_removes_partial_file_and_refuses_redirects(self):
        source = self.root / "key"
        source.write_text(base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("="))
        source.chmod(0o600)
        class Broken:
            def open(self, request, timeout):
                raise OSError("fixture transport interruption")
        destination = self.root / "partial"
        with self.assertRaises(OSError):
            zoe_export.export_archive("https://instance.example", source, "plain", destination, "personal", Broken())
        self.assertFalse(destination.exists())
        with self.assertRaises(ValueError):
            zoe_export.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other.example")

if __name__ == "__main__":
    unittest.main()
