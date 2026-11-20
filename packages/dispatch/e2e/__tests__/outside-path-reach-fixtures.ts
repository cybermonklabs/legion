/** The fixtures of the tests of `e2e/acceptance/outside-path-reach.sh`, the driver of the check
 *  `c-outside-path-reach` (dispatch://INFRA-3862), which a person from outside runs on their own
 *  device to measure which of our listed addresses that device reaches over the path we give them.
 *  Four files use them: `outside-path-reach-input.test.ts` (the inputs the script refuses, the
 *  origin's two answers and its classifier, none of which reaches a probe),
 *  `outside-path-reach.test.ts` (runs that probe), `outside-path-reach-blackhole.test.ts` (the
 *  two runs that need a network dropping TEST-NET-1) and `outside-path-reach-tmout.test.ts` (a
 *  list on a pipe under an inherited TMOUT).
 *  `bun test` collects only `*.test.ts` and its kin, so this module runs nowhere of its own.
 *
 *  Every row runs the script itself against real listeners: a fake Dispatch (`Bun.serve`, which
 *  holds a connection that sends nothing, as any HTTP server does) for the origin, and
 *  `Bun.listen` for a target, each on a port the kernel picks and held for the whole run. The fake
 *  Dispatch answers from a process beside this one, as `fakeDispatch` says. A closed port is one
 *  taken from below the kernel's ephemeral range and found refusing: a port a listener held and
 *  gave back can be handed by the kernel to the next listener any concurrent row, or any other
 *  test file, starts, and the closed target would then read REACHED. A target that a row closes is
 *  held on such a port from the moment the row learns it (`closedRangeHoldingTarget`).
 *  The script lets `http` through only with `OUTSIDE_PATH_ALLOW_HTTP=1` and a `127.0.0.1` origin,
 *  which these rows set. HOME is a directory of the run's own, or of the `driveEach` call's own
 *  for the runs that call starts, so no `~/.curlrc` of the host's reaches the script's curl unless
 *  a row writes one.
 *
 *  Where a row needs an answer no real listener gives (an https origin on port 443, a probe exit
 *  curl gives only on a broken network), it puts a `curl` of its own first on PATH that answers
 *  for the hosts it names and hands every other call to the host's curl.
 */

import { test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess, udp } from "bun";
import type { Answers, Seen } from "./outside-path-reach-origin";

export const script = fileURLToPath(
  new URL("../acceptance/outside-path-reach.sh", import.meta.url)
);
export const withoutTools = skipWithout("bash", "curl", "sleep");
export const bash = hostTool("bash") ?? "bash";
export const curl = hostTool("curl") ?? "curl";
const hostSleep = hostTool("sleep") ?? "sleep";
const mkdir = hostTool("mkdir") ?? "mkdir";
const hostEnv = hostTool("env") ?? "env";

/** The timeout of a row that stops before the probes. It only stops a run that hangs: each such row
 *  is decided by the verdicts and exit codes its runs print, and its work is counted (one runner
 *  per row; 86 `Bun.spawn` calls in one run of the input file, 87 where zsh is present, the fake
 *  Dispatch host included). A hang guard is at least twice the longest correct run, read on the
 *  machines that run it. The slowest such row measured, the byte row (64 runners of 12 runs of the
 *  script), took 54.5 s on two CPUs shared with eight busy loops and five other runs of the file
 *  (378 row results, none failing; 53.7 s in an earlier such run), so the guard is twice that,
 *  rounded up to the second. In CI's dispatch-ui job on 2026-11-19 (job 135904767413) the file's 21
 *  rows passed, the slowest in 1.54 s; alone on the dev box at a 1-minute load of 339 to 695 the
 *  whole file took 1.4 to 1.5 s. */
const rowTimeout = 109_000;

/** One row that stops before the probes: `test.skipIf(skip)` with `rowTimeout`. */
export function row(skip: boolean) {
  return (name: string, body: () => Promise<void>) => test.skipIf(skip)(name, body, rowTimeout);
}

/** The timeout of a row that reaches the probes. It only stops a run that hangs: no row reads how
 *  long a run took. A run takes about 29 s (five rounds 6 s apart, the last up to curl's 5 s), and
 *  the script's own timers, at their defaults, allow it 49 s (two reads of the origin of up to 10 s
 *  each, four sleeps of 6 s and a last probe of 5 s). A hang guard is at least twice the longest
 *  correct run, read on the machines that run it. In CI's dispatch-ui job on 2026-11-17, the one
 *  run of these rows took 29.7 s for the holding-targets row and at most 33.1 s for any such row (a
 *  TMOUT=3 row), so twice that is 66.2 s, past the package's 60 s; 14 runs of the rows they
 *  replaced took at most 31.5 s. The guard is 435 s, three times the slowest such row of an earlier
 *  version, 144.9 s, in 12 runs on two CPUs shared with eight busy loops and five other runs of the
 *  four files (these rows' slowest in 12 such runs: 91.0 s), so a loaded dev box does not stop a
 *  correct run. */
const probingRowTimeout = 435_000;

/** One row that reaches the probes, as `row` with `probingRowTimeout`. */
export function probingRow(skip: boolean) {
  return (name: string, body: () => Promise<void>) =>
    test.skipIf(skip)(name, body, probingRowTimeout);
}

/** The bound every run here gives each of the script's two reads of its origin, to connect and in
 *  all (`OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS` and `OUTSIDE_PATH_ORIGIN_READ_SECONDS`): twice the
 *  longest row's timeout. The fake Dispatch answers from a process of its own, which a loaded host
 *  can keep off the CPU past the script's own 3 s and 10 s; under this bound such a read is late,
 *  and only the row's own timeout ends a run whose fake never answers. A row that reads the
 *  script's own defaults sets both empty. */
const originSeconds = String((2 * probingRowTimeout) / 1000);

/** A bash loop that waits until `file` exists, with no limit but this test process's life: once
 *  this process has gone it ends the waiting shell with status 70, so no waiter outlives this test
 *  process. */
export function untilExists(file: string): string {
  return `until [ -e ${file} ]; do kill -0 ${process.pid} 2>/dev/null || exit 70; ${hostSleep} 0.05; done`;
}

/** The locales the rows that read a list's bytes run under: the script reads bytes in all of them. */
export const locales = ["C", "C.UTF-8", "en_US.UTF-8"];

/** An address of TEST-NET-1, which RFC 5737 reserves for documentation. */
export const blackhole = { host: "192.0.2.1", port: 4222 };

export interface Run {
  code: number;
  stdout: string;
  stderr: string;
}

/** The line the script prints for one target, or for the origin's positive control. */
export function lineFor(run: Run, name: string): string {
  const prefix = name === "origin" ? "control origin " : `target ${name} `;
  const lines = run.stdout.split("\n").filter((each) => each.startsWith(prefix));
  if (lines.length !== 1) {
    throw new Error(`${lines.length} lines for ${name} in:\n${run.stdout}\nstderr:\n${run.stderr}`);
  }
  return lines[0] ?? "";
}

/** The script's last line on stdout, its verdict. */
export function verdict(run: Run): string {
  return run.stdout.trimEnd().split("\n").at(-1) ?? "";
}

/** The verdict the script gives for the list at `where` whose byte at `offset`, counted from 0, is
 *  outside printable ASCII, tab and LF: the byte, its offset and its line read from the file. */
export function byteRefusal(where: string, offset: number): string {
  const bytes = readFileSync(where);
  const line = bytes.subarray(0, offset).filter((byte) => byte === 0x0a).length + 1;
  const hex = (bytes[offset] ?? 0).toString(16).toUpperCase().padStart(2, "0");
  return (
    `UNKNOWN: line ${line} of ${where} holds byte 0x${hex} at offset ${offset}, which is not ` +
    "printable ASCII, a tab or LF: write the list in ASCII, one target per line, with LF or CRLF line endings"
  );
}

/** The fake Dispatch host's own program, which `fakeDispatch` runs. */
const hostProgram = fileURLToPath(new URL("./outside-path-reach-origin.ts", import.meta.url));

/** A fake Dispatch host: its control server's URL, and its process. */
interface Host {
  url: string;
  child: Subprocess;
}

/** This module's fake Dispatch host, which the first `fakeDispatch` starts, so one process serves
 *  every fake Dispatch of a test file rather than a Bun runtime of its own for each. It is kept
 *  here so the pipe to its stdin, whose closing ends it, is never collected. */
let host: Promise<Host> | undefined;

async function startHost(): Promise<Host> {
  const child = Bun.spawn([process.execPath, hostProgram], {
    stdin: "pipe",
    stdout: "pipe",
    stderr: "inherit",
  });
  // Its first line on stdout is its control server's port, and it prints nothing after it.
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let printed = "";
  while (!printed.includes("\n")) {
    const { done, value } = await reader.read();
    if (done) break;
    printed += decoder.decode(value, { stream: true });
  }
  reader.releaseLock();
  const port = Number(printed.split("\n")[0]);
  if (!(port > 0)) {
    child.stdin.end();
    const code = await child.exited;
    throw new Error(
      `the fake Dispatch host exited ${code} with no port printed: ${JSON.stringify(printed)}`
    );
  }
  return { url: `http://127.0.0.1:${port}`, child };
}

/** One request to the fake Dispatch host's control server, at `path`. */
async function control(path: string, init?: RequestInit): Promise<Response> {
  host ??= startHost();
  const { url } = await host;
  const call = `${init?.method ?? "GET"} ${path}`;
  let response: Response;
  try {
    response = await fetch(`${url}${path}`, init);
  } catch (error) {
    // A host that has died or refuses: Bun's own error names neither the host nor the call, and
    // the test runner prints it beneath this one as the cause.
    throw new Error(`the fake Dispatch host did not answer ${call}`, { cause: error });
  }
  if (!response.ok) {
    throw new Error(
      `the fake Dispatch host answered ${call} with ${response.status}: ${await response.text()}`
    );
  }
  return response;
}

/** A fake Dispatch, served by the fake Dispatch host, whose head in `outside-path-reach-origin.ts`
 *  says what it answers for `health`, `projects` and `refusal`. `seen()` reads the list of every
 *  request it answered, in the order it answered them, and `stop()` stops it and closes its
 *  connections.
 *
 *  It answers from a process of its own, whose event loop does nothing but answer, because a
 *  server in the test process answers only while the test process's event loop runs. Under the
 *  script's default of 10 s for each read of the origin (`curl --max-time 10`), with the fake
 *  Dispatch in the test process, the row "fails naming an origin that serves a read with no
 *  credential" read "did not answer as Dispatch" (UNKNOWN) in place of FAIL 2 times in 320 runs of
 *  its file under the e2e lane's load, and once in 47 ordinary runs of the lane. In four runs that
 *  each ran the harness lane 20 times (`bun run test:repeat --lane harness --reruns 20`), a 50 ms
 *  interval timer in that file measured the test process's longest stall at 5,850, 12,681, 6,080
 *  and 1,557 ms, and its longest stall during that row's run of the script at 1,517, 12,631, 6,030
 *  and 757 ms: under that default, a stall past 10 s leaves the script's read of
 *  `/api/v1/projects` unanswered. The runs here give each read `originSeconds` instead. */
export async function fakeDispatch(
  health = 200,
  projects = 401,
  refusal: object = { code: "NO_IDENTITY" }
) {
  const answers: Answers = { health, projects, refusal };
  const started = await control("/", { method: "POST", body: JSON.stringify(answers) });
  const { port } = (await started.json()) as { port: number };
  return {
    port,
    async seen(): Promise<Seen[]> {
      return (await (await control(`/${port}/seen`)).json()) as Seen[];
    },
    async stop(): Promise<void> {
      await control(`/${port}`, { method: "DELETE" });
    },
  };
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
/** The ports taken for closed ones: from 10000 to 2000 short of the ephemeral range, each handed
 *  out once in this process, starting at a random one so two processes seldom try the same ones. */
const closedRange = { low: 10000, span: ephemeralLow() - 2000 - 10000 };
if (!(closedRange.span > 2000)) {
  throw new Error(`the kernel's ephemeral ports start at ${ephemeralLow()}: no room below them`);
}
const closedStart = Math.floor(Math.random() * closedRange.span);
let closedIssued = 0;
/** The UDP sockets that reserve the ports `closedPort` and `closedRangeHoldingTarget` issued,
 *  kept here so none is collected. */
const closedReservations: udp.Socket<"buffer">[] = [];

/** A UDP socket bound to `port` on 127.0.0.1, or undefined where another socket holds it. It is
 *  what keeps a port of the closed range to this process: the kernel frees it however the process
 *  ends, and every other process's `closedPort` and `closedRangeHoldingTarget` bind the same
 *  number first, so none of them issues it meanwhile. */
async function reserve(port: number): Promise<udp.Socket<"buffer"> | undefined> {
  try {
    // A failed bind throws here at once rather than rejecting the promise.
    return await Bun.udpSocket({ hostname: "127.0.0.1", port });
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "EADDRINUSE") return undefined;
    throw error;
  }
}

/** Holds `reservation` for the rest of this process's life. Unreferenced, it holds the port
 *  without keeping the process running. */
function keep(reservation: udp.Socket<"buffer">) {
  reservation.unref();
  closedReservations.push(reservation);
}

/** A port that refuses a connection: the next of the range, wrapping round it, past any a host
 *  program holds. The kernel hands none of them to a `port: 0` listener.
 *  It reserves a port before it connects to it (`reserve`). Unreserved, another process could
 *  issue it too and listen on it, as the row that closes a holding target does, and this
 *  process's closed target would read REACHED. It tests by connecting, not by binding a TCP
 *  listener, since on the BSDs a bind on 127.0.0.1 succeeds beside a listener on every address. */
export async function closedPort(): Promise<number> {
  while (closedIssued < closedRange.span) {
    const port = closedRange.low + ((closedStart + closedIssued++) % closedRange.span);
    const reservation = await reserve(port);
    if (reservation === undefined) continue;
    const refused = await Bun.connect({ hostname: "127.0.0.1", port, socket: { data() {} } }).then(
      (socket) => {
        socket.end();
        return false;
      },
      (error: { code?: string }) => error.code === "ECONNREFUSED"
    );
    if (!refused) {
      reservation.close();
      continue;
    }
    keep(reservation);
    return port;
  }
  throw new Error(`no refused port between ${closedRange.low} and the ephemeral range`);
}

/** A target that accepts and closes the connection at once. */
export function closingTarget() {
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

/** A target that accepts, speaks first and holds the connection open, as NATS does. */
export function holdingTarget() {
  return listenHolding(0, false);
}

/** A holding target on a port of the closed range, for a row that closes it to leave a port that no
 *  `port: 0` listener is handed: the next port of the range that it reserves (`reserve`) and that a
 *  listen of its own then takes. The listen is the test of the port, so no other program can take
 *  the port between the test and the bind, as it could between `closedPort`'s refused connect and a
 *  later listen, and the reservation keeps it from every other process's `closedPort` once the row
 *  closes the target. It is exclusive, so that no kernel lets another socket share the port while
 *  it holds it. */
export async function closedRangeHoldingTarget() {
  while (closedIssued < closedRange.span) {
    const port = closedRange.low + ((closedStart + closedIssued++) % closedRange.span);
    const reservation = await reserve(port);
    if (reservation === undefined) continue;
    try {
      const target = listenHolding(port, true);
      keep(reservation);
      return target;
    } catch (error) {
      reservation.close();
      if (!(error instanceof Error && "code" in error && error.code === "EADDRINUSE")) throw error;
    }
  }
  throw new Error(`no free port between ${closedRange.low} and the ephemeral range`);
}

/** A holding target's listener on `port`, exclusive or not. */
function listenHolding(port: number, exclusive: boolean) {
  return Bun.listen({
    hostname: "127.0.0.1",
    port,
    exclusive,
    socket: {
      open(socket) {
        socket.write("INFO {}\r\n");
      },
      data() {},
    },
  });
}

/** The bash wrapper `drive` runs to give the script its target list. Its arguments: how the list
 *  is given (`stdin`, `fifo` or `substitution`), the list's first part, its last part (none when
 *  empty), the seconds to wait before the last part, whether the list is then held open with
 *  nothing more until the script exits (any word for yes, empty for no), the paths of sleep and
 *  mkfifo, the named pipe's path, the path the wrapper creates once the script exits, then the
 *  shell and the script. Before a last part it writes 128 comment lines of 1024 bytes, 128 KiB,
 *  twice the 64 KiB a new pipe holds on 4 KiB-page Linux, under gVisor and on macOS, so those
 *  writes return only once the script has read past the first part; lines of 1 KiB, since the
 *  script splits the list one line at a time and a long line costs it seconds. The writer of a
 *  named pipe is stopped once the script exits, in case the script stopped before it opened the
 *  pipe, and a held list's writer stops once the test process, the wrapper's parent, has gone. */
const listFeeder = [
  "via=$1 first=$2 later=$3 pause=$4 hold=$5 sleep=$6 mkfifo=$7 fifo=$8 ended=$9",
  "shift 9",
  "feed() {",
  '  printf "%s" "$first"',
  '  [ -z "$later" ] || {',
  '    n=0; while [ "$n" -lt 128 ]; do printf "#%*s\\n" 1022 ""; n=$((n + 1)); done',
  '    "$sleep" "$pause"; printf "%s" "$later"',
  "  }",
  '  [ -z "$hold" ] || until [ -e "$ended" ]; do kill -0 "$PPID" 2>/dev/null || exit 70; "$sleep" 0.05; done',
  "}",
  "case $via in",
  '  stdin) feed | { OUTSIDE_PATH_TARGETS=/dev/stdin "$@"; status=$?; : >"$ended"; exit "$status"; } ;;',
  "  fifo)",
  '    "$mkfifo" "$fifo" || exit 70',
  '    feed >"$fifo" &',
  "    writer=$!",
  '    OUTSIDE_PATH_TARGETS=$fifo "$@"',
  "    status=$?",
  '    : >"$ended"',
  '    kill "$writer" 2>/dev/null',
  '    exit "$status"',
  "    ;;",
  '  substitution) OUTSIDE_PATH_TARGETS=<(feed) "$@"; status=$?; : >"$ended"; exit "$status" ;;',
  "esac",
].join("\n");

/** The bash wrapper `driveEach` runs: the script once for each run in its arguments, each with the
 *  variables its run names on top of the wrapper's own environment, `together` runs at a time, and
 *  each run's stdout, stderr and exit code in the files `<n>.out`, `<n>.err` and `<n>.code` of the
 *  directory it is given, `n` counted from 0. Its arguments: the host's `env`, the shell, the
 *  script, the directory and `together`, then for each run the count of its variables and each
 *  variable as `NAME=VALUE`. `env` puts each variable into the environment it starts the shell
 *  with, so no name passes through the wrapper's own bash, which keeps some names read-only, sets
 *  others for itself (`SHLVL`, `PIPESTATUS`, `DIRSTACK`) and uses its own loop's. */
const eachRunner = [
  "env=$1 shell=$2 script=$3 out=$4 together=$5",
  "shift 5",
  "n=0",
  'while [ "$#" -gt 0 ]; do',
  "  count=$1",
  "  shift",
  "  (",
  // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's slice of its arguments, the run's variables.
  '    (exec "$env" -- "${@:1:count}" "$shell" "$script" </dev/null >"$out/$n.out" 2>"$out/$n.err")',
  '    printf "%s" "$?" >"$out/$n.code"',
  "  ) &",
  '  shift "$count"',
  "  n=$((n + 1))",
  "  [ $((n % together)) -ne 0 ] || wait",
  "done",
  "wait",
].join("\n");

/** Reserves the calling test file's scratch directory and returns it with the helpers that write
 *  there. Call it at the test file's module scope: `temporaryDirectory` registers the hooks that
 *  make and remove the directory in the scope it is called from, and this module is evaluated
 *  once per process, so a directory reserved in its own body would belong to the first file. */
export function reachFixtures() {
  // The rows write about 3,600 entries here, the target files and the three result files of each
  // `driveEach` run, and the removal hook removes what is left inside its 60 s. On a disk other
  // programs share one creation took up to 84 ms, so the directory is on tmpfs where the host has
  // one that allows exec, which the `curl` and `sleep` the rows write here need, and that has room
  // for six runs at once (the loaded runs of docs/testing.md). tmpfs charges a page for each file
  // that holds data, so `driveEach` removes its results once read: with them kept to the end, one
  // run of outside-path-reach-input.test.ts held 10.7 MiB of tmpfs at once; with them removed, 6.7
  // to 7.5 MiB, and one run of all four files 7.3 to 7.8 MiB (three readers, 2026-11-20). So 8 MiB
  // a run, 48 MiB for six, which an empty 64 MiB `/dev/shm` (a Legion role's, Docker's default)
  // holds. One with more than 16 MiB in use, such as a stopped run's leftovers, sends the scratch to
  // `tmpdir()`: a slower removal there, where too little room would fail rows with ENOSPC.
  const scratch = temporaryDirectory("outside-path-reach", {
    root: memoryRoot({ need: 6 * 8 * 2 ** 20 }),
  });

  /** The environment every run of the script starts with, whether `drive` or `driveEach` starts
   *  it: HOME `home`, PATH the host's unless `path` names another, TMPDIR this process's, the
   *  flag that lets the script take the harness's http origin, and `originSeconds` for the two
   *  reads of the origin. A change to how a run is started goes here, so the two helpers cannot
   *  drift apart. */
  function runEnvironment(home: string, path: string | undefined): Record<string, string> {
    return {
      HOME: home,
      PATH: path ?? process.env.PATH ?? "",
      TMPDIR: tmpdir(),
      OUTSIDE_PATH_ALLOW_HTTP: "1",
      OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS: originSeconds,
      OUTSIDE_PATH_ORIGIN_READ_SECONDS: originSeconds,
    };
  }

  /** One run of the script with `env`, over a HOME of its own holding `curlrc` as `.curlrc` when
   *  given, PATH the host's unless given, and run by bash unless `shell` names another program.
   *  With `stdin`, the target list is `stdin`. With `stdinLater`, it goes on with a comment that
   *  overfills the pipe, whose write returns only once the script is reading the list, and then,
   *  `stdinPause` seconds (0 unless given) after that write returned, `stdinLater`. With
   *  `stdinHeld`, the list is then held open with nothing more until the script exits. The run sets
   *  OUTSIDE_PATH_TARGETS to where it is given: `/dev/stdin` on a pipe (the default), a named pipe
   *  in the scratch directory, or a process substitution. Bun hands a child's stdin over as a memfd
   *  or a socket, neither of which reads as a pipe does, so a bash wrapper makes each of them; it
   *  runs sleep and mkfifo by the host's own paths, so a PATH of the script's tools alone serves. */
  async function drive(
    env: Record<string, string>,
    options: {
      path?: string;
      curlrc?: string;
      shell?: string;
      stdin?: string;
      stdinLater?: string;
      stdinPause?: string;
      stdinHeld?: boolean;
      via?: "stdin" | "fifo" | "substitution";
    } = {}
  ): Promise<Run> {
    const home = join(scratch, `home-${randomUUID()}`);
    if (options.curlrc !== undefined) {
      mkdirSync(home);
      writeFileSync(join(home, ".curlrc"), options.curlrc);
    }
    const argv =
      options.stdin === undefined
        ? [options.shell ?? bash, script]
        : [
            bash,
            "-c",
            listFeeder,
            "drive",
            options.via ?? "stdin",
            options.stdin,
            options.stdinLater ?? "",
            options.stdinPause ?? "0",
            options.stdinHeld === true ? "held" : "",
            hostSleep,
            hostTool("mkfifo") ?? "mkfifo",
            join(scratch, `fifo-${randomUUID()}`),
            join(scratch, `ended-${randomUUID()}`),
            options.shell ?? bash,
            script,
          ];
    const child = Bun.spawn(argv, {
      env: { ...runEnvironment(home, options.path), ...env },
      stdin: "ignore",
      stderr: "pipe",
      stdout: "pipe",
    });
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    return { code, stdout, stderr };
  }

  /** One run of the script for each of `runs`, each with its own variables on top of
   *  `runEnvironment`, all started by one bash child of the test process, `together` of them at a
   *  time (all of them unless given), and the results in the order of `runs`. Bun.spawn returns
   *  only once its child has started its program, and that wait holds the test process's event
   *  loop, which every row of the file shares: on a loaded host one spawn held it 13 s. A row that
   *  starts one child for its runs holds the loop once, where a child of its own for each run held
   *  it once for each, and waited behind every other row's spawns before each.
   *  A run started here differs from one `drive` starts in four ways, none of which the script
   *  reads or traps: it inherits SHLVL and PWD from the runner's bash, where `drive`'s run
   *  sets its own, so under bash 5.2 its SHLVL is 2, not 1; it is a background job of a
   *  non-interactive shell, which bash 3.2 starts with SIGINT and SIGQUIT ignored, where `drive`'s
   *  run has them as the test process gives them (bash 5.2 restores them at the `exec`); the runs
   *  of one call share one HOME, in which none is given a `.curlrc`; and its stdout and stderr are
   *  files, where `drive`'s are the sockets Bun.spawn's `pipe` gives. The runner hands each run's
   *  variables to the host's `env`, so every name reaches the run as given, as `drive`'s
   *  `Bun.spawn` passes it; a name no environment can hold (empty, or holding `=`) is refused. */
  async function driveEach(
    runs: Record<string, string>[],
    options: { path?: string; together?: number } = {}
  ): Promise<Run[]> {
    for (const name of runs.flatMap(Object.keys)) {
      if (name === "" || name.includes("=")) {
        throw new Error(`driveEach cannot pass ${JSON.stringify(name)}: no environment holds it`);
      }
    }
    const out = join(scratch, `each-${randomUUID()}`);
    mkdirSync(out);
    const child = Bun.spawn(
      [
        bash,
        "-c",
        eachRunner,
        "each",
        hostEnv,
        bash,
        script,
        out,
        String(options.together ?? runs.length),
        ...runs.flatMap((env) => [
          String(Object.keys(env).length),
          ...Object.entries(env).map(([name, value]) => `${name}=${value}`),
        ]),
      ],
      {
        env: runEnvironment(join(scratch, `home-${randomUUID()}`), options.path),
        stdin: "ignore",
        stdout: "ignore",
        stderr: "pipe",
      }
    );
    const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
    if (code !== 0) throw new Error(`the runner of ${runs.length} runs exited ${code}:\n${stderr}`);
    try {
      return runs.map((_, n) => {
        try {
          const exit = readFileSync(join(out, `${n}.code`), "utf8");
          if (!/^\d+$/.test(exit))
            throw new Error(`it left no exit code (${JSON.stringify(exit)})`);
          return {
            code: Number(exit),
            stdout: readFileSync(join(out, `${n}.out`), "utf8"),
            stderr: readFileSync(join(out, `${n}.err`), "utf8"),
          };
        } catch (error) {
          // The runner exits 0 whatever became of a run, and a redirect it could not make is
          // named only on its own stderr.
          throw new Error(
            `run ${n} of ${runs.length}: ${(error as Error).message}; the runner's stderr:\n${stderr}`
          );
        }
      });
    } finally {
      // Each result file holds a page of tmpfs until it goes: see `scratch`.
      rmSync(out, { recursive: true, force: true });
    }
  }

  /** A target file holding `lines`, under the run's scratch directory. */
  function targets(...lines: string[]): string {
    return written(lines.map((line) => `${line}\n`).join(""));
  }

  /** A target file holding `text` (UTF-8 for a string), byte for byte, under the run's scratch
   *  directory. */
  function written(text: string | Uint8Array): string {
    const path = join(scratch, `targets-${randomUUID()}.txt`);
    writeFileSync(path, text);
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

  /** A PATH directory holding bash, sleep and a `curl` that runs `answers` (bash, which may answer
   *  and exit) and hands every call it does not answer to the host's curl. */
  function pathWithCurl(answers: string): string {
    const path = pathOf("bash", "sleep");
    writeFileSync(join(path, "curl"), `#!${bash}\n${answers}\nexec ${curl} "$@"\n`, {
      mode: 0o755,
    });
    return path;
  }

  /** A PATH directory, and the log it writes, for a run whose rounds a row tells apart. `paced`
   *  names targets, each by the `host:port` its probe's URL ends with. Its `curl` takes each probe
   *  of a paced target as that target's next round: it sets `$target` to the target's name and
   *  `$round` to the round, counted from 1, claims the directory `$rounds/<target>-<round>`, and
   *  logs `probe <target> <round>`. Then it runs `answers` (bash, which may answer and exit) and
   *  hands the call to the host's curl, as `pathWithCurl`'s does. Its `sleep` waits until each
   *  paced target's probe of the round before it is logged, then runs the host's sleep and logs
   *  `slept` and its arguments. The script starts a round only once the sleep before it has
   *  returned, so a paced target's n-th probe is its probe of round n, and the log reads in the
   *  order the run went, however late this host runs any of the processes. */
  function pathRounds(
    paced: Record<string, string>,
    answers: string
  ): { path: string; log: string } {
    const rounds = join(scratch, `rounds-${randomUUID()}`);
    mkdirSync(rounds);
    const log = join(rounds, "log");
    // The next round of `name` is the first whose directory this call makes; a mkdir that fails
    // with no such directory there (the scratch directory gone) ends the call.
    const claim = (name: string) =>
      `round=1; until ${mkdir} "$rounds/${name}-$round" 2>/dev/null; do ` +
      `[ -d "$rounds/${name}-$round" ] || exit 70; round=$((round + 1)); done`;
    const path = pathWithCurl(
      [
        `rounds=${rounds} log=${log} target= round=`,
        'case "$*" in',
        ...Object.entries(paced).map(([name, at]) => `  *telnet://${at}) target=${name} ;;`),
        "esac",
        'if [ -n "$target" ]; then',
        `  ${claim("$target")}`,
        '  echo "probe $target $round" >>"$log"',
        '  : >"$rounds/$target-$round/logged"',
        "fi",
        answers,
      ].join("\n")
    );
    const sleep = join(path, "sleep");
    unlinkSync(sleep); // the host's sleep by a symlink, which a write would follow
    writeFileSync(
      sleep,
      [
        `#!${bash}`,
        `rounds=${rounds} log=${log}`,
        claim("sleep"),
        `for target in ${Object.keys(paced).join(" ")}; do`,
        `  ${untilExists('"$rounds/$target-$round/logged"')}`,
        "done",
        `${hostSleep} "$@"`,
        "status=$?",
        'echo "slept $*" >>"$log"',
        'exit "$status"',
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    return { path, log };
  }

  /** A PATH whose curl refuses every probe of a bracketed (IPv6) target. */
  const v6Only = () =>
    pathWithCurl(`case "$*" in *'telnet://['*) printf '0.000010 0.000000'; exit 7 ;; esac`);

  return {
    scratch,
    drive,
    driveEach,
    targets,
    written,
    list,
    pathOf,
    pathWithCurl,
    pathRounds,
    v6Only,
  };
}
