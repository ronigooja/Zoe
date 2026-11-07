"""Shared lab failures; callers must suppress potentially sensitive details."""


class LabError(RuntimeError):
    """HTTP failures carry status and a bounded, redacted response body."""

    def __init__(self, message, status=None, body=''):
        self.status = status
        self.body = body
        super().__init__(message + (': ' + body if body else ''))
