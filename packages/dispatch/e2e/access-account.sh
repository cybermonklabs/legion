# shellcheck shell=bash
# The secrets broker's service account and the audience its token carries for Dispatch, under the
# harness switch DISPATCH_E2E_ACCESS_ISSUER=1 (e2e/harness-access.ts). Sourced, never run, by the
# two scripts that must name the same pair: e2e/run-access-issuer.sh keeps this account's token
# for this audience, and e2e/run-server.sh has the server verify tokens for this audience and list
# this account in DISPATCH_ACCESS_ACCOUNTS, the account that reads the membership answer. Made-up
# names, as every account this harness names is: the repository is public.
# shellcheck disable=SC2034 # read by the scripts that source this file
access_audience=example-dispatch
# shellcheck disable=SC2034 # read by the scripts that source this file
access_account=system:serviceaccount:example-secrets:example-broker
