# forge — The Forge V5 (Go)

Single-binary rewrite of the V4 Python system. **Read `../docs/v5-plan.md` first**, then the
three frozen contracts before writing any code:

1. `../docs/v5-api-contract.md` — the HTTP/SSE surface (the PWA must run unmodified)
2. `../docs/v5-go-contracts.md` — interfaces, package ownership, dependency rules
3. `../docs/v5-store-schema.md` — SQLite schema v1 + `migrate-v4` design

Ground rules for parallel tracks:

- Edit only the packages your track owns (ownership table in contract 2). `go.mod`/`go.sum`
  belong to the Phase 9 integrator.
- Go work never touches `../forge/*.py` or `../web/src/`.
- Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`.
- CI (`.github/workflows/v5-go.yml`) must stay green: build, vet, staticcheck, `test -race`,
  linux/amd64 cross-compile, web build.

Build and run the skeleton:

```bash
cd go
go test ./...
go build ./cmd/forge
./forge -listen :5001   # stub wiring; GET /api/v1/health answers
```

Module path `github.com/jsaigou/the-forge` is a placeholder (plan open question 1) —
grep for `forge-placeholder` when renaming.
