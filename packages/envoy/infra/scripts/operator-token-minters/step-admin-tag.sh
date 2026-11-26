# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-admin-tag.sh: the step "admin tag". Sourced by operator-token-minters.sh,
# whose header says what each step checks.
# --- admin tag ---
step="admin tag of $SECRET"
if ! have agent-secrets; then
  unknown "$step" "agent-secrets is not on PATH"
elif agent-secrets secret show "$SECRET" --json >"$WORK/secret.json" 2>"$WORK/secret.err"; then
  if ! jq -e 'type == "object"' "$WORK/secret.json" >/dev/null 2>&1; then
    unknown "$step" "agent-secrets secret show $SECRET --json printed no JSON object"
  else
    admin="$(jq -r '.admin // ""' "$WORK/secret.json")"
    tier="$(jq -r '.tier // ""' "$WORK/secret.json")"
    [ "$admin" = true ] || finding "secret:$SECRET" "admin is \"$admin\", not \"true\": without the administrator tag a model judge may decide its release" \
      admin "secret:$SECRET" "" ""
    [ "$tier" = human ] || finding "secret:$SECRET" "tier is \"$tier\", not \"human\"" tier "secret:$SECRET" "" ""
    if [ "$admin" = true ] && [ "$tier" = human ]; then
      pass "$step" "admin is \"true\" and tier is \"human\": each release waits on a person's approval"
    fi
  fi
# A missing secret is a finding, in each wording agent-secrets has had for it: gRPC's own
# `rpc error: code = NotFound` (legion-envoy-v30.43.0, read on 2026-11-21), the store interceptor's
# `GetSecret answered NotFound`, and AWS's ResourceNotFoundException from DescribeSecret.
elif grep -Eq 'code = NotFound|GetSecret answered NotFound|DescribeSecret.*ResourceNotFoundException' "$WORK/secret.err"; then
  finding "secret:$SECRET" "no such secret: the operator's credential must be stored at human tier with --admin true" secret "secret:$SECRET" "" ""
else
  unknown "$step" "agent-secrets secret show $SECRET --json was refused: $(last_line "$WORK/secret.err")"
fi
