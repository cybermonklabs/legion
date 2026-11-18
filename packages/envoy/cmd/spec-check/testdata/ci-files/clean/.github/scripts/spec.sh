#!/usr/bin/env bash
# Checks the tree of the spec files of ACME.
set -euo pipefail
go run ./cmd/spec-check tree --dir .dispatch/spec
