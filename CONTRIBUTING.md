# Contributing to Tellproof

Thank you for helping. Bug reports, new provider integrations, canary probe kinds, docs fixes and test vectors are all welcome.

## Two rules every change keeps

- **No content leaves the customer.** No code path in the agent (`cmd/agent`, `internal/otlp`, `internal/langfuse`, `internal/usage`, `internal/queue`) or the SDKs (`sdk/`) may send prompt or completion text, span content or raw trace IDs. The SDKs never read message content at all, and `--dry-run` must show exactly what would be sent. A change that weakens this is not merged, however useful.
- **The verifiers agree.** A change to `internal/jcs`, `internal/dsse`, `internal/merkle`, `internal/pack` or `web/verify/` must pass every file in `spec/vectors/` and `spec/jcs-vectors.json` in all three verifiers (Go, the browser and `concierge/verify.py`). Vectors are added, never changed: if a test fails, fix the code, not the vector.

And a few habits:

- **Standard library first.** The Go code uses only the standard library (plus the AWS SDK behind the `bedrock` build tag). The SDKs have no runtime dependencies and must never break or block the host app. The web pages have no npm packages, frameworks or build step. A new dependency needs a strong reason; ask in an issue first.
- **Small pull requests**, one change each, with tests.
- **Security problems are not issues.** Report them privately: see [SECURITY.md](SECURITY.md).

## Set up and test

```bash
git clone https://github.com/Lumetrix-Advanced-Technologies/tellproof.git && cd tellproof
make check
```

`make check` needs Go 1.26+, Node.js 22+ and Python 3.10+ with `cryptography`. It runs gofmt, vet, staticcheck, govulncheck, the race tests, every verifier on every vector, and the SDK and packaging tests. It must pass before a pull request is reviewed. The README's [Develop and contribute](README.md#develop-and-contribute) section lists the other targets and where the code lives.

## Pull requests

1. For anything bigger than a fix, open an issue first so we can agree on the approach.
2. Branch from `main`, make the change, add or update tests, run `make check`.
3. Update the README or `CHANGELOG.md` when users would notice the change.
4. Open the pull request and fill in the template.

**How a pull request lands.** This repository is exported from the maintainers' working repository, which also holds the hosted control plane. A pull request is reviewed and discussed here as usual; once it is approved, a maintainer applies it there, keeping you as the author (`Co-authored-by`), and the next export brings it back here, where the pull request is closed with a link to the commit. Dependabot pull requests land the same way.

## Code of Conduct

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Licence

Tellproof is Apache-2.0. By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE), as its section 5 describes.
