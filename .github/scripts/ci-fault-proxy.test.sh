#!/usr/bin/env bash
# Tests for `ci-fault-proxy.py`: each of its three modes over plain HTTP and over CONNECT, a
# host outside --hosts passed on in every one of them, --bind, the window of its own that
# --per-host-window gives each host under --mode recover, and the outages --coalesce-outage-gap
# makes the hosts of one call share, with its --probe-host; the counts by host that
# `.github/scripts/download-retry-proof.sh#report` makes of the requests the proxy logged as
# passed on and as faulted; for a run that faults one host, the window the rig's probe opens and
# the report's faulted-host and attempt checks; and for an image build, the attempt check
# `report` reads from a build log, the Dockerfiles' retry() attempt lines and curl's own
# `curl: (N)` lines.
#
# Nothing here touches the real internet. The host that is passed on is an origin this test
# starts, a `python3 -m http.server` on a loopback port, and the host that is faulted is either
# a name under .invalid, which RFC 2606 reserves to resolve nowhere, or that same origin under
# another of its names — `localhost` and `127.0.0.1` reach the same server and the proxy matches
# on the name the request carries, so the recover case can fault a host that still answers once
# the window closes.
#
# CONNECT is driven with `curl --proxytunnel`, which makes curl tunnel a plain-HTTP request
# through a CONNECT rather than speak TLS: it exercises the proxy's CONNECT path, which is what
# is under test, against an origin that needs no certificate.
#
# Every curl here clears no_proxy and NO_PROXY. A value of `*` in either makes curl ignore -x,
# and the test would then pass by never reaching the proxy at all. A GitHub runner sets neither;
# the pods this repository's agents run in set both.
#
# Every process this script starts is killed by its own pid in the EXIT trap, and everything it
# writes goes under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/ci-fault-proxy.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
proxy_script="$script_dir/ci-fault-proxy.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
pids=()
cleanup() {
  local pid
  for pid in ${pids+"${pids[@]}"}; do
    kill "$pid" 2> /dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

# The origin: one file, served on a loopback port the kernel picks. The port is written to a
# file rather than read out of what the server logs, as the proxy's own --port-file is: the
# banner `python3 -m http.server` prints reached its log only when the server exited on the
# runner, so this harness waited ten seconds and then said the origin reported no port (run
# 42353947192 of this repository, the `test` job, whose origin log was empty). A port written
# by the process that bound it cannot be buffered away and cannot be taken by a second process
# between the choice and the bind.
mkdir "$work/origin"
printf 'the origin answered\n' > "$work/origin/hello.txt"
cat > "$work/origin.py" <<'ORIGIN'
import functools, http.server, os, sys

directory, port_file = sys.argv[1], sys.argv[2]
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=directory)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
temporary = port_file + ".part"
with open(temporary, "w", encoding="utf-8") as handle:
    handle.write(f"{server.server_address[1]}\n")
os.replace(temporary, port_file)
server.serve_forever()
ORIGIN
python3 "$work/origin.py" "$work/origin" "$work/origin.port" > "$work/origin.log" 2>&1 &
pids+=("$!")
origin_port=""
for _ in $(seq 200); do
  if [ -s "$work/origin.port" ]; then
    origin_port=$(cat "$work/origin.port")
    break
  fi
  sleep 0.05
done
if [ -z "$origin_port" ]; then
  echo "  FAIL: the origin did not report a port" >&2
  cat "$work/origin.log" >&2
  exit 1
fi
echo "measured: the origin listens on 127.0.0.1:$origin_port"

# start_proxy <name> <arguments...>: starts the proxy on a free port, waits for its port file and
# sets $proxy and $proxy_port. Its pid joins the list the trap kills.
start_proxy() {
  local name=$1
  shift
  local port_file="$work/$name.port" pid port=""
  python3 "$proxy_script" --port 0 --port-file "$port_file" "$@" > "$work/$name.log" 2>&1 &
  pid=$!
  pids+=("$pid")
  for _ in $(seq 200); do
    if [ -s "$port_file" ]; then
      port=$(cat "$port_file")
      break
    fi
    kill -0 "$pid" 2> /dev/null || break
    sleep 0.05
  done
  if [ -z "$port" ]; then
    echo "  FAIL: the $name proxy did not report a port" >&2
    cat "$work/$name.log" >&2
    exit 1
  fi
  proxy_port=$port
  proxy="http://127.0.0.1:$port"
}

# run_curl <arguments...>: curl with this machine's no_proxy and NO_PROXY cleared, leaving its
# merged output in $out, its exit status in $status and its wall time in whole seconds in
# $seconds.
run_curl() {
  local start
  start=$(date +%s)
  set +e
  out=$(env no_proxy= NO_PROXY= curl --silent --show-error "$@" 2>&1)
  status=$?
  set -e
  seconds=$(($(date +%s) - start))
}

echo "case: --mode fail answers 504 to a faulted host, over plain HTTP and over CONNECT"
start_proxy fail --mode fail --hosts fetch.invalid
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: plain HTTP answered $out, curl exit $status"
check "a plain-HTTP request for a faulted host answers 504" "$(is "$out" 504)"
run_curl -x "$proxy" --proxytunnel http://fetch.invalid/tool.tar.gz
echo "  measured: CONNECT gave curl exit $status: $out"
check "a CONNECT for a faulted host is refused with 504, which curl reports as exit 56" "$(is "$status" 56)"
check "and curl's message names the 504 it got from the proxy" "$(contains "$out" '504')"
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin over plain HTTP" "$(is "$out" 'the origin answered')"
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and through CONNECT" "$(is "$out" 'the origin answered')"

echo "case: --mode recover answers 504 inside its window and passes the request on after it"
# A 10 s window, not the 2 s a reader might write: three loopback requests have to finish inside
# it, and a runner that stalls for a second between them would otherwise read the recovered
# answer and report the window as broken. The case costs 11 s and cannot race.
start_proxy recover --mode recover --recover-after 10 --hosts localhost
window_start=$(date +%s)
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' "http://localhost:$origin_port/hello.txt"
inside_plain=$out
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
inside_tunnel_status=$status
echo "  measured: $(($(date +%s) - window_start))s into the 10 s window, plain HTTP answered $inside_plain and CONNECT gave curl exit $inside_tunnel_status"
check "a plain-HTTP request inside the window answers 504" "$(is "$inside_plain" 504)"
check "a CONNECT inside the window is refused" "$(is "$inside_tunnel_status" 56)"
run_curl -x "$proxy" "http://127.0.0.1:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin inside the window" "$(is "$out" 'the origin answered')"
sleep 11
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
echo "  measured: $(($(date +%s) - window_start))s after the window opened, plain HTTP answered '$out' (curl exit $status)"
check "the same plain-HTTP request after the window reaches the origin" "$(is "$out" 'the origin answered')"
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and the same CONNECT reaches it too" "$(is "$out" 'the origin answered')"

# The recover case above holds no window to the proxy's start rather than to a host's first
# request: it asks for the faulted host at once, so its first request and the proxy's start fall
# in the same second, and a window of either kind answers it the same way. This case holds it,
# by asking for the host only after one window from the start has closed.
echo "case: without --per-host-window, --mode recover keeps one window from the start, which a host first reached after it never meets"
start_proxy oneshot --mode recover --recover-after 2 --hosts localhost
sleep 3
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
echo "  measured: 3 s after the proxy started, the faulted host's first request answered '$out' (curl exit $status)"
check "a faulted host first reached after the one window is passed on at its first request" \
  "$(is "$out" 'the origin answered')"
check "and the proxy logged no window of its own for it" \
  "$(is "$(contains "$(cat "$work/oneshot.log")" 'ci-fault-proxy: window ')" false)"

echo "case: --per-host-window gives a host first reached late a full window of its own, and never a second one"
# A 4 s window, and each reading at least a second clear of its edge: the host is first asked for
# 5 s after the proxy started, a second after one window from the start would have closed; then
# 2.5 s into its own window, 1.5 s before that window closes; then a second after it closed; and
# then once more after as long again as a window, where a second window would answer it.
start_proxy late --mode recover --recover-after 4 --per-host-window --hosts localhost
sleep 5
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' "http://localhost:$origin_port/hello.txt"
late_first=$out
sleep 2.5
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
late_inside_status=$status
late_opened=$(sed -n 's/^ci-fault-proxy: window localhost opened \([0-9.]*\)s after the proxy started$/\1/p' "$work/late.log")
echo "  measured: the first request, 5 s after the proxy started, answered $late_first; 2.5 s later a CONNECT gave curl exit $late_inside_status; the proxy logged the window opening at ${late_opened:-no second at all}s"
check "a host first asked for after one window from the start has closed answers 504 at its first request" \
  "$(is "$late_first" 504)"
check "and a CONNECT 2.5 s into its own 4 s window is refused too" "$(is "$late_inside_status" 56)"
check "and the proxy logged its window opening at that first request, more than 4 s after it started" \
  "$(awk -v s="${late_opened:-0}" 'BEGIN { print (s > 4 ? "true" : "false") }')"
sleep 2.5
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
echo "  measured: 5 s into the host's window, plain HTTP answered '$out' (curl exit $status)"
check "the same request after its window closed reaches the origin" "$(is "$out" 'the origin answered')"
run_curl -x "$proxy" "http://localhost:$origin_port/hello.txt"
late_again=$out
sleep 4.5
run_curl -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
echo "  measured: the next request answered '$late_again', and a CONNECT 4.5 s later '$out' (curl exit $status)"
check "and the request after that reaches it too: a closed window opens no second one" \
  "$(is "$late_again" 'the origin answered')"
check "and so does a CONNECT as long as a window after that" "$(is "$out" 'the origin answered')"
check "the proxy logged one window for the host, and only one" \
  "$(is "$(grep -c '^ci-fault-proxy: window ' "$work/late.log" || true)" 1)"
check "and its first line says it runs a window per host" \
  "$(contains "$(cat "$work/late.log")" '^ci-fault-proxy: mode=recover window=per-host bind=127\.0\.0\.1 ')"

echo "case: --per-host-window keeps each host's window its own, two hosts under one --hosts entry among them"
# Both hosts are names under fetch.invalid, the one entry, so the windows are told apart by the
# host a request names and not by the entry it matched. A name under .invalid resolves nowhere,
# so a request the proxy passes on is answered by the proxy itself with 502, as the rig's probe
# reads its unfaulted host: 504 is the fault, and 502 is the request passed on. b is first asked
# for 3 s into a's 4 s window, and the pair is read again 2 s later, a second after a's window
# closed and 2 s into b's own, b first since a's 502 waits on a resolver; and b once more 3 s
# after that, a second after its own window closed.
start_proxy hosts --mode recover --recover-after 4 --per-host-window --hosts fetch.invalid
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://a.fetch.invalid/tool.tar.gz
a_first=$out
sleep 3
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://b.fetch.invalid/tool.tar.gz
b_first=$out
sleep 2
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://b.fetch.invalid/tool.tar.gz
b_inside=$out
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://a.fetch.invalid/tool.tar.gz
a_after=$out
sleep 3
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://b.fetch.invalid/tool.tar.gz
b_after=$out
echo "  measured: a answered $a_first, then b 3 s later $b_first; 2 s on, b $b_inside and a $a_after; 3 s on again, b $b_after"
check "the first host's first request answers 504" "$(is "$a_first" 504)"
check "and the second host, first asked for while the first host's window is closing, answers 504" "$(is "$b_first" 504)"
check "while the second host, in its own window, still answers 504" "$(is "$b_inside" 504)"
check "and once the first host's window has closed, the first host is passed on" "$(is "$a_after" 502)"
check "and the second host is passed on once its own window has closed" "$(is "$b_after" 502)"
# Bounded, so a proxy that took the flag and served would end here rather than hold the test.
set +e
timeout 10 python3 "$proxy_script" --mode fail --per-host-window --hosts fetch.invalid > "$work/refused.out" 2>&1
refused=$?
set -e
check "--per-host-window is refused with a mode that has no window, as a usage error" \
  "$(is "$refused:$(contains "$(cat "$work/refused.out")" 'has no window')" 2:true)"

# The --coalesce-outage-gap cases below drive the proxy's Fault directly, in a python process of
# their own, on a clock this harness sets by hand, so each decision is read at the exact second
# it names and no case waits on a real clock. The driver builds the Fault the proxy builds for
# --mode recover --hosts fetch.invalid --recover-after 10 --coalesce-outage-gap 5, sets the
# clock to each <second> before that request, and prints a transcript: the lines the proxy
# logged, in the order it logged them, and after each request `at <second> <host>: 504` for a
# fault or `pass` for a request passed on. The clock starts at 0, so each line's seconds after
# the proxy started are the seconds the case names.
cat > "$work/coalesce.py" <<'COALESCE'
import importlib.util, sys

spec = importlib.util.spec_from_file_location("ci_fault_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)
sys.stderr = sys.stdout
now = 0.0
probe = None if sys.argv[2] == "-" else sys.argv[2]
fault = proxy.Fault("recover", ["fetch.invalid"], 10.0, coalesce_gap=5.0, probe_host=probe, clock=lambda: now)
for event in sys.argv[3:]:
    second, _, host = event.partition("=")
    now = float(second)
    print(f"at {second} {host}: {'504' if fault.faults(host) else 'pass'}")
COALESCE
# coalesce <probe host, or - for none> <second>=<host>...: the driver's transcript, in $out.
coalesce() {
  set +e
  out=$(python3 "$work/coalesce.py" "$proxy_script" "$@" 2>&1)
  set -e
}
# transcript <line>...: the lines as one text, to compare with $out.
transcript() { printf '%s\n' "$@"; }

echo "case: --coalesce-outage-gap: a host first reached while an outage is open joins it, answers 504 until that outage ends, and is passed on after"
# a opens outage 1 at 0 s, which ends at 10 s, and b, first reached 4 s in, joins it. A window of
# b's own would end at 14 s, so b's answer at 10 s tells the two apart; 9.9 s is inside outage 1.
coalesce - 0=a.fetch.invalid 4=b.fetch.invalid 9.9=b.fetch.invalid 10=b.fetch.invalid
indent "$out"
check "b's first request logs its first-request line and that it joined outage 1, and b answers 504 until 10 s and is passed on at 10 s" \
  "$(is "$out" "$(transcript \
    'ci-fault-proxy: window a.fetch.invalid opened 0.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 0.0s after the proxy started, ending 10.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 0 a.fetch.invalid: 504' \
    'ci-fault-proxy: window b.fetch.invalid opened 4.0s after the proxy started' \
    'ci-fault-proxy: host b.fetch.invalid joined outage 1' \
    'at 4 b.fetch.invalid: 504' \
    'at 9.9 b.fetch.invalid: 504' \
    'at 10 b.fetch.invalid: pass')")"

echo "case: --coalesce-outage-gap: a host first reached within the gap after an outage ended is passed on and named, and is not faulted later"
# Outage 1 ends at 10 s and the gap is 5 s, so c, first reached at 10 s exactly, and d, at 14.9 s,
# are in the gap. c is asked for again at 20 s, after the gap, where a host decided anew would
# open outage 2 and answer 504.
coalesce - 0=a.fetch.invalid 10=c.fetch.invalid 14.9=d.fetch.invalid 20=c.fetch.invalid
indent "$out"
check "c and d are passed on at their first requests and logged as after outage 1, within 5s, and c is passed on again at 20 s" \
  "$(is "$out" "$(transcript \
    'ci-fault-proxy: window a.fetch.invalid opened 0.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 0.0s after the proxy started, ending 10.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 0 a.fetch.invalid: 504' \
    'ci-fault-proxy: window c.fetch.invalid opened 10.0s after the proxy started' \
    'ci-fault-proxy: host c.fetch.invalid not faulted: after outage 1, within 5s' \
    'at 10 c.fetch.invalid: pass' \
    'ci-fault-proxy: window d.fetch.invalid opened 14.9s after the proxy started' \
    'ci-fault-proxy: host d.fetch.invalid not faulted: after outage 1, within 5s' \
    'at 14.9 d.fetch.invalid: pass' \
    'at 20 c.fetch.invalid: pass')")"

echo "case: --coalesce-outage-gap: the first host reached at or after the gap's end opens outage 2 and answers 504"
# d, in the gap at 14.9 s, does not move the gap's end: e, first reached at 15 s, the end of
# outage 1 and the 5 s gap exactly, opens outage 2, 10 s long from its request.
coalesce - 0=a.fetch.invalid 14.9=d.fetch.invalid 15=e.fetch.invalid
indent "$out"
check "e opens outage 2 at 15 s, joins it and answers 504" \
  "$(is "$out" "$(transcript \
    'ci-fault-proxy: window a.fetch.invalid opened 0.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 0.0s after the proxy started, ending 10.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 0 a.fetch.invalid: 504' \
    'ci-fault-proxy: window d.fetch.invalid opened 14.9s after the proxy started' \
    'ci-fault-proxy: host d.fetch.invalid not faulted: after outage 1, within 5s' \
    'at 14.9 d.fetch.invalid: pass' \
    'ci-fault-proxy: window e.fetch.invalid opened 15.0s after the proxy started' \
    'ci-fault-proxy: outage 2 opened 15.0s after the proxy started, ending 25.0s after' \
    'ci-fault-proxy: host e.fetch.invalid joined outage 2' \
    'at 15 e.fetch.invalid: 504')")"

echo "case: --coalesce-outage-gap: a host that joined outage 1 is passed on while outage 2 is open"
# a opened outage 1 and b joined it; e opens outage 2 at 15 s, and at 16 s, inside outage 2, a
# and b are asked for again.
coalesce - 0=a.fetch.invalid 4=b.fetch.invalid 15=e.fetch.invalid 16=a.fetch.invalid 16=b.fetch.invalid
indent "$out"
check "a and b are passed on at 16 s, and neither logs a line again" \
  "$(is "$out" "$(transcript \
    'ci-fault-proxy: window a.fetch.invalid opened 0.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 0.0s after the proxy started, ending 10.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 0 a.fetch.invalid: 504' \
    'ci-fault-proxy: window b.fetch.invalid opened 4.0s after the proxy started' \
    'ci-fault-proxy: host b.fetch.invalid joined outage 1' \
    'at 4 b.fetch.invalid: 504' \
    'ci-fault-proxy: window e.fetch.invalid opened 15.0s after the proxy started' \
    'ci-fault-proxy: outage 2 opened 15.0s after the proxy started, ending 25.0s after' \
    'ci-fault-proxy: host e.fetch.invalid joined outage 2' \
    'at 15 e.fetch.invalid: 504' \
    'at 16 a.fetch.invalid: pass' \
    'at 16 b.fetch.invalid: pass')")"

echo "case: --coalesce-outage-gap: a host off --hosts is passed on and opens no outage"
coalesce - 0=localhost 2=a.fetch.invalid
indent "$out"
check "localhost is passed on with no line, and a, at 2 s, opens outage 1" \
  "$(is "$out" "$(transcript \
    'at 0 localhost: pass' \
    'ci-fault-proxy: window a.fetch.invalid opened 2.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 2.0s after the proxy started, ending 12.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 2 a.fetch.invalid: 504')")"

echo "case: --coalesce-outage-gap: the --probe-host answers 504 at every request and opens no outage, so the first faulted host opens outage 1 at its own first request"
# The probe host is a name under fetch.invalid, as the rig's is. It is asked for at 0 s and again
# at 30 s, where a host in an outage opened at 0 s, or in its gap, would be passed on; a is first
# reached at 40 s.
coalesce download-retry-probe.fetch.invalid \
  0=download-retry-probe.fetch.invalid 30=download-retry-probe.fetch.invalid 40=a.fetch.invalid
indent "$out"
check "the probe host answers 504 both times with no line, and a opens outage 1 at 40 s" \
  "$(is "$out" "$(transcript \
    'at 0 download-retry-probe.fetch.invalid: 504' \
    'at 30 download-retry-probe.fetch.invalid: 504' \
    'ci-fault-proxy: window a.fetch.invalid opened 40.0s after the proxy started' \
    'ci-fault-proxy: outage 1 opened 40.0s after the proxy started, ending 50.0s after' \
    'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
    'at 40 a.fetch.invalid: 504')")"

echo "case: --coalesce-outage-gap in the proxy itself: its first line says so, its --probe-host answers 504 with no outage, and the first faulted host opens outage 1"
# --probe-host is given in capitals and the request names it in lower case, as the rig's probe
# does: the proxy compares the two lower-cased. The 60 s outage outlasts the case.
start_proxy coalesced --mode recover --recover-after 60 --coalesce-outage-gap 5 --hosts fetch.invalid \
  --probe-host Download-Retry-Probe.Fetch.Invalid
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://download-retry-probe.fetch.invalid/
coalesced_probe=$out
run_curl -x "$proxy" -o /dev/null -w '%{http_code}' http://a.fetch.invalid/tool.tar.gz
coalesced_first=$out
run_curl -x "$proxy" "http://127.0.0.1:$origin_port/hello.txt"
coalesced_pass=$out
coalesced_log=$(cat "$work/coalesced.log")
indent "$coalesced_log"
check "its first line says it runs coalesced outages, with the gap" \
  "$(contains "$coalesced_log" '^ci-fault-proxy: mode=recover window=coalesced gap=5s bind=127\.0\.0\.1 ')"
check "the probe host answers 504, and so does a.fetch.invalid after it" \
  "$(is "$coalesced_probe:$coalesced_first" 504:504)"
check "and a host off --hosts reaches the origin" "$(is "$coalesced_pass" 'the origin answered')"
check "the probe's one line is its 504, and outage 1 opens at a.fetch.invalid's first request, its lines before that request's 504" \
  "$(is "$(sed -n -E '/^ci-fault-proxy: (window|outage|host|504|pass) /{s/ [0-9]+\.[0-9]s after/ <t>s after/g;p;}' <<<"$coalesced_log")" \
    "$(transcript \
      'ci-fault-proxy: 504 GET http://download-retry-probe.fetch.invalid/' \
      'ci-fault-proxy: window a.fetch.invalid opened <t>s after the proxy started' \
      'ci-fault-proxy: outage 1 opened <t>s after the proxy started, ending <t>s after' \
      'ci-fault-proxy: host a.fetch.invalid joined outage 1' \
      'ci-fault-proxy: 504 GET http://a.fetch.invalid/tool.tar.gz' \
      "ci-fault-proxy: pass GET http://127.0.0.1:$origin_port/hello.txt")")"
check "no handler failed" "$(is "$(contains "$coalesced_log" 'handler failed')" false)"

echo "case: --coalesce-outage-gap and --probe-host are refused where they mean nothing, as usage errors"
# run_proxy <arguments...>: the proxy run with them in the foreground, bounded, so a proxy that
# took them and served ends here; its exit status in $status and its output in $out.
run_proxy() {
  set +e
  out=$(timeout 10 python3 "$proxy_script" "$@" 2>&1)
  status=$?
  set -e
}
run_proxy --mode recover --per-host-window --coalesce-outage-gap 5 --hosts fetch.invalid
check "--coalesce-outage-gap with --per-host-window" \
  "$(is "$status:$(contains "$out" '^ci-fault-proxy\.py: error: .*--per-host-window')" 2:true)"
run_proxy --mode fail --coalesce-outage-gap 5 --hosts fetch.invalid
check "--coalesce-outage-gap with a mode that has no window" \
  "$(is "$status:$(contains "$out" '^ci-fault-proxy\.py: error: .*--mode fail')" 2:true)"
run_proxy --mode recover --coalesce-outage-gap 5
check "--coalesce-outage-gap with no --hosts" \
  "$(is "$status:$(contains "$out" '^ci-fault-proxy\.py: error: .*--hosts')" 2:true)"
run_proxy --mode recover --coalesce-outage-gap 0 --hosts fetch.invalid
check "--coalesce-outage-gap of 0 s" \
  "$(is "$status:$(contains "$out" '^ci-fault-proxy\.py: error: .*\b0$')" 2:true)"
run_proxy --mode recover --hosts fetch.invalid --probe-host download-retry-probe.fetch.invalid
check "--probe-host without --coalesce-outage-gap" \
  "$(is "$status:$(contains "$out" '^ci-fault-proxy\.py: error: .*--coalesce-outage-gap')" 2:true)"

echo "case: --mode stall sends nothing, so each attempt ends at the client's own bound"
start_proxy stall --mode stall --hosts fetch.invalid
# The body file is created here, not left to curl: a curl that receives nothing writes no file
# at all, and a check that a file which was never created is empty proves nothing.
: > "$work/stall.body"
run_curl --max-time 2 -o "$work/stall.body" -x "$proxy" http://fetch.invalid/tool.tar.gz
echo "  measured: plain HTTP ended after ${seconds}s with curl exit $status, $(wc -c < "$work/stall.body") bytes from the proxy: $out"
check "a stalled plain-HTTP request ends at curl's --max-time, which curl reports as exit 28" "$(is "$status" 28)"
check "and it took about the 2 s bound" "$([ "$seconds" -ge 2 ] && [ "$seconds" -le 5 ] && echo true || echo false)"
check "and the proxy sent nothing at all" "$([ ! -s "$work/stall.body" ] && echo true || echo false)"
run_curl --max-time 2 -x "$proxy" --proxytunnel http://fetch.invalid/tool.tar.gz
echo "  measured: CONNECT ended after ${seconds}s with curl exit $status: $out"
check "a stalled CONNECT ends at the same bound" "$(is "$status" 28)"
check "and it took about the 2 s bound too" "$([ "$seconds" -ge 2 ] && [ "$seconds" -le 5 ] && echo true || echo false)"
run_curl --max-time 10 -x "$proxy" "http://localhost:$origin_port/hello.txt"
check "a host outside --hosts reaches the origin over plain HTTP" "$(is "$out" 'the origin answered')"
run_curl --max-time 10 -x "$proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
check "and through CONNECT" "$(is "$out" 'the origin answered')"

echo "case: --bind listens on another address, which is how a docker build reaches the proxy"
# 127.0.0.2 stands in for the docker bridge gateway: a second address of this machine that the
# default 127.0.0.1 does not answer on, so the case proves both halves of the option — the proxy
# serves where --bind says, and nowhere else. Every Linux kernel routes the whole 127.0.0.0/8 to
# loopback, so this needs no interface of its own.
start_proxy bind --mode fail --hosts fetch.invalid --bind 127.0.0.2
run_curl -x "http://127.0.0.2:$proxy_port" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: the proxy on 127.0.0.2:$proxy_port answered $out, curl exit $status"
check "a request to the bound address is served" "$(is "$out" 504)"
run_curl --max-time 5 -x "http://127.0.0.1:$proxy_port" -o /dev/null -w '%{http_code}' http://fetch.invalid/tool.tar.gz
echo "  measured: the same port on 127.0.0.1 gave curl exit $status: $out"
check "and 127.0.0.1 is not listening, which curl reports as exit 7" "$(is "$status" 7)"
check "the proxy's first line names the address it bound" \
  "$(contains "$(cat "$work/bind.log")" "^ci-fault-proxy: mode=fail bind=127\.0\.0\.2 port=$proxy_port ")"

echo "case: the proxy logs what it did, and fetched nothing of its own"
fail_log=$(cat "$work/fail.log")
indent "$fail_log"
check "it logged the 504 it answered to the plain-HTTP request" \
  "$(contains "$fail_log" '^ci-fault-proxy: 504 GET http://fetch.invalid/tool.tar.gz$')"
check "and the 504 it answered to the CONNECT" \
  "$(contains "$fail_log" '^ci-fault-proxy: 504 CONNECT fetch.invalid:80$')"
check "and the request it passed on" \
  "$(contains "$fail_log" "^ci-fault-proxy: pass GET http://localhost:$origin_port/hello.txt\$")"
check "no handler failed" "$(is "$(contains "$fail_log" 'handler failed')" false)"

echo "case: download-retry-proof.sh's report counts the requests the proxy passed on by host"
# The rig the download-retry jobs run, driven whole over the origin above: `start` runs the proxy
# behind its stamping loop, `probe` reads its pair before the site, and `report` reads the log
# back. The site is the origin under two of its names, 127.0.0.1 twice over plain HTTP and
# localhost once through CONNECT, so the log holds passes to two hosts in both of the line shapes
# the proxy writes. --mode fail, faulting a host the site never asks for, so the report's own
# check of its mode passes and this case reads the count. RUNNER_TEMP is the rig's state
# directory, under $work, and GITHUB_OUTPUT is emptied, so a run on a runner writes nothing into
# its own step's outputs; no_proxy and NO_PROXY are cleared for the reason run_curl gives.
rig() { env RUNNER_TEMP="$work/rig" GITHUB_OUTPUT= no_proxy= NO_PROXY= "$script_dir/download-retry-proof.sh" "$@"; }
rig_state="$work/rig/download-retry/tally.state"
set +e
rig start tally --mode fail --hosts fetch.invalid > "$work/rig-start.out" 2>&1
rig_start=$?
set -e
[ ! -f "$rig_state" ] || pids+=("$(sed -n 's/^pid=//p' "$rig_state")")
indent "$(cat "$work/rig-start.out")"
check "the rig's start exits 0" "$(is "$rig_start" 0)"
set +e
rig probe tally > "$work/rig-probe.out" 2>&1
rig_probe=$?
set -e
indent "$(cat "$work/rig-probe.out")"
check "and its probe reads the pair as it must" "$(is "$rig_probe" 0)"
rig_proxy=$(sed -n 's/^proxy=//p' "$rig_state" 2> /dev/null || true)
run_curl -x "$rig_proxy" "http://127.0.0.1:$origin_port/hello.txt"
run_curl -x "$rig_proxy" "http://127.0.0.1:$origin_port/hello.txt"
run_curl -x "$rig_proxy" --proxytunnel "http://localhost:$origin_port/hello.txt"
# The stamping loop writes a line a moment after the answer it belongs to, so the three passes
# are waited for rather than raced, as the probe waits for its own.
for _ in $(seq 100); do
  [ "$(grep -c 'ci-fault-proxy: pass ' "$work/rig/download-retry/tally.log" 2> /dev/null || true)" -lt 3 ] || break
  sleep 0.05
done
set +e
rig_out=$(rig report tally --what "the by-host case" --outcome success --attempts 1 --floor 0 --budget 600 2>&1)
rig_report=$?
set -e
indent "$rig_out"
check "the report exits 0" "$(is "$rig_report" 0)"
check "it counts the requests the proxy passed on, and says a count by host follows" \
  "$(contains "$rig_out" '^  requests the proxy passed on: 3, by host:$')"
check "and the two lines after it name both hosts with their own counts, the CONNECT's port taken off" \
  "$(is "$(sed -n '/^  requests the proxy passed on: 3, by host:$/{n;p;n;p;}' <<<"$rig_out")" \
    "$(printf '    2 127.0.0.1\n    1 localhost')")"

echo "case: download-retry-proof.sh passes --per-host-window on, and its report counts the requests the proxy faulted by host, with each host's first request"
# The rig again, under --mode recover with a window per host, the 60 s `start` gives by default,
# which nothing here outlasts. The site asks for two faulted hosts, a.fetch.invalid twice, over
# plain HTTP and through CONNECT, and localhost once, and for 127.0.0.1, off the list, once: the
# one pass the report's recover check asks for.
rig_state="$work/rig/download-retry/perhost.state"
set +e
rig start perhost --mode recover --per-host-window --hosts fetch.invalid,localhost > "$work/rig-perhost-start.out" 2>&1
rig_start=$?
set -e
[ ! -f "$rig_state" ] || pids+=("$(sed -n 's/^pid=//p' "$rig_state")")
indent "$(cat "$work/rig-perhost-start.out")"
check "the rig's start exits 0, and says it passed the flag on" \
  "$(is "$rig_start:$(contains "$(cat "$work/rig-perhost-start.out")" '(--mode recover --per-host-window, faulting fetch.invalid,localhost)$')" 0:true)"
check "and the proxy it started runs a window per host" \
  "$(contains "$(cat "$work/rig/download-retry/perhost.log")" ' ci-fault-proxy: mode=recover window=per-host ')"
set +e
rig probe perhost > "$work/rig-perhost-probe.out" 2>&1
rig_probe=$?
set -e
indent "$(cat "$work/rig-perhost-probe.out")"
check "and its probe reads the pair as it must" "$(is "$rig_probe" 0)"
check "and prints the 504 as the proxy's line for its faulted request, a name under the first entry, not the line of the window that request opened" \
  "$(contains "$(cat "$work/rig-perhost-probe.out")" "^  the proxy's own line for it: [0-9]* ci-fault-proxy: 504 GET http://download-retry-probe\.fetch\.invalid/\$")"
rig_proxy=$(sed -n 's/^proxy=//p' "$rig_state" 2> /dev/null || true)
run_curl -x "$rig_proxy" http://a.fetch.invalid/tool.tar.gz
run_curl -x "$rig_proxy" --proxytunnel http://a.fetch.invalid/tool.tar.gz
run_curl -x "$rig_proxy" "http://localhost:$origin_port/hello.txt"
run_curl -x "$rig_proxy" "http://127.0.0.1:$origin_port/hello.txt"
# The probe's own refusal and the site's three, and the site's one pass, waited for as above.
for _ in $(seq 100); do
  [ "$(grep -c 'ci-fault-proxy: 504 ' "$work/rig/download-retry/perhost.log" 2> /dev/null || true)" -lt 4 ] ||
    [ "$(grep -c 'ci-fault-proxy: pass ' "$work/rig/download-retry/perhost.log" 2> /dev/null || true)" -lt 1 ] || break
  sleep 0.05
done
set +e
rig_out=$(rig report perhost --what "the faulted-by-host case" --outcome success --attempts 1 --floor 0 --budget 600 2>&1)
rig_report=$?
set -e
indent "$rig_out"
check "the report exits 0" "$(is "$rig_report" 0)"
faulted_header='^  requests the proxy faulted: 3, by host, each with its first request in seconds after the proxy started:$'
check "it counts the requests the proxy faulted, and says a count by host with each first request follows" \
  "$(contains "$rig_out" "$faulted_header")"
check "and the first line after it names a.fetch.invalid, faulted twice, the CONNECT's port taken off, with the second its window opened" \
  "$(contains "$(sed -n "/$faulted_header/{n;p;}" <<<"$rig_out")" '^    2 a\.fetch\.invalid, first request at [0-9][0-9]*\.[0-9] s$')"
check "and the second names localhost, faulted once, with its own" \
  "$(contains "$(sed -n "/$faulted_header/{n;n;p;}" <<<"$rig_out")" '^    1 localhost, first request at [0-9][0-9]*\.[0-9] s$')"

echo "case: a run that faults one host: the probe opens no window the site meets, and the report checks the host and the attempts"
# The rig as the download-retry job's sites runs use it: --per-host-window with one host in
# --hosts, fetch.invalid, and --attempt-log. The probe's faulted request names
# download-retry-probe.fetch.invalid, so the window it opens is that name's and fetch.invalid's
# opens on the site's own first request. The site asks fetch.invalid once, refused, and 127.0.0.1
# once, passed on. The attempt log is written here as the retry helpers write it under
# CI_RETRY_ATTEMPT_LOG (.github/scripts/ci-retry.test.sh and ci-tools.test.sh hold the helpers to
# it), and each report below reads the same proxy log against another --faulted or another log.
rig_state="$work/rig/download-retry/onehost.state"
onehost_log="$work/rig/download-retry/onehost.log"
onehost_attempts="$work/rig/download-retry/onehost.attempts"
set +e
env RUNNER_TEMP="$work/rig" GITHUB_OUTPUT="$work/onehost.output" no_proxy= NO_PROXY= \
  "$script_dir/download-retry-proof.sh" start onehost --mode recover --per-host-window --hosts fetch.invalid --attempt-log \
  > "$work/rig-onehost-start.out" 2>&1
rig_start=$?
set -e
[ ! -f "$rig_state" ] || pids+=("$(sed -n 's/^pid=//p' "$rig_state")")
indent "$(cat "$work/rig-onehost-start.out")"
check "the rig's start exits 0, and writes the attempt log it names to the step's outputs" \
  "$(is "$rig_start:$(grep -c "^attempt_log=$onehost_attempts\$" "$work/onehost.output" 2> /dev/null || true)" 0:1)"
set +e
rig probe onehost > "$work/rig-onehost-probe.out" 2>&1
rig_probe=$?
set -e
indent "$(cat "$work/rig-onehost-probe.out")"
check "and its probe reads the pair as it must" "$(is "$rig_probe" 0)"
check "its faulted request names download-retry-probe.fetch.invalid, which the entry covers" \
  "$(contains "$(cat "$work/rig-onehost-probe.out")" "^  the proxy's own line for it: [0-9]* ci-fault-proxy: 504 GET http://download-retry-probe\.fetch\.invalid/\$")"
check "and the window it opened is that name's, with none yet for fetch.invalid" \
  "$(is "$(contains "$(cat "$onehost_log")" ' window download-retry-probe\.fetch\.invalid opened '):$(contains "$(cat "$onehost_log")" ' window fetch\.invalid opened ')" true:false)"
rig_proxy=$(sed -n 's/^proxy=//p' "$rig_state" 2> /dev/null || true)
run_curl -x "$rig_proxy" http://fetch.invalid/tool.tar.gz
run_curl -x "$rig_proxy" "http://127.0.0.1:$origin_port/hello.txt"
# The probe's own refusal and the site's one, and the site's one pass, waited for as above.
for _ in $(seq 100); do
  [ "$(grep -c 'ci-fault-proxy: 504 ' "$onehost_log" 2> /dev/null || true)" -lt 2 ] ||
    [ "$(grep -c 'ci-fault-proxy: pass ' "$onehost_log" 2> /dev/null || true)" -lt 1 ] || break
  sleep 0.05
done
check "the site's own first request for fetch.invalid opened that host's window" \
  "$(contains "$(cat "$onehost_log")" ' window fetch\.invalid opened ')"
# onehost_report <attempt log lines, or - for none> <--faulted host>: the report against that
# log, its output in rig_out and its exit in rig_report.
onehost_report() {
  rm -f "$onehost_attempts"
  [ "$1" = - ] || printf '%s\n' "$1" > "$onehost_attempts"
  set +e
  rig_out=$(rig report onehost --what "the one-host case" --outcome success --attempts 5 --floor 0 --budget 600 --faulted "$2" 2>&1)
  rig_report=$?
  set -e
  indent "$rig_out"
}
onehost_report "$(printf 'attempt 1 exit 22\nattempt 2 exit 0')" fetch.invalid
check "a site that met its one host's fault and passed on attempt 2 of 5 passes the report" "$(is "$rig_report" 0)"
check "which counts the site's own refused requests for that host apart" \
  "$(contains "$rig_out" '^  requests of this site for fetch\.invalid, the one host this run faults, that the proxy refused: 1$')"
check "and prints the attempt lines the site's call logged" \
  "$(is "$(sed -n "/^  the attempts the site's call logged, of the 5 it may make:\$/{n;p;n;p;}" <<<"$rig_out")" "$(printf '    attempt 1 exit 22\n    attempt 2 exit 0')")"
onehost_report "$(printf 'attempt 1 exit 22\nattempt 2 exit 0')" download-retry-probe.fetch.invalid
check "a faulted host only the probe asked for fails the faulted-host check, the probe's request not counted" \
  "$(is "$rig_report:$(contains "$rig_out" '::error::download-retry: the one-host case: the faulted-host check: none of this site.s own requests for download-retry-probe\.fetch\.invalid was refused')" 1:true)"
onehost_report "$(printf 'attempt 1 exit 22\nattempt 2 exit 22\nattempt 3 exit 22\nattempt 4 exit 22\nattempt 5 exit 0')" fetch.invalid
check "a call that passed on its last attempt fails the attempt check" \
  "$(is "$rig_report:$(contains "$rig_out" '::error::download-retry: the one-host case: the attempt check: the call reached attempt 5 of the 5 it may make, its last')" 1:true)"
onehost_report 'attempt 1 exit 0' fetch.invalid
check "and so does a call that passed on its first attempt, with no retry" \
  "$(is "$rig_report:$(contains "$rig_out" '::error::download-retry: the one-host case: the attempt check: the call passed with no retry, on its first attempt')" 1:true)"
onehost_report - fetch.invalid
check "and so does a site that logged no attempt at all" \
  "$(is "$rig_report:$(contains "$rig_out" '::error::download-retry: the one-host case: the attempt check: the attempt log .* holds no attempt that passed')" 1:true)"

echo "case: an image build's log: the report reads each retried call's attempt lines and each RUN's curl lines, and checks them"
# The rig as the download-retry-images jobs use it, without the build: `report --build-log`
# reads a log written here as BuildKit's plain progress writes one. A RUN's header is
# `#<step> [<stage> k/m] RUN <command>`, the command the Dockerfile's text with its continuations
# joined, and each line the RUN prints is `#<step> <seconds> <text>`. The proxy faults
# fetch.invalid under --per-host-window; the site asks it once, refused, and 127.0.0.1 once,
# passed on, so the recover mode's own checks pass and each case reads the attempt check alone.
rig_state="$work/rig/download-retry/image.state"
image_log="$work/rig/download-retry/image.log"
image_build_log="$work/image.build.log"
set +e
rig start image --mode recover --per-host-window --hosts fetch.invalid > "$work/rig-image-start.out" 2>&1
rig_start=$?
set -e
[ ! -f "$rig_state" ] || pids+=("$(sed -n 's/^pid=//p' "$rig_state")")
indent "$(cat "$work/rig-image-start.out")"
check "the rig's start exits 0" "$(is "$rig_start" 0)"
set +e
rig probe image > "$work/rig-image-probe.out" 2>&1
rig_probe=$?
set -e
indent "$(cat "$work/rig-image-probe.out")"
check "and its probe reads the pair as it must" "$(is "$rig_probe" 0)"
rig_proxy=$(sed -n 's/^proxy=//p' "$rig_state" 2> /dev/null || true)
run_curl -x "$rig_proxy" http://fetch.invalid/tool.tar.gz
run_curl -x "$rig_proxy" "http://127.0.0.1:$origin_port/hello.txt"
# The probe's own refusal and the site's one, and the site's one pass, waited for as above.
for _ in $(seq 100); do
  [ "$(grep -c 'ci-fault-proxy: 504 ' "$image_log" 2> /dev/null || true)" -lt 2 ] ||
    [ "$(grep -c 'ci-fault-proxy: pass ' "$image_log" 2> /dev/null || true)" -lt 1 ] || break
  sleep 0.05
done
# image_report <build log line>...: the report against a build log of those lines, its output in
# rig_out, its exit in rig_report and its ::error:: lines in rig_errors.
image_report() {
  printf '%s\n' "$@" > "$image_build_log"
  set +e
  rig_out=$(rig report image --what "the image case" --outcome success --attempts 5 --floor 0 --budget 600 \
    --build-log "$image_build_log" 2>&1)
  rig_report=$?
  set -e
  rig_errors=$(sed -n '/^::error::/p' <<<"$rig_out")
  indent "$rig_out"
}
# The header of a RUN that opens with the canonical retry(), as BuildKit prints it: its command
# holds the attempt line's echo, `$i` and all. A RUN is named in a refusal by the first 80
# characters of its header, which hold no URL here, so none is taken out.
apt_header='#12 [build 2/5] RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; echo "retry: $what: attempt $i of 5 failed" >&2; [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }     && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any'
apt_run=${apt_header:0:80}
# apt_attempts <k>: the apt-get update call's attempt lines for attempts 1 to k, each failed.
apt_attempts() {
  local k
  for k in $(seq "$1"); do printf '#12 %d.5 retry: apt-get update: attempt %d of 5 failed\n' "$((k * 10))" "$k"; done
}

image_report '#1 [internal] load build definition from worker.Dockerfile' '#1 DONE 0.0s' "$apt_header" \
  '#12 0.512 E: Failed to fetch http://deb.debian.org/debian/dists/trixie/InRelease  504  Gateway Timeout' \
  "$(apt_attempts 3)" '#12 71.20 Get:1 http://deb.debian.org/debian trixie InRelease [140 kB]' '#12 DONE 75.3s'
check "a call whose highest failed attempt is 3 of 5 passes the report" "$(is "$rig_report:$rig_errors" 0:)"
check "which prints the call, and that call alone, with its step, its stage and the attempt it reached, 4 of 5, after 3 failed" \
  "$(is "$(sed -n '/^  the retried calls of .* that failed an attempt, /,/^  the `curl: (N)` lines of each RUN, /p' <<<"$rig_out" | sed '1d;$d')" \
    '    #12 (build 2/5) apt-get update: attempt 4 of 5 reached, 3 failed')"

image_report "$apt_header" "$(apt_attempts 4)" '#12 151.2 Get:1 http://deb.debian.org/debian trixie InRelease [140 kB]'
check "a call whose attempt 4 of 5 failed fails the attempt check, naming the call, its step and its RUN" \
  "$(is "$rig_report:$rig_errors" "1:::error::download-retry: the image case: the attempt check: apt-get update in #12 (build 2/5) failed attempt 4 of 5, so attempt 5 of 5, its last, ran: it passed with no attempt to spare or ran out; its RUN: $apt_run")"

image_report "$apt_header" "$(apt_attempts 5)" '#12 300.9 apt-get update failed after 5 attempts (last exit 100)' \
  '#12 ERROR: process "/bin/sh -c retry() { ..." did not complete successfully: exit code: 100'
check "a call that printed \`failed after 5 attempts\` fails the attempt check by that line, beside its attempt 5 of 5" \
  "$(is "$rig_report:$rig_errors" "1:$(printf '%s\n' \
    "::error::download-retry: the image case: the attempt check: apt-get update in #12 (build 2/5) failed attempt 5 of 5, its last, so it ran out; its RUN: $apt_run" \
    "::error::download-retry: the image case: the attempt check: apt-get update in #12 (build 2/5) failed after 5 attempts, its ladder run out; its RUN: $apt_run")")"

image_report "$apt_header" '#12 DONE 4.2s'
check "a RUN header that holds the attempt line's echo is not read as an attempt, so a log of it alone holds none and fails" \
  "$(is "$rig_report:$rig_errors" '1:::error::download-retry: the image case: the attempt check: the build log holds no `retry: <what>: attempt <k> of <n> failed` line and no `curl: (N)` line at all, so the fault reached no retried call of this build')"
image_report "$apt_header" 'retry: apt-get update: attempt 4 of 5 failed' \
  "#12 0.4 + echo 'retry: apt-get update: attempt 4 of 5 failed'" "$(apt_attempts 1)"
check "nor is a line with no step prefix or the shell's xtrace of the echo: the call reads attempt 1 of 5 failed and passes" \
  "$(is "$rig_report:$rig_errors:$(sed -n '/^  the retried calls of .* that failed an attempt, /{n;p;}' <<<"$rig_out")" \
    '0::    #12 (build 2/5) apt-get update: attempt 2 of 5 reached, 1 failed')"

image_report '#1 [internal] load build definition from worker.Dockerfile' '#1 DONE 0.0s' "$apt_header" \
  '#12 3.1 Get:1 http://deb.debian.org/debian trixie InRelease [140 kB]' '#12 DONE 4.2s'
check "a log with no attempt line and no curl line at all fails: the fault reached no retried call" \
  "$(is "$rig_report:$rig_errors" '1:::error::download-retry: the image case: the attempt check: the build log holds no `retry: <what>: attempt <k> of <n> failed` line and no `curl: (N)` line at all, so the fault reached no retried call of this build')"

# A RUN of curl's own `--retry 7` reads, as the worker image's notices stage holds them: each
# failed attempt prints one `curl: (N)` line and no attempt line follows it.
notices_header='#20 [notices 3/4] RUN set -eu;     fetch() { curl -fsSL --retry 7 --retry-all-errors --max-time 60 "$(gh_raw_url "$1" "$2" "$3")" -o "/in/$4"; }'
notices_run=${notices_header:0:80}
# curl_lines <step> <count>: that many `curl: (22)` lines of the step.
curl_lines() {
  local k
  for k in $(seq "$2"); do printf '#%s %d.1 curl: (22) The requested URL returned error: 504\n' "$1" "$k"; done
}
image_report "$notices_header" "$(curl_lines 20 7)" '#20 DONE 130.4s'
check "a RUN with 7 \`curl: (N)\` lines that no attempt line follows fails the attempt check, naming the RUN" \
  "$(is "$rig_report:$rig_errors" "1:::error::download-retry: the image case: the attempt check: #20 (notices 3/4) printed 7 \`curl: (N)\` lines that no retry() attempt line accounts for, and curl --retry 7 prints one for each failed attempt of its 8, so a curl call of that RUN may have reached attempt 8 of 8, its last (read by RUN, which is exact where one curl call of the RUN meets the fault and fails closed where several do); its RUN: $notices_run")"
image_report "$notices_header" "$(curl_lines 20 6)" '#20 DONE 66.4s'
check "and one with 6 passes, its count printed" \
  "$(is "$rig_report:$rig_errors:$(grep -c '^    #20 (notices 3/4): 6 counted toward the 7, and 0 that retry() attempt lines account for$' <<<"$rig_out")" 0::1)"

# A RUN of seven curls each wrapped in retry(), as the worker image's toolchain stage writes
# them: each failed attempt prints curl's `curl: (N)` line and then retry()'s attempt line, the
# shell's xtrace between the two where the RUN runs under `set -x`. Each `what` names the URL
# it fetches, which the report prints as <url>.
toolchain_header='#14 [toolchain 3/9] RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?;'
toolchain_lines=()
for tool in uv node awscli go kubectl yq chromium; do
  toolchain_lines+=("#14 1.0 curl: (56) CONNECT tunnel failed, response 504" "#14 1.0 + s=56" \
    "#14 1.0 + echo 'retry: $tool download from https://fetch.invalid/$tool.tar.gz: attempt 1 of 5 failed'" \
    "#14 1.0 retry: $tool download from https://fetch.invalid/$tool.tar.gz: attempt 1 of 5 failed")
done
image_report "$toolchain_header" "${toolchain_lines[@]}" '#14 DONE 98.0s'
check "7 \`curl: (N)\` lines each followed by a retry() attempt line do not count toward the 7, and the RUN passes" \
  "$(is "$rig_report:$rig_errors:$(grep -c '^    #14 (toolchain 3/9): 0 counted toward the 7, and 7 that retry() attempt lines account for$' <<<"$rig_out")" 0::1)"
check "and each call is printed with the URL its \`what\` names taken out" \
  "$(is "$(grep -c '^    #14 (toolchain 3/9) [a-z]* download from <url>: attempt 2 of 5 reached, 1 failed$' <<<"$rig_out"):$(contains "$rig_out" 'download from https://')" 7:false)"

summary "ci-fault-proxy.py and download-retry-proof.sh's counts by host"
