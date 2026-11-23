import json
import unittest

from services.memory.person_memory import person_user_id


class PersonNamespaceTests(unittest.TestCase):
    def test_owner_and_lineage_both_affect_the_namespace(self):
        def user(lineage, owner):
            return person_user_id(json.dumps([lineage, owner], separators=(",", ":")))

        base = user("lineage-a", "owner-a")
        self.assertEqual(base, user("lineage-a", "owner-a"))
        self.assertNotEqual(base, user("lineage-b", "owner-a"))
        self.assertNotEqual(base, user("lineage-a", "owner-b"))
