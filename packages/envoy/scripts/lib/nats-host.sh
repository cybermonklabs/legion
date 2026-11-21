# shellcheck shell=bash disable=SC2034 # the sourcing script reads nats_host
# packages/envoy/scripts/lib/nats-host.sh
#
# The loopback address a NATS server a script starts itself listens on: one of its own, so no
# client of another stack on the same machine reaches it, whatever port it gets. Sourced, never run,
# by every script that starts a nats-server binary or container, or restarts natsd
# (internal/testnats/cmd/natsd) on its old port. A script that starts natsd once and never again
# needs none of this: natsd claims an address itself when it is given no -a, and names it on its
# `Listening for client connections on <address>:<port>` line, which the script reads.
#
# Why. Two stacks run at once on one development machine, each with a server of its own. While a
# stack restarts its server on its old port, its clients keep dialing that address and port, and
# the other stack's server can be given the port in the gap (nats-server's -p -1, docker's
# 127.0.0.1::4222): the first stack's clients then reach the wrong server, and its own restart
# fails. A server on a loopback address no other stack's server holds is out of their reach.
#
# claim_nats_host BIN_DIR builds internal/testnats/cmd/loopbackhost into BIN_DIR, once per run (a
# binary already there is used as it is), and claims an address for this script's process ($$),
# which it sets in nats_host. The claim is a file in /tmp/legion-loopback-hosts-<uid> naming this
# process (internal/testport's Claim), so no other script or natsd of this user takes the address
# while this script runs, and it is free again once this process exits, even unreleased.
# loopbackhost binds port 0 on the address before it prints it, so an address that is not up fails
# here and names itself. On Linux every address of 127.0.0.0/8 is the loopback, and the address is
# one of 127.1.0.1 through 127.254.255.254; elsewhere only 127.0.0.1 is up, and it is 127.0.0.1.
#
# release_nats_host removes this script's claim, for the script's cleanup; it does nothing when
# there is none. The script passes the address to every start of its server (`-a "$nats_host"`,
# `docker run -p "$nats_host::4222"`), and builds every URL from it, never from 127.0.0.1.
#
# Needs go on PATH. The module is the checkout this file is in.

nats_host=""
nats_host_bin=""

# The locals carry a prefix of their own: a sourcing script's readonly variable of the same name
# (e2e-refill.sh's bin_dir) would refuse a plain `local bin_dir`.
claim_nats_host() {
  local _nats_host_dir=$1 _nats_host_envoy
  _nats_host_envoy="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
  nats_host_bin="$_nats_host_dir/loopbackhost"
  if [ ! -x "$nats_host_bin" ]; then
    mkdir -p -- "$_nats_host_dir"
    go -C "$_nats_host_envoy" build -o "$nats_host_bin" ./internal/testnats/cmd/loopbackhost || {
      echo "nats-host.sh: internal/testnats/cmd/loopbackhost did not build" >&2
      return 1
    }
  fi
  nats_host=$("$nats_host_bin" -owner "$$") || {
    echo "nats-host.sh: no loopback address could be claimed" >&2
    return 1
  }
}

release_nats_host() {
  [ -n "$nats_host" ] && [ -x "$nats_host_bin" ] || return 0
  "$nats_host_bin" -owner "$$" -release "$nats_host" || true
}
