---
title: "A server a test starts serves on a port the kernel chose and something holds until it serves"
category: testing
tags:
  - ports
  - address-already-in-use
  - test-harness
  - held-socket
  - flaky-tests
date: 2026-10-27
status: active
module: scripts/e2e
related_issues:
  - "INFRA-253"
  - "sjawhar/legion#2229"
  - "INFRA-3904"
---

# A server a test starts serves on a port the kernel chose and something holds until it serves

Extends docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md.

## Rule

- Never find a free port, close it, and start a server on that number. Between the close and the
  server's bind any program can take the port, and the server exits on `address already in use`.
- Pick the mechanism by who needs the port first:
  - Nothing needs it before the process starts: the process binds port 0 and the harness reads
    the port from the process's own listening line. `nats-server -p -1` (`-1` is nats-server's
    `RANDOM_PORT`; `-p 0` means its default 4222), envoy-listener `PORT=0`,
    `Bun.serve({ port: 0 })` then `server.port`.
  - The process's own settings must name its port (Dispatch's `DISPATCH_SERVER_URL` is its CSRF
    origin): the harness opens the socket on port 0 and hands it over as a descriptor
    (`scripts/e2e/lib/held-port.py exec` in shell, `cmdtest.HoldPort(t).Pass(cmd, ...)` in Go,
    with `{port}` substituted into the settings).
  - The port must survive restarts, or a config file names it before the process starts: a keeper
    holds one socket for the whole run and hands each start a copy (`held-port.py keep` plus
    `exec --from`, `rig.sh`'s `keep_port` and `start_kept`).
  - The process binds the number itself and cannot take a socket, and the number must be known
    before it starts or survive its restarts (nats-server, whose peers' routes name its route
    port; prometheus-nats-exporter, which logs the port it was told, not the one it bound). On
    Linux with IPv6 enabled, and only for a server whose own socket sets `SO_REUSEADDR`: reserve
    the port for the whole run with a socket bound to `[::]:0` with `SO_REUSEADDR` and
    `IPV6_V6ONLY` off that never listens (`testport.Reserve` in
    `packages/envoy/internal/testport/reserve.go`, as `reservePorts` in
    `packages/envoy/infra/gcp/clusteredhub_live_test.go` calls it; testnats's `Gate` holds its
    closed port with the same helper). A Go server binds beside it and binds it
    again on a restart, since Go's listeners set `SO_REUSEADDR`, and meanwhile the kernel gives the
    port to no `bind` to port 0 on any address and no `connect`. It cannot stop a program that
    binds the same number itself with `SO_REUSEADDR`. The kernel keeps a reserved port from a
    `bind` to port 0 only while `net.ipv4.ip_autobind_reuse` is 0, its default, so `reservePorts`
    checks it and refuses to run otherwise. A host with no such setting reads as 0: a kernel
    before Linux 5.7, which added it, never makes the second search, and in gVisor, which has no
    such file either, 4,000 reservations were given to none of 8,000 binds to port 0 and 4,000
    connects. Other kernels' bind rules differ, and a host without IPv6 fails the reservation
    loudly.
- A server's listening log line prints the bound address (`ln.Addr()`), never the configured one,
  or a port-0 server reports port 0.
- A port someone sets explicitly is used as given and fails with `address already in use` when it
  is taken. Never replace it with another.
- A held or kept socket accepts and queues connections before any server serves it, and between
  two servers on a kept port. A request to it waits instead of being refused. Give every readiness
  or liveness poll a per-request timeout, treat a timeout as "not up" (or "down"), and wait for the
  server's own listening line rather than taking an answer as readiness.
- These do not close the race, so do not reach for them: a port band below
  `ip_local_port_range` (it keeps only its own callers apart, and a pod's range can start at
  16000), a registry of ports already handed out (one binary only), a retry on
  `address already in use` (it runs the same race again), a "port taken?" probe before the start
  (the port can be taken after it).
- Prove a held port deterministically: a child that waits on stdin before it serves, a competing
  bind that must get `EADDRINUSE` in that window, and as the negative control the holder mutated to
  release early. A parallel stress run is an acceptance check, not the control: the old race is
  intermittent and can pass.
- After each forward merge of main, search the new code for the pattern again (`net.Listen` on
  `:0` then `Close`, `pick_port`, a fixed default): main can bring it back.

## Evidence

- `cmdtest.FreeTCPPort` (deleted in `packages/envoy/internal/cmdtest/cmdtest.go`) picked numbers
  in 20000-32767 and remembered what it handed out; `envoy-infra-go` still failed with
  `127.0.0.1:40077: bind: address already in use` (sjawhar/legion#2158, run 40149911299).
- `HoldPort` and its regression `TestHeldPortStaysHeldUntilItsProcessServes`
  (`packages/envoy/internal/cmdtest/cmdtest_test.go`) are the deterministic proof; with
  `HoldPort` releasing early it fails at `cmdtest_test.go:87`. The parallel acceptance run
  (4 copies x 3 rounds, run 40292180529) passed at the head and at the base alike.
- A plain `http.Get` to a held socket hung `go test` for its whole 10-minute timeout in a negative
  control of `cmd/listener`'s `waitHealthy`; it now bounds each request and waits for the
  `envoy-listener listening` line. `held-port.test.ts` flaked once by reading the listening line
  right after `/healthz` answered; it now awaits the line.
- Main's #2264 added `freeLoopbackPort` (listen, close, reuse the number) in
  `packages/envoy/cmd/dispatch/pprof_test.go` while #2229 was open; the merge replaced it with
  `startPprof(ctx, "127.0.0.1:0")`.
- `loopbackPorts.draw` in `packages/envoy/infra/gcp/clusteredhub_live_test.go` (INFRA-3904) held
  each drawn socket only until the draw returned, and `startBound` retried a start up to 10 times
  on `address already in use`. In a container whose `ip_local_port_range` holds 8 ports, the
  kernel gave a drawn port to the next `bind` to port 0 (on `127.0.0.1`, every address or `::1`)
  and to the next `connect`, and the server's bind of it then failed, at a first start and between
  an exit and a restart alike; a `reservePorts` port went to none of them in the same container and
  the server bound it both times. A program binding a reserved number itself without
  `SO_REUSEADDR` got `EADDRINUSE`; with it, it bound the port and then the server could not.
