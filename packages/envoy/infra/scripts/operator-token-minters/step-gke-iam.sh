# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-gke-iam.sh: the steps "live: GKE IAM" and "live: minters". Sourced by
# operator-token-minters.sh, whose header says what each step checks.
live_iam gke
live_iam gsa
[ "${CHECK_MISSING[broker]:-}" = skip ] || live_iam broker
