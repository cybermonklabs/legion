#!/usr/bin/env python3
"""The stand-in GitHub .github/scripts/gh-read.test.sh runs the real gh against.

    python3 .github/scripts/gh-read-stand-in.py <state dir> <port file>

It listens on a free local port, writes that port to <port file>, and answers every request from
the files a case writes to <state dir>, read at each request:
  spend-for    seconds: the first route request spends the budget for that long, writing the
               epoch it is spent until to spent-until (absent: the budget is whole).
  always-spent present: every route is refused, the reset passed or not.
  refuse-calls N: the route's first N requests are refused for the spent budget, its reset an
               hour after the first refusal, and the rate_limit route reports the budget at 0 from
               the first refusal through the Nth: refusals counted, not timed. The count is kept
               in refused-calls.
  graphql-spent  present: the rate_limit route reports the graphql budget at 0, its reset an hour
               off, while the core budget is as the files above say, and POST /graphql is refused
               as GitHub's GraphQL API refuses a spent budget (below, graphql-200).
  secondary    `<status> <retry-after seconds|none>`: route requests are refused for a secondary
               rate limit with that status and that retry-after header (none: no header), the
               first one alone, or every one while `secondary-always` is present; the core
               budget stays whole. Each refusal appends its path to `refused`.
  spent-message, secondary-message  the message of the spent budget's refusal, or of the
               secondary limit's, in place of GitHub's own.
  graphql-200  present: while the budget is spent, POST /graphql is refused as GitHub's GraphQL
               API refuses a spent budget: a 200 whose errors name the limit, with
               x-ratelimit-remaining: 0.
  reply        `<status> <message>`: every route request is answered that status and message,
               with the `Name: value` lines of reply-headers as its headers (none where that file
               is absent: a gateway's 504, say).
  hold-secondary  a token (fresh per case): a request about to take the secondary refusal holds
               until a second request has made the same check, 2 s at most.
  hold-spend-read  a token (fresh per case): a rate_limit request holds its read of spent-until
               until a later request has arrived (10 s at most) and that request's spend has
               opened spent-until (1 s at most after it arrived); the spend holds the opened,
               empty file until that read is done (2 s at most).
  answer       what a route answers once the budget is back: 200 or 404.
  body         the body of a 200.
  release      present: the release routes answer v1 as a release whose name and body must
               reach no log (RELEASE below), each answer held `release-delay` seconds (0.3 by
               default), and POST /graphql answers gh's draft lookup at once with no draft. gh
               release view and edit send that lookup and the REST read together and take the
               REST answer when the lookup finds no draft, so the REST answer closes second,
               after both requests opened.
  rate-limit-delay  seconds the rate_limit route holds its answer, read when it was asked.
  rate-limit-fails  a message: the rate_limit route answers 502 with it.
  rate-limit-reset  a value the rate_limit route answers as both budgets' reset, at 0 remaining.
  words        text the route's own messages end with (a 404's, a spent budget's, a secondary
               limit's): a case's marker, which gh prints on stderr inside the message.
  route-delay  seconds every other route holds its answer, read when it was asked.
  raw          present: GET /repos/o/r/raw answers this file's bytes as text/plain, a body gh's
               request trace prints as it came, line for line.
  redirect     present: GET /repos/o/r/moved answers a 301 to /repos/o/r/pulls/1, which gh
               follows, before any refusal, with the text of redirect-body as its body.
  workflow     present: GET /repos/o/r/actions/workflows/ci.yaml answers that workflow, id 7, so
               gh workflow run goes on to its dispatch, which `answer` decides like any route.
It appends `<epoch> <method> <path>` to `log` for every request it answers, and each request's
body (gh sends one read from stdin chunked), a line of its own, to `bodies`.

Its clock is the real one plus clock-offset, in whole seconds (0 while the file is absent). A case
driven on the stand-in clock (gh-read.test.sh's drive_on_clock) runs the helper with a `sleep`
that waits one real second and then adds its seconds to clock-offset, and with a `date +%s` that
answers the same clock. So a wait the helper sleeps takes a second, and the helper, the stand-in's
spends and resets and the time logged for each request all read one clock: a call the helper makes
before a reset, or before its sleep has ended, reaches the stand-in before that reset. The server
answers each request in a thread of its own, and gh release view and edit send two at once, so the
steps that read a state file and then write one (spent-until, secondary-done, refused-calls) run
under one lock, and so does the read of spent-until each answer is made from. The two hold knobs
make the interleavings that lock rules out certain, so a case can show each part of it is needed.
"""
import http.server
import json
import os
import socketserver
import sys
import threading
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

# The lock the read-then-write steps hold. Without it two threads both find spent-until unset, the
# second truncates the file after the first wrote it, and the first reads it empty: int('') raises,
# its request is answered by nothing, and gh reports an EOF, no rate limit, so the helper has
# nothing to wait for (run 45015045673).
STATE_LOCK = threading.Lock()
# What the hold knobs wait on: how many requests have arrived, and per case (the token the case
# wrote into the knob's file) what the held steps have done.
EVENTS = threading.Condition()
arrivals = [0]
held = {}


def read(name, default=""):
    try:
        with open(os.path.join(state, name)) as handle:
            return handle.read().strip()
    except FileNotFoundError:
        return default


def write(name, value, mode="w"):
    with open(os.path.join(state, name), mode) as handle:
        handle.write(value)


def clock():
    """The stand-in's clock: the real one plus clock-offset."""
    return time.time() + int(read("clock-offset", "0"))


def knob(name):
    """This case's state for a hold knob, or None when the case did not set it."""
    token = read(name)
    if not token:
        return None
    with EVENTS:
        return held.setdefault((name, token), {"checked": 0, "opened": False, "read": False})


def note(hold, key, value=None):
    """Sets a held step's fact (or adds one to a count, with no value) and wakes every waiter."""
    with EVENTS:
        hold[key] = hold[key] + 1 if value is None else value
        EVENTS.notify_all()


def wait_until(predicate, seconds):
    with EVENTS:
        EVENTS.wait_for(predicate, timeout=seconds)


def read_spent_until(path, mine):
    """spent-until as a number, read under STATE_LOCK. Under hold-spend-read a rate_limit read first
    holds until a later request has arrived and opened spent-until for its spend: inside the lock
    that spend cannot open it, so the hold ends at its cap; outside the lock the read would find the
    file opened and empty."""
    hold = knob("hold-spend-read") if path == "/rate_limit" else None
    if hold is not None:
        wait_until(lambda: arrivals[0] > mine, 10)
        wait_until(lambda: hold["opened"], 1)
    try:
        return int(read("spent-until", "0"))
    finally:
        if hold is not None:
            note(hold, "read", True)


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
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            data = b""
            while True:
                size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
                if size == 0:
                    self.rfile.readline()
                    break
                data += self.rfile.read(size)
                self.rfile.readline()
        else:
            length = int(self.headers.get("Content-Length") or 0)
            data = self.rfile.read(length) if length else b""
        if data:
            write("bodies", data.decode(errors="replace") + "\n", "a")
        path = urllib.parse.urlsplit(self.path).path
        now = clock()
        words = f" {read('words')}" if read("words") else ""
        with open(os.path.join(state, "log"), "a") as log:
            log.write(f"{now:.3f} {self.command} {path}\n")
        with EVENTS:
            arrivals[0] += 1
            mine = arrivals[0]
            EVENTS.notify_all()
        with STATE_LOCK:
            if path != "/rate_limit" and read("spend-for") and not read("spent-until"):
                with open(os.path.join(state, "spent-until"), "w") as handle:
                    hold = knob("hold-spend-read")
                    if hold is not None:
                        note(hold, "opened", True)
                        wait_until(lambda: hold["read"], 2)
                    handle.write(str(int(now) + int(read("spend-for"))))
            spent_until = read_spent_until(path, mine)
            if read("refuse-calls"):
                refused = int(read("refused-calls", "0"))
                if path != "/rate_limit":
                    refused += 1
                    write("refused-calls", str(refused))
                    if refused == 1:
                        write("refuse-until", str(int(now) + 3600))
                spent_until = int(read("refuse-until", "0")) if 1 <= refused <= int(read("refuse-calls")) else 0
            secondary = (path != "/rate_limit" and read("secondary") != ""
                         and (not read("secondary-done") or os.path.exists(os.path.join(state, "secondary-always"))))
            hold = knob("hold-secondary") if path != "/rate_limit" and read("secondary") else None
            if hold is not None:
                note(hold, "checked")
                if secondary:
                    wait_until(lambda: hold["checked"] >= 2, 2)
            if secondary:
                write("secondary-done", "1")
                write("refused", f"{path}\n", "a")
        spent = now < spent_until or os.path.exists(os.path.join(state, "always-spent"))
        if path != "/rate_limit":
            time.sleep(float(read("route-delay", "0")))
        if path == "/rate_limit":
            bucket = {"limit": 15000, "used": 15000 if spent else 0,
                      "remaining": 0 if spent else 15000,
                      "reset": spent_until if spent else int(now) + 3600}
            if read("rate-limit-reset"):
                bucket = dict(bucket, used=15000, remaining=0, reset=read("rate-limit-reset"))
            time.sleep(float(read("rate-limit-delay", "0")))
            graphql = dict(bucket, limit=5000)
            if os.path.exists(os.path.join(state, "graphql-spent")):
                graphql = {"limit": 5000, "used": 5000, "remaining": 0, "reset": int(now) + 3600}
            if read("rate-limit-fails"):
                self.answer(502, {"message": read("rate-limit-fails"), "status": "502"})
            else:
                self.answer(200, {"resources": {"core": bucket, "graphql": graphql}, "rate": bucket})
        elif os.path.exists(os.path.join(state, "redirect")) and path == "/repos/o/r/moved":
            data = read("redirect-body").encode()
            self.send_response(301)
            self.send_header("Location", "http://api.github.localhost/repos/o/r/pulls/1")
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif read("reply"):
            status, message = read("reply").split(" ", 1)
            self.answer(int(status), {"message": message, "documentation_url": "https://docs.github.com/rest",
                                      "status": status},
                        [tuple(line.split(": ", 1)) for line in read("reply-headers").splitlines() if line])
        elif secondary:
            status, retry_after = read("secondary").split()
            self.answer(int(status), {
                "message": read("secondary-message",
                                "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
                + words,
                "documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits",
                "status": status},
                [] if retry_after == "none" else [("retry-after", retry_after)])
        elif spent and path == "/graphql" and os.path.exists(os.path.join(state, "graphql-200")):
            self.answer(200, {"data": None, "errors": [
                {"type": "RATE_LIMITED", "message": "API rate limit already exceeded for installation ID 1."}]},
                        [("x-ratelimit-limit", "5000"), ("x-ratelimit-remaining", "0"),
                         ("x-ratelimit-used", "5000"), ("x-ratelimit-reset", str(spent_until)),
                         ("x-ratelimit-resource", "graphql")])
        elif path == "/graphql" and os.path.exists(os.path.join(state, "graphql-spent")):
            self.answer(200, {"data": None, "errors": [
                {"type": "RATE_LIMITED", "message": "API rate limit already exceeded for installation ID 1."}]},
                        [("x-ratelimit-limit", "5000"), ("x-ratelimit-remaining", "0"),
                         ("x-ratelimit-used", "5000"), ("x-ratelimit-reset", str(int(now) + 3600)),
                         ("x-ratelimit-resource", "graphql")])
        elif spent:
            self.answer(403, {"message": read("spent-message", "API rate limit exceeded for installation ID 1.") + words,
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
        elif os.path.exists(os.path.join(state, "workflow")) and path == "/repos/o/r/actions/workflows/ci.yaml":
            self.answer(200, {"id": 7, "node_id": "W_7", "name": "ci", "path": ".github/workflows/ci.yaml",
                              "state": "active"})
        elif os.path.exists(os.path.join(state, "raw")) and path == "/repos/o/r/raw":
            with open(os.path.join(state, "raw"), "rb") as handle:
                data = handle.read()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif read("answer", "200") == "404":
            self.answer(404, {"message": f"Not Found{words}",
                              "documentation_url": "https://docs.github.com/rest", "status": "404"})
        else:
            self.answer(200, json.loads(read("body", "{}")))

    do_GET = do_POST = do_PATCH = do_DELETE = handle_any

    def log_message(self, *args):
        pass


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True

    # gh drops the slower of two requests it raced; the write into its closed socket is no fault.
    # Any other error is the stand-in's own: its traceback goes to the stand-in's stderr, the job
    # log, beside the case it broke.
    def handle_error(self, request, client_address):
        if not isinstance(sys.exc_info()[1], ConnectionError):
            super().handle_error(request, client_address)


with Server(("127.0.0.1", 0), Handler) as httpd:
    with open(portfile, "w") as handle:
        handle.write(str(httpd.server_address[1]))
    httpd.serve_forever()
