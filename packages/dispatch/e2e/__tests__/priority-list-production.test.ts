/** What `e2e/acceptance/priority-list-production.sh` decides, the probe of production's issue list
 *  filtered by a priority that the check c-both-plans-production runs: each look that waits (exit
 *  3) and the line it ends on, each look that cannot measure (exit 2), and the verdict a look
 *  reads from the probe's access log lines (exit 0 or 1).
 *
 *  Every row runs the script with stand-ins for each program that reaches past this host: `curl`,
 *  which answers Dispatch's /healthz, priorities and issue reads from the row's own table and
 *  records each request's User-Agent and URL; `gh`, which answers GitHub's compare with the row's
 *  status and records its arguments; `gcloud`, which answers `logging read` with the row's lines,
 *  the probe id its filter names put in each, and records its arguments; and `sleep`, which
 *  records its argument and returns at once. Each is a bash function the script's bash imports
 *  from its environment, so the script runs unchanged, calls each by the name it calls the
 *  program by, and starts no process for a request (`standIns` says why). So the script's 30 s
 *  pauses between reads of the log cost nothing, and the rows count them; the script itself holds
 *  no variable a test sets for its waits. The three-hour wait and the two-hour window are read
 *  against the host's own clock, from the FLEET_SINCE and PROBE_AT a row gives. DISPATCH_URL names
 *  a host no row reaches.
 *
 *  A row that replays an earlier probe (PROBE_ID, PROBE_AT and PROBE_STOPPED given) has a `curl`
 *  that fails and is recorded where it runs, so the row holds that the look sent nothing.
 */

import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { masked, rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

type Form = "page" | "open" | "whole";
const forms: readonly Form[] = ["page", "open", "whole"];

/** MERGE_SHA, and the commit production's fleet serves in every row that names none of its own. */
const mergeSha = "4f1c2b9e8d7a6c5b4a39281706f5e4d3c2b1a090";
const fleetSha = "9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3";
/** The 16 hex digits of a probe id a replay row makes up. */
const nonce = "0123456789abcdef";
/** DISPATCH_URL in every row: a host no row reaches. */
const dispatchUrl = "https://dispatch.internal.example";
/** The arguments of `gh` for GitHub's compare of MERGE_SHA with `fleetSha`. */
const fleetCompare = `api repos/cybermonklabs/legion/compare/${mergeSha}...${fleetSha} --jq .status`;

/** The inputs every look requires. The bearer and the logging project are made up. */
const required: Readonly<Record<string, string>> = {
  DISPATCH_URL: dispatchUrl,
  DISPATCH_TOKEN: "not-a-real-bearer",
  MERGE_SHA: mergeSha,
  LOGGING_PROJECT: "example-logging-project",
};

/** GET /api/v1/priorities as the stand-in answers it: the second priority holds the most open
 *  issues, 609, counted over every status of `work.by_status`. */
const priorities = JSON.stringify({
  priorities: [
    { id: "small-priority", work: { by_status: { todo: 3, in_progress: 1, backlog: 0 } } },
    {
      id: "largest-priority",
      work: { by_status: { todo: 400, in_progress: 100, backlog: 100, icebox: 9 } },
    },
    { id: "middle-priority", work: { by_status: { todo: 16, triage: 2 } } },
  ],
  work_under_no_priority: { count: 0, by_project: {}, issues: [] },
  priority_work_in_backlog_or_icebox: { count: 0, by_priority: {}, issues: [] },
});

/** What the stand-ins answer in one look. */
interface Production {
  /** /healthz's `fleet_commit`: `fleetSha` unless given, null where no one build serves. */
  readonly fleetCommit?: string | null;
  /** The status GitHub's compare answers, or `unanswered` where `gh` fails. */
  readonly compare?: string;
  /** The form every read of which curl times at 1.5 s; every other read takes 40 ms. */
  readonly slowForm?: Form;
  /** The status every issue read answers, 200 unless given. */
  readonly issueStatus?: number;
  /** Whether every issue read answers a list that holds no issue. */
  readonly noIssue?: boolean;
  /** Whether this look must send nothing: `curl` then records the request and fails. */
  readonly sendsNothing?: boolean;
  /** The text `gcloud logging read` answers, a line each, `@PROBE@` standing for the probe id. */
  readonly log?: readonly string[];
  /** How many reads of the log answer nothing before the reads that answer `log`. */
  readonly lateReads?: number;
}

/** The four stand-ins of a look, as its environment carries them, and the log in `dir`/log. Each
 *  stand-in is a bash function in a `BASH_FUNC_<name>%%` variable, the form in which bash exports
 *  a function, so the script's bash imports it and runs it in place of the program of that name:
 *  each runs in the shell that calls it and starts no process. A look of 600 reads that started a
 *  process for each one took from 1.9 s to 60.1 s in one leg of the repeated harness lane (run
 *  45866491242, round 3), so its 60 s bound decided the row there. Executables of the same names
 *  come first on PATH and only fail, so a shell that did not import a function reaches nothing
 *  past this host. What the stand-ins record goes in `dir`, and lasts from one look of a row to the
 *  next; each also records the process and the subshell depth it ran in, which the row "each
 *  stand-in a look calls runs in the shell that calls it" reads. */
function standIns(dir: string, production: Production): Record<string, string> {
  const time = (form: Form) => (production.slowForm === form ? "1.500000" : "0.040000");
  const status = production.issueStatus ?? 200;
  const issues = production.noIssue ? [] : [{ key: "ACME-1", title: "One issue" }];
  const page = JSON.stringify({ issues, total: issues.length, limit: 50, offset: 0 });
  const list = JSON.stringify(issues);
  const fleet = production.fleetCommit === undefined ? fleetSha : production.fleetCommit;
  const healthz = JSON.stringify({ ok: true, commit: fleet, fleet_commit: fleet });
  const shells = join(dir, "shells");
  const curl = [
    "local out='' agent='' url=''",
    'while [ "$#" -gt 0 ]; do',
    "  case $1 in",
    "    -o) out=$2; shift ;;",
    "    -A) agent=$2; shift ;;",
    "    -w | -H | --max-time) shift ;;",
    "    -*) ;;",
    "    *) url=$1 ;;",
    "  esac",
    "  shift",
    "done",
    `printf '%s %s\\n' "$agent" "$url" >>'${join(dir, "curl-calls")}'`,
    `printf 'curl %s %s\\n' "$BASHPID" "$BASH_SUBSHELL" >>'${shells}'`,
    ...(production.sendsNothing
      ? ["echo 'curl: this look was to send nothing' >&2", "return 7"]
      : []),
    "case $url in",
    `  */healthz) printf '%s' '${healthz}' >"$out"; echo '200 0.012000' ;;`,
    `  */api/v1/priorities) printf '%s' '${priorities}' >"$out"; echo '200 0.031000' ;;`,
    `  *'/api/v1/issues?'*'&limit=50') printf '%s' '${page}' >"$out"; echo '${status} ${time("page")}' ;;`,
    `  *'/api/v1/issues?'*'&open=true') printf '%s' '${list}' >"$out"; echo '${status} ${time("open")}' ;;`,
    `  *'/api/v1/issues?advances='*) printf '%s' '${list}' >"$out"; echo '${status} ${time("whole")}' ;;`,
    `  *) echo "curl: no answer for $url" >&2; return 6 ;;`,
    "esac",
  ];
  const gh = [
    `printf '%s\\n' "$*" >>'${join(dir, "gh-calls")}'`,
    `printf 'gh %s %s\\n' "$BASHPID" "$BASH_SUBSHELL" >>'${shells}'`,
    production.compare === "unanswered"
      ? "echo 'HTTP 502: Bad Gateway (https://api.github.com/repos/cybermonklabs/legion/compare)' >&2; return 1"
      : `echo '${production.compare ?? "ahead"}'`,
  ];
  // The probe id the filter names, a commit's 40 hex digits, a slash and 16 hex digits, is read
  // with a glob and not a regular expression, which would set the script's own BASH_REMATCH. The
  // log is read whole and written back with the probe id in it, its last newline or the lack of one
  // kept, the bytes `sed` wrote.
  const agent = "user_agent=priority-list-production/";
  const probeId = `${"[0-9a-f]".repeat(40)}/${"[0-9a-f]".repeat(16)}`;
  const gcloud = [
    `local dir='${dir}' calls=0 probe='' argument='' text=''`,
    `{ printf 'call\\0'; printf '%s\\0' "$@"; } >>"$dir/gcloud-calls"`,
    `printf 'gcloud %s %s\\n' "$BASHPID" "$BASH_SUBSHELL" >>'${shells}'`,
    'read -r calls 2>/dev/null <"$dir/gcloud-count" || calls=0',
    "calls=$((calls + 1))",
    `printf '%s\\n' "$calls" >"$dir/gcloud-count"`,
    `((calls > ${production.lateReads ?? 0})) || return 0`,
    'for argument in "$@"; do',
    `  if [[ $argument == *${agent}${probeId}/* ]]; then`,
    `    probe=\${argument#*${agent}}`,
    `    probe=\${probe:0:57}`,
    "  fi",
    "done",
    "[[ -n $probe ]] || return 0",
    `IFS= read -r -d '' text <"$dir/log" || true`,
    `printf '%s' "\${text//@PROBE@/"$probe"}"`,
  ];
  const sleep = [
    `printf '%s\\n' "$*" >>'${join(dir, "sleeps")}'`,
    `printf 'sleep %s %s\\n' "$BASHPID" "$BASH_SUBSHELL" >>'${shells}'`,
  ];
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  const unreached = [
    "#!/bin/sh",
    `echo "$0: this row's stand-in function of that name did not reach the script's shell" >&2`,
    "exit 127",
    "",
  ].join("\n");
  const env: Record<string, string> = { PATH: `${bin}:${process.env.PATH ?? ""}` };
  for (const [name, body] of Object.entries({ curl, gh, gcloud, sleep })) {
    writeFileSync(join(bin, name), unreached, { mode: 0o755 });
    env[`BASH_FUNC_${name}%%`] = ["() {", ...body.map((line) => `  ${line}`), "}"].join("\n");
  }
  writeFileSync(join(dir, "log"), (production.log ?? []).join("\n"));
  return env;
}

/** The text of a file of `dir`, empty where the file is missing. Any other failure to read it
 *  throws. */
function recordedText(dir: string, name: string): string {
  try {
    return readFileSync(join(dir, name), "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return "";
    throw error;
  }
}

/** The lines a file of `dir` holds, none where the file is missing. */
function recorded(dir: string, name: string): string[] {
  return recordedText(dir, name).split("\n").slice(0, -1);
}

/** What one look did: its exit code and output, the lines of its stdout but the empty ones, and
 *  what the stand-ins recorded so far in the row's directory, this look's and any earlier look's of
 *  the row: each request `curl` was asked for, the arguments of each `gh` and `gcloud` call, and
 *  each pause `sleep` was asked for. */
interface Look {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  readonly lines: readonly string[];
  readonly requests: readonly { readonly agent: string; readonly url: string }[];
  readonly gh: readonly string[];
  readonly gcloud: readonly (readonly string[])[];
  readonly sleeps: readonly string[];
}

/** One look: the script run from the package root with the stand-ins of `production` in its
 *  environment, the required inputs with `inputs` over them (an undefined one taken away), and
 *  `label` as DISPATCH_E2E_CASE, which the script never reads and which tells the cost lines apart. */
async function look(
  dir: string,
  label: string,
  production: Production,
  inputs: Readonly<Record<string, string | undefined>> = {}
): Promise<Look> {
  const env: Record<string, string> = {
    DISPATCH_E2E_CASE: label,
    HOME: process.env.HOME ?? "",
    ...standIns(dir, production),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
  };
  for (const [name, value] of Object.entries({ ...required, ...inputs })) {
    if (value !== undefined) env[name] = value;
  }
  const run = await runBounded({
    argv: ["bash", "e2e/acceptance/priority-list-production.sh"],
    bound: "short",
    cwd: dispatchPackage,
    env,
    from: import.meta.url,
  });
  return {
    code: run.code,
    stdout: run.stdout,
    stderr: run.stderr,
    lines: run.stdout.split("\n").filter((line) => line !== ""),
    requests: recorded(dir, "curl-calls").map((line) => {
      const space = line.indexOf(" ");
      return { agent: line.slice(0, space), url: line.slice(space + 1) };
    }),
    gh: recorded(dir, "gh-calls"),
    gcloud: recordedText(dir, "gcloud-calls")
      .split("call\0")
      .slice(1)
      .map((call) => call.split("\0").slice(0, -1)),
    sleeps: recorded(dir, "sleeps"),
  };
}

/** What a row's failure says of a look: its exit code and all it printed. */
function said({ code, stdout, stderr }: Look): string {
  return ` The script exited ${code}. Its stdout read:\n${stdout || "(nothing)"}\nIts stderr read:\n${masked(stderr) || "(nothing)"}`;
}

/** The epoch seconds of a time as the script prints one, and the reverse. */
function secondsOf(time: string): number {
  return Date.parse(time) / 1000;
}
function stamp(seconds: number): string {
  return new Date(seconds * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The host's clock around a look, in whole seconds: no earlier than the look's start and no later
 *  than its end. */
function clock(): { from: number; to: () => number } {
  const from = Math.floor(Date.now() / 1000);
  return { from, to: () => Math.ceil(Date.now() / 1000) };
}

/** One access log line as Dispatch's server writes it through slog's text handler (the example of
 *  "Access log" in `cmd/dispatch/README.md`), for a read of `form` by the probe `probe`. */
function accessLine(
  form: Form,
  elapsed: number,
  {
    status = 200,
    method = "GET",
    route = "/api/v1/issues",
    probe = "@PROBE@",
  }: { status?: number; method?: string; route?: string; probe?: string } = {}
): string {
  return `time=2026-11-25T09:00:00.000Z level=INFO msg="dispatch: request" method=${method} route=${route} path=/api/v1/issues caller_kind=shared-token caller="" address=203.0.113.9 peer=10.8.0.12 user_agent=priority-list-production/${probe}/${form} status=${status} elapsed_ms=${elapsed} bytes=48211`;
}

/** The lines of `form`, one for each time of `elapsed`. */
function formLines(form: Form, elapsed: readonly number[]): string[] {
  return elapsed.map((ms) => accessLine(form, ms));
}

/** `count` of `value`. */
function times<T>(count: number, value: T): T[] {
  return Array.from({ length: count }, () => value);
}

/** 200 distinct times from 200 down to 1 ms: their p90, the 180th in ascending order, is 180. */
const distinct = Array.from({ length: 200 }, (_, index) => 200 - index);

/** A log in which every read of the three forms took the times of `distinct`, interleaved. */
const fastLog = distinct.flatMap((ms) => forms.map((form) => accessLine(form, ms)));

/** Lines the gcloud filter may hand back that are no read of the probe's list. Each is over
 *  200 ms and answered 500, so counting any of them changes a verdict. */
const otherLines = [
  accessLine("page", 9000, { status: 500, route: "/api/v1/issues/{key}" }),
  accessLine("open", 9000, { status: 500, method: "POST" }),
  accessLine("whole", 9000, { status: 500, probe: `${fleetSha}/fedcba9876543210` }),
  'time=2026-11-25T09:00:00.000Z level=WARN msg="dispatch: request used the previous shared token" user_agent=priority-list-production/@PROBE@/page',
  "",
];

/** The regular expressions of the check's `run`, each line it greps the log for. */
const passingLines = [
  /^the open list: p90 [0-9]+ ms over 200 reads, at most 200 ms$/,
  /^the whole list: p90 [0-9]+ ms over 200 reads, at most 200 ms$/,
  /^the page of 50: p90 [0-9]+ ms over 200 reads, at most 200 ms$/,
];

/** The line that ends a probe, and the values it names. */
const probeLine = /^PROBE_ID=([0-9a-f]{40}\/[0-9a-f]{16}) PROBE_AT=(\S+) PROBE_STOPPED=(\S+)$/;
function probeOf(run: Look): { id: string; at: string; stopped: string } {
  const match = run.lines.map((line) => probeLine.exec(line)).find((found) => found !== null);
  if (match === undefined) throw new Error(`no PROBE_ID line.${said(run)}`);
  return { id: match[1] ?? "", at: match[2] ?? "", stopped: match[3] ?? "" };
}

/** The lines of the verdict a look reads from the log, one or two for each form. */
function verdict(run: Look): string[] {
  return run.lines.filter((line) => /^the (page of 50|open list|whole list): /.test(line));
}

/** The URL of each form's read of `largest-priority`. */
const urls: Readonly<Record<Form, string>> = {
  page: `${dispatchUrl}/api/v1/issues?advances=largest-priority&limit=50`,
  open: `${dispatchUrl}/api/v1/issues?advances=largest-priority&open=true`,
  whole: `${dispatchUrl}/api/v1/issues?advances=largest-priority`,
};

/** The reads of the issue list a look sent, by form, in order; a read of no form's URL by its URL. */
function readsOf(run: Look): string[] {
  return run.requests
    .filter(({ url }) => url.includes("/api/v1/issues?"))
    .map(({ url }) => forms.find((form) => urls[form] === url) ?? url);
}

/** The inputs of a look that has waited out the fleet's three hours. */
function settled(): Record<string, string> {
  return { FLEET_SINCE: stamp(Math.floor(Date.now() / 1000) - 4 * 3600), FLEET_COMMIT: fleetSha };
}

/** The inputs of a look that replays a probe of `fleetSha` made `ago` seconds before now. */
function replaying(ago: number, stopped = "none"): Record<string, string> {
  return {
    PROBE_ID: `${fleetSha}/${nonce}`,
    PROBE_AT: stamp(Math.floor(Date.now() / 1000) - ago),
    PROBE_STOPPED: stopped,
  };
}

const directories = temporaryDirectories("priority-list-production");

for (const [unset, reason] of [
  ["DISPATCH_URL", "required environment variable DISPATCH_URL is unset"],
  ["DISPATCH_TOKEN", "set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE to an agent bearer"],
  ["MERGE_SHA", "required environment variable MERGE_SHA is unset"],
  ["LOGGING_PROJECT", "required environment variable LOGGING_PROJECT is unset"],
] as const) {
  test(
    `a look with ${unset === "DISPATCH_TOKEN" ? "no bearer" : `${unset} unset`} cannot measure, and reads nothing`,
    async () => {
      const run = await look(directories(), `unset-${unset}`, {}, { [unset]: undefined });
      expect(
        {
          code: run.code,
          reason: run.stderr.includes(reason),
          reads: run.requests.length + run.gh.length + run.gcloud.length,
        },
        said(run)
      ).toEqual({ code: 2, reason: true, reads: 0 });
    },
    rowLimit("short")
  );
}

for (const [label, inputs, reason] of [
  ["MERGE_SHA of 7 digits", { MERGE_SHA: "4f1c2b9" }, "MERGE_SHA is 4f1c2b9"],
  ["PROBE_ID alone", { PROBE_ID: `${fleetSha}/${nonce}` }, "come together"],
  ["BEFORE_DEPLOY=true", { BEFORE_DEPLOY: "true" }, "only 1 sets it"],
  [
    "a FLEET_SINCE this script does not print",
    { FLEET_SINCE: "yesterday", FLEET_COMMIT: fleetSha },
    "FLEET_SINCE is yesterday",
  ],
] as const) {
  test(
    `a look with ${label} cannot measure, and reads no issue`,
    async () => {
      const run = await look(directories(), `malformed-${label}`, {}, inputs);
      expect(
        {
          code: run.code,
          reason: run.stderr.includes(reason),
          reads: readsOf(run).length + run.gcloud.length,
        },
        said(run)
      ).toEqual({ code: 2, reason: true, reads: 0 });
    },
    rowLimit("short")
  );
}

for (const [label, production, inputs, wanted] of [
  [
    "a fleet that does not carry the merge",
    { compare: "behind" },
    {},
    `production's fleet serves ${fleetSha}, which does not carry ${mergeSha} (GitHub's compare: behind)`,
  ],
  [
    "a fleet that serves no one commit",
    { fleetCommit: null },
    {},
    "production's fleet serves no one commit: GET /healthz answered HTTP 200 with fleet_commit null",
  ],
  [
    "a compare GitHub does not answer",
    { compare: "unanswered" },
    {},
    `GitHub did not answer whether ${fleetSha} carries ${mergeSha}: HTTP 502: Bad Gateway`,
  ],
  [
    "BEFORE_DEPLOY=1 at a fleet that carries the merge",
    { compare: "ahead" },
    { BEFORE_DEPLOY: "1" },
    `production's fleet serves ${fleetSha}, which carries ${mergeSha} (GitHub's compare: ahead), and BEFORE_DEPLOY=1 measures a fleet that does not`,
  ],
] as const) {
  test(
    `${label} is a look that waits, and it reads no issue`,
    async () => {
      const run = await look(directories(), `waits-${label}`, production, inputs);
      expect(
        {
          code: run.code,
          said: run.lines.some((line) => line.startsWith(wanted)),
          reads: readsOf(run).length + run.gcloud.length,
          compares: run.gh,
        },
        said(run)
      ).toEqual({
        code: 3,
        said: true,
        reads: 0,
        compares: production.fleetCommit === null ? [] : [fleetCompare],
      });
    },
    rowLimit("short")
  );
}

/** The line that ends a look at a fleet that carries the merge before its three hours have run. */
const fleetLine =
  /^production's fleet serves ([0-9a-f]{40}), which carries the merge: read again at or after (\S+) with FLEET_SINCE=(\S+) FLEET_COMMIT=([0-9a-f]{40})$/;

test(
  "the first look at a fleet that carries the merge names FLEET_SINCE, now, and waits three hours",
  async () => {
    const time = clock();
    const run = await look(directories(), "fleet-first", {});
    const to = time.to();
    const match = run.lines.map((line) => fleetLine.exec(line)).find((found) => found !== null);
    const since = secondsOf(match?.[3] ?? "");
    expect(
      {
        code: run.code,
        fleet: match?.[1],
        commit: match?.[4],
        sinceIsNow: since >= time.from && since <= to,
        wait: secondsOf(match?.[2] ?? "") - since,
        reads: readsOf(run).length,
      },
      said(run)
    ).toEqual({
      code: 3,
      fleet: fleetSha,
      commit: fleetSha,
      sinceIsNow: true,
      wait: 10800,
      reads: 0,
    });
  },
  rowLimit("short")
);

test(
  "a deploy during the wait starts it over for the new commit",
  async () => {
    const earlier = "1234567890abcdef1234567890abcdef12345678";
    const time = clock();
    const run = await look(
      directories(),
      "fleet-deployed",
      {},
      { FLEET_SINCE: stamp(time.from - 5 * 3600), FLEET_COMMIT: earlier }
    );
    const to = time.to();
    const match = run.lines.map((line) => fleetLine.exec(line)).find((found) => found !== null);
    const since = secondsOf(match?.[3] ?? "");
    expect(
      {
        code: run.code,
        commit: match?.[4],
        sinceIsNow: since >= time.from && since <= to,
        reads: readsOf(run).length,
      },
      said(run)
    ).toEqual({ code: 3, commit: fleetSha, sinceIsNow: true, reads: 0 });
  },
  rowLimit("short")
);

test(
  "a look before the three hours have run names the time they end, and the same FLEET_SINCE",
  async () => {
    const since = stamp(Math.floor(Date.now() / 1000) - 3600);
    const run = await look(
      directories(),
      "fleet-waiting",
      {},
      { FLEET_SINCE: since, FLEET_COMMIT: fleetSha }
    );
    expect({ code: run.code, lines: run.lines, reads: readsOf(run).length }, said(run)).toEqual({
      code: 3,
      lines: [
        `production's fleet serves ${fleetSha}, which carries the merge: read again at or after ${stamp(secondsOf(since) + 10800)} with FLEET_SINCE=${since} FLEET_COMMIT=${fleetSha}`,
      ],
      reads: 0,
    });
  },
  rowLimit("short")
);

test(
  "a probe whose lines are all fast prints the three lines the check greps for and exits 0, and its replay sends nothing and reads the same",
  async () => {
    const dir = directories();
    const time = clock();
    const run = await look(dir, "fast-probe", { log: [...otherLines, ...fastLog] }, settled());
    const to = time.to();
    const probe = probeOf(run);
    const at = secondsOf(probe.at);
    expect(
      {
        code: run.code,
        grepped: passingLines.map(
          (line) => run.lines.filter((printed) => line.test(printed)).length
        ),
        priority: run.lines.includes("priority largest-priority: 609 open issues"),
        client: run.lines.filter((line) => line.includes("by curl's clock")),
        verdict: verdict(run),
        probe: { commit: probe.id.slice(0, 40), stopped: probe.stopped },
        probeAtIsAMinuteBefore: at >= time.from - 60 && at <= to - 60,
        requests: run.requests,
        logReads: run.gcloud,
        sleeps: run.sleeps,
      },
      said(run)
    ).toEqual({
      code: 0,
      grepped: [1, 1, 1],
      priority: true,
      client: [
        "the page of 50 by curl's clock: p90 40 ms over 200 reads",
        "the open list by curl's clock: p90 40 ms over 200 reads",
        "the whole list by curl's clock: p90 40 ms over 200 reads",
      ],
      verdict: [
        "the page of 50: p90 180 ms over 200 reads, at most 200 ms",
        "the open list: p90 180 ms over 200 reads, at most 200 ms",
        "the whole list: p90 180 ms over 200 reads, at most 200 ms",
      ],
      probe: { commit: fleetSha, stopped: "none" },
      probeAtIsAMinuteBefore: true,
      requests: [
        { agent: "", url: `${dispatchUrl}/healthz` },
        { agent: "", url: `${dispatchUrl}/api/v1/priorities` },
        ...forms.flatMap((form) =>
          times(200, { agent: `priority-list-production/${probe.id}/${form}`, url: urls[form] })
        ),
      ],
      logReads: [
        [
          "logging",
          "read",
          [
            'resource.type="k8s_container"',
            'resource.labels.namespace_name="dispatch"',
            'textPayload:"msg=\\"dispatch: request\\""',
            `textPayload:"user_agent=priority-list-production/${probe.id}/"`,
            `timestamp>="${probe.at}"`,
            `timestamp<="${stamp(at + 7200)}"`,
          ].join(" AND "),
          "--project",
          "example-logging-project",
          "--format=value(textPayload)",
        ],
      ],
      sleeps: [],
    });

    const replay = await look(
      dir,
      "fast-replay",
      { sendsNothing: true, log: [...otherLines, ...fastLog] },
      { PROBE_ID: probe.id, PROBE_AT: probe.at, PROBE_STOPPED: probe.stopped }
    );
    expect(
      {
        code: replay.code,
        verdict: verdict(replay),
        requests: replay.requests.length,
        compares: replay.gh.at(-1),
      },
      said(replay)
    ).toEqual({
      code: 0,
      verdict: verdict(run),
      requests: run.requests.length,
      compares: fleetCompare,
    });
  },
  rowLimit("short", "short")
);

test(
  "each stand-in a look calls runs in the shell that calls it, and not in a process of its own",
  async () => {
    const dir = directories();
    const run = await look(
      dir,
      "own-shell",
      { slowForm: "page", lateReads: 1, log: formLines("page", times(21, 150)) },
      settled()
    );
    // Each line: the stand-in, its process ($BASHPID) and its subshell depth ($BASH_SUBSHELL).
    // The script calls gh inside `$(...)`, one subshell down, and the other three in its own
    // shell. A stand-in that runs as an executable, or in a subshell of its own, changes its depth
    // or its process.
    const shells = recorded(dir, "shells").map((line) => line.split(" "));
    const calls: Record<string, number> = {};
    for (const [name, , depth] of shells) {
      const call = `${name} at ${depth}`;
      calls[call] = (calls[call] ?? 0) + 1;
    }
    const scriptShell = shells.filter(([, , depth]) => depth === "0").map(([, pid]) => pid);
    expect(
      { code: run.code, calls, scriptProcesses: new Set(scriptShell).size },
      said(run)
    ).toEqual({
      code: 3,
      calls: { "curl at 0": 23, "gh at 1": 1, "gcloud at 0": 2, "sleep at 0": 1 },
      scriptProcesses: 1,
    });
  },
  rowLimit("short")
);

test(
  "the p90 of 200 reads is the 180th in order: 20 reads over 200 ms leave a form at most 200 ms, and 21 put it over",
  async () => {
    const run = await look(
      directories(),
      "p90-rank",
      {
        sendsNothing: true,
        log: [
          ...formLines("page", [...times(10, 120), ...times(20, 450), ...times(170, 120)]),
          ...formLines("open", [...times(21, 450), ...times(179, 120)]),
          ...formLines("whole", times(200, 100)),
        ],
      },
      replaying(900)
    );
    expect({ code: run.code, verdict: verdict(run), requests: run.requests }, said(run)).toEqual({
      code: 1,
      verdict: [
        "the page of 50: p90 120 ms over 200 reads, at most 200 ms",
        "the open list: p90 450 ms over 200 reads, over 200 ms",
        "the whole list: p90 100 ms over 200 reads, at most 200 ms",
      ],
      requests: [],
    });
  },
  rowLimit("short")
);

test(
  "a probe stopped by 21 slow reads whose server lines hold 21 over 200 ms fails that form",
  async () => {
    const run = await look(
      directories(),
      "stopped-slow-server",
      {
        slowForm: "open",
        log: [...formLines("page", times(200, 50)), ...formLines("open", times(21, 1300))],
      },
      settled()
    );
    expect(
      {
        code: run.code,
        client: run.lines.filter((line) => line.includes("by curl's clock")),
        verdict: verdict(run),
        stopped: probeOf(run).stopped,
        reads: readsOf(run),
      },
      said(run)
    ).toEqual({
      code: 1,
      client: [
        "the page of 50 by curl's clock: p90 40 ms over 200 reads",
        "the open list by curl's clock: p90 1500 ms over 21 reads",
        "the whole list by curl's clock: not read",
      ],
      verdict: [
        "the page of 50: p90 50 ms over 200 reads, at most 200 ms",
        "the open list: p90 1300 ms over 21 reads, over 200 ms",
        "the whole list: not read",
      ],
      stopped: "open:21",
      reads: [...times(200, "page"), ...times(21, "open")],
    });
  },
  rowLimit("short")
);

test(
  "a probe stopped by 21 slow reads whose server lines hold fewer over 200 ms is a look that waits for a new probe",
  async () => {
    const time = clock();
    const run = await look(
      directories(),
      "stopped-slow-network",
      {
        slowForm: "page",
        log: formLines("page", [...times(16, 150), ...times(5, 900)]),
      },
      settled()
    );
    const to = time.to();
    const last = /^read again at or after (\S+) with no PROBE_ID$/.exec(run.lines.at(-1) ?? "");
    const again = secondsOf(last?.[1] ?? "");
    expect(
      {
        code: run.code,
        verdict: verdict(run),
        stopped: probeOf(run).stopped,
        anHourOn: again >= time.from + 3600 && again <= to + 3600,
      },
      said(run)
    ).toEqual({
      code: 3,
      verdict: [
        "the page of 50: stopped after 21 reads, 5 of them over 200 ms on the server",
        "the open list: not read",
        "the whole list: not read",
      ],
      stopped: "page:21",
      anHourOn: true,
    });
  },
  rowLimit("short")
);

test(
  "the negative control: before the deploy, BEFORE_DEPLOY=1 probes the fleet that does not carry the head without the wait and fails the page of 50; after it, the replay sends nothing and reads the same",
  async () => {
    const dir = directories();
    const slowPage = formLines(
      "page",
      Array.from({ length: 21 }, (_, index) => 10000 - 50 * index)
    );
    const before = await look(
      dir,
      "control-before",
      { compare: "diverged", slowForm: "page", log: slowPage },
      { BEFORE_DEPLOY: "1" }
    );
    const probe = probeOf(before);
    expect(
      {
        code: before.code,
        verdict: verdict(before),
        stopped: probe.stopped,
        reads: readsOf(before).length,
        waited: before.lines.some((line) => fleetLine.test(line)),
      },
      said(before)
    ).toEqual({
      code: 1,
      verdict: [
        "the page of 50: p90 9900 ms over 21 reads, over 200 ms",
        "the open list: not read",
        "the whole list: not read",
      ],
      stopped: "page:21",
      reads: 21,
      waited: false,
    });

    const after = await look(
      dir,
      "control-after",
      { compare: "behind", sendsNothing: true, log: slowPage },
      { BEFORE_DEPLOY: "1", PROBE_ID: probe.id, PROBE_AT: probe.at, PROBE_STOPPED: probe.stopped }
    );
    expect(
      { code: after.code, verdict: verdict(after), requests: after.requests.length },
      said(after)
    ).toEqual({
      code: before.code,
      verdict: verdict(before),
      requests: before.requests.length,
    });
  },
  rowLimit("short", "short")
);

test(
  "a line whose status is not 200 fails its form",
  async () => {
    const log = [
      ...formLines("page", distinct),
      ...formLines("open", distinct),
      ...formLines("whole", distinct.slice(1)),
      accessLine("whole", 200, { status: 503 }),
    ];
    const run = await look(
      directories(),
      "status-503",
      { sendsNothing: true, log },
      replaying(900)
    );
    expect({ code: run.code, verdict: verdict(run), requests: run.requests }, said(run)).toEqual({
      code: 1,
      verdict: [
        "the page of 50: p90 180 ms over 200 reads, at most 200 ms",
        "the open list: p90 180 ms over 200 reads, at most 200 ms",
        "the whole list: p90 180 ms over 200 reads, at most 200 ms",
        "the whole list: 1 of its lines answered a status other than 200 (503)",
      ],
      requests: [],
    });
  },
  rowLimit("short")
);

for (const [label, compare, inputs] of [
  ["a commit that does not carry the merge", "behind", {}],
  ["a commit that carries the merge under BEFORE_DEPLOY=1", "ahead", { BEFORE_DEPLOY: "1" }],
] as const) {
  test(
    `a probe id of ${label} read another build: the look cannot measure, and reads nothing`,
    async () => {
      const run = await look(
        directories(),
        `another-build-${compare}`,
        { compare, sendsNothing: true, log: fastLog },
        { ...replaying(900), ...inputs }
      );
      expect(
        {
          code: run.code,
          reason: run.stderr.includes("it read another build"),
          compares: run.gh,
          reads: run.requests.length + run.gcloud.length,
        },
        said(run)
      ).toEqual({
        code: 2,
        reason: true,
        compares: [fleetCompare],
        reads: 0,
      });
    },
    rowLimit("short")
  );
}

test(
  "a first answer that is not 200 cannot be measured, and the probe stops there",
  async () => {
    const run = await look(
      directories(),
      "first-503",
      { issueStatus: 503, log: fastLog },
      settled()
    );
    expect(
      {
        code: run.code,
        reason:
          run.stderr.includes("the page of 50: the first answer of GET") &&
          run.stderr.includes("HTTP 503"),
        reads: readsOf(run),
        logReads: run.gcloud.length,
      },
      said(run)
    ).toEqual({ code: 2, reason: true, reads: ["page"], logReads: 0 });
  },
  rowLimit("short")
);

test(
  "a first answer that holds no issue fails, and the probe stops there",
  async () => {
    const run = await look(
      directories(),
      "first-empty",
      { noIssue: true, log: fastLog },
      settled()
    );
    expect(
      { code: run.code, last: run.lines.at(-1), reads: readsOf(run), logReads: run.gcloud.length },
      said(run)
    ).toEqual({
      code: 1,
      last: "the page of 50: the first answer of GET /api/v1/issues?advances=largest-priority&limit=50 holds no issue, so its time shows nothing",
      reads: ["page"],
      logReads: 0,
    });
  },
  rowLimit("short")
);

test(
  "a log that lacks lines within two hours of PROBE_AT is read every 30 s for 10 minutes, then the look waits naming the probe",
  async () => {
    const inputs = replaying(1800, "page:21");
    const time = clock();
    const run = await look(
      directories(),
      "lines-missing",
      { sendsNothing: true, log: formLines("page", times(20, 1500)) },
      inputs
    );
    const to = time.to();
    const last =
      /^read again at or after (\S+) with PROBE_ID=(\S+) PROBE_AT=(\S+) PROBE_STOPPED=(\S+)$/.exec(
        run.lines.at(-1) ?? ""
      );
    const again = secondsOf(last?.[1] ?? "");
    expect(
      {
        code: run.code,
        verdict: verdict(run),
        named: last?.slice(2),
        tenMinutesOn: again >= time.from + 600 && again <= to + 600,
        logReads: run.gcloud.length,
        sleeps: run.sleeps,
      },
      said(run)
    ).toEqual({
      code: 3,
      verdict: [
        "the page of 50: 20 of the 21 reads sent are in the log",
        "the open list: not read",
        "the whole list: not read",
      ],
      named: [inputs.PROBE_ID, inputs.PROBE_AT, "page:21"],
      tenMinutesOn: true,
      logReads: 21,
      sleeps: times(20, "30"),
    });
  },
  rowLimit("short")
);

test(
  "a log that lacks lines two hours after PROBE_AT has lost them, and the look waits for a new probe",
  async () => {
    const time = clock();
    const run = await look(
      directories(),
      "lines-lost",
      { sendsNothing: true, log: formLines("page", times(20, 1500)) },
      replaying(3 * 3600, "page:21")
    );
    const to = time.to();
    const last = /^read again at or after (\S+) with no PROBE_ID$/.exec(run.lines.at(-1) ?? "");
    const again = secondsOf(last?.[1] ?? "");
    expect(
      {
        code: run.code,
        lost: run.lines.includes(
          `the lines of probe ${fleetSha}/${nonce} the log lacks are lost: two hours have passed since PROBE_AT`
        ),
        anHourOn: again >= time.from + 3600 && again <= to + 3600,
      },
      said(run)
    ).toEqual({ code: 3, lost: true, anHourOn: true });
  },
  rowLimit("short")
);

test(
  "a log that holds no line of the probe after 10 minutes cannot be measured",
  async () => {
    const run = await look(
      directories(),
      "no-line",
      { sendsNothing: true, log: [] },
      replaying(900)
    );
    expect(
      {
        code: run.code,
        reason: run.stderr.includes(
          `no line of probe ${fleetSha}/${nonce} reached the log in 10 minutes`
        ),
        logReads: run.gcloud.length,
        sleeps: run.sleeps.length,
      },
      said(run)
    ).toEqual({ code: 2, reason: true, logReads: 21, sleeps: 20 });
  },
  rowLimit("short")
);

test(
  "a log whose lines come late is read again until it holds every line",
  async () => {
    const run = await look(
      directories(),
      "late-lines",
      { sendsNothing: true, log: fastLog, lateReads: 2 },
      replaying(900)
    );
    expect(
      {
        code: run.code,
        grepped: passingLines.map((line) => run.lines.some((printed) => line.test(printed))),
        logReads: run.gcloud.length,
        sleeps: run.sleeps,
      },
      said(run)
    ).toEqual({ code: 0, grepped: [true, true, true], logReads: 3, sleeps: ["30", "30"] });
  },
  rowLimit("short")
);

test(
  "exit 1 comes before exit 3: a form whose held lines already hold 21 over 200 ms fails while its other lines are missing and a stopped form owes a new probe",
  async () => {
    const run = await look(
      directories(),
      "fail-before-wait",
      {
        sendsNothing: true,
        log: [
          ...formLines("page", [...times(129, 100), ...times(21, 700)]),
          ...formLines("open", times(21, 100)),
        ],
      },
      replaying(900, "open:21")
    );
    expect({ code: run.code, verdict: verdict(run), last: run.lines.at(-1) }, said(run)).toEqual({
      code: 1,
      verdict: [
        "the page of 50: p90 700 ms over 150 reads, over 200 ms",
        "the open list: stopped after 21 reads, 0 of them over 200 ms on the server",
        "the whole list: not read",
      ],
      last: "the whole list: not read",
    });
  },
  rowLimit("short")
);
