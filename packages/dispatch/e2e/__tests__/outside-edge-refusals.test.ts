/** What `e2e/acceptance/outside-edge-refusals.sh` decides, and its planted-route control: every
 *  row runs the script itself, over HTTP against a stand-in edge on this host's loopback (under
 *  `OUTSIDE_EDGE_ALLOW_HTTP=1`, which lets `http://127.0.0.1:<port>` through and nothing else), or
 *  over HTTPS through a `curl` of the row's own first on PATH, which records the command line the
 *  script gives it and answers as an edge would.
 *
 *  The stand-in (`outside-edge-refusals-edge.ts`, served from a process beside this one by
 *  `fakeHost`) routes by Host, method and path from a table, sets `X-Outside-Edge-Backend` on the
 *  answers of a route that names a marker, and answers 404 with no marker otherwise, as the
 *  Gateway does. `production` below models the routes the edge serves; the refusal lines are the
 *  sixteen of the plan's refusal file, with example hosts of the same shape. Case (b) of the plan
 *  plants a route for exactly one refusal line, once per line, and the run must fail naming that
 *  line and no other: the check's own control, run before merge, as the live canary is after.
 */

import { describe, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { rowLimit, runBounded } from "./bounded-subprocess";
import type { Edge, Route, Seen } from "./outside-edge-refusals-edge";
import { bash, closedPort, curl, fakeHost, type Run, verdict } from "./outside-path-reach-fixtures";

const script = fileURLToPath(new URL("../acceptance/outside-edge-refusals.sh", import.meta.url));
const scriptName = "outside-edge-refusals.sh";

/** The script needs bash, curl, mktemp, and sha256sum or shasum to hash each answer's body. */
const withoutTools = skipWithout(
  "bash",
  "curl",
  "mktemp",
  ...(hostTool("sha256sum") === null && hostTool("shasum") === null ? ["sha256sum"] : [])
);

// On tmpfs where the host has one that allows exec (the `curl` scripts the rows write here run),
// as the reach fixtures' scratch is: on a disk other programs share, a hook of this file, whose
// only hooks make and remove this directory, timed out after the file's last row, at 10,971 ms
// past Bun's 5 s default, in a `bun test` of it on a dev box at a load of about 350. Each run of
// the script writes at most its two lists here, and a few rows a PATH directory or a TMPDIR of
// their own: under 200 files, a tmpfs page each, so 1 MiB a run, times six runs sharing a host.
const scratch = temporaryDirectory("outside-edge-refusals", {
  root: memoryRoot({ need: 6 * 2 ** 20 }),
});

const edgeHost = fakeHost(
  fileURLToPath(new URL("./outside-edge-refusals-edge.ts", import.meta.url)),
  "the stand-in edge host"
);

/** The body of the stand-in's 404 for a request no route takes. */
const notFound = "no route matched\n";
/** The canary's answer: the plan's canary serves this body with status 200 on every route. */
const canaryBody = "outside-edge-canary";

/** One line of a list: a refusal line, with the answer it expects where its sixth field names one,
 *  or a routed control with its backend. */
interface Line {
  name: string;
  method: string;
  sni: string;
  host: string;
  path: string;
  backend?: string;
  expect?: string;
}

/** The plan's refusal file, in its own layout, with example hosts of the same shape. */
const refusalTable = [
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
const routedTable = [
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
const refusals = parsed(refusalTable, "expect");
const routed = parsed(routedTable, "backend");

/** The body of the broker's own 405 for a method its two routes do not serve. */
const methodRefused = '{"error":"method_not_allowed"}';

/** The routes the edge serves, as the plan describes them: Dispatch's host on every path, the
 *  broker's two exact paths, and the listener's `/v1/` prefix. Each marks its backend. No route of
 *  the edge matches on method (the Gateway class supports none), so the broker's two paths take
 *  every method and the broker answers a method other than POST with its own 405, still marked:
 *  the stand-in models that with the POST answer first and a 405 for every other method after it.
 *  Their statuses differ on purpose: a control is read by its marker, whatever status its backend
 *  answers with. */
const production: Route[] = [
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
const controlAnswers: Record<string, { status: number; body: string }> = {
  "dispatch-healthz": { status: 200, body: "dispatch healthz\n" },
  "broker-device": { status: 400, body: '{"error":"invalid_request"}' },
  "listener-v1": { status: 404, body: "listener: no such registry\n" },
};

/** The canary's route for exactly `line`: its Host, an exact match on its method and path. */
function planted(line: Line, answer: Partial<Route> = {}): Route {
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

function sha(text: string): string {
  return createHash("sha256").update(text).digest("hex");
}

/** A stand-in edge that routes by `routes`, and its `http://127.0.0.1:<port>` address. */
async function standIn(routes: Route[]) {
  const edge: Edge = { routes, notFound };
  const started = await edgeHost("/", { method: "POST", body: JSON.stringify(edge) });
  const { port } = (await started.json()) as { port: number };
  return {
    address: `http://127.0.0.1:${port}`,
    async seen(): Promise<Seen[]> {
      return (await (await edgeHost(`/${port}/seen`)).json()) as Seen[];
    },
    async stop(): Promise<void> {
      await edgeHost(`/${port}`, { method: "DELETE" });
    },
  };
}

/** A list file holding `lines`, each ended by `ending`, under the file's scratch directory. */
function listFile(lines: string[], ending = "\n"): string {
  const path = join(scratch, `list-${randomUUID()}.txt`);
  writeFileSync(path, lines.map((line) => `${line}${ending}`).join(""));
  return path;
}

/** The bound of each probe's connect and read in the runs against the stand-in
 *  (`OUTSIDE_EDGE_CONNECT_SECONDS` and `OUTSIDE_EDGE_READ_SECONDS`). The stand-in answers from a
 *  process of its own, which a loaded host can keep off the CPU past the script's own 3 s and 10 s,
 *  as the fake Dispatch of `outside-path-reach-fixtures.ts` was; under this bound such a probe is
 *  late, never a verdict wrong, and the short bound of the run still stops a run that hangs. A row
 *  that reads the script's own defaults leaves both unset. */
const probeSeconds = "40";

/** One run of the script, a bounded child under the short bound, with `env` alone on top of HOME,
 *  PATH (the host's unless `path` is given), TMPDIR, `label` as DISPATCH_E2E_CASE (which tells its
 *  cost line from the others and the script never reads), the flag that lets the stand-in's http
 *  address through, and `probeSeconds`; a name of `env` given no value is left out. `shell` runs
 *  the script where it is given, bash otherwise. */
async function check(
  label: string,
  env: Record<string, string | undefined>,
  options: { path?: string; shell?: string; tmp?: string } = {}
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
    argv: [options.shell ?? bash, script],
    bound: "short",
    env: given,
    from: import.meta.url,
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

/** The lines of a run's stdout that start with `label` and a space. */
function linesOf(run: Run, label: string): string[] {
  return run.stdout.split("\n").filter((line) => line.startsWith(`${label} `));
}

/** What a failed assertion should show of a run: its whole output. */
function said(run: Run): string {
  return `exit ${run.code}\nstdout:\n${run.stdout}\nstderr:\n${run.stderr}`;
}

/** The line the script prints for a probe whose answer was read whole. */
function probeLine(label: string, name: string, status: number, marker: string, body: string) {
  return `${label} ${name} ${status} ${marker} body-sha256 ${sha(body)}`;
}

/** The control lines of a run in which each routed control answered as `production` has it. */
const visibleControls = routed.map((line) => {
  const { status, body } = controlAnswers[line.name] ?? { status: 0, body: "" };
  return probeLine("control", line.name, status, line.backend ?? "", body);
});

/** The request a probe of `line` sends, as the stand-in records it: the line's method, its Host,
 *  its path, and an empty body (`Content-Length: 0`) for a POST. */
function sent(line: Line): Seen {
  return {
    method: line.method,
    host: line.host,
    path: line.path,
    contentLength: line.method === "POST" ? "0" : null,
  };
}

const passVerdict =
  "PASS: every one of the 16 refusal lines was refused (404 with no marker, or the answer its line names), after each of the 3 routed controls showed its marker";

/** The line a refusal line prints where `production` answers it: the broker's own 405, marked,
 *  for a line whose sixth field names that answer, and the Gateway's 404 with no marker for every
 *  other line. */
function refusedLine(line: Line): string {
  return line.expect === undefined
    ? probeLine("refused", line.name, 404, "none", notFound)
    : probeLine("refused", line.name, 405, "broker", methodRefused);
}

function markerNotVisible(name: string): string {
  return `UNKNOWN: the marker of ${name} is not visible, so no refusal can be read`;
}

/** A PATH directory whose `curl` stands for an edge reached over HTTPS: it appends the arguments of
 *  each probe to `log` (one per line, then `--end--`), and answers the probe by its Host header
 *  and the path of its URL from `answers`, where an entry gives the status (000 for no answer),
 *  the marker header as it is written (name and value) or none, the exit code, and a message for
 *  stderr. Every probe no entry names answers 404 with no marker. It writes the headers and the
 *  body where `-D` and `-o` say, and the status on stdout, as curl's `-w '%{http_code}'` does;
 *  `--version` goes to the host's curl, whose version the script reads. */
function httpsCurl(
  answers: Record<string, { status: string; header?: string; exit?: number; message?: string }>
): { path: string; log: string } {
  const directory = join(scratch, `path-${randomUUID()}`);
  mkdirSync(directory);
  const log = join(directory, "probes.log");
  const cases = Object.entries(answers).map(
    ([key, { status, header, exit, message }]) =>
      `  '${key}') status=${status} header='${header ?? ""}' code=${exit ?? 0} message='${message ?? ""}' ;;`
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
      "  *) status=404 header='' code=0 message='' ;;",
      "esac",
      'if [ "$status" != 000 ]; then',
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
function probesIn(log: string): { argv: string[]; files: string[] }[] {
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

/** The command line the script gives curl for `line` against the edge address `address`. */
function httpsArgv(line: Line, address: string, connect = "3", read = "10"): string[] {
  return [
    "-q",
    "--noproxy",
    "*",
    "-g",
    "-sS",
    "--http1.1",
    "--resolve",
    `${line.sni}:443:${address}`,
    "-H",
    `Host: ${line.host}`,
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
    `https://${line.sni}${line.path}`,
  ];
}

/** The `httpsCurl` answers under which every routed control shows its marker, and the broker
 *  answers `broker-token-get`, the line whose sixth field names it, with its own 405, marked. Two
 *  of them write the header name in lower case, as an HTTP/1.1 front end may: the name is
 *  case-insensitive. */
const httpsControls = {
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

// The rows run one after another, not concurrently. Each waits for a bounded child, and the end of
// each child reads the whole process table synchronously to kill what it left (`stopTree` in
// `bounded-subprocess.ts`), which holds this process's event loop meanwhile. On a dev box of
// 19,066 processes one bounded `true` took 6.1 to 8.5 s, and with these rows concurrent 13 of
// their 63 runs were read past the 60 s bound at about 161 s though each had exited, a dash run
// that stops at its first line among them. One at a time, no sibling's read holds the loop while a
// run's bound is counting.
describe(scriptName, () => {
  test.skipIf(withoutTools)(
    "(a) passes when every refusal line is refused, 404 with no marker or the answer its line names, after each routed control showed its marker",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("a", { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() });
        expect(linesOf(run, "control"), said(run)).toEqual(visibleControls);
        expect(linesOf(run, "refused"), said(run)).toEqual(refusals.map(refusedLine));
        expect(linesOf(run, "REACHED"), said(run)).toEqual([]);
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([]);
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
        // The controls first, then each refusal line once, each with its own method, Host and
        // path, and an empty body for a POST.
        expect(await edge.seen()).toEqual([...routed, ...refusals].map(sent));
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  for (const line of refusals) {
    test.skipIf(withoutTools)(
      `(b) fails naming ${line.name} and no other line when a route is planted for exactly that line`,
      async () => {
        const edge = await standIn([planted(line), ...production]);
        try {
          const run = await check(`b ${line.name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "control"), said(run)).toEqual(visibleControls);
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", line.name, 200, "canary", canaryBody),
          ]);
          expect(linesOf(run, "refused"), said(run)).toEqual(
            refusals.filter((other) => other !== line).map(refusedLine)
          );
          expect(verdict(run), said(run)).toBe(`FAIL: reached ${line.name}`);
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  const enroll = refusals.find((line) => line.name === "broker-enroll") as Line;
  const pending = refusals.find((line) => line.name === "broker-pending") as Line;

  test.skipIf(withoutTools)(
    "(b) reads a planted route's own 404 that carries a marker as reached: the marker, not the status, says a route took it",
    async () => {
      const edge = await standIn([
        planted(enroll, { status: 404, marker: "broker", body: "broker: not found\n" }),
        ...production,
      ]);
      try {
        const run = await check("b marker 404", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "REACHED"), said(run)).toEqual([
          probeLine("REACHED", "broker-enroll", 404, "broker", "broker: not found\n"),
        ]);
        expect(verdict(run), said(run)).toBe("FAIL: reached broker-enroll");
        expect(run.code, said(run)).toBe(1);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  for (const status of [200, 403]) {
    test.skipIf(withoutTools)(
      `(b) reads a planted route's ${status} with no marker as reached: only a 404 with no marker is a refusal`,
      async () => {
        const edge = await standIn([
          planted(pending, { status, marker: undefined, body: "unmarked\n" }),
          ...production,
        ]);
        try {
          const run = await check(`b unmarked ${status}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", "broker-pending", status, "none", "unmarked\n"),
          ]);
          expect(verdict(run), said(run)).toBe("FAIL: reached broker-pending");
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  // A refusal line whose sixth field names its answer, `broker-token-get`'s `405:broker`: no route
  // of the edge matches on method, so a GET on the broker's token path reaches the broker, and its
  // own 405, marked, is the refusal. Exactly that status with exactly that marker is refused, and
  // any other answer is a line reached.
  const tokenGet = refusals.find((line) => line.name === "broker-token-get") as Line;

  test.skipIf(withoutTools)(
    "(f) refuses a line whose sixth field names its answer on exactly that answer: broker-token-get's 405 marked broker",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("f expected", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "refused"), said(run)).toContain(
          probeLine("refused", "broker-token-get", 405, "broker", methodRefused)
        );
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  const otherAnswers: {
    name: string;
    routes: Route[];
    status: number;
    marker: string;
    body: string;
  }[] = [
    {
      // The broker's 405 route gone: the Gateway's own 404, with no marker.
      name: "a 404 with no marker",
      routes: production.filter((route) => !(route.path === tokenGet.path && route.status === 405)),
      status: 404,
      marker: "none",
      body: notFound,
    },
    {
      name: "a 405 marked canary",
      routes: [planted(tokenGet, { status: 405, body: methodRefused }), ...production],
      status: 405,
      marker: "canary",
      body: methodRefused,
    },
    {
      name: "a 405 with no marker",
      routes: [
        planted(tokenGet, { status: 405, marker: undefined, body: methodRefused }),
        ...production,
      ],
      status: 405,
      marker: "none",
      body: methodRefused,
    },
    {
      name: "a 200 marked broker",
      routes: [planted(tokenGet, { marker: "broker", body: "token\n" }), ...production],
      status: 200,
      marker: "broker",
      body: "token\n",
    },
  ];
  for (const { name, routes, status, marker, body } of otherAnswers) {
    test.skipIf(withoutTools)(
      `(f) fails naming broker-token-get and no other line when it is answered ${name}, not its 405:broker`,
      async () => {
        const edge = await standIn(routes);
        try {
          const run = await check(`f ${name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "REACHED"), said(run)).toEqual([
            probeLine("REACHED", "broker-token-get", status, marker, body),
          ]);
          expect(linesOf(run, "refused"), said(run)).toEqual(
            refusals.filter((other) => other !== tokenGet).map(refusedLine)
          );
          expect(verdict(run), said(run)).toBe("FAIL: reached broker-token-get");
          expect(run.code, said(run)).toBe(1);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "(f) reads UNKNOWN, not refused, when the named answer came that curl did not read whole",
    async () => {
      const { path } = httpsCurl({
        ...httpsControls,
        "secrets.edge.example /v1/oauth/token": {
          status: "405",
          header: "X-Outside-Edge-Backend: broker",
          exit: 28,
          message: "curl: (28) Operation timed out",
        },
      });
      const run = await check(
        "f cut short",
        { OUTSIDE_EDGE_ADDRESS: "192.0.2.10", OUTSIDE_EDGE_ALLOW_HTTP: undefined, ...planLists() },
        { path }
      );
      expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
        `UNKNOWN broker-token-get 405 broker body-sha256 ${sha("answer 405\n")}; curl exit 28: curl: (28) Operation timed out`,
      ]);
      expect(linesOf(run, "refused").length, said(run)).toBe(15);
      expect(verdict(run), said(run)).toBe(
        "UNKNOWN: no answer could be read whole for broker-token-get"
      );
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  for (const control of routed) {
    test.skipIf(withoutTools)(
      `(c) reads UNKNOWN, and probes no refusal line, when the marker of ${control.name} is removed`,
      async () => {
        const routes = production.map((route) =>
          route.marker === control.backend ? { ...route, marker: undefined } : route
        );
        const edge = await standIn(routes);
        try {
          const run = await check(`c ${control.name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
          });
          expect(linesOf(run, "control"), said(run)).toEqual(
            visibleControls.map((shown, index) =>
              routed[index] === control ? shown.replace(` ${control.backend} `, " none ") : shown
            )
          );
          expect(verdict(run), said(run)).toBe(markerNotVisible(control.name));
          expect(run.code, said(run)).toBe(2);
          // The controls alone: no refusal line is read once a marker is not visible.
          expect(await edge.seen()).toEqual(routed.map(sent));
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "(c) reads UNKNOWN when a routed control answers with another backend's marker",
    async () => {
      const routes = production.map((route) =>
        route.marker === "dispatch" ? { ...route, marker: "broker" } : route
      );
      const edge = await standIn(routes);
      try {
        const run = await check("c wrong marker", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          ...planLists(),
        });
        expect(linesOf(run, "control")[0], said(run)).toBe(
          probeLine("control", "dispatch-healthz", 200, "broker", "dispatch healthz\n")
        );
        expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
        expect(run.code, said(run)).toBe(2);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  const emptyLists: { name: string; lines: string[] }[] = [
    { name: "an empty refusal file", lines: [] },
    { name: "a refusal file of comments and blank lines alone", lines: ["# none yet", "", "  "] },
  ];
  for (const { name, lines } of emptyLists) {
    test.skipIf(withoutTools)(
      `(d) reads UNKNOWN for ${name}, and probes nothing`,
      async () => {
        const edge = await standIn(production);
        try {
          const empty = listFile(lines);
          const run = await check(`d ${name}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            OUTSIDE_EDGE_ROUTED: listFile(routedTable),
            OUTSIDE_EDGE_REFUSALS: empty,
          });
          expect(run.stdout, said(run)).toBe(
            `UNKNOWN: ${empty} lists no refusal line: nothing was tried, so nothing is proved\n`
          );
          expect(run.code, said(run)).toBe(2);
          expect(await edge.seen()).toEqual([]);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "(d) reads UNKNOWN for an empty routed file: no control, so no refusal can be read",
    async () => {
      const empty = listFile([]);
      const run = await check("d routed", {
        OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
        OUTSIDE_EDGE_ROUTED: empty,
        OUTSIDE_EDGE_REFUSALS: listFile(refusalTable),
      });
      expect(run.stdout, said(run)).toBe(
        `UNKNOWN: ${empty} lists no routed control: no marker can be seen, so no refusal can be read\n`
      );
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  test.skipIf(withoutTools)(
    "(e) reads UNKNOWN when nothing listens at the edge address, never PASS",
    async () => {
      const run = await check("e", {
        OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
        ...planLists(),
      });
      const controls = linesOf(run, "control");
      expect(controls.length, said(run)).toBe(3);
      for (const [index, shown] of controls.entries()) {
        expect(shown, said(run)).toStartWith(
          `control ${routed[index]?.name} 000 none body-sha256 none; curl exit 7: `
        );
      }
      expect(linesOf(run, "refused"), said(run)).toEqual([]);
      expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  test.skipIf(withoutTools)(
    "(e) reads UNKNOWN at an edge address that answers nothing, under the script's own bounds",
    async () => {
      // TEST-NET-1 (RFC 5737), reserved for documentation: a network drops the connect or
      // refuses it, and either way no marker is seen. The default bounds are 3 s to connect, so
      // the three controls take at most about 9 s.
      const run = await check("e blackhole", {
        OUTSIDE_EDGE_ADDRESS: "192.0.2.1",
        OUTSIDE_EDGE_ALLOW_HTTP: undefined,
        OUTSIDE_EDGE_CONNECT_SECONDS: undefined,
        OUTSIDE_EDGE_READ_SECONDS: undefined,
        ...planLists(),
      });
      expect(linesOf(run, "control").length, said(run)).toBe(3);
      for (const shown of linesOf(run, "control")) {
        expect(shown, said(run)).toMatch(/^control \S+ 000 none body-sha256 none; curl exit \d+: /);
      }
      expect(verdict(run), said(run)).toBe(markerNotVisible("dispatch-healthz"));
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  test.skipIf(withoutTools)(
    "probes https://<sni><path> through --resolve <sni>:443:<address>, with each line's Host and method and an empty body for a POST, and leaves no scratch behind",
    async () => {
      const { path, log } = httpsCurl(httpsControls);
      const tmp = join(scratch, `tmp-${randomUUID()}`);
      mkdirSync(tmp);
      const run = await check(
        "https",
        {
          OUTSIDE_EDGE_ADDRESS: "192.0.2.10",
          OUTSIDE_EDGE_ALLOW_HTTP: undefined,
          OUTSIDE_EDGE_CONNECT_SECONDS: undefined,
          OUTSIDE_EDGE_READ_SECONDS: undefined,
          ...planLists(),
        },
        { path, tmp }
      );
      expect(linesOf(run, "control"), said(run)).toEqual([
        probeLine("control", "dispatch-healthz", 200, "dispatch", "answer 200\n"),
        probeLine("control", "broker-device", 400, "broker", "answer 400\n"),
        probeLine("control", "listener-v1", 404, "listener", "answer 404\n"),
      ]);
      expect(verdict(run), said(run)).toBe(passVerdict);
      expect(run.code, said(run)).toBe(0);
      const probes = probesIn(log);
      expect(probes.map((probe) => probe.argv)).toEqual(
        [...routed, ...refusals].map((line) => httpsArgv(line, "192.0.2.10"))
      );
      // Each probe's headers and body go to files of their own under TMPDIR, which the run
      // removes before it exits.
      const files = probes.flatMap((probe) => probe.files);
      expect(new Set(files).size).toBe(files.length);
      for (const file of files) expect(file.startsWith(`${tmp}/`)).toBe(true);
      expect(readdirSync(tmp)).toEqual([]);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  const unread: { name: string; answer: { status: string; exit: number; message: string } }[] = [
    {
      name: "a TLS failure",
      answer: { status: "000", exit: 60, message: "curl: (60) SSL certificate problem" },
    },
    {
      name: "a connect that failed",
      answer: { status: "000", exit: 7, message: "curl: (7) Failed to connect" },
    },
    {
      name: "a 404 with no marker whose answer was cut short",
      answer: { status: "404", exit: 28, message: "curl: (28) Operation timed out" },
    },
  ];
  for (const { name, answer } of unread) {
    test.skipIf(withoutTools)(
      `reads UNKNOWN naming the refusal line whose probe met ${name}, when no line was reached`,
      async () => {
        const { path } = httpsCurl({
          ...httpsControls,
          "secrets.tailnet.example /v1/oauth/token": answer,
        });
        const run = await check(
          `unread ${answer.exit}`,
          {
            OUTSIDE_EDGE_ADDRESS: "192.0.2.10",
            OUTSIDE_EDGE_ALLOW_HTTP: undefined,
            ...planLists(),
          },
          { path }
        );
        const body = answer.status === "000" ? "none" : sha(`answer ${answer.status}\n`);
        expect(linesOf(run, "UNKNOWN"), said(run)).toEqual([
          `UNKNOWN tailnet-secrets ${answer.status} none body-sha256 ${body}; curl exit ${answer.exit}: ${answer.message}`,
        ]);
        expect(linesOf(run, "refused").length, said(run)).toBe(15);
        expect(verdict(run), said(run)).toBe(
          "UNKNOWN: no answer could be read whole for tailnet-secrets"
        );
        expect(run.code, said(run)).toBe(2);
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "fails on a reached line whatever else the run could not read, and reads a marked answer cut short as reached",
    async () => {
      const { path } = httpsCurl({
        ...httpsControls,
        "secrets.tailnet.example /v1/oauth/token": {
          status: "000",
          exit: 7,
          message: "curl: (7) Failed to connect",
        },
        "envoy.edge.example /metrics": {
          status: "200",
          header: "X-Outside-Edge-Backend: listener",
          exit: 28,
          message: "curl: (28) Operation timed out",
        },
      });
      const run = await check(
        "reached and unread",
        { OUTSIDE_EDGE_ADDRESS: "192.0.2.10", OUTSIDE_EDGE_ALLOW_HTTP: undefined, ...planLists() },
        { path }
      );
      expect(linesOf(run, "REACHED"), said(run)).toEqual([
        `REACHED listener-metrics 200 listener body-sha256 ${sha("answer 200\n")}; curl exit 28: curl: (28) Operation timed out`,
      ]);
      expect(linesOf(run, "UNKNOWN").length, said(run)).toBe(1);
      expect(verdict(run), said(run)).toBe("FAIL: reached listener-metrics");
      expect(run.code, said(run)).toBe(1);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  test.skipIf(withoutTools)(
    "reads lists with comments, blank lines, tabs and CRLF line endings",
    async () => {
      const edge = await standIn(production);
      try {
        const run = await check("crlf", {
          OUTSIDE_EDGE_ADDRESS: edge.address,
          OUTSIDE_EDGE_ROUTED: listFile(["# the routed controls", "", ...routedTable], "\r\n"),
          OUTSIDE_EDGE_REFUSALS: listFile([
            "  # name method sni host path",
            ...refusalTable.map((line) => line.replaceAll(/ +/g, "\t")),
            "",
          ]),
        });
        expect(verdict(run), said(run)).toBe(passVerdict);
        expect(run.code, said(run)).toBe(0);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  // A run's output goes to logs anyone may read, such as a public CI log, so it names this
  // machine's host name nowhere: neither a HOSTNAME the environment hands bash, which bash keeps,
  // nor the one bash reads from the system where the environment names none.
  test.skipIf(withoutTools)(
    "prints the machine's host name in no line, whether the environment names it or bash reads it",
    async () => {
      const edge = await standIn(production);
      try {
        const planted = `planted-host-${randomUUID()}`;
        for (const { label, name, env } of [
          { label: "planted", name: planted, env: { HOSTNAME: planted } },
          { label: "read", name: hostname(), env: {} },
        ]) {
          const run = await check(`host name ${label}`, {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...planLists(),
            ...env,
          });
          expect(verdict(run), said(run)).toBe(passVerdict);
          expect(run.stdout.split("\n").filter((line) => line.includes(name))).toEqual([]);
        }
      } finally {
        await edge.stop();
      }
    },
    // The script twice, one after the other, each a bounded child under the short bound.
    rowLimit("short", "short")
  );

  // The run's `curl` sends SIGTERM to the script's own bash at the first probe, found as the
  // topmost ancestor whose command line runs the script (the command substitution that starts
  // curl is a fork of it with the same command line), so the row needs /proc. bash runs the trap
  // once that probe returns, so the stand-in sees one request.
  test.skipIf(withoutTools || !existsSync("/proc/self/stat"))(
    "reads UNKNOWN, never PASS, when a signal stops the run before its verdict, and leaves no scratch behind",
    async () => {
      const edge = await standIn(production);
      try {
        const directory = join(scratch, `path-${randomUUID()}`);
        mkdirSync(directory);
        const tmp = join(scratch, `tmp-${randomUUID()}`);
        mkdirSync(tmp);
        writeFileSync(
          join(directory, "curl"),
          [
            `#!${bash}`,
            `case " $* " in *" --version "*) exec ${curl} "$@" ;; esac`,
            "signal() {",
            "  local p=$PPID target='' stat",
            '  while [ "$p" -gt 1 ] && [ -r "/proc/$p/stat" ]; do',
            `    case $(tr '\\0' ' ' <"/proc/$p/cmdline") in *"${script}"*) target=$p ;; esac`,
            '    stat=$(cat "/proc/$p/stat")',
            // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own expansion, the fields after the command name.
            "    set -- ${stat##*) }",
            "    p=$2",
            "  done",
            '  [ -z "$target" ] || kill -TERM "$target"',
            "}",
            "signal",
            `exec ${curl} "$@"`,
            "",
          ].join("\n"),
          { mode: 0o755 }
        );
        const run = await check(
          "signal",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { path: `${directory}:${process.env.PATH ?? ""}`, tmp }
        );
        expect(verdict(run), said(run)).toBe(
          "UNKNOWN: the run was stopped by a signal before its verdict"
        );
        expect(run.code, said(run)).toBe(2);
        expect(linesOf(run, "refused"), said(run)).toEqual([]);
        expect(await edge.seen()).toEqual([sent(routed[0] as Line)]);
        expect(readdirSync(tmp)).toEqual([]);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  /** One input the script refuses before it probes anything: the variables a row sets on top of
   *  the plan's lists and a stand-in address (`null` unsets one, and `list:<text>` is a file
   *  holding that text), and what it prints. In `env`, `{routed}` and `{refusals}` stand for the
   *  plan's two files; in `says`, for the two files the run was given, and `{address}` for the
   *  stand-in's address. */
  const refusedInputs: { name: string; env: Record<string, string | null>; says: string }[] = [
    {
      name: "no OUTSIDE_EDGE_ADDRESS",
      env: { OUTSIDE_EDGE_ADDRESS: null },
      says: "OUTSIDE_EDGE_ADDRESS is unset: set it to the edge's IPv4 address",
    },
    {
      name: "no OUTSIDE_EDGE_ROUTED",
      env: { OUTSIDE_EDGE_ROUTED: null },
      says: "OUTSIDE_EDGE_ROUTED is unset: set it to the file of 'name method sni host path backend' lines, one routed control per backend",
    },
    {
      name: "no OUTSIDE_EDGE_REFUSALS",
      env: { OUTSIDE_EDGE_REFUSALS: null },
      says: "OUTSIDE_EDGE_REFUSALS is unset: set it to the file of 'name method sni host path [status:marker]' lines the edge must refuse",
    },
    ...["edge.example", "010.0.0.1", "192.0.2.1:443", "::1", "https://192.0.2.1"].map(
      (address) => ({
        name: `the address ${address}`,
        env: { OUTSIDE_EDGE_ADDRESS: address },
        says: `OUTSIDE_EDGE_ADDRESS must be an IPv4 address written as four numbers of 0 to 255 with no leading zero, not ${address}`,
      })
    ),
    {
      name: "an http address without OUTSIDE_EDGE_ALLOW_HTTP",
      env: { OUTSIDE_EDGE_ALLOW_HTTP: null },
      says: "OUTSIDE_EDGE_ADDRESS must be the edge's IPv4 address, not {address}: http is for this script's own tests, with OUTSIDE_EDGE_ALLOW_HTTP=1",
    },
    {
      name: "an http address at another host than 127.0.0.1",
      env: { OUTSIDE_EDGE_ADDRESS: "http://10.0.0.1:8080" },
      says: "OUTSIDE_EDGE_ADDRESS may be http only as http://127.0.0.1:<port>, for this script's own tests, not http://10.0.0.1:8080",
    },
    {
      name: "a refusal file that does not exist",
      env: { OUTSIDE_EDGE_REFUSALS: "{refusals}.missing" },
      says: "OUTSIDE_EDGE_REFUSALS names {refusals}, which could not be read: could not open it",
    },
    {
      name: "a refusal line of seven fields",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:a GET dispatch.edge.example dispatch.tailnet.example /x 405:broker extra",
      },
      says: "line 1 of {refusals} is not 'name method sni host path [status:marker]': a GET dispatch.edge.example dispatch.tailnet.example /x 405:broker extra",
    },
    ...["405", "405:", ":broker", "40x:broker", "099:broker", "600:broker", "405:br/oker"].map(
      (field) => ({
        name: `the expected answer ${field}`,
        env: {
          OUTSIDE_EDGE_REFUSALS: `list:a GET dispatch.edge.example dispatch.tailnet.example /x ${field}`,
        },
        says: `line 1 of {refusals}: the expected answer ${field} is not '<status>:<marker>', a status of 100 to 599 and a marker of letters, digits, '.', '_' and '-'`,
      })
    ),
    {
      name: "a routed line of five fields",
      env: {
        OUTSIDE_EDGE_ROUTED: "list:a GET dispatch.edge.example dispatch.edge.example /healthz",
      },
      says: "line 1 of {routed} is not 'name method sni host path backend': a GET dispatch.edge.example dispatch.edge.example /healthz",
    },
    {
      name: "an address as the Host",
      env: { OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example 10.0.0.1 /x" },
      says: "line 1 of {refusals}: the host 10.0.0.1 is not a host name; no route can name an address, so none could be planted for it",
    },
    {
      name: "an address as the SNI",
      env: { OUTSIDE_EDGE_REFUSALS: "list:a GET 10.0.0.1 dispatch.edge.example /x" },
      says: "line 1 of {refusals}: the sni 10.0.0.1 is not a host name",
    },
    {
      name: "a HEAD line",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a HEAD dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals}: the method HEAD is not one of GET, POST, PUT, PATCH, DELETE or OPTIONS",
    },
    {
      name: "a path without its leading slash",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example dispatch.tailnet.example x/y",
      },
      says: "line 1 of {refusals}: the path x/y is not an absolute path of URL characters",
    },
    {
      name: "a path holding a fragment",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example dispatch.tailnet.example /x#y",
      },
      says: "line 1 of {refusals}: the path /x#y is not an absolute path of URL characters",
    },
    {
      name: "two lines of one name",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:a GET dispatch.edge.example dispatch.tailnet.example /x\na GET dispatch.edge.example dispatch.tailnet.example /y",
      },
      says: "line 2 of {refusals}: the name a is another line's; give each line a name of its own",
    },
    {
      name: "a refusal line named as a routed control",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:dispatch-healthz GET dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals}: the name dispatch-healthz is another line's; give each line a name of its own",
    },
    {
      name: "a byte outside printable ASCII",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:# caf\u00e9\na GET dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals} holds byte 0xC3, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings",
    },
    {
      // bash's `read -d ''` stops at a NUL, so the line holding it is named from the count of the
      // LFs before it.
      name: "a NUL byte",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:# one\na GET dispatch.edge.example dispatch.tailnet.example /x\u0000y",
      },
      says: "line 2 of {refusals} holds byte 0x00, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings",
    },
    {
      name: "a connect bound under 2 s",
      env: { OUTSIDE_EDGE_CONNECT_SECONDS: "1" },
      says: "OUTSIDE_EDGE_CONNECT_SECONDS must be a whole number of seconds from 2 to 999999, not 1",
    },
    {
      name: "a read bound that is no number",
      env: { OUTSIDE_EDGE_READ_SECONDS: "ten" },
      says: "OUTSIDE_EDGE_READ_SECONDS must be a whole number of seconds from 2 to 999999, not ten",
    },
  ];
  for (const { name, env, says } of refusedInputs) {
    test.skipIf(withoutTools)(
      `refuses ${name}, before it probes anything`,
      async () => {
        const edge = await standIn(production);
        try {
          const files = planLists();
          const given: Record<string, string | undefined> = {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...files,
          };
          for (const [variable, value] of Object.entries(env)) {
            if (value === null) given[variable] = undefined;
            else if (value.startsWith("list:")) given[variable] = listFile([value.slice(5)]);
            else
              given[variable] = value
                .replace("{refusals}", files.OUTSIDE_EDGE_REFUSALS ?? "")
                .replace("{routed}", files.OUTSIDE_EDGE_ROUTED ?? "");
          }
          const expected = says
            .replace("{address}", edge.address)
            .replace("{refusals}", given.OUTSIDE_EDGE_REFUSALS ?? "")
            .replace("{routed}", given.OUTSIDE_EDGE_ROUTED ?? "");
          const run = await check(`refuses ${name}`, given);
          expect(run.stdout, said(run)).toBe(`UNKNOWN: ${expected}\n`);
          expect(run.code, said(run)).toBe(2);
          expect(await edge.seen()).toEqual([]);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "refuses a refusal file it cannot read whole, in bash's own words",
    async () => {
      const directory = join(scratch, `directory-${randomUUID()}`);
      mkdirSync(directory);
      const run = await check("unreadable", {
        OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
        OUTSIDE_EDGE_ROUTED: listFile(routedTable),
        OUTSIDE_EDGE_REFUSALS: directory,
      });
      expect(run.stdout, said(run)).toStartWith(
        `UNKNOWN: OUTSIDE_EDGE_REFUSALS names ${directory}, which could not be read whole: `
      );
      expect(run.stdout, said(run)).toContain("Is a directory");
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound.
    rowLimit("short")
  );

  for (const shell of ["dash", "zsh"]) {
    test.skipIf(withoutTools || hostTool(shell) === null)(
      `refuses to run under ${shell}, naming bash`,
      async () => {
        const run = await check(
          `shell ${shell}`,
          { OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`, ...planLists() },
          { shell: hostTool(shell) ?? shell }
        );
        expect(run.stdout, said(run)).toBe(`UNKNOWN: run this with bash: bash ${scriptName}\n`);
        expect(run.code, said(run)).toBe(2);
      },
      // The script, a bounded child under the short bound.
      rowLimit("short")
    );
  }
});
