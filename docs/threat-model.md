# Threat model: what adopting Tellproof exposes

Adding Tellproof to a project adds code to the app's process (the SDK), a local service (the agent), files on disk (the data directory), commands (the `tellproof` launcher), a CI step (the Action) and packages to the supply chain. This page lists, for each, what an attacker could try, what stops it, the test that keeps it stopped, and what is left. The design is in [HOW-IT-WORKS.md](../HOW-IT-WORKS.md#security-model). Report anything missing here privately: [SECURITY.md](../SECURITY.md).

**Assets, in order:** the app's prompts, completions and user data (must never leave, and the SDK never reads them); the app's availability (the SDK must never break or slow it); the integrity of the evidence (no one may add, edit or remove what it records); the agent's signing key and salt secret; raw trace IDs (kept in the data directory for disclosure, never sent).

**Out of scope:** an attacker who already runs code as the same user, or can write the data directory, controls the agent's evidence and key anyway; a compromised machine cannot produce trustworthy evidence. Calls that bypass the SDK are not recorded (trace coverage measures that gap).

## The SDK, inside the app's process

| Threat | Mitigation | Held by |
| --- | --- | --- |
| Content leaves through spans | The SDK never reads message arguments, completions, function arguments, return values or exception messages; it records only provider, model, region, token counts, operation, timing, status and the helpers' facts. | `test_leak.py`, `instrument.test.mjs` (the leak corpus through every instrumented call and helper) |
| The SDK breaks or slows the app | Every instrumentation path is wrapped; errors are swallowed and counted; spans go to a bounded buffer drained by one background thread (an unref'd timer in Node.js) and are dropped and counted when it is full; `TELLPROOF_DISABLED=1`; `init()` idempotent. | SDK tests of failures, full buffers, forks, disabled mode |
| Spans go through a proxy, or are redirected to another host | Python: no proxy handler, and urllib never re-posts a body on a redirect. Node.js: `redirect: 'error'`. Only `http://` and `https://` agent URLs; anything else leaves the SDK off. The agent's answer is read at most 64 KB (Python) or discarded unread (Node.js). | `TransportTest` (Python), "the transport never follows a redirect" (Node.js) |
| The SDK breaks request handling | Web-framework entry points (Flask's `wsgi_app`, Starlette's `__call__`, Django's `get_response`, Node's `Server#emit('request')`) are wrapped to pass everything through unchanged inside a request scope; a failure to open the scope runs the handler without one; `requests=False` turns it off. | `test_simple.py` (fakes), `test_real_libs.py` (real Flask, FastAPI, Django), `simple.test.mjs` (a real `node:http` server) |
| Marked functions leak or change data | `guardrail_check` and `redactor` wrappers pass arguments, results and exceptions through unchanged; a check's result is only compared with `True`, `"block"` and `"flag"`; nothing of either is recorded. | the leak suites (both SDKs run the corpus through them) |
| Evidence overclaims coverage | A decision or redaction counts only for the request it ran in; outside a request it is not attributed (a block still counts as one blocked call) and is counted in `stats()["unscoped"]`. | `test_simple.py`, `simple.test.mjs` |
| A dependency of the SDK is compromised | The SDK has none: the Python standard library, Node.js built-ins. The only optional package is our own agent, which the SDK never imports. | `test_cli.py`, `cli.test.mjs` (the manifests and the SDK's modules) |

*Residual:* with `NODE_USE_ENV_PROXY=1` (Node.js 24+), Node's `fetch` honours `HTTP_PROXY`; add the agent's host to `NO_PROXY`. Spans carry raw trace IDs to the agent, as plain HTTP when the agent is on another host; keep the agent on loopback, in the pod, or behind TLS.

## The agent's receiver (`127.0.0.1:4318`)

| Threat | Mitigation | Held by |
| --- | --- | --- |
| A web page posts fake spans (evidence poisoning) | JSON only: a cross-site JSON POST needs a CORS preflight, which the receiver never grants. On loopback the receiver answers only under a local Host name (`localhost`, `*.localhost`, a loopback address), so DNS rebinding cannot reach it. | `TestReceiverRefusesNonLocalHostsOnLoopback` |
| Another machine posts spans | Loopback only, unless `otlp_allow_remote` is set; the config refuses a non-loopback listen without it. | `TestConfigRefusals` |
| Memory exhaustion | Body and decoded-size limits, gzip bombs included; header and read timeouts. | receiver tests, fuzz targets for OTLP parsing |
| Content in spans reaches a batch | Message attributes are never decoded; values copied into a batch must be identifiers (`spec/identifiers.json`), else `invalid`. | `make leak-test` (agent end to end) |

*Residual:* the receiver has no authentication. Any local process, and with `otlp_allow_remote` anything on the network that can reach it, can add spans. Restrict the port (a NetworkPolicy, a sidecar in the pod).

## The dashboard (`127.0.0.1:4319`)

| Threat | Mitigation | Held by |
| --- | --- | --- |
| A web page reads the evidence (DNS rebinding) | Local Host names only; no CORS; `Cross-Origin-Resource-Policy: same-origin`. | `TestRefusesOtherHostsAndWrites` |
| Script injection from recorded values | No `innerHTML` or `eval` (a test fails on either); a strict Content-Security-Policy; nothing loaded from elsewhere; recorded names are identifiers. | `dashboard_test.go` |
| Changes through the dashboard | GET only; it writes nothing. | `TestRefusesOtherHostsAndWrites` |

## The data directory and the local commands

| Threat | Mitigation | Held by |
| --- | --- | --- |
| Other users read the key, the salt or raw trace IDs | Directory 0700; key, salt, batches, the run record and the log 0600; `doctor` warns about loose permissions and about git committing the key; `init` offers `.gitignore`. | `doctor` and setup tests |
| `stop` hits an unrelated process | `stop` sends no signal and opens no port: it writes `agent.stop`, which only the agent reads. A record of a dead agent is cleared. Only `stop --force`, after 15 seconds and with the process still alive, ends the recorded pid. | `background_test.go` |
| A second agent writes the same log | Refused while the first runs. | `TestStartStatusStopInTheBackground` |

## The `tellproof` launcher

| Threat | Mitigation | Held by |
| --- | --- | --- |
| A `tellproof-agent` planted in a cloned repository runs instead of the agent | The launcher runs the installed package's bundled binary; failing that, it searches only absolute `PATH` directories, never the working directory (which Windows, `shutil.which` and an empty or `.` PATH entry would search). | "an agent in the working directory is never run" (Python and Node.js) |
| Install-time code execution | No install scripts in any package; nothing downloaded at install time. | packaging tests |

## CI: the Action and `ci`

| Threat | Mitigation | Held by |
| --- | --- | --- |
| Script injection through Action inputs | Every input reaches the shell through an environment variable, never an expression in the script. | review |
| An unpinned agent version in CI | The Action's default `version` is the release it belongs to, so pinning the Action (by tag or commit) pins the agent; `version: latest` opts out. | `release.yml` refuses a tag whose Action default differs |
| CI evidence reveals code | `ci run` records gate names, exit status, duration, a hash of the command line and test counts; never output, code or the command line. Canary patches apply in a temporary worktree. | `ci_test.go` |

## Supply chain

| Threat | Mitigation | Held by |
| --- | --- | --- |
| A tampered release | Binaries, checksums, wheels, npm tarballs and the image carry signed build provenance (SLSA via GitHub artifact attestations); `gh attestation verify` checks them. | `release.yml`, `packages.yml` |
| A hijacked publish | PyPI: trusted publishing only, behind a required reviewer. npm: CI can only stage; each version is approved by a maintainer with two-factor authentication. No registry token is stored. | D-062 |
| Workflow compromise | Every action pinned to a commit SHA, `persist-credentials: false`, permissions per job, Dependabot, OpenSSF Scorecard. | the pin test (D-063) |
| An unexpected major version | The SDK's optional agent dependency is bounded below 1.0 on PyPI and npm. | `test_cli.py`, `cli.test.mjs` |

## Not yet done

- Continuous fuzzing in CI (targets exist for OTLP, JCS and DSSE).
- SLSA Build L3 and a reproducible-build check.
- An independent review of the verifiers and the signing path before the first paying customer.
