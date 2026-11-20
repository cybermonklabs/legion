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

# The fixture and Google-consumer boundary is called directly below. It does not parse text from
# airlock-check.sh, so splitting or changing that caller cannot change this fixture test.
boundary="$script_dir/airlock/k7-consumers.sh"
run_boundary() {
  status=0
# shellcheck disable=SC2016 # "$1" expands in bash -c, where it is the boundary path.
  output="$(env "${unset_inputs[@]}" bash -c 'source "$1"; k7_consumer_require_inputs' bash "$boundary" 2>&1)" || status=$?
}
run_boundary
label='k7 fixture boundary refuses all missing inputs'; check test "$status" -eq 2
for input in "${inputs[@]}"; do
  label="the fixture-boundary refusal names $input"; check test "${output#*"$input"}" != "$output"
done

# The external consumers are faked; the fixture builder and the consumer boundary stay real.
# shellcheck source=packages/envoy/scripts/airlock/k7-consumers.sh
source "$script_dir/airlock/k7-consumers.sh"
add_fake_secret() {
  printf '%s\n' "$1" >>"$work/k7-names"
  case "$1" in
    k7-fixture-gcloud-example-login | k7-fixture-gws-example-view-2 | k7-fixture-gws-example-admin-2)
      printf '%s\n' "$3" >>"$work/k7-google-logins"
      ;;
  esac
}
: >"$work/k7-names"
: >"$work/k7-google-logins"
AIRLOCK_GCLOUD_SECRET=GCLOUD_EXAMPLE_LOGIN
AIRLOCK_GWS_READ_SECRET=GWS_EXAMPLE_VIEW_2
AIRLOCK_GWS_ADMIN_SECRET=GWS_EXAMPLE_ADMIN_2
AIRLOCK_GWS_TIER_KEY="operator.with space's [key]"
AIRLOCK_RULES_FILE="$work/k7-rules.json"
printf '{"secrets":[]}\n' >"$AIRLOCK_RULES_FILE"
STACK_SECRETS_PREFIX=k7-fixture-
K7_CONSUMER_WORK_DIR="$work/k7"
k7_consumer_add_fixtures
jq -e --arg gcloud "$AIRLOCK_GCLOUD_SECRET" --arg read "$AIRLOCK_GWS_READ_SECRET" --arg admin "$AIRLOCK_GWS_ADMIN_SECRET" \
  'any(.secrets[]; .name == $gcloud and .hosts == ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudkms.googleapis.com"])
   and any(.secrets[]; .name == $read and .hosts == ["www.googleapis.com", "oauth2.googleapis.com"])
   and any(.secrets[]; .name == $admin and .hosts == ["admin.googleapis.com"])' "$work/k7/rules.json" >/dev/null
# Each login's fake secret is stored under policy.NameToSlug's slug of the operator's name.
for slug in gcloud-example-login gws-example-view-2 gws-example-admin-2; do
  grep -qFx "k7-fixture-$slug" "$work/k7-names"
done
jq -e -s 'length == 3 and all(.[]; type == "object" and .type == "authorized_user" and (.client_id | type == "string") and (.client_secret | type == "string") and (.refresh_token | type == "string"))' "$work/k7-google-logins" >/dev/null
consumer="$work/k7-consumer"
mkdir -p "$consumer/gws" "$consumer/scripts" "$work/k7/pulumi"
jq -n --arg key "$AIRLOCK_GWS_TIER_KEY" \
  '{($key):{tiers:{read:{command:["secret-run","GWS_EXAMPLE_VIEW_2"]}}}}' >"$consumer/gws/accounts.json"
# The consumer's gcloud-broker reads its login under the operator's name for it.
cat >"$consumer/scripts/gcloud-broker" <<'BROKER'
#!/usr/bin/env bash
[[ "$GCLOUD_BROKER_INNER" == 1 ]] && printf 'ERROR: %s\n' "${GCLOUD_EXAMPLE_LOGIN:-unset}"
BROKER
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
chmod +x "$consumer/scripts/gcloud-broker" "$work/consumer-bin/secret-run" "$work/consumer-bin/curl" "$work/consumer-bin/pulumi"
status=0
# shellcheck disable=SC2016 # These variables expand in bash -c after the fake environment is set.
output="$(env -u BASH_ENV PATH="$work/consumer-bin:$PATH" \
  GCLOUD_EXAMPLE_LOGIN=gcloud-placeholder GWS_EXAMPLE_VIEW_2=read-placeholder GWS_EXAMPLE_ADMIN_2=admin-placeholder \
  K7_CONSUMER_WORK_DIR="$work/k7" K7_DOTFILES="$consumer" \
  K7_CONSUMER_GCLOUD_SECRET="$AIRLOCK_GCLOUD_SECRET" K7_CONSUMER_GWS_READ_SECRET="$AIRLOCK_GWS_READ_SECRET" \
  K7_CONSUMER_GWS_ADMIN_SECRET="$AIRLOCK_GWS_ADMIN_SECRET" K7_CONSUMER_TIER_KEY="$AIRLOCK_GWS_TIER_KEY" \
  bash -c 'source "$1"
    k7_consumer_prepare_steps
    [[ "$DOTFILES_DIR" == "$K7_DOTFILES" ]]
    [[ "$XDG_CONFIG_HOME" == "$K7_CONSUMER_WORK_DIR/xdg" && -d "$XDG_CONFIG_HOME" ]]
    [[ "$PATH" == "$K7_DOTFILES/scripts:$K7_DOTFILES/shims:"* ]]
    k7_consumer_google_steps
    cat "$K7_CONSUMER_WORK_DIR/out"' bash "$boundary" 2>&1)" || status=$?
label='operator inputs build matching rules, store names, prepare consumers and run valid commands'; check test "$status" -eq 0
label='the gcloud login, selected tier and both Google Workspace logins reach the real consumer boundary'
check test "$output" = $'kms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=secret-run GWS_EXAMPLE_VIEW_2\ngws=200\nadmin=200'

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
