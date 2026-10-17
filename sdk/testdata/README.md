# Shared SDK scenario

Every SDK implements the same small app (`sdk/python/tests/scenario.py`, and `sdk/js/test/scenario.mjs`) with a fixed clock and fixed IDs, and commits what it would export as `<sdk>-export.jsonl`, one OTLP JSON export per line.

`internal/aggregate` (`TestSDKScenarioExports`) parses every `*-export.jsonl` with the agent's own receiver code and requires it to aggregate to `expected-windows.json`; the concierge's ingest must agree. So the SDKs, the agent and the concierge cannot drift apart on attribute names or semantics.

The SDK test suites fail when their committed export is stale. `expected-windows.json` changes only with a reason in the PR (`go test ./internal/aggregate -run SDKScenario -update`, then check every number by hand against the scenario's comments).

Exports from an earlier SDK release are kept as `<sdk>-v<version>-export.jsonl` and must keep aggregating to `expected-windows-v<version>.json`, so an app that has not upgraded keeps producing the same evidence (D-056). The current scenario includes guardrail canaries (D-057); the v0.2 exports predate them.
