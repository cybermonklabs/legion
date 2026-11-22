#!/usr/bin/env bash
# Tests for gh-read.sh: gh_wait_reset, which waits out a spent API budget when its reset falls
# before the job's or the step's limit and fails by name when it does not, and gh_read's ladder,
# which takes each attempt through it.
#
# Every case runs the real gh against a stand-in GitHub, a local HTTP server the test writes
# below, so the words the helper reads are the ones gh prints for each answer. gh reaches it as
# GitHub's local host (GH_HOST=github.localhost, which gh calls over plain http at
# api.github.localhost) through HTTP_PROXY. The server spends the repository's budget at the
# first route a case calls, for as many seconds as the case asks, so the refusal does not hang on
# how fast gh starts on a loaded machine: until then it answers every route 403 with
# x-ratelimit-remaining: 0, x-ratelimit-reset: that epoch and GitHub's own message, and its
# rate_limit route reports the core budget at 0 with that reset; after it, a route answers what
# the case asked for (a body, or a 404).
#
# The helper reads the job's start and the step's start off the process tree, as the Runner.Worker
# process GitHub's runner starts for each job and that process's child, the step's shell. A case
# runs the helper in a bash under a stand-in Runner.Worker (bash under that name), so the job
# starts when the case does. The last case reads the real one, on a GitHub runner only.
#
# Run from anywhere: .github/scripts/gh-read.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
server=''
trap '[ -z "$server" ] || kill "$server" 2>/dev/null; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

if ! command -v gh > /dev/null; then
  echo "gh-read.test.sh needs gh on PATH: every case runs the real gh against the stand-in GitHub" >&2
  exit 1
fi

# The stand-in GitHub. It reads the case's state from files in its directory at each request:
#   spend-for    seconds: the first route request spends the budget for that long, writing the
#                epoch it is spent until to spent-until (absent: the budget is whole).
#   always-spent present: every route is refused, the reset passed or not.
#   secondary    `<status> <retry-after seconds|none>`: route requests are refused for a secondary
#                rate limit with that status and that retry-after header (none: no header), the
#                first one alone, or every one while `secondary-always` is present; the core
#                budget stays whole.
#   answer       what a route answers once the budget is back: 200 or 404.
#   body         the body of a 200.
#   release      present: the release routes answer v1 as a release whose name and body must
#                reach no log (RELEASE below), each answer held `release-delay` seconds (0.3 by
#                default), and POST /graphql answers gh's draft lookup at once with no draft. gh
#                release view and edit send that lookup and the REST read together and take the
#                REST answer when the lookup finds no draft, so the REST answer closes second,
#                after both requests opened.
#   rate-limit-delay  seconds the rate_limit route holds its answer, read when it was asked.
#   raw          present: GET /repos/o/r/raw answers this file's bytes as text/plain, a body gh's
#                request trace prints as it came, line for line.
# and appends `<epoch> <method> <path>` to `log` for every request it answers.
cat > "$work/github.py" <<'PY'
import http.server
import json
import os
import socketserver
import sys
import time
import urllib.parse

state, portfile = sys.argv[1:3]
RELEASE = {"id": 1, "tag_name": "v1", "name": "the draft's name", "body": "the draft's body, before the leak guard",
           "draft": True, "prerelease": False, "target_commitish": "main", "assets": [],
           "author": {"login": "builder"}, "created_at": "2026-11-21T00:00:00Z", "published_at": None,
           "url": "http://api.github.localhost/repos/o/r/releases/1",
           "html_url": "http://github.localhost/o/r/releases/tag/v1",
           "upload_url": "http://uploads.github.localhost/repos/o/r/releases/1/assets{?name,label}",
           "tarball_url": "t", "zipball_url": "z"}


def read(name, default=""):
    try:
        with open(os.path.join(state, name)) as handle:
            return handle.read().strip()
    except FileNotFoundError:
        return default


class Handler(http.server.BaseHTTPRequestHandler):
    def answer(self, status, body, headers=()):
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        path = urllib.parse.urlsplit(self.path).path
        now = time.time()
        with open(os.path.join(state, "log"), "a") as log:
            log.write(f"{now:.3f} {self.command} {path}\n")
        if path != "/rate_limit" and read("spend-for") and not read("spent-until"):
            with open(os.path.join(state, "spent-until"), "w") as handle:
                handle.write(str(int(now) + int(read("spend-for"))))
        spent_until = int(read("spent-until", "0"))
        spent = now < spent_until or os.path.exists(os.path.join(state, "always-spent"))
        if path == "/rate_limit":
            bucket = {"limit": 15000, "used": 15000 if spent else 0,
                      "remaining": 0 if spent else 15000,
                      "reset": spent_until if spent else int(now) + 3600}
            time.sleep(float(read("rate-limit-delay", "0")))
            self.answer(200, {"resources": {"core": bucket, "graphql": dict(bucket, limit=5000)},
                              "rate": bucket})
        elif (path != "/rate_limit" and read("secondary")
              and (not read("secondary-done") or os.path.exists(os.path.join(state, "secondary-always")))):
            with open(os.path.join(state, "secondary-done"), "w") as handle:
                handle.write("1")
            status, retry_after = read("secondary").split()
            self.answer(int(status), {
                "message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
                "documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits",
                "status": status},
                [] if retry_after == "none" else [("retry-after", retry_after)])
        elif spent:
            self.answer(403, {"message": "API rate limit exceeded for installation ID 1.",
                              "documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api",
                              "status": "403"},
                        [("x-ratelimit-limit", "15000"), ("x-ratelimit-remaining", "0"),
                         ("x-ratelimit-used", "15000"), ("x-ratelimit-reset", str(spent_until)),
                         ("x-ratelimit-resource", "core")])
        elif os.path.exists(os.path.join(state, "release")) and path == "/graphql":
            self.answer(200, {"data": {"repository": {"release": None}}})
        elif os.path.exists(os.path.join(state, "release")) and path.startswith("/repos/o/r/releases"):
            time.sleep(float(read("release-delay", "0.3")))
            self.answer(200, RELEASE)
        elif os.path.exists(os.path.join(state, "raw")) and path == "/repos/o/r/raw":
            with open(os.path.join(state, "raw"), "rb") as handle:
                data = handle.read()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif read("answer", "200") == "404":
            self.answer(404, {"message": "Not Found",
                              "documentation_url": "https://docs.github.com/rest", "status": "404"})
        else:
            self.answer(200, json.loads(read("body", "{}")))

    do_GET = do_POST = do_PATCH = handle_any

    def log_message(self, *args):
        pass


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True

    # gh drops the slower of two requests it raced; the write into its closed socket is no fault.
    def handle_error(self, request, client_address):
        pass


with Server(("127.0.0.1", 0), Handler) as httpd:
    with open(portfile, "w") as handle:
        handle.write(str(httpd.server_address[1]))
    httpd.serve_forever()
PY

mkdir -p "$work/state" "$work/bin" "$work/gh-config"
python3 "$work/github.py" "$work/state" "$work/port" &
server=$!
for _ in $(seq 50); do
  [ -s "$work/port" ] && break
  sleep 0.1
done
if ! [ -s "$work/port" ]; then
  echo "the stand-in GitHub wrote no port" >&2
  exit 1
fi
port=$(<"$work/port")

# The stand-in runner process: bash under the name GitHub's runner starts each job as.
ln -s "$(command -v bash)" "$work/bin/Runner.Worker"

# github <spend for seconds|never> [<answer>]: this case's GitHub. `never` leaves the budget
# whole; a number spends it, at the case's first route request, until that many seconds after it.
# The answer once the budget is back is a 200 with {"title": "t"} unless the case names 404. The
# log starts empty.
github() {
  rm -f "$work/state"/*
  [ "$1" = never ] || echo "$1" > "$work/state/spend-for"
  echo "${2:-200}" > "$work/state/answer"
  echo '{"title": "t"}' > "$work/state/body"
  : > "$work/state/log"
}

# drive [--no-worker] [NAME=value...] -- <shell code>: runs the code in a bash that has sourced
# gh-read.sh, as a step's shell under the stand-in Runner.Worker (none with --no-worker), with gh
# pointed at the stand-in GitHub and the NAME=value pairs set; JOB_TIMEOUT_MINUTES and
# STEP_TIMEOUT_MINUTES are unset unless a pair sets them. Sets out (stdout), err (stderr),
# status and seconds (its wall time, whole seconds).
drive() {
  local worker=yes started
  local -a assignments=()
  if [ "$1" = --no-worker ]; then
    worker=''
    shift
  fi
  while [ "$1" != -- ]; do
    assignments+=("$1")
    shift
  done
  shift
  local -a command=(bash -c "source \"\$GH_READ\"; $1")
  # `; exit` keeps the stand-in worker a process of its own: a `-c` holding one command alone
  # would exec it, and the worker would be gone from the tree the helper reads.
  [ -z "$worker" ] || command=("$work/bin/Runner.Worker" -c '"$@"; exit $?' worker "${command[@]}")
  started=$(date +%s)
  set +e
  out=$(cd "$work" && env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES -u GITHUB_TOKEN \
    GH_READ="$script_dir/gh-read.sh" GH_HOST=github.localhost GH_TOKEN=t \
    GH_CONFIG_DIR="$work/gh-config" GH_NO_UPDATE_NOTIFIER=1 GH_PROMPT_DISABLED=1 \
    HTTP_PROXY="http://127.0.0.1:$port" http_proxy="http://127.0.0.1:$port" NO_PROXY= no_proxy= \
    GH_RETRY_SECONDS=0 "${assignments[@]}" "${command[@]}" 2> "$work/err")
  status=$?
  set -e
  seconds=$(($(date +%s) - started))
  err=$(<"$work/err")
}

# calls <path>: how many requests the stand-in GitHub answered for that path.
calls() { awk -v path="$1" '$3 == path' "$work/state/log" | wc -l | tr -d ' '; }
# nth_at <path> <n>: the epoch (whole seconds) of the n-th request for that path.
nth_at() { awk -v path="$1" -v n="$2" '$3 == path && ++seen == n { split($1, t, "."); print t[1] }' "$work/state/log"; }
spent_until() { cat "$work/state/spent-until"; }
utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
errors() { grep -c '^::error::' <<<"$err" || true; }
warnings() { grep -c '^::warning::' <<<"$err" || true; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }
below() { [ "$1" -lt "$2" ] && echo true || echo false; }

echo "case: a spent budget whose reset falls before the job's limit is waited out"
# 10 s: the refused call and its rate_limit read must both land before the reset, or the helper
# reads the window as turned over and calls again at once, with no wait to log; a loaded host was
# seen to take over 3 s for the two. The same holds for each case below that wants the wait logged.
github 10
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
reset=$(spent_until)
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the call it made after the wait" "$(is "$out" '{"title":"t"}')"
check "calls the route twice: refused, then answered" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "reads the reset from the rate_limit route once" "$(is "$(calls /rate_limit)" 1)"
check "makes the second call no sooner than the reset" "$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$reset")"
check "logs exactly one line about the wait" "$(is "$(warnings)" 1)"
check "which names the reset" "$(contains "$err" "^::warning::the API budget is spent until $(utc "$reset");")"
check "and the job's limit it waits within" "$(contains "$err" "within this job's limit at ")"
check "and fails nothing" "$(is "$(errors)" 0)"
check "gh's own words for the refusal stay in the log, once: the answered call printed none" \
  "$(is "$(grep -c 'API rate limit exceeded for installation ID 1' <<<"$err")" 1)"

# A case that must not wait spends the budget for a minute or more, so a wait would take that
# long, and holds the case to under 30 s, which leaves gh room to start on a loaded machine.
echo "case: a spent budget whose reset falls past the job's limit fails by name at once"
github 3600
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "calls the route once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "prints exactly one error" "$(is "$(errors)" 1)"
check "naming the reset and the limit" \
  "$(contains "$err" "^::error::the API budget is spent until $(utc "$(spent_until)"), past this job's limit at ")"
check "and nothing on stdout" "$(is "$out" '')"

echo "case: a reset before the limit but inside the margin is not waited for"
# The margin is the time the call and the rest of the step still need after the wait: a reset 40 s
# away under a one-minute limit leaves less than the 30 s margin. With no margin this case sleeps
# 40 s.
github 40
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the limit it would outlive" "$(contains "$err" "past this job's limit at ")"

echo "case: a limit that has already passed is not waited for"
github 60
drive JOB_TIMEOUT_MINUTES=0 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the limit" "$(contains "$err" "past this job's limit at ")"

echo "case: a step limit tighter than the job's binds"
github 60
drive JOB_TIMEOUT_MINUTES=10 STEP_TIMEOUT_MINUTES=0 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the step's limit" "$(contains "$err" "past this step's limit at ")"

echo "case: a step limit alone is a limit"
github 10
drive STEP_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "waits and passes" "$(is "$status|$out" '0|{"title":"t"}')"
check "within the step's limit" "$(contains "$err" "within this step's limit at ")"

echo "case: a plain 404 fails as it did before"
github never 404
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits as gh does" "$(is "$status" 1)"
check "gh's words reach stderr" "$(contains "$err" '^gh: Not Found (HTTP 404)$')"
check "the body gh prints reaches stdout, as with a bare gh" "$(contains "$out" '"message":"Not Found"')"
check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"
check "calls once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "logs nothing of its own" "$(is "$(warnings)|$(errors)" '0|0')"

echo "case: a call that succeeds passes through untouched"
github never
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints gh's stdout" "$(is "$out" '{"title":"t"}')"
check "and no stderr" "$(is "$err" '')"
check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"

echo "case: a step that names no limit does not wait"
github 60
drive -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the reset and the variable it lacks" \
  "$(contains "$err" "^::error::the API budget is spent until $(utc "$(spent_until)"), and this step names no time limit to wait within (JOB_TIMEOUT_MINUTES is not set)")"

echo "case: a limit that is no whole number of minutes does not wait"
github 60
drive JOB_TIMEOUT_MINUTES=ten -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the value" "$(contains "$err" "JOB_TIMEOUT_MINUTES is 'ten', not a whole number of minutes")"

echo "case: no Runner.Worker among the step's parents, so no start to measure from"
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  # On a runner the real Runner.Worker is an ancestor of every process this harness starts.
  echo "  SKIP: on a GitHub runner every process of this harness runs under its job's Runner.Worker"
else
  github 60
  drive --no-worker JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
  check "fails" "$(is "$status" 75)"
  check "with no sleep" "$(below "$seconds" 30)"
  check "naming why" "$(contains "$err" 'no Runner.Worker process is among its parents')"
fi

# Every script that calls the helper runs under set -euo pipefail, most with inherit_errexit: a
# refused run inside the helper must not end the caller before the helper decides.
strict='set -euo pipefail; shopt -s inherit_errexit;'
echo "case: a caller under set -e still gets the wait"
github 3
drive JOB_TIMEOUT_MINUTES=5 -- "$strict"' gh_wait_reset gh api repos/o/r/pulls/1 > answer; cat answer'
check "exits 0" "$(is "$status" 0)"
check "with the answer after the wait" "$(is "$out" '{"title":"t"}')"
check "having called the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"

echo "case: a caller under set -e gets gh's own status for a 404, and stops there"
github never 404
drive JOB_TIMEOUT_MINUTES=5 -- "$strict"' gh_wait_reset gh api repos/o/r/pulls/1 > answer; echo reached'
check "exits as gh does" "$(is "$status" 1)"
check "and runs nothing after the call" "$(is "$(contains "$out" reached)" false)"

echo "case: a caller under set -e stops at a budget spent past the limit, named"
github 3600
drive JOB_TIMEOUT_MINUTES=1 -- "$strict"' x=$(gh_wait_reset gh api repos/o/r/pulls/1); echo reached'
check "exits with the helper's status" "$(is "$status" 75)"
check "naming it" "$(contains "$err" '^::error::the API budget is spent until ')"
check "and runs nothing after the call" "$(is "$(contains "$out" reached)" false)"

echo "case: the budget is spent again when the wait ends"
github 2
touch "$work/state/always-spent"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "after waiting once" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "naming it" "$(contains "$err" "^::error::the API budget is still spent after the wait for its reset at $(utc "$(spent_until)")")"

echo "case: --stderr keeps the deciding call's words in a file and the helper's own lines in the log"
github 10 404
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset --stderr "$PWD/words" gh api repos/o/r/pulls/1 > body; echo "status=$?"; cat words'
check "the call after the wait decides" "$(contains "$out" '^status=1$')"
check "the file holds that call's words" "$(contains "$out" '^gh: Not Found (HTTP 404)$')"
check "and not the refusal's" "$(is "$(contains "$out" 'rate limit exceeded')" false)"
check "the wait still reaches the log" "$(contains "$err" '^::warning::the API budget is spent until ')"
rm -f "$work/words"

echo "case: gh_read takes its attempt through the wait"
github 3
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_read gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints the answer" "$(is "$out" '{"title":"t"}')"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"

echo "case: gh_read stops its ladder on a budget it cannot wait for"
github 3600
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_read gh api repos/o/r/pulls/1'
check "fails with the helper's status" "$(is "$status" 75)"
check "calls the route once, not three times" "$(is "$(calls /repos/o/r/pulls/1)" 1)"

echo "case: gh_read still climbs its ladder on any other refusal"
github never 404
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_read gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 1)"
check "after three attempts" "$(is "$(calls /repos/o/r/pulls/1)" 3)"

# A secondary rate limit spends no budget: GitHub's answer names how long to wait in a retry-after
# header, and its documentation says to wait at least a minute where the answer names none.
# secondary <status> <retry-after|none> [always]: this case's GitHub refuses the first route call
# (every route call, with `always`) for a secondary rate limit.
secondary() {
  echo "$1 $2" > "$work/state/secondary"
  [ "${3:-}" != always ] || touch "$work/state/secondary-always"
}

echo "case: a secondary rate limit's retry-after is waited out"
github never
secondary 429 3
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the call after the wait" "$(is "$out" '{"title":"t"}')"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "reads no rate limit, since no budget is spent" "$(is "$(calls /rate_limit)" 0)"
check "waits the 3 s GitHub asked for" \
  "$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(($(nth_at /repos/o/r/pulls/1 1) + 3))")"
check "logs exactly one line about the wait, naming the retry-after" \
  "$(is "$(grep -c "^::warning::GitHub's secondary rate limit asks this call to wait 3 s (its retry-after)" <<<"$err")" 1)"
check "and the job's limit it waits within" "$(contains "$err" "within this job's limit at ")"
check "gh's own words for the refusal stay in the log, once" \
  "$(is "$(grep -c 'secondary rate limit.* (HTTP 429)$' <<<"$err")" 1)"
check "and none of gh's request trace does" "$(is "$(contains "$err" '^\* Request\|^< \|^> ')" false)"

echo "case: a secondary rate limit with no retry-after takes GitHub's one-minute minimum"
# A minute and the margin do not fit in a one-minute limit, so the case fails at once rather than
# waiting the minute; the error is where the 60 s shows.
github never
secondary 403 none
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "naming the 60 s minimum and the limit" \
  "$(contains "$err" "^::error::GitHub's secondary rate limit asks this call to wait 60 s (GitHub's documented minimum, as the answer named no retry-after), until .*, past this job's limit at ")"

echo "case: a secondary rate limit whose retry-after falls past the limit fails by name at once"
github never
secondary 403 3600
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(below "$seconds" 30)"
check "calls once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "naming the retry-after and the limit" \
  "$(contains "$err" "^::error::GitHub's secondary rate limit asks this call to wait 3600 s (its retry-after), until .*, past this job's limit at ")"

echo "case: a secondary rate limit that refuses again after the wait fails by name"
github never
secondary 403 2 always
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "after waiting once" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "naming it" "$(contains "$err" "^::error::GitHub refused this call for its rate limit again after the wait")"

echo "case: a wrapped command's secondary rate limit takes the minimum, its headers unread"
# The helper reads the headers of `gh api`'s own answers alone: a command it does not run as `gh
# api` itself (the browser lane's bash wrapper, gh release, gh run) has none it can read.
github never
secondary 403 3
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset bash -c "gh api repos/o/r/pulls/1"'
check "fails" "$(is "$status" 75)"
check "naming the 60 s minimum" "$(contains "$err" "wait 60 s (GitHub's documented minimum, as the answer named no retry-after)")"

echo "case: gh_read takes a secondary rate limit through the wait too"
github never
secondary 403 2
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_read gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "the second call made after the wait, not by the ladder at once" \
  "$(contains "$err" "^::warning::GitHub's secondary rate limit asks this call to wait 2 s (its retry-after)")"

# gh release view, edit and upload send a GraphQL lookup and a REST request together, so both are
# open at once. The helper reads gh's request trace (GH_DEBUG=api) for `gh api` alone, which has
# one request open at a time: a trace of two open requests let the second answer's headers and
# body through to the log, a draft release's name and body among them, ahead of release-push.sh's
# leak guard.
echo "case: gh release view through the helper prints the release and nothing on stderr"
github never
touch "$work/state/release"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release view v1 --repo o/r --json name,body -q ".name, .body"'
check "exits 0" "$(is "$status" 0)"
check "prints the name and the body on stdout" "$(is "$out" "the draft's name
the draft's body, before the leak guard")"
check "and nothing on stderr: no answer, no header, no trace" "$(is "$err" '')"
check "both requests were made, the REST answer closing second" \
  "$(is "$(calls /graphql)|$(calls /repos/o/r/releases/tags/v1)" '1|1')"

echo "case: gh release edit through the helper writes nothing on stderr"
github never
touch "$work/state/release"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release edit v1 --repo o/r --draft=false'
check "exits 0" "$(is "$status" 0)"
check "and nothing on stderr" "$(is "$err" '')"
check "it patched the release" "$(is "$(calls /repos/o/r/releases/1)" 1)"

echo "case: gh release view under a spent budget waits, and the log holds no release"
github 10
touch "$work/state/release"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release view v1 --repo o/r --json name,body -q ".name, .body"'
check "exits 0" "$(is "$status" 0)"
check "prints the release on stdout" "$(contains "$out" "^the draft's name$")"
check "after one wait" "$(is "$(warnings)" 1)"
check "with nothing of the release on stderr" "$(is "$(contains "$err" "the draft's")" false)"
check "and no line of a trace" "$(is "$(contains "$err" '^\* Request\|^< \|^> \|^{')" false)"

# A raw answer is printed into gh's request trace as it came, and a line of it can read like gh's
# own end of a request. The trace of one request runs from its `* Request at` line to its last
# `* Request took` line, so no line of the body can end it early; a paginated read, several
# requests in turn, is not traced at all.
echo "case: a raw answer's line that reads like gh's own end of a request does not end the trace"
github never
printf '%s\n' "a raw file's first line" '* Request took 1.5ms' 'a raw line after the forged end of the request' \
  > "$work/state/raw"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/raw -H "Accept: application/vnd.github.raw"'
check "exits 0" "$(is "$status" 0)"
check "prints the raw answer on stdout, whole" "$(contains "$out" '^a raw line after the forged end of the request$')"
check "and nothing on stderr" "$(is "$err" '')"

echo "case: a paginated raw read through the helper writes nothing on stderr"
github never
printf '%s\n' "a raw file's first line" '* Request took 1.5ms' 'a raw line after the forged end of the request' \
  > "$work/state/raw"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api --paginate repos/o/r/raw -H "Accept: application/vnd.github.raw"'
check "exits 0" "$(is "$status" 0)"
check "prints the raw answer on stdout, whole" "$(contains "$out" '^a raw line after the forged end of the request$')"
check "and nothing on stderr" "$(is "$err" '')"

echo "case: the wait is measured from after the rate_limit read, not before it"
# The rate_limit route takes 5 s to answer a reset 3 s after the refusal: the budget is back by the
# time the answer arrives, so the call goes again at once. A clock read before the probe sleeps the
# reset's 4 s on top of the probe's 5, so the second call lands 6 s or more after the reset; the row
# allows 5, room for a loaded host.
github 3
echo 5 > "$work/state/rate-limit-delay"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
reset=$(spent_until)
check "exits 0" "$(is "$status" 0)"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "the second call no later than 5 s after the reset" "$(at_least "$((reset + 5))" "$(nth_at /repos/o/r/pulls/1 2)")"

echo "case: gh_read with no limit set keeps its ladder's retries for a rate limit"
# With no limit the helper cannot wait, and says so; the ladder still makes its later attempts, as
# it did before the helper, and one lands after a short reset.
# The attempts come about 8 s and 24 s after the first; the budget is back between 19 and 20 s
# after it (the stand-in floors its clock). Each refused attempt's rate_limit read must land before
# the reset, or the helper reads the window as turned over and calls again at once, so the second
# attempt has 10 s of room for a slow gh on a loaded machine (a read that took over 6 s was seen),
# and the third lands 4 s after the reset.
github 20
drive GH_RETRY_SECONDS=8 -- 'gh_read gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints the answer" "$(is "$out" '{"title":"t"}')"
check "after three attempts" "$(is "$(calls /repos/o/r/pulls/1)" 3)"
check "the two refused ones each named the missing limit" \
  "$(is "$(grep -c 'names no time limit to wait within' <<<"$err")" 2)"

echo "case: on a GitHub runner, the job's start is read off the real Runner.Worker"
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  # This harness's step runs inside ci.yaml's test job, whose limit is 30 minutes, so a job still
  # running started less than 30 minutes ago and no later than now.
  now=$(date +%s)
  if deadline=$(JOB_TIMEOUT_MINUTES=30 bash -c 'source "$1"; _gh_read_deadline' _ "$script_dir/gh-read.sh"); then
    deadline=${deadline%% *}
    check "finds this job's Runner.Worker" true
    check "whose start is no later than now" "$(at_least "$((now + 1))" "$((deadline - 1800))")"
    check "and less than the job's 30 minutes ago" "$(at_least "$((deadline - 1800))" "$((now - 1800))")"
  else
    check "finds this job's Runner.Worker ($deadline)" false
  fi
else
  echo "  SKIP: not on a GitHub runner (GITHUB_ACTIONS is not true)"
fi

summary "gh-read.sh's wait for a spent API budget and its retry ladder"
