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
    for bad in 'gws_operator_read' 'GWS__READ' '_GWS' 'GWS-READ' 'ÉGWS' 'ghp_pasted-in-by-mistake'; do
      run_check "${supplied[@]}" "${inputs[$i]}=$bad"
      label="k7 refuses ${inputs[$i]}=$bad, no broker name"; check test "$status" -eq 2
      label="that refusal names ${inputs[$i]}"; check test "${output#*"${inputs[$i]} is no broker name"}" != "$output"
      # A value such as _GWS is also part of the input's own name, so the name is taken out of the
      # output first, and the value must then appear nowhere in what is left.
      without_name="${output//"${inputs[$i]}"/}"
      label="that refusal does not repeat the value $bad"; check test "${without_name#*"$bad"}" = "$without_name"
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
AIRLOCK_PROFILES_FILE="$work/k7-profiles.json"
printf '{"profiles":[]}\n' >"$AIRLOCK_PROFILES_FILE"
STACK_SECRETS_PREFIX=k7-fixture-
K7_CONSUMER_WORK_DIR="$work/k7"
k7_consumer_add_fixtures
jq -e --arg gcloud "$AIRLOCK_GCLOUD_SECRET" --arg read "$AIRLOCK_GWS_READ_SECRET" --arg admin "$AIRLOCK_GWS_ADMIN_SECRET" \
  'any(.profiles[]; .name == $gcloud and .proxy.hosts == ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudkms.googleapis.com"])
   and any(.profiles[]; .name == $read and .proxy.hosts == ["www.googleapis.com", "oauth2.googleapis.com"])
   and any(.profiles[]; .name == $admin and .proxy.hosts == ["admin.googleapis.com"])' "$work/k7/profiles.json" >/dev/null
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
# run_consumers [state]: writes the step script's preamble with the real k7_consumer_preamble, as
# k7_steps does, runs it, checks the setup it made, then runs the Google consumers. A state file
# is sourced after the setup, so a control can leave the setup wrong and show the check refuses
# it. PREAMBLE_EDIT, a sed program, edits the preamble first, so a control can swap its mapping.
run_consumers() {
  status=0
  K7_DOTFILES="$consumer" k7_consumer_preamble "$work/k7" >"$work/preamble.sh"
  if [[ -n "${PREAMBLE_EDIT:-}" ]]; then sed -i "$PREAMBLE_EDIT" "$work/preamble.sh"; fi
  # shellcheck disable=SC2016 # These variables expand in bash -c after the fake environment is set.
  output="$(env -u BASH_ENV PATH="$work/consumer-bin:$PATH" \
    GCLOUD_EXAMPLE_LOGIN=gcloud-placeholder GWS_EXAMPLE_VIEW_2=read-placeholder GWS_EXAMPLE_ADMIN_2=admin-placeholder \
    bash -c 'set -u
      source "$1"
      if [[ -n "${2:-}" ]]; then source "$2"; fi
      [[ "$DOTFILES_DIR" == "$K7_DOTFILES" ]] || { echo "setup: DOTFILES_DIR is $DOTFILES_DIR" >&2; exit 1; }
      [[ "$XDG_CONFIG_HOME" == "$K7_CONSUMER_WORK_DIR/xdg" && -d "$XDG_CONFIG_HOME" ]] || { echo "setup: XDG_CONFIG_HOME is $XDG_CONFIG_HOME" >&2; exit 1; }
      [[ "$PATH" == "$K7_DOTFILES/scripts:$K7_DOTFILES/shims:"* ]] || { echo "setup: PATH does not start with the consumer scripts and shims" >&2; exit 1; }
      k7_consumer_google_steps
      cat "$K7_CONSUMER_WORK_DIR/out"' bash "$work/preamble.sh" "$@" 2>&1)" || status=$?
}
run_consumers
label='operator inputs build matching profiles, store names, prepare consumers and run valid commands'; check test "$status" -eq 0
label='the gcloud login, selected tier and both Google Workspace logins reach the real consumer boundary'
check test "$output" = $'kms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=secret-run GWS_EXAMPLE_VIEW_2\ngws=200\nadmin=200'

# Control: the preamble with the read and administrator logins swapped. The read call is then sent
# the administrator's login and the administrator call the read login, which the fake curl refuses.
PREAMBLE_EDIT='s/^K7_CONSUMER_GWS_READ_SECRET=/K7_SWAP=/; s/^K7_CONSUMER_GWS_ADMIN_SECRET=/K7_CONSUMER_GWS_READ_SECRET=/; s/^K7_SWAP=/K7_CONSUMER_GWS_ADMIN_SECRET=/' run_consumers
label='a preamble that swaps the read and administrator logins fails both Google Workspace calls'
check test "$output" = $'kms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=secret-run GWS_EXAMPLE_VIEW_2\ngws=\nadmin='

# Controls: each wrong setup state must stop the consumers and name the variable that is wrong.
cat >"$work/wrong-dotfiles.sh" <<'STATE'
export DOTFILES_DIR="$K7_CONSUMER_WORK_DIR/wrong-dotfiles"
STATE
cat >"$work/wrong-xdg.sh" <<'STATE'
export XDG_CONFIG_HOME="$K7_CONSUMER_WORK_DIR/wrong-xdg"
STATE
cat >"$work/wrong-path.sh" <<'STATE'
export PATH="$K7_DOTFILES/shims:$PATH"
STATE
for wrong in DOTFILES_DIR:wrong-dotfiles XDG_CONFIG_HOME:wrong-xdg PATH:wrong-path; do
  run_consumers "$work/${wrong#*:}.sh"
  label="a wrong ${wrong%%:*} stops the consumers"; check test "$status" -ne 0
  label="that refusal names ${wrong%%:*}"; check test "${output#*"setup: ${wrong%%:*}"}" != "$output"
done

# --- k8a's broker start, read out of airlock-check.sh and run on its own: broker_env copies the
# environment of a stand-in for the stack's broker, whose BROKER_DATABASE_URL holds a password,
# and k8a_start_broker starts a stand-in broker on it under `timeout`. Every argument list of that
# start (a stand-in env first on PATH records its own, as env -i would have carried the copy) and
# the stand-in broker's own arguments hold no password, and the stand-in broker's environment is
# exactly the copy and the overrides, no variable of this test's own among them.
start_dir="$work/broker-start"
mkdir -p "$start_dir/bin"
real_env="$(command -v env)"
cat >"$start_dir/bin/env" <<EOF
#!/usr/bin/env bash
printf 'env %s\n' "\$*" >>"$start_dir/argv"
exec ${real_env} "\$@"
EOF
cat >"$start_dir/broker" <<EOF
#!/usr/bin/env bash
tr '\0' ' ' </proc/\$\$/cmdline >"$start_dir/broker-cmdline"
tr '\0' '\n' </proc/\$\$/environ | sed 's/=.*//' | sort >"$start_dir/broker-environ"
echo "fake broker: fatal: \$BROKER_PROFILES_FILE is refused" >&2
exit 1
EOF
chmod +x "$start_dir/bin/env" "$start_dir/broker"
# The stand-in's environment is exactly these three; the real env starts it, not the recording one.
"$real_env" -i PATH="$start_dir/bin:/usr/bin:/bin" BROKER_DATABASE_URL='postgres://broker:not-a-secret@127.0.0.1:5999/db' \
  STAND_IN=1 sleep 60 &
stand_in=$!
# broker_env reads the stand-in's environment once it is sleep's, not env's before it execs sleep;
# a stand-in that is not sleep by the end of the wait ends the test, naming what it is.
stand_in_cmdline() { tr '\0' ' ' <"/proc/$stand_in/cmdline" 2>/dev/null; }
for _ in $(seq 1 50); do
  [[ "$(stand_in_cmdline)" == sleep* ]] && break
  sleep 0.1
done
[[ "$(stand_in_cmdline)" == sleep* ]] ||
  { printf 'FAIL: the stand-in broker is not sleep after 5 s: %s\n' "$(stand_in_cmdline)"; exit 1; }
eval "$(grep -m1 '^contains() {' "$script_dir/airlock-check.sh")"
# with_broker_env is lib/exec-env.sh's, which airlock-check.sh sources, as this test does.
# shellcheck source-path=SCRIPTDIR source=lib/exec-env.sh
. "$script_dir/lib/exec-env.sh"
for function in broker_env k8a_start_broker; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/airlock-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done
# shellcheck disable=SC2034 # WORK_DIR and BROKER_BIN are read by k8a_start_broker, eval'd above
STACK_LOG="$start_dir/stack.log" WORK_DIR="$start_dir" BROKER_BIN="$start_dir/broker"
printf '  broker 1: api 127.0.0.1:1, proxy 127.0.0.1:2, pid %s, log x\n' "$stand_in" >"$STACK_LOG"
status=0
PATH="$start_dir/bin:$PATH" k8a_start_broker "$start_dir/profiles.json" label || status=$?
kill "$stand_in" 2>/dev/null || true
output="$(cat "$start_dir/argv" "$start_dir/broker-cmdline" 2>/dev/null || true)"
label="k8a's broker start puts the stack broker's password in no process's arguments"
passed=false
[[ "$status" -eq 0 && -s "$start_dir/broker-cmdline" ]] && ! grep -q not-a-secret "$start_dir/broker-cmdline" &&
  ! grep -qs not-a-secret "$start_dir/argv" && passed=true
check "$passed"
output="$(cat "$start_dir/broker-environ" 2>/dev/null || true)"
want_environ="$(printf '%s\n' BROKER_AIRLOCK_LISTEN_ADDR BROKER_DATABASE_URL BROKER_LISTEN_ADDR BROKER_METRICS_LISTEN_ADDR BROKER_PROFILES_FILE BROKER_PUBLIC_URL PATH STAND_IN | sort)"
label="k8a's broker start gives the broker exactly the stack broker's environment and its overrides"
check test "$output" = "$want_environ"

# --- k6's az wrapper, read out of airlock-check.sh and run on its own, on a box whose az on PATH
# is a version manager's shim: a script that takes its own directory out of PATH and execs the az
# it then finds there, as mise's does when it falls through. k6_azure_wrapper records the az on
# PATH, the shim, and writes the wrapper; the wrapper then runs first on PATH, as k6_azure puts it,
# with a config directory that holds no cloud yet, as az-broker's does. It registers the stand-in
# cloud, selects it and runs the command, each through the shim, and the verdict is the calls the
# az behind the shim recorded: those three, in that order. A wrapper that ran the shim on its own
# PATH handed the shim back to the wrapper, and the two called each other, a new pair at each
# cloud register. So the shim refuses a fourth call in one chain of calls and says so: the row
# ends there rather than filling the machine. The controls show the setup could fail: the az the
# wrapper records is the shim, and the shim runs the az put where k6_azure puts the wrapper.
az_dir="$work/az-wrapper"
mkdir -p "$az_dir/shim" "$az_dir/real" "$az_dir/probe" "$az_dir/config"
cat >"$az_dir/real/az" <<EOF
#!/usr/bin/env bash
printf 'az %s\n' "\$*" >>"$az_dir/calls"
EOF
{
  printf '#!/usr/bin/env bash\nshim_dir=%q\n' "$az_dir/shim"
  cat <<'EOF'
calls=$((${AIRLOCK_TEST_AZ_SHIM_CALLS:-0} + 1))
if ((calls > 3)); then
  echo "az shim: call $calls in one chain of calls, each through the one before: a loop" >&2
  exit 97
fi
export AIRLOCK_TEST_AZ_SHIM_CALLS="$calls"
handed_path=""
IFS=: read -r -a path_dirs <<<"$PATH"
for dir in "${path_dirs[@]}"; do
  [[ "$dir" == "$shim_dir" ]] || handed_path="${handed_path:+${handed_path}:}${dir}"
done
export PATH="$handed_path"
exec az "$@"
EOF
} >"$az_dir/shim/az"
printf '#!/usr/bin/env bash\necho probe\n' >"$az_dir/probe/az"
chmod +x "$az_dir/real/az" "$az_dir/shim/az" "$az_dir/probe/az"
definition="$(sed -n '/^k6_azure_wrapper() {/,/^}/p' "$script_dir/airlock-check.sh")"
[[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no k6_azure_wrapper()\n'; exit 1; }
eval "$definition"
# shellcheck disable=SC2034 # the two URLs are read by k6_azure_wrapper, eval'd above
WORK_DIR="$az_dir/work" AIRLOCK_MICROSOFT_URL=https://login.microsoft.test AIRLOCK_AZURE_RESOURCE_URL=https://management.azure.test
box_path="$az_dir/shim:$az_dir/real:$PATH"
PATH="$box_path" k6_azure_wrapper
status=0
output="${REAL_AZ-}"
label="the az k6_azure_wrapper records on a box whose az is a shim is that shim"
check test "${REAL_AZ-}" = "$az_dir/shim/az"
output="$(PATH="$az_dir/probe:$box_path" "$az_dir/shim/az" version 2>&1)" || status=$?
label="the shim runs the az put first on PATH, where k6_azure puts the wrapper"
check test "$output" = probe
status=0
# The wrapper's directory before the PATH the wrapper recorded, as k6_azure runs it.
output="$(PATH="$WORK_DIR/k6/bin:${K6_AZ_PATH-}" AZURE_CONFIG_DIR="$az_dir/config" timeout 60 az group list -o json 2>&1)" || status=$?
output="${output:+$output; }the az behind the shim recorded: $(tr '\n' ';' 2>/dev/null <"$az_dir/calls" || echo nothing)"
label="k6's az wrapper, on a box whose az is a shim, registers the cloud, selects it and runs the command, each through the shim to the az behind it"
passed=false
[[ "$status" -eq 0 && "$(awk '{print $2, $3}' "$az_dir/calls" 2>/dev/null)" == $'cloud register\ncloud set\ngroup list' ]] && passed=true
check "$passed"

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
