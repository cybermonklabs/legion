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
 *  reaches the script's curl unless a row writes one.
 *
 *  Where a row needs an answer no real listener gives (an https origin on port 443, a probe exit
 *  curl gives only on a broken network), it puts a `curl` of its own first on PATH that answers
 *  for the hosts it names and hands every other call to the host's curl.
 *
 *  A probe that connects to a holding listener ends only at curl's `--max-time` of five seconds, so
 *  every row that reaches the origin takes about five seconds; the rows run concurrently.
 *
 *  The blackholed rows need a network that drops a packet to 192.0.2.1 (TEST-NET-1, RFC 5737)
 *  rather than rejecting it: the module asks once, with the script's own probe, and where the
 *  network refuses it the rows are skipped with that reason printed, never passed.
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

/** The script's own probe, run once here against the blackholed address: exit 28 with the lookup
 *  done and no connect is a drop, and the rows run; exit 7 is a network that refuses the address
 *  at once, and they are skipped; anything else is a host that cannot run them, which fails. */
async function blackholeDrops(): Promise<string | null> {
  if (withoutTools) return "bash or curl is missing";
  const child = Bun.spawn(
    [
      curl,
      "-q",
      "--noproxy",
      "*",
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
    { env: { LC_ALL: "C" }, stdin: "ignore", stderr: "pipe", stdout: "pipe" }
  );
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  const [lookup = "", connect = ""] = stdout.trim().split(" ");
  if (code === 28 && /[1-9]/.test(lookup) && /^0\.0+$/.test(connect)) return null;
  if (code === 7) {
    return (
      `${blackhole.host}:${blackhole.port} was refused at once here (curl exit 7, "${stdout.trim()}"): ` +
      "this network rejects that address rather than dropping it, so the blackholed rows cannot run"
    );
  }
  throw new Error(
    `the blackhole probe of ${blackhole.host}:${blackhole.port} gave curl exit ${code}, ` +
      `"${stdout.trim()}": neither a drop nor a refusal, so this host cannot run these rows`
  );
}
const blackholeSkip = await blackholeDrops();
if (blackholeSkip !== null) console.warn(`Skipped the blackholed rows: ${blackholeSkip}.`);

interface Run {
  code: number;
  stdout: string;
  stderr: string;
  seconds: number;
}

/** One run of the script with `env`, over a HOME of its own holding `curlrc` as `.curlrc` when
 *  given, and PATH the host's unless given. */
async function drive(
  env: Record<string, string>,
  options: { path?: string; curlrc?: string } = {}
): Promise<Run> {
  const home = join(scratch, `home-${randomUUID()}`);
  if (options.curlrc !== undefined) {
    mkdirSync(home);
    writeFileSync(join(home, ".curlrc"), options.curlrc);
  }
  const started = performance.now();
  const child = Bun.spawn([bash, script], {
    env: {
      HOME: home,
      PATH: options.path ?? process.env.PATH ?? "",
      TMPDIR: tmpdir(),
      OUTSIDE_PATH_ALLOW_HTTP: "1",
      ...env,
    },
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
  const lines = run.stdout.split("\n").filter((each) => each.startsWith(prefix));
  if (lines.length !== 1) {
    throw new Error(`${lines.length} lines for ${name} in:\n${run.stdout}\nstderr:\n${run.stderr}`);
  }
  return lines[0] ?? "";
}

/** The script's last line on stdout, its verdict. */
function verdict(run: Run): string {
  return run.stdout.trimEnd().split("\n").at(-1) ?? "";
}

/** One request the fake Dispatch answered. */
interface Seen {
  method: string;
  path: string;
  credential: string | null;
}

/** A fake Dispatch: `/healthz` answers `health`, and `/api/v1/projects` answers 200 to a request
 *  carrying a credential (an `Authorization` or a `Cookie`), as Dispatch would, and `projects` to
 *  one carrying none. `seen` lists every request it answered. */
function fakeDispatch(health = 200, projects = 401) {
  const seen: Seen[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      const credential = request.headers.get("authorization") ?? request.headers.get("cookie");
      seen.push({ method: request.method, path: pathname, credential });
      if (pathname === "/healthz") return Response.json({ ok: health === 200 }, { status: health });
      if (pathname === "/api/v1/projects") {
        const status = credential === null ? projects : 200;
        if (status === 302) return Response.redirect("https://accounts.example/signin", 302);
        return Response.json(status === 200 ? [] : { code: "NO_IDENTITY" }, { status });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
  return Object.assign(server, { seen });
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
/** The ports this file takes for closed ones: from 10000 to 2000 short of the ephemeral range,
 *  each handed out once, starting at a random one so two processes of this file rarely meet. */
const closedRange = { low: 10000, span: ephemeralLow() - 2000 - 10000 };
if (!(closedRange.span > 2000)) {
  throw new Error(`the kernel's ephemeral ports start at ${ephemeralLow()}: no room below them`);
}
const closedStart = Math.floor(Math.random() * closedRange.span);
let closedIssued = 0;

/** A port that refuses a connection: the next of this file's own, wrapping round the range, past
 *  any a host program holds. The kernel hands none of them to a `port: 0` listener. */
async function closedPort(): Promise<number> {
  while (closedIssued < closedRange.span) {
    const port = closedRange.low + ((closedStart + closedIssued++) % closedRange.span);
    const refused = await Bun.connect({ hostname: "127.0.0.1", port, socket: { data() {} } }).then(
      (socket) => {
        socket.end();
        return false;
      },
      (error: { code?: string }) => error.code === "ECONNREFUSED"
    );
    if (refused) return port;
  }
  throw new Error(`no refused port between ${closedRange.low} and the ephemeral range`);
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

/** A target file of two closed targets, written with a comment line and an inline comment as a
 *  person would, then `extra`. */
async function list(...extra: string[]): Promise<string> {
  return targets(
    "# name host port, the address each name stands for beside it",
    `closed-a 127.0.0.1 ${await closedPort()}`,
    `closed-b 127.0.0.1 ${await closedPort()} # 127.0.0.1`,
    ...extra
  );
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

/** A PATH directory holding bash and a `curl` that runs `answers` (bash, which may answer and
 *  exit) and hands every call it does not answer to the host's curl. */
function pathWithCurl(answers: string): string {
  const path = pathOf("bash");
  writeFileSync(join(path, "curl"), `#!${bash}\n${answers}\nexec ${curl} "$@"\n`, {
    mode: 0o755,
  });
  return path;
}

describe.concurrent("outside-path-reach.sh", () => {
  test.skipIf(withoutTools)(
    "passes when the origin answers as Dispatch and every target is unreachable, its control read through time_connect",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(),
        });
        // Bun.serve holds the probe's connection, so curl ends at --max-time with 28: the control
        // reads REACHED from time_connect, not from a clean exit.
        expect(lineFor(run, "origin")).toMatch(
          new RegExp(`^control origin 127\\.0\\.0\\.1:${origin.port} REACHED; curl exit 28, `)
        );
        expect(lineFor(run, "closed-a")).toContain(" unreachable; curl exit 7,");
        expect(lineFor(run, "closed-b")).toContain(" unreachable; curl exit 7,");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
        expect(origin.seen).toEqual([
          { method: "GET", path: "/healthz", credential: null },
          { method: "GET", path: "/api/v1/projects", credential: null },
        ]);
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
          OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${listening.port}`),
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

  // Two holding targets beside the holding origin: probed one after another they would take
  // fifteen seconds, so a run under ten shows they are probed at once.
  test.skipIf(withoutTools)(
    "reads targets that accept and hold the connection as reached, all probed at once within ten seconds",
    async () => {
      const origin = fakeDispatch();
      const first = holdingTarget();
      const second = holdingTarget();
      try {
        const run = await drive({
          OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
          OUTSIDE_PATH_TARGETS: await list(
            `nats-a 127.0.0.1 ${first.port}`,
            `nats-b 127.0.0.1 ${second.port}`
          ),
        });
        expect(lineFor(run, "nats-a")).toContain(" REACHED; curl exit 28,");
        expect(lineFor(run, "nats-b")).toContain(" REACHED; curl exit 28,");
        expect(verdict(run)).toBe("FAIL: reached nats-a nats-b");
        expect(run.code).toBe(1);
        expect(run.seconds).toBeLessThan(10);
      } finally {
        origin.stop(true);
        first.stop(true);
        second.stop(true);
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
          OUTSIDE_PATH_TARGETS: await list(`nats 127.0.0.1 ${port}`),
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
          OUTSIDE_PATH_TARGETS: await list(`blackhole ${blackhole.host} ${blackhole.port}`),
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
          OUTSIDE_PATH_TARGETS: await list(
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
      const origin = fakeDispatch(200, 200);
      try {
        const where = `http://127.0.0.1:${origin.port}`;
        const run = await drive({ OUTSIDE_ORIGIN: where, OUTSIDE_PATH_TARGETS: await list() });
        expect(verdict(run)).toBe(`FAIL: ${where} served a read with no credential`);
        expect(run.code).toBe(1);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "is unknown when the origin's answers are not Dispatch's, a health route that fails or a read that is not refused",
    async () => {
      for (const [health, projects] of [
        [503, 401],
        [200, 403],
        [200, 302],
      ] as const) {
        const origin = fakeDispatch(health, projects);
        try {
          const where = `http://127.0.0.1:${origin.port}`;
          const run = await drive({ OUTSIDE_ORIGIN: where, OUTSIDE_PATH_TARGETS: await list() });
          expect({ health, projects, verdict: verdict(run), code: run.code }).toEqual({
            health,
            projects,
            verdict: `UNKNOWN: ${where} did not answer as Dispatch; reached nothing, so proved nothing`,
            code: 2,
          });
        } finally {
          origin.stop(true);
        }
      }
    }
  );

  test.skipIf(withoutTools)(
    "is unknown when nothing answers at the origin, since it reached nothing",
    async () => {
      const where = `http://127.0.0.1:${await closedPort()}`;
      const run = await drive({ OUTSIDE_ORIGIN: where, OUTSIDE_PATH_TARGETS: await list() });
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
      const path = pathWithCurl(
        `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac`
      );
      try {
        const run = await drive(
          { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
          { path }
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
          OUTSIDE_PATH_TARGETS: await list("ghost does-not-exist.invalid 4222"),
        });
        expect(lineFor(run, "ghost")).toContain(" unresolved");
        expect(verdict(run)).toBe("UNKNOWN: ghost unresolved or unknown");
        expect(run.code).toBe(2);
      } finally {
        origin.stop(true);
      }
    }
  );

  // Answers only a broken network gives, from a curl that names them: an exit no rule classifies,
  // a probe that printed no times, and a name lookup that timed out.
  const odd =
    'case "$*" in\n' +
    "  *telnet://exit35.invalid:*) printf '0.000010 0.000000'; exit 35 ;;\n" +
    "  *telnet://blank.invalid:*) exit 28 ;;\n" +
    "  *telnet://slowdns.invalid:*) printf '0.000000 0.000000'; exit 28 ;;\n" +
    "esac";

  test.skipIf(withoutTools)(
    "is unknown, never a pass, when a probe's answer fits no rule or its lookup timed out",
    async () => {
      const origin = fakeDispatch();
      try {
        const run = await drive(
          {
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: await list(
              "odd exit35.invalid 4222",
              "blank blank.invalid 4222",
              "slowdns slowdns.invalid 4222"
            ),
          },
          { path: pathWithCurl(odd) }
        );
        expect(lineFor(run, "odd")).toBe(
          "target odd exit35.invalid:4222 UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000)"
        );
        expect(lineFor(run, "blank")).toBe(
          "target blank blank.invalid:4222 UNKNOWN (curl exit 28, time_namelookup , time_connect )"
        );
        expect(lineFor(run, "slowdns")).toBe(
          "target slowdns slowdns.invalid:4222 unresolved (lookup timed out); curl exit 28, time_namelookup 0.000000, time_connect 0.000000"
        );
        expect(verdict(run)).toBe("UNKNOWN: odd blank slowdns unresolved or unknown");
        expect(run.code).toBe(2);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "fails naming every reached target, whatever else the run could not see",
    async () => {
      const origin = fakeDispatch();
      const hub = closingTarget();
      const nats = holdingTarget();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const [beside, blind] = await Promise.all([
          drive(
            {
              ...env,
              OUTSIDE_PATH_TARGETS: await list(
                "odd exit35.invalid 4222",
                `hub 127.0.0.1 ${hub.port}`,
                "slowdns slowdns.invalid 4222",
                `nats 127.0.0.1 ${nats.port}`
              ),
            },
            { path: pathWithCurl(odd) }
          ),
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${hub.port}`) },
            {
              path: pathWithCurl(
                `case "$*" in *telnet://127.0.0.1:${origin.port}*) printf '0.000010 0.000000'; exit 7 ;; esac`
              ),
            }
          ),
        ]);
        expect(verdict(beside)).toBe("FAIL: reached hub nats");
        expect(beside.code).toBe(1);
        expect(lineFor(blind, "origin")).toContain(" unreachable; ");
        expect(verdict(blind)).toBe("FAIL: reached hub");
        expect(blind.code).toBe(1);
      } finally {
        origin.stop(true);
        hub.stop(true);
        nats.stop(true);
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
          drive({ ...env, OUTSIDE_PATH_TARGETS: await list() }, { path }),
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(`hub 127.0.0.1 ${listening.port}`) },
            { path }
          ),
        ]);
        expect(passing.stderr).toBe("");
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

  test.skipIf(withoutTools)(
    "reads what the device itself reaches, whatever its curl configuration or proxy says",
    async () => {
      const origin = fakeDispatch();
      const holding = holdingTarget();
      try {
        const env = { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}` };
        const nats = `nats localhost ${holding.port}`;
        const [credentials, resolve, proxy] = await Promise.all([
          // A ~/.curlrc that sends a credential with every request would make the read the
          // script sends with none come back 200.
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list() },
            {
              curlrc:
                'header = "Authorization: Bearer from-curlrc"\nheader = "Cookie: dsession=from-curlrc"\n',
            }
          ),
          // A ~/.curlrc that resolves one target elsewhere would read it unreachable.
          drive(
            { ...env, OUTSIDE_PATH_TARGETS: await list(nats) },
            { curlrc: `resolve = localhost:${holding.port}:${blackhole.host}\n` }
          ),
          // A proxy the origin is exempt from would carry the probe of every other target, and a
          // proxy that refuses would read each of them unreachable.
          drive({
            ...env,
            OUTSIDE_PATH_TARGETS: await list(nats),
            ALL_PROXY: `socks5://127.0.0.1:${await closedPort()}`,
            NO_PROXY: "127.0.0.1",
          }),
        ]);
        expect(verdict(credentials)).toStartWith("PASS");
        expect(credentials.code).toBe(0);
        expect(origin.seen.filter((request) => request.credential !== null)).toEqual([]);
        expect(lineFor(resolve, "nats")).toContain(" REACHED; ");
        expect(verdict(resolve)).toBe("FAIL: reached nats");
        expect(lineFor(proxy, "nats")).toContain(" REACHED; ");
        expect(verdict(proxy)).toBe("FAIL: reached nats");
      } finally {
        origin.stop(true);
        holding.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "takes an https origin on its default port or an IPv6 origin, and an IPv6 target, in brackets",
    async () => {
      // A curl that answers as Dispatch at each of these origins, reaches each one's own port,
      // and is refused by the IPv6 target; every other call goes to the host's curl.
      const path = pathWithCurl(
        'case "$*" in\n' +
          "  */healthz*) printf 200; exit 0 ;;\n" +
          "  */api/v1/projects*) printf 401; exit 0 ;;\n" +
          "  *'telnet://dispatch-outside.example:443'* | *'telnet://[fd00::1]:443'* | *'telnet://[fd00::1]:8443'*)\n" +
          "    printf '0.000010 0.000100'; exit 28 ;;\n" +
          "  *'telnet://[::1]:4222'*) printf '0.000010 0.000000'; exit 7 ;;\n" +
          "esac"
      );
      for (const [origin, control] of [
        ["https://dispatch-outside.example", "dispatch-outside.example:443"],
        ["https://[fd00::1]", "[fd00::1]:443"],
        ["https://[fd00::1]:8443/", "[fd00::1]:8443"],
      ] as const) {
        const run = await drive(
          { OUTSIDE_ORIGIN: origin, OUTSIDE_PATH_TARGETS: await list("v6 ::1 4222") },
          { path }
        );
        expect(lineFor(run, "origin")).toStartWith(`control origin ${control} REACHED; `);
        expect(lineFor(run, "v6")).toStartWith("target v6 [::1]:4222 unreachable; ");
        expect(verdict(run)).toStartWith("PASS");
        expect(run.code).toBe(0);
      }
    }
  );

  test.skipIf(withoutTools)("is unknown when curl is missing", async () => {
    const origin = fakeDispatch();
    try {
      const run = await drive(
        { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
        { path: pathOf("bash") }
      );
      expect(verdict(run)).toStartWith("UNKNOWN: curl missing");
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

  test.skipIf(withoutTools)("is unknown when curl is older than 7.40", async () => {
    const origin = fakeDispatch();
    const path = pathWithCurl(
      `case "$*" in *--version*) echo 'curl 7.39.0 (x86_64-redhat-linux-gnu) libcurl/7.39.0'; exit 0 ;; esac`
    );
    try {
      const run = await drive(
        { OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`, OUTSIDE_PATH_TARGETS: await list() },
        { path }
      );
      expect(verdict(run)).toBe(
        "UNKNOWN: curl missing or too old: this curl is '7.39.0'; install curl 7.40 or later"
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
      expect(verdict(run)).toEndWith("lists no target: nothing was tried, so nothing is proved");
      expect(verdict(run)).toStartWith("UNKNOWN: ");
      expect(run.code).toBe(2);
    } finally {
      origin.stop(true);
    }
  });

  test.skipIf(withoutTools)("refuses each missing input by name", async () => {
    const noOrigin = await drive({ OUTSIDE_PATH_TARGETS: await list() });
    expect(verdict(noOrigin)).toStartWith("UNKNOWN: OUTSIDE_ORIGIN is unset");
    expect(noOrigin.code).toBe(2);
    const noTargets = await drive({ OUTSIDE_ORIGIN: "https://dispatch-outside.example" });
    expect(verdict(noTargets)).toStartWith("UNKNOWN: OUTSIDE_PATH_TARGETS is unset");
    expect(noTargets.code).toBe(2);
  });

  test.skipIf(withoutTools)(
    "refuses a target list it cannot read whole or a line it cannot take, before going on the network",
    async () => {
      const origin = fakeDispatch();
      try {
        for (const [where, reason] of [
          [await list("hub 127.0.0.1"), "is not 'name host port': hub 127.0.0.1"],
          [
            await list("hub svc:envoy-hub 4222"),
            "target hub: svc:envoy-hub is a Tailscale service selector",
          ],
          [await list("hub host_name 4222"), "target hub: host_name is neither a host name"],
          [await list("hub 127.0.0.1 0"), "target hub: 0 is not a TCP port"],
          [await list("hub 127.0.0.1 0443"), "target hub: 0443 is not a TCP port"],
          [await list("hub 127.0.0.1 65536"), "target hub: 65536 is not a TCP port"],
          [await list("hub 127.0.0.1 nats"), "target hub: nats is not a TCP port"],
          [scratch, "which this device could not read whole"],
          [join(scratch, "no-such-list.txt"), "which this device could not read whole"],
        ] as const) {
          const run = await drive({
            OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
            OUTSIDE_PATH_TARGETS: where,
          });
          expect({ reason, verdict: verdict(run).startsWith("UNKNOWN: "), code: run.code }).toEqual(
            { reason, verdict: true, code: 2 }
          );
          expect(verdict(run)).toContain(reason);
        }
        expect(origin.seen).toEqual([]);
      } finally {
        origin.stop(true);
      }
    }
  );

  test.skipIf(withoutTools)(
    "refuses an origin that is not an https origin alone, unless it is the local harness's with the flag set",
    async () => {
      const path = await list();
      for (const [origin, allow] of [
        ["http://127.0.0.1:8766", ""],
        ["http://dispatch-outside.example", "1"],
        ["https://dispatch-outside.example/path", "1"],
        ["https://user@dispatch-outside.example", "1"],
        ["https://dispatch-outside.example:0443", "1"],
        ["https://::1", "1"],
        ["ftp://dispatch-outside.example", "1"],
      ] as const) {
        const run = await drive({
          OUTSIDE_ORIGIN: origin,
          OUTSIDE_PATH_ALLOW_HTTP: allow,
          OUTSIDE_PATH_TARGETS: path,
        });
        expect({
          origin,
          verdict: verdict(run).startsWith("UNKNOWN: OUTSIDE_ORIGIN "),
          code: run.code,
        }).toEqual({ origin, verdict: true, code: 2 });
      }
    }
  );

  test.skipIf(withoutTools)(
    "classifies each recorded triple by whether TCP connected, and by whether the lookup did",
    async () => {
      const path = pathOf("bash");
      for (const [triple, want] of [
        // A resolver that never answered: the lookup timed out, so nothing was tried.
        ["28 0.000000 0.000000", "unresolved (lookup timed out)"],
        // A blackholed address: the lookup completed and the connect timed out.
        ["28 0.000027 0.000000", "unreachable"],
        ["28 0.000027 0.000113", "REACHED"],
        ["7 0.000010 0.000000", "unreachable"],
        // A completed connect is REACHED whatever the exit.
        ["7 0.000010 0.000200", "REACHED"],
        ["6 0.000000 0.000000", "unresolved"],
        [
          "35 0.000010 0.000000",
          "UNKNOWN (curl exit 35, time_namelookup 0.000010, time_connect 0.000000)",
        ],
        ["28  ", "UNKNOWN (curl exit 28, time_namelookup , time_connect )"],
        [
          "28 0.000027 0.0.0",
          "UNKNOWN (curl exit 28, time_namelookup 0.000027, time_connect 0.0.0)",
        ],
      ] as const) {
        const run = await drive({ OUTSIDE_PATH_CLASSIFY: triple }, { path });
        expect({ triple, class: run.stdout.trimEnd(), code: run.code }).toEqual({
          triple,
          class: want,
          code: 0,
        });
      }
    }
  );
});
