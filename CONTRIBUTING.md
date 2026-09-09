# Contributing

Contributions are welcome, and live validation is the single most valuable one right now: this provider has not yet been exercised against a real Morpheus appliance. See [Compatibility and limitations](docs/compatibility.md#known-uncertain-areas) for the specific areas most likely to need correction, and please report what you find either way.

## Before opening a pull request

1. Open an issue for significant behavior changes.
2. Keep changes focused.
3. Run formatting, tests, and a local build.
4. Add or update documentation.
5. Describe any live Omni/Morpheus testing completed.

```bash
gofmt -w $(find . -name '*.go' -not -path './vendor/*')
go mod tidy
go test ./...
go build ./cmd/omni-infra-provider-morpheus
```

## Pull request information

Include:

- Problem being solved
- Design and tradeoffs
- Test coverage
- Omni version tested
- Morpheus appliance version and cloud type tested
- Talos version tested
- Upgrade or migration considerations

## Code expectations

- Preserve idempotent reconciliation.
- Do not log credentials, join tokens, or cloud-init secrets.
- Validate all provider input before creating resources.
- Prefer deterministic names and explicit ownership descriptions.
- Make deletion tolerant of already-absent resources.
- Keep Machine Class schema and Go structs synchronized.
- Add tests for image identity and validation logic.

## Generated code

Files under `api/specs` are generated protobuf output. Changes to the protobuf schema should include regenerated Go files and the generator/version information used — see [Development](docs/development.md#regenerating-protobuf).

## License

By contributing, you agree that your contribution is licensed under the Mozilla Public License 2.0.
