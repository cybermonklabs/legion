// The driver of verification-results.sh: it drives a real envoy-dispatch over HTTP as one session
// (the run's agent token, each write naming the session as `actor`) and one person (the dev
// sign-in cookie), seeds the run's fake GitHub, publishes each pull-request event as the
// normalized `pull_request` envelope on the run's own NATS, the one path Dispatch's delivery
// intake reads, and reads two of its answers back through the `dispatch results` command as well
// as through the API. Three more cases run the `dispatch` command as a process against the same
// server: a bare `-` given to a text flag is refused, on every text flag the command's help
// lists, and every other value of a text flag is kept byte for byte.
//
// The shell script runs the driver again for each one-pass phase (onePassPhases), in up to four
// halves: `--phase <phase>-prepare` while the server of the step before still runs, with no pass
// to come; `--phase <phase>-fixture` while no server runs; `--phase <phase>-before` through a
// server that started with no pass, where the shell asks for it; and `--phase <phase>-assert`
// after it starts the server again with a start pass and an interval of one hour. That is the
// boundary of one complete pass (prepareOnePass, awaitOnePass). Before its first case the driver
// checks its counter of pull request reads, and the phase `boundary` checks the boundary of one
// pass. older-rows has two halves more, which run first, through the release before's build:
// `--phase older-rows-release-writes` and `--phase older-rows-release-pass` (the section
// "older-rows").
//
// The seven checks of the attribution run as such phases, in the project ATT and its repository
// acme/attribution-live, in this order (the section "the seven checks"):
//   older-rows             300 rows that the release before's build wrote hold their stored
//                          issue before any pass, and one pass reads each of them and leaves none.
//   rule-reaches-each-row  the rule reaches 5,000 rows with texts at the read, with no read of
//                          GitHub: after a person's link, a move of an issue to another
//                          repository and a new issue that a stored branch names, too.
//   key-reading-changes    a second key reading reaches 5,000 pull requests that the server under
//                          test stored, with no read of GitHub.
//   facts-only             no writer takes a fact from the envelope.
//   read-compares          a results read compares with the pull request that it learns, and says
//                          what it does not know yet.
//   stores-agree           the timeline and the results routes name one issue for each pull
//                          request.
//   limit-of-one           the rate limit of one installation ends no step for another.
//
//   VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> \
//     VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> \
//     VERIFICATION_RESULTS_E2E_DISPATCH_LOG=<the server's log> \
//     VERIFICATION_RESULTS_E2E_STATE=<a JSON file> \
//     [VERIFICATION_RESULTS_E2E_DISPATCH_CLI=<bin/dispatch.ts>] \
//     [VERIFICATION_RESULTS_E2E_ONLY=<case>] [VERIFICATION_RESULTS_E2E_OLD_BUILD=1] \
//     bun scripts/e2e/verification-results.ts \
//       [--phase <phase>-prepare|<phase>-fixture|<phase>-before|<phase>-assert|older-rows-release-writes|older-rows-release-pass] \
//       <dispatch-url> <nats-url> <fake-github-url>
//
// The `dispatch` command is the bin/dispatch.ts VERIFICATION_RESULTS_E2E_DISPATCH_CLI names, an
// absolute path the shell script made, else this checkout's packages/envoy-client/bin/dispatch.ts.
// The log is the file every start of the server writes to, which attributionLines reads. The state
// file holds what one process of the driver leaves for a later one: the issues a case seeded, and
// the length of the log at each phase's fixture. Only a negative control's command sets the last
// two variables: VERIFICATION_RESULTS_E2E_ONLY runs one case of the seven checks alone, and
// VERIFICATION_RESULTS_E2E_OLD_BUILD=1 names a server built before the attribution line of a pass.
//
// The database URL is the run's own, which the shell script resets before the server migrates it.
// The driver writes SQL for the fixtures that no route of Dispatch can write: stored pull request
// rows as the release before writes them, with no texts; texts rows as a read of this build
// writes them; timeline rows, owed reads and partial rows; a second key reading
// (fixtures/attribution_reading_b.sql); and the delete of the reconcile's records of its passes at
// each phase's boundary. It writes no row of older-rows and no row of key-reading-changes' own
// 5,000 pull requests: the servers store those from events, passes and the API. It reads SQL for
// the answers that no route gives: the issue and the source a row records, its texts and branch
// keys, the delivery rows of limit-of-one, the records of the passes and whether a process holds
// the reconcile's lease.
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
// Until stores-agree, the run writes no delivery settings, as a deployment that never configured
// the delivery timeline holds none: the intake writes a pull request's head row from its envelope
// all the same, and every head of the cases before the phases reaches Dispatch that way.
// stores-agree and limit-of-one write the settings their passes need. older-rows writes them
// through the release before's build, whose intake takes no event without them, on a database the
// shell empties before the cases.
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
/** Two settings that only a negative control's command gives. VERIFICATION_RESULTS_E2E_ONLY names
 *  one case of the seven checks: the driver then runs that case alone, with the writes of the case
 *  it stands on (onePassPhases), and skips every other case. VERIFICATION_RESULTS_E2E_OLD_BUILD=1
 *  says the server is a build before the attribution line of a pass, so the boundary of one pass
 *  does not count that line (awaitOnePass). A run of the head sets neither. */
const onlyCase = process.env.VERIFICATION_RESULTS_E2E_ONLY ?? "";
const oldBuild = process.env.VERIFICATION_RESULTS_E2E_OLD_BUILD === "1";
/** The exit status of a half of a one-pass phase that VERIFICATION_RESULTS_E2E_ONLY skips, which
 *  the shell's `driver` reads as a skip: 0 is a pass, 1 a failed case, 2 a step outside the cases. */
const phaseSkipped = 3;
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
    "usage: VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> VERIFICATION_RESULTS_E2E_DISPATCH_LOG=<log> VERIFICATION_RESULTS_E2E_STATE=<file> [VERIFICATION_RESULTS_E2E_ONLY=<case>] [VERIFICATION_RESULTS_E2E_OLD_BUILD=1] bun verification-results.ts [--phase <phase>-prepare|<phase>-fixture|<phase>-before|<phase>-assert|older-rows-release-writes|older-rows-release-pass] <dispatch-url> <nats-url> <fake-github-url>\n"
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

// --- the seven checks: names, repositories and commits -------------------------------------------
//
// The seven checks of the attribution (the section "the seven checks" below) run in a project of
// their own, ATT. Its default repository, acme/attribution-live, is on an installation of its
// own. So the fake's count of the reads of that repository counts the reads of the cases' pull
// requests and no other reads (caseReads). Each pass also reads again the open pull requests that
// the earlier cases left in acme/verification-live, once their last read is five minutes old
// (delivery.PullRequestHeadStaleAfter).

/** The seven checks, in the order the shell runs them. */
const sevenChecks = [
  "older-rows",
  "rule-reaches-each-row",
  "key-reading-changes",
  "facts-only",
  "read-compares",
  "stores-agree",
  "limit-of-one",
] as const;
type SevenCheck = (typeof sevenChecks)[number];

/** The project of the seven checks, and its repositories: the default one; a second one, which
 *  an issue of rule-reaches-each-row moves to; and the name the default one had before a rename,
 *  which older-rows stores rows under. The fake holds no repository of that old name, so its
 *  installation lookup answers 404, as GitHub answers for a renamed repository's old name. */
const attributionProject = "ATT";
const attributionRepo = "acme/attribution-live";
const attributionSecondRepo = "acme/attribution-second";
const attributionOldName = "acme/old-name";
const attributionInstallation = 259;
/** The repositories of limit-of-one, on another installation: the one whose pull requests wait,
 *  and the deploy repository of its delivery settings. */
const betaRepo = "beta/tools";
const betaDeployRepo = "beta/deploy";
const betaInstallation = 258;
/** The deploy repository of older-rows' delivery settings, on the installation of the seven
 *  checks: the release before's intake takes no event without settings. */
const olderRowsDeployRepo = "acme/attribution-deploy";
/** The repositories whose reads of pull requests a case of the seven checks counts. */
const attributionRepos = [attributionRepo, attributionSecondRepo, attributionOldName];

/** GitHub's update time of each pull request of older-rows and rule-reaches-each-row. The rows
 *  that the release before stored hold an earlier event_at, so a read's answer replaces them
 *  (upsertHeadRow). */
const attributionUpdatedAt = "2026-11-10T00:00:00Z";
const releaseBeforeEventAt = "2026-11-09T00:00:00Z";
/** The first and the last of key-reading-changes' own pull requests, which the server under test
 *  stores (the section "key-reading-changes"). */
const readingFirst = 20001;
const readingLast = 25000;

/** The head commit of pull request `number` of the seven checks: its number in hex, padded to 40
 *  characters with `pad`. Another pad gives another head of the same pull request. */
function attributionSha(number: number, pad = "f"): string {
  return number.toString(16).padStart(40, pad);
}

/** The commits a case of the seven checks records a result at. The record route refuses a commit
 *  that the repository does not hold, so the repository's seed holds each one from the start. */
const sevenCommits = {
  /** The head of facts-only's open pull request. */
  factsOnlyHead: "f1".repeat(20),
  /** An earlier commit of that pull request, which facts-only records a result at. */
  factsOnlyEarly: "f2".repeat(20),
  /** read-compares: the head of R1's pull request, and an earlier commit of it. */
  compareHead: "b1".repeat(20),
  compareEarly: "b2".repeat(20),
  /** read-compares: the head of the pull request whose answer is not known. */
  notKnownHead: "b3".repeat(20),
};

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
  return publishEnvelope(payload);
}

/** Publishes the envelope of `payload` on the subject of its repository and number, as
 *  publishPullRequest does, with no seed of the fake: a case that sends an envelope that differs
 *  from GitHub's answer seeds that answer itself. */
async function publishEnvelope(payload: Record<string, string>): Promise<Published> {
  const [owner, name] = String(payload.repo).split("/");
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

/** Runs one case and prints its `ok` or `FAIL` line. Under VERIFICATION_RESULTS_E2E_ONLY the run
 *  of the cases before the phases (no --phase) skips each case, since a control of one case of the
 *  seven checks needs none of them. A phase decides on its own which of its halves run. */
async function runCase(name: string, body: () => Promise<string | undefined>): Promise<void> {
  if (onlyCase !== "" && phaseArg === undefined) {
    console.log(`skip ${name}: VERIFICATION_RESULTS_E2E_ONLY=${onlyCase} runs that case alone`);
    return;
  }
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
 *  routes: the read of one pull request, the read of its commits, the read of the pull requests of
 *  a commit, and each pull request that a GraphQL query asks for. It sums every repository, or the
 *  `repositories` given, by the name each request used. */
async function pullReads(repositories?: readonly string[]): Promise<number> {
  const response = await fetch(new URL("/__fixture/requests", githubUrl));
  if (!response.ok) {
    throw new Error(`GET /__fixture/requests: ${response.status} ${await response.text()}`);
  }
  const { pulls } = (await response.json()) as { pulls: Record<string, Record<string, number>> };
  return Object.entries(pulls)
    .filter(([name]) => repositories === undefined || repositories.includes(name))
    .flatMap(([, routes]) => Object.values(routes))
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
 *  A control of a build before that line (VERIFICATION_RESULTS_E2E_OLD_BUILD=1) writes no such
 *  line, so for it the boundary rests on the records and the lease alone, and so does the pass
 *  of the release before's build that older-rows waits for (`countLines` false). A log with no
 *  such line still fails a run that does not give that setting. Every phase's answer rests on this
 *  boundary, so any other reading stops the driver (setupFailed). It answers what it read. */
async function awaitOnePass(linesBefore: number, countLines = !oldBuild): Promise<string> {
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
    (lines !== 1 && countLines)
  ) {
    setupFailed(
      "the boundary of one pass",
      `duty_passes holds ${shown}, want ${reconcileLease} and ${reconcileLease}/${onePassPeriodNs} at one boundary, each of period ${onePassPeriodNs} ns; the lease is ${leaseHeld ? "still held" : "free"}; the log holds ${lines} more attribution line(s) than at the fixture, want ${countLines ? "1" : "any (a build before that line)"}`
    );
  }
  return countLines
    ? `one pass: duty_passes holds ${shown}; the lease is free; the log holds 1 more attribution line`
    : `one pass: duty_passes holds ${shown}; the lease is free; the log holds ${lines} more attribution line(s), not counted: the server is a build before that line`;
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
  const [links] = line(
    /^Links that more than one issue holds in other letter cases, or a pull request in its other form: ([\d,]+)\.$/m
  );
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

/** One phase that the shell runs around one complete pass (one_pass_phase), in up to four halves:
 *  `prepare`, the writes that need a server, while the server of the step before still runs and
 *  makes no pass; `fixture`, the writes while no server runs; `before`, the reads through a server
 *  that started with no pass, where the shell asks for them; and `assert`, the case after the
 *  start pass, given what awaitOnePass read. `prepare`, `before` and `assert` are parts of the case
 *  `caseName`. `baseOf` names the cases of the seven checks that stand on this phase's writes:
 *  under VERIFICATION_RESULTS_E2E_ONLY, the phase writes for them and asserts nothing.
 *  `releaseWrites` and `releasePass` are older-rows' two halves through the release before's build
 *  (the shell's older_rows_upgrade), which run before every other. */
interface OnePassPhase {
  readonly assert: (pass: string) => Promise<string | undefined>;
  readonly baseOf?: readonly SevenCheck[];
  readonly before?: () => Promise<string | undefined>;
  readonly caseName: string;
  readonly fixture?: () => Promise<void>;
  readonly prepare?: () => Promise<string | undefined>;
  readonly releasePass?: () => Promise<string | undefined>;
  readonly releaseWrites?: () => Promise<string | undefined>;
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
//
// The seven checks add the repositories of their own project, acme/attribution-live and
// acme/attribution-second, and limit-of-one adds beta/tools and beta/deploy
// (attributionRepositories). Each seed sends them again too.

/** One pull request the fake answers for, as its single-pull-request route reads a fixture:
 *  the head commit and branch, the repository the head branch is in, the base branch, the
 *  state, the title and body, the time GitHub last updated it, and the merge's own commit; and
 *  as its commits read and its GraphQL route read it: the messages of its commits, and the status
 *  the commits read answers in place of them. limit-of-one sets two more: `searched`, so the
 *  merged search finds the pull request, and `author`, which also makes the fake answer the
 *  fields that the completion of a partial pull request reads. */
interface FakePullRequest {
  readonly author?: string;
  readonly base_ref?: string;
  readonly body?: string;
  readonly commit_messages?: readonly string[];
  readonly commits_status?: number;
  readonly head_ref?: string;
  readonly head_repo: string;
  readonly head_sha: string;
  readonly merge_commit_sha?: string;
  readonly merged: boolean;
  readonly searched?: boolean;
  readonly state: string;
  readonly title: string;
  readonly updated_at?: string;
}

/** One repository of the seven checks, as the fake's fixture route reads it: the installation
 *  and its permissions, the default branch, the commits it holds, the pull requests of each
 *  commit (`commit_pulls`), its pull requests, and whether its installation spent its rate limit. */
interface FakeRepository {
  readonly actions?: string;
  readonly commit_pulls?: Record<string, readonly number[]>;
  readonly commits?: readonly string[];
  readonly contents: string;
  readonly default_branch?: string;
  readonly installation_id: number;
  readonly pull_requests?: string;
  readonly pulls?: Record<string, FakePullRequest>;
  readonly rate_limited?: boolean;
}

/** Every pull request answer set so far, keyed by number. */
const githubPulls: Record<string, FakePullRequest> = {};
/** Every repository of the seven checks, as a case seeded it last, keyed by its name. */
const attributionRepositories: Record<string, FakeRepository> = {};

/** Seeds the fake: the proof's repository with every pull request answer set so far and `pulls`,
 *  the repository the App is not installed on, and each repository of the seven checks, with
 *  `repositories` in place of the ones it names. */
async function seedRepositories(
  pulls: Record<string, FakePullRequest>,
  repositories: Record<string, FakeRepository> = {}
): Promise<void> {
  Object.assign(githubPulls, pulls);
  Object.assign(attributionRepositories, repositories);
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
      ...attributionRepositories,
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  }).catch((error: unknown) => setupFailed("fake GitHub seed", error));
  if (!seeded.ok) setupFailed("fake GitHub seed", `${seeded.status} ${await seeded.text()}`);
}

/** Seeds the repository `name` of the seven checks again, as `change` makes it from its last
 *  seed, and every other repository as it was. */
async function seedAttribution(
  name: string,
  change: (seeded: FakeRepository) => FakeRepository
): Promise<void> {
  const seeded = attributionRepositories[name];
  if (seeded === undefined) throw new Error(`the fake holds no seed of ${name}`);
  await seedRepositories({}, { [name]: change(seeded) });
}

// One half of a one-pass phase (OnePassPhase). The process first takes the repositories the fake
// answers for, as an earlier process of the driver seeded them, so a seed of this process keeps
// them.
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
    // The seven checks (the section "the seven checks"), in the order the shell runs them.
    "older-rows": {
      assert: olderRowsAfterOnePass,
      before: olderRowsBeforeThePass,
      caseName: "older-rows",
      fixture: resetPullReads,
      releasePass: olderRowsReleasePass,
      releaseWrites: olderRowsReleaseWrites,
    },
    "older-rows-set-back": {
      assert: olderRowsAfterTheSetBack,
      caseName: "older-rows",
      fixture: olderRowsSetBack,
    },
    "rule-reaches-each-row": {
      assert: (pass) => ruleAtTheBoundary(pass, "the rows of the release before"),
      baseOf: ["key-reading-changes", "stores-agree"],
      caseName: "rule-reaches-each-row",
      fixture: ruleFixture,
      prepare: ruleIssues,
    },
    "rule-reaches-each-row-link": {
      assert: (pass) => ruleAtTheBoundary(pass, "a person's link"),
      caseName: "rule-reaches-each-row",
      fixture: resetPullReads,
      prepare: ruleLink,
    },
    "rule-reaches-each-row-move": {
      assert: (pass) => ruleAtTheBoundary(pass, "a move to the second repository"),
      caseName: "rule-reaches-each-row",
      fixture: resetPullReads,
      prepare: ruleMove,
    },
    "rule-reaches-each-row-new-issue": {
      assert: ruleNewIssue,
      caseName: "rule-reaches-each-row",
      fixture: ruleNewIssuePull,
      prepare: ruleProbe,
    },
    "key-reading-changes": {
      assert: readingAfterOnePass,
      caseName: "key-reading-changes",
      fixture: readingFixture,
      prepare: readingStored,
    },
    "facts-only": {
      assert: factsOnlyAfterOnePass,
      caseName: "facts-only",
      fixture: factsOnlyFixture,
      prepare: factsOnlyEnvelope,
    },
    "read-compares": {
      assert: readComparesAfterOnePass,
      caseName: "read-compares",
      fixture: readComparesFixture,
      prepare: readComparesFirstReads,
    },
    "stores-agree": {
      assert: (pass) => storesAgreeRead(pass, "after one pass"),
      before: () => storesAgreeRead(undefined, "before a pass"),
      caseName: "stores-agree",
      fixture: storesAgreeFixture,
      prepare: storesAgreeSettings,
    },
    "stores-agree-link": {
      assert: (pass) => storesAgreeRead(pass, "after a person's link and one more pass"),
      caseName: "stores-agree",
      prepare: storesAgreeLink,
    },
    "limit-of-one": {
      assert: limitAfterOnePass,
      caseName: "limit-of-one",
      fixture: limitFixture,
      prepare: limitSettings,
    },
  };
  const [, name = "", half] =
    /^(.+)-(prepare|fixture|before|assert|release-writes|release-pass)$/.exec(phaseArg) ?? [];
  const phase = onePassPhases[name];
  if (phase === undefined || half === undefined) {
    setupFailed(
      `phase ${phaseArg}`,
      `no one-pass phase answers --phase ${phaseArg}; the phases are ${Object.keys(onePassPhases).join(", ")}, each with -prepare, -fixture, -before or -assert, and older-rows with -release-writes and -release-pass too`
    );
  }
  if (onlyCase !== "" && !sevenChecks.some((check) => check === onlyCase)) {
    setupFailed(
      "VERIFICATION_RESULTS_E2E_ONLY",
      `${onlyCase} is no case of the seven checks (${sevenChecks.join(", ")})`
    );
  }
  // Under VERIFICATION_RESULTS_E2E_ONLY, a phase of that case runs whole, and so does the
  // self-check of the boundary. A phase that the case stands on makes its writes and asserts
  // nothing. The driver skips every other phase. A skipped half exits with phaseSkipped, so the
  // shell prints `skip <phase>` for the phase and never `ok`.
  const runs =
    onlyCase === "" || phase.caseName === onlyCase || name === "boundary"
      ? "whole"
      : phase.baseOf?.some((check) => check === onlyCase)
        ? "writes"
        : "none";
  if (runs === "none" || (runs === "writes" && (half === "before" || half === "assert"))) {
    console.log(
      `skip ${phase.caseName} (--phase ${phaseArg}): VERIFICATION_RESULTS_E2E_ONLY=${onlyCase}`
    );
    void nats.close();
    process.exit(phaseSkipped);
  }
  const seeded = await fetch(new URL("/__fixture/repos", githubUrl)).catch((error: unknown) =>
    setupFailed("read of the fake GitHub's seed", error)
  );
  if (!seeded.ok)
    setupFailed("read of the fake GitHub's seed", `${seeded.status} ${await seeded.text()}`);
  const repositories = (await seeded.json()) as Record<string, FakeRepository>;
  for (const [fullName, repository] of Object.entries(repositories)) {
    if (fullName === repo) Object.assign(githubPulls, repository.pulls ?? {});
    else if (fullName !== hiddenRepo) attributionRepositories[fullName] = repository;
  }
  if (half === "prepare") {
    if (phase.prepare !== undefined && runs === "whole") {
      await runCase(phase.caseName, phase.prepare);
      summarise();
    }
    if (phase.prepare !== undefined) {
      const step = `writes of ${phase.caseName}, which VERIFICATION_RESULTS_E2E_ONLY=${onlyCase} stands on`;
      const failure = await phase.prepare().catch((error: unknown) => setupFailed(step, error));
      if (failure !== undefined) setupFailed(step, failure);
    }
    void nats.close();
    process.exit(0);
  }
  if (half === "release-writes" || half === "release-pass") {
    const step = half === "release-writes" ? phase.releaseWrites : phase.releasePass;
    if (step === undefined) {
      setupFailed(
        `phase ${phaseArg}`,
        `the phase ${name} has no half ${half}: only older-rows runs on the release before`
      );
    }
    await runCase(phase.caseName, step);
    summarise();
  }
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
  if (half === "before") {
    const before = phase.before;
    if (before === undefined) {
      setupFailed(`phase ${phaseArg}`, `the phase ${name} reads nothing before its pass`);
    }
    await runCase(phase.caseName, before);
    // The read is one before any pass only while no pass has run: the server started with no
    // pass, and its first tick can still fall within the read.
    const passes = await sql(`select count(*) from duty_passes where ${reconcileRecords}`);
    if (passes !== "0") {
      setupFailed(
        "the read before the pass",
        `duty_passes holds ${passes} record(s) of the reconcile: a pass ran before the read ended`
      );
    }
    summarise();
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
// The repositories of the seven checks: the project's default one holds the commits a case
// records at, and its second one holds nothing. stores-agree gives the first the permissions a
// pass with delivery settings checks.
await seedRepositories(
  {},
  {
    [attributionRepo]: {
      commits: Object.values(sevenCommits),
      contents: "read",
      default_branch: "main",
      installation_id: attributionInstallation,
      pulls: {},
    },
    [attributionSecondRepo]: {
      contents: "read",
      default_branch: "main",
      installation_id: attributionInstallation,
    },
  }
);

for (const [key, name, mapped] of [
  ["VRF", "Verification results", repo],
  ["VRH", "Verification results, unreadable repository", hiddenRepo],
  [attributionProject, "Attribution", attributionRepo],
] as const) {
  await asPerson("POST", "/api/v1/projects", { key, name }).catch((error: unknown) =>
    setupFailed(`project ${key}`, error)
  );
  await asSession("PUT", `/api/v1/settings/repo-projects/${mapped}`, {
    default: true,
    project: key,
  }).catch((error: unknown) => setupFailed(`repository mapping ${mapped}`, error));
}
// The project of the seven checks maps two more repositories: a second one, which an issue can
// move to, and the name its default repository had before a rename.
for (const [mapped, mapping] of [
  [attributionSecondRepo, { project: attributionProject }],
  [attributionOldName, { project: attributionProject, renamed_to: attributionRepo }],
] as const) {
  await asSession("PUT", `/api/v1/settings/repo-projects/${mapped}`, mapping).catch(
    (error: unknown) => setupFailed(`repository mapping ${mapped}`, error)
  );
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
if (results.some((result) => result.name === "the-results-route-is-served" && !result.ok)) {
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

// --- the seven checks ----------------------------------------------------------------------------
//
// Seven cases of the attribution, each driven through the server, the `dispatch` command and the
// fake GitHub, each around one complete pass (onePassPhases): older-rows, rule-reaches-each-row,
// key-reading-changes, facts-only, read-compares, stores-agree and limit-of-one. Their issues are
// in the project ATT (attributionProject), and their names in the comments (L1, B7, T7, F2, R2,
// S3, ...) stand for the keys the server gave them, which the state file holds (sevenState). Each
// case that wants no read of GitHub runs before stores-agree writes the delivery settings, since a
// pass with settings reads GitHub for its other steps.

/** What the seven checks leave for a later process of the driver, in the state file. */
interface SevenChecksState {
  /** older-rows: the keys of L0 to L5, N0 to N5 and D0 to D5. */
  readonly olderRows?: {
    readonly D: readonly string[];
    readonly L: readonly string[];
    readonly N: readonly string[];
  };
  /** rule-reaches-each-row: the keys of B0 to B99 and T0 to T99, and the steps made so far. */
  readonly rule?: {
    readonly B: readonly string[];
    readonly T: readonly string[];
    readonly linked?: boolean;
    readonly moved?: boolean;
    readonly newIssue?: string;
    readonly nextKey?: string;
  };
  /** key-reading-changes: the reading `dispatch attribution` printed before the change, and how
   *  many lines `dispatch delivery: keys remade` the log held then. */
  readonly reading?: { readonly recorded: string; readonly remadeLines?: number };
  /** facts-only: the keys of F1 and F2, and the checked_at the fixture moved #16001 back to. */
  readonly factsOnly?: { readonly F1: string; readonly F2: string; readonly movedTo?: string };
  /** read-compares: the keys of R1 and R2. */
  readonly readCompares?: { readonly R1: string; readonly R2: string };
  /** stores-agree: the keys of S1, S2 and S3. */
  readonly storesAgree?: { readonly S1: string; readonly S2: string; readonly S3: string };
}

function sevenState(): SevenChecksState {
  return (readState()["seven-checks"] ?? {}) as SevenChecksState;
}

/** Writes `change` over what the seven checks left in the state file. */
function writeSevenState(change: SevenChecksState): void {
  writeFileSync(
    statePath,
    JSON.stringify({ ...readState(), "seven-checks": { ...sevenState(), ...change } })
  );
}

/** The part `key` of what the seven checks left, which an earlier phase wrote. */
function sevenPart<K extends keyof SevenChecksState>(key: K): NonNullable<SevenChecksState[K]> {
  const part = sevenState()[key];
  if (part === undefined) throw new Error(`the state file holds no ${key}: its phase did not run`);
  return part as NonNullable<SevenChecksState[K]>;
}

/** Seeds one issue of the project ATT for each of `names`, ten at a time, each with one
 *  `before_merge` check `attribution`, and answers their keys in the order of `names`. */
async function seedAttributionIssues(names: readonly string[]): Promise<string[]> {
  const keys: string[] = [];
  for (let at = 0; at < names.length; at += 10) {
    keys.push(
      ...(await Promise.all(
        names
          .slice(at, at + 10)
          .map((name) =>
            seedIssue(
              attributionProject,
              `The issue ${name} of the seven checks`,
              checkFor(
                "attribution",
                "The issue's results are compared with the pull requests the attribution gives it."
              )
            )
          )
      ))
    );
  }
  return keys;
}

/** What the check `attribution` of one issue names: the pull requests it compares with, those
 *  whose head Dispatch has not read, those whose answer is not known, and the reason it compares
 *  with nothing, empty where it compares with something. */
interface CheckPulls {
  readonly compared: readonly number[];
  readonly notKnown: readonly number[];
  readonly notRead: readonly number[];
  readonly nothing: string;
}

/** Reads GET /api/v1/issues/{key}/results for each of `keys`, ten at a time. */
async function checkPullsOf(keys: readonly string[]): Promise<Map<string, CheckPulls>> {
  const answers = new Map<string, CheckPulls>();
  for (let at = 0; at < keys.length; at += 10) {
    const chunk = keys.slice(at, at + 10);
    const checks = await Promise.all(chunk.map((key) => checkOf(key, "attribution")));
    chunk.forEach((key, index) => {
      const check = checks[index] as CheckBlock;
      answers.set(key, {
        compared: (check.compared_with ?? []).flatMap((basis) =>
          basis.pull_request === undefined ? [] : [basis.pull_request]
        ),
        notKnown: (check.issues_not_known ?? []).map((entry) => entry.number),
        notRead: (check.heads_not_read ?? []).map((head) => head.number),
        nothing: check.compared_nothing ?? "",
      });
    });
  }
  return answers;
}

/** Each issue of `answers` whose check names pull request number n, by n: through compared_with,
 *  and through heads_not_read where `withNotRead` is set. */
function holdersOf(answers: Map<string, CheckPulls>, withNotRead = false): Map<number, string[]> {
  const holders = new Map<number, string[]>();
  for (const [key, pulls] of answers) {
    for (const number of [...pulls.compared, ...(withNotRead ? pulls.notRead : [])]) {
      holders.set(number, [...(holders.get(number) ?? []), key]);
    }
  }
  return holders;
}

/** Each pull request from `first` to `last` whose issues in `answers` (by compared_with) are not
 *  the one issue that `want` gives it, or none where `want` gives undefined, in the words
 *  `#<n> under <issues>, want <issue>`. */
function ownersDiffer(
  answers: Map<string, CheckPulls>,
  first: number,
  last: number,
  want: (number: number) => string | undefined
): string[] {
  const holders = holdersOf(answers);
  const differ: string[] = [];
  for (let number = first; number <= last; number++) {
    const held = [...(holders.get(number) ?? [])].sort().join(" and ") || "no issue";
    const wanted = want(number) ?? "no issue";
    if (held !== wanted) differ.push(`#${number} under ${held}, want ${wanted}`);
  }
  return differ;
}

/** The reads of pull requests the fake answered for the repositories of the seven checks, and
 *  for every repository, in the words of a note. */
async function caseReads(): Promise<{ readonly count: number; readonly shown: string }> {
  const count = await pullReads(attributionRepos);
  const all = await pullReads();
  return {
    count,
    shown: `the fake answered ${count} read(s) of pull requests of ${attributionRepos.join(", ")} since the fixture, and ${all} of every repository (each pass reads again the open pull requests of ${repo} that the earlier cases left)`,
  };
}

/** The delivery settings a case writes through PUT /api/v1/settings/delivery, with `deployRepo` as
 *  the deploy repository, whose permissions each pass checks. */
function deliverySettings(deployRepo: string): Json {
  return {
    deploy_repo: deployRepo,
    deploy_workflow_path: ".github/workflows/deploy.yml",
    population_authors: ["octocat"],
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_names: ["deploy / deploy"],
  };
}

/** The SQL array of the quoted `keys`, which a statement indexes from 1. */
function sqlKeys(keys: readonly string[]): string {
  return `array[${keys.map((key) => `'${key}'`).join(", ")}]`;
}

// --- older-rows ----------------------------------------------------------------------------------
//
// v-older-rows, on a database that the release before's build wrote (the shell's
// older_rows_upgrade, which runs it before any other case). 300 merged pull requests #7001 to
// #7300 (index i = n - 7000, j = i % 6), each on the branch legion/N(j) with the body
// `Follows D(j). Closes L(j).` (#7051 to #7060: `Follows D(j).`), as GitHub answers them from the
// start (olderRowsPull). The release before's build stores them itself, under the delivery
// settings that its intake needs to take any event:
//   - with the issues L(j) alone made and acme/old-name the project's default repository, the
//     merged events of #7221 to #7240 under that name; then, the repository renamed to
//     acme/attribution-live, the events of #7001 to #7220 and #7241 to #7260. Its intake stores
//     each results row from its event with no texts, which it has nowhere to store, as L(j): the
//     body names it, and no candidate above it names an issue, N(j) being none yet. It stores
//     #7051 to #7060 as no issue. The author of #7001 to #7060 is of the settings' population, so
//     the intake stores their timeline rows too, as L(j) or no issue;
//   - then the issues D(j), and one pass of its reconcile. The pass resolves the timeline rows of
//     #7001 to #7060 again from their stored inputs, whose first issue is now D(j), and leaves each
//     merged results row as the rule of its day judged it. Its merged search finds #7261 to #7300
//     and stores their timeline rows alone, as D(j);
//   - then the issues N(j), which take the keys that the branches named before they existed
//     (keysAfter, seedForetoldIssues).
// This checkout's build then starts on that database. Before any pass, each results row's stored
// answer holds (L(j), or no issue over the timeline's D(j)), and D(j) names #7261 to #7300 as heads
// Dispatch has not read, with no read of GitHub. One pass reads all 300 and leaves none to read,
// and each is then N(j)'s, #7221 to #7240 under acme/attribution-live. A second pass, after each
// stored issue is set back to L(j), reads GitHub for none and leaves each under N(j).

/** older-rows' issue of a list (L, N or D) for pull request `number`, by its index i % 6. */
function olderRowsIssue(keys: readonly string[], number: number): string {
  return keys[(number - 7000) % 6] ?? "";
}

/** The names of older-rows' six issues of one list: L0 to L5, N0 to N5 or D0 to D5. */
function olderRowsNames(list: "D" | "L" | "N"): string[] {
  return [0, 1, 2, 3, 4, 5].map((j) => `${list}${j}`);
}

/** older-rows' pull request `number` as GitHub answers it from the start of the case. Its author
 *  is of the settings' population where a timeline row comes of it, and the merged search finds
 *  #7261 to #7300. */
function olderRowsPull(number: number): FakePullRequest {
  const { D, L, N } = sevenPart("olderRows");
  return {
    author: number <= 7060 || number >= 7261 ? "octocat" : "implementer[bot]",
    base_ref: "main",
    body:
      number >= 7051 && number <= 7060
        ? `Follows ${olderRowsIssue(D, number)}.`
        : `Follows ${olderRowsIssue(D, number)}. Closes ${olderRowsIssue(L, number)}.`,
    commit_messages: [`Work on change ${number}.`],
    head_ref: `legion/${olderRowsIssue(N, number)}`,
    head_repo: attributionRepo,
    head_sha: attributionSha(number),
    merge_commit_sha: attributionSha(number, "e"),
    merged: true,
    searched: number >= 7261,
    state: "closed",
    title: `Change ${number}`,
    updated_at: attributionUpdatedAt,
  };
}

/** The merged event of older-rows' pull request `number` under the repository name `under`, as
 *  GitHub sent it: GitHub's facts under the name the repository had then, at the time of the
 *  merge, before which GitHub's later update time of the pull request lies. */
function olderRowsEvent(number: number, under: string): Record<string, string> {
  const pull = olderRowsPull(number);
  return {
    action: "closed",
    author: pull.author ?? "",
    base_ref: pull.base_ref ?? "main",
    body: pull.body ?? "",
    head_ref: pull.head_ref ?? "",
    head_repo: under,
    head_sha: pull.head_sha,
    kind: "pr",
    merge_commit_sha: pull.merge_commit_sha ?? "",
    merged: "true",
    number: String(number),
    repo: under,
    title: pull.title,
    updated_at: releaseBeforeEventAt,
    url: `https://github.com/${under}/pull/${number}`,
  };
}

/** Answers whether `probe` answered true within `ms`, asking it every pollMs. */
async function awaitTrue(probe: () => Promise<boolean>, ms: number): Promise<boolean> {
  const deadline = Date.now() + ms;
  for (;;) {
    if (await probe()) return true;
    if (Date.now() >= deadline) return false;
    await Bun.sleep(pollMs);
  }
}

/** How many lines of the server's log bind the intake's durable of pull request events. */
function eventsBoundLines(): number {
  return readFileSync(dispatchLog, "utf8")
    .split("\n")
    .filter(
      (line) =>
        line.includes('msg="dispatch delivery: NATS consumer bound"') &&
        line.includes("name=delivery-events")
    ).length;
}

/** The keys that the next `count` issues of the project ATT take after `keys`, whose newest is the
 *  newest key it gave. */
function keysAfter(keys: readonly string[], count: number): string[] {
  const newest = Math.max(...keys.map((key) => Number(key.slice(attributionProject.length + 1))));
  return Array.from({ length: count }, (_, at) => `${attributionProject}-${newest + 1 + at}`);
}

/** Makes the issues `names` one after another, so each takes the next key of the project, and
 *  answers a failure where one takes another key than `foretold` gives it. */
async function seedForetoldIssues(
  names: readonly string[],
  foretold: readonly string[]
): Promise<string | undefined> {
  for (const [at, name] of names.entries()) {
    const [made] = await seedAttributionIssues([name]);
    if (made !== foretold[at]) {
      return `the issue ${name} took ${made}, want ${foretold[at]}, the key that GitHub's answers name`;
    }
  }
  return undefined;
}

/** The count that `statement` answers. */
async function countOf(statement: string): Promise<number> {
  return Number(await sql(statement));
}

/** older-rows, through the release before's build started with no pass at its start: the project
 *  with acme/old-name its default repository, the issues L(j), GitHub's answers, the delivery
 *  settings, the merged events of #7221 to #7240 under the old name, the rename, the events of
 *  #7001 to #7220 and #7241 to #7260, and the issues D(j). It waits for the intake's bind and for
 *  each event's row. The keys of D(j) and N(j) are foretold, since GitHub's answers name them before
 *  they exist. */
async function olderRowsReleaseWrites(): Promise<string | undefined> {
  await asPerson("POST", "/api/v1/projects", { key: attributionProject, name: "Attribution" });
  await asSession("PUT", `/api/v1/settings/repo-projects/${attributionOldName}`, {
    default: true,
    project: attributionProject,
  });
  const L = await seedAttributionIssues(olderRowsNames("L"));
  const D = keysAfter(L, 6);
  const N = keysAfter(D, 6);
  writeSevenState({ olderRows: { D, L, N } });
  note(
    `L0 to L5 are ${L.join(", ")}; GitHub's answers name ${D.join(", ")} for D0 to D5 and ${N.join(", ")} for N0 to N5, the keys the next issues take`
  );
  const pulls: Record<string, FakePullRequest> = {};
  for (let number = 7001; number <= 7300; number++) pulls[String(number)] = olderRowsPull(number);
  const permissions = { actions: "read", contents: "read", pull_requests: "read" };
  await seedRepositories(
    {},
    {
      [attributionRepo]: {
        ...permissions,
        commits: Object.values(sevenCommits),
        default_branch: "main",
        installation_id: attributionInstallation,
        pulls,
      },
      [olderRowsDeployRepo]: {
        ...permissions,
        default_branch: "main",
        installation_id: attributionInstallation,
      },
    }
  );
  const bound = eventsBoundLines();
  await asPerson("PUT", "/api/v1/settings/delivery", deliverySettings(olderRowsDeployRepo));
  if (!(await awaitTrue(async () => eventsBoundLines() > bound, 60_000))) {
    return "the release before's intake bound no durable of pull request events within 60 s of its delivery settings";
  }
  note(
    `the delivery settings name ${olderRowsDeployRepo}; the release before's intake bound its durable`
  );
  const publish = async (first: number, last: number, under: string) => {
    for (let number = first; number <= last; number++) {
      await publishEnvelope(olderRowsEvent(number, under));
    }
  };
  const stored = (under: string, first: number, last: number) =>
    countOf(
      `select count(*) from pull_request_heads where repo = '${under}' and number between ${first} and ${last}`
    );
  await publish(7221, 7240, attributionOldName);
  if (
    !(await awaitTrue(async () => (await stored(attributionOldName, 7221, 7240)) === 20, 120_000))
  ) {
    return `the release before stored ${await stored(attributionOldName, 7221, 7240)} of the 20 results rows of #7221 to #7240 under ${attributionOldName} within 120 s of their events`;
  }
  await asSession("PUT", `/api/v1/settings/repo-projects/${attributionRepo}`, {
    default: true,
    project: attributionProject,
  });
  await asSession("PUT", `/api/v1/settings/repo-projects/${attributionOldName}`, {
    project: attributionProject,
    renamed_to: attributionRepo,
  });
  note(
    `${attributionOldName} is renamed to ${attributionRepo}, now the project's default repository`
  );
  await publish(7001, 7220, attributionRepo);
  await publish(7241, 7260, attributionRepo);
  const timeline = () =>
    countOf(
      `select count(*) from delivery_pull_requests where repo = '${attributionRepo}' and number between 7001 and 7060`
    );
  const settled = async () =>
    (await stored(attributionRepo, 7001, 7260)) === 240 && (await timeline()) === 60;
  if (!(await awaitTrue(settled, 300_000))) {
    return `the release before stored ${await stored(attributionRepo, 7001, 7260)} of the 240 results rows under ${attributionRepo} and ${await timeline()} of the 60 timeline rows of #7001 to #7060 within 300 s of their events`;
  }
  const rows = await sql(
    `select coalesce(issue_key, 'no issue') || ' by ' || attribution_source || ': ' || count(*)
     from pull_request_heads where number between 7001 and 7300 group by issue_key, attribution_source
     order by 1`
  );
  note(`the release before stored 260 results rows with no texts: ${rows.replaceAll("\n", "; ")}`);
  const failure = await seedForetoldIssues(olderRowsNames("D"), D);
  if (failure !== undefined) return failure;
  note(`D0 to D5 are ${D.join(", ")}`);
  await prepareOnePass();
  return undefined;
}

/** older-rows, through the release before's build started again with one pass at its start: that
 *  pass; the results rows and the timeline rows it leaves, each as the section above says; then the
 *  issues N(j). */
async function olderRowsReleasePass(): Promise<string | undefined> {
  note(await awaitOnePass(0, false));
  const { D, L, N } = sevenPart("olderRows");
  const read = async (statement: string) =>
    new Map(
      (await sql(statement))
        .split("\n")
        .filter((row) => row !== "")
        .map((row) => {
          const [number = "", answer = ""] = row.split("|");
          return [Number(number), answer] as const;
        })
    );
  const results = await read(
    `select number, repo || ' ' || coalesce(issue_key, 'no issue') from pull_request_heads
     where number between 7001 and 7300`
  );
  const timeline = await read(
    `select number, coalesce(issue_key, 'no issue') from delivery_pull_requests
     where repo = '${attributionRepo}' and number between 7001 and 7300`
  );
  const differ: string[] = [];
  for (let number = 7001; number <= 7300; number++) {
    const under = number >= 7221 && number <= 7240 ? attributionOldName : attributionRepo;
    const stored = number >= 7051 && number <= 7060 ? "no issue" : olderRowsIssue(L, number);
    const wanted = [
      ["results row", results.get(number), number <= 7260 ? `${under} ${stored}` : undefined],
      [
        "timeline row",
        timeline.get(number),
        number <= 7060 || number >= 7261 ? olderRowsIssue(D, number) : undefined,
      ],
    ] as const;
    for (const [row, got, want] of wanted) {
      if (got !== want) differ.push(`#${number} ${row} ${got ?? "none"}, want ${want ?? "none"}`);
    }
  }
  note(
    `after the release before's pass: ${results.size} results rows and ${timeline.size} timeline rows of #7001 to #7300; ${differ.length} differ from what the release before writes`
  );
  if (differ.length > 0) {
    return `the release before's build left ${differ.length} row(s) of older-rows other than it writes them: ${differ.slice(0, 5).join("; ")}`;
  }
  const failure = await seedForetoldIssues(olderRowsNames("N"), N);
  if (failure !== undefined) return failure;
  note(`N0 to N5 are ${N.join(", ")}`);
  return undefined;
}

/** older-rows, before any pass, through a server that started with no pass: each pull request
 *  with a results row is a basis of the issue that row stores, or of none; D(j) names its pull
 *  requests with a timeline row alone as heads Dispatch has not read; and no read of GitHub. */
async function olderRowsBeforeThePass(): Promise<string | undefined> {
  const { D, L, N } = sevenPart("olderRows");
  const answers = await checkPullsOf([...L, ...N, ...D]);
  const stored = (number: number) =>
    number <= 7260 && (number < 7051 || number > 7060) ? olderRowsIssue(L, number) : undefined;
  for (let number = 7001; number <= 7260; number++) {
    const key = stored(number);
    if (key !== undefined && !answers.get(key)?.compared.includes(number)) {
      const j = (number - 7000) % 6;
      return `#${number} is not a basis of L${j} before its read (L${j} is ${key}, which compares with ${answers.get(key)?.compared.length ?? 0} pull request(s))`;
    }
  }
  const differ = ownersDiffer(answers, 7001, 7300, stored);
  note(
    `before the pass: each of #7001 to #7260 is a basis of the issue its results row stores, or of none; ${differ.length} of the 300 pull requests are under another issue`
  );
  if (differ.length > 0) {
    return `${differ.length} pull request(s) are under another issue before their read: ${differ.slice(0, 5).join("; ")}`;
  }
  for (const [j, key] of D.entries()) {
    const wanted: number[] = [];
    for (let number = 7261; number <= 7300; number++) {
      if ((number - 7000) % 6 === j) wanted.push(number);
    }
    const notRead = [...(answers.get(key)?.notRead ?? [])].sort((a, b) => a - b);
    if (notRead.join(",") !== wanted.join(",")) {
      return `D${j} (${key}) names heads_not_read ${notRead.join(", ") || "none"}, want ${wanted.join(", ")}`;
    }
    const printed = await resultsCommand(key);
    const head = commandHeadLines(printed)[0] ?? "";
    const named = [
      ...printed.matchAll(/^ {2}Not read yet: the head of pull request #(\d+)\.$/gm),
    ].map((match) => Number(match[1]));
    note(`dispatch results --issue ${key} (D${j}): ${head}`);
    note(`  and ${named.length} line(s) Not read yet: the head of pull request #N.`);
    for (const words of [
      "Compared with nothing: Dispatch has not read the head of a pull request of this issue yet.",
      "A pass stands: not known.",
    ]) {
      if (!head.includes(words)) {
        return `dispatch results --issue ${key} (D${j}) prints ${JSON.stringify(head)}, want ${JSON.stringify(words)}`;
      }
    }
    if ([...named].sort((a, b) => a - b).join(",") !== wanted.join(",")) {
      return `dispatch results --issue ${key} (D${j}) names ${named.join(", ") || "no pull request"} in Not read yet: the head of pull request #N., want ${wanted.join(", ")}`;
    }
  }
  const reads = await caseReads();
  note(reads.shown);
  return reads.count === 0
    ? undefined
    : `the reads before the pass asked GitHub for ${reads.count} read(s) of pull requests, want 0`;
}

/** older-rows, after one pass: the pass's counts, which leave nothing to read; each pull request
 *  N(i % 6)'s; and one results row of each of #7221 to #7240, under the case's repository. */
async function olderRowsAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const line = attributionLines().at(-1) ?? {};
  note(
    `the pass logged facts_left_before=${line.facts_left_before} facts_left=${line.facts_left} reads_left=${line.reads_left} facts_refused=${line.facts_refused}`
  );
  for (const [field, want] of [
    ["facts_left_before", "300"],
    ["facts_left", "0"],
    ["reads_left", "0"],
    ["facts_refused", "0"],
  ] as const) {
    if (line[field] !== want) return `${field} is ${line[field]} after one pass, want ${want}`;
  }
  const counts = await attributionCounts();
  note(
    `dispatch attribution: ${counts.facts_left} pull request(s) wait for their facts, and a pass still reads ${counts.reads_left}`
  );
  if (counts.facts_left !== 0 || counts.reads_left !== 0) {
    return `dispatch attribution prints ${counts.facts_left} that wait for their facts and ${counts.reads_left} that a pass still reads, want 0 and 0`;
  }
  const failure = await olderRowsUnderN("after one pass");
  if (failure !== undefined) return failure;
  const [canonical, oldName] = (
    await sql(
      `select count(*) filter (where h.repo = '${attributionRepo}' and exists (select 1 from
           pull_request_texts t where t.repo = h.repo and t.number = h.number)),
         count(*) filter (where h.repo = '${attributionOldName}')
       from pull_request_heads h where h.number between 7221 and 7240`
    )
  ).split("|");
  note(
    `#7221 to #7240: ${canonical} results row(s) with texts under ${attributionRepo}, ${oldName} under ${attributionOldName}`
  );
  return canonical === "20" && oldName === "0"
    ? undefined
    : `#7221 to #7240 hold ${canonical} results row(s) with texts under ${attributionRepo} and ${oldName} under ${attributionOldName}, want 20 and 0`;
}

/** older-rows: each of the 300 pull requests is N(i % 6)'s, and no other issue's. */
async function olderRowsUnderN(step: string): Promise<string | undefined> {
  const { D, L, N } = sevenPart("olderRows");
  const differ = ownersDiffer(await checkPullsOf([...L, ...N, ...D]), 7001, 7300, (number) =>
    olderRowsIssue(N, number)
  );
  note(`${step}: ${300 - differ.length} of the 300 pull requests are under N(i % 6)`);
  return differ.length === 0
    ? undefined
    : `${step}, ${differ.length} pull request(s) are not under N(i % 6): ${differ.slice(0, 5).join("; ")}`;
}

/** older-rows' second fixture: each of the 300 rows stored as L(i % 6) again. */
async function olderRowsSetBack(): Promise<void> {
  const { L } = sevenPart("olderRows");
  const set = await sql(
    `with set as (update pull_request_heads set issue_key = (${sqlKeys(L)})[(number - 7000) % 6 + 1]
       where repo = '${attributionRepo}' and number between 7001 and 7300 returning 1)
     select count(*) from set`
  );
  note(`older-rows: ${set} rows stored as L(i % 6) again`);
  await resetPullReads();
}

/** older-rows, after the second pass: each pull request N(i % 6)'s, with no read of GitHub. */
async function olderRowsAfterTheSetBack(pass: string): Promise<string | undefined> {
  note(pass);
  const failure = await olderRowsUnderN("after the rows are set back and one more pass");
  if (failure !== undefined) return failure;
  const reads = await caseReads();
  note(reads.shown);
  return reads.count === 0
    ? undefined
    : `the pass after the set-back asked GitHub for ${reads.count} read(s) of pull requests, want 0`;
}

// --- rule-reaches-each-row -----------------------------------------------------------------------
//
// v-rule-reaches-each-row. 5,000 merged pull requests #10001 to #15000 (k = (n - 10000) % 100) on
// the branch legion/<B(k) in lower case>, with the body `Follows T(k). Work of B(k).`, each with a
// results row stored as T(k) and its texts (with no texts table, the build before it, a results
// row stored as T(k) by attribution rule 2). The branch ranks above the body, so the rule gives
// each one to B(k) at the read, with no read of GitHub, whatever its row stores. Three more
// phases: a person links T7 to #10107, B8 moves to the project's second repository, and a person
// makes the issue that the branch of a new pull request #15001 names. Each reads every B(k) and
// T(k) again, and none reads GitHub.

/** The issue the rule gives pull request `number` of rule-reaches-each-row, after the steps that
 *  `rule` records; on the second reading (`readingB`), a branch in lower case names nothing. */
function ruleOwner(
  rule: NonNullable<SevenChecksState["rule"]>,
  number: number,
  readingB: boolean
): string | undefined {
  const k = (number - 10000) % 100;
  if (rule.linked === true && number === 10107) return rule.T[7];
  if (readingB || (rule.moved === true && k === 8)) return rule.T[k];
  return rule.B[k];
}

/** rule-reaches-each-row, before its fixture: the issues B0 to B99 and T0 to T99. */
async function ruleIssues(): Promise<string | undefined> {
  const hundred = Array.from({ length: 100 }, (_, k) => k);
  const B = await seedAttributionIssues(hundred.map((k) => `B${k}`));
  const T = await seedAttributionIssues(hundred.map((k) => `T${k}`));
  writeSevenState({ rule: { B, T } });
  note(`B0 to B99 are ${B[0]} to ${B[99]}; T0 to T99 are ${T[0]} to ${T[99]}`);
  return undefined;
}

/** rule-reaches-each-row's fixture, while no server runs: GitHub's answer for the 5,000 pull
 *  requests, and their rows. */
async function ruleFixture(): Promise<void> {
  const { B, T } = sevenPart("rule");
  const pulls: Record<string, FakePullRequest> = {};
  for (let number = 10001; number <= 15000; number++) {
    const k = (number - 10000) % 100;
    pulls[String(number)] = {
      base_ref: "main",
      body: `Follows ${T[k]}. Work of ${B[k]}.`,
      commit_messages: [`Work on change ${number}.`],
      head_ref: `legion/${String(B[k]).toLowerCase()}`,
      head_repo: attributionRepo,
      head_sha: attributionSha(number),
      merge_commit_sha: attributionSha(number, "e"),
      merged: true,
      state: "closed",
      title: `Change ${number}`,
      updated_at: attributionUpdatedAt,
    };
  }
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    pulls: { ...seeded.pulls, ...pulls },
  }));
  const texts = await textsTableExists();
  const k = "(n - 10000) % 100 + 1";
  await sql(
    `insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key,
       attribution_source, title, url, event_at, checked_at, github_read_at${texts ? "" : ", attribution_rule"})
     select '${attributionRepo}', n, lpad(to_hex(n), 40, 'f'), 'legion/' || lower((${sqlKeys(B)})[${k}]),
       'main', 'merged', (${sqlKeys(T)})[${k}], 'title_body', 'Change ' || n,
       'https://github.com/${attributionRepo}/pull/' || n, '${attributionUpdatedAt}', now(), now()${texts ? "" : ", 2"}
     from generate_series(10001, 15000) n`
  );
  if (texts) {
    await sql(
      `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title,
         body, commit_messages, updated_at)
       select h.repo, h.number, h.head_sha, h.head_ref, h.repo, h.base_ref, h.title,
         'Follows ' || (${sqlKeys(T)})[(h.number - 10000) % 100 + 1] || '. Work of '
           || (${sqlKeys(B)})[(h.number - 10000) % 100 + 1] || '.',
         array['Work on change ' || h.number || '.'], h.event_at
       from pull_request_heads h where h.repo = '${attributionRepo}' and h.number between 10001 and 15000`
    );
  }
  note(
    texts
      ? "rule-reaches-each-row: 5,000 results rows stored as T(k), each with its texts, which the trigger keys"
      : "rule-reaches-each-row: no texts table (a build before it): 5,000 results rows stored as T(k) by attribution rule 2, with no texts"
  );
  await resetPullReads();
}

/** rule-reaches-each-row, at the boundary of a pass after `step`. */
async function ruleAtTheBoundary(pass: string, step: string): Promise<string | undefined> {
  note(pass);
  return ruleReads(step, false);
}

/** The reads of rule-reaches-each-row after `step`: each B(k) and T(k), and the new issue where
 *  it exists, through GET /api/v1/issues/{key}/results; B0, B50 and T0 through `dispatch results`;
 *  and the fake's count of reads, which must be 0. Each pull request must be the issue's that
 *  ruleOwner gives. A failure names the counts under B0 to B99 and under T0 to T99, and the reads. */
async function ruleReads(step: string, readingB: boolean): Promise<string | undefined> {
  const rule = sevenPart("rule");
  const answers = await checkPullsOf([
    ...rule.B,
    ...rule.T,
    ...(rule.newIssue === undefined ? [] : [rule.newIssue]),
  ]);
  const reads = await caseReads();
  const holders = holdersOf(answers);
  const under = (keys: readonly string[]) => {
    let count = 0;
    for (let number = 10001; number <= 15000; number++) {
      if ((holders.get(number) ?? []).some((key) => keys.includes(key))) count++;
    }
    return count;
  };
  const [underB, underT] = [under(rule.B), under(rule.T)];
  const differ = ownersDiffer(answers, 10001, 15000, (number) => ruleOwner(rule, number, readingB));
  note(
    `after ${step}: ${underB} of the 5,000 pull requests are under B0 to B99 and ${underT} under T0 to T99; ${differ.length} differ from the rule`
  );
  note(reads.shown);
  if (differ.length > 0) {
    return `after ${step}, ${underT} of 5000 pull requests are under T0 to T99 and ${underB} under B0 to B99 at the boundary, and the fake answered ${reads.count} read(s) of pull requests; ${differ.length} differ from the rule, the first: ${differ.slice(0, 5).join("; ")}`;
  }
  for (const [name, key] of [
    ["B0", rule.B[0]],
    ["B50", rule.B[50]],
    ["T0", rule.T[0]],
  ] as const) {
    const head = commandHeadLines(await resultsCommand(String(key)))[0] ?? "";
    const named = [...head.matchAll(/the head of pull request #(\d+)/g)]
      .map((match) => Number(match[1]))
      .filter((number) => number >= 10001 && number <= 15000)
      .sort((a, b) => a - b);
    const wanted: number[] = [];
    for (let number = 10001; number <= 15000; number++) {
      if (ruleOwner(rule, number, readingB) === key) wanted.push(number);
    }
    note(
      `dispatch results --issue ${key} (${name}) compares with ${named.length} of the pull requests`
    );
    if (named.join(",") !== wanted.join(",")) {
      return `after ${step}, dispatch results --issue ${key} (${name}) compares with ${named.length} of the pull requests, want ${wanted.length}: ${JSON.stringify(head.slice(0, 300))}`;
    }
  }
  return reads.count === 0
    ? undefined
    : `after ${step}, the pass and the reads asked GitHub for ${reads.count} read(s) of pull requests, want 0`;
}

/** rule-reaches-each-row: a person links T7 to #10107. */
async function ruleLink(): Promise<string | undefined> {
  const rule = sevenPart("rule");
  await asPerson("PATCH", issuePath(String(rule.T[7])), {
    external_links: [{ url: `https://github.com/${attributionRepo}/pull/10107` }],
  });
  writeSevenState({ rule: { ...rule, linked: true } });
  note(`a person linked ${rule.T[7]} (T7) to ${attributionRepo}#10107, which B7's branch names`);
  return undefined;
}

/** rule-reaches-each-row: B8 moves to the project's second repository, so its branch key holds no
 *  pull request of the first one, and the body gives T8 its 50 pull requests. */
async function ruleMove(): Promise<string | undefined> {
  const rule = sevenPart("rule");
  await asPerson("PATCH", issuePath(String(rule.B[8])), { repo: attributionSecondRepo });
  writeSevenState({ rule: { ...rule, moved: true } });
  note(`${rule.B[8]} (B8) moved to ${attributionSecondRepo}`);
  return undefined;
}

/** rule-reaches-each-row: the probe issue, whose key gives the next number of the project. */
async function ruleProbe(): Promise<string | undefined> {
  const rule = sevenPart("rule");
  const probe = String(
    (
      (await asPerson("POST", "/api/v1/issues", {
        force: true,
        project: attributionProject,
        title: "The probe that reads the next number of the project",
      })) as Json
    ).key
  );
  const number = Number(probe.slice(attributionProject.length + 1));
  const nextKey = `${attributionProject}-${number + 1}`;
  writeSevenState({ rule: { ...rule, nextKey } });
  note(`the probe issue is ${probe}, so the next issue of ${attributionProject} is ${nextKey}`);
  return undefined;
}

/** rule-reaches-each-row's last fixture: pull request #15001, whose branch names the key of the
 *  issue nobody has made yet, in lower case as the other branches are written. */
async function ruleNewIssuePull(): Promise<void> {
  const { nextKey } = sevenPart("rule");
  const branch = `legion/${String(nextKey).toLowerCase()}`;
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    pulls: {
      ...seeded.pulls,
      "15001": {
        base_ref: "main",
        body: "A change whose body names no issue.",
        commit_messages: ["Work on change 15001."],
        head_ref: branch,
        head_repo: attributionRepo,
        head_sha: attributionSha(15001),
        merge_commit_sha: attributionSha(15001, "e"),
        merged: true,
        state: "closed",
        title: "Change 15001",
        updated_at: attributionUpdatedAt,
      },
    },
  }));
  await sql(
    `insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state,
       attribution_source, title, url, event_at, checked_at, github_read_at)
     values ('${attributionRepo}', 15001, '${attributionSha(15001)}', '${branch}', 'main', 'merged',
       'none', 'Change 15001', 'https://github.com/${attributionRepo}/pull/15001',
       '${attributionUpdatedAt}', now(), now())`
  );
  if (await textsTableExists()) {
    await sql(
      `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title,
         body, commit_messages, updated_at)
       values ('${attributionRepo}', 15001, '${attributionSha(15001)}', '${branch}',
         '${attributionRepo}', 'main', 'Change 15001', 'A change whose body names no issue.',
         array['Work on change 15001.'], '${attributionUpdatedAt}')`
    );
  }
  note(`#15001 is stored on ${branch}, whose key no issue holds yet`);
  await resetPullReads();
}

/** rule-reaches-each-row, after its last pass: a person makes the issue the branch of #15001
 *  names, which must take the key the probe foretold, and that issue compares with #15001. */
async function ruleNewIssue(pass: string): Promise<string | undefined> {
  note(pass);
  const rule = sevenPart("rule");
  const [made] = await seedAttributionIssues(["the issue a branch names"]);
  note(`a person made ${made}`);
  if (made !== rule.nextKey) {
    return `the new issue is ${made}, want ${rule.nextKey}, the key the branch of #15001 names`;
  }
  writeSevenState({ rule: { ...rule, newIssue: made } });
  const [pulls] = (await checkPullsOf([made])).values();
  note(
    `${made} compares with ${pulls?.compared.map((number) => `#${number}`).join(", ") || "nothing"}`
  );
  if (pulls?.compared.join(",") !== "15001") {
    return `${made} compares with ${pulls?.compared.join(", ") || "nothing"}, want #15001, whose branch names it`;
  }
  return ruleReads("a new issue that a stored branch names", false);
}

// --- key-reading-changes -------------------------------------------------------------------------
//
// v-key-reading-changes, on 5,000 pull requests of its own that the server under test stores:
// #20001 to #25000 (k = (n - 20000) % 100), merged, on the branch legion/<B(k) in lower case> with
// the body `Follows T(k). Work of B(k).`, as rule-reaches-each-row's are. The shell starts the
// server with its reconcile every 2 seconds (one_pass_phase --store-every-2s). The driver
// publishes their merged events; the server's intake takes each and reads it from GitHub, and its
// passes read each once more by the batched query, which reads the commit messages too. So a
// negative control's build stores them as that build stores any pull request. Then, with the
// server down, the driver applies fixtures/attribution_reading_b.sql, a second reading on which a
// branch in lower case names nothing. At the boundary of one pass the reading differs from the one
// recorded before, and each of the 5,000 is T(k)'s and no B(k)'s, with no read of GitHub: the
// statement computes the keys of each row on another reading from its texts. rule-reaches-each-
// row's 5,000 rows are T(k)'s too. Then the key remake loop writes the keys again.

/** key-reading-changes' pull request `number` as GitHub answers it. */
function readingPull(number: number): FakePullRequest {
  const { B, T } = sevenPart("rule");
  const k = (number - 20000) % 100;
  return {
    base_ref: "main",
    body: `Follows ${T[k]}. Work of ${B[k]}.`,
    commit_messages: [`Work on change ${number}.`],
    head_ref: `legion/${String(B[k]).toLowerCase()}`,
    head_repo: attributionRepo,
    head_sha: attributionSha(number),
    merge_commit_sha: attributionSha(number, "e"),
    merged: true,
    state: "closed",
    title: `Change ${number}`,
    updated_at: attributionUpdatedAt,
  };
}

/** key-reading-changes, before its fixture, through the server started with its reconcile every 2
 *  seconds: GitHub's answers for its 5,000 pull requests and their merged events; the wait until
 *  the intake has stored each, and then until the passes have read the 5,000 by the batched query,
 *  or each has texts whose commit messages are read; and the reading `dispatch attribution` prints.
 *  The waits are for those facts, whatever the build stores, and fail the case only when a build
 *  stores nothing for 600 s. */
async function readingStored(): Promise<string | undefined> {
  const pulls: Record<string, FakePullRequest> = {};
  for (let number = readingFirst; number <= readingLast; number++) {
    pulls[String(number)] = readingPull(number);
  }
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    pulls: { ...seeded.pulls, ...pulls },
  }));
  await resetPullReads();
  for (const [number, pull] of Object.entries(pulls)) {
    await publishEnvelope({
      action: "closed",
      base_ref: pull.base_ref ?? "main",
      body: pull.body ?? "",
      head_ref: pull.head_ref ?? "",
      head_repo: pull.head_repo,
      head_sha: pull.head_sha,
      kind: "pr",
      merge_commit_sha: pull.merge_commit_sha ?? "",
      merged: "true",
      number,
      repo: attributionRepo,
      title: pull.title,
      updated_at: pull.updated_at ?? attributionUpdatedAt,
      url: `https://github.com/${attributionRepo}/pull/${number}`,
    });
  }
  const range = `repo = '${attributionRepo}' and number between ${readingFirst} and ${readingLast}`;
  const stored = () => countOf(`select count(*) from pull_request_heads where ${range}`);
  if (!(await awaitTrue(async () => (await stored()) === 5000, 600_000))) {
    return `the server stored ${await stored()} of the 5,000 pull requests #${readingFirst} to #${readingLast} within 600 s of their events`;
  }
  note(
    `the server's intake stored the 5,000 pull requests #${readingFirst} to #${readingLast} from their events`
  );
  const batched = async () => {
    const response = await fetch(new URL("/__fixture/requests", githubUrl));
    const { pulls: reads } = (await response.json()) as {
      pulls: Record<string, Record<string, number>>;
    };
    return reads[attributionRepo]?.graphql_pulls ?? 0;
  };
  const read = () =>
    countOf(
      `select count(*) from pull_request_texts where ${range} and commit_messages is not null`
    );
  if (
    !(await awaitTrue(async () => (await batched()) >= 5000 || (await read()) === 5000, 600_000))
  ) {
    return `600 s after the server stored them, its passes had read ${await batched()} of the 5,000 pull requests by the batched query, and ${await read()} have texts with their commit messages`;
  }
  note(
    `the server's passes read ${await batched()} of them by the batched query; ${await read()} have texts with their commit messages`
  );
  return readingRecorded();
}

/** key-reading-changes, before its fixture: the reading `dispatch attribution` prints. */
async function readingRecorded(): Promise<string | undefined> {
  const counts = await attributionCounts();
  writeSevenState({ reading: { recorded: counts.reading } });
  note(`dispatch attribution prints the key reading ${counts.reading}`);
  return undefined;
}

/** key-reading-changes after the change of the reading: each of its 5,000 pull requests is T(k)'s
 *  and no B(k)'s, through GET /api/v1/issues/{key}/results, and the fake answered no read of a
 *  pull request since the fixture. */
async function readingReads(): Promise<string | undefined> {
  const { B, T } = sevenPart("rule");
  const answers = await checkPullsOf([...B, ...T]);
  const reads = await caseReads();
  const holders = holdersOf(answers);
  let [underB, underT] = [0, 0];
  for (let number = readingFirst; number <= readingLast; number++) {
    const held = holders.get(number) ?? [];
    if (held.some((key) => B.includes(key))) underB++;
    if (held.some((key) => T.includes(key))) underT++;
  }
  const differ = ownersDiffer(
    answers,
    readingFirst,
    readingLast,
    (number) => T[(number - 20000) % 100]
  );
  note(
    `after the change of the reading: ${underB} of the 5,000 pull requests the server stored are under B0 to B99 and ${underT} under T0 to T99; ${differ.length} differ from the reading`
  );
  note(reads.shown);
  if (differ.length > 0 || reads.count > 0) {
    return `after the change of the reading, ${underT} of the 5,000 pull requests the server stored are under T0 to T99 and ${underB} under B0 to B99, and the fake answered ${reads.count} read(s) of pull requests; ${differ.length} differ from the reading${differ.length > 0 ? `, the first: ${differ.slice(0, 5).join("; ")}` : ""}`;
  }
  return undefined;
}

/** The lines `dispatch delivery: keys remade` of the server's log, every start of it. */
function keysRemadeLines(): string[] {
  return readFileSync(dispatchLog, "utf8")
    .split("\n")
    .filter((line) => line.includes('msg="dispatch delivery: keys remade"'));
}

/** key-reading-changes' fixture, while no server runs: the second reading. */
async function readingFixture(): Promise<void> {
  const file = join(import.meta.dir, "fixtures/attribution_reading_b.sql");
  const child = Bun.spawn(["psql", databaseUrl, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", file], {
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  if (code !== 0) throw new Error(`psql -f ${file} exited ${code}: ${stderr.trim()}`);
  writeSevenState({
    reading: { ...sevenPart("reading"), remadeLines: keysRemadeLines().length },
  });
  note(`applied ${file}`);
  await resetPullReads();
}

/** key-reading-changes, after one pass: another reading, each of its own pull requests and each of
 *  rule-reaches-each-row's T(k)'s, no read of GitHub; then, within 60 s, no row whose keys wait,
 *  and the log line of the remake. */
async function readingAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const { recorded, remadeLines = 0 } = sevenPart("reading");
  const counts = await attributionCounts();
  note(
    `dispatch attribution: the key reading ${counts.reading} (before the change ${recorded}); ${counts.keys_on_another_reading} pull request(s) whose keys wait`
  );
  if (counts.reading === recorded) {
    return `the key reading is ${counts.reading} at the boundary, the reading recorded before the change`;
  }
  const own = await readingReads();
  if (own !== undefined) return own;
  const failure = await ruleReads("the change of the reading", true);
  if (failure !== undefined) return failure;
  const { newIssue } = sevenPart("rule");
  if (newIssue !== undefined) {
    const [pulls] = (await checkPullsOf([newIssue])).values();
    note(
      `${newIssue} compares with ${pulls?.compared.join(", ") || "nothing"} on the second reading`
    );
    if ((pulls?.compared.length ?? 0) > 0) {
      return `${newIssue} compares with ${pulls?.compared.join(", ")} on the second reading, want nothing: its branch is in lower case`;
    }
  }
  const deadline = Date.now() + 60_000;
  let waiting = counts.keys_on_another_reading;
  let remade = keysRemadeLines().slice(remadeLines);
  while ((waiting !== 0 || remade.length === 0) && Date.now() < deadline) {
    await Bun.sleep(1_000);
    waiting = (await attributionCounts()).keys_on_another_reading;
    remade = keysRemadeLines().slice(remadeLines);
  }
  note(`then dispatch attribution prints ${waiting} pull request(s) whose keys wait`);
  for (const line of remade) note(`the log: ${line.slice(line.indexOf("msg="))}`);
  if (waiting !== 0) {
    return `60 s after the boundary, dispatch attribution prints ${waiting} pull request(s) whose keys wait, want 0`;
  }
  return remade.length > 0
    ? undefined
    : "the log holds no line dispatch delivery: keys remade since the change of the reading";
}

// --- facts-only ----------------------------------------------------------------------------------
//
// v-facts-only. Issues F1 and F2, and the open pull request #16001 on the branch legion/F2 with
// the body `Follows F1. Closes F2.`; GitHub names it as the pull request of an earlier commit too.
// Three writes reach it: an envelope with no head_ref, a pass after its checked_at moves back 10
// minutes, and a result of F2 at the earlier commit. After each, its texts hold GitHub's body, its
// branch keys are {F2}, and F2 compares with it while F1 does not: no writer takes a fact from the
// envelope.

/** facts-only's open pull request, as GitHub answers it. */
function factsOnlyPull(F1: string, F2: string): FakePullRequest {
  return {
    base_ref: "main",
    body: `Follows ${F1}. Closes ${F2}.`,
    commit_messages: ["Work on the change of the facts."],
    head_ref: `legion/${F2}`,
    head_repo: attributionRepo,
    head_sha: sevenCommits.factsOnlyHead,
    merged: false,
    state: "open",
    title: "A change of the facts",
    updated_at: "2026-11-11T00:00:00Z",
  };
}

/** facts-only, the first write: the issues, GitHub's answer, and an envelope with no head_ref. */
async function factsOnlyEnvelope(): Promise<string | undefined> {
  const [F1 = "", F2 = ""] = await seedAttributionIssues(["F1", "F2"]);
  writeSevenState({ factsOnly: { F1, F2 } });
  note(`F1 is ${F1}, F2 is ${F2}`);
  const pull = factsOnlyPull(F1, F2);
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    commit_pulls: { ...seeded.commit_pulls, [sevenCommits.factsOnlyEarly]: [16001] },
    pulls: { ...seeded.pulls, "16001": pull },
  }));
  const published = await publishEnvelope({
    action: "opened",
    base_ref: "main",
    body: String(pull.body),
    head_repo: attributionRepo,
    head_sha: pull.head_sha,
    kind: "pr",
    merged: "false",
    number: "16001",
    repo: attributionRepo,
    title: pull.title,
    updated_at: String(pull.updated_at),
    url: `https://github.com/${attributionRepo}/pull/16001`,
  });
  note(`published the envelope of #16001 with no head_ref on ${published.subject}`);
  return factsOnlyHolds("the envelope with no head_ref", afterEvent(published));
}

/** facts-only's wants after the write `step`: the texts of #16001 (waited for until `by`), F1
 *  comparing with nothing of it, F2 comparing with it, and its texts and branch keys GitHub's. */
async function factsOnlyHolds(step: string, by = Date.now()): Promise<string | undefined> {
  const { F1, F2 } = sevenPart("factsOnly");
  const texts = () =>
    sql(
      `select t.head_ref, t.branch_keys::text, t.body from pull_request_texts t
       where t.repo = '${attributionRepo}' and t.number = 16001 and t.head_sha = '${sevenCommits.factsOnlyHead}'`
    );
  let row = await texts();
  while (row === "" && Date.now() < by) {
    await Bun.sleep(pollMs);
    row = await texts();
  }
  if (row === "") return `after ${step}, #16001 has no texts at its head`;
  const [f1, f2] = [
    (await checkPullsOf([F1])).get(F1)?.compared ?? [],
    (await checkPullsOf([F2])).get(F2)?.compared ?? [],
  ];
  const [headRef, branchKeys, body] = row.split("|");
  note(
    `after ${step}: #16001's texts hold the branch ${headRef}, the branch keys ${branchKeys} and the body ${JSON.stringify(body)}; F1 compares with ${f1.join(", ") || "nothing"}, F2 with ${f2.join(", ") || "nothing"}`
  );
  if (f1.includes(16001))
    return `F1 (${F1}) compares with #16001 after ${step}, want F2 (${F2}) alone`;
  if (!f2.includes(16001)) return `F2 (${F2}) does not compare with #16001 after ${step}`;
  const wantedBody = factsOnlyPull(F1, F2).body;
  if (body !== wantedBody) {
    return `after ${step}, #16001's texts hold the body ${JSON.stringify(body)}, want GitHub's ${JSON.stringify(wantedBody)}`;
  }
  return branchKeys === `{${F2}}`
    ? undefined
    : `after ${step}, #16001's branch keys are ${branchKeys}, want {${F2}}`;
}

/** facts-only's fixture, while no server runs: #16001's checked_at moves back 10 minutes, so the
 *  pass's open arm reads it (delivery.PullRequestHeadStaleAfter is five minutes). */
async function factsOnlyFixture(): Promise<void> {
  const movedTo = await sql(
    `update pull_request_heads set checked_at = checked_at - interval '10 minutes'
     where repo = '${attributionRepo}' and number = 16001 returning checked_at`
  );
  if (movedTo === "") throw new Error(`#16001 has no results row in ${attributionRepo}`);
  writeSevenState({ factsOnly: { ...sevenPart("factsOnly"), movedTo } });
  note(`#16001's checked_at moved back to ${movedTo}`);
}

/** facts-only, after one pass: the pass read #16001, and the wants hold; then a result of F2 at
 *  the earlier commit, whose read-through writes #16001 again, and the wants hold again. */
async function factsOnlyAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const { F2, movedTo } = sevenPart("factsOnly");
  const checkedAt = await sql(
    `select checked_at from pull_request_heads where repo = '${attributionRepo}' and number = 16001`
  );
  note(`#16001's checked_at is ${checkedAt}, moved back to ${movedTo} before the pass`);
  if (checkedAt === movedTo)
    return `the pass did not read #16001: its checked_at is still ${movedTo}`;
  const afterPass = await factsOnlyHolds("one pass of the open arm");
  if (afterPass !== undefined) return afterPass;
  const readsBefore = await pullReads([attributionRepo]);
  await record(F2, {
    check: "attribution",
    commit: sevenCommits.factsOnlyEarly,
    observed: "the run passed at an earlier commit of the pull request",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const readThrough = (await pullReads([attributionRepo])) - readsBefore;
  note(
    `recorded a result of F2 at ${sevenCommits.factsOnlyEarly.slice(0, 12)}; its read-through made ${readThrough} read(s) of pull requests`
  );
  if (readThrough === 0) return "the result at the earlier commit read no pull request from GitHub";
  return factsOnlyHolds("a result at an earlier commit");
}

// --- read-compares -------------------------------------------------------------------------------
//
// v-read-compares. R1 holds a pass at Rhead, recorded first, and a newer pass at Rearly; GitHub
// names no pull request of either at the record, then #16101 at the head Rhead for both. One read
// of `dispatch results` compares with #16101, and the pass stands. R2 holds a pass at R2head, the
// head of #16102, whose branch, title and body name no key, whose commit message names R2, and
// whose commits GitHub answers 500: Dispatch does not know yet whether #16102 is R2's, so the read
// says so and says nothing of a head it has not read. Once GitHub answers the commits, one pass
// reads them, and R2 compares with #16102.

/** The head line of `dispatch results --issue <key>` (commandHeadLines), and what it printed. */
async function resultsHead(
  key: string
): Promise<{ readonly head: string; readonly printed: string }> {
  const printed = await resultsCommand(key);
  return { head: commandHeadLines(printed)[0] ?? "", printed };
}

/** read-compares, before its pass: R1's records and its one read, then R2's record and its one
 *  read. R2's part runs whatever R1's part found, since each proves a rule of its own: a build that
 *  compares R1 at the first read and still says no for R2, or the other way round, fails on the
 *  part it breaks. The failure names R2's part first, then R1's. */
async function readComparesFirstReads(): Promise<string | undefined> {
  const [R1 = "", R2 = ""] = await seedAttributionIssues(["R1", "R2"]);
  writeSevenState({ readCompares: { R1, R2 } });
  note(`R1 is ${R1}, R2 is ${R2}`);
  const passAt = (issue: string, commit: string, observed: string) =>
    record(issue, {
      check: "attribution",
      commit,
      observed,
      run: "bash scripts/e2e/verification-results.sh",
      surface: "cli",
    });
  await passAt(R1, sevenCommits.compareHead, "the run passed at the head");
  await passAt(R1, sevenCommits.compareEarly, "a newer run passed at an earlier commit");
  note(
    `R1 holds a pass at ${sevenCommits.compareHead.slice(0, 12)}, then a newer one at ${sevenCommits.compareEarly.slice(0, 12)}; GitHub named no pull request of either`
  );
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    commit_pulls: {
      ...seeded.commit_pulls,
      [sevenCommits.compareHead]: [16101],
      [sevenCommits.compareEarly]: [16101],
    },
    pulls: {
      ...seeded.pulls,
      "16101": {
        base_ref: "main",
        body: "The change GitHub names once the passes are recorded.",
        commit_messages: ["Work on the change GitHub names."],
        head_ref: `legion/${R1}`,
        head_repo: attributionRepo,
        head_sha: sevenCommits.compareHead,
        merged: false,
        state: "open",
        title: "A change GitHub names late",
        updated_at: "2026-11-12T00:00:00Z",
      },
    },
  }));
  const first = await resultsHead(R1);
  note(`dispatch results --issue ${R1}: ${first.head}`);
  let r1Failure: string | undefined;
  if (
    !first.head.includes("the head of pull request #16101") ||
    !first.head.includes("A pass stands: yes")
  ) {
    const second = await resultsHead(R1);
    note(`a second dispatch results --issue ${R1}: ${second.head}`);
    r1Failure = `the first dispatch results --issue ${R1} (R1) prints ${JSON.stringify(first.head)}, want Compared with #16101 and A pass stands: yes; a second read prints ${JSON.stringify(second.head)}`;
  }
  const r2Failure = await readComparesNotKnown(R2, passAt);
  const failures = [r2Failure, r1Failure].filter((failure) => failure !== undefined);
  return failures.length === 0 ? undefined : failures.join("; and ");
}

/** read-compares' part of R2, before its pass: a pass at the head of #16102, whose answer is not
 *  known while its commits answer 500, and one read of `dispatch results`. */
async function readComparesNotKnown(
  R2: string,
  passAt: (issue: string, commit: string, observed: string) => Promise<CheckResultRecorded>
): Promise<string | undefined> {
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    commit_pulls: { ...seeded.commit_pulls, [sevenCommits.notKnownHead]: [16102] },
    pulls: {
      ...seeded.pulls,
      "16102": {
        base_ref: "main",
        body: "A change whose texts name no issue.",
        commit_messages: [`Dispatch: ${R2}`],
        commits_status: 500,
        head_ref: "work/no-key-in-this-branch",
        head_repo: attributionRepo,
        head_sha: sevenCommits.notKnownHead,
        merged: false,
        state: "open",
        title: "A change whose commits GitHub does not list",
        updated_at: "2026-11-12T00:00:00Z",
      },
    },
  }));
  await passAt(R2, sevenCommits.notKnownHead, "the run passed at the head of a change");
  const r2 = await resultsHead(R2);
  note(`R2 is ${R2}; dispatch results --issue ${R2}:`);
  for (const line of r2.printed.split("\n")) note(`  ${line}`);
  const stands = /A pass stands: ([^.]*)\./.exec(r2.head)?.[1] ?? "nothing";
  if (stands !== "not known") return `R2 answers A pass stands: ${stands}`;
  const stored = await sql(
    `select t.commit_messages is null from pull_request_texts t
     where t.repo = '${attributionRepo}' and t.number = 16102`
  );
  if (stored !== "t") {
    return `the record's read-through stored #16102 ${stored === "" ? "with no texts" : "with its messages read"}, want its texts with the messages not read`;
  }
  for (const words of [
    "Where that commit stands is not known: Dispatch does not know yet whether a pull request it read is this issue's",
    "  Not known yet: whether pull request #16102 is this issue's.",
  ]) {
    if (!r2.printed.includes(words)) return `the output for R2 holds no ${JSON.stringify(words)}`;
  }
  for (const words of ["has not read the head", "of this issue"]) {
    if (r2.printed.includes(words)) return `the output for R2 holds ${JSON.stringify(words)}`;
  }
  return undefined;
}

/** read-compares' fixture, while no server runs: GitHub answers #16102's commits again. */
async function readComparesFixture(): Promise<void> {
  await seedAttribution(attributionRepo, (seeded) => {
    const { commits_status: _cleared, ...pull } = seeded.pulls?.["16102"] ?? {
      head_repo: attributionRepo,
      head_sha: sevenCommits.notKnownHead,
      merged: false,
      state: "open",
      title: "",
    };
    return { ...seeded, pulls: { ...seeded.pulls, "16102": pull } };
  });
  note("GitHub answers the commits of #16102 again");
}

/** read-compares, after one pass: R2 compares with #16102, and its pass stands. */
async function readComparesAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const { R2 } = sevenPart("readCompares");
  const { head } = await resultsHead(R2);
  note(`dispatch results --issue ${R2}: ${head}`);
  return head.includes("the head of pull request #16102") && head.includes("A pass stands: yes")
    ? undefined
    : `after one pass, dispatch results --issue ${R2} (R2) prints ${JSON.stringify(head)}, want Compared with #16102 and A pass stands: yes`;
}

// --- stores-agree --------------------------------------------------------------------------------
//
// v-stores-agree, on rule-reaches-each-row's rows as the cases before it left them. The delivery
// settings, the 5,000 timeline rows of those pull requests stored as T(k), and #16201, which has
// no facts: its timeline row says S1, its results row says S2, and GitHub's branch is legion/S3.
// Before a pass, after one pass, and after a person links T9 to #10109 and one more pass, the
// timeline route and the results routes name one issue for each pull request: S2 for #16201 before
// its read and S3 after it, and T9 for #10109 after the link. It counts no reads: a pass with
// settings reads GitHub for its other steps.

/** stores-agree, before its fixture: the permissions, the delivery settings, and S1 to S3. */
async function storesAgreeSettings(): Promise<string | undefined> {
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    actions: "read",
    pull_requests: "read",
  }));
  await asPerson("PUT", "/api/v1/settings/delivery", deliverySettings(attributionRepo));
  const [S1 = "", S2 = "", S3 = ""] = await seedAttributionIssues(["S1", "S2", "S3"]);
  writeSevenState({ storesAgree: { S1, S2, S3 } });
  note(`the delivery settings name ${attributionRepo}; S1 is ${S1}, S2 is ${S2}, S3 is ${S3}`);
  return undefined;
}

/** stores-agree's fixture, while no server runs: the timeline rows and #16201. */
async function storesAgreeFixture(): Promise<void> {
  const { T } = sevenPart("rule");
  const { S1, S2, S3 } = sevenPart("storesAgree");
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    pulls: {
      ...seeded.pulls,
      "16201": {
        base_ref: "main",
        body: "A change whose texts Dispatch has not read.",
        commit_messages: ["Work on change 16201."],
        head_ref: `legion/${S3}`,
        head_repo: attributionRepo,
        head_sha: attributionSha(16201),
        merge_commit_sha: attributionSha(16201, "e"),
        merged: true,
        state: "closed",
        title: "Change 16201",
        updated_at: attributionUpdatedAt,
      },
    },
  }));
  await sql(
    `insert into delivery_pull_requests (repo, number, title, url, author, merged_at, issue_key)
     select '${attributionRepo}', n, 'Change ' || n, 'https://github.com/${attributionRepo}/pull/' || n,
       'octocat', now() - interval '1 hour',
       case when n = 16201 then '${S1}' else (${sqlKeys(T)})[(n - 10000) % 100 + 1] end
     from (select generate_series(10001, 15000) n union all select 16201) pulls`
  );
  await sql(
    `insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key,
       attribution_source, title, url, event_at, checked_at, github_read_at)
     values ('${attributionRepo}', 16201, '${attributionSha(16201)}', 'legion/${S3}', 'main', 'merged',
       '${S2}', 'title_body', 'Change 16201', 'https://github.com/${attributionRepo}/pull/16201',
       '${releaseBeforeEventAt}', now(), now())`
  );
  note(
    `stores-agree: 5,000 timeline rows stored as T(k); #16201 with a timeline row of ${S1}, a results row of ${S2} and no texts, on GitHub's branch legion/${S3}`
  );
}

/** stores-agree's reads at `step`: GET /api/v1/delivery/timeline and the results route of every
 *  issue of the seven checks name one issue for each pull request of the case's repository, and
 *  #16201 and #10109 take the issue the step wants. */
async function storesAgreeRead(
  pass: string | undefined,
  step: string
): Promise<string | undefined> {
  if (pass !== undefined) note(pass);
  const seven = sevenState();
  const { S2, S3 } = sevenPart("storesAgree");
  const timeline = (await asSession("GET", "/api/v1/delivery/timeline")) as {
    prs: { issue: string | null; number: number; repo: string }[];
  };
  const inTimeline = new Map<number, string>();
  for (const pull of timeline.prs) {
    if (pull.repo === attributionRepo) inTimeline.set(pull.number, pull.issue ?? "no issue");
  }
  const keys = [
    ...(seven.olderRows === undefined
      ? []
      : [...seven.olderRows.L, ...seven.olderRows.N, ...seven.olderRows.D]),
    ...(seven.rule === undefined
      ? []
      : [
          ...seven.rule.B,
          ...seven.rule.T,
          ...(seven.rule.newIssue === undefined ? [] : [seven.rule.newIssue]),
        ]),
    ...Object.values(seven.factsOnly ?? {}).filter((value) =>
      value.startsWith(`${attributionProject}-`)
    ),
    ...Object.values(seven.readCompares ?? {}),
    ...Object.values(seven.storesAgree ?? {}),
  ];
  const holders = holdersOf(await checkPullsOf(keys), true);
  const numbers = new Set([...inTimeline.keys(), ...holders.keys()]);
  const two: string[] = [];
  for (const number of [...numbers].sort((a, b) => a - b)) {
    const named = holders.get(number) ?? [];
    const listed = inTimeline.get(number);
    if (named.length > 1) {
      two.push(`#${number} under ${named.join(" and ")} in the results`);
    } else if (listed !== undefined && listed !== (named[0] ?? "no issue")) {
      two.push(
        `#${number} under ${listed} in the timeline and ${named[0] ?? "no issue"} in the results`
      );
    }
  }
  note(
    `${step}: the timeline lists ${inTimeline.size} pull requests of ${attributionRepo}, and the results routes of ${keys.length} issues name ${holders.size}; ${two.length} with two issues`
  );
  if (two.length > 0) {
    return `${step}, ${two.length} pull request(s) have two issues: ${two.slice(0, 5).join("; ")}`;
  }
  const wants: (readonly [number, string, string])[] = [
    [16201, step === "before a pass" ? S2 : S3, step === "before a pass" ? "S2" : "S3"],
  ];
  if (step.includes("link")) wants.push([10109, String(seven.rule?.T[9]), "T9"]);
  for (const [number, key, name] of wants) {
    const listed = inTimeline.get(number) ?? "no row";
    const named = holders.get(number)?.join(" and ") ?? "no issue";
    note(`${step}: #${number} is under ${listed} in the timeline and ${named} in the results`);
    if (listed !== key || named !== key) {
      return `${step}, #${number} is under ${listed} in the timeline and ${named} in the results, want ${name} (${key}) in both`;
    }
  }
  return undefined;
}

/** stores-agree: a person links T9 to #10109. */
async function storesAgreeLink(): Promise<string | undefined> {
  const { T } = sevenPart("rule");
  await asPerson("PATCH", issuePath(String(T[9])), {
    external_links: [{ url: `https://github.com/${attributionRepo}/pull/10109` }],
  });
  note(`a person linked ${T[9]} (T9) to ${attributionRepo}#10109`);
  return undefined;
}

// --- limit-of-one --------------------------------------------------------------------------------
//
// v-limit-of-one, T10's case through the server. beta/tools, on an installation of its own, has
// five pull requests that wait: #11 owed a read, #12 open and read an hour ago, #13 merged with no
// facts, #14 a partial timeline row, and #15, which only the merged search finds. The case's
// repository's installation has spent its rate limit, and it has a read owed (#16301) and a partial
// row (#16302) too. After one pass each beta/tools pull request is read and written, and the
// pass's log line names the first installation's limit.

/** One pull request of limit-of-one as GitHub answers it, as T10's test seeds it: open or merged,
 *  updated an hour ago, with an author, so the fake answers what the completion reads. */
function limitPull(
  repository: string,
  number: number,
  open: boolean,
  updatedAt: string
): FakePullRequest {
  return {
    author: "octocat",
    base_ref: "main",
    body: `The body of change ${number}.`,
    commit_messages: [`Work on change ${number}.`],
    head_ref: `work/change-${number}`,
    head_repo: repository,
    head_sha: attributionSha(number, "e"),
    ...(open ? {} : { merge_commit_sha: attributionSha(number, "d") }),
    merged: !open,
    state: open ? "open" : "closed",
    title: `Change ${number}`,
    updated_at: updatedAt,
  };
}

/** limit-of-one, before its fixture: the two beta repositories, and delivery settings whose deploy
 *  repository is beta/deploy, as T10's test writes them. */
async function limitSettings(): Promise<string | undefined> {
  const updatedAt = new Date(Math.floor(Date.now() / 1000) * 1000 - 3_600_000).toISOString();
  const permissions = {
    actions: "read",
    contents: "read",
    default_branch: "main",
    installation_id: betaInstallation,
    pull_requests: "read",
  };
  const pulls: Record<string, FakePullRequest> = {};
  for (const number of [11, 12, 13, 14, 15]) {
    pulls[String(number)] = {
      ...limitPull(betaRepo, number, number === 12, updatedAt),
      ...(number === 15 ? { searched: true } : {}),
    };
  }
  await seedRepositories(
    {},
    { [betaDeployRepo]: { ...permissions }, [betaRepo]: { ...permissions, pulls } }
  );
  await asPerson("PUT", "/api/v1/settings/delivery", deliverySettings(betaDeployRepo));
  note(
    `the delivery settings name ${betaDeployRepo}; ${betaRepo} holds #11 to #15, updated at ${updatedAt}`
  );
  return undefined;
}

/** limit-of-one's fixture, while no server runs: the first installation's limit, and the rows. */
async function limitFixture(): Promise<void> {
  const updatedAt =
    attributionRepositories[betaRepo]?.pulls?.["12"]?.updated_at ?? attributionUpdatedAt;
  await seedAttribution(attributionRepo, (seeded) => ({
    ...seeded,
    pulls: {
      ...seeded.pulls,
      "16301": limitPull(attributionRepo, 16301, false, updatedAt),
      "16302": limitPull(attributionRepo, 16302, false, updatedAt),
    },
    rate_limited: true,
  }));
  await sql(
    `insert into pull_request_reads_owed (repo, number, reason)
     values ('${betaRepo}', 11, 'failure'), ('${attributionRepo}', 16301, 'failure')`
  );
  await sql(
    `insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state,
       attribution_source, title, url, event_at, checked_at, github_read_at)
     values
       ('${betaRepo}', 12, '${attributionSha(12, "0")}', 'work/none', 'main', 'open', 'none',
        'Change 12', 'https://github.com/${betaRepo}/pull/12', '2026-11-01T00:00:12Z',
        now() - interval '1 hour', now() - interval '1 hour'),
       ('${betaRepo}', 13, '${attributionSha(13, "0")}', 'work/change-13', 'main', 'merged', 'none',
        'Change 13', 'https://github.com/${betaRepo}/pull/13', '2026-11-01T00:00:13Z', now(), now())`
  );
  if (await textsTableExists()) {
    await sql(
      `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title,
         body, commit_messages, updated_at)
       values ('${betaRepo}', 12, '${attributionSha(12, "0")}', 'work/none', '${betaRepo}', 'main',
         'Change 12', 'The body before the rename.', '{}', '2026-11-01T00:00:12Z')`
    );
  }
  await sql(
    `insert into delivery_pull_requests (repo, number, title, url, author, created_at, partial)
     values
       ('${betaRepo}', 14, 'the title of the search', 'https://github.com/${betaRepo}/pull/14',
        'octocat', '2026-11-01T00:00:14Z', true),
       ('${attributionRepo}', 16302, 'the title of the search',
        'https://github.com/${attributionRepo}/pull/16302', 'octocat', '2026-11-01T00:00:14Z', true)`
  );
  note(
    `limit-of-one: ${attributionRepo}'s installation has spent its rate limit; ${betaRepo} holds #11 owed a read, #12 open, #13 merged with no facts and #14 partial, and the merged search alone finds #15`
  );
}

/** limit-of-one, after one pass: each beta/tools pull request read and written, and the log line
 *  naming the first installation's limit. A build before the texts table (a negative control)
 *  answers #13's messages as "no texts table", which fails the case as well. */
async function limitAfterOnePass(pass: string): Promise<string | undefined> {
  note(pass);
  const line = attributionLines().at(-1) ?? {};
  const scope = line.rate_limit_scope ?? "no rate_limit_scope";
  note(`the pass logged rate_limit_scope=${scope}`);
  const messages13 = (await textsTableExists())
    ? `(select count(*)::text from pull_request_heads h where h.repo = '${betaRepo}' and h.number = 13
          and exists (select 1 from pull_request_texts t where t.repo = h.repo and t.number = h.number
            and t.commit_messages is not null))`
    : "'no texts table'";
  const rows = await sql(
    `select
       (select count(*) from pull_request_heads where repo = '${betaRepo}' and number = 11),
       (select count(*) from pull_request_reads_owed where repo = '${betaRepo}' and number = 11),
       (select head_sha from pull_request_heads where repo = '${betaRepo}' and number = 12),
       ${messages13},
       (select coalesce(partial::text, 'missing') || ' ' || (first_commit_at is not null)::text
          from delivery_pull_requests where repo = '${betaRepo}' and number = 14),
       (select coalesce(partial::text, 'missing') from delivery_pull_requests
          where repo = '${betaRepo}' and number = 15),
       (select coalesce(partial::text, 'missing') from delivery_pull_requests
          where repo = '${attributionRepo}' and number = 16302)`
  );
  const [heads11, owed11, head12, read13, row14 = "", row15 = "", row16302 = ""] = rows.split("|");
  note(
    `${betaRepo}: #11 ${heads11} results row(s) and ${owed11} owed read(s); #12 at ${String(head12).slice(0, 12)}; #13 ${read13} row(s) with its messages; #14 partial and a first commit: ${row14 || "missing"}; #15 partial: ${row15 || "missing"}; ${attributionRepo} #16302 partial: ${row16302 || "missing"}`
  );
  const failures: string[] = [];
  if (scope !== `installation:${attributionInstallation}`) {
    failures.push(
      `the pass logged rate_limit_scope=${scope}, want installation:${attributionInstallation}`
    );
  }
  if (heads11 !== "1" || owed11 !== "0")
    failures.push(`#11 holds ${heads11} results row(s) and ${owed11} owed read(s), want 1 and 0`);
  if (head12 !== attributionSha(12, "e"))
    failures.push(`#12 is at ${head12}, want GitHub's head ${attributionSha(12, "e")}`);
  if (read13 !== "1") failures.push(`#13 has no texts with its messages (${read13})`);
  if (row14 !== "false true")
    failures.push(
      row14.startsWith("true")
        ? "#14 is still partial"
        : `#14 reads ${row14 || "missing"}, want written and not partial, with its first commit`
    );
  if (row15 !== "false")
    failures.push(row15 === "" ? "#15 is missing" : `#15 reads partial ${row15}, want false`);
  return failures.length === 0 ? undefined : failures.join("; ");
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
