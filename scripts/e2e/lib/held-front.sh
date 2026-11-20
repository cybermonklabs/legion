# shellcheck shell=bash
# The held front of a server that takes no socket and must serve one port across its restarts: a
# keeper (lib/held-port.py keep) holds one loopback port for the whole run, and a front serves that
# port by forwarding each connection to the server's own port, which the kernel chose as the server
# bound it (natsd -p -1). When the server stops and starts again it binds a new port the kernel
# chooses, and a new front is started on the kept socket, forwarding there, so every client keeps
# the one URL it was given, the kept port's, and no port is free between its choice and its bind.
#
# A front lives only while its server does: the caller stops the front when it stops the server,
# and starts a new one once the server serves again. A front left running would forward to the
# port the server released, which another program can bind in the meantime. Between two fronts the
# kept port still listens: the kernel queues a connection until the next front accepts it, so a
# client sees a hang rather than a refusal, and every poll of a kept port carries a timeout.
#
# scripts/e2e/envoy-session-delivery.sh fronts natsd this way for each proxy it cuts, and each
# --build-from listener that takes no ENVOY_LISTEN_FD; scripts/e2e/envoy-session-states.sh and
# packages/envoy/scripts/e2e-refill.sh front natsd itself, which they stop and start again under a
# running listener.
#
# Sourced, never run: it defines held_front_argv and held_front_answers, starts nothing, and needs
# python3 and socat (1.8.0 or later, for ACCEPT-FD) on the PATH the caller runs the front with.

held_front_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

# held_front_argv VAR KEEPER_SOCKET PORT sets the array VAR to the command that serves the port the
# keeper behind the unix socket KEEPER_SOCKET holds, forwarding each connection to 127.0.0.1:PORT:
# held-port.py exec --from takes a copy of the kept listening socket as descriptor 3 and executes
# socat in place, which accepts on it (ACCEPT-FD; held-port.py names the descriptor
# FRONT_LISTEN_FD, which socat does not read), one fork per connection. The caller runs it under
# its own isolation, log and process bookkeeping: stopped with its process group, it cuts every
# connection through it, and the keeper keeps the port.
held_front_argv() {
  local -n _held_front=$1
  _held_front=(python3 "$held_front_dir/held-port.py" exec --from "$2" --fd-env FRONT_LISTEN_FD --
    socat "ACCEPT-FD:3,fork" "TCP:127.0.0.1:$3")
}

# held_front_answers PORT WORD succeeds when a connection to 127.0.0.1:PORT reads WORD, the first
# bytes the server behind the front sends (natsd's INFO), within 2 s: a front is serving there and
# its server answers. A kept port with no front accepts the connection and sends nothing.
held_front_answers() {
  local _word=""
  (exec 3<>"/dev/tcp/127.0.0.1/$1" && read -r -t 2 -N "${#2}" _word <&3 && [[ "$_word" == "$2" ]]) 2>/dev/null
}
