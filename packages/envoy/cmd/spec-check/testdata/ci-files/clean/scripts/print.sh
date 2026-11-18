#!/usr/bin/env bash
# Prints the approved text of one statement, so that a change copies it.
set -euo pipefail
spec-check print --approved a.json --id an-order-survives-a-restart
