#!/usr/bin/env bash
# Checks the tree of the spec files of ACME.
set -euo pipefail
spec-check tree --dir .dispatch/spec
