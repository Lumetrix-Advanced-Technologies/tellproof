# Security

Plumbline's verifiers are the reason a reviewer can trust a pack without trusting us, so a bug that lets a tampered, unsigned or wrongly dated pack verify is the most serious kind of report. So is any way the agent or the SDKs could send prompt or completion text, span content or raw trace IDs off the customer's machine.

Report a vulnerability privately with GitHub's "Report a vulnerability" button on this repository (Security tab). Please do not open a public issue. We acknowledge reports within two working days and tell you what we will do and when.

When reporting a verifier bug, include the envelope, the JWKS and the verifier you used (`plumbline-verify`, the verify page or `concierge/verify.py`). A failing case becomes a new test vector in `spec/vectors/`, which all three verifiers must then agree on.
