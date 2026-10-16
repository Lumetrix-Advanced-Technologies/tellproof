'use strict';
// Encoding of recorded spans as an OTLP/HTTP JSON ExportTraceServiceRequest.

const STATUS_ERROR = 2;

function value(v) {
  if (typeof v === 'boolean') return { boolValue: v };
  if (typeof v === 'number' && Number.isInteger(v)) return { intValue: String(v) }; // 64-bit ints travel as strings
  if (typeof v === 'number') return { doubleValue: v };
  if (typeof v === 'bigint') return { intValue: String(v) };
  return { stringValue: String(v) };
}

function attrs(d) {
  return Object.keys(d)
    .sort()
    .map((k) => ({ key: k, value: value(d[k]) }));
}

function encode(spans, resource, scopeVersion) {
  const out = spans.map((s) => {
    const span = {
      traceId: s.traceId,
      spanId: s.spanId,
      name: s.name,
      kind: s.kind,
      startTimeUnixNano: String(s.start),
      endTimeUnixNano: String(s.end),
      attributes: attrs(s.attrs),
      status: s.error ? { code: STATUS_ERROR } : {},
    };
    if (s.parent) span.parentSpanId = s.parent;
    return span;
  });
  return JSON.stringify({
    resourceSpans: [
      {
        resource: { attributes: attrs(resource) },
        scopeSpans: [{ scope: { name: 'plumbline', version: scopeVersion }, spans: out }],
      },
    ],
  });
}

module.exports = { encode };
