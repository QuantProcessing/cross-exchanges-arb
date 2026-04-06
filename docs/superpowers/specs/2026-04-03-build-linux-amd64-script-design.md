# Linux amd64 Build Script Design

**Date:** 2026-04-03

## Goal

Add a single command that packages the project as a Linux `amd64` executable without requiring developers to remember the cross-compilation flags.

## MVP Design

- Add `scripts/build-linux-amd64.sh`.
- The script will:
  - resolve the repository root from the script location
  - create `dist/`
  - run `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`
  - output `dist/cross-arb-linux-amd64`
  - print the artifact path on success
- Update `README.md` and `README_CN.md` to recommend the script as the default packaging path.

## Why This Shape

- A shell script matches the current repository style better than introducing a new `Makefile`.
- Keeping the output path fixed makes deployment and CI usage straightforward.
- Limiting scope to `linux/amd64` avoids unnecessary release abstraction for the current need.

## Acceptance Criteria

- Running `bash scripts/build-linux-amd64.sh` creates `dist/cross-arb-linux-amd64`.
- The script exits non-zero on build failure.
- Documentation points developers to the script instead of only the raw `go build` command.
