#!/usr/bin/env python3
"""An HTTP proxy that injects the download fault this repository's retries answer.

It serves plain HTTP (an absolute-URI request line) and HTTPS (CONNECT), because a fetch of a
tool reaches a proxy as either one: curl of a release asset over https, `bun install` of a
registry, `go mod download` of a module proxy, apt over http. Three modes:

  fail     answer HTTP 504 to every request for a named host, for the whole run
  recover  answer HTTP 504 for --recover-after seconds, then pass the request on
  stall    accept the connection and send nothing at all, so each attempt of the client ends at
           its own time bound rather than at an answer

A host the --hosts list does not name is passed on in every mode, so a step can fault the host
under test and still reach the rest of the network — the Actions service among it, which a job
that cannot report is no proof of anything. An empty --hosts faults every host.

    .github/scripts/ci-fault-proxy.py --mode recover --recover-after 60 \\
        --hosts registry.npmjs.org --port 0 --port-file "$RUNNER_TEMP/proxy.port"

--port 0 takes a free port from the kernel and --port-file writes it, so a workflow step starts
the proxy, reads the port and sets http_proxy and https_proxy for the step under test. The file
is written whole, by rename, so a reader never sees half a port; by default the recovery window
starts the moment it appears, which is the first moment a caller can use the proxy, and is one
window for every host.

--per-host-window gives --mode recover one window per host instead. Each host the --hosts list
faults answers 504 for --recover-after seconds counted from that host's own first request, and is
passed on at every request after its window closes: a window opens once per host and never
again. "A host" is the host a request names, lower-cased: the host of a CONNECT's authority or of
a plain request's absolute URI, without its port, and not the --hosts entry it matched. An entry
covers every host under it (golang.org covers proxy.golang.org, sum.golang.org and
google.golang.org), and one window per entry would open at the first of them and could close
before a client first asks for the next, which would then never meet the fault. A step that first
reaches a host minutes in, as a docker build reaches the hosts of its last RUN, still meets that
host's fault on its first request. When a host's window opens it logs one line,
`window <host> opened <seconds>s after the proxy started`, on the same clock as the window. The
other two modes have no window, so they refuse the flag, and a caller that does not pass it keeps
the one window.

--coalesce-outage-gap <G> gives --mode recover outages that the hosts of one call share, in
place of a window per host. A real outage is one stretch of time, and a retried call that first
reaches its later hosts only after it recovers (`go mod download` asks the module proxy, then
sum.golang.org, then the hosts its modules name) would meet a fresh window at each of them: one
outage turned into a series that the call runs out of attempts in. So the first request of a
faulted host (the host a request names, lower-cased, as above) opens an outage when none is
open: outage 1, then 2, each --recover-after seconds long from that request. A host first
reached while an outage is open joins it: it answers 504 until that outage ends and is passed on
after. A host first reached less than G seconds after an outage ended is the same call going on:
it is passed on at every request, and its line names it. The first host reached G seconds or
more after an outage ended opens the next one. A host's first request decides it for the whole
run: no later request faults it again, extends an outage, or extends the G seconds. Each first
request logs the `window` line above, then `outage <N> opened <t>s after the proxy started,
ending <u>s after` when it opens one, then `host <host> joined outage <N>` or `host <host> not
faulted: after outage <N>, within <G>s`. It needs a --hosts list, since with every host faulted
any request at all would open an outage, and it refuses --per-host-window. With --probe-host
<name>, it answers that one host 504 at every request and leaves the outages alone: the host opens
and joins none, and the proxy logs no line for it but its 504. A rig's probe then proves the
proxy before the site runs, and outage 1 opens at the site's own first request.

--bind is the address it listens on, 127.0.0.1 by default. A `RUN` of a docker build runs in a
network namespace of its own, where 127.0.0.1 is the build container and not the runner, so a
build reaches this proxy only on an address the container can route to: the docker bridge
gateway, which the download-retry-images job and the five runs of the download-retry-images-worker
job of .github/workflows/ci-job-download-retry.yaml bind to and pass into the build as http_proxy.
Nothing else changes: a caller that names no --bind listens where it always did.

It makes no fetch of its own: every byte it sends upstream is one a client sent it. It is Python
3 from the runner image, with nothing to install.
"""

from __future__ import annotations

import argparse
import os
import select
import socket
import socketserver
import sys
import threading
import time
from collections.abc import Callable

READ_SIZE = 65536
HEADER_LIMIT = 65536


def log(message: str) -> None:
    sys.stderr.write(f"ci-fault-proxy: {message}\n")
    sys.stderr.flush()


def plain_seconds(value: float) -> str:
    """A number of seconds as a person writes it: 5 for 5.0, and 2.5 for 2.5."""
    return str(int(value)) if value.is_integer() else str(value)


class Fault:
    """Which hosts fail, how they fail, and until when."""

    def __init__(
        self,
        mode: str,
        hosts: list[str],
        recover_after: float,
        per_host_window: bool = False,
        coalesce_gap: float | None = None,
        probe_host: str | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.mode = mode
        self.hosts = [h.lower() for h in hosts]
        self.recover_after = recover_after
        self.per_host_window = per_host_window
        # The clock every window and outage is read on. The proxy keeps time.monotonic; a test
        # passes a clock it sets by hand, so it can read each decision at the second it names.
        self.clock = clock
        self.started = clock()
        # Under --per-host-window, the moment of each faulted host's first request, on the clock
        # of `started`. A window is read from it and never reset, so a host's window opens once.
        # The server runs a thread per connection, so two first requests for one host can arrive
        # together: the lock makes exactly one of them the first.
        self.first_request: dict[str, float] = {}
        self.lock = threading.Lock()
        # Under --coalesce-outage-gap: the gap, the host --probe-host leaves out of every outage,
        # the number of the latest outage (0 before the first opens) and its end on the clock of
        # `started`, and each host's decision, made at its first request and kept for the run: the
        # number of the outage it joined, or ("grace", N, gap) for a host first reached within the
        # gap after outage N ended. The lock above guards all of it.
        self.coalesce_gap = coalesce_gap
        self.probe_host = probe_host.lower() if probe_host else None
        self.outage = 0
        self.outage_end = 0.0
        self.decision: dict[str, int | tuple[str, int, float]] = {}

    def faults(self, host: str) -> bool:
        host = host.lower()
        if host == self.probe_host:
            # Refused at every request, and no part of any outage: it opens, joins and logs none.
            return True
        if self.hosts and not any(host == h or host.endswith("." + h) for h in self.hosts):
            return False
        if self.mode != "recover":
            return True
        if self.coalesce_gap is not None:
            return self.in_outage(host)
        if not self.per_host_window:
            return (self.clock() - self.started) < self.recover_after
        with self.lock:
            now = self.clock()
            opened = self.first_request.get(host)
            if opened is None:
                opened = self.first_request[host] = now
                # Logged under the lock, so this line comes before the line of any request this
                # window answers, the first among them.
                log(f"window {host} opened {now - self.started:.1f}s after the proxy started")
        return (now - opened) < self.recover_after

    def in_outage(self, host: str) -> bool:
        """Under --coalesce-outage-gap: whether this request of a faulted host falls in the outage
        that the host's first request joined. That first request decides the host for the run."""
        with self.lock:
            now = self.clock()
            decision = self.decision.get(host)
            if decision is None:
                # Every state change and its line happen under the lock, so a decision's line
                # comes before the line of any request it answers, the first among them.
                log(f"window {host} opened {now - self.started:.1f}s after the proxy started")
                if self.outage and self.outage_end <= now < self.outage_end + self.coalesce_gap:
                    decision = ("grace", self.outage, self.coalesce_gap)
                    log(f"host {host} not faulted: after outage {self.outage}, "
                        f"within {plain_seconds(self.coalesce_gap)}s")
                else:
                    if not self.outage or now >= self.outage_end + self.coalesce_gap:
                        self.outage += 1
                        self.outage_end = now + self.recover_after
                        log(f"outage {self.outage} opened {now - self.started:.1f}s after the proxy started, "
                            f"ending {self.outage_end - self.started:.1f}s after")
                    decision = self.outage
                    log(f"host {host} joined outage {decision}")
                self.decision[host] = decision
            # A host that joined an earlier outage is passed on: that outage ended before the
            # latest one opened, since the next opens only a gap after the last one ended.
            return decision == self.outage and now < self.outage_end


class Handler(socketserver.StreamRequestHandler):
    # No read buffer: this handler reads the request line and the headers off the socket and then
    # hands the rest of the connection to pipe(), which reads the socket directly. A buffered
    # rfile would have swallowed whatever arrived in the same packet as the headers — a POST body,
    # or the TLS ClientHello a client may send right after CONNECT — and pipe() would never see it.
    rbufsize = 0
    timeout = 300
    fault: Fault
    stall_seconds: float

    def handle(self) -> None:
        try:
            request_line = self.rfile.readline(HEADER_LIMIT)
        except OSError:
            return
        if not request_line:
            return
        try:
            method, target, _version = request_line.decode("latin-1").split()
        except ValueError:
            self.answer(400, b"ci-fault-proxy: malformed request line\n")
            return
        headers = self.read_headers()
        if headers is None:
            self.answer(400, b"ci-fault-proxy: malformed headers\n")
            return
        try:
            if method.upper() == "CONNECT":
                self.do_connect(target)
            else:
                self.do_plain(method, target, headers)
        except ValueError as error:
            # A port that is not a number, and anything else a malformed target makes of the
            # parsing below: an answer, not a traceback from a thread nobody is reading.
            self.answer(400, f"ci-fault-proxy: {error}\n".encode())

    def read_headers(self) -> list[bytes] | None:
        headers: list[bytes] = []
        while True:
            line = self.rfile.readline(HEADER_LIMIT)
            if not line:
                return None
            if line in (b"\r\n", b"\n"):
                return headers
            headers.append(line)
            if len(headers) > 200:
                return None

    def answer(self, status: int, body: bytes) -> None:
        reason = {400: "Bad Request", 502: "Bad Gateway", 504: "Gateway Timeout"}.get(status, "Error")
        head = (
            f"HTTP/1.1 {status} {reason}\r\n"
            f"Content-Length: {len(body)}\r\n"
            "Content-Type: text/plain\r\n"
            "Connection: close\r\n\r\n"
        ).encode("latin-1")
        try:
            self.wfile.write(head + body)
            self.wfile.flush()
        except OSError:
            pass

    def stall(self, host: str, what: str) -> None:
        """Hold the connection open and send nothing, until the client gives up or the run ends."""
        log(f"stall {what} {host}")
        deadline = time.monotonic() + self.stall_seconds
        while time.monotonic() < deadline:
            ready, _, _ = select.select([self.connection], [], [], 1.0)
            if ready and not self.connection.recv(READ_SIZE):
                return

    @staticmethod
    def split_authority(authority: str, default_port: int) -> tuple[str, int]:
        if authority.startswith("["):
            host, _, rest = authority[1:].partition("]")
            port = int(rest[1:]) if rest.startswith(":") else default_port
            return host, port
        host, _, port = authority.partition(":")
        return host, int(port) if port else default_port

    def injected(self, host: str, label: str, what: str) -> bool:
        """Whether the fault answered this request itself: a stall that held it, or a 504."""
        if not self.fault.faults(host):
            return False
        if self.fault.mode == "stall":
            self.stall(host, label)
            return True
        log(f"504 {what}")
        self.answer(504, b"ci-fault-proxy: injected fault\n")
        return True

    def connect_upstream(self, host: str, port: int, what: str) -> socket.socket | None:
        """The connection to the origin, or None once a 502 has answered for its refusal."""
        try:
            upstream = socket.create_connection((host, port), timeout=30)
        except OSError as error:
            log(f"502 {what}: {error}")
            self.answer(502, f"ci-fault-proxy: {error}\n".encode())
            return None
        log(f"pass {what}")
        return upstream

    def do_connect(self, authority: str) -> None:
        host, port = self.split_authority(authority, 443)
        what = f"CONNECT {host}:{port}"
        if self.injected(host, "CONNECT", what):
            return
        upstream = self.connect_upstream(host, port, what)
        if upstream is None:
            return
        try:
            self.wfile.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
            self.wfile.flush()
            self.pipe(upstream)
        finally:
            upstream.close()

    def do_plain(self, method: str, target: str, headers: list[bytes]) -> None:
        if not target.lower().startswith("http://"):
            self.answer(400, b"ci-fault-proxy: an absolute URI is required\n")
            return
        rest = target[len("http://") :]
        authority, slash, path = rest.partition("/")
        path = "/" + path if slash else "/"
        host, port = self.split_authority(authority, 80)
        what = f"{method} http://{authority}{path}"
        if self.injected(host, method, what):
            return
        # The hop-by-hop headers go, and the connection is a single exchange: this proxy keeps no
        # pool, so an origin left holding the connection open would only make pipe() wait for its
        # own timeout after the client already has the whole answer.
        kept = [h for h in headers if not h.lower().startswith((b"proxy-", b"connection:", b"keep-alive:"))]
        upstream = self.connect_upstream(host, port, what)
        if upstream is None:
            return
        try:
            head = f"{method} {path} HTTP/1.1\r\n".encode("latin-1") + b"".join(kept) + b"Connection: close\r\n\r\n"
            upstream.sendall(head)
            self.pipe(upstream)
        finally:
            upstream.close()

    def pipe(self, upstream: socket.socket) -> None:
        downstream = self.connection
        sockets = [downstream, upstream]
        while True:
            ready, _, bad = select.select(sockets, [], sockets, 60)
            if bad or not ready:
                return
            for source in ready:
                sink = upstream if source is downstream else downstream
                try:
                    chunk = source.recv(READ_SIZE)
                except OSError:
                    return
                if not chunk:
                    return
                try:
                    sink.sendall(chunk)
                except OSError:
                    return


class Proxy(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def handle_error(self, request, client_address) -> None:
        """One line, where the base class prints a traceback: a bug in this program must read as
        a bug in it, not as the fault it injects."""
        error = sys.exc_info()[1]
        log(f"handler failed: {type(error).__name__}: {error}")


def write_port_file(path: str, port: int) -> None:
    """The port, written whole: a reader polling for the file never reads half of a number."""
    temporary = f"{path}.{os.getpid()}.tmp"
    with open(temporary, "w", encoding="utf-8") as handle:
        handle.write(f"{port}\n")
    os.replace(temporary, path)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mode", choices=("fail", "recover", "stall"), required=True)
    parser.add_argument("--hosts", default="", help="comma-separated hosts to fault; empty faults every host")
    parser.add_argument("--recover-after", type=float, default=60.0,
                        help="seconds of 504s before --mode recover passes requests on, counted from the port file, "
                             "or under --per-host-window from each host's own first request, "
                             "or under --coalesce-outage-gap from the request that opens each outage")
    parser.add_argument("--per-host-window", action="store_true",
                        help="--mode recover only: give each faulted host (the host a request names, lower-cased) "
                             "a window of its own, opened at that host's first request and never again, in place "
                             "of one window for every host from the port file; logs a line as each window opens")
    parser.add_argument("--coalesce-outage-gap", type=float, metavar="SECONDS",
                        help="--mode recover only, with --hosts: one outage for every faulted host first reached "
                             "while it is open, from the first request no outage covers; a host first reached less "
                             "than SECONDS after an outage ended is passed on, and the first one reached later opens "
                             "the next outage; logs a line as each outage opens and as each host is decided")
    parser.add_argument("--probe-host", metavar="NAME",
                        help="--coalesce-outage-gap only: a host answered 504 at every request and kept out of "
                             "every outage, which it opens, joins and logs none of, so a rig's probe leaves outage 1 "
                             "to the site's first request")
    parser.add_argument("--stall-seconds", type=float, default=600.0,
                        help="how long --mode stall holds one connection open before dropping it")
    parser.add_argument("--bind", default="127.0.0.1",
                        help="the address to listen on; a docker build reaches it on the bridge gateway, not on 127.0.0.1")
    parser.add_argument("--port", type=int, default=0, help="0 takes a free port from the kernel")
    parser.add_argument("--port-file", default="", help="where to write the port it listens on")
    args = parser.parse_args(argv)
    if args.per_host_window and args.mode != "recover":
        parser.error(f"--per-host-window gives --mode recover a window per host, and --mode {args.mode} has no window")

    hosts = [h.strip() for h in args.hosts.split(",") if h.strip()]
    if args.coalesce_outage_gap is not None:
        if args.mode != "recover":
            parser.error(f"--coalesce-outage-gap joins the windows of --mode recover into outages, "
                         f"and --mode {args.mode} has no window")
        if args.per_host_window:
            parser.error("--coalesce-outage-gap gives the hosts of one call one outage they share and "
                         "--per-host-window gives each host a window of its own: pass one of them")
        if not args.coalesce_outage_gap > 0:
            parser.error(f"--coalesce-outage-gap is a number of seconds more than 0, not {args.coalesce_outage_gap:g}")
        if not hosts:
            parser.error("--coalesce-outage-gap needs a --hosts list: an empty --hosts faults every host, "
                         "so any request at all would open an outage")
    elif args.probe_host is not None:
        parser.error("--probe-host keeps one host out of the outages of --coalesce-outage-gap, "
                     "and this run does not pass that flag")
    Handler.stall_seconds = args.stall_seconds
    server = Proxy((args.bind, args.port), Handler)
    port = server.server_address[1]
    if args.port_file:
        write_port_file(args.port_file, port)
    # The clock of --mode recover starts here, after the port is announced and before a single
    # request is served: between bind and serve_forever a connection waits in the backlog, so no
    # request can be answered out of the window it belongs to. Under --per-host-window each
    # window opens later, at its host's first request, and under --coalesce-outage-gap each outage
    # at the request that opens it; their lines give those moments on this clock.
    Handler.fault = Fault(
        args.mode, hosts, args.recover_after, args.per_host_window,
        coalesce_gap=args.coalesce_outage_gap, probe_host=args.probe_host,
    )
    if args.coalesce_outage_gap is not None:
        window = f" window=coalesced gap={plain_seconds(args.coalesce_outage_gap)}s"
    else:
        window = " window=per-host" if args.per_host_window else ""
    log(f"mode={args.mode}{window} bind={args.bind} port={port} hosts={','.join(hosts) or 'every host'} pid={os.getpid()}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
