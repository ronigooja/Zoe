import threading
import unittest
from types import SimpleNamespace

from .auth_tracking import UpstreamUnauthorized, attach_auth_tracker, check_auth, reject_auth


class AuthTrackingTests(unittest.TestCase):
    def test_rejection_survives_swallowing_and_thread_boundary(self):
        adapter = SimpleNamespace()
        tracker = attach_auth_tracker(adapter)

        def swallowed():
            try:
                reject_auth(adapter)
            except Exception:
                pass

        thread = threading.Thread(target=swallowed)
        thread.start()
        thread.join()
        with self.assertRaises(UpstreamUnauthorized):
            check_auth(tracker)
        tracker.clear()
        check_auth(tracker)
