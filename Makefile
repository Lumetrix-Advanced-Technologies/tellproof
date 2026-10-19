# Tellproof evidence agent, verifier and SDKs. Requires Go 1.26+; the tests
# also use Node 22+ and Python 3.10+ with `cryptography` (no other packages).
SHELL := /bin/bash
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2026.2.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
VERSION ?= dev

.PHONY: check fmt-check vet staticcheck vuln test vectors sdk-test package-test release-binaries release-sums packages agent-image agent-dry leak-test

check: fmt-check vet staticcheck vuln test vectors sdk-test package-test

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...
	go vet -tags bedrock ./...

staticcheck:
	go run $(STATICCHECK) ./...

vuln:
	go run $(GOVULNCHECK) ./...
	go run $(GOVULNCHECK) -tags bedrock ./...

test:
	go test -race ./...
	go test -race -tags bedrock ./internal/usage/...

# Every verifier must agree on every file in spec/vectors/ and spec/jcs-vectors.json.
vectors:
	go test -race -run 'Vector|JCS' ./internal/...
	node --test web/verify/verify.test.js
	python3 concierge/verify.py spec/vectors/test-public-key.pem spec/vectors/pass/*.dsse.json
	python3 concierge/verify.py spec/vectors/test-public-key.pem spec/vectors/extra/pass-no-disclosure.dsse.json
	python3 -m unittest concierge/tests/test_verify_keys.py

sdk-test:
	python3 -m unittest discover -s sdk/python/tests
	node --test sdk/js/test/sdk.test.mjs sdk/js/test/instrument.test.mjs sdk/js/test/real-libs.test.mjs sdk/js/test/cli.test.mjs sdk/js/test/simple.test.mjs

# The content-leak regression suite alone (D-065): spec/leak-corpus.json through the
# agent (dry run, receiver, signed log, dashboard, reports), Langfuse, the identifier
# rules (Go and Python) and both SDKs. make check runs it too.
leak-test:
	go test -count=1 -run 'Leak|Identifier|ContentInReadAttributes' ./cmd/agent ./internal/otlp ./internal/langfuse
	python3 -m unittest discover -s sdk/python/tests -p 'test_leak.py'
	node --test --test-name-pattern 'leak corpus' sdk/js/test/instrument.test.mjs

# The pip and npm packages of the agent, built from release binaries (D-054).
package-test:
	python3 -m unittest discover -s packaging -p 'test_*.py'

packages: release-binaries
	python3 packaging/build_wheels.py --dist dist --version $(VERSION) --out dist/pypi
	python3 packaging/build_npm.py --dist dist --version $(VERSION) --out dist/npm

# Static binaries: tellproof-verify, tellproof-agent and tellproof-agent-bedrock
# (with the Bedrock usage poller) for every platform, or for GOOS/GOARCH alone.
release-binaries:
	@mkdir -p dist
	@for p in $(if $(GOOS),$(GOOS)/$(GOARCH),$(PLATFORMS)); do os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/tellproof-verify-$$os-$$arch$$ext ./cmd/verify || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/tellproof-agent-$$os-$$arch$$ext ./cmd/agent || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -tags bedrock -o dist/tellproof-agent-bedrock-$$os-$$arch$$ext ./cmd/agent || exit 1; \
	done; ls dist

release-sums:
	cd dist && sha256sum tellproof-* $$(ls RELEASE-NOTES.md 2>/dev/null) > SHA256SUMS && cat SHA256SUMS

agent-image:
	docker build -f cmd/agent/Dockerfile --build-arg VERSION=$(VERSION) -t tellproof-agent:$(VERSION) .

agent-dry:
	TELLPROOF_SALT_SECRET=$${TELLPROOF_SALT_SECRET:-dry-run-salt-not-a-secret} go run ./cmd/agent --config agent.example.json --dry-run --replay concierge/testdata/otlp-sample.jsonl

# The local dashboard on a week of synthetic data, for working on its design.
dashboard-demo:
	rm -rf dist/demo-log && go run ./tools/demolog -out dist/demo-log -days 7 && go run ./cmd/agent dashboard --data-dir dist/demo-log
