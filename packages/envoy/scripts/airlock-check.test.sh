#!/usr/bin/env bash
# Exercises k7's required operator inputs before it creates a work directory or starts a stack.
# A stand-in mktemp stops the accepted run at that boundary, without network calls or builds.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/lcsnames-airlock-inputs.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
cat >"$work/bin/mktemp" <<'EOF'
#!/usr/bin/env bash
echo 'airlock-input-test: reached work directory creation' >&2
exit 93
EOF
chmod +x "$work/bin/mktemp"

failed=0
check_id=k7
run_check() {
  status=0
  output="$(env -u AIRLOCK_GWS_READ_SECRET -u AIRLOCK_GWS_ADMIN_SECRET -u AIRLOCK_GWS_TIER_KEY \
    PATH="$work/bin:$PATH" "$@" bash "$script_dir/airlock-check.sh" "$check_id" 2>&1)" || status=$?
}
check() {
  if "$@"; then
    printf 'PASS: %s\n' "$label"
  else
    printf 'FAIL: %s (exit %s): %s\n' "$label" "$status" "$output"
    failed=$((failed + 1))
  fi
}

inputs=(AIRLOCK_GWS_READ_SECRET AIRLOCK_GWS_ADMIN_SECRET AIRLOCK_GWS_TIER_KEY)
values=(GWS_OPERATOR_READ_OAUTH GWS_OPERATOR_ADMIN_OAUTH operator)
run_check
label='k7 refuses all missing inputs before setup'; check test "$status" -eq 2
for input in "${inputs[@]}"; do
  label="the refusal names $input"; check test "${output#*"$input"}" != "$output"
done
for i in "${!inputs[@]}"; do
  supplied=()
  for j in "${!inputs[@]}"; do
    if [[ "$i" != "$j" ]]; then supplied+=("${inputs[$j]}=${values[$j]}"); fi
  done
  run_check "${supplied[@]}"
  label="k7 refuses when only ${inputs[$i]} is missing"; check test "$status" -eq 2
  label="the individual refusal names ${inputs[$i]}"; check test "${output#*"${inputs[$i]}"}" != "$output"
  run_check "${supplied[@]}" "${inputs[$i]}="
  label="k7 refuses an empty ${inputs[$i]}"; check test "$status" -eq 2
done
run_check AIRLOCK_GWS_READ_SECRET=GWS_OPERATOR_READ_OAUTH AIRLOCK_GWS_ADMIN_SECRET=GWS_OPERATOR_ADMIN_OAUTH AIRLOCK_GWS_TIER_KEY=operator
label='all three inputs pass preflight and reach setup'; check test "$status" -eq 93
label='the accepted run reaches the work directory boundary'; check test "${output#*'airlock-input-test: reached work directory creation'}" != "$output"
check_id=k6 run_check
label='other checks do not require the k7 inputs'; check test "$status" -eq 93

# Intercept the real driver just before check_k7 starts its stack. Exercise its fixture builders
# and the generated Google Workspace commands, without fetching consumers or calling a service.
cat >"$work/lcsnames-builders.sh" <<'HOOK'
probe_builders() {
  set -e
  add_fake_secret() { printf '%s\n' "$1" >>"$PROBE_DIR/lcsnames-names"; }
  : >"$PROBE_DIR/lcsnames-names"
  AIRLOCK_RULES_FILE="$PROBE_DIR/lcsnames-rules.json"
  printf '{"secrets":[]}\n' >"$AIRLOCK_RULES_FILE"
  STACK_SECRETS_PREFIX=lcsnames-fixture-
  k7_names
  jq -e --arg read "$AIRLOCK_GWS_READ_SECRET" --arg admin "$AIRLOCK_GWS_ADMIN_SECRET" \
    'any(.secrets[]; .name == $read and .hosts == ["www.googleapis.com", "oauth2.googleapis.com"]) and any(.secrets[]; .name == $admin and .hosts == ["admin.googleapis.com"])' "$WORK_DIR/k7/rules.json" >/dev/null
  names="$(cat "$PROBE_DIR/lcsnames-names")"
  [[ "$names" == *lcsnames-fixture-gws-example-view-2* && "$names" == *lcsnames-fixture-gws-example-admin-2* ]]
  K7_DOTFILES="$PROBE_DIR/lcsnames-consumer"
  mkdir -p "$K7_DOTFILES/gws"
  jq -n --arg key "$AIRLOCK_GWS_TIER_KEY" \
    '{($key):{tiers:{read:{command:["secret-run","GWS_EXAMPLE_VIEW_2"]}}}}' >"$K7_DOTFILES/gws/accounts.json"
  K7_GH_DIR=/unused
  AIRLOCK_S_URL=https://unused.example
  AIRLOCK_BOTO3_PYTHON=python3
  STANDS_BIN=/unused
  k7_steps
  env -u BASH_ENV bash -n "$WORK_DIR/k7/steps.sh"
  # Select commands from the actual generated script, not copies of their implementation.
  cat >"$PROBE_DIR/lcsnames-steps.sh" <<'STEPS'
set -euo pipefail
say() { printf '%s\n' "$1"; }
STEPS
  selected=0
  while IFS= read -r line; do
    case "$line" in
      gws_read_secret=* | gws_admin_secret=* | gws_tier_key=* | 'say "gws_tier='* | 'say "gws='* | 'say "admin='*)
        printf '%s\n' "$line" >>"$PROBE_DIR/lcsnames-steps.sh"
        selected=$((selected + 1))
        ;;
    esac
  done <"$WORK_DIR/k7/steps.sh"
  [[ "$selected" == 6 ]]
  env -u BASH_ENV PATH="$PROBE_DIR/consumer-bin:$PATH" \
    GWS_EXAMPLE_VIEW_2=read-placeholder GWS_EXAMPLE_ADMIN_2=admin-placeholder \
    bash "$PROBE_DIR/lcsnames-steps.sh"
}
trap 'if [[ "$BASH_COMMAND" == check_k7 ]]; then probe_builders; exit; fi' DEBUG
HOOK
mkdir -p "$work/consumer-bin"
cat >"$work/consumer-bin/secret-run" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --reason && "$2" == 'gws operator read' && "$3" == GWS_EXAMPLE_VIEW_2 && "$4" == -- ]]
shift 4
exec "$@"
RUNNER
cat >"$work/consumer-bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
header="$(cat)"
case "$*" in
  *'https://www.googleapis.com/drive/v3/about?fields=user'*) [[ "$header" == 'Authorization: Bearer read-placeholder' ]] ;;
  *'https://admin.googleapis.com/admin/directory/v1/users?domain=example.com'*) [[ "$header" == 'Authorization: Bearer admin-placeholder' ]] ;;
  *) echo 'unexpected request' >&2; exit 3 ;;
esac
printf 200
CURL
chmod +x "$work/consumer-bin/secret-run" "$work/consumer-bin/curl"
status=0
output="$(env BASH_ENV="$work/lcsnames-builders.sh" PROBE_DIR="$work" \
  AIRLOCK_GWS_READ_SECRET=GWS_EXAMPLE_VIEW_2 AIRLOCK_GWS_ADMIN_SECRET=GWS_EXAMPLE_ADMIN_2 \
  AIRLOCK_GWS_TIER_KEY="operator.with space's [key]" bash "$script_dir/airlock-check.sh" k7 2>&1)" || status=$?
label='operator inputs build matching rules, store names and valid consumer commands'; check test "$status" -eq 0
label='the selected tier and both logins reach the real generated commands'
check test "$output" = $'gws_tier=secret-run GWS_EXAMPLE_VIEW_2\ngws=200\nadmin=200'

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
