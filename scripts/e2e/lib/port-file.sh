# shellcheck shell=bash
# The wait for the port file lib/held-port.py writes: `exec` writes it before it runs its command,
# `keep` once it hands out copies of its socket. lib/rig.sh, scripts/e2e/envoy-session-delivery.sh,
# scripts/e2e/verifiers-staging-token.sh, packages/envoy/scripts/search-smoke.sh and
# packages/envoy/scripts/searchbench.sh source it. Sourced, never run: it defines
# port_file_seconds, read_port_file and pass_flags, and needs nothing of its caller's.

# port_file_seconds bounds read_port_file's wait.
port_file_seconds=30

# read_port_file FILE PID VAR waits, at most port_file_seconds, for FILE while PID, the helper's
# process, lives, and assigns VAR the port FILE names. It prints nothing, so each caller says what
# failed in its own terms: it returns 0 once VAR is set, 1 when PID exits before FILE is written,
# and 2 when the wait runs out. The locals' names are ones no caller passes: printf -v would
# otherwise assign the local.
read_port_file() {
  local _port_file=$1 _port_pid=$2 _port_var=$3 _port_value _port_poll
  for ((_port_poll = 0; _port_poll < port_file_seconds * 10; _port_poll++)); do
    if [ -s "$_port_file" ]; then
      read -r _port_value <"$_port_file"
      printf -v "$_port_var" '%s' "$_port_value"
      return 0
    fi
    kill -0 "$_port_pid" 2>/dev/null || return 1
    sleep 0.1
  done
  return 2
}

# pass_flags NAME=value... sets the array `passed` to `--pass NAME` for each: the names held-port.py
# hands on from its own environment to the command it execs. Only the names travel; the values stay
# in the caller's shell and reach held-port.py through its environment (exec_with_env,
# packages/envoy/scripts/lib/exec-env.sh), so none is in any process's arguments.
pass_flags() {
  local _setting
  passed=()
  for _setting in "$@"; do passed+=(--pass "${_setting%%=*}"); done
}
