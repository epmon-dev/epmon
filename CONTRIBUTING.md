# Contributing to epmon

## Ground rules

- One binary, one database file, zero cgo. Dependencies need justification.
- Honest history is the invariant: missing telemetry is `null`, never
  interpolated. Any change that fabricates uptime will be rejected.
- Headless-first: every mutation must work via the REST API. The embedded
  UI consumes the API; it never bypasses it.
- `prober`, `scheduler` and the `store` interface are public API with
  compatibility promises — changing their signatures needs discussion first.

## Workflow

```sh
go build ./... && go vet ./... && go test ./...
```

- Table-driven tests next to the code they cover (`*_test.go`).
- New API surface needs an OpenAPI entry (`internal/api/openapi.yaml` is
  embedded and served live — the binary always serves its own contract).
- New metrics need a name/label stability note: renames are breaking.
- Run `gofmt -l .` clean before pushing. CI runs build, vet and `-race`.

## Pull requests

Small, one concern per PR, with the failing-then-passing test included.
Describe the operator-visible behavior change, not just the diff.
