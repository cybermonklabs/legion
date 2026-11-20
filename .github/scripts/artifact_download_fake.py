#!/usr/bin/env python3
"""A stand-in for GitHub's REST API and its artifact storage, for artifact_download.test.sh.

  artifact_download_fake.py --zip <file> --port-file <file> [options]

It serves, on 127.0.0.1 and a port it chooses and writes to --port-file once it listens:

  GET /repos/<repo>/actions/runs/<run>/artifacts?name=<n>  the one artifact, <n> its name, with
                                                           the zip's size and SHA-256 digest
  GET /repos/<repo>/actions/artifacts/<id>/zip             302 to /blob/<id>?sig=<n> on this
                                                           server, n counting the signings
  GET /repos/<repo>/actions/runs/<run>/attempts/<a>        the run attempt, with run_started_at
  GET /repos/<repo>/actions/runs/<run>/attempts/<a>/jobs   for attempt --attempt: another
                                                           runner's in-progress job, started
                                                           earlier, then a completed job on
                                                           runner --runner, then job 99, in
                                                           progress on --runner; for any other
                                                           attempt, job 99 completed
  GET /blob/<id>                                           the zip, whole or one Range

The times it gives are whole seconds, as GitHub's are, counted from S, the moment of its first
API request on the runner's clock, so that a wait before the script under test starts takes
nothing from its deadline: job 99 started --started-ago s before S (2 by default, so its start
is before the script began), the run attempt started --run-ago s before S and the artifact was
made --artifact-ago s before S. At that first request it logs
"fake: times on the runner's clock: job 99 started at epoch <J>, the run attempt at epoch <R>,
the artifact at epoch <A>" on stderr, so a test reads them on the clock the script under test
runs on. Each request is logged to stderr with its range, so a test can count them. Runs until
killed.

The storage and the API misbehave as the options say, each the shape of a fault a read meets:

  --rate <bytes/s>      every connection together is sent no faster than this
  --stall-after <bytes> each connection sends this many bytes of its body and then nothing,
                        holding its socket open
  --slow-first <bytes/s> the first request is sent at this rate and every other at full speed:
                        one slow connection beside fast ones
  --ignore-range        the storage answers every request with 200 and the whole zip
  --misalign            the storage answers every range with a 206 for bytes that start one
                        past those asked for
  --misalign-first <k>  the same for its first k range requests only, then the bytes asked for
  --silent              the storage reads a request and never answers it
  --refuse              the redirect names a port nothing listens on
  --blob-status <code>  the storage answers every request with this status
  --fail-first <k>      the storage answers its first k requests with --fail-status, then
                        normally: the shape run 44758704517's storage showed (503s, read again)
  --refuse-urls <k>     the storage answers its first k requests with 403, as for an expired URL
  --url-life <s>        the storage refuses with 403 a URL signed more than s seconds before
  --missing             the API lists no artifact of that name
  --digest <d>          the API records this digest instead of the zip's own
  --no-digest           the API records no digest
  --corrupt-at <n>      the zip is served, and its digest recorded, with byte n changed: an
                        upload whose digest matches a zip that cannot be extracted
  --jobs-status <code>  the jobs API answers with this status
  --twin-job            the jobs API lists a second in-progress job on --runner
  --attempt <a>         the run attempt whose jobs list holds job 99 in progress
  --clock-offset <s>    the API's clock runs s seconds ahead of the runner's: its Date header and
                        every time it gives are moved by s
  --trickle <route>     the jobs route, the artifacts route or all the API's JSON routes (all)
                        send their body one byte per --trickle-gap s
"""
import argparse
import datetime
import hashlib
import http.server
import json
import math
import os
import socket
import sys
import threading
import time
import urllib.parse


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--zip", required=True)
    p.add_argument("--port-file", required=True)
    p.add_argument("--name", default="the-artifact")
    p.add_argument("--artifact-id", type=int, default=4242)
    p.add_argument("--runner", default="GitHub Actions 7")
    p.add_argument("--started-ago", type=int, default=2)
    p.add_argument("--run-ago", type=int, default=60)
    p.add_argument("--artifact-ago", type=int, default=10)
    p.add_argument("--rate", type=float, default=0.0)
    p.add_argument("--stall-after", type=int, default=-1)
    p.add_argument("--silent", action="store_true")
    p.add_argument("--refuse", action="store_true")
    p.add_argument("--blob-status", type=int, default=0)
    p.add_argument("--fail-first", type=int, default=0)
    p.add_argument("--fail-status", type=int, default=503)
    p.add_argument("--refuse-urls", type=int, default=0)
    p.add_argument("--url-life", type=float, default=0.0)
    p.add_argument("--missing", action="store_true")
    p.add_argument("--digest", default="")
    p.add_argument("--no-digest", action="store_true")
    p.add_argument("--corrupt-at", type=int, default=-1)
    p.add_argument("--jobs-status", type=int, default=200)
    p.add_argument("--twin-job", action="store_true")
    p.add_argument("--attempt", type=int, default=1)
    p.add_argument("--clock-offset", type=int, default=0)
    p.add_argument("--trickle", choices=("jobs", "artifacts", "all"))
    p.add_argument("--trickle-gap", type=float, default=1.0)
    p.add_argument("--slow-first", type=float, default=0.0)
    p.add_argument("--ignore-range", action="store_true")
    p.add_argument("--misalign", action="store_true")
    p.add_argument("--misalign-first", type=int, default=0)
    args = p.parse_args()

    data = open(args.zip, "rb").read()
    if args.corrupt_at >= 0:
        data = data[:args.corrupt_at] + bytes([data[args.corrupt_at] ^ 0xFF]) + data[args.corrupt_at + 1:]
    digest = "" if args.no_digest else args.digest or "sha256:" + hashlib.sha256(data).hexdigest()
    state = {"blob_requests": 0, "tokens": 0.0, "at": time.monotonic(), "signed": []}
    lock = threading.Lock()

    # A port that refuses every connection, for --refuse: bound and never listened on, and held
    # for the fake's whole life. A port bound and closed again is free for any other process of
    # the machine to take, and a connection to it then reaches that process instead of a refusal.
    dead = socket.socket()
    dead.bind(("127.0.0.1", 0))
    dead_port = dead.getsockname()[1]

    def start():
        """S, fixed at the first API request, with the times it gives logged then."""
        with lock:
            if state.get("start") is None:
                s = state["start"] = math.floor(time.time())
                sys.stderr.write(f"fake: times on the runner's clock: job 99 started at epoch "
                                 f"{s - args.started_ago}, the run attempt at epoch {s - args.run_ago}, "
                                 f"the artifact at epoch {s - args.artifact_ago}\n")
                sys.stderr.flush()
            return state["start"]

    def iso(runner_epoch):
        """A runner-clock time as the API, whose clock runs --clock-offset ahead, writes it."""
        return datetime.datetime.fromtimestamp(runner_epoch + args.clock_offset,
                                               datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    def throttle(n):
        """Waits until n more bytes may go, at --rate over every connection together."""
        if args.rate <= 0:
            return
        while True:
            with lock:
                t = time.monotonic()
                state["tokens"] = min(args.rate, state["tokens"] + (t - state["at"]) * args.rate)
                state["at"] = t
                if state["tokens"] >= n:
                    state["tokens"] -= n
                    return
                wait = (n - state["tokens"]) / args.rate
            time.sleep(wait)

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def date_time_string(self, timestamp=None):
            return super().date_time_string((time.time() if timestamp is None else timestamp)
                                            + args.clock_offset)

        def send_header(self, keyword, value):
            # A header value with a line break would end the header there and start another:
            # refused, so no text of a request reaches a header unchecked.
            if "\r" in str(value) or "\n" in str(value):
                raise ValueError(f"the fake refuses a {keyword} header holding a line break")
            super().send_header(keyword, value)

        def log_message(self, fmt, *a):
            sys.stderr.write("fake: " + (fmt % a) + "\n")

        def reply_json(self, status, body, route=None):
            raw = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Connection", "close")
            self.end_headers()
            if route is not None and args.trickle in (route, "all"):
                for i in range(len(raw)):
                    self.wfile.write(raw[i:i + 1])
                    self.wfile.flush()
                    time.sleep(args.trickle_gap)
                return
            self.wfile.write(raw)

        def do_GET(self):
            u = urllib.parse.urlsplit(self.path)
            # repos/<owner>/<repo>/actions/... : the route starts at index 3.
            parts = u.path.strip("/").split("/")[3:]
            if u.path.startswith("/blob/"):
                return self.blob(u)
            s = start()
            if len(parts) == 4 and parts[0:2] == ["actions", "runs"] and parts[3] == "artifacts":
                name = urllib.parse.parse_qs(u.query).get("name", [""])[0]
                arts = [] if args.missing or name != args.name else [{
                    "id": args.artifact_id, "name": args.name, "size_in_bytes": len(data),
                    "digest": digest or None, "expired": False,
                    "created_at": iso(s - args.artifact_ago)}]
                return self.reply_json(200, {"total_count": len(arts), "artifacts": arts}, "artifacts")
            if parts == ["actions", "artifacts", str(args.artifact_id), "zip"]:
                port = dead_port if args.refuse else self.server.server_address[1]
                with lock:
                    state["signed"].append(time.monotonic())
                    sig = len(state["signed"])
                self.send_response(302)
                self.send_header("Location", f"http://127.0.0.1:{port}/blob/{args.artifact_id}?sig={sig}")
                self.send_header("Content-Length", "0")
                self.send_header("Connection", "close")
                self.end_headers()
                return
            if len(parts) == 5 and parts[0:2] == ["actions", "runs"] and parts[3] == "attempts":
                return self.reply_json(200, {"id": 11, "run_attempt": int(parts[4]),
                                             "run_started_at": iso(s - args.run_ago)}, "attempt")
            if len(parts) == 6 and parts[0:2] == ["actions", "runs"] and parts[5] == "jobs":
                if args.jobs_status != 200:
                    return self.reply_json(args.jobs_status, {"message": "the fake's jobs API fails"})
                ours = parts[4] == str(args.attempt)
                jobs = [
                    {"id": 98, "name": "lint", "runner_name": "GitHub Actions 3",
                     "status": "in_progress", "started_at": iso(s - args.started_ago - 30)},
                    {"id": 96, "name": "binaries", "runner_name": args.runner,
                     "status": "completed", "started_at": iso(s - args.started_ago - 120)},
                    {"id": 99, "name": "shard (1)", "runner_name": args.runner,
                     "status": "in_progress" if ours else "completed",
                     "started_at": iso(s - args.started_ago - (0 if ours else 900))},
                ]
                if args.twin_job:
                    jobs.insert(0, {"id": 97, "name": "shard (2)", "runner_name": args.runner,
                                    "status": "in_progress", "started_at": iso(s - args.started_ago - 40)})
                return self.reply_json(200, {"total_count": len(jobs), "jobs": jobs}, "jobs")
            self.reply_json(404, {"message": "the fake serves no such route"})

        def blob(self, u):
            with lock:
                state["blob_requests"] += 1
                count = state["blob_requests"]
            rng = self.headers.get("Range", "")
            sys.stderr.write(f"fake: blob request {count} range '{rng}'\n")
            if args.silent:
                # Read the request, answer nothing, hold the socket until the client leaves.
                while self.rfile.read(1):
                    pass
                return
            if count <= args.refuse_urls:
                return self.reply_json(403, {"message": "AuthenticationFailed"})
            if args.url_life > 0:
                sig = urllib.parse.parse_qs(u.query).get("sig", ["0"])[0]
                with lock:
                    signed = state["signed"][int(sig) - 1] if sig.isdigit() and 0 < int(sig) <= len(state["signed"]) else None
                age = None if signed is None else time.monotonic() - signed
                if age is None or age > args.url_life:
                    sys.stderr.write(f"fake: blob request {count} refused: its URL is expired\n")
                    return self.reply_json(403, {"message": "AuthenticationFailed: expired"})
            if args.blob_status:
                return self.reply_json(args.blob_status, {"message": "the fake's storage fails"})
            if count <= args.fail_first:
                return self.reply_json(args.fail_status, {"message": "the fake's storage fails once"})
            start, end = 0, len(data) - 1
            if rng.startswith("bytes=") and not args.ignore_range:
                a, _, b = rng[len("bytes="):].partition("-")
                start, end = int(a), min(int(b) if b else len(data) - 1, len(data) - 1)
                if args.misalign or count <= args.misalign_first:
                    start = min(start + 1, end)
                self.send_response(206)
                self.send_header("Content-Range", f"bytes {start}-{end}/{len(data)}")
            else:
                self.send_response(200)
            self.send_header("Content-Type", "application/zip")
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Content-Length", str(end - start + 1))
            self.send_header("Connection", "close")
            self.end_headers()
            sent, pos = 0, start
            try:
                while pos <= end:
                    n = min(16 * 1024, end - pos + 1)
                    if 0 <= args.stall_after <= sent:
                        sys.stderr.write(f"fake: blob request {count} sent {sent} bytes, then silence\n")
                        while True:
                            time.sleep(3600)
                    if args.stall_after >= 0:
                        n = min(n, args.stall_after - sent)
                    throttle(n)
                    if args.slow_first > 0 and count == 1:
                        time.sleep(n / args.slow_first)
                    self.wfile.write(data[pos:pos + n])
                    pos += n
                    sent += n
            except (BrokenPipeError, ConnectionResetError):
                pass
            sys.stderr.write(f"fake: blob request {count} sent {sent} bytes\n")

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = True
    tmp = args.port_file + ".tmp"
    with open(tmp, "w") as f:
        f.write(str(server.server_address[1]))
    os.replace(tmp, args.port_file)
    server.serve_forever()


if __name__ == "__main__":
    main()
