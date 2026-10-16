"""Background exporter: a bounded buffer drained by one daemon thread.

offer() never blocks on the network and never raises; when the buffer is full
the span is dropped and counted. Batches go to the agent as OTLP JSON.
"""

from __future__ import annotations

import os
import threading
import time
import urllib.request
from typing import Callable

Transport = Callable[[bytes], None]


def http_transport(url: str, timeout: float = 3.0) -> Transport:
    """POST to the agent. Proxies are never used: the agent is local, and
    trace IDs must not travel through a corporate proxy."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def send(body: bytes) -> None:
        req = urllib.request.Request(url, data=body, method="POST",
                                     headers={"Content-Type": "application/json", "User-Agent": "plumbline-python"})
        with opener.open(req, timeout=timeout) as resp:
            resp.read(1 << 16)
            if resp.status // 100 != 2:
                raise OSError(f"agent answered HTTP {resp.status}")

    return send


class Exporter:
    def __init__(self, transport: Transport, encode: Callable[[list], bytes], *,
                 max_queue: int = 2048, batch_size: int = 512, interval: float = 1.0) -> None:
        self._transport = transport
        self._encode = encode
        self._max_queue = max_queue
        self._batch_size = batch_size
        self._interval = interval
        self.exported = 0
        self.dropped = 0
        self.export_errors = 0
        self._reset()
        if hasattr(os, "register_at_fork"):
            os.register_at_fork(after_in_child=self._reset)

    def _reset(self) -> None:
        # In a forked child the parent's thread is gone and its lock may be held.
        self._cond = threading.Condition(threading.Lock())
        self._buf: list = []
        self._inflight = 0
        self._waiters: list[threading.Event] = []
        self._stopped = False
        self._thread: threading.Thread | None = None

    def offer(self, span: dict) -> None:
        with self._cond:
            if self._stopped or len(self._buf) >= self._max_queue:
                self.dropped += 1
                return
            self._buf.append(span)
            if self._thread is None:  # started lazily, so preforking servers fork first
                self._thread = threading.Thread(target=self._run, name="plumbline-exporter", daemon=True)
                self._thread.start()
            if len(self._buf) >= self._batch_size:
                self._cond.notify()

    def _run(self) -> None:
        while True:
            with self._cond:
                deadline = time.monotonic() + self._interval
                while not self._stopped and not self._waiters and len(self._buf) < self._batch_size:
                    left = deadline - time.monotonic()
                    if left <= 0:
                        break
                    self._cond.wait(left)
                batch, self._buf = self._buf[:self._batch_size], self._buf[self._batch_size:]
                self._inflight = len(batch)
            ok = True
            if batch:
                try:
                    self._transport(self._encode(batch))
                except Exception:  # never raise into the app; the agent may be down
                    ok = False
            with self._cond:
                if not ok:
                    self.export_errors += 1
                    self.dropped += len(batch)
                else:
                    self.exported += len(batch)
                self._inflight = 0
                if not self._buf:
                    for w in self._waiters:
                        w.set()
                    self._waiters.clear()
                if self._stopped and not self._buf:
                    self._thread = None
                    return

    def flush(self, timeout: float = 2.0) -> bool:
        """Wait until everything offered so far was sent (or dropped)."""
        with self._cond:
            if not self._buf and not self._inflight:
                return True
            if self._thread is None or not self._thread.is_alive():
                return False
            ev = threading.Event()
            self._waiters.append(ev)
            self._cond.notify()
        return ev.wait(timeout)

    def shutdown(self, timeout: float = 2.0) -> bool:
        ok = self.flush(timeout)
        with self._cond:
            self._stopped = True
            self._cond.notify()
        return ok
