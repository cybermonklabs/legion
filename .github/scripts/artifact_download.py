#!/usr/bin/env python3
"""Download one artifact of this workflow run, waiting for its bytes rather than for a clock.

  .github/scripts/artifact_download.py --name <artifact> --dest <directory> \
      --budget-of <workflow file>:<job id> --after <seconds>

The shard and alone jobs of .github/workflows/dispatch-browser.yaml run it in place of
actions/download-artifact, which reads the whole zip over one connection, starts again from
byte 0 after a read that fails or sends nothing for 30 s, and so throws away what it had read.
.github/ci-notes/dispatch-browser.md#what-the-shard-job-downloads-in-place-of-a-go-build says
what that cost and why this script is shaped as it is. What it does, in order:

 1. Works out when the download must have ended: the job's own start plus the job's
    `timeout-minutes`, read from the workflow file this run executes (--budget-of), less
    --after, the seconds the steps after this one take at their budgets and measured maxima.
    The job's start is read from the jobs API as the one in-progress job of this run attempt
    whose runner is $RUNNER_NAME, and its age is read on GitHub's clock alone: the answer's
    Date header less the job's started_at (runner_time_before), so a runner clock that differs
    from GitHub's moves nothing. When that start cannot be read (the API fails, no job or two
    jobs match, or the start it gives falls after this step began), the start is taken as the
    latest of two times GitHub records before it, read the same way, and the log says so: this
    run attempt's run_started_at, and the created_at of the artifact, which the job this one
    needs made before this job was queued. Each is earlier than the job's start, so the end
    worked out from it is never later than the true one. That end is the one bound this script
    waits under: the time the job's budget leaves the download, so no later step is cut short
    by it.
 2. Reads the artifact's id, size and digest from the REST API
    (`GET /repos/<repo>/actions/runs/<run>/artifacts?name=<artifact>`, the newest of that
    name), and refuses one with no digest: its bytes could not be checked.
 3. Reads the zip from the storage URL the API redirects to (`.../artifacts/<id>/zip`), in
    ranges, over CONNECTIONS connections at once. Each byte is asked for once and kept once: a
    connection that fails, closes early or sends nothing for --silence seconds ends, and the
    rest of its range is read on a new connection from the byte it stopped at. An idle
    connection takes half of the largest range still being read, so one slow connection does
    not set the pace; the storage may go on sending the busy connection's old range past its
    new end until that connection closes, and those bytes are dropped. It goes on for as long
    as the deadline allows, whatever the rate. Each request logs one line: the range asked
    for, the status and Content-Range answered (206 for a range; a 200, the whole zip, and a
    206 for other bytes are refused and logged as such), the bytes kept, the time and how it
    ended; the last line gives the whole transfer's rate.
 4. Checks the zip's SHA-256 against the digest the upload recorded, extracts it into a new
    directory beside --dest, and only then moves that directory to --dest, so a failed
    extraction leaves no file there.

Every API read is held to a deadline, its body included: the download's own, or, for the reads
that must come before the download's deadline is known (the jobs read, and in the fallback the
artifacts and run-attempt reads), EARLY_READ of their own. It fails by name: at once for an
artifact this run does not hold or that records no digest, a token the API refuses, a storage
that refuses freshly signed URLs, or a digest that differs; and at the deadline otherwise, with
the bytes that arrived, the requests it made and what the storage answered them. It exits 0 on
success, 1 on a failure, 2 on a usage error.

Its environment is a workflow step's: GH_TOKEN (the job's token, which needs
`actions: read`), GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT, RUNNER_NAME and
RUNNER_TEMP, with GITHUB_API_URL for the API's address (https://api.github.com when unset).
Its test, .github/scripts/artifact_download.test.sh, points GITHUB_API_URL at a local server.
It needs python3 and PyYAML, which this repository's browser image and GitHub's ubuntu-24.04
runner both carry, and no other package.
"""
import argparse
import datetime
import email.utils
import hashlib
import http.client
import json
import os
import shutil
import sys
import tempfile
import threading
import time
import urllib.parse
import zipfile
import zlib

# Connections read at once. A choice, not a measurement: with eight, one slow connection holds
# an eighth of the bytes until an idle one takes half of what it has left, and eight TLS
# handshakes cost little next to a read (a response's headers arrived 0.20 to 0.34 s after the
# read began in the three slow reads the notes name).
CONNECTIONS = 8
# A range is not split below this: four reads of READ_SIZE, so a split costs a request for at
# least a quarter of a mebibyte, and a slow connection is left holding at most twice this, which
# the slowest read that completed (248,764 B/s, the notes' 427 s read) moves in 2 s.
MIN_SPLIT = 256 * 1024
READ_SIZE = 64 * 1024
# How long a connection may send nothing before it is ended and its range read again from where
# it stopped: GitHub's own figure for the same read, actions/download-artifact v8.0.1's
# "Blob storage chunk did not respond in 30000ms" (its dist/index.js, streamExtractExternal).
SILENCE = 30
# How long the storage URL the API redirects to lives: GitHub's REST reference for "Download an
# artifact" (https://docs.github.com/en/rest/actions/artifacts#download-an-artifact) says "This
# URL expires after 1 minute" (one URL read on 2026-11-20 was signed for ten, its st and se
# 14:02:52Z to 14:12:57Z, which the reference does not promise). A URL is read again before a new
# request once half of that minute has passed since the request that signed it went out: the
# half left is for a storage clock ahead of the API's, and a URL the storage refuses anyway is
# signed again on its 403 (Signer.refused).
URL_LIFE = 60
# Waits after a request that ended early: 1, 2, 4 and then 8 s, never past the deadline.
BACKOFF_CAP = 8
PROGRESS_EVERY = 15
# Freshly signed URLs the storage may refuse in a row before the read fails by name.
REFUSED_URLS = 3
# The resolution of the two GitHub times an age is read from. GitHub writes both in whole
# seconds, the HTTP Date header (RFC 9110's IMF-fixdate, "Fri, 20 Nov 2026 14:02:52 GMT") and the
# API's ISO 8601 times (started_at, run_started_at, created_at: "2026-11-20T14:02:52Z"), so each
# may stand up to 1 s from the moment it names, and the age may be 2 s short.
RESOLUTION = 2
# How long each API read made before the deadline is known may take, its waits on a 5xx or a
# rate limit included: the jobs read, and in the fallback the artifacts and run-attempt reads,
# which share one. In the 14 legs of runs 44758704517 and 44773074892 the jobs read took at most
# 2.9 s, counted from the step's first log line to the line that gives the deadline, so with the
# process's own start (the artifacts read took at most 0.44 s), and this repository's rule for a
# bound is the measured maximum plus 15 per cent, rounded up: 2.9 x 1.15 = 3.3 -> 4 s. Before the
# deadline is known the step so spends at most 2 x 4 = 8 s on the API, of the shard job's 1,320 s
# and the alone job's 900 s; after that, every read is held to the deadline.
EARLY_READ = 4


class Refused(Exception):
    """A failure no wait can change: the read ends at once, with this message."""


class Expired(Exception):
    """The storage refused a signed URL; the next request reads a fresh one."""


class Reply:
    """An API answer: its status, headers and body, and `sent`, the runner's clock when the
    request went out, with `took`, the seconds until its body had arrived."""
    __slots__ = ("status", "headers", "body", "sent", "took")

    def __init__(self, status, headers, body, sent, took):
        self.status, self.headers, self.body, self.sent, self.took = status, headers, body, sent, took


def now():
    return time.time()


def iso(epoch):
    return datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime("%H:%M:%SZ")


def log(message):
    print(f"artifact_download: {message}", flush=True)


def github_time(text):
    """An ISO 8601 time as the API writes it ("2026-11-20T14:02:52Z"), as an epoch."""
    return datetime.datetime.fromisoformat(text.replace("Z", "+00:00")).timestamp()


def runner_time_before(reply, at):
    """A time on the runner's clock that is no later than `at`, a time GitHub gave in `reply`,
    read on GitHub's clock alone. `reply`'s Date header less `at` is how long before the answer
    `at` was; GitHub stamped that Date after the request went out, so the same span taken back
    from `sent`, the runner's clock at that moment, is no later than `at`, whatever the runner's
    clock reads against GitHub's. RESOLUTION allows for the two times' whole seconds. Returns
    None when the answer has no Date header."""
    date = reply.headers.get("Date")
    if not date:
        return None
    answered = email.utils.parsedate_to_datetime(date).timestamp()
    return reply.sent - (answered - github_time(at)) - RESOLUTION


def request(url, headers, timeout):
    """One GET, following no redirect. Returns (connection, response); the caller closes."""
    parts = urllib.parse.urlsplit(url)
    cls = http.client.HTTPSConnection if parts.scheme == "https" else http.client.HTTPConnection
    conn = cls(parts.hostname, parts.port, timeout=timeout)
    target = parts.path or "/"
    if parts.query:
        target += "?" + parts.query
    try:
        conn.request("GET", target, headers=headers)
        return conn, conn.getresponse()
    except BaseException:
        conn.close()
        raise


def bounded(fn, deadline):
    """fn() on a thread of its own, held to the deadline: returns what fn returns or raises what
    it raises, and raises TimeoutError once the deadline passes first. A socket's timeout bounds
    each wait for a byte but not an answer sent a byte at a time, so this holds the whole of one:
    the connection, the headers and the body. A thread left behind writes nothing, and ends with
    its socket or with the process."""
    result, done = [], threading.Event()

    def target():
        try:
            result.append((True, fn()))
        except BaseException as e:
            result.append((False, e))
        done.set()

    threading.Thread(target=target, daemon=True).start()
    if not done.wait(max(0.0, deadline - now())):
        raise TimeoutError("the deadline passed while the answer was still arriving")
    ok, value = result[0]
    if not ok:
        raise value
    return value


class Api:
    def __init__(self, base, repo, token):
        self.base = base.rstrip("/")
        self.repo = repo
        self.headers = {
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "legion-ci-artifact-download",
        }

    def get(self, path, timeout):
        sent = now()
        conn, resp = request(self.base + path, self.headers, timeout)
        try:
            body = resp.read()
        finally:
            conn.close()
        return Reply(resp.status, resp.headers, body, sent, now() - sent)

    def call(self, path, deadline, want):
        """GET <path> until it answers one of `want`, a refusal, or the deadline passes; each
        answer is held to the deadline whole (bounded). Returns a Reply."""
        backoff = 1
        last = "no request made"
        while True:
            left = deadline - now()
            if left <= 0:
                raise TimeoutError(f"{path}: the API did not answer in time (last: {last})")
            try:
                reply = bounded(lambda: self.get(path, min(SILENCE, left)), deadline)
                if reply.status in want:
                    return reply
                # A refusal no wait changes; a 403 that is a rate limit, a 429 and a 5xx are
                # waited out.
                if reply.status in (401, 404, 410) or (
                        reply.status == 403 and b"rate limit" not in reply.body.lower()):
                    raise Refused(f"the API answered {path} with HTTP {reply.status}: "
                                  f"{reply.body[:300].decode('utf-8', 'replace')}")
                last = f"HTTP {reply.status}"
            except (OSError, http.client.HTTPException) as e:
                last = f"{type(e).__name__}: {e}"
            time.sleep(max(0, min(backoff, deadline - now())))
            backoff = min(backoff * 2, BACKOFF_CAP)

    def json(self, path, deadline):
        reply = self.call(path, deadline, (200,))
        return json.loads(reply.body), reply

    def location(self, artifact_id, deadline):
        """(url, sent): the storage URL the API redirects to, and when the request that signed
        it went out."""
        reply = self.call(f"/repos/{self.repo}/actions/artifacts/{artifact_id}/zip", deadline, (302,))
        url = reply.headers.get("Location")
        if not url:
            raise Refused(f"the API's redirect for artifact {artifact_id} names no Location")
        return url, reply.sent


class Signer:
    """The storage URL, read again when half its life has passed or after the storage refused
    it."""

    def __init__(self, api, artifact_id, deadline, url_life):
        self.api, self.artifact_id, self.deadline = api, artifact_id, deadline
        self.refresh = url_life / 2
        self.lock = threading.Lock()
        self.url_, self.at, self.serial = None, 0.0, 0
        # The signings the storage refused while younger than self.refresh, since it last served
        # one: counted by signing, since several connections can be refused the same one.
        self.refused_fresh = set()

    def url(self):
        """(url, signing): the URL to read and the number of the signing that made it."""
        with self.lock:
            if self.url_ is None or now() - self.at >= self.refresh:
                self.url_, self.at = self.api.location(self.artifact_id, self.deadline)
                self.serial += 1
            return self.url_, self.serial

    def refused(self, signing):
        with self.lock:
            if signing == self.serial and now() - self.at < self.refresh:
                self.refused_fresh.add(signing)
            if len(self.refused_fresh) >= REFUSED_URLS:
                raise Refused(f"the storage refused {REFUSED_URLS} freshly signed URLs in a row "
                              "with HTTP 403")
            if signing == self.serial:
                self.url_ = None

    def served(self):
        with self.lock:
            self.refused_fresh.clear()


class Part:
    __slots__ = ("pos", "end", "active")

    def __init__(self, start, end):
        self.pos, self.end, self.active = start, end, False


# What the storage did with a request that kept no byte, by kind, for the error at the deadline.
NO_ANSWER = "had no answer"
WHOLE_ZIP = "were answered HTTP 200 with the whole zip, not the range asked for"
OTHER_BYTES = "were answered 206 with other bytes than those asked for"


class Reader:
    """Reads [0, size) of the signed URL into fd over CONNECTIONS threads, under one lock."""

    def __init__(self, size, signer, fd, deadline, silence):
        self.size, self.signer, self.fd = size, signer, fd
        self.deadline, self.silence = deadline, silence
        self.cond = threading.Condition()
        step = max(MIN_SPLIT, -(-size // CONNECTIONS))
        self.parts = [Part(s, min(s + step, size)) for s in range(0, size, step)] or [Part(0, 0)]
        self.received = 0
        self.requests = 0
        self.ended_early = 0
        self.last_error = "no request ended early"
        self.refusal = None
        # Requests that kept no byte, by what the storage did: NO_ANSWER, WHOLE_ZIP, OTHER_BYTES or
        # "were answered HTTP <status>", with the last error of each kind.
        self.unused = {}
        # Requests that ended, so that requests - finished is how many still waited when the
        # deadline passed.
        self.finished = 0

    def done(self):
        return all(p.pos >= p.end for p in self.parts)

    def claim(self):
        for p in self.parts:
            if not p.active and p.pos < p.end:
                p.active = True
                return p
        busy = [p for p in self.parts if p.active and p.end - p.pos >= 2 * MIN_SPLIT]
        if not busy:
            return None
        widest = max(busy, key=lambda p: p.end - p.pos)
        mid = widest.pos + (widest.end - widest.pos) // 2
        taken = Part(mid, widest.end)
        widest.end = mid
        taken.active = True
        self.parts.append(taken)
        return taken

    def open_connections(self):
        return sum(1 for p in self.parts if p.active)

    def worker(self):
        backoff = 1
        while True:
            with self.cond:
                while True:
                    if self.refusal or self.done() or now() >= self.deadline:
                        self.cond.notify_all()
                        return
                    part = self.claim()
                    if part is not None:
                        break
                    self.cond.wait(0.5)
            added = [0]
            try:
                self.fetch(part, added)
                backoff = 1
            except Refused as e:
                with self.cond:
                    self.refusal = self.refusal or str(e)
                    self.cond.notify_all()
                return
            except (Expired, OSError, http.client.HTTPException, ValueError) as e:
                with self.cond:
                    self.ended_early += 1
                    self.last_error = str(e) if isinstance(e, Expired) else f"{type(e).__name__}: {e}"
                # A request that moved bytes is followed at once; one that moved none waits,
                # longer each time, so a refusing service is not asked in a tight loop.
                if added[0]:
                    backoff = 1
                else:
                    time.sleep(max(0, min(backoff, self.deadline - now())))
                    backoff = min(backoff * 2, BACKOFF_CAP)
            finally:
                with self.cond:
                    part.active = False
                    self.cond.notify_all()

    def fetch(self, part, added):
        """Reads part from part.pos until its end, which a split may move nearer, adding to
        added[0] as bytes land. Returns when the part is whole or the deadline has passed, and
        raises on a response that refuses or ends short. The socket's timeout, --silence, bounds
        each wait for a byte; the deadline itself is held by run(), which stops waiting on the
        workers when it passes. Every request ends in one log line: the range it asked for, the
        status and Content-Range the storage answered, the bytes it kept, its time and how it
        ended, so a run's log shows what the storage did with each range."""
        with self.cond:
            start, end = part.pos, part.end
        if start >= end or now() >= self.deadline:
            return
        with self.cond:
            self.requests += 1
            number = self.requests
        began, conn, status, answered, ended = now(), None, None, "", "its range is whole"
        unused = None
        try:
            url, signing = self.signer.url()
            conn, resp = request(url, {"Range": f"bytes={start}-{end - 1}",
                                       "User-Agent": "legion-ci-artifact-download"},
                                 min(self.silence, max(1.0, self.deadline - now())))
            status, answered = resp.status, resp.headers.get("Content-Range", "")
            if resp.status == 403:
                self.signer.refused(signing)
                raise Expired("the storage refused its signed URL with HTTP 403")
            if resp.status in (404, 410):
                raise Refused(f"the storage answered HTTP {resp.status}: the artifact's zip is gone")
            if resp.status == 200:
                unused = WHOLE_ZIP
                raise ConnectionError("the storage answered a range request with HTTP 200, the "
                                      "whole zip and not the range")
            if resp.status != 206:
                unused = f"were answered HTTP {resp.status}"
                raise ConnectionError(f"the storage answered a range request with HTTP {resp.status}")
            if not answered.startswith(f"bytes {start}-"):
                unused = OTHER_BYTES
                raise ConnectionError(f"the storage answered bytes {start}- with '{answered}'")
            self.signer.served()
            while now() < self.deadline:
                chunk = resp.read1(READ_SIZE)
                with self.cond:
                    if not chunk:
                        raise ConnectionError(f"the response ended {part.end - part.pos:,} bytes "
                                              "short of its range")
                    use = min(len(chunk), part.end - part.pos)
                    if use > 0:
                        os.pwrite(self.fd, chunk[:use], part.pos)
                        part.pos += use
                        self.received += use
                        added[0] += use
                    if part.pos >= part.end:
                        self.cond.notify_all()
                        return
            ended = "the deadline passed"
        except BaseException as e:
            ended = str(e) if isinstance(e, (Expired, Refused)) else f"{type(e).__name__}: {e}"
            raise
        finally:
            if conn is not None:
                conn.close()
            took = now() - began
            # A request that kept no byte counts toward what the storage did, unless its URL was
            # refused (that is signed again) or the zip was gone (that fails at once).
            with self.cond:
                self.finished += 1
                if not added[0] and (unused or status in (None, 206)):
                    kind = unused or (NO_ANSWER if status is None else
                                      "were answered 206 and then sent no byte of their range")
                    count, _ = self.unused.get(kind, (0, ""))
                    self.unused[kind] = (count + 1, ended)
            log(f"request {number}: bytes={start}-{end - 1} answered "
                f"{status if status is not None else 'nothing'}{' ' + answered if answered else ''}, "
                f"{added[0]:,} bytes in {took:.1f} s ({added[0] / max(1e-9, took) / 1e6:.2f} MB/s): {ended}")

    def unused_answers(self):
        """What the storage did with the requests that kept no byte, in words, and whether it
        answered any of them at all. A request still waiting when the deadline passed had no
        answer yet, and is named so."""
        with self.cond:
            parts = [f"{count} {kind} (the last: {last})" for kind, (count, last) in self.unused.items()]
            waiting = self.requests - self.finished
            answered = any(k != NO_ANSWER for k in self.unused)
        if waiting:
            parts.append(f"{waiting} were still waiting for an answer when the deadline passed")
        return "; ".join(parts) or "none was answered", answered

    def run(self, name):
        threads = [threading.Thread(target=self.worker, daemon=True) for _ in range(CONNECTIONS)]
        started = now()
        for t in threads:
            t.start()
        next_report, last_received, last_at = started + PROGRESS_EVERY, 0, started
        with self.cond:
            while not (self.refusal or self.done() or now() >= self.deadline):
                self.cond.wait(min(1.0, max(0.0, self.deadline - now())))
                if now() >= next_report and not self.done():
                    rate = (self.received - last_received) / max(1e-9, now() - last_at)
                    log(f"{name}: {self.received:,} of {self.size:,} bytes "
                        f"({100 * self.received // max(1, self.size)}%) after {now() - started:.0f} s, "
                        f"{rate / 1000:.0f} kB/s over the last {now() - last_at:.0f} s, "
                        f"{self.open_connections()} connections open, {self.requests} requests, "
                        f"{self.ended_early} ended early")
                    next_report, last_received, last_at = now() + PROGRESS_EVERY, self.received, now()
            return now() - started


def read_budget(spec):
    import yaml
    path, sep, job = spec.rpartition(":")
    if not sep or not path or not job:
        usage(f"--budget-of takes <workflow file>:<job id>, not '{spec}'")
    with open(path) as f:
        doc = yaml.safe_load(f)
    minutes = ((doc.get("jobs") or {}).get(job) or {}).get("timeout-minutes")
    if not isinstance(minutes, int) or minutes <= 0:
        usage(f"{path} gives job '{job}' no whole number of timeout-minutes (it holds {minutes!r})")
    return minutes * 60


def job_start(api, repo, deadline, begun):
    """(start, how): the job's own start as a time on the runner's clock no later than it, and the
    words that say where it came from; or (None, why it could not be read)."""
    runner = os.environ.get("RUNNER_NAME", "")
    run, attempt = os.environ.get("GITHUB_RUN_ID", ""), os.environ.get("GITHUB_RUN_ATTEMPT", "1")
    if not runner:
        return None, "RUNNER_NAME is not set"
    try:
        matched = []
        for page in range(1, 11):
            data, reply = api.json(f"/repos/{repo}/actions/runs/{run}/attempts/{attempt}/jobs"
                                   f"?per_page=100&page={page}", deadline)
            jobs = data.get("jobs") or []
            matched += [(job, reply) for job in jobs
                        if job.get("runner_name") == runner and job.get("status") == "in_progress"]
            if len(jobs) < 100:
                break
        if not matched:
            return None, f"no in-progress job of attempt {attempt} ran on runner '{runner}'"
        if len(matched) > 1:
            ids = ", ".join(str(job.get("id")) for job, _ in matched)
            return None, (f"{len(matched)} in-progress jobs of attempt {attempt} ran on runner "
                          f"'{runner}' ({ids}), so which one this is cannot be told")
        job, reply = matched[0]
        what = f"job {job.get('id')} '{job.get('name')}'"
        if not job.get("started_at"):
            return None, f"the jobs API gives {what} no started_at"
        start = runner_time_before(reply, job["started_at"])
        if start is None:
            return None, "the jobs API's answer has no Date header to read the job's age on"
        if start > begun:
            return None, (f"the jobs API gives {what} a start of {job['started_at']}, after this "
                          f"step began by its own answer's Date, {reply.headers.get('Date')}")
        return start, (f"{what}, whose runner_name '{job.get('runner_name')}' is this step's "
                       f"RUNNER_NAME, started at {job['started_at']}, "
                       f"{reply.sent - start - RESOLUTION:.0f} s before its answer's Date on "
                       f"GitHub's clock (the answer took {reply.took:.2f} s)")
    except (Refused, TimeoutError, TypeError, ValueError, KeyError) as e:
        return None, str(e)


def find_artifact(api, repo, run, name, deadline):
    """(artifact, reply): the newest unexpired artifact of the name in this run, and the answer
    that listed it."""
    query = urllib.parse.urlencode({"name": name, "per_page": 100})
    data, reply = api.json(f"/repos/{repo}/actions/runs/{run}/artifacts?{query}", deadline)
    found = [a for a in data.get("artifacts") or [] if a.get("name") == name and not a.get("expired")]
    if not found:
        raise Refused(f"run {run} holds no unexpired artifact named '{name}' "
                      f"(the API listed {data.get('total_count', 0)} of that name)")
    return max(found, key=lambda a: (a.get("created_at") or "", a.get("id") or 0)), reply


def start_floor(api, repo, run, artifact, listed, deadline, begun):
    """(start, how): when the job's own start cannot be read, the latest of two times GitHub
    records before it, as a time on the runner's clock no later than it (runner_time_before):
    this run attempt's run_started_at, and the artifact's created_at, made by the job this one
    needs, which ended before this one was queued. A time that falls after this step began
    cannot be one of those and is left out. (None, why) when neither is read."""
    attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "1")
    found, missed = [], []
    try:
        data, reply = api.json(f"/repos/{repo}/actions/runs/{run}/attempts/{attempt}", deadline)
        candidates = [(f"run attempt {attempt}'s run_started_at", data.get("run_started_at"), reply)]
    except (Refused, TimeoutError, ValueError) as e:
        candidates, missed = [], [f"run attempt {attempt}'s start ({e})"]
    candidates.append((f"artifact {artifact.get('id')}'s created_at", artifact.get("created_at"), listed))
    for what, at, reply in candidates:
        try:
            start = runner_time_before(reply, at) if at else None
        except (TypeError, ValueError):
            start = None
        if start is None or start > begun:
            missed.append(f"{what} ({at or 'not given'})")
        else:
            found.append((start, f"{what}, {at} (the answer took {reply.took:.2f} s)"))
    if not found:
        return None, "neither time read: " + "; ".join(missed)
    start, how = max(found)
    return start, how + (f" (unread: {'; '.join(missed)})" if missed else "")


def usage(message):
    print(f"::error::artifact_download: {message}", file=sys.stderr, flush=True)
    sys.exit(2)


def fail(message):
    stopped = now()
    print(f"artifact_download: stopped at {iso(stopped)} (epoch {stopped:.2f})", flush=True)
    print(f"::error::artifact_download: {message}", flush=True)
    sys.stdout.flush()
    os._exit(1)


def place(zip_path, dest, name):
    """Extracts the zip into a new directory beside dest and moves it to dest whole, so no file
    reaches dest unless every file was extracted. Returns the names extracted."""
    parent = os.path.dirname(os.path.abspath(dest))
    os.makedirs(parent, exist_ok=True)
    staging = tempfile.mkdtemp(prefix=f".{os.path.basename(dest)}-", dir=parent)
    try:
        os.chmod(staging, 0o755)
        with zipfile.ZipFile(zip_path) as z:
            names = z.namelist()
            z.extractall(staging)
        if os.path.isdir(dest):
            os.rmdir(dest)
        os.rename(staging, dest)
        return names
    except (OSError, EOFError, zlib.error, zipfile.BadZipFile, zipfile.LargeZipFile) as e:
        shutil.rmtree(staging, ignore_errors=True)
        fail(f"{name}: the zip matched its digest but could not be extracted into {dest}, which "
             f"holds nothing of it: {type(e).__name__}: {e}")


def main():
    begun = now()
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--name", required=True)
    parser.add_argument("--dest", required=True)
    parser.add_argument("--budget-of", required=True)
    parser.add_argument("--after", type=int, required=True)
    parser.add_argument("--silence", type=float, default=SILENCE)
    parser.add_argument("--url-life", type=float, default=URL_LIFE)
    args = parser.parse_args()
    env = {k: os.environ.get(k, "") for k in ("GH_TOKEN", "GITHUB_REPOSITORY", "GITHUB_RUN_ID")}
    missing = [k for k, v in env.items() if not v]
    if missing:
        usage(f"{', '.join(missing)} not set; a workflow step sets GITHUB_REPOSITORY and "
              "GITHUB_RUN_ID, and the step's env passes GH_TOKEN: ${{ github.token }}")
    if os.path.lexists(args.dest) and not (os.path.isdir(args.dest) and not os.listdir(args.dest)):
        usage(f"--dest {args.dest} holds files already; this script places the artifact in a "
              "directory of its own")
    repo, run = env["GITHUB_REPOSITORY"], env["GITHUB_RUN_ID"]
    budget = read_budget(args.budget_of)
    api = Api(os.environ.get("GITHUB_API_URL") or "https://api.github.com", repo, env["GH_TOKEN"])

    # No deadline is known before the job's start is read, so these reads are held to EARLY_READ
    # of their own.
    artifact = None
    start, how = job_start(api, repo, now() + EARLY_READ, begun)
    if start is not None:
        found = f"{how}; "
    else:
        cap = now() + EARLY_READ
        try:
            artifact, listed = find_artifact(api, repo, run, args.name, cap)
        except TimeoutError as e:
            fail(f"{args.name}: the job's start could not be read ({how}), and the API never "
                 f"listed the run's artifacts either ({e})")
        except Refused as e:
            fail(f"{args.name}: {e}")
        start, floor = start_floor(api, repo, run, artifact, listed, cap, begun)
        if start is None:
            fail(f"{args.name}: the job's start could not be read ({how}), nor any time GitHub "
                 f"records before it ({floor}), so what the job's budget leaves is not known")
        found = (f"the job's start could not be read ({how}), so the latest time GitHub records "
                 f"before it stands for it: {floor}; ")
    deadline = start + budget - args.after
    log(f"{args.name}: must have arrived by {iso(deadline)} (epoch {deadline:.2f}), "
        f"{deadline - now():.0f} s from now: {found}{args.budget_of} gives the job "
        f"{budget // 60} min, and the steps after this one take {args.after} s at their budgets "
        "and maxima")
    if deadline - now() <= 0:
        fail(f"{args.name}: this job's {budget // 60}-minute budget ({args.budget_of}) leaves "
             f"the download no time: the steps after it take {args.after} s, and the job "
             f"started {now() - start:.0f} s ago at most")

    if artifact is None:
        try:
            artifact, _ = find_artifact(api, repo, run, args.name, deadline)
        except TimeoutError as e:
            fail(f"{args.name}: the API never listed the run's artifacts before the deadline ({e})")
        except Refused as e:
            fail(f"{args.name}: {e}")
    size, digest = int(artifact["size_in_bytes"]), artifact.get("digest") or ""
    log(f"{args.name}: artifact {artifact['id']}, {size:,} bytes, digest {digest or 'none recorded'}")
    if not digest:
        fail(f"{args.name}: the API records no digest for artifact {artifact['id']}, so its bytes "
             "could not be checked before they are extracted; the actions/upload-artifact v7.0.1 "
             "the binaries job runs sends one with every upload")

    tmpdir = os.environ.get("RUNNER_TEMP") or tempfile.gettempdir()
    fd, zip_path = tempfile.mkstemp(prefix=f"artifact-{artifact['id']}-", suffix=".zip", dir=tmpdir)
    os.ftruncate(fd, size)
    signer = Signer(api, artifact["id"], deadline, args.url_life)
    reader = Reader(size, signer, fd, deadline, args.silence)
    took = reader.run(args.name)
    if reader.refusal or not reader.done():
        given_up = now()
        log(f"{args.name}: gave up at {iso(given_up)} (epoch {given_up:.2f}), "
            f"{given_up - deadline:+.1f} s from the deadline")
    if reader.refusal:
        fail(f"{args.name}: {reader.refusal}; {reader.received:,} of {size:,} bytes had arrived")
    if not reader.done():
        rate = reader.received / max(1e-9, took)
        if reader.received == 0:
            what, answered = reader.unused_answers()
            fail(f"the artifact service sent no usable byte of {args.name} ({size:,} bytes) in the "
                 f"{took:.0f} s this job's budget left the download: {reader.requests} requests, "
                 f"of which {what}. "
                 + ("No answer the storage gave could be used" if answered else
                    "The service did not answer")
                 + "; nothing the change under test did is read here")
        fail(f"{args.name} did not arrive in the {took:.0f} s this job's budget left it: "
             f"{reader.received:,} of {size:,} bytes at {rate / 1000:.0f} kB/s over "
             f"{reader.requests} requests, {reader.ended_early} of them ended early (the last in "
             f"{reader.last_error}). The artifact service was too slow for this leg; nothing the "
             "change under test did is read here")
    os.close(fd)

    sha = hashlib.sha256()
    with open(zip_path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            sha.update(block)
    got = "sha256:" + sha.hexdigest()
    if got != digest:
        fail(f"{args.name}: the zip read is {got}, not the {digest} its upload recorded")
    names = place(zip_path, args.dest, args.name)
    os.unlink(zip_path)
    log(f"{args.name}: {size:,} bytes in {took:.1f} s over {reader.requests} requests on "
        f"{CONNECTIONS} connections ({size / max(1e-9, took) / 1e6:.1f} MB/s), "
        f"{reader.ended_early} ended early; {got} matches the digest its upload recorded; "
        f"{len(names)} files into {args.dest}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
