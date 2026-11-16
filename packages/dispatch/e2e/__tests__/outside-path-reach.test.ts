/** What `e2e/acceptance/outside-path-reach.sh` decides: the driver of the check
 *  `c-outside-path-reach` (dispatch://INFRA-3862), which a person from outside runs on their own
 *  device to prove the path we give them reaches Dispatch and nothing else of ours.
 *
 *  Every row runs the script itself against real listeners: a fake Dispatch (`Bun.serve`, which
 *  holds a connection that sends nothing, as any HTTP server does) for the origin, and
 *  `Bun.listen` for a target, each on a port the kernel picks and held for the whole run. A closed
 *  port is one this file takes from below the kernel's ephemeral range and finds refusing: a port
 *  a listener held and gave back can be handed by the kernel to the next listener any concurrent
 *  row, or any other test file, starts, and the closed target would then read REACHED.
 *  The script lets `http` through only with `OUTSIDE_PATH_ALLOW_HTTP=1` and a `127.0.0.1` origin,
 *  which these rows set. HOME is a directory of the run's own, so no `~/.curlrc` of the host's
 *  reaches the script's curl.
 *
 *  A probe that connects to a holding listener ends only at curl's `--max-time` of five seconds, so
 *  every row that reaches the origin takes about five seconds; the rows run concurrently.
 *
 *  The blackholed row needs a network that drops a packet to 192.0.2.1 (TEST-NET-1, RFC 5737)
 *  rather than rejecting it: the module asks once, with the script's own probe, and where the
 *  answer comes at once the row is skipped with that reason printed, never passed.
 */

import { describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

const script = fileURLToPath(new URL("../acceptance/outside-path-reach.sh", import.meta.url));
const scratch = temporaryDirectory("outside-path-reach");
const withoutTools = skipWithout("bash", "curl");
const bash = hostTool("bash") ?? "bash";
const curl = hostTool("curl") ?? "curl";

/** A blackholed address: TEST-NET-1 is routed nowhere, so a network that drops it times out. */
const blackhole = { host: "192.0.2.1", port: 4222 };

/** The script's own probe, run once here against the blackholed address: exit 28 with no
 *  completed connect is a drop; anything else is a network that answers it at once. */
async function blackholeDrops(): Promise<string | null> {
  if (withoutTools) return "bash or curl is missing";
  const child = Bun.spawn(
    [
      curl,
      "-sS",
      "-o",
      "/dev/null",
      "--connect-timeout",
      "3",
      "--max-time",
      "5",
      "-w",
      "%{time_namelookup} %{time_connect}",
      `telnet://${blackhole.host}:${blackhole.port}`,
    ],
    {
      env: { HOME: join(scratch, `home-${randomUUID()}`), LC_ALL: "C" },
      stdin: "ignore",
      stderr: "pipe",
      stdout: "pipe",
    }
  );
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  if (code === 28 && /^\S+ 0\.0+$/.test(stdout.trim())) return null;
  return (
    `${blackhole.host}:${blackhole.port} was answered at once here (curl exit ${code}, ` +
    `"${stdout.trim()}"): this network rejects that address rather than dropping it, so the ` +
    "blackholed row cannot run"
  );
}
const blackholeSkip = await blackholeDrops();
if (blackholeSkip !== null) console.warn(`Skipped the blackholed row: ${blackholeSkip}.`);

type Run = { code: number; stdout: string; stderr: string; seconds: number };

/** One run of the script with `env` over a fresh HOME, and PATH the host's unless given. */
async function drive(env: Record<string, string>, path = process.env.PATH ?? ""): Promise<Run> {
  const home = join(scratch, `home-${randomUUID()}`);
  const started = performance.now();
  const child = Bun.spawn([bash, script], {
    env: { HOME: home, PATH: path, TMPDIR: tmpdir(), OUTSIDE_PATH_ALLOW_HTTP: "1", ...env },
    stdin: "ignore",
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout, stderr, seconds: (performance.now() - started) / 1000 };
}

/** The line the script prints for one target, or for the origin's positive control. */
function lineFor(run: Run, name: string): string {
  const prefix = name === "origin" ? "control origin " : `target ${name} `;
  const line = run.stdout.split("\n").find((each) => each.startsWith(prefix));
  if (line === undefined) {
    throw new Error(`no line for ${name} in:\n${run.stdout}\nstderr:\n${run.stderr}`);
  }
  return line;
}

/** The script's last line on stdout, its verdict. */
function verdict(run: Run): string {
  return run.stdout.trimEnd().split("\n").at(-1) ?? "";
}

/** A fake Dispatch: `/healthz` answers 200 and `/api/v1/projects` answers `projects`. */
function fakeDispatch(projects = 401) {
  return Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/healthz") return Response.json({ ok: true });
      if (pathname === "/api/v1/projects") {
        return Response.json(projects === 200 ? [] : { code: "NO_IDENTITY" }, { status: projects });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
}

/** The lowest port the kernel hands out for `port: 0` here: Linux's `ip_local_port_range`, else
 *  49152, where macOS's range and IANA's dynamic ports start. */
function ephemeralLow(): number {
  try {
    const [low] = readFileSync("/proc/sys/net/ipv4/ip_local_port_range", "utf8")
      .trim()
      .split(/\s+/);
    return Number(low);
  } catch {
    return 49152;
  }
}
const belowEphemeral = ephemeralLow() - 2000;
if (!(belowEphemeral > 12000)) {
  throw new Error(`the kernel's ephemeral ports start at ${belowEphemeral + 2000}: no room below`);
}
/** The next port this file takes, from a random start between 10000 and the ephemeral range, so
 *  two processes of this file rarely start at the same port, and never one of this one's twice. */
let nextPort = 10000 + Math.floor(Math.random() * (belowEphemeral - 10000));

/** A port that refuses a connection: the next of this file's own, past any a host program holds.
 *  The kernel never hands it to a `port: 0` listener, so it stays closed through the run. */
async function closedPort(): Promise<number> {
  while (nextPort < belowEphemeral) {
    const port = nextPort++;
    const refused = await Bun.connect({ hostname: "127.0.0.1", port, socket: { data() {} } }).then(
      (socket) => {
        socket.end();
        return false;
      },
      () => true
    );
    if (refused) return port;
  }
  throw new Error(`no refused port between 10000 and ${belowEphemeral}`);
}

/** A target that accepts and closes the connection at once. */
function closingTarget() {
  return Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.end();
      },
      data() {},
    },
  });
}

/** A target that accepts, speaks first and holds the connection open, as NATS does, on `port`. */
function holdingTarget(port = 0) {
  return Bun.listen({
    hostname: "127.0.0.1",
    port,
    socket: {
      open(socket) {
        socket.write("INFO {}\r\n");
      },
      data() {},
    },
  });
}

/** A target file holding `lines`, under the run's scratch directory. */
function targets(...lines: string[]): string {
  const path = join(scratch, `targets-${randomUUID()}.txt`);
  writeFileSync(path, lines.map((line) => `${line}\n`).join(""));
  return path;
}

/** Two closed targets, written with a comment line and an inline comment as a person would. */
async function twoClosed(): Promise<string[]> {
  return [
    "# name host port, the address each name stands for beside it",
    `closed-a 127.0.0.1 ${await closedPort()}`,
    `closed-b 127.0.0.1 ${await closedPort()} # 127.0.0.1`,
  ];
}

/** A PATH directory holding only symlinks to the named host programs. */
function pathOf(...programs: string[]): string {
  const directory = join(scratch, `path-${randomUUID()}`);
  mkdirSync(directory);
  for (const program of programs) {
    const found = hostTool(program);
    if (found === null) throw new Error(`${program} is not on this host's PATH`);
    symlinkSync(found, join(directory, program));
  }
  return directory;
}

describe.concurrent("outside-path-reach.sh", () => {
  test.skipIf(withoutTools)(
    "passes when the origin answers as Dispatch and every target is unreachable, its control read through time_connect",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
        });
        expect(lineFor(run, "origin")).toContain("REACHED");
        // Bun.serve holds the probe's connection, so curl ends at --max-time with 28: the control
        // reads REACHED from time_connect, not from a clean exit.
        expect(lineFor(run, "origin")).toContain("curl exit 28,");
        expect(lineFor(run, "closed-a")).toContain(" unreachable; curl exit 7,");
        expect(lineFor(run, "closed-b")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "fails naming a target that listens, which is the check's negative control",
    async () => {
      const origin = fakeDispatch();
      const listening = closingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed()), `hub 127.0.0.1 ${listening.port}`),
        });
        expect(lineFor(run, "hub")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached hub");
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "reads a target that accepts and holds the connection as reached, within ten seconds",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed()), `nats 127.0.0.1 ${holding.port}`),
        });
        expect(lineFor(run, "nats")).toContain(" REACHED; curl exit 28,");
        expect(verdict(run)).toBe("FAIL: reached nats");
        expect(run.code).toBe(1);
        expect(run.seconds).toBeLessThan(10);
      } finally {
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "reads the same holding target as unreachable once it is closed, and passes",
    async () => {
      const origin = fakeDispatch();
      // On a port below the kernel's ephemeral range, so once closed no other listener takes it.
      const holding = holdingTarget(await closedPort());
      const { port } = holding;
      holding.stop(true);
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed()), `nats 127.0.0.1 ${port}`),
        });
        expect(lineFor(run, "nats")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools || blackholeSkip !== null)(
    "reads a blackholed address as unreachable and passes within ten seconds",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(
            ...(await twoClosed()),
            `blackhole ${blackhole.host} ${blackhole.port}`
          ),
        });
        expect(lineFor(run, "blackhole")).toContain(" unreachable; curl exit 28,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
        expect(run.seconds).toBeLessThan(10);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools || blackholeSkip !== null)(
    "fails the blackholed run once the holding target is added beside it",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(
            ...(await twoClosed()),
            `blackhole ${blackhole.host} ${blackhole.port}`,
            `nats 127.0.0.1 ${holding.port}`
          ),
        });
        expect(lineFor(run, "blackhole")).toContain(" unreachable; ");
        expect(lineFor(run, "nats")).toContain(" REACHED; ");
        expect(verdict(run)).toBe("FAIL: reached nats");
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "fails naming an origin that serves a read with no credential",
    async () => {
      const origin = fakeDispatch(200);
      try {
        const where = `http://127.0.0.1:${origin.port}`;
        const run = await drive({
          OUTSIDE_ORIGIN: where,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
        });
        expect(verdict(run)).toBe(`FAIL: ${where} served a read with no credential`);
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "is unknown when nothing answers at the origin, since it reached nothing",
    async () => {
      const where = `http://127.0.0.1:${await closedPort()}`;
      const run = await drive({
        OUTSIDE_ORIGIN: where,
        OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
      });
      expect(verdict(run)).toBe(
        `UNKNOWN: ${where} did not answer as Dispatch; reached nothing, so proved nothing`
      );
      expect(run.code).toBe(2);
    }
  );

  test.skipIf(withoutTools)(
    "is unknown, never a pass, when Dispatch answers but the probe cannot connect to the origin itself",
    async () => {
      const origin = fakeDispatch();
      // A curl that answers HTTP as the host's does but whose probe of the origin's own port is
      // refused, as behind a proxy that carries the HTTP requests while the device's own connects
      // go nowhere: every target then reads unreachable, and only the control stands between
      // that and a PASS.
      const path = pathOf("bash");
      writeFileSync(
        join(path, "curl"),
        `#!${bash}\n` +
          `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac\n` +
          `exec ${curl} "$@"\n`,
        { mode: 0o755 }
      );
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
          },
          path
        );
        expect(lineFor(run, "origin")).toContain(" unreachable; curl exit 7,");
        expect(lineFor(run, "closed-a")).toContain(" unreachable; ");
        expect(verdict(run)).toBe("UNKNOWN: the probe could not connect to the origin itself");
        expect(run.code).toBe(2);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "is unknown, never a pass, when a target does not resolve",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(
            ...(await twoClosed()),
            "ghost does-not-exist.invalid 4222"
          ),
        });
        expect(lineFor(run, "ghost")).toContain(" unresolved");
        expect(verdict(run)).toBe("UNKNOWN: ghost unresolved or unknown");
        expect(run.code).toBe(2);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "needs only bash and curl: with no timeout on PATH it still passes, and still fails",
    async () => {
      const path = pathOf("bash", "curl");
      const origin = fakeDispatch();
      const listening = closingTarget();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [passing, failing] = await Promise.all([
          drive({ ...env, OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())) }, path),
          drive(
            {
              ...env,
              OUTSIDE_PATH_TARGETS: targets(
                ...(await twoClosed()),
                `hub 127.0.0.1 ${listening.port}`
              ),
            },
            path
          ),
        ]);
        expect(passing.stderr).not.toContain("command not found");
        expect(verdict(passing)).toStartWith("PASS");
        expect(passing.code).toBe(0);
        expect(lineFor(failing, "hub")).toContain(" REACHED; ");
        expect(verdict(failing)).toBe("FAIL: reached hub");
        expect(failing.code).toBe(1);
      } finally {
        origin.stop(true);
        listening.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)("is unknown when curl is missing", async () => {
    const origin = fakeDispatch();
    try {
      const run = await drive(
        {
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
        },
        pathOf("bash")
      );
      expect(verdict(run)).toStartWith("UNKNOWN: curl missing");
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

  test.skipIf(withoutTools)("is unknown when curl is older than 7.40", async () => {
    const origin = fakeDispatch();
    const path = pathOf("bash");
    writeFileSync(
      join(path, "curl"),
      `#!${bash}\n` +
        `[ "$1" = --version ] && { echo 'curl 7.29.0 (x86_64-redhat-linux-gnu) libcurl/7.29.0'; exit 0; }\n` +
        `exec ${curl} "$@"\n`,
      { mode: 0o755 }
    );
    try {
      const run = await drive(
        {
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: targets(...(await twoClosed())),
        },
        path
      );
      expect(verdict(run)).toBe(
        "UNKNOWN: curl missing or too old: this curl is '7.29.0'; install curl 7.40 or later"
      );
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

  test.skipIf(withoutTools)("is unknown when the target file lists no target", async () => {
    const origin = fakeDispatch();
    try {
      const run = await drive({
        OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
        OUTSIDE_PATH_TARGETS: targets("# nothing listed yet", ""),
      });
      expect(verdict(run)).toStartWith("UNKNOWN: ");
      expect(verdict(run)).toContain("lists no target");
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

  test.skipIf(withoutTools)("refuses each missing input by name", async () => {
    const list = targets(...(await twoClosed()));
    const noOrigin = await drive({ OUTSIDE_PATH_TARGETS: list });
    expect(noOrigin.stdout + noOrigin.stderr).toContain("OUTSIDE_ORIGIN is unset");
    expect(noOrigin.stdout).not.toContain("PASS");
    expect(noOrigin.code).toBe(2);
    const noTargets = await drive({ OUTSIDE_ORIGIN: "https://dispatch-outside.example" });
    expect(noTargets.stdout + noTargets.stderr).toContain("OUTSIDE_PATH_TARGETS is unset");
    expect(noTargets.code).toBe(2);
  });

  test.skipIf(withoutTools)(
    "refuses a target written as a svc: selector, which resolves nowhere",
    async () => {
      const run = await drive({
        OUTSIDE_ORIGIN: "https://dispatch-outside.example",
        OUTSIDE_PATH_TARGETS: targets(...(await twoClosed()), "hub svc:envoy-hub 4222"),
      });
      expect(run.stdout + run.stderr).toContain("svc:envoy-hub");
      expect(run.stdout).not.toContain("PASS");
      expect(run.code).toBe(2);
    }
  );

  test.skipIf(withoutTools)(
    "refuses an http origin unless it is the local harness's, with the flag set",
    async () => {
      const list = targets(...(await twoClosed()));
      for (const [origin, allow] of [
        ["http://127.0.0.1:8766", ""],
        ["http://dispatch-outside.example", "1"],
        ["https://dispatch-outside.example/path", "1"],
      ] as const) {
        const run = await drive({
          OUTSIDE_ORIGIN: origin,
          OUTSIDE_PATH_ALLOW_HTTP: allow,
          OUTSIDE_PATH_TARGETS: list,
        });
        expect(run.stdout + run.stderr).toContain("OUTSIDE_ORIGIN");
        expect(run.code).toBe(2);
      }
    }
  );

  test.skipIf(withoutTools)(
    "classifies each recorded triple by whether TCP connected, and by whether the lookup did",
    async () => {
      for (const [triple, want] of [
        // A resolver that never answered: the lookup timed out, so nothing was tried.
        ["28 0.000000 0.000000", "unresolved (lookup timed out)"],
        // A blackholed address: the lookup completed and the connect timed out.
        ["28 0.000027 0.000000", "unreachable"],
        ["28 0.000027 0.000113", "REACHED"],
        ["7 0.000010 0.000000", "unreachable"],
        ["6 0.000000 0.000000", "unresolved"],
        [
          "35 0.000010 0.000000",
          "UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000)",
        ],
        ["28  ", "UNKNOWN (curl exit 28, time_namelookup , time_connect )"],
      ] as const) {
        const run = await drive({ OUTSIDE_PATH_CLASSIFY: triple }, pathOf("bash"));
        expect({ triple, class: run.stdout.trimEnd(), code: run.code }).toEqual({
          triple,
          class: want,
          code: 0,
        });
      }
    }
  );
});
