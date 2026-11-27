#!/usr/bin/env bash
# Exercises k7's required operator inputs before it creates a work directory or starts a stack.
# A stand-in mktemp stops the accepted run at that boundary, without network calls or builds.
# Then, under fakes, k7's fixture and consumer boundary (airlock/k7-consumers.sh), and k8a's
# broker start, k6's az wrapper and the run's result line, each read out of airlock-check.sh.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/airlock-k7-inputs.XXXXXX")"
approve_stand=""
# One EXIT trap for the whole test: it stops the approval page's stand-in, where one was started,
# by its PID and waits for it, then removes the work directory.
trap '[[ -z "$approve_stand" ]] || { kill "$approve_stand" 2>/dev/null || true; wait "$approve_stand" 2>/dev/null || true; }; rm -rf "$work"' EXIT
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
    CHECK_ID=test EXPECTED=() OBSERVED=() CONTROL=() VERDICTS=() PASSED=0 TOTAL=0 NOT_JUDGED=0
    for verdict in "$@"; do judge "$verdict" "a property" "seen"; done
    verdict=0
    report >"$work/report.out" || verdict=$?
    echo "$verdict:$(tail -n1 "$work/report.out")"
  )
}
status=0
output="$(airlock_report)"
label="airlock-check.sh's report says UNKNOWN and fails when the run judged no property"
check test "$output" = "3:check test: UNKNOWN: no property was judged, so nothing passed"
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
  printf '%s %s\n' "$1" "$(jq -r '.proxy // "untagged"' <<<"$2")" >>"$work/k7-tags"
  case "$1" in
    k7-fixture-gcloud-example-login | k7-fixture-gws-example-view-2 | k7-fixture-gws-example-admin-2)
      printf '%s\n' "$3" >>"$work/k7-google-logins"
      ;;
  esac
}
: >"$work/k7-names"
: >"$work/k7-tags"
: >"$work/k7-google-logins"
AIRLOCK_GCLOUD_SECRET=GCLOUD_EXAMPLE_LOGIN
AIRLOCK_GWS_READ_SECRET=GWS_EXAMPLE_VIEW_2
AIRLOCK_GWS_ADMIN_SECRET=GWS_EXAMPLE_ADMIN_2
AIRLOCK_GWS_TIER_KEY="operator.with space's [key]"
AIRLOCK_PROFILES_FILE="$work/k7-profiles.json"
printf '{"kinds":[{"name":"stest","hosts":["s.airlock.test"],"ways":["swap"]}],"profiles":[]}\n' >"$AIRLOCK_PROFILES_FILE"
STACK_SECRETS_PREFIX=k7-fixture-
K7_CONSUMER_WORK_DIR="$work/k7"
k7_consumer_add_fixtures
# The fixtures are in the kinds form: the stack's own kind kept, one kind per consumer with the
# hosts it calls and the way its value takes, no profile and no proxy object, and each consumer's
# fake secret tagged with its own kind.
jq -e '(.kinds | map(.name)) == ["stest", "slack", "github", "tailscale", "gcloud", "gwsread", "gwsadmin"]
   and any(.kinds[]; .name == "gcloud" and .hosts == ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudkms.googleapis.com"] and .ways == ["mint"])
   and any(.kinds[]; .name == "gwsread" and .hosts == ["www.googleapis.com", "oauth2.googleapis.com"] and .ways == ["mint"])
   and any(.kinds[]; .name == "gwsadmin" and .hosts == ["admin.googleapis.com"] and .ways == ["mint"])
   and any(.kinds[]; .name == "github" and .hosts == ["api.github.com", "github.com"] and .ways == ["swap"])
   and .profiles == [] and ([.. | objects | select(has("proxy"))] | length) == 0' "$work/k7/profiles.json" >/dev/null
[[ "$(sort "$work/k7-tags")" == "$(printf '%s\n' 'k7-fixture-gcloud-example-login gcloud' 'k7-fixture-github-admin-pat github' \
  'k7-fixture-gws-example-admin-2 gwsadmin' 'k7-fixture-gws-example-view-2 gwsread' 'k7-fixture-slack-bot-token slack' \
  'k7-fixture-tailscale-api-key tailscale' | sort)" ]]
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

# --- k8's broker start, read out of airlock-check.sh and run on its own: broker_env copies the
# environment of a stand-in for the stack's broker, whose BROKER_DATABASE_URL holds a password,
# and k8_start_broker starts a stand-in broker on it under `timeout`. Every argument list of that
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
for function in broker_env k8_start_broker; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/airlock-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done
# shellcheck disable=SC2034 # WORK_DIR and BROKER_BIN are read by k8_start_broker, eval'd above
STACK_LOG="$start_dir/stack.log" WORK_DIR="$start_dir" BROKER_BIN="$start_dir/broker"
printf '  broker 1: api 127.0.0.1:1, proxy 127.0.0.1:2, pid %s, log x\n' "$stand_in" >"$STACK_LOG"
status=0
PATH="$start_dir/bin:$PATH" k8_start_broker "$start_dir/profiles.json" label || status=$?
kill "$stand_in" 2>/dev/null || true
output="$(cat "$start_dir/argv" "$start_dir/broker-cmdline" 2>/dev/null || true)"
label="k8's broker start puts the stack broker's password in no process's arguments"
passed=false
[[ "$status" -eq 0 && -s "$start_dir/broker-cmdline" ]] && ! grep -q not-a-secret "$start_dir/broker-cmdline" &&
  ! grep -qs not-a-secret "$start_dir/argv" && passed=true
check "$passed"
output="$(cat "$start_dir/broker-environ" 2>/dev/null || true)"
want_environ="$(printf '%s\n' BROKER_AIRLOCK_LISTEN_ADDR BROKER_DATABASE_URL BROKER_LISTEN_ADDR BROKER_METRICS_LISTEN_ADDR BROKER_PROFILES_FILE BROKER_PUBLIC_URL PATH STAND_IN | sort)"
label="k8's broker start gives the broker exactly the stack broker's environment and its overrides"
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

# --- approve_record, read out of airlock-check.sh and run on its own against a stand-in of the
# broker's approval page. The stand-in signs the approver in as the stack's issuer does (a
# redirect, a sign-in cookie, a session cookie), prints a form for two records with two printed
# fields and two profile_digest fields on one line, as the broker's record.html prints them, and
# holds the POST to the broker's own rules for them: every printed record must come back, or the
# page token's MAC is refused 403 (approval.pageMAC), and every fingerprint must come back, or the
# answer is 403 "Profile changed" (approval.profilesMoved); nothing is decided either way, else
# 303. The controls run the same helper with its profile_digest fields taken out, and with its
# printed fields taken out, each of which the stand-in must answer 403.
approve_dir="$work/approve"
mkdir -p "$approve_dir"
cat >"$approve_dir/stand.py" <<'PY'
import http.server, sys, urllib.parse
REC = "ab" * 32
PRINTED = [REC, "cd" * 32]
DIGESTS = [REC + " AIRLOCK_AZURE_LOGIN " + "1f" * 32, PRINTED[1] + " AIRLOCK_OTHER " + "2e" * 32]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, headers=(), body=b""):
        self.send_response(code)
        for k, v in headers: self.send_header(k, v)
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        base = "http://127.0.0.1:%d" % self.server.server_port
        cookie = self.headers.get("Cookie", "")
        if self.path == "/approve/" + REC and "__Host-broker-session=s" not in cookie:
            return self.send(302, [("Location", base + "/authorize"), ("Set-Cookie", "__Host-broker-signin=i; Secure")])
        if self.path == "/authorize":
            return self.send(302, [("Location", base + "/callback")])
        if self.path == "/callback" and "__Host-broker-signin=i" in cookie:
            return self.send(302, [("Location", base + "/approve/" + REC), ("Set-Cookie", "__Host-broker-session=s; Secure")])
        if self.path == "/approve/" + REC:
            fields = "".join('<input type="hidden" name="printed" value="%s">' % p for p in PRINTED)
            fields += "".join('<input type="hidden" name="profile_digest" value="%s">' % d for d in DIGESTS)
            fields += '<input type="hidden" name="token" value="tok"><input type="hidden" name="expires" value="9">'
            fields += '<input type="hidden" name="record" value="%s">' % REC
            return self.send(200, [("Content-Type", "text/html")], ("<form>" + fields + "</form>").encode())
        self.send(404)
    def do_POST(self):
        form = urllib.parse.parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode(), keep_blank_values=True)
        signed_in = ("__Host-broker-session=s" in self.headers.get("Cookie", "") and self.headers.get("Origin")
                     and form.get("decision") == ["approve"] and form.get("record") == [REC] and form.get("token") == ["tok"])
        if not signed_in or sorted(form.get("printed", [])) != sorted(PRINTED):
            return self.send(403, body=b"Page token invalid")
        if sorted(form.get("profile_digest", [])) != sorted(DIGESTS):
            return self.send(403, body=b"Profile changed")
        self.send(303)
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
python3 "$approve_dir/stand.py" >"$approve_dir/port" &
approve_stand=$!
for _ in $(seq 1 50); do [[ -s "$approve_dir/port" ]] && break; sleep 0.1; done
definition="$(sed -n '/^approve_record() {/,/^}/p' "$script_dir/airlock-check.sh")"
[[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no approve_record()\n'; exit 1; }
approve_once() { # <the helper's definition> <work dir>
  eval "$1"
  WORK_DIR="$2" AGENT_SECRETS_URL="http://127.0.0.1:$(cat "$approve_dir/port")" approve_record "$(printf 'ab%.0s' $(seq 1 32))"
  cat "$2/approve/decisions"
}
status=0
output="$(approve_once "$definition" "$approve_dir/with" 2>&1)" || status=$?
label="approve_record posts back each printed record and each profile_digest the page printed, and the approval is decided (303)"
check test "$output" = 303
# shellcheck disable=SC2016 # the pattern is the helper's own text, with its "$value" unexpanded
without="${definition//'digests+=(--data-urlencode "profile_digest=$value")'/:}"
[[ "$without" != "$definition" ]] || { printf 'FAIL: the control could not take profile_digest out of approve_record\n'; exit 1; }
output="$(approve_once "$without" "$approve_dir/without" 2>&1)" || status=$?
label="control: the same helper without its profile_digest fields is refused 403 Profile changed"
check test "$output" = 403
# shellcheck disable=SC2016 # the pattern is the helper's own text, with its "$value" unexpanded
unprinted="${definition//'printed+=(--data-urlencode "printed=$value")'/:}"
[[ "$unprinted" != "$definition" ]] || { printf 'FAIL: the control could not take printed out of approve_record\n'; exit 1; }
output="$(approve_once "$unprinted" "$approve_dir/unprinted" 2>&1)" || status=$?
label="control: the same helper without its printed fields is refused 403, its page token's records unnamed"
check test "$output" = 403
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

# --- This file's own EXIT trap, read out of it and run in a shell of its own under set -euo
# pipefail, as this test runs: with a stand-in process to stop, still running or already exited,
# the shell must end with the status its last command left - 0 after a pass, 1 after a failure -
# must have stopped the stand-in, and must remove its work directory either way. A trap whose wait
# on the killed stand-in ends the shell under set -e answers 143 and leaves the directory, which is
# how CI read a test that passed as a failure; one whose kill of a stand-in that already exited
# ends it answers 1; and one that stops nothing leaves the stand-in running after the shell ends.
trap_line="$(grep -m1 '^trap .*approve_stand.* EXIT$' "${BASH_SOURCE[0]}")"
[[ -n "$trap_line" ]] || { printf 'FAIL: this file has no EXIT trap that stops approve_stand\n'; exit 1; }
for stand in running exited; do
  for want in 0 1; do
    trap_work="$(mktemp -d "$work/trap.XXXXXX")/work"
    mkdir -p "$trap_work"
    status=0
    # The child writes its stand-in's PID beside the work directory, which the trap does not
    # remove, so the test can ask after the child has exited whether that process still runs.
    # shellcheck disable=SC2016 # the script is the child shell's, expanded there
    bash -c 'set -euo pipefail; work="$1"; sleep 60 & approve_stand=$!; printf "%s\n" "$approve_stand" >"$1.pid"; if [[ "$4" == exited ]]; then kill "$approve_stand"; wait "$approve_stand" || true; fi; eval "$2"; exit "$3"' \
      _ "$trap_work" "$trap_line" "$want" "$stand" >/dev/null 2>&1 || status=$?
    stand_pid="$(cat "$trap_work.pid" 2>/dev/null || true)"
    stopped=unknown
    if [[ -n "$stand_pid" ]]; then
      stopped=stopped
      if kill -0 "$stand_pid" 2>/dev/null; then
        stopped=running
        kill "$stand_pid" 2>/dev/null || true
      fi
    fi
    label="this file's EXIT trap, with a stand-in that is $stand, ends the shell with its own status $want, stops the stand-in and removes the work directory"
    output="status $status; left: $([[ -e "$trap_work" ]] && echo "$trap_work" || echo nothing); stand-in: $stopped"
    check test "$output" = "status $want; left: nothing; stand-in: stopped"
  done
done

# --- k47's readers, read out of airlock-check.sh and run on fixtures of the shapes the stack writes:
# the kind of dev-airlock.sh's profiles file, broker log lines, the Admin API stand's records, the
# token exchange stand's records and lists, and the field reader every judgement of k47 goes
# through. Each has a control in which the fixture is wrong in the one way the reader is there to
# see, and the reader must say so.
for function in k47_kind k47_lines k47_api k47_exchanges k47_hits k47_secrets_json k47_field k47_control_edit k8_copies k8_names k8_named k8_untagged; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/airlock-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done
eval "$(grep -m1 '^K47_KIND_WANT=' "$script_dir/airlock-check.sh")"
k47="$work/k47"
mkdir -p "$k47"
allow3='[{"method":"GET","pathPrefix":"/v1/organizations/"},{"method":"POST","pathPrefix":"/v1/organizations/"},{"method":"DELETE","pathPrefix":"/v1/organizations/"}]'
# The kind as dev-airlock.sh writes it, read out of that script's own heredoc so a change there
# reaches this row.
kind_line="$(sed -n '/^  {"name": "anthropic"/{s/^  {"name": "anthropic", \(.*\)$/\1/p;n;s/^ *\(.*\)}$/\1/p}' "$script_dir/dev-airlock.sh" | tr '\n' ' ')"
kind_json="{\"name\": \"anthropic\", ${kind_line}}"
kind_json="${kind_json//\$ANTHROPIC_HOST/api.anthropic.com}"
kind_json="${kind_json//\$ANTHROPIC_MAX_GRANT/1h}"
kind_json="${kind_json//\$ANTHROPIC_ALLOW/$allow3}"
printf '{"kinds": [%s], "profiles": []}\n' "$kind_json" >"$k47/profiles.json"
output="$(k47_kind "$k47/profiles.json")"
label="k47_kind reads dev-airlock.sh's anthropic kind as the spec's: $K47_KIND_WANT"
check test "$output" = "$K47_KIND_WANT"
jq '(.kinds[0].allow) = [{"method":"GET","pathPrefix":"/v1/organizations/"}]' "$k47/profiles.json" >"$k47/get-only.json"
output="$(k47_kind "$k47/get-only.json")"
label="control: a kind that allows GET alone is not read as the spec's"
check test "$output" != "$K47_KIND_WANT"
output="$(k47_kind <(printf '{"kinds": [], "profiles": []}'))"
label="control: a file with no anthropic kind reads as nothing"
check test -z "$output"

# Broker lines: three carried calls naming the way mint, three refusals naming an allow rule, one
# mint line with a hash, and lines of another secret and another host the reader must leave out.
n=AIRLOCK_ANTHROPIC_NARROWED
hash="$(printf '%s' 'sk-ant-oat01-fix-not-a-secret' | sha256sum | cut -d' ' -f1)"
{
  for m in GET POST DELETE; do
    printf '{"message":"airlock proxy: a call to a bound host","secret":"%s","host":"api.anthropic.com","method":"%s","path":"/v1/organizations/workspaces","allowed":true,"way":"mint","grant":"g1"}\n' "$n" "$m"
  done
  for p in "POST /v1/oauth/token" "POST /v1/messages" "PUT /v1/organizations/workspaces/w1"; do
    printf '{"message":"broker: answer refused","caller_class":"airlock","host":"api.anthropic.com","secrets":["%s"],"method":"%s","path":"%s","reason":"not allowed by rule"}\n' "$n" "${p%% *}" "${p#* }"
  done
  printf '{"message":"broker: a token was minted for a call","secret":"%s","way":"mint","sha256":"%s"}\n' "$n" "$hash"
  printf '{"message":"airlock proxy: a call to a bound host","secret":"OTHER","host":"api.anthropic.com","allowed":true,"way":"mint"}\n'
  printf '{"message":"airlock proxy: a call to a bound host","secret":"%s","host":"s.airlock.test","allowed":true,"way":"mint"}\n' "$n"
  printf 'not a JSON line\n'
} >"$k47/log.jsonl"
lines="$(k47_lines "$n" <"$k47/log.jsonl")"
output="$lines"
label="k47_lines counts one secret's six lines at api.anthropic.com, three carried by mint, three refused naming the rule, and its one hashed mint line"
check test "$(k47_field "$lines" lines) $(k47_field "$lines" allowed) $(k47_field "$lines" allowed_mint) $(k47_field "$lines" refused) $(k47_field "$lines" refused_rule) $(k47_field "$lines" mints) $(k47_field "$lines" mints_hashed) $(k47_field "$lines" mint_hashes)" = "6 3 3 3 3 1 1 $hash"
label="k47_field reads a value that holds spaces whole: the refused methods and paths"
check test "$(k47_field "$lines" refused_methods)" = "POST /v1/oauth/token,POST /v1/messages,PUT /v1/organizations/workspaces/w1"
sed 's/,"way":"mint","grant":"g1"/,"grant":"g1"/' "$k47/log.jsonl" | sed '2s/"reason":"[^"]*"/"reason":"host not bound"/' >"$k47/log-control.jsonl"
lines="$(k47_lines "$n" <"$k47/log-control.jsonl")"
output="$lines"
label="control: carried lines that name no way and a refusal that names no allow rule are not counted as such"
check test "$(k47_field "$lines" allowed_mint) $(k47_field "$lines" refused_rule)" = "0 3"

# The Admin API stand's records and the exchange's list: three allowed requests carrying one token
# the exchange answered for svac_x, against a placeholder and a refused request reaching the stand.
printf '[{"access_token":"sk-ant-oat01-fix-not-a-secret","expires_in":3600,"service_account_id":"svac_x"},{"access_token":"sk-ant-oat01-oth-not-a-secret","expires_in":3600,"service_account_id":"svac_y"}]\n' >"$k47/tokens.json"
jq -n '[("GET /v1/organizations/workspaces"), ("POST /v1/organizations/workspaces"), ("DELETE /v1/organizations/workspaces/w1")]
  | map(split(" ") | {method: .[0], path: .[1], query: "", host: "api.anthropic.com", header: {Authorization: ["Bearer sk-ant-oat01-fix-not-a-secret"]}, body: ""})' >"$k47/api.json"
api="$(k47_api "$k47/api.json" "$k47/tokens.json" svac_x)"
output="$api"
label="k47_api counts three allowed records carrying the token the exchange answered for the secret, one value, no placeholder and no refused request"
check test "$(k47_field "$api" allowed) $(k47_field "$api" token_ok) $(k47_field "$api" distinct) $(k47_field "$api" placeholders) $(k47_field "$api" refused_reached)" = "3 3 1 0 0"
api="$(k47_api "$k47/api.json" "$k47/tokens.json" svac_y)"
output="$api"
label="control: the same records read against another secret's tokens carry none of its tokens"
check test "$(k47_field "$api" token_ok)" = 0
jq '. + [{method: "POST", path: "/v1/messages", query: "", host: "api.anthropic.com", header: {Authorization: ["Bearer agent-secrets-placeholder-not-a-secret"]}, body: ""}]' "$k47/api.json" >"$k47/api-control.json"
api="$(k47_api "$k47/api-control.json" "$k47/tokens.json" svac_x)"
output="$api"
label="control: a refused request that reached the stand with a placeholder is counted as both"
check test "$(k47_field "$api" refused_reached) $(k47_field "$api" placeholders)" = "1 1"

# The exchange's records against the iam stand's list.
printf '[{"account":"a@p.iam.gserviceaccount.com","audience":"https://api.anthropic.com","token":"idt-1"}]\n' >"$k47/identities.json"
jq -n '[{method: "POST", path: "/v1/oauth/token", body: ({grant_type: "g", assertion: "idt-1", organization_id: "org", federation_rule_id: "fdrl_r", service_account_id: "svac_x", workspace_id: "wrkspc_w"} | tojson)}]' >"$k47/exchange.json"
exchanges="$(k47_exchanges "$k47/exchange.json" "$k47/identities.json" a@p.iam.gserviceaccount.com svac_x org fdrl_r wrkspc_w)"
output="$exchanges"
label="k47_exchanges reads one exchange with the reader's token and the stored value's ids"
check test "$(k47_field "$exchanges" exchanges) $(k47_field "$exchanges" matching) $(k47_field "$exchanges" identity_tokens) $(k47_field "$exchanges" other_audiences)" = "1 1 1 0"
exchanges="$(k47_exchanges "$k47/exchange.json" "$k47/identities.json" a@p.iam.gserviceaccount.com svac_x other-org fdrl_r wrkspc_w)"
output="$exchanges"
label="control: an exchange that names another organization is not read as matching"
check test "$(k47_field "$exchanges" matching)" = 0

# The token search, and the list the rules test reads tags from.
printf 'X=sk-ant-oat01-a\n' >"$k47/env-with.txt"
printf 'X=agent-secrets-placeholder-a\n' >"$k47/env-without.txt"
output="$(k47_hits "$k47/env-with.txt") / $(k47_hits "$k47/env-without.txt") / $(k47_hits "$k47/nothing-here")"
label="k47_hits finds the token where it is, not where it is not, and a file it could not read is zero bytes, never a clean search"
check test "$output" = "hits=1 bytes=17 / hits=0 bytes=30 / hits=0 bytes=0"
printf '{"secrets":[{"name":"example/agent-secrets/airlock-anthropic-narrowed","tags":{"proxy":"anthropic","proxy-narrowed":"true"},"value":"v"}]}\n' >"$k47/fake.json"
output="$(k47_secrets_json "$k47/fake.json" | jq -c .)"
label="k47_secrets_json lists each fake secret as gcloud lists it, its tags as its annotations, and no value"
check test "$output" = '[{"name":"projects/000000000000/secrets/agent-secrets-airlock-anthropic-narrowed","annotations":{"proxy":"anthropic","proxy-narrowed":"true"}}]'
printf '[{"name":"a","annotations":{"owner":"o","proxy":"aws"}},{"name":"b","annotations":{"owner":"o","proxy":"stest"}},{"name":"c","annotations":{"owner":"o"}}]\n' >"$k47/store.json"
output="$(k8_untagged "$k47/store.json" aws | jq -c .)"
label="k8_untagged takes the proxy tag off the named kind's secrets alone and keeps every other tag and secret"
check test "$output" = '[{"name":"a","annotations":{"owner":"o"}},{"name":"b","annotations":{"owner":"o","proxy":"stest"}},{"name":"c","annotations":{"owner":"o"}}]'
output="$(k8_untagged "$k47/store.json" glogin | jq -c .)"
label="control: k8_untagged of a kind no secret carries changes nothing"
check test "$output" = "$(jq -c . "$k47/store.json")"

# The two control edits, on copies of the files they edit: each takes its text out of this tree's
# file, and refuses a file it has already been made on, whose build would be the stack's own.
envoy_dir="$(cd "$script_dir/.." && pwd)"
for defect in kind-allow lifetime; do
  ctl="$work/ctl-$defect"
  mkdir -p "$ctl/internal/cipher/proxyrules" "$ctl/internal/cipher/secrets"
  cp "$envoy_dir/internal/cipher/proxyrules/airlock.go" "$ctl/internal/cipher/proxyrules/"
  cp "$envoy_dir/internal/cipher/secrets/anthropicfederation.go" "$ctl/internal/cipher/secrets/"
  status=0
  k47_control_edit "$ctl" "$defect" || status=$?
  output="status $status"
  label="k47's $defect edit applies to this tree's file and takes its text out"
  check test "$status" -eq 0
  status=0
  k47_control_edit "$ctl" "$defect" || status=$?
  output="status $status"
  label="control: the $defect edit refuses a file that no longer holds its text"
  check test "$status" -ne 0
done

# --- k8's copies of a profiles file of the stack's shape: each of the nine it writes differs from
# the head's file, the malformed one is no JSON, and each passing copy's output names what k8_named
# wants. The control is a head with none of the kinds the copies edit, whose copies are the head.
k8d="$work/k8"
mkdir -p "$k8d"
jq -n --argjson allow "$allow3" '{kinds: [{name: "stest", hosts: ["s.airlock.test"], ways: ["swap"]},
  {name: "scoped", hosts: ["s.airlock.test"], headers: ["authorization", "x-key"], ways: ["swap"], allow: [{method: "GET", pathPrefix: "/read"}]},
  {name: "aws", hosts: ["aws.airlock.test"], ways: ["sign"]},
  {name: "anthropic", hosts: ["api.anthropic.com"], ways: ["mint"], allow: $allow}],
  profiles: [{name: "AIRLOCK_ANTHROPIC_NARROWED", allow: $allow}]}' >"$k8d/head.json"
k8_copies "$k8d/head.json" "$k8d"
same=""
while read -r copy want; do
  [[ "$want" == unknown || "$copy" == malformed ]] && continue
  [[ "$(jq -cS . "$k8d/$copy.json")" != "$(jq -cS . "$k8d/head.json")" ]] || same+=" $copy"
done <"$k8d/copies"
output="copies equal to the head:${same:- none}; listed: $(tr '\n' ',' <"$k8d/copies")"
label="k8_copies writes nine copies, each changed from the head's file, and lists fields as not run"
check test -z "$same" -a "$(wc -l <"$k8d/copies")" -eq 10 -a "$(grep -c ' unknown$' "$k8d/copies")" -eq 1
status=0
jq . "$k8d/malformed.json" >/dev/null 2>&1 || status=$?
output="jq exited $status"
label="the malformed copy is no JSON"
check test "$status" -ne 0
mkdir -p "$k8d/empty"
printf '{"kinds": [], "profiles": []}\n' >"$k8d/empty/head.json"
k8_copies "$k8d/empty/head.json" "$k8d/empty"
output="$(jq -cS . "$k8d/empty/no-host.json")"
label="control: on a head with none of the kinds it edits, k8_copies' no-host copy is the head itself, which the row above would report"
check test "$output" = "$(jq -cS . "$k8d/empty/head.json")"
output='kind "stest": hosts extra.airlock.test added'
label="k8_named reads a passing copy's added host off the report line"
check k8_named new-host "$output"
label="control: k8_named does not read another kind's line as the copy's"
check test "$(k8_named new-host 'kind "scoped": hosts extra.airlock.test added' && echo yes || echo no)" = no
label="k8_named reads the parse position of the malformed copy"
check k8_named malformed 'profiles check: k8/malformed.json: invalid character ] after object key:value pair at byte 74'

# --- The verdict: a property this head does not build is no pass, and a run that judged nothing
# is no pass either.
for function in pass fail unknown deferred report; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/airlock-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: airlock-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done
verdict() { # <steps...>: runs them on fresh counters, answers report's last line and exit status
  local step out
  EXPECTED=() OBSERVED=() CONTROL=() VERDICTS=() PASSED=0 TOTAL=0 NOT_JUDGED=0 CHECK_ID=kx
  for step in "$@"; do $step what slice seen; done
  out="$(report)"
  printf '%s %s' "$?" "$(printf '%s' "$out" | tail -n 1)"
}
output="$(verdict pass)"; label="a run whose every property passed passes"; check test "${output%% *}" = 0
output="$(verdict pass unknown)"; label="a run with an UNKNOWN property exits 3, not 0"; check test "${output%% *}" = 3
output="$(verdict pass deferred)"; label="a run with a DEFERRED property exits 3, not 0"; check test "${output%% *}" = 3
output="$(verdict)"; label="a run that judged nothing exits 3, not 0"; check test "${output%% *}" = 3
output="$(verdict pass fail unknown)"; label="a run with a failed property exits 1"; check test "${output%% *}" = 1

if [[ "$failed" -ne 0 ]]; then
  printf 'airlock-check inputs: failed (%s checks)\n' "$failed"
  exit 1
fi
echo 'airlock-check inputs: passed'
