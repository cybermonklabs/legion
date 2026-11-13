// The driver of verification-results.sh: it drives a real envoy-dispatch over HTTP as one session
// (the run's agent token, each write naming the session as `actor`) and one person (the dev
// sign-in cookie), seeds the run's fake GitHub, publishes each pull-request event as the
// normalized `pull_request` envelope on the run's own NATS, the one path Dispatch's delivery
// intake reads, and reads two of its answers back through the `dispatch results` command of this
// checkout as well as through the API.
//
//   VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> \
//     VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> bun scripts/e2e/verification-results.ts \
//     <dispatch-url> <nats-url> <fake-github-url>
//
// The database URL is the run's own, which the shell script resets before the server migrates it.
// The driver writes SQL for one fixture and reads it for one answer: a stored pull request row of
// the attribution rule before this build's, which no route of Dispatch can write, since every
// write stamps the rule this build judges by; and the issue and the source that row holds now,
// which no route answers either.
//
// Each case prints `== <case>`, what it read, then `ok <case>` or `FAIL <case>: <why>`; a summary
// follows, and the exit status is 1 when any case failed. The first case,
// `the-results-route-is-served`, is the run's first result write, against a plain issue whose
// spec marks nothing: when it fails, as it does against a binary built from the base commit,
// which serves no such route, the run stops there, since every later case records a result too.
// Each later case seeds the issue it drives, with the spec and the Verification document that
// case needs. A step outside the cases that fails (the sign-in, the projects, the fake GitHub's
// seed, the delivery settings, the probe issue) exits 2 naming it.
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CheckBasis, CheckBlock, CheckResult, CheckResultRecorded } from "@legion/contracts";
import { connect, StringCodec } from "nats";

const [dispatchUrl, natsUrl, githubUrl] = process.argv.slice(2);
const agentToken = process.env.VERIFICATION_RESULTS_E2E_AGENT_TOKEN ?? "";
const databaseUrl = process.env.VERIFICATION_RESULTS_E2E_DATABASE_URL ?? "";
if (
  dispatchUrl === undefined ||
  natsUrl === undefined ||
  githubUrl === undefined ||
  !agentToken ||
  !databaseUrl
) {
  process.stderr.write(
    "usage: VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> VERIFICATION_RESULTS_E2E_DATABASE_URL=<dsn> bun verification-results.ts <dispatch-url> <nats-url> <fake-github-url>\n"
  );
  process.exit(2);
}
const origin = new URL(dispatchUrl).origin;

/** How long a head a bus event teaches Dispatch may take to settle, and how often the results are
 *  read again. The intake binds its durable once the settings cache holds a row, takes the
 *  envelope and writes the pull request's head row; nothing else is waited on, since every other
 *  answer is computed while the results are read. */
const headWaitMs = 30_000;
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

const personCookie = await signIn(person).catch((error: unknown) => setupFailed("sign-in", error));

/** The person's request: the session cookie, and the dashboard origin every cookie write needs. */
function asPerson(method: string, path: string, body?: Json): Promise<unknown> {
  return call(method, path, { Cookie: personCookie, Origin: origin }, body);
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

/** Waits, bounded, for the commit of the result at `commit` to stand where `want` says, and
 *  answers what it last read. Where a commit stands is computed while the results are read, so
 *  this writes nothing and polls the read. */
async function awaitCommitStands(
  key: string,
  blockID: string,
  commit: string,
  want: string
): Promise<string> {
  const deadline = Date.now() + headWaitMs;
  let seen = await commitStandsAt(key, blockID, commit);
  while (seen !== want && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    seen = await commitStandsAt(key, blockID, commit);
  }
  return seen;
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

/** Publishes one normalized `pull_request` envelope, the shape Envoy relays and the shape
 *  Dispatch's intake reads: the envelope's `payload` is the flat string map, and its subject is
 *  `notifications.github.<owner>.<repo>.pr.<number>` with each segment's dots written `_`. */
async function publishPullRequest(payload: Record<string, string>): Promise<string> {
  const [owner, name] = repo.split("/");
  const subject = `notifications.github.${String(owner).replaceAll(".", "_")}.${String(name).replaceAll(".", "_")}.pr.${payload.number}`;
  const eventID = `verification-results-pr-${payload.number}-${payload.action}-${payload.head_sha.slice(0, 7)}`;
  nats.publish(
    subject,
    codec.encode(
      JSON.stringify({
        dedupe_key: eventID,
        event_id: eventID,
        issued_at: Date.now(),
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
  return subject;
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

/** What one check compares its results with, as a case prints and compares it: every basis in
 *  the order the answer carries them, or the reason the check compared with nothing. */
function comparedOf(check: CheckBlock): string {
  const bases = check.compared_with ?? [];
  if (bases.length > 0) return bases.map(basisOf).join(", ");
  return `nothing: ${check.compared_nothing ?? "no reason"}`;
}

/** The `dispatch` command of this checkout (packages/envoy-client/bin/dispatch.ts), which the
 *  bases case reads the same answers through: the surface a reviewer reads, beside the API the
 *  rest of the proof reads. */
const dispatchCommand = join(import.meta.dir, "../../packages/envoy-client/bin/dispatch.ts");

/** Runs `dispatch results --issue <key> [--at <commit>]` against the run's own server, as the
 *  run's session, and answers what it printed. The command is given a home of its own and the
 *  three variables that name the server, so no envoy.json, token file or session state of the
 *  caller's is read. */
async function resultsCommand(key: string, at?: string): Promise<string> {
  const home = mkdtempSync(join(tmpdir(), "verification-results-cli-"));
  try {
    const child = Bun.spawn(
      [
        process.execPath,
        dispatchCommand,
        "results",
        "--issue",
        key,
        ...(at === undefined ? [] : ["--at", at]),
      ],
      {
        env: {
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: session,
          DISPATCH_TOKEN: agentToken,
          DISPATCH_URL: dispatchUrl,
          HOME: home,
          PATH: process.env.PATH ?? "",
        },
        stderr: "pipe",
        stdout: "pipe",
      }
    );
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    if (code !== 0) {
      throw new Error(`dispatch results --issue ${key} exited ${code}: ${stderr.trim()}`);
    }
    return stdout;
  } finally {
    rmSync(home, { force: true, recursive: true });
  }
}

/** The head line the command prints for each check: `- <block id> (<when>). Compared with …`,
 *  which is where the command names the bases. */
function commandHeadLines(printed: string): string[] {
  return printed.split("\n").filter((line) => line.startsWith("- "));
}

/** Runs one statement against the run's own database and answers what psql printed, rows bare
 *  and columns separated by `|` (`-tA`). The driver reads and writes one table this way,
 *  pull_request_heads, for the one fixture and the one answer no route of Dispatch carries. */
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

/** One stored pull request row, as pull_request_heads holds it: the issue it credits, the
 *  source that decided that issue, its state, the head commit and the attribution rule that
 *  judged it. "absent" where no row is stored yet. */
async function storedHead(number: number): Promise<string> {
  const row = await sql(
    `select coalesce(issue_key, 'nobody'), attribution_source, state, head_sha, attribution_rule
     from pull_request_heads where repo = '${repo}' and number = ${number}`
  );
  if (row === "") return "absent";
  const [issue, source, state, head, rule] = row.split("|");
  return `${issue} by ${source} (${state} at ${String(head).slice(0, 12)}, rule ${rule})`;
}

/** Waits, bounded, for the stored row of `number` to read as `want`, and answers what it last
 *  read. The reconcile is the writer here, so this polls and writes nothing. */
async function awaitStoredHead(number: number, want: string): Promise<string> {
  const deadline = Date.now() + headWaitMs;
  let seen = await storedHead(number);
  while (seen !== want && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    seen = await storedHead(number);
  }
  return seen;
}

/** Every `issue.pull_request_attribution_changed` event on one issue, oldest first, in the
 *  words a case compares with: the pull request, the issue it left, the issue it reached and
 *  the source that decided the new one. */
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

/** Waits, bounded, for one issue to carry `want` attribution events, and answers what it last
 *  read. */
async function awaitAttributionMoves(key: string, want: number): Promise<string[]> {
  const deadline = Date.now() + headWaitMs;
  let seen = await attributionMoves(key);
  while (seen.length < want && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    seen = await attributionMoves(key);
  }
  return seen;
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
// `pulls` is what GitHub answers about one pull request, which is read by one reader alone: the
// reconcile's refresh of a stored head row. The attribution case seeds the two pull requests its
// own stored rows name, since their titles name an issue this run creates, and the fixture route
// replaces the whole map, so the seed is written here once and sent again with them.

/** One pull request the fake answers for, as its single-pull-request route reads a fixture:
 *  the head commit, the repository the head branch is in, the state and the title. */
interface FakePullRequest {
  readonly head_repo: string;
  readonly head_sha: string;
  readonly merged: boolean;
  readonly state: string;
  readonly title: string;
}

async function seedRepositories(pulls: Record<string, FakePullRequest>): Promise<void> {
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
        pulls,
      },
      [hiddenRepo]: { contents: "read", installation_id: 257, installation_missing: true },
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  }).catch((error: unknown) => setupFailed("fake GitHub seed", error));
  if (!seeded.ok) setupFailed("fake GitHub seed", `${seeded.status} ${await seeded.text()}`);
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

// The delivery intake handles an envelope only while the settings cache holds a row, so the proof
// writes one; the deploy repository is the proof's own, which the access check reads through the
// fake GitHub before the row is stored.
await asSession("PUT", "/api/v1/settings/delivery", {
  deploy_repo: repo,
  deploy_workflow_path: ".github/workflows/deploy.yml",
  excluded_repos: [],
  population_authors: ["octocat"],
  pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
  production_job_names: ["verification-release / verification-release"],
}).catch((error: unknown) => setupFailed("delivery settings", error));

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
  `setup: person ${person}; session ${session}; probe issue ${probeIssue}; repositories ${repo} (default branch main) and ${hiddenRepo} (no installation). Each case seeds the issue it drives.`
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
  note(`published the opened envelope on ${opened}`);
  await record(a4Issue, {
    check: "a4",
    commit: commits.headOne,
    observed: "recorded at the pull request's first head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFirstHead = await awaitCommitStands(a4Issue, "a4", commits.headOne, "at_basis");
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
  note(`published the synchronize envelope on ${pushed}`);
  const moved = await awaitCommitStands(a4Issue, "a4", commits.headOne, "other_commit");
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
  const atNewHead = await awaitCommitStands(a4Issue, "a4", commits.headTwo, "at_basis");
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
  note(`published the fork's envelope on ${forked}`);
  await record(forkIssue, {
    check: "fork",
    commit: commits.fork,
    observed: "recorded at the commit the fork's pull request heads",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFork = await awaitCommitStands(forkIssue, "fork", commits.fork, "at_basis");
  note(`the commit of the result at the fork's head ${commits.fork.slice(0, 12)} stands ${atFork}`);
  if (atFork === "at_basis") return "a fork's pull request was taken for a basis";
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
  note(`published the same pull request with its head in ${repo} on ${owned}`);
  const atOwnHead = await awaitCommitStands(forkIssue, "fork", commits.fork, "at_basis");
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
  for (const pull of openPulls) {
    await publishPullRequest({
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
  const atOwnHead = await awaitCommitStands(
    basesIssue,
    "before",
    commits.comparedHeadA2,
    "at_basis"
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
  await publishPullRequest({
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
    "other_commit"
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
// the head branch, and the head branch above a key in the title or the body. Five pull requests
// drive it: one its branch credits to a child while its title names the parent first, one its
// body credits, one whose link beats its branch, one whose link beats its title, and two stored
// rows of the rule before this build's, which the reconcile re-judges.
//
// The case's negative controls are in it: the pull request whose body decides is on a branch
// that names no issue, so the branch rule swallows no other source; the pull request a person
// linked is on the child's own branch, so the link stands above it; and the last envelope moves
// a head without moving its attribution, which appends no event, so an event marks a change and
// never a pass.
await runCase("k-attribution-what-decides-a-pull-request-issue", async () => {
  // A check's id must be one among the issue and the issues above it: a result naming an id the
  // child and the parent both carry is refused 409 CHECK_AMBIGUOUS, so each issue's check is
  // named for the issue it proves.
  const checkFor = (id: string, what: string): string =>
    `:::check{#${id} serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}\n${what}\n:::\n`;
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
      head: commits.attributionBranch,
      head_ref: `legion/${child}`,
      number: 41,
      title: `${parent} and ${child}: the child's pull request`,
      want: `${child} by branch`,
    },
    {
      body: `This one proves ${bodied}.`,
      head: commits.attributionBody,
      head_ref: "work/no-key-in-this-branch",
      number: 42,
      title: "A pull request whose title names no issue",
      want: `${bodied} by title_body`,
    },
    {
      body: "A person linked this pull request to an issue of its own.",
      head: commits.attributionLinkOverBranch,
      head_ref: `legion/${child}`,
      number: 43,
      title: "A pull request on the child's branch that a person linked elsewhere",
      want: `${linkedOverBranch} by external_link`,
    },
    {
      body: "A person linked this pull request too.",
      head: commits.attributionLinkOverTitle,
      head_ref: "work/no-key-in-this-one-either",
      number: 44,
      title: `${titled} is the issue this title names`,
      want: `${linkedOverTitle} by external_link`,
    },
  ];
  for (const envelope of envelopes) {
    await publishPullRequest({
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
  }
  for (const envelope of envelopes) {
    const wanted = `${envelope.want} (open at ${envelope.head.slice(0, 12)}, rule 2)`;
    const stored = await awaitStoredHead(envelope.number, wanted);
    note(`#${envelope.number} on ${envelope.head_ref}, titled ${JSON.stringify(envelope.title)}`);
    note(`  is stored as ${stored}`);
    if (stored !== wanted) return `#${envelope.number} is stored as ${stored}, want ${wanted}`;
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

  // Two stored rows the rule before this build's credited to the parent, which no route of
  // Dispatch can write: every write stamps the rule this build judges by. The reconcile takes
  // the open one because no read has reached it and the merged one because an older rule judged
  // it, reads each pull request from GitHub, and moves both to the child the answer names.
  await seedRepositories({
    "45": {
      head_repo: repo,
      head_sha: commits.attributionStoredOpen,
      merged: false,
      state: "open",
      title: `${child} the stored open pull request`,
    },
    "46": {
      head_repo: repo,
      head_sha: commits.attributionStoredMerged,
      merged: true,
      state: "closed",
      title: `${child} the stored merged pull request`,
    },
  });
  const stored = [
    { head: commits.attributionStoredOpen, number: 45, state: "open" },
    { head: commits.attributionStoredMerged, number: 46, state: "merged" },
  ];
  for (const row of stored) {
    await sql(
      `insert into pull_request_heads
         (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
          title, url, attribution_rule)
       values ('${repo}', ${row.number}, '${row.head}', 'legion/${child}', 'main', '${row.state}',
          '${parent}', 'title_body', 'the stored pull request #${row.number}',
          'https://github.com/${repo}/pull/${row.number}', 1)`
    );
    note(`#${row.number} is stored ${row.state}, credited to ${parent} by title_body under rule 1`);
  }
  for (const row of stored) {
    const wanted = `${child} by title_body (${row.state} at ${row.head.slice(0, 12)}, rule 2)`;
    const seen = await awaitStoredHead(row.number, wanted);
    note(`the reconcile re-judged #${row.number}: ${seen}`);
    if (seen !== wanted) return `#${row.number} reads ${seen} after the reconcile, want ${wanted}`;
  }

  // One pass re-judges both rows, in an order of the reconcile's own, so the moves are compared
  // sorted: which of the two it read first says nothing about the rule.
  const parentMoves = await awaitAttributionMoves(parent, 2);
  for (const move of parentMoves) note(`${parent} records ${move}`);
  const childMoves = await awaitAttributionMoves(child, 3);
  for (const move of childMoves) note(`${child} records ${move}`);
  const wantedMoves = [
    `#45 ${parent} → ${child} by title_body`,
    `#46 ${parent} → ${child} by title_body`,
  ];
  if ([...parentMoves].sort().join("; ") !== wantedMoves.join("; ")) {
    return `${parent} records ${parentMoves.join("; ")}, want ${wantedMoves.join("; ")}`;
  }
  const wantedChild = [`#41 nobody → ${child} by branch`, ...wantedMoves];
  if ([...childMoves].sort().join("; ") !== wantedChild.join("; ")) {
    return `${child} records ${childMoves.join("; ")}, want ${wantedChild.join("; ")}`;
  }

  // An attribution that does not move appends no event: the same pull request at a new head,
  // credited to the child as before, is written and recorded nowhere.
  await publishPullRequest({
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
  const wantedHead = `${child} by branch (open at ${commits.attributionBranchMoved.slice(0, 12)}, rule 2)`;
  const movedHead = await awaitStoredHead(41, wantedHead);
  note(`#41 after the push is stored as ${movedHead}`);
  if (movedHead !== wantedHead) return `#41 after the push is stored as ${movedHead}`;
  const afterPush = await attributionMoves(child);
  note(`${child} still records ${afterPush.length} move(s), the push among them appending none`);
  return afterPush.length === childMoves.length
    ? undefined
    : `${child} records ${afterPush.length} moves after the push, want ${childMoves.length}`;
});

summarise();
