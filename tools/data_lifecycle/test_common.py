import unittest

from data_lifecycle.common import (
    load_inventory,
)


class LoadInventoryTests(unittest.TestCase):
    def test_missing_inventory_is_an_empty_managed_inventory(self):
        self.assertEqual(
            load_inventory(None),
            {
                "roots": {},
                "service_config_paths": [],
                "artifact_paths": [],
                "persistent_volume_paths": [],
                "secret_refs": [],
            },
        )
