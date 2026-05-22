# AGENTS.md

## Purpose

`discrawl` is a local-first Discord archive CLI. Preserve the read-only archive
model: inspect local exports, caches, databases, and snapshots without mutating
live Discord state.

Reusable archive mechanics belong in `crawlkit`. Keep Discord-specific parsing,
metadata, auth discovery, and CLI behavior in this repository.

## Development Rules

- Do not write to live Discord app data or real user archive stores.
- Use temp directories and temp SQLite databases in tests.
- Do not print tokens, cookies, message bodies, attachment contents, emails, or
  decrypted key material from diagnostics.
- Keep CLI output explicit about partial coverage, missing caches, and
  unavailable local state.
- Prefer small Go stdlib-first changes unless a dependency is already part of
  the repo contract.

## Validation

Run before handoff:

```bash
GOWORK=off go mod tidy
git diff --exit-code -- go.mod go.sum
GOWORK=off go vet ./...
GOWORK=off go test -count=1 ./...
```
