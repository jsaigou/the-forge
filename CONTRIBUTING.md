# Contributing to The Forge

Thanks for your interest in improving The Forge.

## Ground rules

1. **Open an issue first.** Describe the problem or proposal before writing
   code — scheduler and auth behavior especially have design context worth
   aligning on (see `docs/adr/`).
2. **PRs against `main`.** Keep them focused; one logical change per PR.
3. **Sign-off optional.** DCO sign-off (`git commit -s`) is appreciated but
   not enforced; by contributing you agree your work is licensed under the
   repository's Apache-2.0 license.

## Development

```bash
go build ./go/cmd/forge          # daemon + CLIs live under go/cmd/
go test ./...                    # Go test suite
cd web && npm ci && npm run build  # dashboard (React + Vite)
```

CI runs the same checks on every push/PR (see `.github/workflows/ci.yml`).

## What we do not accept

- Generated binaries or model weights as repo content.
- Secrets of any shape — the scrub gate fails the build on credential-like
  patterns, private hostnames or personal paths.
