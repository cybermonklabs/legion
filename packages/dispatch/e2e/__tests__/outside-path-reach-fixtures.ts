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
 *  `Bun.listen` for a target, each on a port the kernel picks and held for the whole run. A closed
 *  port is one taken from below the kernel's ephemeral range and found refusing: a port a listener
 *  held and gave back can be handed by the kernel to the next listener any concurrent row, or any
 *  other test file, starts, and the closed target would then read REACHED.
 *  The script lets `http` through only with `OUTSIDE_PATH_ALLOW_HTTP=1` and a `127.0.0.1` origin,
 *  which these rows set. HOME is a directory of the run's own, so no `~/.curlrc` of the host's
 *  reaches the script's curl unless a row writes one.
 *
 *  Where a row needs an answer no real listener gives (an https origin on port 443, a probe exit
 *  curl gives only on a broken network), it puts a `curl` of its own first on PATH that answers
 *  for the hosts it names and hands every other call to the host's curl.
 */

import { test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { udp } from "bun";

export const script = fileURLToPath(
  new URL("../acceptance/outside-path-reach.sh", import.meta.url)
);
export const withoutTools = skipWithout("bash", "curl", "sleep");
export const bash = hostTool("bash") ?? "bash";
export const curl = hostTool("curl") ?? "curl";
const hostSleep = hostTool("sleep") ?? "sleep";
const mkdir = hostTool("mkdir") ?? "mkdir";

/** One row that stops before the probes: `test.skipIf(skip)` with a timeout of the row's own,
 *  60 s, past bun's default of 5 s and whatever timeout the runner sets. */
export function row(skip: boolean) {
  return (name: string, body: () => Promise<void>) => test.skipIf(skip)(name, body, 60_000);
}

/** One row that reaches the probes, as `row` with a timeout of its own. It only stops a run that
 *  hangs: no row reads how long a run took. A run takes about 29 s (five rounds 6 s apart, the last
 *  up to curl's 5 s), and the script's own timers allow it 49 s (two reads of the origin of up to
 *  10 s each, four sleeps of 6 s and a last probe of 5 s). A hang guard is at least twice the
 *  longest correct run, read on the machines that run it. In CI's dispatch-ui job on 2026-11-17,
 *  the one run of these rows took 29.7 s for the holding-targets row and at most 33.1 s for any
 *  such row (a TMOUT=3 row), so twice that is 66.2 s, past the package's 60 s; 14 runs of the rows
 *  they replaced took at most 31.5 s. The guard is 435 s, three times the slowest such row of an
 *  earlier version, 144.9 s, in 12 runs on two CPUs shared with eight busy loops and five other runs
 *  of the four files (these rows' slowest in 12 such runs: 91.0 s), so a loaded dev box does not
 *  stop a correct run. */
export function probingRow(skip: boolean) {
  return (name: string, body: () => Promise<void>) => test.skipIf(skip)(name, body, 435_000);
}

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

/** One request the fake Dispatch answered. */
interface Seen {
  method: string;
  path: string;
  credential: string | null;
}

/** A fake Dispatch: `/healthz` answers `health`, and `/api/v1/projects` answers 200 to a request
 *  carrying a credential (an `Authorization` or a `Cookie`), as Dispatch would, and `projects` to
 *  one carrying none, with Dispatch's own refusal body unless `refusal` names another. `seen` lists
 *  every request it answered. */
export function fakeDispatch(
  health = 200,
  projects = 401,
  refusal: object = { code: "NO_IDENTITY" }
) {
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
        return Response.json(status === 200 ? [] : refusal, { status });
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
/** The ports taken for closed ones: from 10000 to 2000 short of the ephemeral range, each handed
 *  out once in this process, starting at a random one so two processes seldom try the same ones. */
const closedRange = { low: 10000, span: ephemeralLow() - 2000 - 10000 };
if (!(closedRange.span > 2000)) {
  throw new Error(`the kernel's ephemeral ports start at ${ephemeralLow()}: no room below them`);
}
const closedStart = Math.floor(Math.random() * closedRange.span);
let closedIssued = 0;
/** The UDP sockets that reserve the ports `closedPort` issued, kept here so none is collected. */
const closedReservations: udp.Socket<"buffer">[] = [];

/** A port that refuses a connection: the next of the range, wrapping round it, past any a host
 *  program holds. The kernel hands none of them to a `port: 0` listener.
 *  It reserves a port before it connects to it: it binds a UDP socket to the same number on
 *  127.0.0.1 and holds it for the rest of this process's life, and the kernel frees it however the
 *  process ends. So no other process's `closedPort` can issue it meanwhile. Unreserved, another
 *  process could issue it too and listen on it, as the row that closes a holding target does, and
 *  this process's closed target would read REACHED. It tests by connecting, not by binding a TCP
 *  listener, since on the BSDs a bind on 127.0.0.1 succeeds beside a listener on every address. */
export async function closedPort(): Promise<number> {
  while (closedIssued < closedRange.span) {
    const port = closedRange.low + ((closedStart + closedIssued++) % closedRange.span);
    let reservation: udp.Socket<"buffer">;
    try {
      // A failed bind throws here at once rather than rejecting the promise.
      reservation = await Bun.udpSocket({ hostname: "127.0.0.1", port });
    } catch (error) {
      if (error instanceof Error && "code" in error && error.code === "EADDRINUSE") continue;
      throw error;
    }
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
    // Unreferenced, it holds the port without keeping the process running.
    reservation.unref();
    closedReservations.push(reservation);
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

/** A target that accepts, speaks first and holds the connection open, as NATS does, on `port`. */
export function holdingTarget(port = 0) {
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

/** Reserves the calling test file's scratch directory and returns it with the helpers that write
 *  there. Call it at the test file's module scope: `temporaryDirectory` registers the hooks that
 *  make and remove the directory in the scope it is called from, and this module is evaluated
 *  once per process, so a directory reserved in its own body would belong to the first file. */
export function reachFixtures() {
  const scratch = temporaryDirectory("outside-path-reach");

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
      env: {
        ...bunCacheOff,
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
    return { code, stdout, stderr };
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

  return { scratch, drive, targets, written, list, pathOf, pathWithCurl, pathRounds, v6Only };
}
