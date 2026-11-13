from __future__ import annotations

import logging
import os
import signal
import threading

from .config import WorkerConfig
from .mem0_backend import ControlledMem0
from .memory_api import MemoryAPIServer, create_memory_api

LOG = logging.getLogger("zoe.memory.worker")


class Mem0ContentLogFilter(logging.Filter):
    """Mem0 2.2 logs memory contents at INFO and can echo them on failures."""

    def filter(self, record: logging.LogRecord) -> bool:
        return not record.name.startswith("mem0") or logging.getLogger().isEnabledFor(logging.DEBUG)


def configure_logging() -> None:
    level = logging.DEBUG if os.environ.get("ZOE_LOG_LEVEL", "info").lower() == "debug" else logging.INFO
    logging.basicConfig(level=level, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    logging.getLogger().setLevel(level)
    # Transport debug logs may contain credentials. Content debugging is scoped
    # to Zoe/Mem0 rather than HTTP libraries.
    for name in ("httpx", "httpcore", "openai", "urllib3"):
        logging.getLogger(name).setLevel(logging.WARNING)
    for handler in logging.getLogger().handlers:
        if not any(isinstance(item, Mem0ContentLogFilter) for item in handler.filters):
            handler.addFilter(Mem0ContentLogFilter())


def serve_memory_api(server: MemoryAPIServer) -> None:
    def shutdown(signum, frame):
        # BaseServer.shutdown must run outside the serving thread. Starting
        # it asynchronously also handles SIGTERM arriving just before serve.
        threading.Thread(target=server.shutdown, name="memory-api-shutdown", daemon=True).start()

    previous = signal.signal(signal.SIGTERM, shutdown)
    try:
        server.serve_forever()
    finally:
        signal.signal(signal.SIGTERM, previous)
        server.server_close()


def main() -> None:
    configure_logging()
    if not os.environ.get("ZOE_MEMORY_API_TOKEN_FILE", ""):
        LOG.error("ZOE_MEMORY_API_TOKEN_FILE is required; memory API cannot start")
        raise SystemExit(1)
    backend = ControlledMem0(WorkerConfig.from_env())
    try:
        serve_memory_api(create_memory_api(backend))
    finally:
        backend.memory.db.close()


if __name__ == "__main__":
    main()
