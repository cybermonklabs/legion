#!/usr/bin/env bash
# Compares the spec files of ACME with the approved spec.
set -euo pipefail
go run ./cmd/spec-check compare --dir .dispatch/spec --approved a.json
