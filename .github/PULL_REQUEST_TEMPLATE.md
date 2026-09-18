## What changes for operators?

## Failing-then-passing test

## Checklist

- [ ] `go build ./... && go vet ./... && go test ./...` green
- [ ] `gofmt -l .` clean
- [ ] OpenAPI updated if API surface changed (`internal/api/openapi.yaml`)
- [ ] No fabricated history: missing telemetry still reads `null`
- [ ] No secrets, tokens, or hosts I don't own in the diff
