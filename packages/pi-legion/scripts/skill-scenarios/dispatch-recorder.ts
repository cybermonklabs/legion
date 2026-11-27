#!/usr/bin/env bun
/**
 * The `dispatch` a case run's Legion pane finds first on its PATH, in front of the real CLI: it
 * stands in the pane's launcher directory (`<state>/bin`, which the pane's PI_SHELL_PREFIX keeps
 * first on the PATH of every command the role runs), and the real CLI is the next `dispatch` on
 * PATH after that directory (the Envoy plugin's bin/dispatch). Each invocation runs the real CLI
 * with its argv, its working directory, its stdin, stdout and stderr as they are, and exits with
 * the CLI's status. The CLI's Dispatch (`DISPATCH_URL`) is a loopback proxy of the invocation's
 * own, on a port the kernel chose, which forwards every request to the run's Dispatch as it came,
 * gives back Dispatch's answer as Dispatch gave it, and keeps each request's method and path. So
 * the requests are the ones this invocation sent, whatever runs beside it. A pane with no
 * DISPATCH_URL runs the CLI with no proxy, and its requests are unknown.
 *
 * After the CLI exits it appends one line to <run>/calls.jsonl:
 *   {at, done, as: "dispatch", argv, exit, requests: [{method, path}] | null, call}
 * `at` and `done` are when the CLI started and ended, `requests` is null where no proxy ran, and
 * `call` is the CLI's own reading of argv (dispatch-command.ts parseCommand): `{command, flags,
 * dryRun}`, the subcommand and each flag it set by the name `dispatch <command> --help` gives it,
 * or null where the CLI reads none (a help, a usage error). A file flag's text is read in the
 * working directory before the CLI runs, as the CLI reads it; stdin (`-`) is the CLI's alone and
 * stands as `-`. outcome.ts reads a `dispatch-read` action from the line.
 *
 *   bun dispatch-recorder.ts <run dir> <the recorder's own directory> [argv...]
 */
import { appendFileSync, readFileSync, realpathSync } from "node:fs";
import { constants } from "node:os";
import path from "node:path";
import { commandName, flagName, parseCommand } from "@legion/envoy-client/dispatch-command";

const [runDir, ownDir, ...argv] = Bun.argv.slice(2);
if (runDir === undefined || ownDir === undefined) {
  console.error("usage: bun dispatch-recorder.ts <run dir> <its own directory> [argv...]");
  process.exit(2);
}

/** Each entry of PATH but the recorder's own directory, so the CLI it runs is the real one. */
function pathAfter(own: string): string {
  const real = (entry: string) => {
    try {
      return realpathSync(entry);
    } catch {
      return entry;
    }
  };
  const mine = real(own);
  return (process.env.PATH ?? "")
    .split(path.delimiter)
    .filter((entry) => entry !== "" && real(entry) !== mine)
    .join(path.delimiter);
}

const cli = Bun.which("dispatch", { PATH: pathAfter(ownDir) });
if (cli === null) {
  console.error("dispatch: no dispatch CLI on PATH after the run's recorder");
  process.exit(127);
}

/** The CLI's own reading of a call: its subcommand, the flags it set and whether it is a dry run. */
interface RecordedCall {
  readonly command: string;
  readonly flags: Record<string, unknown>;
  readonly dryRun: boolean;
}

/** The CLI's own reading of argv, with a file flag's text read in this working directory. */
function readingOf(args: readonly string[]): RecordedCall | null {
  const parsed = parseCommand(args, {
    readText: (file) => (file === "-" ? "-" : readFileSync(path.resolve(file), "utf8")),
  });
  if (parsed.kind !== "call") return null;
  return {
    command: commandName(parsed.tool),
    flags: Object.fromEntries(
      Object.entries(parsed.args).map(([field, value]) => [flagName(field), value])
    ),
    dryRun: parsed.dryRun,
  };
}

/** Headers of one hop that a proxy does not hand on, and `host`, which names the proxy. */
const HOP_HEADERS = [
  "connection",
  "keep-alive",
  "proxy-connection",
  "transfer-encoding",
  "upgrade",
  "te",
  "trailer",
  "host",
];

const upstream = process.env.DISPATCH_URL?.replace(/\/+$/, "");
const requests: { method: string; path: string }[] = [];
const proxy =
  upstream === undefined || !URL.canParse(upstream)
    ? undefined
    : Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        idleTimeout: 0,
        async fetch(request) {
          const url = new URL(request.url);
          requests.push({ method: request.method, path: url.pathname });
          const headers = new Headers(request.headers);
          for (const name of HOP_HEADERS) headers.delete(name);
          let answered: Response;
          try {
            answered = await fetch(`${upstream}${url.pathname}${url.search}`, {
              method: request.method,
              headers,
              body:
                request.method === "GET" || request.method === "HEAD"
                  ? undefined
                  : await request.arrayBuffer(),
              redirect: "manual",
              decompress: false,
            });
          } catch (error) {
            return Response.json(
              { error: `the recorder could not reach the run's Dispatch at ${upstream}: ${error}` },
              { status: 502 }
            );
          }
          const back = new Headers(answered.headers);
          for (const name of [...HOP_HEADERS, "content-length"]) back.delete(name);
          return new Response(answered.body, {
            status: answered.status,
            statusText: answered.statusText,
            headers: back,
          });
        },
      });

let call: RecordedCall | null = null;
try {
  call = readingOf(argv);
} catch {
  // The CLI reads its own arguments again and says what is wrong with them.
}
const at = new Date().toISOString();
const child = Bun.spawn([cli, ...argv], {
  stdin: "inherit",
  stdout: "inherit",
  stderr: "inherit",
  env: {
    ...process.env,
    ...(proxy === undefined ? {} : { DISPATCH_URL: `http://127.0.0.1:${proxy.port}` }),
  },
});
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"] as const)
  process.on(signal, () => child.kill(signal));
await child.exited;
const exit =
  child.exitCode ??
  128 + (child.signalCode === null ? 0 : (constants.signals[child.signalCode] ?? 0));
proxy?.stop(true);
appendFileSync(
  path.join(runDir, "calls.jsonl"),
  `${JSON.stringify({
    at,
    done: new Date().toISOString(),
    as: "dispatch",
    argv,
    exit,
    requests: proxy === undefined ? null : requests,
    call,
  })}\n`
);
process.exit(exit);
