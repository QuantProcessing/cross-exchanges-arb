# Linux amd64 Build Script Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a repository script that builds `dist/cross-arb-linux-amd64` for Linux `amd64`.

**Architecture:** Keep the feature intentionally small: one shell script for the build path, one regression test that executes the script, and one documentation update that points developers at the script.

**Tech Stack:** Bash, Go 1.26, existing module root build entrypoint, Go `testing`

---

## File Map

### Existing files to modify

- `README.md`
- `README_CN.md`

### New files to create

- `scripts/build-linux-amd64.sh`
- `build_script_test.go`

## Task 1: Add a Failing Regression Test

**Files:**
- Create: `build_script_test.go`

- [ ] **Step 1: Write a test that runs `bash scripts/build-linux-amd64.sh` and expects `dist/cross-arb-linux-amd64` to exist**
- [ ] **Step 2: Run `env GOCACHE=/tmp/go-build go test ./... -run TestBuildLinuxAmd64Script -count=1` and verify it fails because the script does not exist yet**

## Task 2: Implement the Script

**Files:**
- Create: `scripts/build-linux-amd64.sh`

- [ ] **Step 1: Add a POSIX-friendly Bash script with `set -euo pipefail`**
- [ ] **Step 2: Resolve the repository root from the script path, create `dist/`, and run `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/cross-arb-linux-amd64 .`**
- [ ] **Step 3: Re-run `env GOCACHE=/tmp/go-build go test ./... -run TestBuildLinuxAmd64Script -count=1` and verify it passes**

## Task 3: Document the Default Packaging Path

**Files:**
- Modify: `README.md`
- Modify: `README_CN.md`

- [ ] **Step 1: Replace the raw Linux build example with the script-based path while still showing the produced binary name**
- [ ] **Step 2: Run `env GOCACHE=/tmp/go-build go test ./... -count=1` and verify the repo still passes**
