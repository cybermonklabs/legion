# shellcheck shell=bash
# The one reader of the port a process that bound port 0 itself logs once it listens. lib/rig.sh
# and scripts/e2e/envoy-session-delivery.sh source it, so a change to how such a line is matched
# reaches both. Sourced, never run: it defines report_port and reported_port, and needs nothing of
# its caller's.

# report_port LOG PATTERN prints the port a process that bound port 0 itself reported: the digits
# right after the first match of the extended regular expression PATTERN in LOG (nats-server -p -1:
# 'Listening for client connections on [^ ]*:'). The first match is the first one on the first line
# that holds any: grep -m1 stops after that line, and -o prints every match on it, one per line. It
# fails while LOG holds no such line, so it is also the poll that waits for one.
report_port() {
  local _match
  _match=$(grep -m1 -oE -- "$2[0-9]+" "$1" 2>/dev/null) || return 1
  _match=${_match%%$'\n'*}
  printf '%s\n' "${_match##*[!0-9]}"
}

# reported_port LOG PATTERN VAR assigns VAR the port report_port prints, rather than printing it, so
# a poll that runs it in the caller's own shell (poll_until, wait_for, await) keeps the answer. It
# fails as report_port does. The local's name is one no caller passes: printf -v would otherwise
# assign the local.
reported_port() {
  local _reported_port
  _reported_port=$(report_port "$1" "$2") || return 1
  printf -v "$3" '%s' "$_reported_port"
}
