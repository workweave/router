# record_provider_fixtures

Refreshes the translation-conformance **upstream fixtures** from live providers.

The conformance suite (`internal/proxy/conformance_*_test.go`) is hermetic: a mock
provider replays a canned response from `internal/proxy/testdata/conformance/`, and
the test asserts the router translated it correctly. Those canned responses should
be *real* provider output. This tool regenerates them: for each case it runs the
same inbound Anthropic body through the router's own `Prepare*` emit, sends the
translated request to the real upstream, and writes the raw response to the fixture.

It is **not** a test (`package main`), so `go test ./...` never runs it and CI never
hits the network. It is gated on `RECORD=1` and the relevant API key being set;
cases whose key is missing are skipped.

## Usage (from the repo root)

```bash
RECORD=1 \
  OPENROUTER_API_KEY=… \
  OPENAI_API_KEY=… \
  GOOGLE_API_KEY=… \
  go run ./scripts/record_provider_fixtures
```

Then regenerate the goldens and review the diff before committing:

```bash
go test ./internal/proxy/ -run TestConformance -update
git diff internal/proxy/testdata/conformance
```

## Adding a case

Add an entry to `cases` in `main.go` whose `fixture` path matches the
`upstreamFixture` of the conformance case that reads it. Keep the inbound
Anthropic body identical to the conformance case so the recorded response
corresponds to the request the suite actually sends.
