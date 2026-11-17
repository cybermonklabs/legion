/** The fixtures of the tests of `e2e/acceptance/outside-path-reach.sh`, the driver of the check
 *  `c-outside-path-reach` (dispatch://POMJU-3862), which a person from outside runs on their own
 *  device to measure which of our listed addresses that device reaches over the path we give them.
 *  Three files use them: `outside-path-reach-input.test.ts` (the inputs the script refuses, the
 *  origin's two answers and its classifier, none of which reaches a probe),
 *  `outside-path-reach.test.ts` (runs that probe) and `outside-path-reach-blackhole.test.ts` (the
 *  two runs that need a network dropping TEST-NET-1).
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
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

export const script = fileURLToPath(
  new URL("../acceptance/outside-path-reach.sh", import.meta.url)
);
export const withoutTools = skipWithout("bash", "curl", "sleep");
export const bash = hostTool("bash") ?? "bash";
export const curl = hostTool("curl") ?? "curl";

/** One row: `test.skipIf(skip)` with a timeout of the row's own. A run that reaches the probes
 *  takes about 29 s (five rounds 6 s apart, the last up to curl's 5 s), past bun's default of
 *  5 s, so every row carries 60 s whatever timeout the runner sets. */
export function row(skip: boolean) {
  return (name: string, body: () => Promise<void>) => test.skipIf(skip)(name, body, 60_000);
}

/** The locales the rows that read a list's bytes run under: the script reads bytes in all of them. */
export const locales = ["C", "C.UTF-8", "en_US.UTF-8"];

/** An address of TEST-NET-1, which RFC 5737 reserves for documentation. */
export const blackhole = { host: "192.0.2.1", port: 4222 };

export interface Run {
  code: number;
  stdout: string;
  stderr: string;
  seconds: number;
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
 *  out once in this process, starting at a random one so two processes rarely meet. */
const closedRange = { low: 10000, span: ephemeralLow() - 2000 - 10000 };
if (!(closedRange.span > 2000)) {
  throw new Error(`the kernel's ephemeral ports start at ${ephemeralLow()}: no room below them`);
}
const closedStart = Math.floor(Math.random() * closedRange.span);
let closedIssued = 0;

/** A port that refuses a connection: the next of the range, wrapping round it, past any a host
 *  program holds. The kernel hands none of them to a `port: 0` listener. */
export async function closedPort(): Promise<number> {
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
 *  is given (`stdin`, `fifo` or `substitution`), the list's first part, the part written 2 s after
 *  it (none when empty), the paths of sleep and mkfifo, the named pipe's path, then the shell and
 *  the script. The writer of a named pipe is stopped once the script exits, in case the script
 *  stopped before it opened the pipe. */
const listFeeder = [
  "via=$1 first=$2 later=$3 sleep=$4 mkfifo=$5 fifo=$6",
  "shift 6",
  'feed() { printf "%s" "$first"; [ -z "$later" ] || { "$sleep" 2; printf "%s" "$later"; }; }',
  "case $via in",
  '  stdin) feed | OUTSIDE_PATH_TARGETS=/dev/stdin "$@" ;;',
  "  fifo)",
  '    "$mkfifo" "$fifo" || exit 70',
  '    feed >"$fifo" &',
  "    writer=$!",
  '    OUTSIDE_PATH_TARGETS=$fifo "$@"',
  "    status=$?",
  '    kill "$writer" 2>/dev/null',
  '    exit "$status"',
  "    ;;",
  '  substitution) OUTSIDE_PATH_TARGETS=<(feed) "$@" ;;',
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
   *  With `stdin`, the target list is `stdin`, then `stdinLater` 2 s after it, and the run sets
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
      via?: "stdin" | "fifo" | "substitution";
    } = {}
  ): Promise<Run> {
    const home = join(scratch, `home-${randomUUID()}`);
    if (options.curlrc !== undefined) {
      mkdirSync(home);
      writeFileSync(join(home, ".curlrc"), options.curlrc);
    }
    const started = performance.now();
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
            hostTool("sleep") ?? "sleep",
            hostTool("mkfifo") ?? "mkfifo",
            join(scratch, `fifo-${randomUUID()}`),
            options.shell ?? bash,
            script,
          ];
    const child = Bun.spawn(argv, {
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

  /** A PATH whose curl refuses every probe of a bracketed (IPv6) target. */
  const v6Only = () =>
    pathWithCurl(`case "$*" in *'telnet://['*) printf '0.000010 0.000000'; exit 7 ;; esac`);

  return { scratch, drive, targets, written, list, pathOf, pathWithCurl, v6Only };
}
