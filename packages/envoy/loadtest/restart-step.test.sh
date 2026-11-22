#!/usr/bin/env bash
# packages/envoy/loadtest/restart-step.test.sh
#
# Proves restart-step.sh's two stops. kill_and_restart, the step's own kill of the stack's
# listener, starts the listener again only once the killed one has ended, every thread of it: its
# main thread a zombie is not enough while another thread still holds the listener's port. And
# end_step, its stop of the step it started when the script itself fails: the SIGTERM goes to that
# step's own `loadgen run`, found below the run-step.sh the script started, and to no other
# process, whatever its command line; and the stop returns only once that loadgen has ended, every
# thread of it. It needs no stack. The listener is a Python stand-in that answers the listener's
# sessions read, ends its main thread alone on its signal and holds its port in a second thread
# until this suite releases it. The step is a shell that runs a stand-in loadgen below an
# intermediate shell, as run-step.sh runs stack.sh's loadgen; that stand-in's main thread leaves
# first on SIGTERM while a second thread holds its port one second more, as the threads of a Go
# loadgen end one by one. A second stand-in with its command line runs in a session of its own, as
# another run's loadgen would, and the stop must send it nothing. The functions under test are
# restart-step.sh's own text, read out of the script and run with common.sh, which the script
# sources.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/test-lib.sh
source "$here/test-lib.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/restart-step-test.XXXXXX")
readonly tmp
# Every process this suite starts is recorded, and its exit ends them by their pids.
cleanup() {
  end_recorded "$tmp/started" || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}
pass() { printf 'PASS: %s\n' "$*"; }

command -v python3 >/dev/null || fail "restart-step.test.sh needs python3 on PATH for its stand-in loadgen"

# The functions under test, each from its `NAME() {` line to the `}` that closes it.
: >"$tmp/functions.sh"
for fn in listener_wait_ended kill_and_restart step_loadgen end_step; do
  awk -v open="$fn() {" '$0 == open { on = 1 } on { print } on && $0 == "}" { exit }' "$here/restart-step.sh" >>"$tmp/functions.sh"
done
for fn in listener_wait_ended kill_and_restart end_step; do
  grep -qx "$fn() {" "$tmp/functions.sh" || fail "restart-step.sh holds no function $fn"
done

mkdir -p "$tmp/bin"
cat >"$tmp/bin/loadgen" <<'EOF'
#!/usr/bin/env python3
# The stand-in loadgen: `loadgen run -name NAME -out OUT`. It holds a listening port in a second
# thread, writes that port and then its pid next to PIDFILE, and on SIGTERM writes PIDFILE.term
# and ends its main thread alone, by the exit system call of that thread; the second thread ends
# the process a second later, the port still held until then.
import ctypes, os, platform, signal, socket, threading, time

pidfile = os.environ["PIDFILE"]
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", 0))
listener.listen(4)
stop = threading.Event()


def hold():
    stop.wait()
    time.sleep(1)
    os._exit(0)


def on_term(signum, frame):
    with open(pidfile + ".term", "a") as f:
        f.write("TERM\n")
    stop.set()
    ctypes.CDLL(None).syscall({"x86_64": 60, "aarch64": 93}[platform.machine()], 0)


threading.Thread(target=hold).start()
signal.signal(signal.SIGTERM, on_term)
with open(pidfile + ".port", "w") as f:
    f.write("%d\n" % listener.getsockname()[1])
with open(pidfile + ".tmp", "w") as f:
    f.write("%d\n" % os.getpid())
os.rename(pidfile + ".tmp", pidfile)
while True:
    time.sleep(0.1)
EOF
chmod +x "$tmp/bin/loadgen"

# ---- kill_and_restart: the listener starts again only once the killed one has ended --------------
# The stand-in listener answers the sessions read kill_and_restart polls, with the stack's token,
# on the port stack.json names. SIGKILL ends each thread of a process when the kernel next runs it,
# often the main one first, so for a moment the main thread is a zombie while another thread still
# holds the port. The stand-in holds that moment until this row releases it: kill_and_restart's
# SIGKILL to it is sent as a SIGTERM, on which its main thread leaves by the exit system call of
# that thread alone, and its second thread holds the port until the release file exists. The row
# orders that on events: once the main thread is a zombie, it waits until kill_and_restart has
# napped twice since, which the row's sleep counts, or has gone on. A kill that waits for every
# thread is still napping then, and goes on once the row releases the port; a kill that takes the
# zombie main thread for an ended listener has gone on, and the row fails on a line of its own.
#
# The stand-in picks its own port, by a bind to port 0 that it holds from then on, and writes the
# port it got; the row reads it back before it writes stack.json. No port is chosen by one process
# and bound by another, which another program on the machine could take in between. The restart
# kill_and_restart makes runs the same command line, and binds the port that first start wrote, as
# the stack's listener binds the port stack.json names.
cat >"$tmp/bin/listener" <<'EOF'
#!/usr/bin/env python3
# The stand-in listener: `listener PORT_FILE TOKEN MARK RELEASE`. It binds the port PORT_FILE names,
# or, when PORT_FILE does not exist yet, port 0, and then writes the port it got to PORT_FILE. A
# second thread answers GET /v1/sessions with 200 for the bearer TOKEN on 127.0.0.1 at that port.
# On SIGTERM it writes MARK and ends its main thread alone; the second thread stops answering and
# holds the port until RELEASE exists.
import ctypes, os, platform, signal, socket, sys, threading, time

port_file, token, mark, release = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
port = int(open(port_file).read()) if os.path.exists(port_file) else 0
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen(16)
s.settimeout(0.05)
if port == 0:
    with open(port_file + ".tmp", "w") as f:
        f.write("%d\n" % s.getsockname()[1])
    os.rename(port_file + ".tmp", port_file)
stopping = threading.Event()


def serve():
    while not stopping.is_set():
        try:
            c, _ = s.accept()
        except socket.timeout:
            continue
        c.settimeout(1)
        try:
            asked = c.recv(4096).decode(errors="replace")
            ok = asked.startswith("GET /v1/sessions") and ("Bearer " + token) in asked
            c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n[]" if ok
                      else b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        except OSError:
            pass
        c.close()
    while not os.path.exists(release):
        time.sleep(0.01)
    os._exit(0)


def on_term(signum, frame):
    with open(mark, "a") as f:
        f.write("TERM\n")
    stopping.set()
    ctypes.CDLL(None).syscall({"x86_64": 60, "aarch64": 93}[platform.machine()], 0)


# The handler first: the suite sends its TERM once the listener answers, and a TERM before the
# handler would end the whole process at once.
signal.signal(signal.SIGTERM, on_term)
threading.Thread(target=serve).start()
while True:
    time.sleep(0.1)
EOF
chmod +x "$tmp/bin/listener"
kr=$tmp/kr
mkdir -p "$kr/run" "$kr/root" "$kr/logs" "$kr/out"
kr_token=restart-test-token
"$tmp/bin/listener" "$kr/port" "$kr_token" "$kr/term" "$kr/release" </dev/null >/dev/null 2>&1 &
old=$!
record_pid "$tmp/started" "$old"
echo "$old" >"$kr/run/listener.pid"
# The waits for the stand-in are events, each with a backstop of 600 s for a hang, named as one.
t0=$EPOCHSECONDS
until [ -s "$kr/port" ]; do
  ! process_ended "$old" || fail "the stand-in listener (pid $old) ended before it wrote the port it bound"
  ((EPOCHSECONDS - t0 <= 600)) || fail "the stand-in listener (pid $old) wrote no port within 600 s, the row's backstop for a hang"
  sleep 0.05
done
kr_port=$(<"$kr/port")
printf '{"ports": {"listener": %s}, "listener_token": "%s"}\n' "$kr_port" "$kr_token" >"$kr/root/stack.json"
until printf 'Authorization: Bearer %s' "$kr_token" | curl -fsS --max-time 1 -H @- "http://127.0.0.1:$kr_port/v1/sessions" >/dev/null 2>&1; do
  ! process_ended "$old" || fail "the stand-in listener (pid $old) ended before it answered on port $kr_port"
  ((EPOCHSECONDS - t0 <= 600)) || fail "the stand-in listener did not answer on port $kr_port within 600 s, the row's backstop for a hang"
  sleep 0.05
done
: >"$kr/naps"
# shellcheck disable=SC2016 # expanded by the bash that sources common.sh and the functions
bash -c '
  source "$1"
  source "$2"
  RUN=$3/run ROOT=$3/root LOGS=$3/logs out=$3/out down=0 naps=$3/naps
  die() { printf "die: %s\n" "$*"; exit 1; }
  note() { printf "note: %s\n" "$*"; }
  sleep() {
    printf "%s\n" "$*" >>"$naps"
    command sleep "$@"
  }
  victim=$(<"$RUN/listener.pid")
  kill() {
    if [ "$1" = -KILL ] && [ "${2:-}" = "$victim" ] && [ "$#" = 2 ]; then
      command kill -TERM "$victim" # no-wait: its caller, kill_and_restart, waits for the victim
      return
    fi
    command kill "$@" # no-wait: a pass-through; its caller waits for what it signals
  }
  kill_and_restart
  echo "kill_and_restart returned"
' _ "$here/common.sh" "$tmp/functions.sh" "$kr" >"$kr/out.log" 2>&1 &
kr_job=$!
# kr_record_new: the listener kill_and_restart started, once its job has ended, recorded for the
# suite's exit (record_pid) when it is not the killed one, on every path out of the row: a row that
# fails has seen kill_and_restart start that listener as well, and would leave it holding the port.
kr_record_new() {
  new=$(cat "$kr/run/listener.pid" 2>/dev/null || true)
  [ -z "$new" ] || [ "$new" = "$old" ] || record_pid "$tmp/started" "$new"
}
# kr_fail MESSAGE: the port released, kill_and_restart waited for and the listener it started
# recorded, then the row fails.
kr_fail() {
  : >"$kr/release"
  wait "$kr_job" 2>/dev/null || true
  kr_record_new
  fail "$* (kill_and_restart said: $(cat "$kr/out.log"))"
}
# The row waits on events, each with a backstop of 600 s for a hang, named as one: the box's speed
# moves when an event comes, never which one comes.
t0=$EPOCHSECONDS
until [ -e "$kr/term" ] && [ "$(process_state "$old")" = Z ]; do
  process_ended "$kr_job" && kr_fail "kill_and_restart ended before the killed listener's main thread (pid $old) was a zombie"
  ((EPOCHSECONDS - t0 <= 600)) || kr_fail "the killed listener's main thread (pid $old) was no zombie within 600 s of kill_and_restart's start, the row's backstop for a hang"
  sleep 0.05
done
mapfile -t kr_naps <"$kr/naps"
kr_then=${#kr_naps[@]}
while ((${#kr_naps[@]} <= kr_then + 1)) && ! grep -q '^note: killed the listener' "$kr/out.log" && ! process_ended "$kr_job"; do
  ((EPOCHSECONDS - t0 <= 600)) || kr_fail "kill_and_restart neither napped nor went on within 600 s of its start, the row's backstop for a hang"
  sleep 0.01
  mapfile -t kr_naps <"$kr/naps"
done
if grep -q '^note: killed the listener' "$kr/out.log" || process_ended "$kr_job"; then
  kr_fail "kill_and_restart went on past its wait while the killed listener's other thread still held port $kr_port: it took the zombie main thread for an ended listener"
fi
: >"$kr/release"
kr_rc=0
wait "$kr_job" || kr_rc=$?
kr_record_new
((kr_rc == 0)) || fail "kill_and_restart ended with $kr_rc: $(cat "$kr/out.log")"
grep -qx 'kill_and_restart returned' "$kr/out.log" || fail "kill_and_restart did not return: $(cat "$kr/out.log")"
jq -e --argjson o "$old" --argjson n "${new:-0}" '.old_pid == $o and .new_pid == $n and .new_pid != $o' "$kr/out/restart.json" >/dev/null ||
  fail "restart.json does not name the killed listener (pid $old) and the restarted one (pid ${new:-none}): $(cat "$kr/out/restart.json" 2>/dev/null)"
printf 'Authorization: Bearer %s' "$kr_token" | curl -fsS --max-time 5 -H @- "http://127.0.0.1:$kr_port/v1/sessions" >/dev/null 2>&1 ||
  fail "the restarted listener (pid $new) does not answer on port $kr_port"
end_process "$new" || fail "the restarted listener (pid $new) still runs 10 s after its SIGKILL"
wait "$old" 2>/dev/null || true
pass "kill_and_restart starts the listener again only once the killed one has ended, every thread of it, not once its main thread is a zombie"

# ---- end_step: the step's own loadgen, and no other process of that command line ---------------
# The shell that runs end_step starts the step itself, as restart-step.sh does, so the step is its
# child and end_step's wait on it is the script's own. The other loadgen is started once the step's
# is up, in a session of its own. Every pid goes to a file of this suite's and to its record.
name=restart-test-${tmp##*.} out=$tmp/out
end_out=$tmp/end.out end_rc=0
# shellcheck disable=SC2016 # expanded by the bash that sources common.sh and the functions
timeout 150 bash -c '
  source "$1"
  source "$2"
  source "$3"
  tmp=$4 name=$5 out=$6
  up() { local t0=$EPOCHSECONDS; until [ -s "$1" ]; do ((EPOCHSECONDS - t0 <= 10)) || { echo "no $1 within 10 s"; exit 1; }; sleep 0.05; done; }
  PIDFILE=$tmp/own bash -c "( \"\$1\" run -name \"\$2\" -out \"\$3\"; : ); wait" _ "$tmp/bin/loadgen" "$name" "$out" &
  step=$!
  echo "$step" >"$tmp/step"
  record_pid "$tmp/started" "$step"
  up "$tmp/own"
  record_pid "$tmp/started" "$(<"$tmp/own")"
  PIDFILE=$tmp/planted setsid -f "$tmp/bin/loadgen" run -name "$name" -out "$out" </dev/null >/dev/null 2>&1
  up "$tmp/planted"
  record_pid "$tmp/started" "$(<"$tmp/planted")"
  # The loadgen'"'"'s listening socket, by its inode (socket_inodes): a connect to its port number
  # after it has let go would reach whatever program the machine gave that number to next.
  own_socket=$(socket_inodes "$(<"$tmp/own")")
  [ -n "$own_socket" ] || { echo "the step'"'"'s loadgen (pid $(<"$tmp/own")) showed no socket"; exit 1; }
  false || end_step
  ended=no held=no
  ! process_ended "$(<"$tmp/own")" || ended=yes
  # shellcheck disable=SC2086 # one inode a word
  ! socket_held $own_socket || held=yes
  echo "the moment end_step returned: the step'"'"'s loadgen had ended: $ended, its socket was still open: $held"
' _ "$here/common.sh" "$here/test-lib.sh" "$tmp/functions.sh" "$tmp" "$name" "$out" >"$end_out" 2>&1 || end_rc=$?
((end_rc == 0)) || fail "end_step's shell ended with $end_rc: $(cat "$end_out")"
step=$(<"$tmp/step") own=$(<"$tmp/own") planted=$(<"$tmp/planted")
# A SIGTERM the other loadgen took is written within its next 0.1 s nap; it is given 2 s.
t0=$EPOCHSECONDS
until [ -e "$tmp/planted.term" ] || ((EPOCHSECONDS - t0 > 2)); do sleep 0.1; done
[ -e "$tmp/planted.term" ] &&
  fail "end_step sent SIGTERM to a loadgen with the step's command line in another session (pid $planted), which this script never started: $(cat "$end_out")"
process_ended "$planted" &&
  fail "the loadgen in another session (pid $planted) has gone, though nothing of this suite signals it: $(cat "$end_out")"
[ -e "$tmp/own.term" ] || fail "end_step sent the step's own loadgen (pid $own) no SIGTERM: $(cat "$end_out")"
grep -qx "the moment end_step returned: the step's loadgen had ended: yes, its socket was still open: no" "$end_out" ||
  fail "end_step returned before the step's loadgen (pid $own) had ended, every thread of it, its socket closed: $(cat "$end_out")"
process_ended "$step" || fail "end_step returned with the step (pid $step) still there: $(cat "$end_out")"
end_process "$planted" || fail "the loadgen in another session (pid $planted) still runs 10 s after its SIGKILL"
pass "end_step sends its SIGTERM to the step's own loadgen run alone, not to one of the same command line in another session, and returns once that loadgen has ended, every thread of it, and the step with it"

# ---- end_step: a step that has already ended is no step to end -----------------------------------
# A step that has exited and that nothing has reaped yet is a zombie, which `kill -0` calls running.
# end_step reads the step's own state, by the pid and start the script recorded when it started it
# (proc_ended), so a step that has ended gets no stop: no line says it is being ended, and nothing
# is sent. Here the step is a child that exits under a parent that never waits -- a sleep that a
# shell became by exec -- so it stays a zombie while end_step runs. The waits are events, each with
# a backstop of 600 s for a hang.
zs_out=$tmp/end-zombie.out zs_rc=0
# shellcheck disable=SC2016 # expanded by the bash that sources common.sh and the functions
timeout 1500 bash -c '
  source "$1"
  source "$2"
  source "$3"
  tmp=$4 name=$5 out=$6
  bash -c "\"\$1\" 0.1 & echo \"\$!\" >\"\$2.tmp\"; mv -- \"\$2.tmp\" \"\$2\"; exec \"\$1\" 300" _ "$(command -v sleep)" "$tmp/zstep" &
  parent=$!
  record_pid "$tmp/started" "$parent"
  t0=$EPOCHSECONDS
  until [ -s "$tmp/zstep" ]; do
    ((EPOCHSECONDS - t0 <= 600)) || { echo "the zombie stand-in wrote no pid within 600 s, the row'"'"'s backstop for a hang"; exit 1; }
    sleep 0.05
  done
  step=$(<"$tmp/zstep") step_start=""
  proc_start_into step_start "$step" || { echo "the start of the stand-in step (pid $step) could not be read"; exit 1; }
  until [ "$(process_state "$step")" = Z ]; do
    ((EPOCHSECONDS - t0 <= 600)) || { echo "the stand-in step (pid $step) was no zombie within 600 s, the row'"'"'s backstop for a hang"; exit 1; }
    sleep 0.05
  done
  false || end_step
  echo "end_step returned"
  # no-wait: its own child, which the wait below reaps.
  kill -KILL "$parent"
  wait "$parent" 2>/dev/null || true
' _ "$here/common.sh" "$here/test-lib.sh" "$tmp/functions.sh" "$tmp" "$name" "$out" >"$zs_out" 2>&1 || zs_rc=$?
((zs_rc == 0)) || fail "end_step's shell over a step that has ended ended with $zs_rc: $(cat "$zs_out")"
grep -qx 'end_step returned' "$zs_out" || fail "end_step over a step that has ended did not return: $(cat "$zs_out")"
! grep -qF 'ending the step' "$zs_out" ||
  fail "end_step set out to end a step that had already ended, a zombie that nothing had reaped: it read the step's pid in the table for a step that runs: $(cat "$zs_out")"
pass "end_step takes a step that has ended and that nothing has reaped, a zombie, for one that has ended, and sends it nothing"

printf 'PASS: packages/envoy/loadtest/restart-step.test.sh\n'
