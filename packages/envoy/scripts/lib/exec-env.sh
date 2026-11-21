# shellcheck shell=bash
# packages/envoy/scripts/lib/exec-env.sh
#
# How these scripts start a program with exactly the environment they name when that environment
# holds a secret (a database URL and its password, a bearer token): through the environment of the
# shell that execs the program, /proc/<pid>/environ, which only its own user and root read, and
# never through env's arguments, /proc/<pid>/cmdline, which every user of the machine reads (ps
# prints them) for as long as the process runs.
#
# Sourced, never run. dev-stack.sh (start_with_only), airlock-check.sh and judge-check.sh
# (start_extra_broker and the brokers of their checks) call it. Three functions:
#
#   exec_with_env NAME=value... -- COMMAND [ARG...]
#       Execs COMMAND, in place of the shell that runs it, with exactly the NAME=value entries as
#       its environment. A caller runs it in a subshell, `( exec_with_env ... -- CMD ) >log 2>&1 &`,
#       so that $! is COMMAND's pid. Every variable and function the shell exports loses its export
#       first: an unset one is gone, and a readonly one, which no unset removes, keeps its value in
#       the shell and loses only its export, so it reaches COMMAND no more. An entry whose name no
#       shell can export (an exported function's BASH_FUNC_name%%, copied from another process's
#       environment) is left out. An entry that names a readonly variable cannot take its value,
#       so the start is refused: the variable is named on standard error, nothing runs, and it
#       returns 2. So does an exported variable that is still exported after both steps.
#   broker_env_of PID [NAME=value...]
#       Sets BROKER_ENV to process PID's environment, one NAME=value entry each (/proc/PID/environ
#       is NUL-separated, so a value holding a space or a quote stays whole), with the entries
#       after PID last, so one of them replaces the copy of its name.
#   with_broker_env COMMAND [ARG...]
#       exec_with_env with BROKER_ENV's entries: `( with_broker_env "$BROKER_BIN" ) &` starts a
#       broker with the environment broker_env_of read.

# exec_with_env NAME=value... -- COMMAND [ARG...]: see the header.
exec_with_env() {
  local -a exec_with_env_settings=() exec_with_env_names=()
  local exec_with_env_name exec_with_env_flags exec_with_env_entry
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
    exec_with_env_settings+=("$1")
    shift
  done
  if [ "$#" -lt 2 ]; then
    printf '%s: exec_with_env: no -- COMMAND after the settings\n' "${0##*/}" >&2
    return 2
  fi
  shift
  mapfile -t exec_with_env_names < <(compgen -e)
  for exec_with_env_name in "${exec_with_env_names[@]}"; do
    unset "$exec_with_env_name" 2>/dev/null || true
  done
  # What is still exported is readonly, or a global an unset of a caller's local uncovered.
  mapfile -t exec_with_env_names < <(compgen -e)
  # shellcheck disable=SC2163 # each is a variable's name, whose export export -n takes off
  for exec_with_env_name in "${exec_with_env_names[@]}"; do
    export -n "$exec_with_env_name"
  done
  # shellcheck disable=SC2163 # each is an exported function's name, whose export export -n -f takes off
  while read -r _ exec_with_env_flags exec_with_env_name; do
    if [[ "$exec_with_env_flags" == *x* ]]; then export -n -f "$exec_with_env_name"; fi
  done < <(declare -F)
  mapfile -t exec_with_env_names < <(compgen -e)
  if [ "${#exec_with_env_names[@]}" -gt 0 ]; then
    printf '%s: refused to start %s: %s stayed exported, so it would reach it\n' \
      "${0##*/}" "$1" "${exec_with_env_names[*]}" >&2
    return 2
  fi
  for exec_with_env_entry in "${exec_with_env_settings[@]}"; do
    exec_with_env_name="${exec_with_env_entry%%=*}"
    [[ "$exec_with_env_name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    # shellcheck disable=SC2163 # each entry is NAME=value, which export sets as it exports
    if ! export "$exec_with_env_entry" 2>/dev/null; then
      printf '%s: refused to start %s: %s is readonly in this shell, so it cannot take the value the start gives it\n' \
        "${0##*/}" "$1" "$exec_with_env_name" >&2
      return 2
    fi
  done
  exec "$@"
}

# broker_env_of PID [NAME=value...]: see the header.
broker_env_of() {
  local pid="$1"
  shift
  BROKER_ENV=()
  mapfile -d '' -t BROKER_ENV <"/proc/$pid/environ"
  BROKER_ENV+=("$@")
}

# with_broker_env COMMAND [ARG...]: see the header.
with_broker_env() {
  exec_with_env "${BROKER_ENV[@]}" -- "$@"
}
