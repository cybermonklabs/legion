#!/usr/bin/env bash
# Exercises k7's required operator inputs before it creates a work directory or starts a stack.
# A stand-in mktemp stops the accepted run at that boundary, without network calls or builds.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/airlock-k7-inputs.XXXXXX")"
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
inputs=(AIRLOCK_GCLOUD_SECRET AIRLOCK_GWS_READ_SECRET AIRLOCK_GWS_ADMIN_SECRET AIRLOCK_GWS_TIER_KEY)
values=(GCLOUD_OPERATOR GWS_OPERATOR_READ_OAUTH GWS_OPERATOR_ADMIN_OAUTH operator)
unset_inputs=()
for input in "${inputs[@]}"; do unset_inputs+=(-u "$input"); done
run_check() {
  status=0
  output="$(env "${unset_inputs[@]}" PATH="$work/bin:$PATH" "$@" bash "$script_dir/airlock-check.sh" "$check_id" 2>&1)" || status=$?
}
check() {
  if "$@"; then
    printf 'PASS: %s\n' "$label"
  else
    printf 'FAIL: %s (exit %s): %s\n' "$label" "$status" "$output"
    failed=$((failed + 1))
  fi
}

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
  # The first three name broker secrets, which are of policy.NameToSlug's form or no name at all.
  if [[ "${inputs[$i]}" != AIRLOCK_GWS_TIER_KEY ]]; then
    for bad in 'gws_operator_read' 'GWS__READ' '_GWS' 'GWS-READ' 'ÉGWS'; do
      run_check "${supplied[@]}" "${inputs[$i]}=$bad"
      label="k7 refuses ${inputs[$i]}=$bad, no broker name"; check test "$status" -eq 2
      label="that refusal names ${inputs[$i]}"; check test "${output#*"${inputs[$i]}=$bad is no broker name"}" != "$output"
    done
  fi
done
all=()
for i in "${!inputs[@]}"; do all+=("${inputs[$i]}=${values[$i]}"); done
run_check "${all[@]}"
label='all four inputs pass preflight and reach setup'; check test "$status" -eq 93
label='the accepted run reaches the work directory boundary'; check test "${output#*'airlock-input-test: reached work directory creation'}" != "$output"
check_id=k6 run_check
label='other checks do not require the k7 inputs'; check test "$status" -eq 93

# Intercept the real driver just before check_k7 starts its stack. Exercise its fixture builders
# and the generated gcloud and Google Workspace commands, without fetching consumers or calling a
# service.
cat >"$work/k7-builders.sh" <<'HOOK'
probe_builders() {
  set -e
  add_fake_secret() { printf '%s\n' "$1" >>"$PROBE_DIR/k7-names"; }
  : >"$PROBE_DIR/k7-names"
  AIRLOCK_RULES_FILE="$PROBE_DIR/k7-rules.json"
  printf '{"secrets":[]}\n' >"$AIRLOCK_RULES_FILE"
  STACK_SECRETS_PREFIX=k7-fixture-
  k7_names
  jq -e --arg gcloud "$AIRLOCK_GCLOUD_SECRET" --arg read "$AIRLOCK_GWS_READ_SECRET" --arg admin "$AIRLOCK_GWS_ADMIN_SECRET" \
    'any(.secrets[]; .name == $gcloud and .hosts == ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudkms.googleapis.com"])
     and any(.secrets[]; .name == $read and .hosts == ["www.googleapis.com", "oauth2.googleapis.com"])
     and any(.secrets[]; .name == $admin and .hosts == ["admin.googleapis.com"])' "$WORK_DIR/k7/rules.json" >/dev/null
  # Each login's fake secret is stored under policy.NameToSlug's slug of the operator's name.
  for slug in gcloud-example-login gws-example-view-2 gws-example-admin-2; do
    grep -qFx "k7-fixture-$slug" "$PROBE_DIR/k7-names"
  done
  K7_DOTFILES="$PROBE_DIR/k7-consumer"
  mkdir -p "$K7_DOTFILES/gws" "$K7_DOTFILES/scripts" "$WORK_DIR/k7/pulumi"
  jq -n --arg key "$AIRLOCK_GWS_TIER_KEY" \
    '{($key):{tiers:{read:{command:["secret-run","GWS_EXAMPLE_VIEW_2"]}}}}' >"$K7_DOTFILES/gws/accounts.json"
  # The consumer's gcloud-broker reads its login under the operator's name for it.
  cat >"$K7_DOTFILES/scripts/gcloud-broker" <<'BROKER'
#!/usr/bin/env bash
[[ "$GCLOUD_BROKER_INNER" == 1 ]] && printf 'ERROR: %s\n' "${GCLOUD_EXAMPLE_LOGIN:-unset}"
BROKER
  K7_GH_DIR=/unused
  AIRLOCK_S_URL=https://unused.example
  AIRLOCK_BOTO3_PYTHON=python3
  STANDS_BIN=/unused
  k7_steps
  env -u BASH_ENV bash -n "$WORK_DIR/k7/steps.sh"
  # Select commands from the actual generated script, not copies of their implementation.
  cat >"$PROBE_DIR/k7-steps.sh" <<'STEPS'
set -euo pipefail
say() { printf '%s\n' "$1"; }
STEPS
  selected=0
  while IFS= read -r line; do
    case "$line" in
      gcloud_secret=* | gws_read_secret=* | gws_admin_secret=* | gws_tier_key=* | 'say "kms='* | 'say "gcloud='* | 'say "gws_tier='* | 'say "gws='* | 'say "admin='*)
        printf '%s\n' "$line" >>"$PROBE_DIR/k7-steps.sh"
        selected=$((selected + 1))
        ;;
    esac
  done <"$WORK_DIR/k7/steps.sh"
  [[ "$selected" == 9 ]]
  env -u BASH_ENV PATH="$PROBE_DIR/consumer-bin:$PATH" \
    GCLOUD_EXAMPLE_LOGIN=gcloud-placeholder GWS_EXAMPLE_VIEW_2=read-placeholder GWS_EXAMPLE_ADMIN_2=admin-placeholder \
    bash "$PROBE_DIR/k7-steps.sh"
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
# Pulumi's gcpkms provider is given the gcloud login as its access token.
cat >"$work/consumer-bin/pulumi" <<'PULUMI'
#!/usr/bin/env bash
[[ "$1 $2" == 'stack init' ]] && printf 'error: token %s\n' "${GOOGLE_OAUTH_ACCESS_TOKEN:-unset}"
PULUMI
chmod +x "$work/consumer-bin/secret-run" "$work/consumer-bin/curl" "$work/consumer-bin/pulumi"
status=0
output="$(env BASH_ENV="$work/k7-builders.sh" PROBE_DIR="$work" \
  AIRLOCK_GCLOUD_SECRET=GCLOUD_EXAMPLE_LOGIN \
  AIRLOCK_GWS_READ_SECRET=GWS_EXAMPLE_VIEW_2 AIRLOCK_GWS_ADMIN_SECRET=GWS_EXAMPLE_ADMIN_2 \
  AIRLOCK_GWS_TIER_KEY="operator.with space's [key]" bash "$script_dir/airlock-check.sh" k7 2>&1)" || status=$?
label='operator inputs build matching rules, store names and valid consumer commands'; check test "$status" -eq 0
label='the gcloud login, the selected tier and both Google Workspace logins reach the real generated commands'
check test "$output" = $'kms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=secret-run GWS_EXAMPLE_VIEW_2\ngws=200\nadmin=200'

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
