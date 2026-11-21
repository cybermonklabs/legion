/** The fixtures of the tests of `e2e/acceptance/outside-edge-refusals.sh`, the HTTPS refusal check
 *  of an outside edge. Three files use them, one concern each:
 *  `outside-edge-refusals.test.ts` (the answers the script reads, against a stand-in edge or a
 *  `curl` of the row's own, and the planted-route control), `outside-edge-refusals-input.test.ts`
 *  (the inputs, tools and shells it refuses before any probe) and
 *  `outside-edge-refusals-signal.test.ts` (a HUP, INT or TERM, and the exit's cleanup).
 *  `bun test` collects only `*.test.ts` and its kin, so this module runs nowhere of its own.
 *
 *  Every row runs the script itself, over HTTP against a stand-in edge on this host's loopback
 *  (under `OUTSIDE_EDGE_ALLOW_HTTP=1`, which lets `http://127.0.0.1:<port>` through and nothing
 *  else), or over HTTPS through a `curl` of the row's own first on PATH, which records the command
 *  line the script gives it and answers as an edge would. The stand-in
 *  (`outside-edge-refusals-edge.ts`, served for each row from a process of its own by `fakeHost`)
 *  routes by Host, method and the path as it came on the wire from a table, sets
 *  `X-Outside-Edge-Backend` on the answers of a route that names a marker, and answers 404 with no
 *  marker otherwise, as the Gateway does. `production` below models the routes the edge serves;
 *  the refusal lines are the sixteen of the plan's refusal file, with example hosts of the same
 *  shape.
 */

import { createHash, randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type BoundName, runBounded } from "./bounded-subprocess";
import type { Edge, Route, Seen } from "./outside-edge-refusals-edge";
import { bash, curl, fakeHost, type Run } from "./outside-path-reach-fixtures";

export const script = fileURLToPath(
  new URL("../acceptance/outside-edge-refusals.sh", import.meta.url)
);
export const scriptName = "outside-edge-refusals.sh";

/** The script needs bash, curl, mktemp, and sha256sum or shasum to hash each answer's body. */
export const withoutTools = skipWithout(
  "bash",
  "curl",
  "mktemp",
  ...(hostTool("sha256sum") === null && hostTool("shasum") === null ? ["sha256sum"] : [])
);

const edgeHost = fakeHost<Edge, Seen>(
  fileURLToPath(new URL("./outside-edge-refusals-edge.ts", import.meta.url)),
  "a stand-in edge host"
);

/** The body of the stand-in's 404 for a request no route takes. */
export const notFound = "no route matched\n";
/** The canary's answer: the plan's canary serves this body with status 200 on every route. */
export const canaryBody = "outside-edge-canary";

/** One line of a list: a refusal line, with the answer it expects where its sixth field names one,
 *  or a routed control with its backend. */
export interface Line {
  name: string;
  method: string;
  sni: string;
  host: string;
  path: string;
  backend?: string;
  expect?: string;
}

/** The plan's refusal file, in its own layout, with example hosts of the same shape. */
export const refusalTable = [
  "tailnet-dispatch   GET  dispatch.edge.example dispatch.tailnet.example        /auth/whoami",
  "tailnet-secrets    POST secrets.edge.example  secrets.tailnet.example         /v1/oauth/token",
  "cluster-dispatch   GET  dispatch.edge.example dispatch.app.svc.cluster.local  /api/v1",
  "cluster-broker     POST secrets.edge.example  broker.app.svc.cluster.local    /v1/oauth/token",
  "cluster-listener   GET  envoy.edge.example    listener.app.svc.cluster.local  /v1/registry/x",
  "broker-enroll      POST secrets.edge.example  secrets.edge.example            /v1/enrollments",
  "broker-introspect  POST secrets.edge.example  secrets.edge.example            /v1/oauth/introspect",
  "broker-pending     GET  secrets.edge.example  secrets.edge.example            /v1/pending",
  "broker-approve     GET  secrets.edge.example  secrets.edge.example            /approve/x",
  "broker-token-get   GET  secrets.edge.example  secrets.edge.example            /v1/oauth/token      405:broker",
  "broker-token-tail  POST secrets.edge.example  secrets.edge.example            /v1/oauth/token/x",
  "broker-healthz     GET  secrets.edge.example  secrets.edge.example            /healthz",
  "broker-dispatch    GET  secrets.edge.example  secrets.edge.example            /api/v1/projects",
  "listener-metrics   GET  envoy.edge.example    envoy.edge.example              /metrics",
  "listener-healthz   GET  envoy.edge.example    envoy.edge.example              /healthz",
  "listener-webhook   POST envoy.edge.example    envoy.edge.example              /webhook/github",
];

/** The plan's routed file, one positive control per backend. */
export const routedTable = [
  "dispatch-healthz GET  dispatch.edge.example dispatch.edge.example /healthz                       dispatch",
  "broker-device    POST secrets.edge.example  secrets.edge.example  /v1/oauth/device_authorization broker",
  "listener-v1      GET  envoy.edge.example    envoy.edge.example    /v1/registry/none              listener",
];

/** The lines of `table`, whose sixth field, where one is written, is `sixth`. */
function parsed(table: string[], sixth: "backend" | "expect"): Line[] {
  return table.map((text) => {
    const [name = "", method = "", sni = "", host = "", path = "", last] = text.split(/\s+/);
    return { name, method, sni, host, path, [sixth]: last };
  });
}
export const refusals = parsed(refusalTable, "expect");
export const routed = parsed(routedTable, "backend");

/** The body of the broker's own 405 for a method its two routes do not serve. */
export const methodRefused = '{"error":"method_not_allowed"}';

/** The routes the edge serves, as the plan describes them: Dispatch's host on every path, the
 *  broker's two exact paths, and the listener's `/v1/` prefix. Each marks its backend. No route of
 *  the edge matches on method (the Gateway class supports none), so the broker's two paths take
 *  every method and the broker answers a method other than POST with its own 405, still marked:
 *  the stand-in models that with the POST answer first and a 405 for every other method after it.
 *  Their statuses differ on purpose: a control is read by its marker, whatever status its backend
 *  answers with. */
export const production: Route[] = [
  {
    host: "dispatch.edge.example",
    path: "/",
    prefix: true,
    status: 200,
    marker: "dispatch",
    body: "dispatch healthz\n",
  },
  ...["/v1/oauth/device_authorization", "/v1/oauth/token"].flatMap((path) => [
    {
      host: "secrets.edge.example",
      method: "POST",
      path,
      status: 400,
      marker: "broker",
      body: '{"error":"invalid_request"}',
    },
    { host: "secrets.edge.example", path, status: 405, marker: "broker", body: methodRefused },
  ]),
  {
    host: "envoy.edge.example",
    path: "/v1/",
    prefix: true,
    status: 404,
    marker: "listener",
    body: "listener: no such registry\n",
  },
];

/** The status and body each routed control answers with in `production`, by control name. */
export const controlAnswers: Record<string, { status: number; body: string }> = {
  "dispatch-healthz": { status: 200, body: "dispatch healthz\n" },
  "broker-device": { status: 400, body: '{"error":"invalid_request"}' },
  "listener-v1": { status: 404, body: "listener: no such registry\n" },
};

/** The canary's route for exactly `line`: its Host, an exact match on its method and path. */
export function planted(line: Line, answer: Partial<Route> = {}): Route {
  return {
    host: line.host,
    method: line.method,
    path: line.path,
    status: 200,
    marker: "canary",
    body: canaryBody,
    ...answer,
  };
}

export function sha(text: string): string {
  return createHash("sha256").update(text).digest("hex");
}

/** A stand-in edge that routes by `routes`, in a host of its own, and its
 *  `http://127.0.0.1:<port>` address. A request no route takes is answered 404 with no marker and
 *  `ownNotFound` as its body, `notFound` unless given. */
export async function standIn(routes: Route[], ownNotFound = notFound) {
  const { port, seen, stop } = await edgeHost({ routes, notFound: ownNotFound });
  return { address: `http://127.0.0.1:${port}`, seen, stop };
}

/** The bound of each probe's connect and read in the runs against the stand-in
 *  (`OUTSIDE_EDGE_CONNECT_SECONDS` and `OUTSIDE_EDGE_READ_SECONDS`). The stand-in answers from a
 *  process of its own, which a loaded host can keep off the CPU past the script's own 3 s and 10 s,
 *  as the fake Dispatch of `outside-path-reach-fixtures.ts` was; under this bound such a probe is
 *  late, never a verdict wrong, and the bound of the run still stops a run that hangs. A row that
 *  reads the script's own defaults leaves both unset. */
const probeSeconds = "40";

/** Reserves the calling test file's scratch directory and returns it with the helpers that write
 *  there or run the script, `from` being the calling file's `import.meta.url`, which names it in
 *  each run's cost line. Call it at the test file's module scope, as `reachFixtures` of
 *  `outside-path-reach-fixtures.ts` is: `temporaryDirectory` registers the hooks that make and
 *  remove the directory in the scope it is called from, and this module is evaluated once per
 *  process, so a directory reserved in its own body would belong to the first file. */
export function edgeFixtures(from: string) {
  // On tmpfs where the host has one that allows exec (the `curl` scripts the rows write here run),
  // as the reach fixtures' scratch is: on a disk other programs share, a hook of the first test
  // file, whose only hooks made and removed this directory, timed out after the file's last row,
  // at 10,971 ms past Bun's 5 s default, in a `bun test` of it on a dev box at a load of about 350.
  // Each run of the script writes at most its two lists here, and a few rows a PATH directory or a
  // TMPDIR of their own: under 200 files, a tmpfs page each, so 1 MiB a run, times six runs
  // sharing a host.
  const scratch = temporaryDirectory("outside-edge-refusals", {
    root: memoryRoot({ need: 6 * 2 ** 20 }),
  });

  /** A list file holding `lines`, each ended by `ending`, under the file's scratch directory. */
  function listFile(lines: string[], ending = "\n"): string {
    const path = join(scratch, `list-${randomUUID()}.txt`);
    writeFileSync(path, lines.map((line) => `${line}${ending}`).join(""));
    return path;
  }

  /** One run of the script, a bounded child under `bound` (`edgeRun`, the bound of a run that
   *  probes, unless given), with `env` alone on top of HOME, PATH (the host's unless `path` is
   *  given), TMPDIR, `label` as DISPATCH_E2E_CASE (which tells its cost line from the others and
   *  the script never reads), the flag that lets the stand-in's http address through, and
   *  `probeSeconds`; a name of `env` given no value is left out. `shell` runs the script where it
   *  is given, bash otherwise; `argv`, where it is given, is run in place of the shell and the
   *  script. */
  async function check(
    label: string,
    env: Record<string, string | undefined>,
    options: {
      path?: string;
      shell?: string;
      tmp?: string;
      argv?: string[];
      bound?: BoundName;
    } = {}
  ): Promise<Run> {
    const given: Record<string, string> = {
      HOME: scratch,
      PATH: options.path ?? process.env.PATH ?? "",
      TMPDIR: options.tmp ?? tmpdir(),
      DISPATCH_E2E_CASE: label,
      OUTSIDE_EDGE_ALLOW_HTTP: "1",
      OUTSIDE_EDGE_CONNECT_SECONDS: probeSeconds,
      OUTSIDE_EDGE_READ_SECONDS: probeSeconds,
    };
    for (const [name, value] of Object.entries(env)) {
      if (value === undefined) delete given[name];
      else given[name] = value;
    }
    const { code, stdout, stderr } = await runBounded({
      argv: options.argv ?? [options.shell ?? bash, script],
      bound: options.bound ?? "edgeRun",
      env: given,
      from,
    });
    return { code, stdout, stderr };
  }

  /** The lists of the plan, as files. */
  function planLists(): Record<string, string> {
    return {
      OUTSIDE_EDGE_ROUTED: listFile(routedTable),
      OUTSIDE_EDGE_REFUSALS: listFile(refusalTable),
    };
  }

  /** A directory of the file's scratch, made new. */
  function directoryOf(kind: string): string {
    const directory = join(scratch, `${kind}-${randomUUID()}`);
    mkdirSync(directory);
    return directory;
  }

  return {
    scratch,
    listFile,
    check,
    planLists,
    directoryOf,
    /** `httpsCurl`'s PATH directory, under the file's scratch directory. */
    httpsCurl: (answers: HttpsAnswers) => httpsCurl(directoryOf("path"), answers),
  };
}

/** The lines of a run's stdout that start with `label` and a space. */
export function linesOf(run: Run, label: string): string[] {
  return run.stdout.split("\n").filter((line) => line.startsWith(`${label} `));
}

/** What a failed assertion should show of a run: its whole output. */
export function said(run: Run): string {
  return `exit ${run.code}\nstdout:\n${run.stdout}\nstderr:\n${run.stderr}`;
}

/** The line the script prints for a probe whose answer was read whole. */
export function probeLine(
  label: string,
  name: string,
  status: number,
  marker: string,
  body: string
) {
  return `${label} ${name} ${status} ${marker} body-sha256 ${sha(body)}`;
}

/** The control lines of a run in which each routed control answered as `production` has it. */
export const visibleControls = routed.map((line) => {
  const { status, body } = controlAnswers[line.name] ?? { status: 0, body: "" };
  return probeLine("control", line.name, status, line.backend ?? "", body);
});

/** The Host of a refusal line's unrouted control, which no route can name. */
export const unroutedHost = "unrouted.outside-edge-refusals.invalid";

/** The line an unrouted control prints where the edge answers it with its own 404, `body`. */
export function unroutedLine(line: Line, body = notFound): string {
  return probeLine("unrouted", line.name, 404, "none", body);
}

/** The request a probe of `line` sends, as the stand-in records it: the line's method, its Host,
 *  its path, and an empty body (`Content-Length: 0`) for a POST. */
export function sent(line: Line, host = line.host): Seen {
  return {
    method: line.method,
    host,
    path: line.path,
    contentLength: line.method === "POST" ? "0" : null,
  };
}

/** Every request a run that reads `lines` sends after the routed controls, in order: for a line
 *  of five fields its unrouted control, then the line; for a line of six, the line alone. */
export function sentFor(lines: Line[]): Seen[] {
  return lines.flatMap((line) =>
    line.expect === undefined ? [sent(line, unroutedHost), sent(line)] : [sent(line)]
  );
}

export const passVerdict =
  "PASS: every one of the 16 refusal lines was refused (the edge's own 404 with no marker, or the answer its line names), after each of the 3 routed controls showed its marker";

/** The line a refusal line prints where `production` answers it: the broker's own 405, marked,
 *  for a line whose sixth field names that answer, and the Gateway's 404 with no marker for every
 *  other line. */
export function refusedLine(line: Line): string {
  return line.expect === undefined
    ? probeLine("refused", line.name, 404, "none", notFound)
    : probeLine("refused", line.name, 405, "broker", methodRefused);
}

export function markerNotVisible(name: string): string {
  return `UNKNOWN: the marker of ${name} is not visible, so no refusal can be read`;
}

/** The answers of `httpsCurl`'s `curl`, by `<host> <path>`: the status (000 for no answer), the
 *  marker header as it is written (name and value) or none, the exit code, and a message for
 *  stderr; or, in place of the marker header, `blocks`, the whole text curl's `-D` writes, every
 *  header block of the answer (an informational `1xx` before the final one, as curl writes it),
 *  with `\r\n` written as such. */
export type HttpsAnswers = Record<
  string,
  { status: string; header?: string; blocks?: string; exit?: number; message?: string }
>;

/** A PATH directory, `directory`, whose `curl` stands for an edge reached over HTTPS: it appends
 *  the arguments of each probe to `log` (one per line, then `--end--`), and answers the probe by
 *  its Host header and the path of its URL from `answers`. Every probe no entry names, an unrouted
 *  control's among them, answers 404 with no marker. It writes the headers and the body where `-D`
 *  and `-o` say, and the status on stdout, as curl's `-w '%{http_code}'` does; `--version` goes to
 *  the host's curl, whose version the script reads. */
function httpsCurl(directory: string, answers: HttpsAnswers): { path: string; log: string } {
  const log = join(directory, "probes.log");
  const cases = Object.entries(answers).map(
    ([key, { status, header, blocks, exit, message }]) =>
      `  '${key}') status=${status} header='${header ?? ""}' blocks='${blocks ?? ""}' code=${exit ?? 0} message='${message ?? ""}' ;;`
  );
  writeFileSync(
    join(directory, "curl"),
    [
      `#!${bash}`,
      `case " $* " in *" --version "*) exec ${curl} "$@" ;; esac`,
      `{ printf '%s\\n' "$@"; printf '%s\\n' --end--; } >>${log}`,
      "headers='' body='' host='' url='' previous=''",
      'for word in "$@"; do',
      // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own expansion, the Host's value.
      "  case $previous in -D) headers=$word ;; -o) body=$word ;; -H) host=${word#Host: } ;; esac",
      "  case $word in https://* | http://*) url=$word ;; esac",
      "  previous=$word",
      "done",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own expansion, the URL after its scheme.
      "rest=${url#*://}",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own expansion, the URL's path.
      "path=/${rest#*/}",
      'case "$host $path" in',
      ...cases,
      "  *) status=404 header='' blocks='' code=0 message='' ;;",
      "esac",
      'if [ -n "$blocks" ]; then',
      `  printf '%b' "$blocks" >"$headers"`,
      `  printf 'answer %s\\n' "$status" >"$body"`,
      'elif [ "$status" != 000 ]; then',
      `  { printf 'HTTP/1.1 %s Stand-in\\r\\n' "$status"; [ -z "$header" ] || printf '%s\\r\\n' "$header"; printf '\\r\\n'; } >"$headers"`,
      `  printf 'answer %s\\n' "$status" >"$body"`,
      "fi",
      `[ -z "$message" ] || printf '%s\\n' "$message" >&2`,
      `printf '%s' "$status"`,
      'exit "$code"',
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  return { path: `${directory}:${process.env.PATH ?? ""}`, log };
}

/** The probes `httpsCurl`'s log recorded, each the arguments of one call, with the paths that
 *  follow `-D` and `-o` written as `<headers>` and `<body>`, and those paths. */
export function probesIn(log: string): { argv: string[]; files: string[] }[] {
  const text = existsSync(log) ? readFileSync(log, "utf8") : "";
  const calls = text.split("--end--\n").filter((call) => call !== "");
  return calls.map((call) => {
    const argv = call.slice(0, -1).split("\n");
    const files: string[] = [];
    return {
      argv: argv.map((word, index) => {
        const before = argv[index - 1];
        if (before === "-D" || before === "-o") {
          files.push(word);
          return before === "-D" ? "<headers>" : "<body>";
        }
        return word;
      }),
      files,
    };
  });
}

/** The command line the script gives curl for `line` against the edge address `address`, with
 *  `host` as its Host: the line's own unless given, as an unrouted control's is. */
export function httpsArgv(
  line: Line,
  address: string,
  { connect = "3", read = "10", host = line.host } = {}
): string[] {
  return [
    "-q",
    "--noproxy",
    "*",
    "-g",
    "--path-as-is",
    "-sS",
    "--http1.1",
    "--resolve",
    `${line.sni}:443:${address}`,
    "-H",
    `Host: ${host}`,
    "-X",
    line.method,
    ...(line.method === "POST" ? ["-d", ""] : []),
    "-D",
    "<headers>",
    "-o",
    "<body>",
    "-w",
    "%{http_code}",
    "--connect-timeout",
    connect,
    "--max-time",
    read,
    "--max-filesize",
    "1048576",
    `https://${line.sni}${line.path}`,
  ];
}

/** The `httpsCurl` answers under which every routed control shows its marker, and the broker
 *  answers `broker-token-get`, the line whose sixth field names it, with its own 405, marked. Two
 *  of them write the header name in lower case, as an HTTP/1.1 front end may: the name is
 *  case-insensitive. */
export const httpsControls: HttpsAnswers = {
  "dispatch.edge.example /healthz": { status: "200", header: "X-Outside-Edge-Backend: dispatch" },
  "secrets.edge.example /v1/oauth/device_authorization": {
    status: "400",
    header: "x-outside-edge-backend: broker",
  },
  "envoy.edge.example /v1/registry/none": {
    status: "404",
    header: "x-outside-edge-backend:listener",
  },
  "secrets.edge.example /v1/oauth/token": {
    status: "405",
    header: "X-Outside-Edge-Backend: broker",
  },
};
