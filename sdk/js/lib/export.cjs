'use strict';
// Background exporter: a bounded buffer drained one batch at a time.
// offer() never waits on the network and never throws; when the buffer is
// full the span is dropped and counted. Timers are unref'd, so the SDK never
// keeps a process alive.

// POST to the agent. Only http and https URLs (anything else leaves the SDK
// off); a redirect is an error, so spans never follow one to another host;
// the answer's body is discarded unread, whatever its size.
function httpTransport(url, timeoutMs = 3000) {
  const { protocol } = new URL(url);
  if (protocol !== 'http:' && protocol !== 'https:') throw new Error('the agent URL must be http:// or https://');
  return async (body) => {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'User-Agent': 'tellproof-js' },
      body,
      redirect: 'error',
      signal: AbortSignal.timeout(timeoutMs),
    });
    await r.body?.cancel();
    if (!r.ok) throw new Error(`agent answered HTTP ${r.status}`);
  };
}

class Exporter {
  constructor(transport, encode, { maxQueue = 2048, batchSize = 512, intervalMs = 1000 } = {}) {
    this._transport = transport;
    this._encode = encode;
    this._maxQueue = maxQueue;
    this._batchSize = batchSize;
    this._intervalMs = intervalMs;
    this._buf = [];
    this._inflight = false;
    this._timer = null;
    this._waiters = [];
    this._stopped = false;
    this.exported = 0;
    this.dropped = 0;
    this.exportErrors = 0;
  }

  offer(span) {
    if (this._stopped || this._buf.length >= this._maxQueue) {
      this.dropped++;
      return;
    }
    this._buf.push(span);
    if (this._buf.length >= this._batchSize) this._schedule(0);
    else this._schedule(this._intervalMs);
  }

  _schedule(ms) {
    if (this._inflight) return; // the running drain picks the rest up
    if (this._timer !== null) {
      if (ms > 0) return;
      clearTimeout(this._timer);
    }
    this._timer = setTimeout(() => {
      this._timer = null;
      this._drain();
    }, ms);
    if (typeof this._timer.unref === 'function') this._timer.unref();
  }

  async _drain() {
    if (this._inflight) return;
    this._inflight = true;
    try {
      while (this._buf.length) {
        const batch = this._buf.splice(0, this._batchSize);
        try {
          await this._transport(this._encode(batch));
          this.exported += batch.length;
        } catch {
          // never throw into the app; the agent may be down
          this.exportErrors++;
          this.dropped += batch.length;
        }
        if (this._buf.length && this._buf.length < this._batchSize && !this._waiters.length && !this._stopped) break;
      }
    } finally {
      this._inflight = false;
    }
    if (this._buf.length) this._schedule(this._intervalMs);
    else {
      const w = this._waiters.splice(0);
      for (const resolve of w) resolve(true);
    }
  }

  // Resolves true once everything offered so far was sent (or dropped), false
  // on timeout. Unlike the background timers, a flush keeps the process alive
  // while it waits: the caller asked to wait (a script's last line, a
  // serverless handler), so the event loop must not exit under it.
  flush(timeoutMs = 2000) {
    if (!this._buf.length && !this._inflight) return Promise.resolve(true);
    return new Promise((resolve) => {
      const t = setTimeout(() => resolve(false), timeoutMs); // ref'd on purpose; cleared when done
      this._waiters.push((v) => {
        clearTimeout(t);
        resolve(v);
      });
      if (!this._inflight) {
        if (this._timer !== null) {
          clearTimeout(this._timer);
          this._timer = null;
        }
        this._drain();
      }
    });
  }

  async shutdown(timeoutMs = 2000) {
    const ok = await this.flush(timeoutMs);
    this._stopped = true;
    if (this._timer !== null) clearTimeout(this._timer);
    return ok;
  }
}

module.exports = { Exporter, httpTransport };
