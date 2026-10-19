# Security

Tellproof's verifiers are the reason a reviewer can trust a pack without trusting us, so a bug that lets a tampered, unsigned or wrongly dated pack verify is the most serious kind of report. So is any way the agent or the SDKs could send prompt or completion text, span content or raw trace IDs off the customer's machine.

What adopting Tellproof exposes, and how each surface is defended, is in the [threat model](https://github.com/Lumetrix-Advanced-Technologies/tellproof/blob/main/docs/threat-model.md).

## Reporting a vulnerability

Report it privately at https://github.com/Lumetrix-Advanced-Technologies/tellproof/security/advisories/new (the "Report a vulnerability" button on the Security tab). Please do not open a public issue, discussion or pull request for it.

- We acknowledge a report within two working days and tell you what we will do and when.
- We fix or mitigate a confirmed vulnerability, release the fix, and publish a GitHub security advisory crediting you (unless you prefer otherwise), within 90 days of the report; sooner for anything that leaks content or lets a tampered pack verify.
- Please keep the details private until the advisory is published, or until 90 days have passed, whichever is first.

When reporting a verifier bug, include the envelope, the JWKS and the verifier you used (`tellproof-verify`, the verify page or `concierge/verify.py`). A failing case becomes a new test vector in `spec/vectors/`, which all three verifiers must then agree on.

## Supported versions

Security fixes go into the latest release of the agent, the verifier and the SDKs (see https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases). Upgrade to it; older releases are not patched.

## Verifying what you install

Every release binary, its checksums, the wheels, the npm tarballs and the container image carry signed build provenance: `gh attestation verify <file> --repo Lumetrix-Advanced-Technologies/tellproof`. The provenance is also attached to each release as `tellproof-<version>.intoto.jsonl`.
