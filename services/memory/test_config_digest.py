import hashlib
import unittest

from .memory_api import config_fingerprint


class ConfigDigestTest(unittest.TestCase):
    def test_shared_vector(self):
        key = "fixture-key"
        digest = config_fingerprint("https://example.test/v1", "model<&", "high", "", key)
        self.assertEqual(digest, "43f3dcbcd23a6f32af6ad6f797e65694eb3a1886b9d574e9e8399ad6c5e551b1")
        self.assertNotEqual(digest, hashlib.sha256(key.encode()).hexdigest())
        self.assertNotIn(key, digest)
