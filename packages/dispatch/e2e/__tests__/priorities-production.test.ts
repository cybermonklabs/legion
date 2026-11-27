/** What `e2e/acceptance/priorities-production.sh` decides, the look at production's priorities
 *  read that the check c-both-forms-production runs: each look that waits (exit 3) and the line it
 *  ends on, each look that cannot measure (exit 2), and the verdict a look reads from the access
 *  log lines of GET /api/v1/priorities (exit 0 or 1), the full read and the summary told apart by
 *  each line's `bytes`.
 *
 *  Every row runs the script with stand-ins for each program that reaches past this host or reads
 *  its clock: `curl`, which answers Dispatch's /healthz from the row's own table and records each
 *  URL; `gh`, which answers GitHub's compare with the row's status and records its arguments;
 *  `gcloud`, which answers `logging read` with the row's lines and records its arguments; and
 *  `date`, which answers `date -u +%s` with the row's own now and records its arguments. Each is a
 *  bash function the script's bash imports from its environment (`standInFunctions`), so the
 *  script runs unchanged and calls each by the name it calls the program by. So every time a row
 *  reads is the row's own, and a row holds the exact line the script prints. DISPATCH_URL names a
 *  host no row reaches. What this file shares with `priority-list-production.test.ts`, the
 *  stand-in `gh` among it, is `production-look-fixture.ts`'s.
 *
 *  A row of the negative control (WINDOW_START) has a `curl` and a `gh` that fail and are
 *  recorded where they run, so the row holds that the look tested no commit of the fleet.
 */

import { expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";
import {
  accessLogRead,
  curlArguments,
  dispatchPackage,
  dispatchUrl,
  distinct,
  fleetCompare,
  fleetSha,
  gcloudCalls,
  gcloudRecord,
  gh,
  healthz,
  lookEnv,
  mergeSha,
  type Printed,
  printed,
  recorded,
  requestLine,
  requiredInputs,
  said,
  standInFunctions,
  times,
} from "./production-look-fixture";

/** The host's clock in every row that names none of its own. */
const now = "2026-11-25T12:00:00Z";

/** What the stand-ins answer in one look. */
interface Production {
  /** /healthz's `fleet_commit`: `fleetSha` unless given, null where no one build serves. */
  readonly fleetCommit?: string | null;
  /** Whether /healthz gives no answer: `curl` then fails as an unreachable host makes it. */
  readonly healthzDown?: boolean;
  /** The status GitHub's compare answers, `ahead` unless given, or `unanswered` where `gh`
   *  fails. */
  readonly compare?: string;
  /** Whether this look must test no commit: `curl` and `gh` then record the call and fail. */
  readonly testsNoCommit?: boolean;
  /** The text `gcloud logging read` answers, a line each. */
  readonly log?: readonly string[];
  /** Whether `gcloud logging read` fails, as a refused read does. */
  readonly logRefused?: boolean;
  /** The time `date -u +%s` answers, `now` unless given. */
  readonly clock?: string;
}

/** The four stand-ins of a look, as its environment carries them (`standInFunctions`), and the
 *  log in `dir`/log. What they record goes beside them in `dir`. */
function standIns(dir: string, production: Production): Record<string, string> {
  // `$FUNCNAME` names the stand-in, curl or gh, whose function runs the line.
  const refuse = production.testsNoCommit
    ? ['echo "$FUNCNAME: this look was to test no commit of the fleet" >&2', "return 7"]
    : [];
  const curl = [
    ...curlArguments,
    `printf '%s\\n' "$url" >>'${join(dir, "curl-calls")}'`,
    ...refuse,
    ...(production.healthzDown
      ? [
          "echo 'curl: (6) Could not resolve host: dispatch.internal.example' >&2",
          "printf '000'",
          "return 6",
        ]
      : []),
    "case $url in",
    `  */healthz) printf '%s' '${healthz(production.fleetCommit)}' >"$out"; printf '200' ;;`,
    `  *) echo "curl: no answer for $url" >&2; return 6 ;;`,
    "esac",
  ];
  const gcloud = [
    gcloudRecord(dir),
    ...(production.logRefused
      ? [
          "echo 'ERROR: (gcloud.logging.read) PERMISSION_DENIED: Permission denied for all log views.' >&2",
          "return 1",
        ]
      : []),
    `cat '${join(dir, "log")}'`,
  ];
  const epoch = Date.parse(production.clock ?? now) / 1000;
  const date = [
    `printf '%s\\n' "$*" >>'${join(dir, "date-calls")}'`,
    `[ "$*" = '-u +%s' ] || { echo "date: the stand-in answers -u +%s alone, not $*" >&2; return 64; }`,
    `echo ${epoch}`,
  ];
  return standInFunctions(
    dir,
    { curl, gh: gh(dir, production.compare, refuse), gcloud, date },
    production.log
  );
}

/** What one look did: what it printed, and what the stand-ins recorded: each URL `curl` was asked
 *  for, and the arguments of each `gh`, `gcloud` and `date` call. */
interface Look extends Printed {
  readonly urls: readonly string[];
  readonly gh: readonly string[];
  readonly gcloud: readonly (readonly string[])[];
  readonly date: readonly string[];
}

/** One look: the script run from the package root with the stand-ins of `production` in its
 *  environment, the required inputs with `inputs` over them (an undefined one taken away), and
 *  `label` as DISPATCH_E2E_CASE. */
async function look(
  label: string,
  production: Production,
  inputs: Readonly<Record<string, string | undefined>> = {}
): Promise<Look> {
  const dir = directories();
  const functions = standIns(dir, production);
  const run = await runBounded({
    argv: ["bash", "e2e/acceptance/priorities-production.sh"],
    bound: "short",
    cwd: dispatchPackage,
    env: lookEnv(label, functions, { ...requiredInputs, ...inputs }),
    from: import.meta.url,
  });
  return {
    ...printed(run),
    urls: recorded(dir, "curl-calls"),
    gh: recorded(dir, "gh-calls"),
    gcloud: gcloudCalls(dir),
    date: recorded(dir, "date-calls"),
  };
}

/** How many programs past the host a look ran: curl, gh and gcloud. */
function reads(run: Look): number {
  return run.urls.length + run.gh.length + run.gcloud.length;
}

/** One access log line (`requestLine`) for a read of the priorities that took `elapsed` ms and
 *  sent `bytes`. */
function accessLine(
  elapsed: number,
  bytes: number,
  {
    status = 200,
    method = "GET",
    route = "/api/v1/priorities",
    agent = '"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko)"',
  }: { status?: number; method?: string; route?: string; agent?: string } = {}
): string {
  return requestLine({
    method,
    route,
    path: "/api/v1/priorities",
    callerKind: "user",
    caller: "someone@example.com",
    agent,
    status,
    elapsed,
    bytes,
  });
}

/** The bytes of a full read and of a summary, as production's lines carry them. */
const fullBytes = 25550;
const summaryBytes = 151;

/** A line of the full read, and of the summary, for each time of `elapsed`. */
function fullLines(elapsed: readonly number[], bytes = fullBytes): string[] {
  return elapsed.map((ms) => accessLine(ms, bytes));
}
function summaryLines(elapsed: readonly number[], bytes = summaryBytes): string[] {
  return elapsed.map((ms) => accessLine(ms, bytes));
}

/** Lines the gcloud filter may hand back that are no GET of the route answered 200. Each took
 *  9 s, so counting any of them in either form changes a verdict. The first four are lines of the
 *  route, which the count of the route's lines holds; the rest are not. */
const otherLines = [
  accessLine(9000, 400, { method: "POST", status: 201 }),
  accessLine(9000, 120, { status: 500 }),
  accessLine(9000, fullBytes, { status: 304 }),
  accessLine(9000, summaryBytes, {
    status: 500,
    agent: '"probe status=200 elapsed_ms=5 bytes=151 x"',
  }),
  accessLine(9000, fullBytes, { route: "/api/v1/priorities/{id}" }),
  accessLine(9000, fullBytes, { route: "/api/v1/priorities/{id}/relations" }),
  'time=2026-11-25T09:00:00.000Z level=WARN msg="dispatch: request used the previous shared token" route=/api/v1/priorities',
  "",
];

/** The regular expressions of the check's `run`, each line it greps the log for. */
const passingLines = [
  /^the full read: p90 [0-9]+ ms over [0-9]+ reads, at most 200 ms$/,
  /^the summary: p90 [0-9]+ ms over [0-9]+ reads, at most 200 ms$/,
];

/** The lines of the verdict a look reads from the log. */
function verdict(run: Look): string[] {
  return run.lines.filter((line) => /^the (full read|summary): /.test(line));
}

/** The `gcloud` arguments of a look at the three hours from `start` to `end`. */
function logRead(start: string, end: string): string[] {
  return accessLogRead(
    'textPayload:"route=/api/v1/priorities "',
    `timestamp>="${start}"`,
    `timestamp<"${end}"`
  );
}

/** FLEET_SINCE of a look whose three hours have passed: four hours before `now`. */
const settled = { FLEET_SINCE: "2026-11-25T08:00:00Z" };
/** The line a look at a fleet that carries the merge prints before it reads the log. */
const carriesLine = `production's fleet serves ${fleetSha}, which carries ${mergeSha} (GitHub's compare: ahead): this look reads the three hours from 2026-11-25T08:00:00Z`;

const directories = temporaryDirectories("priorities-production");

for (const unset of ["DISPATCH_URL", "MERGE_SHA", "LOGGING_PROJECT"] as const) {
  test(
    `a look with ${unset} unset cannot measure, and reads nothing`,
    async () => {
      const run = await look(`unset-${unset}`, {}, { [unset]: undefined });
      expect(
        {
          code: run.code,
          reason: run.stderr.includes(`required environment variable ${unset} is unset`),
          reads: reads(run),
        },
        said(run)
      ).toEqual({ code: 2, reason: true, reads: 0 });
    },
    rowLimit("short")
  );
}

for (const [label, inputs, reason] of [
  ["MERGE_SHA of 7 digits", { MERGE_SHA: "4f1c2b9" }, "MERGE_SHA is 4f1c2b9"],
  [
    "a FLEET_SINCE this script does not print",
    { FLEET_SINCE: "yesterday" },
    "FLEET_SINCE is yesterday",
  ],
  [
    "a WINDOW_START this script does not print",
    { WINDOW_START: "2026-11-23 20:00" },
    "WINDOW_START is 2026-11-23 20:00",
  ],
  [
    "both FLEET_SINCE and WINDOW_START",
    { FLEET_SINCE: settled.FLEET_SINCE, WINDOW_START: "2026-11-23T20:00:00Z" },
    "FLEET_SINCE and WINDOW_START both name the start of the three hours",
  ],
] as const) {
  test(
    `a look with ${label} cannot measure, and reads nothing`,
    async () => {
      const run = await look(`malformed-${label}`, {}, inputs);
      expect(
        { code: run.code, reason: run.stderr.includes(reason), reads: reads(run) },
        said(run)
      ).toEqual({ code: 2, reason: true, reads: 0 });
    },
    rowLimit("short")
  );
}

test(
  "a /healthz that gives no answer cannot be measured, and the look reads no log",
  async () => {
    const run = await look("healthz-down", { healthzDown: true }, settled);
    expect(
      {
        code: run.code,
        reason: run.stderr.includes(
          `GET ${dispatchUrl}/healthz got no answer: curl: (6) Could not resolve host`
        ),
        compares: run.gh.length,
        logReads: run.gcloud.length,
      },
      said(run)
    ).toEqual({ code: 2, reason: true, compares: 0, logReads: 0 });
  },
  rowLimit("short")
);

for (const [label, production, wanted] of [
  [
    "a fleet behind the merge",
    { compare: "behind" },
    `production's fleet serves ${fleetSha}, which does not carry ${mergeSha} (GitHub's compare: behind)`,
  ],
  [
    "a fleet diverged from the merge",
    { compare: "diverged" },
    `production's fleet serves ${fleetSha}, which does not carry ${mergeSha} (GitHub's compare: diverged)`,
  ],
  [
    "a fleet that serves no one commit",
    { fleetCommit: null },
    "production's fleet serves no one commit: GET /healthz answered HTTP 200 with fleet_commit null",
  ],
  [
    "a compare GitHub does not answer",
    { compare: "unanswered" },
    `GitHub did not answer whether ${fleetSha} carries ${mergeSha}: HTTP 502: Bad Gateway (https://api.github.com/repos/cybermonklabs/legion/compare) `,
  ],
] as const) {
  test(
    `${label} is a look that waits, and it reads no log`,
    async () => {
      const run = await look(`waits-${label}`, { ...production, log: otherLines }, settled);
      expect(
        {
          code: run.code,
          lines: run.lines,
          urls: run.urls,
          compares: run.gh,
          logReads: run.gcloud.length,
        },
        said(run)
      ).toEqual({
        code: 3,
        lines: [wanted],
        urls: [`${dispatchUrl}/healthz`],
        compares: "fleetCommit" in production ? [] : [fleetCompare],
        logReads: 0,
      });
    },
    rowLimit("short")
  );
}

test(
  "the first look at a fleet that carries the merge names FLEET_SINCE, now, and waits three hours",
  async () => {
    const run = await look("fleet-first", { compare: "identical" });
    expect(
      { code: run.code, lines: run.lines, logReads: run.gcloud.length, clock: run.date },
      said(run)
    ).toEqual({
      code: 3,
      lines: [
        `production's fleet serves ${fleetSha}, which carries the merge: read again at or after 2026-11-25T15:00:00Z with FLEET_SINCE=2026-11-25T12:00:00Z`,
      ],
      logReads: 0,
      clock: ["-u +%s"],
    });
  },
  rowLimit("short")
);

test(
  "a look before the three hours from FLEET_SINCE have run names the time they end, and the same FLEET_SINCE",
  async () => {
    const run = await look("fleet-waiting", {}, { FLEET_SINCE: "2026-11-25T11:00:00Z" });
    expect({ code: run.code, lines: run.lines, logReads: run.gcloud.length }, said(run)).toEqual({
      code: 3,
      lines: [
        `production's fleet serves ${fleetSha}, which carries the merge: read again at or after 2026-11-25T14:00:00Z with FLEET_SINCE=2026-11-25T11:00:00Z`,
      ],
      logReads: 0,
    });
  },
  rowLimit("short")
);

test(
  "three hours whose two forms are fast print the two lines the check greps for and exit 0; 2,047 bytes is the summary and 2,048 the full read",
  async () => {
    const log = [
      ...otherLines,
      ...fullLines(distinct.slice(0, 1), 2048),
      ...fullLines(distinct.slice(1, 199)),
      ...fullLines(distinct.slice(199), 90614),
      ...summaryLines(distinct.slice(0, 199)),
      ...summaryLines(distinct.slice(199), 2047),
    ];
    const run = await look("fast", { log }, settled);
    expect(
      {
        code: run.code,
        grepped: passingLines.map(
          (line) => run.lines.filter((printed) => line.test(printed)).length
        ),
        lines: run.lines,
        urls: run.urls,
        compares: run.gh,
        logReads: run.gcloud,
      },
      said(run)
    ).toEqual({
      code: 0,
      grepped: [1, 1],
      lines: [
        carriesLine,
        "the log from 2026-11-25T08:00:00Z to 2026-11-25T11:00:00Z holds 404 lines of route /api/v1/priorities, 400 of them GET answered 200",
        "the full read: p90 180 ms over 200 reads, at most 200 ms",
        "the full read: smallest 2048 bytes, largest 90614 bytes",
        "the summary: p90 180 ms over 200 reads, at most 200 ms",
        "the summary: smallest 151 bytes, largest 2047 bytes",
      ],
      urls: [`${dispatchUrl}/healthz`],
      compares: [fleetCompare],
      logReads: [logRead("2026-11-25T08:00:00Z", "2026-11-25T11:00:00Z")],
    });
  },
  rowLimit("short")
);

test(
  "the p90 of 200 reads is the 180th in order: 20 reads over 200 ms leave a form at most 200 ms, and 21 put it over",
  async () => {
    const log = [
      ...fullLines([...times(10, 120), ...times(20, 450), ...times(170, 120)]),
      ...summaryLines([...times(21, 450), ...times(179, 120)]),
    ];
    const run = await look("p90-rank", { log }, settled);
    expect({ code: run.code, verdict: verdict(run) }, said(run)).toEqual({
      code: 1,
      verdict: [
        "the full read: p90 120 ms over 200 reads, at most 200 ms",
        "the full read: smallest 25550 bytes, largest 25550 bytes",
        "the summary: p90 450 ms over 200 reads, over 200 ms",
        "the summary: smallest 151 bytes, largest 151 bytes",
      ],
    });
  },
  rowLimit("short")
);

test(
  "a form with fewer than 200 reads is a look that waits for the next three hours, which start where these end",
  async () => {
    const run = await look("fewer", { log: fullLines(times(199, 90)) }, settled);
    expect({ code: run.code, verdict: verdict(run), last: run.lines.at(-1) }, said(run)).toEqual({
      code: 3,
      verdict: [
        "the full read: p90 90 ms over 199 reads, at most 200 ms",
        "the full read: smallest 25550 bytes, largest 25550 bytes",
        "the full read: 199 reads, fewer than 200",
        "the summary: 0 reads, fewer than 200",
      ],
      last: "read again at or after 2026-11-25T14:00:00Z with FLEET_SINCE=2026-11-25T11:00:00Z",
    });
  },
  rowLimit("short")
);

test(
  "exit 1 comes before exit 3: a form over 200 ms fails the look while the other holds fewer than 200 reads",
  async () => {
    const log = [...fullLines(times(150, 700)), ...summaryLines(times(300, 40))];
    const run = await look("fail-before-wait", { log }, settled);
    expect({ code: run.code, verdict: verdict(run), last: run.lines.at(-1) }, said(run)).toEqual({
      code: 1,
      verdict: [
        "the full read: p90 700 ms over 150 reads, over 200 ms",
        "the full read: smallest 25550 bytes, largest 25550 bytes",
        "the full read: 150 reads, fewer than 200",
        "the summary: p90 40 ms over 300 reads, at most 200 ms",
        "the summary: smallest 151 bytes, largest 151 bytes",
      ],
      last: "the summary: smallest 151 bytes, largest 151 bytes",
    });
  },
  rowLimit("short")
);

test(
  "the negative control: WINDOW_START reads the three hours from that time, tests no commit of the fleet, and fails the slow full read",
  async () => {
    const log = [
      ...otherLines,
      ...fullLines([...times(2000, 1403), ...times(292, 2868)]),
      ...summaryLines([...times(3000, 150), ...times(451, 522)]),
    ];
    const run = await look(
      "control",
      { testsNoCommit: true, log },
      { WINDOW_START: "2026-11-23T20:00:00Z" }
    );
    const failed = /^the full read: p90 ([0-9]+) ms over [0-9]+ reads, over 200 ms$/.exec(
      run.lines.find((line) => line.startsWith("the full read: p90")) ?? ""
    );
    expect(
      {
        code: run.code,
        overBound: Number(failed?.[1]) > 200,
        lines: run.lines,
        tested: run.urls.length + run.gh.length,
        logReads: run.gcloud,
      },
      said(run)
    ).toEqual({
      code: 1,
      overBound: true,
      lines: [
        "WINDOW_START=2026-11-23T20:00:00Z: this look reads the three hours from that time and skips the test of the fleet's commit",
        "the log from 2026-11-23T20:00:00Z to 2026-11-23T23:00:00Z holds 5747 lines of route /api/v1/priorities, 5743 of them GET answered 200",
        "the full read: p90 2868 ms over 2292 reads, over 200 ms",
        "the full read: smallest 25550 bytes, largest 25550 bytes",
        "the summary: p90 522 ms over 3451 reads, over 200 ms",
        "the summary: smallest 151 bytes, largest 151 bytes",
      ],
      tested: 0,
      logReads: [logRead("2026-11-23T20:00:00Z", "2026-11-23T23:00:00Z")],
    });
  },
  rowLimit("short")
);

test(
  "a WINDOW_START whose three hours are still to come is a look that waits, and it reads no log",
  async () => {
    const run = await look(
      "control-early",
      { testsNoCommit: true, log: otherLines },
      { WINDOW_START: "2026-11-25T10:00:00Z" }
    );
    expect({ code: run.code, last: run.lines.at(-1), reads: reads(run) }, said(run)).toEqual({
      code: 3,
      last: "the three hours from 2026-11-25T10:00:00Z end at 2026-11-25T13:00:00Z, which is still to come: read again at or after that time with WINDOW_START=2026-11-25T10:00:00Z",
      reads: 0,
    });
  },
  rowLimit("short")
);

test(
  "a log read that fails cannot be measured",
  async () => {
    const run = await look("log-refused", { logRefused: true }, settled);
    expect(
      {
        code: run.code,
        reason: run.stderr.includes(
          "gcloud logging read failed: ERROR: (gcloud.logging.read) PERMISSION_DENIED"
        ),
        verdict: verdict(run),
      },
      said(run)
    ).toEqual({ code: 2, reason: true, verdict: [] });
  },
  rowLimit("short")
);

test(
  "a line of the route without its status, elapsed_ms and bytes cannot be measured, and is named by its time",
  async () => {
    const torn =
      'time=2026-11-25T09:15:00.000Z level=INFO msg="dispatch: request" method=GET route=/api/v1/priorities path=/api/v1/priorities caller_kind=user';
    const run = await look(
      "torn-line",
      { log: [...fullLines(times(200, 50)), torn, ...summaryLines(times(200, 50))] },
      settled
    );
    expect(
      {
        code: run.code,
        reason: run.stderr.includes(
          "the line of /api/v1/priorities at 2026-11-25T09:15:00.000Z holds no status, elapsed_ms and bytes"
        ),
        verdict: verdict(run),
      },
      said(run)
    ).toEqual({ code: 2, reason: true, verdict: [] });
  },
  rowLimit("short")
);
