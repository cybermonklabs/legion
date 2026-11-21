#!/usr/bin/env bash
# register-gate-basis.sh's run across two projects on a Dispatch built from this checkout, which
# takes no parent of another project on any route: the run e2e.yaml makes on a GitHub-hosted
# runner for the case the cross-project gate exists for. It is register-gate-basis.sh with
# REGISTER_GATE_BASIS_ACROSS_PROJECTS=1 and REGISTER_GATE_BASIS_LINK=sql, and nothing else: each
# root of project LEGION is set under an issue of project OTHR by a write straight into the scratch
# Dispatch's database, and the run holds that the root's registration on an approved issue of OTHR
# opens its step, that a registration on an open approval request of OTHR opens at the one
# controller tick after the person approves, and that a registration once the issue of OTHR is
# accepted is refused with 409 BASIS_ACCEPTED. Every step, check and output is
# register-gate-basis.sh's (scripts/e2e/README.md, "register-gate-basis.sh").
#
# Run it from a checkout on a Linux host with go, bun (after `bun install` at the checkout's
# root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/register-gate-basis-across-projects.sh
# It reads what register-gate-basis.sh reads, REGISTER_GATE_BASIS_EVIDENCE_DIR and
# REGISTER_GATE_BASIS_DISPATCH_ROOT among them, and sets the two variables above whatever the
# caller's environment holds. A run against a Dispatch that takes a parent of another project
# through its own route is register-gate-basis.sh with REGISTER_GATE_BASIS_ACROSS_PROJECTS=1 alone.
set -euo pipefail

export REGISTER_GATE_BASIS_ACROSS_PROJECTS=1 REGISTER_GATE_BASIS_LINK=sql
exec bash "$(dirname "$0")/register-gate-basis.sh"
