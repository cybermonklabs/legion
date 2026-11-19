// The driver of verification-results.sh: it drives a real envoy-dispatch over HTTP as one session
// (the run's agent token, each write naming the session as `actor`) and one person (the dev
// sign-in cookie), seeds the run's fake GitHub, publishes each pull-request event as the
// normalized `pull_request` envelope on the run's own NATS, the one path Dispatch's delivery
// intake reads, and reads two of its answers back through the `dispatch results` command as well
// as through the API. Three more cases run the `dispatch` command as a process against the same
// server: a bare `-` given to a text flag is refused, on every text flag the command's help
// lists, and every other value of a text flag is kept byte for byte.
//
// The shell script runs the driver again for each one-pass phase (onePassPhases): with
// `--phase <phase>-fixture` while no server runs, and with `--phase <phase>-assert` after it
// starts the server again with a start pass and an interval of one hour. That is the boundary of
// one complete pass (prepareOnePass, awaitOnePass). Before its first case the driver checks its
// counter of pull request reads, and the phase `boundary` checks the boundary of one pass.
//
//   VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> \
//     VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> \
//     VERIFICATION_RESULTS_E2E_DISPATCH_LOG=<the server's log> \
//     VERIFICATION_RESULTS_E2E_STATE=<a JSON file> \
//     [VERIFICATION_RESULTS_E2E_DISPATCH_CLI=<bin/dispatch.ts>] \
//     bun scripts/e2e/verification-results.ts [--phase <phase>-fixture|<phase>-assert] \
//       <dispatch-url> <nats-url> <fake-github-url>
//
// The `dispatch` command is the bin/dispatch.ts VERIFICATION_RESULTS_E2E_DISPATCH_CLI names, an
// absolute path the shell script made, else this checkout's packages/envoy-client/bin/dispatch.ts.
// The log is the file every start of the server writes to, which attributionLines reads. The state
// file holds what one process of the driver leaves for a later one: the issues a case seeded, and
// the length of the log at each phase's fixture.
//
// The database URL is the run's own, which the shell script resets before the server migrates it.
// The driver writes SQL for the fixtures that no route of Dispatch can write: stored pull request
// rows as the release before writes them, with no texts, and the delete of the reconcile's records
// of its passes at each phase's boundary. It reads SQL for the answers that no route gives: the
// issue and the source a row records, whether its texts are stored, the records of the passes and
// whether a process holds the reconcile's lease.
//
// Each case prints `== <case>`, what it read, then `ok <case>` or `FAIL <case>: <why>`; a summary
// follows, and the exit status is 1 when any case failed. The first case,
// `the-results-route-is-served`, is the run's first result write, against a plain issue whose
// spec marks nothing: when it fails, as it does against a binary built from the base commit,
// which serves no such route, the run stops there, since every later case records a result too.
// Each later case seeds the issue it drives, with the spec and the Verification document that
// case needs. A step outside the cases that fails (the sign-in, the projects, the fake GitHub's
// seed, the probe issue, the counter of reads, a phase's fixture, the boundary of one pass) exits
// 2 naming it.
//
// The run writes no delivery settings, as a deployment that never configured the delivery timeline
// holds none: the intake writes a pull request's head row from its envelope all the same, and
// every head below reaches Dispatch that way.
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type {
  CheckBasis,
  CheckBlock,
  CheckResult,
  CheckResultRecorded,
  HeadNotRead,
  IssueSummaryPage,
} from "@legion/contracts";
import { connect, StringCodec } from "nats";
// By path: the root package does not depend on @legion/contracts, so bun resolves no value from
// it here. The type import above is erased before the driver runs.
import { MAX_ISSUE_PAGE_LIMIT } from "../../packages/contracts/src/dispatch-api-limits";

/** The arguments: `--phase <name>-fixture` or `--phase <name>-assert` where the shell runs one half
 *  of a one-pass phase, and the three URLs. */
const args = process.argv.slice(2);
const phaseAt = args.indexOf("--phase");
const phaseArg = phaseAt === -1 ? undefined : args.splice(phaseAt, 2)[1];
const [dispatchUrl, natsUrl, githubUrl] = args;
const agentToken = process.env.VERIFICATION_RESULTS_E2E_AGENT_TOKEN ?? "";
const databaseUrl = process.env.VERIFICATION_RESULTS_E2E_DATABASE_URL ?? "";
const dispatchLog = process.env.VERIFICATION_RESULTS_E2E_DISPATCH_LOG ?? "";
const statePath = process.env.VERIFICATION_RESULTS_E2E_STATE ?? "";
if (
  dispatchUrl === undefined ||
  natsUrl === undefined ||
  githubUrl === undefined ||
  !agentToken ||
  !databaseUrl ||
  !dispatchLog ||
  !statePath ||
  (phaseAt !== -1 && phaseArg === undefined)
) {
  process.stderr.write(
    "usage: VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> VERIFICATION_RESULTS_E2E_DISPATCH_LOG=<log> VERIFICATION_RESULTS_E2E_STATE=<file> bun verification-results.ts [--phase <phase>-fixture|<phase>-assert] <dispatch-url> <nats-url> <fake-github-url>\n"
  );
  process.exit(2);
}
const origin = new URL(dispatchUrl).origin;

/** How long after a pull request's event, measured from its publish, the results route may take
 *  to compare a check with the head that event reports: v-head-no-settings says "within 5
 *  seconds of the event". The intake binds its durable at
 *  start, takes the envelope, reads the pull request from GitHub and writes its head row;
 *  nothing else is waited on, since every other answer is computed while the results are read. */
const headAfterEventMs = 5_000;
/** The interval the shell starts the server with for a one-pass phase
 *  (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=1h), in the nanoseconds duty_passes stores. */
const onePassPeriodNs = String(3_600_000_000_000);
/** How long a phase's start pass may take to end: it reads GitHub for each row it takes. */
const onePassWaitMs = 120_000;
const pollMs = 250;

const person = "alice";
const session = "verification-results-e2e";
/** The repository the proof's issues resolve to, and the one the App is not installed on. */
const repo = "acme/verification-live";
const hiddenRepo = "acme/verification-hidden";

/** The commits the proof's repository holds, which is every commit a case records against it:
 *  the record route asks GitHub whether the repository the check's issue resolves to holds a
 *  result's commit and refuses one it does not, and the seed below gives that repository these
 *  and nothing else. A result's commit is 40 lowercase hex characters, which is all Dispatch
 *  reads of it. */
const heldCommits = {
  /** The result read back under its check. */
  a2: "1".repeat(40),
  /** The pull request's first head. */
  headOne: "2".repeat(40),
  /** The pull request's second head, which ages the result at the first. */
  headTwo: "3".repeat(40),
  /** The after-merge result the closed issue takes. */
  closed: "4".repeat(40),
  /** One of the two commits the default-branch check records at, the one the compare calls
   *  behind. The branch holds it as well, and it is named here so the fixture says what the case
   *  needs rather than leaning on the compare's seed. */
  behind: "5".repeat(40),
  /** The other of those two, the one the compare calls diverged. */
  diverged: "6".repeat(40),
  /** The head of the stranger's pull request. */
  fork: "9".repeat(40),
  /** The bases case's first open pull request's head, which the quoted check calls A. */
  comparedHeadA: "a1".repeat(20),
  /** Its second open pull request's head, A2: the head of another pull request of the same
   *  issue, which is what a run naming one head alone would miss. */
  comparedHeadA2: "a2".repeat(20),
  /** The head the event moves the first pull request to, A3. */
  comparedHeadA3: "a3".repeat(20),
  /** The head of that issue's merged pull request, whose base branch the fake holds no
   *  commits for. */
  comparedMergedHead: "a4".repeat(20),
  /** The commit GitHub made when that pull request merged, which the merge itself proves the
   *  base branch held. */
  comparedMergeCommit: "a5".repeat(20),
  /** The commit the default branch carries, which the after-merge result and the
   *  default-branch result are both recorded at. */
  comparedOnMain: "a6".repeat(20),
  /** The commit the third before-merge result is at, which heads no pull request. */
  comparedApart: "a7".repeat(20),
  /** The commit the bases case names to the command and to the API, which no result is at. */
  comparedNamed: "a8".repeat(20),
  /** The head of the attribution case's first pull request, the one its branch credits to the
   *  child. */
  attributionBranch: "c1".repeat(20),
  /** The head that first pull request moves to, which moves no attribution with it. */
  attributionBranchMoved: "c2".repeat(20),
  /** The head of the pull request its body credits. */
  attributionBody: "c3".repeat(20),
  /** The head of the pull request a person's link credits, against its branch. */
  attributionLinkOverBranch: "c4".repeat(20),
  /** The head of the pull request a person's link credits, against its title. */
  attributionLinkOverTitle: "c5".repeat(20),
  /** The head of the stored open row the reconcile re-judges. */
  attributionStoredOpen: "c6".repeat(20),
  /** The head of the stored merged row it re-judges with it. */
  attributionStoredMerged: "c7".repeat(20),
  /** The commit the bare-`-` case records its results at: the result the refusal leads to, and
   *  under a command built before the refusal, the result of the reported call, which would
   *  otherwise be refused for its commit rather than stored with `-` in it. */
  dashRefused: "d1".repeat(20),
  /** The commit the case of the kept values records its three results at. */
  valuesKept: "d2".repeat(20),
};

/** Every commit the cases name: the ones that repository holds, and three it does not.
 *  `mainHead` is nobody's result, only the end of the default branch, which the branch below
 *  holds; `unheld` is the commit the seed leaves out, so that GitHub's answer that the
 *  repository holds no such commit is what the refusal case drives; and `hidden` is recorded
 *  against the repository the App is not installed on, where the installation's 404 stops
 *  Dispatch before it can ask about a commit at all. */
const commits = {
  ...heldCommits,
  mainHead: "7".repeat(40),
  hidden: "8".repeat(40),
  unheld: "a".repeat(40),
};

/** The two rows the phase k-attribution stores as the release before writes them, credited to the
 *  attribution case's parent with no texts, and the state GitHub answers for each. */
const storedAttributionRows = [
  { head: commits.attributionStoredOpen, merged: false, number: 45, state: "open" },
  { head: commits.attributionStoredMerged, merged: true, number: 46, state: "merged" },
] as const;

type Json = Record<string, unknown>;

const codec = StringCodec();
const nats = await connect({ name: "verification-results", servers: natsUrl });

async function call(
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: Json
): Promise<unknown> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`);
  return text === "" ? undefined : JSON.parse(text);
}

/** The session's request: the shared agent token, and on a write the session as its `actor`. */
function asSession(method: string, path: string, body?: Json): Promise<unknown> {
  const headers = { Authorization: `Bearer ${agentToken}` };
  const payload =
    body === undefined ? undefined : { ...body, actor: { id: session, kind: "session" } };
  return call(method, path, headers, payload);
}

/** A write the server refuses, as the case that asserts the refusal reads it: `answer` is its
 *  status and its code, and `error` the words it carries, which a case may read for the commit
 *  and the repository they name. */
interface Refusal {
  readonly answer: string;
  readonly error: string;
}

async function refusal(method: string, path: string, body: Json): Promise<Refusal> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: JSON.stringify({ ...body, actor: { id: session, kind: "session" } }),
    headers: { Authorization: `Bearer ${agentToken}`, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (response.ok) return { answer: `${response.status} (the write was taken)`, error: text };
  let code = "";
  let error = text;
  try {
    const parsed = JSON.parse(text) as Json;
    code = String(parsed.code ?? "");
    error = String(parsed.error ?? text);
  } catch {
    code = text;
  }
  return { answer: `${response.status} ${code}`, error };
}

async function signIn(login: string): Promise<string> {
  const path = `/auth/_dev/signin?login=${encodeURIComponent(login)}`;
  const response = await fetch(new URL(path, dispatchUrl), { redirect: "manual" });
  const cookie = response.headers
    .getSetCookie()
    .find((header) => header.startsWith("dsession="))
    ?.split(";")[0];
  if (response.status !== 302 || cookie === undefined) {
    throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  }
  return cookie;
}

function setupFailed(step: string, error: unknown): never {
  process.stderr.write(`setup ${step} failed: ${error instanceof Error ? error.message : error}\n`);
  process.exit(2);
}

/** The person's session cookie, signed in at the first request that needs one: a phase's fixture
 *  runs while no server runs, and a server that starts again signs its cookies with a new key. */
let personCookie: Promise<string> | undefined;

/** The person's request: the session cookie, and the dashboard origin every cookie write needs. */
async function asPerson(method: string, path: string, body?: Json): Promise<unknown> {
  personCookie ??= signIn(person).catch((error: unknown) => setupFailed("sign-in", error));
  return call(method, path, { Cookie: await personCookie, Origin: origin }, body);
}

function issuePath(key: string, rest = ""): string {
  return `/api/v1/issues/${encodeURIComponent(key)}${rest}`;
}

/** The checks of an issue's Verification document, each with the commits and branches its results
 *  are compared with, whether a pass of it stands, and every result recorded against it, as
 *  `GET /api/v1/issues/{key}/results` answers. `at` names one commit (`?at=`), which becomes the
 *  one basis of every check of the issue. */
async function checksOf(key: string, at?: string): Promise<readonly CheckBlock[]> {
  const query = at === undefined ? "" : `?at=${encodeURIComponent(at)}`;
  const body = (await asSession("GET", issuePath(key, `/results${query}`))) as {
    checks: CheckBlock[];
  };
  return body.checks;
}

/** One check of an issue, by its block id. */
async function checkOf(key: string, blockID: string): Promise<CheckBlock> {
  const found = (await checksOf(key)).find((check) => check.block_id === blockID);
  if (found === undefined) throw new Error(`${key} has no check ${blockID}`);
  return found;
}

/** Where one result's commit stands, in one word a case compares with: the `commit_stands`
 *  answer, with the reason an `unknown` carries after a colon. */
function standsOf(result: CheckResult): string {
  const stands = result.commit_stands;
  if (stands === undefined) return "unset";
  const reason = stands.reason ?? "";
  return reason === "" ? stands.answer : `${stands.answer}:${reason}`;
}

/** Where the commit of the newest result recorded at `commit` against `blockID` stands, or
 *  "absent" where that check holds no result at that commit. */
async function commitStandsAt(key: string, blockID: string, commit: string): Promise<string> {
  const result = (await checkOf(key, blockID)).results.find((row) => row.commit_sha === commit);
  return result === undefined ? "absent" : standsOf(result);
}

/** Whether a pass of `blockID` stands, in one word with the roles it names: the `pass_stands`
 *  answer the gate reads. */
async function passStandsOf(key: string, blockID: string): Promise<string> {
  const pass = (await checkOf(key, blockID)).pass_stands;
  const roles = pass.roles ?? [];
  return roles.length === 0 ? pass.answer : `${pass.answer} (${roles.join(", ")})`;
}

/** Waits, until the deadline `by` (milliseconds since the epoch), for the commit of the result at
 *  `commit` to stand where `want` says, and answers what it last read. Where a commit stands is
 *  computed while the results are read, so this writes nothing and polls the read. The answer it
 *  accepts is held to the deadline too: a read that answers `want` after `by` answers that it was
 *  late, so a route that answers right but late meets no bound. */
async function awaitCommitStands(
  key: string,
  blockID: string,
  commit: string,
  want: string,
  by: number
): Promise<string> {
  let seen = await commitStandsAt(key, blockID, commit);
  while (seen !== want && Date.now() < by) {
    await Bun.sleep(pollMs);
    seen = await commitStandsAt(key, blockID, commit);
  }
  const late = Date.now() - by;
  return seen === want && late > 0 ? `${seen}, read ${late} ms past the deadline` : seen;
}

/** Records one result through `POST /api/v1/issues/{key}/results`, and answers what Dispatch
 *  says about it: the stored row and the line the pull-request body carries. */
async function record(key: string, input: Json): Promise<CheckResultRecorded> {
  return (await asSession("POST", issuePath(key, "/results"), {
    role: "tester",
    verdict: "pass",
    ...input,
  })) as CheckResultRecorded;
}

/** One envelope the driver published: its subject, and the moment it was published, which the
 *  5-second bound of v-head-no-settings is measured from (afterEvent). */
interface Published {
  subject: string;
  at: number;
}

/** The deadline of an answer the event `published` decides: headAfterEventMs after its publish. */
function afterEvent(published: Published): number {
  return published.at + headAfterEventMs;
}

/** Publishes one normalized `pull_request` envelope, the shape Envoy relays and the shape
 *  Dispatch's intake reads: the envelope's `payload` is the flat string map, and its subject is
 *  `notifications.github.<owner>.<repo>.pr.<number>` with each segment's dots written `_`.
 *  The intake takes from it only which pull request changed and reads that pull request from
 *  GitHub, so the fake's answer is set first to what GitHub holds after the event. */
async function publishPullRequest(payload: Record<string, string>): Promise<Published> {
  await seedRepositories({
    [payload.number]: {
      base_ref: payload.base_ref,
      body: payload.body,
      head_ref: payload.head_ref,
      head_repo: payload.head_repo,
      head_sha: payload.head_sha,
      merge_commit_sha: payload.merge_commit_sha,
      merged: payload.merged === "true",
      state: payload.action === "closed" ? "closed" : "open",
      title: payload.title,
      updated_at: payload.updated_at,
    },
  });
  const [owner, name] = repo.split("/");
  const subject = `notifications.github.${String(owner).replaceAll(".", "_")}.${String(name).replaceAll(".", "_")}.pr.${payload.number}`;
  const eventID = `verification-results-pr-${payload.number}-${payload.action}-${payload.head_sha.slice(0, 7)}`;
  const at = Date.now();
  nats.publish(
    subject,
    codec.encode(
      JSON.stringify({
        dedupe_key: eventID,
        event_id: eventID,
        issued_at: at,
        payload: JSON.stringify(payload),
        payload_summary: subject,
        source: "github",
        source_event_id: eventID,
        topic: subject,
        trace_id: eventID,
      })
    )
  );
  await nats.flush();
  return { at, subject };
}

/** One basis of a check, in the words a case compares with: the kind, the commit or the branch
 *  it names, the pull request it is the head of, and when Dispatch learned it with the kind of
 *  time that is (`read_at` is Dispatch's own read, `updated_at` is GitHub's update time as an
 *  event carried it). */
function basisOf(basis: CheckBasis): string {
  const learned =
    basis.learned_at === undefined ? "" : ` (${basis.learned_at.kind} ${basis.learned_at.at})`;
  if (basis.kind === "branch") return `branch ${basis.branch ?? "unnamed"}${learned}`;
  const number = basis.pull_request === undefined ? "" : ` of pull request #${basis.pull_request}`;
  return `${basis.kind} ${(basis.commit ?? "").slice(0, 12)}${number}${learned}`;
}

/** One pull request whose head Dispatch has not read, in the words a case compares with: its
 *  number, and the word and the time of GitHub's refusal of its read where GitHub refuses it. */
function headNotReadOf(head: HeadNotRead): string {
  if (head.refused_as === undefined) return `#${head.number}`;
  const since = head.refused_since === undefined ? "" : ` ${head.refused_since}`;
  return `#${head.number} (${head.refused_as}${since})`;
}

/** What one check compares its results with, as a case prints and compares it: every basis in
 *  the order the answer carries them, or the reason the check compared with nothing. Then each
 *  pull request whose head Dispatch has not read, and each one whose answer is not known. */
function comparedOf(check: CheckBlock): string {
  const bases = check.compared_with ?? [];
  const compared =
    bases.length > 0
      ? bases.map(basisOf).join(", ")
      : `nothing: ${check.compared_nothing ?? "no reason"}`;
  const headsNotRead = check.heads_not_read ?? [];
  const issuesNotKnown = check.issues_not_known ?? [];
  return [
    compared,
    ...(headsNotRead.length === 0
      ? []
      : [`heads_not_read ${headsNotRead.map(headNotReadOf).join(", ")}`]),
    ...(issuesNotKnown.length === 0
      ? []
      : [`issues_not_known ${issuesNotKnown.map((entry) => `#${entry.number}`).join(", ")}`]),
  ].join("; ");
}

/** The input that names the `dispatch` command to run, empty when it is unset. The shell script
 *  makes it an absolute path, since one case runs the command in a directory of its own. */
const dispatchCommandInput = process.env.VERIFICATION_RESULTS_E2E_DISPATCH_CLI ?? "";

/** The `dispatch` command the cases run: the bin/dispatch.ts the input names, else this
 *  checkout's (packages/envoy-client/bin/dispatch.ts). The bases case reads two of its answers
 *  through it, beside the API the rest of the proof reads, and the cases of a bare `-` drive it
 *  as a process. A negative control names another build's here, with no change to the checkout. */
const dispatchCommand =
  dispatchCommandInput === ""
    ? join(import.meta.dir, "../../packages/envoy-client/bin/dispatch.ts")
    : dispatchCommandInput;

/** What one run of the `dispatch` command answered: its exit code and what it printed. */
interface DispatchRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** Runs the `dispatch` command with `argv` against the run's own server, as the run's session,
 *  and answers its exit code and what it printed, whatever the code. The command is given a home
 *  of its own and the variables that name its host, its session and the server, so no
 *  envoy.json, token file or session state of the caller's is read. Its standard input holds
 *  `stdin`, or nothing when that is absent, and it runs in `cwd`, or in the driver's working
 *  directory when that is absent. */
async function runDispatch(
  argv: readonly string[],
  { stdin, cwd }: { readonly stdin?: string; readonly cwd?: string } = {}
): Promise<DispatchRun> {
  const home = mkdtempSync(join(tmpdir(), "verification-results-cli-"));
  try {
    const child = Bun.spawn([process.execPath, dispatchCommand, ...argv], {
      cwd,
      env: {
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: session,
        DISPATCH_TOKEN: agentToken,
        DISPATCH_URL: dispatchUrl,
        HOME: home,
        PATH: process.env.PATH ?? "",
      },
      stderr: "pipe",
      stdin: stdin === undefined ? "ignore" : new Blob([stdin]),
      stdout: "pipe",
    });
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    return { code, stderr, stdout };
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
}

/** One word of a command line as a shell reads it back: bare when it holds no character a shell
 *  reads, else in single quotes. */
function shellWord(word: string): string {
  return /^[A-Za-z0-9_./:@%+=,-]+$/.test(word) ? word : `'${word.replaceAll("'", "'\\''")}'`;
}

/** The command line `dispatch` with `argv`, each word as `shellWord` writes it. */
function commandText(argv: readonly string[]): string {
  return `dispatch ${argv.map(shellWord).join(" ")}`;
}

/** The failure of one step of a case that runs the `dispatch` command: the step's number, its
 *  command line, the exit code it got and the one it wanted, what else went wrong when `why`
 *  names it, then its stdout and its stderr as JSON strings, so every byte of them reads in the
 *  FAIL line. */
function stepFailed(
  step: number,
  argv: readonly string[],
  run: DispatchRun,
  wanted: number,
  why?: string
): string {
  return [
    `step ${step}: ${commandText(argv)} exited ${run.code}, want ${wanted}${why === undefined ? "" : `: ${why}`}`,
    `stdout ${JSON.stringify(run.stdout)}`,
    `stderr ${JSON.stringify(run.stderr)}`,
  ].join("\n   ");
}

/** The argv of `dispatch result` recording an implementer's pass of `check` on `issue` at
 *  `commit`, which a step extends with the fields it sends. */
function resultArgv(issue: string, check: string, commit: string): string[] {
  return [
    "result",
    "--issue",
    issue,
    "--check",
    check,
    "--role",
    "implementer",
    "--verdict",
    "pass",
    "--commit",
    commit,
  ];
}

/** Runs `dispatch results --issue <key> [--at <commit>]` and answers what it printed. */
async function resultsCommand(key: string, at?: string): Promise<string> {
  const run = await runDispatch([
    "results",
    "--issue",
    key,
    ...(at === undefined ? [] : ["--at", at]),
  ]);
  if (run.code !== 0) {
    throw new Error(`dispatch results --issue ${key} exited ${run.code}: ${run.stderr.trim()}`);
  }
  return run.stdout;
}

/** The head line the command prints for each check: `- <block id> (<when>). Compared with …`,
 *  which is where the command names the bases. */
function commandHeadLines(printed: string): string[] {
  return printed.split("\n").filter((line) => line.startsWith("- "));
}

/** Runs one statement against the run's own database and answers what psql printed, rows bare
 *  and columns separated by `|` (`-tA`), for the fixtures and the answers no route of Dispatch
 *  carries (the header lists them). */
async function sql(statement: string): Promise<string> {
  const child = Bun.spawn(
    ["psql", databaseUrl, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-tA", "-c", statement],
    { stderr: "pipe", stdout: "pipe" }
  );
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (code !== 0) throw new Error(`psql exited ${code}: ${stderr.trim()}`);
  return stdout.trim();
}

/** Whether the database holds pull_request_texts, the table of the texts a read of GitHub stores:
 *  the build of a commit before it, which a negative control runs, has none. */
async function textsTableExists(): Promise<boolean> {
  return (await sql("select to_regclass('pull_request_texts') is not null")) === "t";
}

/** One stored pull request row, as pull_request_heads holds it: the issue it records and the
 *  source of that issue, which a pass of the reconcile writes from the statement's answer
 *  (SyncAttributions), its state and head commit, which a read of GitHub writes, and whether its
 *  texts are stored. "absent" where no row is stored yet. */
async function storedHead(number: number): Promise<string> {
  const texts = (await textsTableExists())
    ? "exists (select 1 from pull_request_texts t where t.repo = h.repo and t.number = h.number)"
    : "null";
  const row = await sql(
    `select coalesce(h.issue_key, 'nobody'), coalesce(h.attribution_source, 'no source'), h.state,
       h.head_sha, ${texts}
     from pull_request_heads h where h.repo = '${repo}' and h.number = ${number}`
  );
  if (row === "") return "absent";
  const [issue, source, state, head, stored] = row.split("|");
  const shown = { f: "no texts", t: "texts stored" }[stored ?? ""] ?? "no texts table";
  return `${issue} by ${source} (${state} at ${String(head).slice(0, 12)}, ${shown})`;
}

/** Waits, until the deadline `by`, for the stored row of `number` to hold the facts `facts` (the
 *  part of storedHead in parentheses), and answers what it last read. The intake writes the facts
 *  of an event at once, and a pass writes the issue later, so this waits on the facts alone. It
 *  polls and writes nothing. The answer it accepts is held to the deadline too, as
 *  awaitCommitStands holds its own: a row that reads `facts` after `by` answers that it was late,
 *  so a writer that writes right but late meets no bound. */
async function awaitStoredHead(number: number, facts: string, by: number): Promise<string> {
  const holds = (seen: string) => seen.endsWith(`(${facts})`);
  let seen = await storedHead(number);
  while (!holds(seen) && Date.now() < by) {
    await Bun.sleep(pollMs);
    seen = await storedHead(number);
  }
  const late = Date.now() - by;
  return holds(seen) && late > 0 ? `${seen}, read ${late} ms past the deadline` : seen;
}

/** Waits, until the deadline `by`, for the check `blockID` of `key` to compare with `commit`, and
 *  answers the bases it last read, and whether they hold `commit` by the deadline. The statement
 *  gives a pull request's issue at the read, so the reader answers before any pass writes that
 *  issue on the row. */
async function awaitComparedWith(
  key: string,
  blockID: string,
  commit: string,
  by: number
): Promise<{ readonly held: boolean; readonly shown: string }> {
  const holds = (check: CheckBlock) =>
    (check.compared_with ?? []).some((basis) => basis.commit === commit);
  let check = await checkOf(key, blockID);
  while (!holds(check) && Date.now() < by) {
    await Bun.sleep(pollMs);
    check = await checkOf(key, blockID);
  }
  const late = Date.now() - by;
  const shown = late > 0 ? `${comparedOf(check)}, read ${late} ms late` : comparedOf(check);
  return { held: holds(check) && late <= 0, shown };
}

/** Every `issue.pull_request_attribution_changed` event on one issue, oldest first, in the
 *  words a case compares with: the pull request, the issue it left, the issue it reached and
 *  the source that decided the new one. A pass of the reconcile appends them. */
async function attributionMoves(key: string): Promise<string[]> {
  const events = (await asSession("GET", issuePath(key, "/events?limit=200"))) as {
    type: string;
    payload: { number: number; from: string | null; to: string | null; source: string };
  }[];
  return events
    .filter((event) => event.type === "issue.pull_request_attribution_changed")
    .map(
      (event) =>
        `#${event.payload.number} ${event.payload.from ?? "nobody"} → ${event.payload.to ?? "nobody"} by ${event.payload.source}`
    );
}

function note(line: string): void {
  console.log(`   ${line}`);
}

const results: { name: string; ok: boolean }[] = [];

async function runCase(name: string, body: () => Promise<string | undefined>): Promise<void> {
  console.log(`== ${name}`);
  let failure: string | undefined;
  try {
    failure = await body();
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
  }
  results.push({ name, ok: failure === undefined });
  console.log(failure === undefined ? `ok ${name}` : `FAIL ${name}: ${failure}`);
}

function summarise(stopped?: string): never {
  const failed = results.filter((result) => !result.ok).map((result) => result.name);
  if (stopped !== undefined) console.log(`   ${stopped}`);
  console.log(
    `summary: ${results.length - failed.length} ok, ${failed.length} FAIL${failed.length === 0 ? "" : ` (${failed.join(", ")})`}`
  );
  void nats.close();
  process.exit(failed.length === 0 ? 0 : 1);
}

// --- the tooling of one complete pass ----------------------------------------------------------
//
// A check that says "after one pass" reads the answers at this boundary, in five steps:
//   1. The driver writes the fixture while no server runs (`--phase <phase>-fixture`).
//   2. It deletes the reconcile's records of its passes, and reads the database's clock
//      (prepareOnePass).
//   3. The shell starts the server with a pass at its start and an interval of one hour, so that
//      start pass is the one pass of the phase.
//   4. The pass ends when a record of the lease appears (awaitOnePass). Step 2 deleted every
//      record, so the first one is the start pass's. duty.Exclusive records a pass after the pass
//      runs, and gives the lease back after the record.
//   5. The driver asserts (`--phase <phase>-assert`). The next tick is an hour away.
// A record's boundary is the aligned start of its hour, not the end of the pass, so the driver
// does not compare it with the clock that it read in step 2.

/** What one process of the driver leaves for a later one in the state file, by key. */
function readState(): Record<string, unknown> {
  try {
    return JSON.parse(readFileSync(statePath, "utf8")) as Record<string, unknown>;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") return {};
    throw error;
  }
}

/** Every read of pull requests the fake GitHub has answered since resetPullReads, summed over its
 *  repositories and its routes: the read of one pull request, the read of its commits, the read of
 *  the pull requests of a commit, and each pull request that a GraphQL query asks for. */
async function pullReads(): Promise<number> {
  const response = await fetch(new URL("/__fixture/requests", githubUrl));
  if (!response.ok) {
    throw new Error(`GET /__fixture/requests: ${response.status} ${await response.text()}`);
  }
  const { pulls } = (await response.json()) as { pulls: Record<string, Record<string, number>> };
  return Object.values(pulls)
    .flatMap((routes) => Object.values(routes))
    .reduce((sum, reads) => sum + reads, 0);
}

/** Empties the fake GitHub's counts of reads: the reads of pull requests, and of check runs. */
async function resetPullReads(): Promise<void> {
  const response = await fetch(new URL("/__fixture/requests", githubUrl), { method: "DELETE" });
  if (!response.ok) {
    throw new Error(`DELETE /__fixture/requests: ${response.status} ${await response.text()}`);
  }
}

/** The lease of the delivery reconcile (delivery.ReconcileLease), and the rows of duty_passes
 *  that record its passes: one by the lease's name, which the release before reads, and one for
 *  each period, `<name>/<period_ns>` (duty.passKey). */
const reconcileLease = "delivery-reconcile";
const reconcileRecords = `lease = '${reconcileLease}' or lease like '${reconcileLease}/%'`;

/** Step 2 of one complete pass: deletes every record of the reconcile's passes, and answers the
 *  database's clock at that moment. */
async function prepareOnePass(): Promise<string> {
  return sql(
    `with gone as (delete from duty_passes where ${reconcileRecords} returning 1)
     select clock_timestamp()`
  );
}

/** Step 4 of one complete pass, and its self-check: waits, bounded by onePassWaitMs, for the
 *  record of the start pass, and then for the lease to be free. Then it holds the pass to the
 *  boundary. duty.Exclusive's recordPass writes two records of one pass, by the lease's name and
 *  by its period: both must be there, at one boundary, at the period of one hour. The lease must
 *  be free, and the log must hold one more attribution line than at the fixture (linesBefore).
 *  Every phase's answer rests on this boundary, so any other reading stops the driver
 *  (setupFailed). It answers what it read. */
async function awaitOnePass(linesBefore: number): Promise<string> {
  const deadline = Date.now() + onePassWaitMs;
  const records = () =>
    sql(
      `select lease, period_ns, boundary from duty_passes where ${reconcileRecords}
       order by lease collate "C"`
    );
  let recorded = await records();
  while (recorded === "" && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    recorded = await records();
  }
  // duty.leaseHeld: the lease is a session's two-key advisory lock, the namespace "LOOP"
  // (duty.leaseNamespace) and the hashtext of the lease's name.
  const held = async () =>
    (await sql(
      `select exists (select 1 from pg_locks
         where locktype = 'advisory' and granted and objsubid = 2
           and database = (select oid from pg_database where datname = current_database())
           and classid = ${0x4c4f4f50}::oid and objid = hashtext('${reconcileLease}')::oid)`
    )) === "t";
  let leaseHeld = await held();
  while (leaseHeld && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    leaseHeld = await held();
  }
  const rows = recorded === "" ? [] : recorded.split("\n").map((row) => row.split("|"));
  const shown =
    rows.map(([lease, period, at]) => `${lease} (period ${period} ns, at ${at})`).join(", ") ||
    "no record";
  const kept = rows.map(([lease, period]) => `${lease} ${period}`).join(", ");
  const wanted = `${reconcileLease} ${onePassPeriodNs}, ${reconcileLease}/${onePassPeriodNs} ${onePassPeriodNs}`;
  const lines = attributionLines().length - linesBefore;
  if (
    kept !== wanted ||
    new Set(rows.map((row) => row[2])).size !== 1 ||
    leaseHeld ||
    lines !== 1
  ) {
    setupFailed(
      "the boundary of one pass",
      `duty_passes holds ${shown}, want ${reconcileLease} and ${reconcileLease}/${onePassPeriodNs} at one boundary, each of period ${onePassPeriodNs} ns; the lease is ${leaseHeld ? "still held" : "free"}; the log holds ${lines} more attribution line(s) than at the fixture, want 1`
    );
  }
  return `one pass: duty_passes holds ${shown}; the lease is free; the log holds 1 more attribution line`;
}

/** Each line `dispatch delivery: pull request attribution` of the server's log, oldest first, as
 *  its attributes. A pass writes one, with its counts (facts_left_before, facts_left, reads_left,
 *  facts_refused, changes, events, moves_queued) and the scope of the first rate limit that it met
 *  (rate_limit_scope). The log holds every start of the server in the run. */
function attributionLines(): Record<string, string>[] {
  return readFileSync(dispatchLog, "utf8")
    .split("\n")
    .filter((line) => line.includes('msg="dispatch delivery: pull request attribution"'))
    .map((line) =>
      Object.fromEntries(
        [...line.matchAll(/(\w+)=("(?:[^"\\]|\\.)*"|\S*)/g)].map(([, key = "", value = ""]) => [
          key,
          value.startsWith('"') ? (JSON.parse(value) as string) : value,
        ])
      )
    );
}

/** What `dispatch attribution` prints, as numbers where its lines count: each of its seven lines,
 *  read by its words (attributionCommand, packages/envoy-client/src/dispatch-commands/session.ts). */
interface AttributionCounts {
  readonly facts_left: number;
  readonly facts_refused: number;
  readonly keys_on_another_reading: number;
  readonly links_in_two_cases: number;
  readonly moves_queued: number;
  readonly owed_failures: number;
  readonly owed_refusals: number;
  readonly reading: string;
  readonly reads_left: number;
}

/** Runs `dispatch attribution` through the driver's runner of the command (runDispatch), and reads
 *  its seven lines. A line that it does not print fails the read, naming the line and the output. */
async function attributionCounts(): Promise<AttributionCounts> {
  const run = await runDispatch(["attribution"]);
  if (run.code !== 0) {
    throw new Error(`dispatch attribution exited ${run.code}: ${run.stderr.trim()}`);
  }
  const line = (pattern: RegExp): string[] => {
    const found = run.stdout.match(pattern);
    if (found === null) {
      throw new Error(
        `dispatch attribution printed no line ${pattern.source}: ${JSON.stringify(run.stdout)}`
      );
    }
    return found.slice(1);
  };
  const count = (printed: string | undefined) => Number(printed?.replaceAll(",", ""));
  const [factsLeft] = line(/^Pull requests that wait for their facts from GitHub: ([\d,]+)\.$/m);
  const [readsLeft, factsRefused] = line(
    /^Pull requests that a pass still reads from GitHub: ([\d,]+) \(([\d,]+) of them refused\)\.$/m
  );
  const [keysWait] = line(/^Pull requests whose keys wait to be read again: ([\d,]+)\.$/m);
  const [movesWait] = line(/^Changes of attribution whose events wait: ([\d,]+)\.$/m);
  const [reading] = line(/^The key reading: (.*)\.$/m);
  const [links] = line(/^Links that more than one issue holds in other letter cases: ([\d,]+)\.$/m);
  const [failures, refusals] = line(
    /^Pull requests whose head is not read while a read is owed: ([\d,]+) that GitHub did not answer.*, and ([\d,]+) that GitHub refuses/m
  );
  return {
    facts_left: count(factsLeft),
    facts_refused: count(factsRefused),
    keys_on_another_reading: count(keysWait),
    links_in_two_cases: count(links),
    moves_queued: count(movesWait),
    owed_failures: count(failures),
    owed_refusals: count(refusals),
    reading: reading ?? "",
    reads_left: count(readsLeft),
  };
}

/** One phase that the shell runs around one complete pass: the fixture it writes while no server
 *  runs, and the case that asserts after the start pass, given what awaitOnePass read. */
interface OnePassPhase {
  readonly assert: (pass: string) => Promise<string | undefined>;
  readonly caseName: string;
  readonly fixture?: () => Promise<void>;
}

// --- setup ---------------------------------------------------------------------------------------

// The fake GitHub answers for two repositories: the one the proof's issues resolve to, whose
// default branch holds the `behind` commit, the commit the bases case's branch checks are read
// at, and ends at `mainHead`, and one the App is not installed on, whose every read is GitHub's
// 404.
//
// The first is given `heldCommits` and no commit besides, so every commit a case records against
// it is held and the one the refusal case drives earns the 422 GitHub answers for a commit a
// repository does not hold. What each commit is for stands at its own declaration above, which
// is the one list of them.
//
// `commit_pulls` is empty on purpose: the record route's read-through asks GitHub which pull
// requests name a result's commit, and with no answer there the only thing that can teach
// Dispatch a head is the envelope the cases publish.
//
// `pulls` is what GitHub answers about one pull request. Two readers read it: the intake, which
// takes from each envelope only which pull request changed and reads that pull request here,
// and the reconcile's refresh of a stored head row. So each publish first sets the pull
// request's answer to what GitHub holds after that event (publishPullRequest), and the
// attribution case adds the two pull requests its own stored rows name. The fixture route
// replaces the whole map, so every answer set is kept in `githubPulls` and sent again with the
// next.

/** One pull request the fake answers for, as its single-pull-request route reads a fixture:
 *  the head commit and branch, the repository the head branch is in, the base branch, the
 *  state, the title and body, the time GitHub last updated it, and the merge's own commit; and
 *  as its commits read and its GraphQL route read it: the messages of its commits, and the status
 *  the commits read answers in place of them. */
interface FakePullRequest {
  readonly base_ref?: string;
  readonly body?: string;
  readonly commit_messages?: readonly string[];
  readonly commits_status?: number;
  readonly head_ref?: string;
  readonly head_repo: string;
  readonly head_sha: string;
  readonly merge_commit_sha?: string;
  readonly merged: boolean;
  readonly state: string;
  readonly title: string;
  readonly updated_at?: string;
}

/** Every pull request answer set so far, keyed by number. */
const githubPulls: Record<string, FakePullRequest> = {};

async function seedRepositories(pulls: Record<string, FakePullRequest>): Promise<void> {
  Object.assign(githubPulls, pulls);
  const seeded = await fetch(new URL("/__fixture/repos", githubUrl), {
    body: JSON.stringify({
      [repo]: {
        actions: "read",
        branch_commits: { main: [commits.behind, commits.comparedOnMain, commits.mainHead] },
        commits: Object.values(heldCommits),
        contents: "read",
        default_branch: "main",
        installation_id: 256,
        pull_requests: "read",
        pulls: githubPulls,
      },
      [hiddenRepo]: { contents: "read", installation_id: 257, installation_missing: true },
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  }).catch((error: unknown) => setupFailed("fake GitHub seed", error));
  if (!seeded.ok) setupFailed("fake GitHub seed", `${seeded.status} ${await seeded.text()}`);
}

// One half of a one-pass phase: the fixture while no server runs, or the case after the start
// pass. The process first takes the pull requests the fake answers, as an earlier process of the
// driver seeded them, so a seed of this process keeps them.
if (phaseArg !== undefined) {
  const onePassPhases: Record<string, OnePassPhase> = {
    // The self-check of the boundary: a fixture of nothing, then one start of the server.
    boundary: {
      assert: async (pass) => {
        note(pass);
        return undefined;
      },
      caseName: "one-pass-boundary",
    },
    "k-attribution": {
      assert: kAttributionAfterOnePass,
      caseName: "k-attribution-after-one-pass",
      fixture: kAttributionFixture,
    },
  };
  const [, name = "", half] = /^(.+)-(fixture|assert)$/.exec(phaseArg) ?? [];
  const phase = onePassPhases[name];
  if (phase === undefined || half === undefined) {
    setupFailed(
      `phase ${phaseArg}`,
      `no one-pass phase answers --phase ${phaseArg}; the phases are ${Object.keys(onePassPhases).join(", ")}, each with -fixture or -assert`
    );
  }
  const seeded = await fetch(new URL("/__fixture/repos", githubUrl)).catch((error: unknown) =>
    setupFailed("read of the fake GitHub's seed", error)
  );
  if (!seeded.ok)
    setupFailed("read of the fake GitHub's seed", `${seeded.status} ${await seeded.text()}`);
  const repositories = (await seeded.json()) as Record<
    string,
    { pulls?: Record<string, FakePullRequest> }
  >;
  Object.assign(githubPulls, repositories[repo]?.pulls ?? {});
  const passKey = `${name}/pass`;
  if (half === "fixture") {
    try {
      await phase.fixture?.();
      const fixtureAt = await prepareOnePass();
      const linesBefore = attributionLines().length;
      writeFileSync(
        statePath,
        JSON.stringify({ ...readState(), [passKey]: { fixtureAt, linesBefore } })
      );
      note(`${phaseArg}: the fixture is written, and no pass is recorded, at ${fixtureAt}`);
    } catch (error) {
      setupFailed(`fixture of the phase ${name}`, error);
    }
    void nats.close();
    process.exit(0);
  }
  const fixture = readState()[passKey] as { readonly linesBefore?: number } | undefined;
  if (fixture?.linesBefore === undefined) {
    setupFailed(
      `phase ${phaseArg}`,
      `the state file holds no fixture of ${name}: run --phase ${name}-fixture first`
    );
  }
  const pass = await awaitOnePass(fixture.linesBefore);
  await runCase(phase.caseName, () => phase.assert(pass));
  summarise();
}

// The counter's self-check: one read of a seeded pull request, made here and not by Dispatch,
// counts as one read. The fake holds that pull request alone until the seed below replaces it.
// Dispatch holds no row of a pull request yet, so it reads none beside it.
try {
  const counterSeed = await fetch(new URL("/__fixture/repos", githubUrl), {
    body: JSON.stringify({
      [repo]: {
        contents: "read",
        installation_id: 256,
        pulls: { "1": { head_sha: commits.mainHead, state: "open", title: "The counter's read" } },
      },
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!counterSeed.ok) throw new Error(`the seed answered ${counterSeed.status}`);
  await resetPullReads();
  const read = await fetch(new URL(`/repos/${repo}/pulls/1`, githubUrl));
  const counted = await pullReads();
  if (!read.ok || counted !== 1) {
    throw new Error(
      `one GET /repos/${repo}/pulls/1 answered ${read.status} and counts ${counted} read(s), want 200 and 1`
    );
  }
} catch (error) {
  setupFailed("the counter of pull request reads", error);
}
await seedRepositories({});

for (const [key, name, mapped] of [
  ["VRF", "Verification results", repo],
  ["VRH", "Verification results, unreadable repository", hiddenRepo],
] as const) {
  await asPerson("POST", "/api/v1/projects", { key, name }).catch((error: unknown) =>
    setupFailed(`project ${key}`, error)
  );
  await asSession("PUT", `/api/v1/settings/repo-projects/${mapped}`, {
    default: true,
    project: key,
  }).catch((error: unknown) => setupFailed(`repository mapping ${mapped}`, error));
}

/** Seeds one issue with a spec that marks one requirement and a Verification document holding
 *  `checks`, whose `%KEY%` is the issue's own key — a `serves` is refused unless it names a block
 *  that exists, so the requirement is written with the issue, before any check points at it — and
 *  answers that key. `parent` makes it the child of another issue, which the attribution case
 *  needs: a child's pull request answers for the child's own checks and for no check above it.
 *
 *  `force` is what every seed here needs: the run's issues are all about checks and pull
 *  requests, so a create of the next one is answered 409 POSSIBLE_DUPLICATE naming the issues
 *  a case before it seeded. */
async function seedIssue(
  project: string,
  title: string,
  checks: string,
  parent?: string
): Promise<string> {
  const issue = (await asPerson("POST", "/api/v1/issues", {
    force: true,
    parent,
    project,
    spec: `${title}.\n\n:::requirement{#r1}\nThe proof records a result against a check that serves this.\n:::\n`,
    title,
  })) as Json;
  const key = String(issue.key);
  await asSession("POST", issuePath(key, "/artifacts"), {
    content: `How ${key} is proved.\n\n${checks.replaceAll("%KEY%", key)}`,
    name: "verification.md",
  });
  return key;
}

/** One check block for the `checks` of `seedIssue`: the check `id`, saying `what`, which serves
 *  the requirement every seeded spec marks and names this script as its run. */
function checkFor(id: string, what: string): string {
  return `:::check{#${id} serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}\n${what}\n:::\n`;
}

// One issue with a plain spec, which every Dispatch serves: the route probe below writes its
// result against this one, so the probe reaches the record route rather than a typed block the
// server may not know.
const probeIssue = String(
  (
    (await asPerson("POST", "/api/v1/issues", {
      project: "VRF",
      spec: "The proof's first result write goes to this issue.\n",
      title: "The results route answers",
    }).catch((error: unknown) => setupFailed("the probe issue", error))) as Json
  ).key
);

console.log(
  `setup: person ${person}; session ${session}; probe issue ${probeIssue}; repositories ${repo} (default branch main) and ${hiddenRepo} (no installation); dispatch command ${dispatchCommand} (${dispatchCommandInput === "" ? "this checkout's" : "VERIFICATION_RESULTS_E2E_DISPATCH_CLI"}); the fake GitHub counts one direct read of a pull request as 1 read. Each case seeds the issue it drives.`
);

// --- cases ---------------------------------------------------------------------------------------

// The run's first result write, and the one every later case depends on: the record route takes a
// result and judges the check it names. A binary that does not serve the route at all answers 404
// and the run stops here, which is what this proof's negative control reads.
await runCase("the-results-route-is-served", async () => {
  const { answer } = await refusal("POST", issuePath(probeIssue, "/results"), {
    check: "no-such-check",
    commit: commits.a2,
    role: "tester",
    verdict: "pass",
  });
  note(`a result naming a check ${probeIssue} does not have answers ${answer}`);
  return answer === "400 UNKNOWN_CHECK" ? undefined : `the record route answers ${answer}`;
});
if (!results[0]?.ok) {
  summarise("every later case records a result too, so the run stops at the first result write");
}

// A2. A recorded result is read back under its check, with the fields it was recorded with.
await runCase("a2-record-and-read", async () => {
  const a2Issue = await seedIssue(
    "VRF",
    "A result is recorded and read back",
    `:::check{#a2 serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh" negative_control="Record nothing and the check reads no result."}
The result read back under this check is the one that was recorded.
:::
`
  );
  note(`${a2Issue} holds the check a2`);
  const answered = await record(a2Issue, {
    check: "a2",
    commit: commits.a2,
    negative_control: "Recorded nothing and the check read no result.",
    observed: "the record route answered 201",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  note(`the pull-request body line is ${JSON.stringify(answered.line)}`);
  note(`the record's answer says it was stored at ${answered.result.created_at}`);
  const check = await checkOf(a2Issue, "a2");
  note(
    `${a2Issue} check a2 (${check.when}) holds ${check.results.length} result(s): ${check.results
      .map(
        (row) =>
          `${row.verdict} at ${row.commit_sha.slice(0, 12)} by ${row.role} (${standsOf(row)})`
      )
      .join(", ")}`
  );
  if (check.results.length !== 1) return `the check holds ${check.results.length} results, want 1`;
  const [recorded] = check.results;
  if (recorded?.commit_sha !== commits.a2 || recorded.verdict !== "pass") {
    return `the result read back is ${JSON.stringify(recorded)}`;
  }
  if (recorded.observed !== "the record route answered 201") {
    return `the result's observed reads ${JSON.stringify(recorded.observed)}`;
  }
  // The check has not been reworded since, which the read answers on its own: the words the
  // result proved are the words the check carries now.
  if (recorded.check_words_moved !== false) {
    return `the result reads check_words_moved ${JSON.stringify(recorded.check_words_moved)}`;
  }
  // The record's own answer carries the moment the row was stored, not the zero time: a reader
  // of `dispatch result` takes it for when the run happened, and only the later read was ever
  // right about it.
  const stored = Date.parse(answered.result.created_at);
  if (Number.isNaN(stored) || stored <= 0) {
    return `the record's answer carries created_at ${JSON.stringify(answered.result.created_at)}`;
  }
  if (answered.result.created_at !== recorded.created_at) {
    return `the record answered created_at ${answered.result.created_at}, the read gives ${recorded.created_at}`;
  }
  return undefined;
});

// A4. The head a `before_merge` result is compared with is the one the bus taught Dispatch: a
// push moves it with no write of anyone's to Dispatch, and the result at the head the push
// replaced stands at another commit when it is next read.
await runCase("a4-a-push-moves-the-head", async () => {
  const a4Issue = await seedIssue(
    "VRF",
    "A push moves the head a result is measured against",
    `:::check{#a4 serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result stands while its commit is the pull request's head.
:::
`
  );
  const opened = await publishPullRequest({
    action: "opened",
    base_ref: "main",
    body: `Proves ${a4Issue}.`,
    head_ref: `legion/${a4Issue}`,
    head_repo: repo,
    head_sha: commits.headOne,
    kind: "pr",
    merged: "false",
    number: "7",
    repo,
    title: `${a4Issue} the proof's pull request`,
    updated_at: new Date(Date.now() - 60_000).toISOString(),
    url: `https://github.com/${repo}/pull/7`,
  });
  note(`published the opened envelope on ${opened.subject}`);
  await record(a4Issue, {
    check: "a4",
    commit: commits.headOne,
    observed: "recorded at the pull request's first head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFirstHead = await awaitCommitStands(
    a4Issue,
    "a4",
    commits.headOne,
    "at_basis",
    afterEvent(opened)
  );
  note(`the commit of the result at ${commits.headOne.slice(0, 12)} stands ${atFirstHead}`);
  if (atFirstHead !== "at_basis") {
    return `the commit of the result at the first head stands ${atFirstHead}`;
  }
  const passAtFirstHead = await passStandsOf(a4Issue, "a4");
  note(`a pass of a4 stands: ${passAtFirstHead}`);
  if (passAtFirstHead !== "yes (tester)") return `a pass of a4 stands ${passAtFirstHead}`;

  const pushed = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `Proves ${a4Issue}.`,
    head_ref: `legion/${a4Issue}`,
    head_repo: repo,
    head_sha: commits.headTwo,
    kind: "pr",
    merged: "false",
    number: "7",
    repo,
    title: `${a4Issue} the proof's pull request`,
    updated_at: new Date().toISOString(),
    url: `https://github.com/${repo}/pull/7`,
  });
  note(`published the synchronize envelope on ${pushed.subject}`);
  const moved = await awaitCommitStands(
    a4Issue,
    "a4",
    commits.headOne,
    "other_commit",
    afterEvent(pushed)
  );
  note(`after the push the same result stands ${moved}, with no write of Dispatch's`);
  if (moved !== "other_commit") {
    return `the commit of the result at the first head stands ${moved} after the push`;
  }
  const passAfterPush = await passStandsOf(a4Issue, "a4");
  note(`after the push a pass of a4 stands: ${passAfterPush}`);
  if (passAfterPush !== "no") return `after the push a pass of a4 stands ${passAfterPush}`;

  await record(a4Issue, {
    check: "a4",
    commit: commits.headTwo,
    observed: "recorded at the pull request's new head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atNewHead = await awaitCommitStands(
    a4Issue,
    "a4",
    commits.headTwo,
    "at_basis",
    afterEvent(pushed)
  );
  note(`the result at the new head ${commits.headTwo.slice(0, 12)} stands ${atNewHead}`);
  if (atNewHead !== "at_basis") return `the result at the new head stands ${atNewHead}`;
  const passAtNewHead = await passStandsOf(a4Issue, "a4");
  note(`at the new head a pass of a4 stands: ${passAtNewHead}`);
  return passAtNewHead === "yes (tester)"
    ? undefined
    : `at the new head a pass of a4 stands ${passAtNewHead}`;
});

// A fork's pull request is no basis for a result. Anyone may open one against a public
// repository, at any commit and with any title, so the head a result is compared with is only
// a pull request whose head branch lives in this repository. The same pull request reported with
// its head here, which is the only difference between the two envelopes, is a basis.
await runCase("a-fork-pull-request-is-no-basis", async () => {
  const forkIssue = await seedIssue(
    "VRF",
    "A fork's pull request is no basis for a result",
    `:::check{#fork serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result stands while its commit is the head of a pull request of this repository.
:::
`
  );
  const forked = await publishPullRequest({
    action: "opened",
    base_ref: "main",
    body: `Proves ${forkIssue}.`,
    head_ref: `legion/${forkIssue}`,
    head_repo: `stranger/${String(repo.split("/")[1])}`,
    head_sha: commits.fork,
    kind: "pr",
    merged: "false",
    number: "9",
    repo,
    title: `${forkIssue} the stranger's pull request`,
    updated_at: new Date(Date.now() - 60_000).toISOString(),
    url: `https://github.com/${repo}/pull/9`,
  });
  note(`published the fork's envelope on ${forked.subject}`);
  await record(forkIssue, {
    check: "fork",
    commit: commits.fork,
    observed: "recorded at the commit the fork's pull request heads",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFork = await awaitCommitStands(
    forkIssue,
    "fork",
    commits.fork,
    "at_basis",
    afterEvent(forked)
  );
  note(`the commit of the result at the fork's head ${commits.fork.slice(0, 12)} stands ${atFork}`);
  if (atFork.startsWith("at_basis"))
    return `a fork's pull request was taken for a basis (${atFork})`;
  const compared = await checkOf(forkIssue, "fork");
  note(
    `the check compared with ${compared.compared_with.length} commit(s), and names ${compared.compared_nothing ?? "no reason"} for that`
  );
  if (compared.compared_nothing !== "no_pull_request_or_branch") {
    return `the check compared with nothing for ${JSON.stringify(compared.compared_nothing)}`;
  }

  // The same pull request, now reported with its head in this repository, is a basis: the
  // refusal above is the head's repository and nothing else.
  const owned = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `Proves ${forkIssue}.`,
    head_ref: `legion/${forkIssue}`,
    head_repo: repo,
    head_sha: commits.fork,
    kind: "pr",
    merged: "false",
    number: "9",
    repo,
    title: `${forkIssue} the stranger's pull request`,
    updated_at: new Date().toISOString(),
    url: `https://github.com/${repo}/pull/9`,
  });
  note(`published the same pull request with its head in ${repo} on ${owned.subject}`);
  const atOwnHead = await awaitCommitStands(
    forkIssue,
    "fork",
    commits.fork,
    "at_basis",
    afterEvent(owned)
  );
  note(`the same result now stands ${atOwnHead}`);
  return atOwnHead === "at_basis"
    ? undefined
    : `the result at this repository's head stands ${atOwnHead}`;
});

// A5. A closed issue takes an after-merge result, and still refuses a document write.
await runCase("a5-a-closed-issue-takes-the-after-merge-result", async () => {
  const a5Issue = await seedIssue(
    "VRF",
    "A closed issue takes an after-merge result",
    `:::check{#after when="after_merge" surface="api" run="curl the production route"}
The production check runs after the merge.
:::

:::check{#before surface="cli" run="bun test"}
The pre-merge check runs before it.
:::
`
  );
  await asSession("PATCH", issuePath(a5Issue), { status: "done" });
  const closed = (await asSession("GET", issuePath(a5Issue))) as Json;
  note(`${a5Issue} is ${String(closed.status)}`);
  const afterMerge = await record(a5Issue, {
    check: "after",
    commit: commits.closed,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  note(`the after-merge result recorded: ${JSON.stringify(afterMerge.line)}`);
  const check = await checkOf(a5Issue, "after");
  if (check.results.length !== 1)
    return `the after-merge check holds ${check.results.length} results, want 1`;

  const { answer: beforeMerge } = await refusal("POST", issuePath(a5Issue, "/results"), {
    check: "before",
    commit: commits.closed,
    role: "tester",
    verdict: "pass",
  });
  note(`a result for the before-merge check there answers ${beforeMerge}`);
  if (beforeMerge !== "409 ISSUE_CLOSED") return `the before-merge result answers ${beforeMerge}`;

  const { answer: document } = await refusal("POST", issuePath(a5Issue, "/artifacts"), {
    content: "A note the closed issue must refuse.\n",
    name: "note.md",
  });
  note(`a document write there answers ${document}`);
  return document === "409 ISSUE_CLOSED" ? undefined : `the document write answers ${document}`;
});

// A10. A `default_branch` result stands while GitHub says the branch holds its commit.
await runCase("a10-a-default-branch-result-follows-the-branch", async () => {
  const a10Issue = await seedIssue(
    "VRF",
    "A default-branch result stands while the branch holds its commit",
    `:::check{#branch when="default_branch" surface="api" run="curl the production route"}
The default branch holds the commit this check ran at.
:::
`
  );
  await record(a10Issue, {
    check: "branch",
    commit: commits.behind,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const held = await commitStandsAt(a10Issue, "branch", commits.behind);
  note(
    `the result at ${commits.behind.slice(0, 12)}, which the compare calls behind, stands ${held}`
  );
  if (held !== "at_basis") return `the result at the commit the branch holds stands ${held}`;

  await record(a10Issue, {
    check: "branch",
    commit: commits.diverged,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const apart = await commitStandsAt(a10Issue, "branch", commits.diverged);
  note(
    `the result at ${commits.diverged.slice(0, 12)}, which the compare calls diverged, stands ${apart}`
  );
  return apart === "other_commit" ? undefined : `the result at the diverged commit stands ${apart}`;
});

// A commit the repository does not hold: the record route asks GitHub about every result's
// commit before it writes anything, and the fake answers the 422 GitHub answers for a 40-hex
// commit it was not seeded with. The refusal names the commit whole and the `owner/repo` the
// check's issue resolves to, and the check keeps no row, since nothing is written before the
// read.
await runCase("a-result-at-a-commit-the-repository-does-not-hold-is-refused", async () => {
  const unheldIssue = await seedIssue(
    "VRF",
    "An unpushed commit earns a refusal",
    `:::check{#unheld serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result is recorded only at a commit the repository holds.
:::
`
  );
  const refused = await refusal("POST", issuePath(unheldIssue, "/results"), {
    check: "unheld",
    commit: commits.unheld,
    observed: "the run passed at a commit nobody pushed",
    role: "tester",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
    verdict: "pass",
  });
  note(`a result at ${commits.unheld.slice(0, 12)} answers ${refused.answer}: ${refused.error}`);
  if (refused.answer !== "400 RESULT_COMMIT_UNKNOWN") {
    return `the record route answers ${refused.answer}`;
  }
  if (!refused.error.includes(commits.unheld)) {
    return `the refusal does not name the commit: ${refused.error}`;
  }
  if (!refused.error.includes(repo)) {
    return `the refusal does not name ${repo}: ${refused.error}`;
  }
  const check = await checkOf(unheldIssue, "unheld");
  note(`${unheldIssue} check unheld holds ${check.results.length} result(s)`);
  return check.results.length === 0
    ? undefined
    : `the refused result left ${check.results.length} row(s) under the check`;
});

// The unreadable repository: Dispatch cannot say where the commit stands, so it answers unknown
// with the reason — for a `before_merge` check as well as a `default_branch` one. Nothing can
// teach Dispatch the pull request heads of a repository it cannot read, so "this commit heads
// none I know" is no evidence that the work has moved on, and a pass the reviewer reads is
// held open rather than refused.
await runCase("an-unreadable-repository-answers-unknown", async () => {
  const hiddenIssue = await seedIssue(
    "VRH",
    "A result on a repository Dispatch cannot read",
    `:::check{#hidden when="default_branch" surface="api" run="curl the production route"}
Dispatch cannot see the repository this check's issue resolves to.
:::

:::check{#hidden-pre surface="cli" run="bash scripts/e2e/verification-results.sh"}
The pre-merge check of the same unreadable repository.
:::
`
  );
  await record(hiddenIssue, {
    check: "hidden",
    commit: commits.hidden,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const unknown = await commitStandsAt(hiddenIssue, "hidden", commits.hidden);
  note(`${hiddenIssue} resolves to ${hiddenRepo}, which the installation lookup does not find`);
  note(`the default_branch result at ${commits.hidden.slice(0, 12)} stands ${unknown}`);
  if (unknown !== "unknown:repository_unreadable") {
    return `the default_branch result stands ${unknown}, want unknown:repository_unreadable`;
  }
  const passUnknown = await passStandsOf(hiddenIssue, "hidden");
  note(`a pass of the default_branch check stands: ${passUnknown}`);
  if (passUnknown !== "unknown") {
    return `a pass of the default_branch check stands ${passUnknown}, want unknown`;
  }

  await record(hiddenIssue, {
    check: "hidden-pre",
    commit: commits.hidden,
    observed: "the pre-merge run passed",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const beforeMerge = await commitStandsAt(hiddenIssue, "hidden-pre", commits.hidden);
  note(`the before_merge result at ${commits.hidden.slice(0, 12)} stands ${beforeMerge}`);
  if (beforeMerge !== "unknown:repository_unreadable") {
    return `the before_merge result stands ${beforeMerge}, want unknown:repository_unreadable`;
  }
  const passBeforeMerge = await passStandsOf(hiddenIssue, "hidden-pre");
  note(`a pass of the before_merge check stands: ${passBeforeMerge}`);
  return passBeforeMerge === "unknown"
    ? undefined
    : `a pass of the before_merge check stands ${passBeforeMerge}, want unknown`;
});

// k-compared-commit. What each check compares its results with, read through the `dispatch
// results` command and through the API: a `before_merge` check names the head of every pull
// request of its issue, open or merged, an `after_merge` check names the branch the merge went
// to and the default branch, and a `default_branch` check names the default branch alone. A
// commit named to the command (`--at`) or to the API (`?at=`) replaces all of them, for every
// check of the issue. Dispatch then reads GitHub for nothing but the check runs of a check CI
// proves, at the issue's newest pull request head as on every read.
//
// The case's negative control is the second open pull request: with no commit named, the result
// at its head stands at that head, so a run that named one head alone would fail here.
//
// The last read is of an issue whose repository Dispatch cannot read, which says that rather
// than saying it has nothing to compare with.
await runCase("k-compared-commit-the-bases-each-check-names", async () => {
  const basesIssue = await seedIssue(
    "VRF",
    "The bases each check compares its results with",
    `:::check{#before serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A before-merge result is compared with the head of every pull request of this issue.
:::

:::check{#after when="after_merge" surface="api" run="curl the production route"}
An after-merge result is compared with the branch the merge went to and with the default branch.
:::

:::check{#branch when="default_branch" surface="api" run="curl the production route"}
A default-branch result is compared with the default branch alone.
:::
`
  );
  const openPulls = [
    { head: commits.comparedHeadA, number: 21, ref: `legion/${basesIssue}` },
    { head: commits.comparedHeadA2, number: 22, ref: `legion/${basesIssue}-follow-up` },
  ];
  const openedPulls = new Map<number, Published>();
  for (const pull of openPulls) {
    const published = await publishPullRequest({
      action: "opened",
      base_ref: "main",
      body: `Proves ${basesIssue}.`,
      head_ref: pull.ref,
      head_repo: repo,
      head_sha: pull.head,
      kind: "pr",
      merged: "false",
      number: String(pull.number),
      repo,
      title: `${basesIssue} pull request #${pull.number}`,
      updated_at: new Date(Date.now() - 60_000).toISOString(),
      url: `https://github.com/${repo}/pull/${pull.number}`,
    });
    openedPulls.set(pull.number, published);
  }
  // The merged pull request's base branch is one the fake holds no commits for, as a repository
  // that deletes a branch on the merge leaves it: the compare read says that branch does not
  // carry the commit, and the default branch answers for the work. The envelope names no author
  // of the delivery population, so the merge writes this head row and nothing else.
  await publishPullRequest({
    action: "closed",
    base_ref: "release/old",
    body: `Proves ${basesIssue}.`,
    head_ref: `legion/${basesIssue}-merged`,
    head_repo: repo,
    head_sha: commits.comparedMergedHead,
    kind: "pr",
    merge_commit_sha: commits.comparedMergeCommit,
    merged: "true",
    number: "23",
    repo,
    title: `${basesIssue} merged pull request #23`,
    updated_at: new Date(Date.now() - 60_000).toISOString(),
    url: `https://github.com/${repo}/pull/23`,
  });
  note(`${basesIssue} has pull requests #21 and #22 open, and #23 merged into release/old`);

  for (const commit of [commits.comparedHeadA, commits.comparedHeadA2, commits.comparedApart]) {
    await record(basesIssue, {
      check: "before",
      commit,
      observed: "the pre-merge run passed",
      run: "bash scripts/e2e/verification-results.sh",
      surface: "cli",
    });
  }
  for (const check of ["after", "branch"]) {
    await record(basesIssue, {
      check,
      commit: commits.comparedOnMain,
      observed: "the production route answered",
      run: "curl the production route",
      surface: "api",
    });
  }
  // A2 is pull request #22's head, so its answer is due 5 s after #22's event.
  const openedA2 = openedPulls.get(22);
  if (openedA2 === undefined) return "pull request #22 was not published";
  const atOwnHead = await awaitCommitStands(
    basesIssue,
    "before",
    commits.comparedHeadA2,
    "at_basis",
    afterEvent(openedA2)
  );
  if (atOwnHead !== "at_basis") return `the result at A2 stands ${atOwnHead} before any read`;

  // The command, with no commit named. Its head line is where it names the bases.
  const printed = await resultsCommand(basesIssue);
  for (const line of commandHeadLines(printed)) note(`dispatch results: ${line}`);
  const wantedLines = [
    `the head of pull request #21 at ${commits.comparedHeadA}`,
    `the head of pull request #22 at ${commits.comparedHeadA2}`,
    `the head of pull request #23 at ${commits.comparedMergedHead}`,
    "branch release/old",
    "branch main",
  ];
  for (const wanted of wantedLines) {
    if (!printed.includes(wanted)) return `the command named no ${wanted}`;
  }

  // The API, with no commit named: the same bases, each with its pull request's number and the
  // time Dispatch learned that head. A before-merge check names the head of every pull request
  // of its issue, the merged one's among them.
  const before = await checkOf(basesIssue, "before");
  note(`the API's before check compares with ${comparedOf(before)}`);
  const heads = new Map((before.compared_with ?? []).map((basis) => [basis.pull_request, basis]));
  if (heads.size !== 3)
    return `the before check compares with ${comparedOf(before)}, want the heads of #21, #22 and #23`;
  for (const [number, head] of [
    [21, commits.comparedHeadA],
    [22, commits.comparedHeadA2],
    [23, commits.comparedMergedHead],
  ] as const) {
    const basis = heads.get(number);
    if (basis === undefined || basis.kind !== "pull_request" || basis.commit !== head) {
      return `the basis of pull request #${number} is ${JSON.stringify(basis)}`;
    }
    if (basis.learned_at === undefined) {
      return `the basis of pull request #${number} says no time Dispatch learned it`;
    }
  }
  for (const [commit, number] of [
    [commits.comparedHeadA, 21],
    [commits.comparedHeadA2, 22],
  ] as const) {
    const result = before.results.find((row) => row.commit_sha === commit);
    const stands = result === undefined ? "absent" : standsOf(result);
    const stood = result?.commit_stands?.basis;
    note(
      `the result at ${commit.slice(0, 12)} stands ${stands}, at ${stood === undefined ? "no basis" : basisOf(stood)}`
    );
    if (stands !== "at_basis" || stood?.pull_request !== number) {
      return `the result at ${commit.slice(0, 12)} stands ${stands}, want at_basis at the head of #${number}`;
    }
  }
  note(
    "each result stands at its own pull request's head, so a read naming one head alone fails this control"
  );
  const apart = await commitStandsAt(basesIssue, "before", commits.comparedApart);
  note(
    `the result at ${commits.comparedApart.slice(0, 12)}, which heads no pull request, stands ${apart}`
  );
  if (apart !== "other_commit") return `the result at the unrelated commit stands ${apart}`;

  const after = await checkOf(basesIssue, "after");
  note(`the API's after_merge check compares with ${comparedOf(after)}`);
  const branches = (after.compared_with ?? []).map((basis) => `${basis.kind} ${basis.branch}`);
  if (branches.join(", ") !== "branch release/old, branch main") {
    return `the after_merge check compares with ${comparedOf(after)}`;
  }
  const merged = after.results.find((row) => row.commit_sha === commits.comparedOnMain);
  const mergedStands = merged === undefined ? "absent" : standsOf(merged);
  const mergedBasis = merged?.commit_stands?.basis;
  if (mergedStands !== "at_basis" || mergedBasis?.branch !== "main") {
    return `the after_merge result stands ${mergedStands} at ${JSON.stringify(mergedBasis)}, want the branch that carries it`;
  }
  note(`the after_merge result stands at ${basisOf(mergedBasis)}, the branch that carries it`);

  const branchCheck = await checkOf(basesIssue, "branch");
  note(`the API's default_branch check compares with ${comparedOf(branchCheck)}`);
  if ((branchCheck.compared_with ?? []).map((basis) => basis.branch).join(", ") !== "main") {
    return `the default_branch check compares with ${comparedOf(branchCheck)}`;
  }

  // A commit named: every check of the issue compares with that commit and nothing else.
  for (const [named, what] of [
    [commits.comparedHeadA, "A, the first pull request's head"],
    [commits.comparedNamed, "B, a commit no result is at"],
  ] as const) {
    const printedAt = await resultsCommand(basesIssue, named);
    for (const line of commandHeadLines(printedAt))
      note(`dispatch results --at ${named.slice(0, 12)}: ${line}`);
    if (!printedAt.includes(`the commit you named, ${named}`)) {
      return `the command with --at ${what} named another basis`;
    }
    const checks = await checksOf(basesIssue, named);
    for (const check of checks) {
      const bases = (check.compared_with ?? []).map((basis) => `${basis.kind} ${basis.commit}`);
      if (bases.join(", ") !== `named ${named}`) {
        return `?at=${what}: check ${check.block_id} compares with ${comparedOf(check)}`;
      }
      for (const result of check.results) {
        const stands = standsOf(result);
        const wanted = result.commit_sha === named ? "at_basis" : "other_commit";
        if (stands !== wanted) {
          return `?at=${what}: the result at ${result.commit_sha.slice(0, 12)} stands ${stands}, want ${wanted}`;
        }
      }
    }
    note(
      `with ${what} named, every check compares with that commit alone, and every result elsewhere reads at another commit`
    );
  }

  // The event moves the first pull request's head. Its update time is a minute ahead of this
  // write, so the row keeps the event's own time: the basis then names GitHub's update time and
  // not a read of Dispatch's. Nothing reads GitHub between the two reads, since the new head is
  // a head Dispatch holds.
  const movedAt = new Date(Date.now() + 60_000).toISOString();
  const movedEvent = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `Proves ${basesIssue}.`,
    head_ref: `legion/${basesIssue}`,
    head_repo: repo,
    head_sha: commits.comparedHeadA3,
    kind: "pr",
    merged: "false",
    number: "21",
    repo,
    title: `${basesIssue} pull request #21`,
    updated_at: movedAt,
    url: `https://github.com/${repo}/pull/21`,
  });
  const movedTo = await awaitCommitStands(
    basesIssue,
    "before",
    commits.comparedHeadA,
    "other_commit",
    afterEvent(movedEvent)
  );
  if (movedTo !== "other_commit") return `after the event the result at A stands ${movedTo}`;
  const afterMove = await checkOf(basesIssue, "before");
  note(`after the event the before check compares with ${comparedOf(afterMove)}`);
  const moved = (afterMove.compared_with ?? []).find((basis) => basis.pull_request === 21);
  if (moved?.commit !== commits.comparedHeadA3) {
    return `the basis of pull request #21 is ${JSON.stringify(moved)}, want A3`;
  }
  if (moved.learned_at?.kind !== "updated_at") {
    return `the basis of pull request #21 was learned ${JSON.stringify(moved.learned_at)}, want the pull request's update time`;
  }
  if (Date.parse(moved.learned_at.at) !== Date.parse(movedAt)) {
    return `the basis names ${moved.learned_at.at}, want the event's own update time ${movedAt}`;
  }
  note(
    `the basis of #21 names A3 at the event's own update time ${movedAt}, as the pull request's update time and not as a read of Dispatch's`
  );

  // The repository Dispatch cannot read says so, and marks no result.
  const hiddenBases = await seedIssue(
    "VRH",
    "The bases of a check whose repository Dispatch cannot read",
    `:::check{#unreadable serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A check of an issue whose repository Dispatch cannot read compares with nothing it can name.
:::
`
  );
  await record(hiddenBases, {
    check: "unreadable",
    commit: commits.hidden,
    observed: "the pre-merge run passed",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const unreadable = await checkOf(hiddenBases, "unreadable");
  const printedHidden = await resultsCommand(hiddenBases);
  for (const line of commandHeadLines(printedHidden)) note(`dispatch results: ${line}`);
  note(`the API says the check compared with ${comparedOf(unreadable)}`);
  if (unreadable.compared_nothing !== "repository_unreadable") {
    return `the unreadable repository's check compared with ${comparedOf(unreadable)}`;
  }
  if (!printedHidden.includes("Compared with nothing: Dispatch cannot read the repository")) {
    return "the command did not say Dispatch cannot read the repository";
  }
  const passUnreadable = await passStandsOf(hiddenBases, "unreadable");
  note(`a pass of it stands: ${passUnreadable}`);
  return passUnreadable === "unknown"
    ? undefined
    : `a pass of it stands ${passUnreadable}, want unknown`;
});

// k-attribution. What decides a pull request's issue: the five ranked sources
// (delivery/attribution.go), of which a person's link to the pull request's own URL ranks above
// the head branch, and the head branch above a key in the title or the body. Six pull requests
// drive it: one its branch credits to a child while its title names the parent first, one its
// body credits, one whose link beats its branch, one whose link beats its title, and two rows
// stored as the release before writes them, with no texts, which one pass reads and records again.
//
// This case drives the four pull requests of events, against the server that runs a pass every
// two seconds. The intake stores the facts and the texts of each one at once, and the statement
// gives its issue at each read. The phase k-attribution then stores the two rows of the release
// before, and after one pass it reads the issue that each of the six rows records
// (kAttributionFixture, kAttributionAfterOnePass).
//
// The case's negative controls are in it: the pull request whose body decides is on a branch
// that names no issue, so the branch rule swallows no other source; the pull request a person
// linked is on the child's own branch, so the link stands above it and the child does not compare
// with it; the issue a linked title names does not compare with it either; and the last envelope
// moves a head and not its attribution, so the pass of the phase appends no event for it.
await runCase("k-attribution-what-decides-a-pull-request-issue", async () => {
  // A check's id must be one among the issue and the issues above it: a result naming an id the
  // child and the parent both carry is refused 409 CHECK_AMBIGUOUS, so each issue's check is
  // named for the issue it proves.
  const parent = await seedIssue(
    "VRF",
    "The parent whose child carries the work",
    checkFor("parent", "The parent's results are compared with the parent's own pull requests.")
  );
  const child = await seedIssue(
    "VRF",
    "The child that carries the work",
    checkFor("child", "The child's results are compared with the child's own pull request."),
    parent
  );
  const bodied = await seedIssue(
    "VRF",
    "The issue a pull request's body names",
    checkFor("body", "A pull request whose branch names no issue is credited by its body.")
  );
  const linkedOverBranch = await seedIssue(
    "VRF",
    "The issue a person linked a pull request to, against its branch",
    checkFor("link-over-branch", "A person's link to a pull request stands above its branch.")
  );
  const titled = await seedIssue(
    "VRF",
    "The issue a pull request's title names",
    checkFor("title", "A key in a title is the weakest source but the commit messages.")
  );
  const linkedOverTitle = await seedIssue(
    "VRF",
    "The issue a person linked a pull request to, against its title",
    checkFor("link-over-title", "A person's link to a pull request stands above its title.")
  );
  note(
    `parent ${parent}, child ${child} of it; ${bodied}, ${linkedOverBranch}, ${titled} and ${linkedOverTitle} beside them`
  );

  // The two links a person wrote, before any envelope is published: the attribution reads them
  // as it writes the row.
  for (const [issue, number] of [
    [linkedOverBranch, 43],
    [linkedOverTitle, 44],
  ] as const) {
    await asPerson("PATCH", issuePath(issue), {
      external_links: [{ url: `https://github.com/${repo}/pull/${number}` }],
    });
    note(`a person linked ${issue} to ${repo}#${number}`);
  }

  const envelopes = [
    {
      body: `This carries part of ${parent}, and ${child} is the issue it closes.`,
      check: "child",
      head: commits.attributionBranch,
      head_ref: `legion/${child}`,
      issue: child,
      number: 41,
      title: `${parent} and ${child}: the child's pull request`,
    },
    {
      body: `This one proves ${bodied}.`,
      check: "body",
      head: commits.attributionBody,
      head_ref: "work/no-key-in-this-branch",
      issue: bodied,
      number: 42,
      title: "A pull request whose title names no issue",
    },
    {
      body: "A person linked this pull request to an issue of its own.",
      check: "link-over-branch",
      head: commits.attributionLinkOverBranch,
      head_ref: `legion/${child}`,
      issue: linkedOverBranch,
      number: 43,
      title: "A pull request on the child's branch that a person linked elsewhere",
    },
    {
      body: "A person linked this pull request too.",
      check: "link-over-title",
      head: commits.attributionLinkOverTitle,
      head_ref: "work/no-key-in-this-one-either",
      issue: linkedOverTitle,
      number: 44,
      title: `${titled} is the issue this title names`,
    },
  ];
  const publishedEnvelopes = new Map<number, Published>();
  for (const envelope of envelopes) {
    const published = await publishPullRequest({
      action: "opened",
      base_ref: "main",
      body: envelope.body,
      head_ref: envelope.head_ref,
      head_repo: repo,
      head_sha: envelope.head,
      kind: "pr",
      merged: "false",
      number: String(envelope.number),
      repo,
      title: envelope.title,
      updated_at: new Date(Date.now() - 60_000).toISOString(),
      url: `https://github.com/${repo}/pull/${envelope.number}`,
    });
    publishedEnvelopes.set(envelope.number, published);
  }
  // The intake stores each pull request's facts and texts, and the reader of the check gives
  // its issue from them, both within the bound of an event. The issue that a row records waits
  // for a pass, which the phase reads.
  for (const envelope of envelopes) {
    const published = publishedEnvelopes.get(envelope.number);
    if (published === undefined) return `#${envelope.number} was not published`;
    const facts = `open at ${envelope.head.slice(0, 12)}, texts stored`;
    const stored = await awaitStoredHead(envelope.number, facts, afterEvent(published));
    note(`#${envelope.number} on ${envelope.head_ref}, titled ${JSON.stringify(envelope.title)}`);
    note(`  is stored as ${stored}`);
    if (!stored.endsWith(`(${facts})`)) {
      return `#${envelope.number} is stored as ${stored}, want its facts (${facts})`;
    }
    const compared = await awaitComparedWith(
      envelope.issue,
      envelope.check,
      envelope.head,
      afterEvent(published)
    );
    note(`  ${envelope.issue} compares with ${compared.shown}`);
    if (!compared.held) {
      return `${envelope.issue} compares with ${compared.shown}, want the head of #${envelope.number}`;
    }
  }
  // A person's link stands above the branch and the title: the child does not take the pull
  // request on its branch that a person linked elsewhere, and the issue a title names does not
  // take the pull request whose title names it.
  for (const [issue, check, number, head] of [
    [child, "child", 43, commits.attributionLinkOverBranch],
    [titled, "title", 44, commits.attributionLinkOverTitle],
  ] as const) {
    const bases = await checkOf(issue, check);
    note(`${issue} compares with ${comparedOf(bases)}`);
    if ((bases.compared_with ?? []).some((basis) => basis.commit === head)) {
      return `${issue} took the head of #${number}, which a person linked to another issue`;
    }
  }

  // The child's own pull request answers for the child's checks and for none of the parent's.
  await record(child, {
    check: "child",
    commit: commits.attributionBranch,
    observed: "the child's run passed at its pull request's head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  await record(parent, {
    check: "parent",
    commit: commits.attributionBranch,
    observed: "the same commit, recorded against the parent's check",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const childCheck = (await checksOf(child, commits.attributionBranch))[0];
  const childStands = childCheck?.results.map(standsOf).join(", ") ?? "";
  note(
    `${child} with ?at=${commits.attributionBranch.slice(0, 12)} compares with ${comparedOf(childCheck as CheckBlock)}, and its result stands ${childStands}`
  );
  if (childStands !== "at_basis") return `the child's results stand ${childStands}, want at_basis`;
  const childHeads = await checkOf(child, "child");
  note(`${child} compares with ${comparedOf(childHeads)}`);
  const parentHeads = await checkOf(parent, "parent");
  note(`${parent} compares with ${comparedOf(parentHeads)}`);
  if (
    !(childHeads.compared_with ?? []).some((basis) => basis.commit === commits.attributionBranch)
  ) {
    return `the child's check does not compare with its own pull request's head`;
  }
  if (
    (parentHeads.compared_with ?? []).some((basis) => basis.commit === commits.attributionBranch)
  ) {
    return `the parent's check took the child's head`;
  }
  const parentStands = await commitStandsAt(parent, "parent", commits.attributionBranch);
  note(`the parent's result at the child's head stands ${parentStands}`);
  if (parentStands !== "other_commit") return `the parent's result stands ${parentStands}`;

  // An attribution that does not move appends no event: the same pull request at a new head,
  // credited to the child as before. The one pass of the phase comes after this push, and appends
  // the moves of the two stored rows alone.
  const pushed = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `This carries part of ${parent}, and ${child} is the issue it closes.`,
    head_ref: `legion/${child}`,
    head_repo: repo,
    head_sha: commits.attributionBranchMoved,
    kind: "pr",
    merged: "false",
    number: "41",
    repo,
    title: `${parent} and ${child}: the child's pull request`,
    updated_at: new Date().toISOString(),
    url: `https://github.com/${repo}/pull/41`,
  });
  const movedFacts = `open at ${commits.attributionBranchMoved.slice(0, 12)}, texts stored`;
  const movedHead = await awaitStoredHead(41, movedFacts, afterEvent(pushed));
  note(`#41 after the push is stored as ${movedHead}`);
  if (!movedHead.endsWith(`(${movedFacts})`)) {
    return `#41 after the push is stored as ${movedHead}, want its facts (${movedFacts})`;
  }
  const afterPush = await awaitComparedWith(
    child,
    "child",
    commits.attributionBranchMoved,
    afterEvent(pushed)
  );
  note(`${child} compares with ${afterPush.shown}`);
  if (!afterPush.held) {
    return `${child} compares with ${afterPush.shown} after the push, want the new head of #41`;
  }
  const issues: AttributionIssues = {
    bodied,
    child,
    linkedOverBranch,
    linkedOverTitle,
    parent,
    titled,
  };
  writeFileSync(statePath, JSON.stringify({ ...readState(), "k-attribution": issues }));
  return undefined;
});

/** The issues that the case k-attribution seeded, which its phase reads from the state file. */
interface AttributionIssues {
  readonly bodied: string;
  readonly child: string;
  readonly linkedOverBranch: string;
  readonly linkedOverTitle: string;
  readonly parent: string;
  readonly titled: string;
}

/** The fixture of the phase k-attribution, which runs while no server runs: GitHub's answer for
 *  each of the two stored rows, whose title names the child, and the two rows as the release before
 *  writes them, credited to the parent by title_body, with no texts and no attribution_rule. No
 *  route of Dispatch can write such a row: every write of this build stores the texts. */
async function kAttributionFixture(): Promise<void> {
  const { child, parent } = readState()["k-attribution"] as AttributionIssues;
  await seedRepositories(
    Object.fromEntries(
      storedAttributionRows.map((row) => [
        String(row.number),
        {
          head_repo: repo,
          head_sha: row.head,
          merged: row.merged,
          state: row.merged ? "closed" : "open",
          title: `${child} the stored ${row.state} pull request`,
        },
      ])
    )
  );
  for (const row of storedAttributionRows) {
    await sql(
      `insert into pull_request_heads
         (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url)
       values ('${repo}', ${row.number}, '${row.head}', 'legion/${child}', 'main', '${row.state}',
          '${parent}', 'title_body', 'the stored pull request #${row.number}',
          'https://github.com/${repo}/pull/${row.number}')`
    );
    note(
      `#${row.number} is stored ${row.state}, credited to ${parent} by title_body, with no texts`
    );
  }
}

/** The case of the phase k-attribution, after one pass: the issue and the source that each of
 *  the six rows records, and its texts; the moves that the pass appended; and what
 *  `dispatch attribution` and the pass's log line count. */
async function kAttributionAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const { bodied, child, linkedOverBranch, linkedOverTitle, parent } = readState()[
    "k-attribution"
  ] as AttributionIssues;
  const short = (commit: string) => commit.slice(0, 12);
  const wanted: (readonly [number, string])[] = [
    [41, `${child} by branch (open at ${short(commits.attributionBranchMoved)}, texts stored)`],
    [42, `${bodied} by title_body (open at ${short(commits.attributionBody)}, texts stored)`],
    [
      43,
      `${linkedOverBranch} by external_link (open at ${short(commits.attributionLinkOverBranch)}, texts stored)`,
    ],
    [
      44,
      `${linkedOverTitle} by external_link (open at ${short(commits.attributionLinkOverTitle)}, texts stored)`,
    ],
    ...storedAttributionRows.map(
      (row) =>
        [
          row.number,
          `${child} by title_body (${row.state} at ${short(row.head)}, texts stored)`,
        ] as const
    ),
  ];
  for (const [number, want] of wanted) {
    const seen = await storedHead(number);
    note(`#${number} records ${seen}`);
    if (seen !== want) return `#${number} records ${seen} after one pass, want ${want}`;
  }

  // The pass reads both stored rows and moves both, in an order of its own, so the moves are
  // compared sorted. The push of #41 moved no attribution, so the pass appended no event of it.
  const parentMoves = await attributionMoves(parent);
  for (const move of parentMoves) note(`${parent} records ${move}`);
  const childMoves = await attributionMoves(child);
  for (const move of childMoves) note(`${child} records ${move}`);
  const wantedMoves = storedAttributionRows.map(
    (row) => `#${row.number} ${parent} → ${child} by title_body`
  );
  if ([...parentMoves].sort().join("; ") !== wantedMoves.join("; ")) {
    return `${parent} records ${parentMoves.join("; ")}, want ${wantedMoves.join("; ")}`;
  }
  const wantedChild = [`#41 nobody → ${child} by branch`, ...wantedMoves];
  if ([...childMoves].sort().join("; ") !== wantedChild.join("; ")) {
    return `${child} records ${childMoves.join("; ")}, want ${wantedChild.join("; ")}`;
  }

  // The pass read the facts of the two stored rows: the open arm reads #45, which no read has
  // checked, before the facts line starts, so facts_left_before counts #46 alone. The pass leaves
  // no pull request with no facts, and none that a pass still reads.
  const line = attributionLines().at(-1) ?? {};
  note(
    `the pass logged facts_left_before=${line.facts_left_before} facts_left=${line.facts_left} reads_left=${line.reads_left} events=${line.events}`
  );
  if (line.facts_left !== "0" || line.reads_left !== "0") {
    return `the pass logged facts_left=${line.facts_left} and reads_left=${line.reads_left}, want 0 and 0`;
  }
  const counts = await attributionCounts();
  note(
    `dispatch attribution: ${counts.facts_left} pull request(s) wait for their facts, and a pass still reads ${counts.reads_left}`
  );
  return counts.facts_left === 0 && counts.reads_left === 0
    ? undefined
    : `dispatch attribution prints ${counts.facts_left} that wait for their facts and ${counts.reads_left} that a pass still reads, want 0 and 0`;
}

// --- a bare `-` given to a text flag -----------------------------------------------------------
//
// The three cases of dispatch://INFRA-2618. Each runs the `dispatch` command as a process against
// the run's own server (runDispatch) and judges each step by its exit code and what it printed.
// Their negative controls name another build's bin/dispatch.ts in
// VERIFICATION_RESULTS_E2E_DISPATCH_CLI (scripts/e2e/README.md, "Negative control").

/** The first text of R1 of dispatch://INFRA-2618/spec, which `dispatch result` prints for the
 *  reported call. Its example is the command's own help example, not a value of this run. It is
 *  written out here and not built with the code it checks. */
const reportedCallRefusal = `dispatch result was not called: 3 problems
- --run - sends the one character "-". --run-file <path> reads --run from a file, and --run-file - reads it from stdin. One flag of a call may read stdin.
- --observed - sends the one character "-". --observed-file <path> reads --observed from a file, and --observed-file - reads it from stdin. One flag of a call may read stdin.
- --negative-control - sends the one character "-". --negative-control-file <path> reads --negative-control from a file, and --negative-control-file - reads it from stdin. One flag of a call may read stdin.
- Example: dispatch result --issue DSP-1 --check k1 --role implementer --verdict pass --commit 1111111111111111111111111111111111111111`;

/** The second text of that requirement, which `dispatch issue --title -` prints, written out the
 *  same way. */
const titleRefusal = `dispatch issue was not called: 1 problem
- --title - sends the one character "-". --title-file <path> reads --title from a file, and --title-file - reads it from stdin. One flag of a call may read stdin.
- Example: dispatch issue --project DSP --title 'Native workspace'`;

/** The problem line that requirement gives for a bare `-` on the text flag `flag`, as a refusal
 *  lists it. */
function dashLine(flag: string): string {
  return `- ${flag} - sends the one character "-". ${flag}-file <path> reads ${flag} from a file, and ${flag}-file - reads it from stdin. One flag of a call may read stdin.`;
}

/** The lines a refusal the command printed lists between its `was not called` line and its
 *  `- Example:` line, or undefined when it printed no such pair. */
function problemLines(printed: string): string[] | undefined {
  const lines = printed.split("\n");
  const head = lines.findIndex((line) => line.includes(" was not called: "));
  const example = lines.findIndex((line) => line.startsWith("- Example: "));
  return head === -1 || example <= head ? undefined : lines.slice(head + 1, example);
}

/** The three text fields of a result, in the words a case prints and compares. */
function fieldsOf(result: Pick<CheckResult, "run" | "observed" | "negative_control">): string {
  return `run ${JSON.stringify(result.run)}, observed ${JSON.stringify(result.observed)}, negative_control ${JSON.stringify(result.negative_control)}`;
}

/** How many issues `project` holds, counted over every page of
 *  `GET /api/v1/issues?project=<key>&limit=&offset=`, up to the total the pages name. */
async function issueCount(project: string): Promise<number> {
  let counted = 0;
  let offset = 0;
  let total: number;
  do {
    const page = (await asSession(
      "GET",
      `/api/v1/issues?project=${encodeURIComponent(project)}&limit=${MAX_ISSUE_PAGE_LIMIT}&offset=${offset}`
    )) as IssueSummaryPage;
    counted += page.issues?.length ?? 0;
    total = page.total;
    offset += MAX_ISSUE_PAGE_LIMIT;
  } while (offset < total);
  return counted;
}

/** The bytes of one version of an issue's uploaded file, as
 *  `GET /api/v1/issues/{key}/artifacts/{slug}/versions/{n}` serves them: under the file's own
 *  content type, else `application/octet-stream`. `call` parses JSON, so it cannot read them. */
async function artifactBytes(key: string, slug: string, version: number): Promise<Uint8Array> {
  const path = issuePath(key, `/artifacts/${encodeURIComponent(slug)}/versions/${version}`);
  const response = await fetch(new URL(path, dispatchUrl), {
    headers: { Authorization: `Bearer ${agentToken}` },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return new Uint8Array(await response.arrayBuffer());
}

// k-refused. The reported call and the other forms of a bare `-` are refused before any request:
// none of them leaves a result, a comment, a message or an issue. The calls the refusal leads to
// still record what a file and stdin hold, and `--path -` still uploads a local file named `-`.
await runCase("dash-refused", async () => {
  const issue = await seedIssue(
    "VRF",
    "A bare - given to a text flag is refused",
    checkFor(
      "k1",
      "The command refuses a bare - given to a text flag, and records what a file or stdin holds."
    )
  );
  note(`${issue} holds the before_merge check k1`);
  const result = resultArgv(issue, "k1", commits.dashRefused);
  // The here-document the reported call put its evidence in.
  const hereDocument = "bun test packages/envoy-client\n412 pass\n0 fail\n";

  // Step 1, the reported call. A command that takes it records a result with `-` in each of the
  // three fields, which the failure names.
  const reported = [...result, "--run", "-", "--observed", "-", "--negative-control", "-"];
  const first = await runDispatch(reported, { stdin: hereDocument });
  if (first.code === 0) {
    const stored = ((await checkOf(issue, "k1")).results ?? []).map(fieldsOf);
    return stepFailed(1, reported, first, 2, `k1 stores ${stored.join("; ") || "no result"}`);
  }
  if (first.code !== 2 || first.stdout !== `${reportedCallRefusal}\n` || first.stderr !== "") {
    return stepFailed(
      1,
      reported,
      first,
      2,
      "stdout must be the first text of R1, and stderr must be empty"
    );
  }
  note("step 1, the reported call: exit 2, the first text of R1 exactly, and nothing on stderr");

  // Steps 2 to 5: the other form of the flag, a dry run, and two other commands.
  for (const [step, argv, flag] of [
    [2, [...result, "--run=-"], "--run"],
    [3, [...result, "--observed", "-", "--dry-run"], "--observed"],
    [4, ["comment", "--issue", issue, "--body", "-"], "--body"],
    [5, ["message", "--issue", issue, "--body", "-"], "--body"],
  ] as const) {
    const run = await runDispatch(argv);
    const problems = problemLines(run.stdout);
    if (
      run.code !== 2 ||
      problems?.length !== 1 ||
      problems[0] !== dashLine(flag) ||
      run.stdout.split("\n").some((line) => line.startsWith("- Allowed flags:"))
    ) {
      return stepFailed(
        step,
        argv,
        run,
        2,
        `the refusal must list the one problem line for ${flag}, and no Allowed flags line`
      );
    }
    note(`step ${step}, ${commandText(argv)}: exit 2, the one problem line for ${flag}`);
  }

  // Step 6, which would create an issue: the project holds as many issues after it as before.
  const before = await issueCount("VRF");
  const titled = ["issue", "--project", "VRF", "--title", "-"];
  const sixth = await runDispatch(titled);
  const after = await issueCount("VRF");
  if (sixth.code !== 2 || sixth.stdout !== `${titleRefusal}\n` || after !== before) {
    return stepFailed(
      6,
      titled,
      sixth,
      2,
      `stdout must be the second text of R1, and VRF holds ${before} issues before the call and ${after} after it`
    );
  }
  note(
    `step 6, dispatch issue --project VRF --title -: exit 2, the second text of R1 exactly, and VRF holds ${after} issues before and after it`
  );

  const held = ((await checkOf(issue, "k1")).results ?? []).length;
  // Go writes an empty list as null. Anything else that is not a list is no answer to count, so a
  // read whose shape moved fails here rather than reading as none.
  const listed = async (rest: string): Promise<unknown[]> => {
    const answer = await asSession("GET", issuePath(issue, rest));
    if (answer === null) return [];
    if (!Array.isArray(answer)) throw new Error(`GET ${rest} of ${issue} answered no list`);
    return answer;
  };
  const comments = await listed("/comments");
  const messages = await listed("/messages");
  const left = `${held} result(s) under k1, ${comments.length} comment(s) and ${messages.length} message(s)`;
  note(`after steps 1 to 6, ${issue} holds ${left}`);
  if (held + comments.length + messages.length > 0) {
    return `after steps 1 to 6, ${issue} holds ${left}, want none`;
  }

  const scratch = mkdtempSync(join(tmpdir(), "verification-results-dash-"));
  try {
    // Step 7, the call the refusal of step 1 leads to: two files and the here-document.
    const runText = "bun test packages/envoy-client/src/__tests__/dispatch-command.test.ts\n";
    const controlText =
      "The command built before the refusal takes the reported call and stores -.\n";
    const runFile = join(scratch, "run.txt");
    const controlFile = join(scratch, "negative-control.txt");
    writeFileSync(runFile, runText);
    writeFileSync(controlFile, controlText);
    const led = [
      ...result,
      "--run-file",
      runFile,
      "--observed-file",
      "-",
      "--negative-control-file",
      controlFile,
    ];
    const seventh = await runDispatch(led, { stdin: hereDocument });
    if (seventh.code !== 0) return stepFailed(7, led, seventh, 0);
    const stored = (await checkOf(issue, "k1")).results ?? [];
    note(
      `step 7, the call the refusal leads to: exit 0, and k1 holds ${stored.length} result(s): ${stored.map(fieldsOf).join("; ")}`
    );
    const wanted = { negative_control: controlText, observed: hereDocument, run: runText };
    const [recorded] = stored;
    if (
      stored.length !== 1 ||
      recorded?.run !== wanted.run ||
      recorded.observed !== wanted.observed ||
      recorded.negative_control !== wanted.negative_control
    ) {
      return stepFailed(7, led, seventh, 0, `k1 must hold one result, with ${fieldsOf(wanted)}`);
    }

    // Step 8: `--path` names a local file, so `--path -` uploads the file named `-` of the
    // directory the command runs in.
    const upload = join(scratch, "upload");
    mkdirSync(upload);
    const dashBytes = Buffer.from("The file named -, which dispatch artifact --path - uploads.\n");
    writeFileSync(join(upload, "-"), dashBytes);
    const uploaded = ["artifact", "--issue", issue, "--name", "dash.txt", "--path", "-"];
    const eighth = await runDispatch(uploaded, { cwd: upload });
    if (eighth.code !== 0) return stepFailed(8, uploaded, eighth, 0);
    // dash-txt is the slug Dispatch gives the name dash.txt (artifactSlug,
    // packages/envoy/internal/dispatch/api/artifacts.go).
    const bytes = Buffer.from(await artifactBytes(issue, "dash-txt", 1));
    note(
      `step 8, ${commandText(uploaded)} beside a file named -: exit 0, and dash.txt version 1 holds ${JSON.stringify(bytes.toString())}`
    );
    return bytes.equals(dashBytes)
      ? undefined
      : stepFailed(
          8,
          uploaded,
          eighth,
          0,
          `dash.txt version 1 must hold the bytes of the file named -, ${JSON.stringify(dashBytes.toString())}`
        );
  } finally {
    rmSync(scratch, { force: true, recursive: true });
  }
});

// k-every-flag. Every text flag the command's help lists refuses a bare `-`. The case finds the
// flags as a reader does, from `dispatch --help` and each command's own `--help`: each
// `--X-file <path>` row whose `--X` the help lists with a value other than `<json>`, which leaves
// the JSON flags out, and not `--path-file` of `dispatch artifact`, whose `--path` names a local
// file to upload. A dry run sends nothing. The case judges the line as well as the exit, since a
// dry run that lacks a required flag exits 2 too.
await runCase("dash-refused-every-flag", async () => {
  // The floors k-every-flag names: a help that lists fewer text flags calls fewer of them.
  const fewestCommands = 28;
  const fewestFlags = 112;
  let step = 0;
  const listing = ["--help"];
  step += 1;
  const listed = await runDispatch(listing);
  if (listed.code !== 0) return stepFailed(step, listing, listed, 0);
  const commands = listed.stdout
    .split("\n")
    .flatMap((line) => /^ {2}([a-z][a-z0-9-]*) {2,}\S/.exec(line)?.[1] ?? []);
  let commandsCalled = 0;
  let flagsCalled = 0;
  for (const command of commands) {
    const helping = [command, "--help"];
    step += 1;
    const help = await runDispatch(helping);
    if (help.code !== 0) return stepFailed(step, helping, help, 0);
    // Each flag row of the help, with its value's placeholder: `--run <text>`, `--force`.
    const rows = new Map(
      help.stdout.split("\n").flatMap((line) => {
        const row = /^ {2}(--[a-z0-9-]+)(?: (<[^>]*>))?$/.exec(line);
        return row === null ? [] : [[String(row[1]), row[2] ?? ""] as const];
      })
    );
    const called: string[] = [];
    for (const [fileFlag, placeholder] of rows) {
      if (placeholder !== "<path>" || !fileFlag.endsWith("-file")) continue;
      const flag = fileFlag.slice(0, -"-file".length);
      const value = rows.get(flag);
      if (value === undefined || value === "<json>") continue;
      if (command === "artifact" && fileFlag === "--path-file") continue;
      const argv = [command, flag, "-", "--dry-run"];
      step += 1;
      const run = await runDispatch(argv);
      const lined = run.stdout.split("\n").includes(dashLine(flag));
      if (run.code !== 2 || !lined) {
        return stepFailed(
          step,
          argv,
          run,
          2,
          lined ? undefined : `its output holds no problem line for ${flag}`
        );
      }
      called.push(flag);
    }
    if (called.length === 0) continue;
    note(`dispatch ${command}: ${called.join(", ")}`);
    commandsCalled += 1;
    flagsCalled += called.length;
  }
  note(
    `called ${flagsCalled} flags over ${commandsCalled} commands, of the ${commands.length} commands dispatch --help lists`
  );
  if (commandsCalled < fewestCommands || flagsCalled < fewestFlags) {
    return `called ${flagsCalled} flags over ${commandsCalled} commands, want at least ${fewestFlags} flags over ${fewestCommands} commands`;
  }
  return undefined;
});

// k-roundtrip. Every other value of a text flag reaches the stored result byte for byte: a value
// longer than `-` that starts with `-`, values that hold `|`, a value written
// `--<field>=<value>`, and a `-` that a file or stdin gives. A value that starts with `--` and is
// a word of its own still reads as a flag, as before.
await runCase("values-kept", async () => {
  const issue = await seedIssue(
    "VRF",
    "Every other value of a text flag is kept",
    [
      checkFor("v1", "A value that starts with - and holds |, and two lines of stdin, are kept."),
      checkFor("v2", "A value written --observed=<value>, and a - that a file holds, are kept."),
      checkFor("v3", "A - that stdin gives is kept."),
    ].join("\n")
  );
  note(`${issue} holds the before_merge checks v1, v2 and v3`);
  const run =
    "go test ./internal/workflow/ -run 'AppendOnly|Line|StartWrittenOverAnOwedRow' -count=1";
  const lines = "--- PASS: TestAppendOnly (0.00s)\nok\n";
  const control = "-run 'A|B' with the guard taken out: 2 tests fail";
  const passLine = "--- PASS: TestLine (0.00s)";
  const scratch = mkdtempSync(join(tmpdir(), "verification-results-kept-"));
  try {
    const dashFile = join(scratch, "dash");
    writeFileSync(dashFile, "-");
    // Steps 1 to 3, each with what its result must hold. A field a step does not send is empty.
    const steps = [
      {
        argv: [
          ...resultArgv(issue, "v1", commits.valuesKept),
          "--run",
          run,
          "--observed-file",
          "-",
          "--negative-control",
          control,
        ],
        check: "v1",
        stdin: lines,
        want: { negative_control: control, observed: lines, run },
      },
      {
        argv: [
          ...resultArgv(issue, "v2", commits.valuesKept),
          `--observed=${passLine}`,
          "--negative-control-file",
          dashFile,
        ],
        check: "v2",
        want: { negative_control: "-", observed: passLine, run: "" },
      },
      {
        argv: [...resultArgv(issue, "v3", commits.valuesKept), "--observed-file", "-"],
        check: "v3",
        stdin: "-",
        want: { negative_control: "", observed: "-", run: "" },
      },
    ];
    for (const [index, step] of steps.entries()) {
      const answered = await runDispatch(step.argv, { stdin: step.stdin });
      if (answered.code !== 0) return stepFailed(index + 1, step.argv, answered, 0);
      note(
        `step ${index + 1}, ${commandText(step.argv)}${step.stdin === undefined ? "" : ` with ${JSON.stringify(step.stdin)} on stdin`}: exit 0`
      );
    }

    // Step 4: the value as a word of its own starts with `--`, so it reads as a flag.
    const split = [...resultArgv(issue, "v3", commits.valuesKept), "--observed", passLine];
    const fourth = await runDispatch(split);
    if (fourth.code !== 2 || !fourth.stdout.split("\n").includes("- --observed needs a value")) {
      return stepFailed(
        4,
        split,
        fourth,
        2,
        "the output must hold the line - --observed needs a value"
      );
    }
    note(`step 4, ${commandText(split)}: exit 2, - --observed needs a value`);

    // Step 5: each stored field, byte for byte.
    const checks = await checksOf(issue);
    for (const [index, step] of steps.entries()) {
      const stored = checks.find((check) => check.block_id === step.check)?.results ?? [];
      note(
        `step 5: ${step.check} holds ${stored.length} result(s): ${stored.map(fieldsOf).join("; ")}`
      );
      const [result] = stored;
      if (stored.length !== 1 || result === undefined) {
        return `step 5: ${step.check} holds ${stored.length} results, want the one step ${index + 1} recorded`;
      }
      for (const field of ["run", "observed", "negative_control"] as const) {
        if (result[field] !== step.want[field]) {
          return `step 5: the ${field} of ${step.check}'s result reads ${JSON.stringify(result[field])}, want ${JSON.stringify(step.want[field])}, the bytes step ${index + 1} gave`;
        }
      }
    }
    return undefined;
  } finally {
    rmSync(scratch, { force: true, recursive: true });
  }
});

summarise();
