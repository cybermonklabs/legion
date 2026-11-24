#!/usr/bin/env bash
# Tests for fetch.sh: a download that a server answers is fetched once; one that fails is tried
# twice more and recovers; one that fails every attempt fails by name with curl's exit status and
# the last HTTP status; a body that stalls is cut at the bound, and no attempt starts once twice the
# bound has passed; a stall is cut 30 s in when the bound is longer, while a body that keeps moving
# above the floor is not; a Retry-After that would wait past twice the bound ends the retries and
# fails by name whatever curl exits; a curl that exits 0 with a 2xx and writes no file fails by
# name; a 200 with an empty body fails by name and leaves no file; and a call without a stage, a
# bound, an address or an -o file is refused.
#
# The cases run the real script against a stand-in server on 127.0.0.1, a small Python script in
# this harness's own temporary directory. The path of each request says what the stand-in answers,
# and each case asks a path of its own, so the stand-in's log counts each case's requests apart:
#   /ok/<case>                 200 and the body;
#   /503-once/<case>           503 the first time, then 200 and the body;
#   /503-always/<case>         503 every time;
#   /404/<case>                404 every time;
#   /stall-once/<case>         the first time, 200 with the body's length, five of its bytes and
#                              then nothing; then 200 and the body;
#   /stall-always/<case>       that stall every time;
#   /slow/<case>               200 and SLOW_BYTES bytes at 20,000 bytes a second, twice the stall
#                              cut's floor, for 32 s, past its 30 s window;
#   /retry-after-long/<case>   503 with `Retry-After: 100`;
#   /empty/<case>              200 with an empty body.
# A case whose server ends each attempt by itself runs with a bound of ENDS (30 s), far above the
# time it takes, so a slow runner cannot cut it; a case whose server stalls runs with STALLS (2 s),
# so it costs seconds; the stall cut's two cases run with CUTS (120 s), a bound the case's
# deadline ends first, so only the stall cut's 30 s can end the stalled attempt in time. Each call
# runs under a deadline of its own (DEADLINE): a fetch that never ends fails its case by name
# rather than holding the test job until its limit. How long a case took is no assertion; how many
# requests the stand-in saw is.
#
# Run from anywhere: .github/scripts/fetch.test.sh
# CI runs it in ci.yaml's test job, under that runner's own curl alone (ubuntu-24.04-arm's
# 8.5.0). The worker image's stages run two other curls, bookworm's 7.88.1 (the tools and notices
# stages) and trixie's 8.14.1 (the toolchain and runtime-base stages), and they differ where the
# Retry-After case looks: 8.14.1 exits 0 there. So a change of fetch.sh runs this test by hand
# under each as well, in the image the stage builds FROM with apt's curl and python3. For trixie:
#   docker run --rm -v "$PWD:/src" -w /src ghcr.io/cybermonklabs/legion/ci@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 \
#     sh -c 'apt-get update && apt-get install -y --no-install-recommends ca-certificates curl python3 && .github/scripts/fetch.test.sh'
# and for bookworm the same with the tools stage's image,
# ghcr.io/cybermonklabs/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/fetch.sh"
work=$(mktemp -d)
server_pid=
trap '[ -z "$server_pid" ] || kill "$server_pid" 2> /dev/null || true; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

ENDS=30
STALLS=2
CUTS=120
DEADLINE=60
BODY="the pinned bytes"
SLOW_BYTES=640000

cat > "$work/standin.py" <<'PY'
import http.server
import sys
import threading
import time

BODY = b"the pinned bytes\n"
SLOW_BYTES = int(sys.argv[2])
log = open(sys.argv[1], "a", buffering=1)
seen = {}
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    def answer(self, with_body):
        mode = self.path.split("/")[1]
        with lock:
            earlier = seen.get(self.path, 0)
            seen[self.path] = earlier + 1
        if mode == "ok" or (mode in ("503-once", "stall-once") and earlier):
            status = 200
        elif mode == "empty":
            status = "empty"
        elif mode == "slow":
            status = "slow"
        elif mode in ("stall-once", "stall-always"):
            status = "stall"
        elif mode == "404":
            status = 404
        else:
            status = 503
        log.write(f"{self.command} {self.path} {status}\n")
        if status == "stall":
            self.send_response(200)
            self.send_header("Content-Length", str(len(BODY)))
            self.end_headers()
            if with_body:
                self.wfile.write(BODY[:5])
                self.wfile.flush()
            time.sleep(120)
            return
        if status == "empty":
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if status == "slow":
            # 2,000 bytes every tenth of a second: 20,000 bytes a second, until SLOW_BYTES are sent.
            self.send_response(200)
            self.send_header("Content-Length", str(SLOW_BYTES))
            self.end_headers()
            sent = 0
            while with_body and sent < SLOW_BYTES:
                chunk = min(2000, SLOW_BYTES - sent)
                self.wfile.write(b"x" * chunk)
                self.wfile.flush()
                sent += chunk
                time.sleep(0.1)
            return
        payload = BODY if status == 200 else b"not this time\n"
        self.send_response(status)
        if mode == "retry-after-long":
            self.send_header("Retry-After", "100")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if with_body:
            self.wfile.write(payload)

    def do_GET(self):
        self.answer(True)

    def do_HEAD(self):
        self.answer(False)

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
server.daemon_threads = True
print(server.server_address[1], flush=True)
server.serve_forever()
PY
: > "$work/requests"
python3 "$work/standin.py" "$work/requests" "$SLOW_BYTES" > "$work/port" &
server_pid=$!
for _ in $(seq 100); do [ -s "$work/port" ] && break; sleep 0.1; done
port=$(<"$work/port")
[ -n "$port" ] || { echo "FAIL: the stand-in server did not start"; exit 1; }
base="http://127.0.0.1:$port"
echo "curl: $(curl --version | sed -n 1p)"

# run_fetch <case> <argument…>: the script with these arguments, waited for under DEADLINE; `out`
# is what it printed on both streams and `status` its exit. A script still running at the deadline
# fails the case by name.
run_fetch() {
  local case=$1
  shift
  set +e
  timeout --kill-after=5 "$DEADLINE" "$script" "$@" > "$work/stdout" 2> "$work/stderr"
  status=$?
  set -e
  out=$(cat "$work/stdout" "$work/stderr")
  check "$case: the script ended before the case's ${DEADLINE} s deadline" \
    "$(is "$([ "$status" -ne 124 ] && [ "$status" -ne 137 ] && echo yes)" yes)"
}
# requests <path>: the requests the stand-in saw for that path, one `<method> <status>` a line.
requests() { awk -v path="$1" '$2 == path { print $1, $3 }' "$work/requests" | paste -sd, -; }
# failed_after <bound>: the words of the failure line between the address and curl's exit.
failed_after() { printf 'failed after up to 3 attempts of %s s, or 2 where the first two each ran to the bound' "$1"; }

echo "case: a download the server answers is fetched once"
run_fetch "answers" test "$ENDS" "$base/ok/answers" -o "$work/answers"
check "exits 0" "$(is "$status" 0)"
check "asked once" "$(is "$(requests /ok/answers)" "GET 200")"
check "wrote the body" "$(is "$(<"$work/answers")" "$BODY")"
check "printed nothing" "$(is "$out" "")"

echo "case: a 503 once is tried again and recovers"
run_fetch "503 once" test "$ENDS" "$base/503-once/download" -o "$work/503-once"
check "exits 0" "$(is "$status" 0)"
check "asked twice" "$(is "$(requests /503-once/download)" "GET 503,GET 200")"
check "wrote the body" "$(is "$(<"$work/503-once")" "$BODY")"

echo "case: a check that the address answers (-I) is tried again and recovers"
run_fetch "503 once, HEAD" test "$ENDS" "$base/503-once/head" -I -o /dev/null
check "exits 0" "$(is "$status" 0)"
check "asked twice, by HEAD" "$(is "$(requests /503-once/head)" "HEAD 503,HEAD 200")"

echo "case: a 503 on every attempt fails by name after three, and leaves no file behind"
echo "stale bytes" > "$work/503-always"
run_fetch "503 always" test "$ENDS" "$base/503-always/download" -o "$work/503-always"
check "exits 1" "$(is "$status" 1)"
check "asked three times" "$(is "$(requests /503-always/download)" "GET 503,GET 503,GET 503")"
check "names the stage, the address, the bound, curl's exit and the HTTP status" \
  "$(contains "$out" "^test: $base/503-always/download $(failed_after "$ENDS") (curl exit 22, HTTP 503)$")"
check "removed the file that stood at the -o path before the call" "$(is "$([ -e "$work/503-always" ] && echo there || echo gone)" gone)"

echo "case: a 404 fails by name after three"
run_fetch "404" test "$ENDS" "$base/404/download" -o "$work/404"
check "exits 1" "$(is "$status" 1)"
check "asked three times" "$(is "$(requests /404/download)" "GET 404,GET 404,GET 404")"
check "names the stage, the address, the bound, curl's exit and the HTTP status" \
  "$(contains "$out" "^test: $base/404/download $(failed_after "$ENDS") (curl exit 22, HTTP 404)$")"

echo "case: a body that stalls is cut at the bound, and the next attempt fetches it whole"
run_fetch "stall once" test "$STALLS" "$base/stall-once/download" -o "$work/stall-once"
check "exits 0" "$(is "$status" 0)"
check "asked twice" "$(is "$(requests /stall-once/download)" "GET stall,GET 200")"
check "wrote the whole body and no byte of the cut attempt" "$(is "$(<"$work/stall-once")" "$BODY")"

echo "case: a body that stalls every time is cut at each bound, and two attempts that run to the bound leave no room for a third"
run_fetch "stall always" test "$STALLS" "$base/stall-always/download" -o "$work/stall-always"
check "exits 1" "$(is "$status" 1)"
# The first attempt is cut at 2 s and the second starts at 3 s, after curl's 1 s pause, and is cut
# at 5 s: past twice the bound, so curl starts no third. The bound, 2 s, is under the stall cut's
# 30 s, so here it is the bound that ends each attempt, which the failure line's "or 2" names.
check "asked twice" "$(is "$(requests /stall-always/download)" "GET stall,GET stall")"
check "names the stage, the address, the bound, curl's timeout and the status the stall followed" \
  "$(contains "$out" "^test: $base/stall-always/download $(failed_after "$STALLS") (curl exit 28, HTTP 200)$")"

echo "case: a stall is cut 30 s in by the stall cut when the bound is longer, and the next attempt fetches it whole"
# The bound, 120 s, is past the case's 60 s deadline, so only the stall cut can end the first
# attempt in time: it has moved 5 bytes and then nothing, under 10,000 bytes a second for 30 s.
run_fetch "stall cut" test "$CUTS" "$base/stall-once/cut" -o "$work/stall-cut"
check "exits 0" "$(is "$status" 0)"
check "asked twice" "$(is "$(requests /stall-once/cut)" "GET stall,GET 200")"
check "wrote the whole body" "$(is "$(<"$work/stall-cut")" "$BODY")"

echo "case: a body that keeps moving above the floor past the stall cut's 30 s window is not cut"
run_fetch "slow but moving" test "$CUTS" "$base/slow/moving" -o "$work/slow"
check "exits 0" "$(is "$status" 0)"
check "asked once" "$(is "$(requests /slow/moving)" "GET slow")"
check "wrote every byte" "$(is "$(wc -c < "$work/slow")" "$SLOW_BYTES")"

echo "case: a Retry-After that would wait past twice the bound ends the retries and fails by name"
run_fetch "Retry-After" test "$STALLS" "$base/retry-after-long/download" -o "$work/retry-after"
check "exits 1" "$(is "$status" 1)"
check "asked once" "$(is "$(requests /retry-after-long/download)" "GET 503")"
# curl 7.88.1 and 8.5.0 exit 22 here; curl 8.14.1 prints its error 22 and exits 0, and the HTTP
# status is what fails the fetch.
check "names the stage, the address, the bound, curl's exit and the HTTP status" \
  "$(contains "$out" "^test: $base/retry-after-long/download $(failed_after "$STALLS") (curl exit \(22\|0\), HTTP 503)$")"
check "wrote no file" "$(is "$([ -e "$work/retry-after" ] && echo there || echo none)" none)"

echo "case: a curl that exits 0 on a 503, as curl 8.14.1 does after such a Retry-After, and one that exits 0 on a 200 and writes no file"
# A curl of the case's own, first on PATH, which prints the status it is given for `-w` and
# exits 0 without writing anything: curl 8.14.1's answer above, on whatever curl runs this test.
mkdir -p "$work/fake-curl"
printf '#!/bin/sh\nprintf "%%s" "$(cat %s)"\n' "$work/fake-curl/status" > "$work/fake-curl/curl"
chmod +x "$work/fake-curl/curl"
echo 503 > "$work/fake-curl/status"
PATH="$work/fake-curl:$PATH" run_fetch "exit 0 on a 503" test "$ENDS" "$base/ok/exit-0-503" -o "$work/exit-0-503"
check "exits 1 on the 503" "$(is "$status" 1)"
check "names curl's exit 0 and the 503" \
  "$(contains "$out" "^test: $base/ok/exit-0-503 $(failed_after "$ENDS") (curl exit 0, HTTP 503)$")"
echo 200 > "$work/fake-curl/status"
PATH="$work/fake-curl:$PATH" run_fetch "no file written" test "$ENDS" "$base/ok/no-file-written" -o "$work/no-file-written"
check "exits 1 on the 200 with no file" "$(is "$status" 1)"
check "names the file that is not there" \
  "$(contains "$out" "^test: $base/ok/no-file-written $(failed_after "$ENDS") (curl exit 0, HTTP 200, and nothing at $work/no-file-written)$")"

echo "case: a 200 with an empty body fails by name, so no caller ships a 0-byte file"
# The worker image's notices stage keeps what fetch.sh writes with no checksum after it, so an
# empty answer would otherwise become an empty licence file in the image.
run_fetch "empty" test "$ENDS" "$base/empty/download" -o "$work/empty"
check "exits 1" "$(is "$status" 1)"
check "names the empty file" \
  "$(contains "$out" "^test: $base/empty/download $(failed_after "$ENDS") (curl exit 0, HTTP 200, and $work/empty is empty)$")"
check "leaves no file at the -o path" "$(is "$([ -e "$work/empty" ] && echo there || echo gone)" gone)"
run_fetch "empty, HEAD" test "$ENDS" "$base/empty/head" -I -o /dev/null
check "a check that the address answers (-I -o /dev/null) still exits 0" "$(is "$status" 0)"

echo "case: a call without a stage, a bound and an address, or an -o file, or with a bound that is no number of seconds"
run_fetch "no address" test "$ENDS"
check "exits 2" "$(is "$status" 2)"
check "says the usage" "$(contains "$out" "usage: fetch.sh <stage> <seconds> <url>")"
run_fetch "no bound" test 1m "$base/ok/no-bound" -o "$work/no-bound"
check "exits 2 for a bound of 1m" "$(is "$status" 2)"
check "names the bound" "$(contains "$out" "the bound '1m' is no whole number of seconds above 0")"
check "asks nothing" "$(is "$(requests /ok/no-bound)" "")"
run_fetch "no file" test "$ENDS" "$base/ok/no-file"
check "exits 2 without an -o file" "$(is "$status" 2)"
check "names the missing -o" "$(contains "$out" "no -o <file> among the curl arguments")"
check "asks nothing without one" "$(is "$(requests /ok/no-file)" "")"

summary "fetch.sh"
