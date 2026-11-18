"""Per-adapter upstream authentication tracking, safe across Mem0 threads."""
from threading import Event


class UpstreamUnauthorized(Exception):
    """An upstream credential was rejected; upstream details are not retained."""


def attach_auth_tracker(llm):
    tracker = Event()
    llm._zoe_auth_rejected = tracker
    return tracker


def reject_auth(llm):
    tracker = getattr(llm, "_zoe_auth_rejected", None)
    if tracker is not None:
        tracker.set()
    raise UpstreamUnauthorized() from None


def check_auth(tracker):
    if tracker.is_set():
        raise UpstreamUnauthorized() from None
