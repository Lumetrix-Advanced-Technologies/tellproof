## What this changes

<!-- One or two sentences, and the issue it closes, if any. -->

## How it was tested

<!-- The tests you added or ran. `make check` must pass. -->

## Checklist

- [ ] `make check` passes.
- [ ] No content leaves the customer: no prompt or completion text, span content or raw trace IDs are sent, logged or shown (or this change does not touch the agent or the SDKs).
- [ ] The verifiers agree: every vector still passes, and no vector was changed (or this change does not touch the trust core or `web/verify/`).
- [ ] No new dependency (or the issue explains why it is needed).
- [ ] README or `CHANGELOG.md` updated if users would notice.
