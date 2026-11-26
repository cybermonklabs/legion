#!/usr/bin/env bash
# Exercises k7's required operator inputs before it creates a work directory or starts a stack.
# A stand-in mktemp stops the accepted run at that boundary, without network calls or builds.
# Then, under fakes, k7's fixture and consumer boundary (airlock/k7-consumers.sh), and k8a's
# broker start, k6's az wrapper and the run's result line, each read out of airlock-check.sh.
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

# --- The result, read out of airlock-check.sh and run on its own: a run that asserted nothing
# proved nothing, so report says UNKNOWN and fails; one property of two failing fails by count. Its
# pass, fail, judge and report run in a subshell, apart from this test's own check.
airlock_report() { # <judge status>...: report's exit status and its last line
  (
    for name in pass fail judge report; do
      definition="$(sed -n "/^${name}() {/,/^}/p" "$script_dir/airlock-check.sh")"
      [[ -n "$definition" ]] || { echo "airlock-check.sh defines no ${name}()"; exit 0; }
      eval "$definition"
    done
    # shellcheck disable=SC2034 # report reads each
    CHECK_ID=test EXPECTED=() OBSERVED=() CONTROL=() VERDICTS=() PASSED=0 TOTAL=0
    for verdict in "$@"; do judge "$verdict" "a property" "seen"; done
    verdict=0
    report >"$work/report.out" || verdict=$?
    echo "$verdict:$(tail -n1 "$work/report.out")"
  )
}
status=0
output="$(airlock_report)"
label="airlock-check.sh's report says UNKNOWN and fails when the run judged no property"
check test "$output" = "1:check test: UNKNOWN (no property was evaluated)"
output="$(airlock_report 0 1)"
label="airlock-check.sh's report fails a run with one property of two failing"
check test "$output" = "1:check test: failed (1 of 2)"
output="$(airlock_report 0 0)"
label="airlock-check.sh's report passes a run whose every property passed"
check test "$output" = "0:check test: passed"

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
mkdir -p "$consumer/gws" "$work/k7/pulumi"
jq -n --arg key "$AIRLOCK_GWS_TIER_KEY" \
  '{($key):{tiers:{read:{command:["agent-secrets","GWS_EXAMPLE_VIEW_2","--reason","gws operator read","--"]}}}}' >"$consumer/gws/accounts.json"
mkdir -p "$work/consumer-bin"
# The fake gcloud answers as Google refuses a token, naming the token it was handed, when the
# steps set it up as an admin gcloud command's grant does: a 0600 token file in its own
# CLOUDSDK_CONFIG, prompts and file logging off, and neither the login's variable nor any account,
# configuration or impersonation of the caller's left. Otherwise it names what is wrong. It
# records the CLOUDSDK_CONFIG it was given, so the test can see the steps remove it.
cat >"$work/consumer-bin/gcloud" <<'GCLOUD'
#!/usr/bin/env bash
printf '%s\n' "${CLOUDSDK_CONFIG:-unset}" >>"${K7_TEST_GCLOUD_DIRS:?}"
wrong=""
[[ "$*" == 'projects list --format=json' ]] || wrong+=" arguments $*;"
[[ -d "${CLOUDSDK_CONFIG:-}" && "${CLOUDSDK_AUTH_ACCESS_TOKEN_FILE:-}" == "$CLOUDSDK_CONFIG/token" ]] || wrong+=" no token file in CLOUDSDK_CONFIG;"
[[ "$(stat -c %a "${CLOUDSDK_AUTH_ACCESS_TOKEN_FILE:-}" 2>/dev/null)" == 600 ]] || wrong+=" token file mode;"
[[ "${CLOUDSDK_CORE_DISABLE_PROMPTS:-}" == 1 && "${CLOUDSDK_CORE_DISABLE_FILE_LOGGING:-}" == true ]] || wrong+=" prompts or file logging on;"
for name in GCLOUD_EXAMPLE_LOGIN GCLOUD_ACCOUNT CLOUDSDK_ACTIVE_CONFIG_NAME CLOUDSDK_CORE_ACCOUNT CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT GOOGLE_APPLICATION_CREDENTIALS; do
  [[ -z "${!name+set}" ]] || wrong+=" $name is set;"
done
if [[ -n "$wrong" ]]; then printf 'ERROR: fake gcloud:%s\n' "$wrong"; exit 1; fi
printf 'ERROR: %s\n' "$(cat "$CLOUDSDK_AUTH_ACCESS_TOKEN_FILE")"
GCLOUD
# The fake client records the names and flags it is given, up to --, and runs the command after
# it in the environment the outer grant set up, as the real client runs it under its own grant.
cat >"$work/consumer-bin/agent-secrets" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
before=()
while (($#)) && [[ "$1" != -- ]]; do before+=("$1"); shift; done
[[ "${1:-}" == -- ]] || { echo "fake agent-secrets: no -- in the call" >&2; exit 2; }
shift
printf '%s\n' "${before[*]}" >>"${K7_TEST_CLIENT_CALLS:?}"
exec "$@"
RUNNER
# The real GitHub CLI, which the gh step puts first on PATH, says what it was given; the gh behind
# it plays Legion's wrapper, which refuses a call whose GH_TOKEN is set.
gh_dir="$work/real-gh"
mkdir -p "$gh_dir"
cat >"$gh_dir/gh" <<'GH'
#!/usr/bin/env bash
printf 'gh %s with GH_TOKEN %s and GITHUB_TOKEN %s\n' "$*" "${GH_TOKEN:-unset}" "${GITHUB_TOKEN:-unset}"
GH
cat >"$work/consumer-bin/gh" <<'GH'
#!/usr/bin/env bash
echo 'wrapper gh: refused, GH_TOKEN is set'
GH
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
chmod +x "$work/consumer-bin/gcloud" "$work/consumer-bin/agent-secrets" "$work/consumer-bin/curl" "$work/consumer-bin/pulumi" \
  "$work/consumer-bin/gh" "$gh_dir/gh"
# run_consumers [state]: writes the step script's preamble with the real k7_consumer_preamble, as
# k7_steps does, runs it, checks the setup it made, then runs the GitHub and Google consumers. A
# state file is sourced after the setup, so a control can leave the setup wrong and show the check
# refuses it. PREAMBLE_EDIT, a sed program, edits the preamble first, so a control can swap its
# mapping. The caller's own gcloud account, configuration and impersonation are set, as a box can
# set them, so the fake gcloud shows the steps drop each. The outer grant's values are the fake
# environment's; K7_TEST_CLIENT_CALLS collects what each nested agent-secrets call was given.
run_consumers() {
  status=0
  K7_DOTFILES="$consumer" K7_GH_DIR="$gh_dir" k7_consumer_preamble "$work/k7" >"$work/preamble.sh"
  if [[ -n "${PREAMBLE_EDIT:-}" ]]; then sed -i "$PREAMBLE_EDIT" "$work/preamble.sh"; fi
  # shellcheck disable=SC2016 # These variables expand in bash -c after the fake environment is set.
  output="$(env -u BASH_ENV PATH="$work/consumer-bin:$PATH" K7_TEST_GCLOUD_DIRS="$work/gcloud-dirs" K7_TEST_CLIENT_CALLS="$work/client-calls" \
    GITHUB_ADMIN_PAT=github-placeholder GCLOUD_EXAMPLE_LOGIN=gcloud-placeholder GWS_EXAMPLE_VIEW_2=read-placeholder GWS_EXAMPLE_ADMIN_2=admin-placeholder \
    GCLOUD_ACCOUNT=caller CLOUDSDK_ACTIVE_CONFIG_NAME=caller CLOUDSDK_CORE_ACCOUNT=caller@example.com \
    CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT=caller@example.iam.gserviceaccount.com GOOGLE_APPLICATION_CREDENTIALS=/caller.json \
    bash -c 'set -u
      source "$1"
      if [[ -n "${2:-}" ]]; then source "$2"; fi
      [[ "$DOTFILES_DIR" == "$K7_DOTFILES" ]] || { echo "setup: DOTFILES_DIR is $DOTFILES_DIR" >&2; exit 1; }
      [[ "$XDG_CONFIG_HOME" == "$K7_CONSUMER_WORK_DIR/xdg" && -d "$XDG_CONFIG_HOME" ]] || { echo "setup: XDG_CONFIG_HOME is $XDG_CONFIG_HOME" >&2; exit 1; }
      [[ "$PATH" == "$K7_DOTFILES/shims:"* ]] || { echo "setup: PATH does not start with the consumer shims" >&2; exit 1; }
      k7_consumer_github_step
      k7_consumer_google_steps
      cat "$K7_CONSUMER_WORK_DIR/out"' bash "$work/preamble.sh" "$@" 2>&1)" || status=$?
}
: >"$work/gcloud-dirs"
: >"$work/client-calls"
run_consumers
label='operator inputs build matching profiles, store names, prepare consumers and run valid commands'; check test "$status" -eq 0
label='the GitHub admin token, gcloud login, selected tier and both Google Workspace logins reach the real consumer boundary'
check test "$output" = $'gh=gh api user with GH_TOKEN github-placeholder and GITHUB_TOKEN github-placeholder\nkms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=agent-secrets GWS_EXAMPLE_VIEW_2 --reason gws operator read --\ngws=200\nadmin=200'
label='the GitHub admin and Google Workspace read calls each ask agent-secrets for their one name, with their reason'
output="$(cat "$work/client-calls")"
check test "$output" = $'GITHUB_ADMIN_PAT --reason k7: one gh api call with a fake admin token\nGWS_EXAMPLE_VIEW_2 --reason gws operator read'
label='the gcloud steps remove the CLOUDSDK_CONFIG they made once gcloud ends'
output="CLOUDSDK_CONFIG directories the fake gcloud was given: $(tr '\n' ' ' <"$work/gcloud-dirs")"
removed=false
if [[ "$(wc -l <"$work/gcloud-dirs")" -eq 1 ]]; then
  read -r gcloud_dir <"$work/gcloud-dirs"
  [[ "$gcloud_dir" == /* && ! -e "$gcloud_dir" ]] && removed=true
fi
check "$removed"

# Control: the preamble with the read and administrator logins swapped. The read call is then sent
# the administrator's login and the administrator call the read login, which the fake curl refuses.
PREAMBLE_EDIT='s/^K7_CONSUMER_GWS_READ_SECRET=/K7_SWAP=/; s/^K7_CONSUMER_GWS_ADMIN_SECRET=/K7_CONSUMER_GWS_READ_SECRET=/; s/^K7_SWAP=/K7_CONSUMER_GWS_ADMIN_SECRET=/' run_consumers
label='a preamble that swaps the read and administrator logins fails both Google Workspace calls'
check test "$output" = $'gh=gh api user with GH_TOKEN github-placeholder and GITHUB_TOKEN github-placeholder\nkms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=agent-secrets GWS_EXAMPLE_VIEW_2 --reason gws operator read --\ngws=\nadmin='

# Control: with no real GitHub CLI directory the gh step reaches the wrapper, which refuses it.
gh_dir='' run_consumers
label='a gh step with no real GitHub CLI first on PATH reaches the wrapper'
check test "${output%%$'\n'*}" = 'gh=wrapper gh: refused, GH_TOKEN is set'

# The Google Workspace step reads the tier command only to report it: a read tier whose command
# names another client (here `secrets`) changes the reported tier and nothing of the call.
jq -n --arg key "$AIRLOCK_GWS_TIER_KEY" \
  '{($key):{tiers:{read:{command:["secrets","GWS_EXAMPLE_VIEW_2","--"]}}}}' >"$consumer/gws/accounts.json"
run_consumers
label='a read tier command that names another client changes only the reported tier'
check test "$output" = $'gh=gh api user with GH_TOKEN github-placeholder and GITHUB_TOKEN github-placeholder\nkms=error: token gcloud-placeholder\ngcloud=ERROR: gcloud-placeholder\ngws_tier=secrets GWS_EXAMPLE_VIEW_2 --\ngws=200\nadmin=200'

# Controls: each wrong setup state must stop the consumers and name the variable that is wrong.
cat >"$work/wrong-dotfiles.sh" <<'STATE'
export DOTFILES_DIR="$K7_CONSUMER_WORK_DIR/wrong-dotfiles"
STATE
cat >"$work/wrong-xdg.sh" <<'STATE'
export XDG_CONFIG_HOME="$K7_CONSUMER_WORK_DIR/wrong-xdg"
STATE
cat >"$work/wrong-path.sh" <<'STATE'
export PATH="/usr/bin:$PATH"
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
# with a config directory that holds no cloud yet, as k6's steps make it. It registers the stand-in
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

# --- k6's login steps, read out of airlock-check.sh and run by sh as each k6 run runs them under
# its grant, with a fake az first on PATH that records its login and the state it was handed. Their
# TMPDIR is a directory of the test's own, so what they leave there shows: nothing, after a login,
# after a value with no federated token, which never reaches az, and after a TERM.
login_dir="$work/az-login"
mkdir -p "$login_dir/bin" "$login_dir/tmp"
cat >"$login_dir/bin/az" <<'EOF'
#!/usr/bin/env bash
dir="${AZURE_CONFIG_DIR%/config}"
printf 'az %s; config %s; token %s, mode %s; login variable %s\n' "$*" "${AZURE_CONFIG_DIR:-unset}" \
  "$(cat "$dir/token" 2>/dev/null)" "$(stat -c %a "$dir/token" 2>/dev/null)" "${AIRLOCK_AZURE_LOGIN+set}" >>"${K6_TEST_AZ_CALLS:?}"
EOF
chmod +x "$login_dir/bin/az"
definition="$(sed -n '/^k6_azure_login_steps() {/,/^}/p' "$script_dir/airlock-check.sh")"
[[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no k6_azure_login_steps()\n'; exit 1; }
eval "$definition"
# run_login <azure-login value> <command...>: runs the steps as k6_azure does, the command after them.
run_login() {
  local value="$1"
  shift
  status=0
  : >"$login_dir/calls"
  output="$(PATH="$login_dir/bin:$PATH" TMPDIR="$login_dir/tmp" K6_TEST_AZ_CALLS="$login_dir/calls" AIRLOCK_AZURE_LOGIN="$value" \
    sh -c "$(k6_azure_login_steps)" sh "$@" 2>&1)" || status=$?
}
login='{"tenant_id":"tenant-1","client_id":"client-1","federated_token":"federated-placeholder","expires_at":"2026-11-24T06:00:00Z"}'
# shellcheck disable=SC2016 # expands in the command the steps run
run_login "$login" sh -c 'printf "the command ran with config %s; login variable %s\n" "$AZURE_CONFIG_DIR" "${AIRLOCK_AZURE_LOGIN+set}"; exit 7'
label="k6's login steps keep the command's exit status"; check test "$status" -eq 7
command_output="$output"
output="$(cat "$login_dir/calls")"
config="${output#*; config }"
config="${config%%;*}"
label="k6's login steps log az in as the service principal from a 0600 token file beside a throwaway AZURE_CONFIG_DIR, with the login's variable gone"
check test "$output" = "az login --service-principal --username client-1 --tenant tenant-1 --federated-token @${config%/config}/token --allow-no-subscriptions --output none; config $config; token federated-placeholder, mode 600; login variable "
label="that AZURE_CONFIG_DIR is a new directory under TMPDIR"
check test "${config#"$login_dir/tmp/"}" != "$config"
label="the command runs in the same AZURE_CONFIG_DIR, without the login's variable"
output="$command_output"
check test "$output" = "the command ran with config $config; login variable "
label="k6's login steps remove their directory when the command ends"
output="$(ls -A "$login_dir/tmp")"
check test -z "$output"
# Control: a value with no federated token stops the steps before az and before the command.
run_login '{"tenant_id":"tenant-1","client_id":"client-1"}' sh -c 'echo the command ran'
label="a login value with no federated token stops the steps before az and the command, and leaves nothing"
output="status $status; az calls: $(cat "$login_dir/calls"); output: $output; left: $(ls -A "$login_dir/tmp")"
passed=false
[[ "$status" -ne 0 && ! -s "$login_dir/calls" && "$output" != *'the command ran'* && -z "$(ls -A "$login_dir/tmp")" ]] && passed=true
check "$passed"
# A TERM to the steps' shell while the command runs: the shell ends with 143 once the command has
# ended, and its EXIT trap still removes the directory. Without the TERM trap the shell would die
# at once and leave the token file behind.
started="$work/az-login-started"
PATH="$login_dir/bin:$PATH" TMPDIR="$login_dir/tmp" K6_TEST_AZ_CALLS="$login_dir/calls" AIRLOCK_AZURE_LOGIN="$login" \
  sh -c "$(k6_azure_login_steps)" sh sh -c ': >"$0"; sleep 1' "$started" &
steps=$!
for _ in $(seq 1 100); do [[ -e "$started" ]] && break; sleep 0.05; done
kill -TERM "$steps"
status=0
wait "$steps" || status=$?
label="a TERM to k6's login steps ends them with 143 and still removes their directory"
output="status $status; left: $(ls -A "$login_dir/tmp")"
check test "$output" = "status 143; left: "

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
