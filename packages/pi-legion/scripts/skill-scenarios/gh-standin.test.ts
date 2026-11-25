import { afterEach, beforeAll, expect, setDefaultTimeout, test } from "bun:test";
import {
  appendFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { DAEMON_MODULE, labelGhSelectors, labelGhTable, RIG_GH_TOKEN } from "./daemon-pane";
import { readWorld, sentBody } from "./gh-call";
import { ROUTE_EVIDENCE } from "./gh-route-evidence";
import { routeOf, WORLD_ROUTES, worldRouteOf } from "./gh-routes";
import { type GhSelectors, type GhTable, ghTargetOf, MODELLED, optionLines } from "./gh-selectors";
import {
  mergeableOf,
  mergeStateOf,
  type Recorded,
  reviewDecisionOf,
  type ThreadWorld,
} from "./gh-threads";
import { PR_FIELDS } from "./gh-view";
import {
  ghUndeclared,
  guardRefusal,
  loadRun,
  offWorldAllowed,
  offWorldReach,
  unmergedHead,
  workflowUses,
} from "./health";
import {
  argvOf,
  derive,
  lineShapes,
  type PinnedRead,
  pinnedRead,
  pinnedReads,
  RECORD_READ,
  shapeDiff,
  shapeOf,
  stderrOf,
} from "./production-shapes";
import { pusherOf, roleApp } from "./rest-objects";
import {
  absentFields,
  DESCRIPTION_COMMIT,
  derive as deriveExtract,
  extractCommit,
  keyDiff,
  restOperation,
  servedRoutes,
} from "./rest-required";
import { REMOVED_NAMES, WORLD_PROJECT } from "./run-names";
import { bundled, scriptDefinition, spawnChecked, writeGhSelectors } from "./test-programs";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// GitHub answers the pull request's head as the commit its branch points at, and the worker skill
// has every pushing role read that head after its own push and stop when it is not its commit. A
// stand-in that answers the fixture's first commit for ever stalls every such role: measured on
// 2026-11-06, two runs of the loop that follows an approval pushed their text fixes, read the
// head, found the fixture's
// commit and ended with a WAITING line instead of completing. So the stand-in reads the tip of the
// run's own bare remote at the call, and serves the pull request's diff from it too.
// The rows spawn the stand-in's bundle, built from gh-standin.ts for this suite (test-programs.ts
// says why: a spawn of the source transpiles it on every call).
const STANDIN = bundled(path.join(import.meta.dir, "gh-standin.ts"));
const made: string[] = [];
/** rig.sh's own functions and top-level assignments, read out of the script: the routes every
 * world shares are fixture_write's, built here by running it, as rig.test.ts builds its worlds,
 * so no copy of them lives in this file to drift from the script. */
const RIG = path.join(import.meta.dir, "rig.sh");
const fn = scriptFunctions(RIG);
/** rig.sh's own top-level assignment of `name`, the line as the script writes it. */
function assignment(name: string): string {
  const found = new RegExp(`^${name}=.*$`, "m").exec(readFileSync(RIG, "utf8"));
  if (found === null) throw new Error(`rig.sh assigns no ${name} at the start of a line`);
  return found[0];
}
/** The login every answer of a world names a user by (rig.sh's `world_login`). */
const WORLD_LOGIN = assignment("world_login").replace(/^world_login='(.*)'$/, "$1");
/** That login as GitHub's GraphQL names a bot under `author`: its App's slug, with no `[bot]`
 * (gh-threads.ts's `actorOf`). */
const WORLD_AUTHOR = WORLD_LOGIN.replace(/\[bot\]$/, "");

afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function git(dir: string, ...args: string[]): string {
  const result = spawnChecked(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** The gh owners directory a worker pane names (daemon-pane.ts's `LEGION_GH_OWNERS_DIR`, the
 * claim's gh bundle under the run's state directory), which the Legion gh wrapper's refusal of an
 * owner outside the project names. */
const PANE_OWNERS = "/run/state/secrets/LWEVAL-1-reviewer-gh";
/** The environment the stand-in answers a call under in the run whose directory is `dir`
 * (GH_DATA_DIR), as the pane of the role `by` names (LEGION_ROLE): this process's own without
 * the role and the GH_REPO and GH_CONFIG_DIR a Legion pod's environment may carry, so a call
 * records `by` and no other role, reads no repository of the pod's and names no gh directory of
 * its own, with the pane's gh owners directory (PANE_OWNERS) in place of the pod's, and `vars`
 * over it (the GH_REPO a pane names, and the GH_CONFIG_DIR with which main's wrapper since #4035
 * runs a read naming only owners outside the project, gh-pane-token.ts's `runsOnPaneToken`). */
function callEnv(
  dir: string,
  by?: string,
  vars: Record<string, string> = {}
): Record<string, string | undefined> {
  const own = Object.fromEntries(
    Object.entries(process.env).filter(
      ([name]) => name !== "LEGION_ROLE" && name !== "GH_REPO" && name !== "GH_CONFIG_DIR"
    )
  );
  return {
    ...own,
    GH_DATA_DIR: dir,
    LEGION_GH_OWNERS_DIR: PANE_OWNERS,
    ...(by === undefined ? {} : { LEGION_ROLE: by }),
    ...vars,
  };
}

/** The run directory every `runDir` is a copy of: the routes rig.sh's fixture_write writes for
 * every world, run here against a pull request whose head is the first of two commits and whose
 * body is "a body", the world.json it writes, a bare remote of its own whose issue branch holds
 * those two commits, and the gh selectors of this checkout worker_boot writes into a run
 * (`writeGhSelectors`).
 * Nothing fixture_write writes names the directory, so a copy is the run it would have written
 * there. It is built once, in the hook below, because building it is a dozen git and bash
 * processes, which a row that built its own spent before its first call of the stand-in:
 * measured on 2026-11-15 on a 2-CPU pod running two suites of this directory at once, a build
 * took a median of 471 ms (933 ms at most) and a copy 130 ms (154 ms). Each row copies it, so no
 * row reads what another wrote; the property each row checks is the one it checked on a
 * directory of its own. */
const template = temporaryDirectory("gh-standin-template");
const heads = { first: "", second: "" };
beforeAll(() => {
  const work = path.join(template, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  heads.first = git(work, "rev-parse", "HEAD");
  writeFileSync(path.join(work, "docs.md"), "the text fix\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "docs: the text fix");
  heads.second = git(work, "rev-parse", "HEAD");
  git(work, "update-ref", "refs/heads/legion/LWEVAL-1", heads.first);
  const written = spawnChecked(
    [
      "bash",
      "-c",
      [
        fn("fixture_write"),
        fn("concluded_checks"),
        fn("base_ruleset_routes"),
        fn("role_identity"),
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        ...["world_login", "world_implement_app", "world_implement_app_id"].map(assignment),
        ...["world_review_app", "world_review_app_id"].map(assignment),
        'C1=$2 body="a body" worker_pr=1001 worker_pr_url=https://example.invalid/pull/1001',
        "fixture_write '[]' '[]'",
      ].join("\n"),
      "-",
      template,
      heads.first,
    ],
    { env: { ...process.env } }
  );
  if (written.exitCode !== 0) throw new Error(`fixture_write: ${written.stderr.toString()}`);
  git(template, "clone", "-q", "--bare", work, path.join(template, "remote.git"));
  git(path.join(template, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", heads.first);
  writeGhSelectors(template);
  // Reading the selectors runs gh-selectors.go once, a compile of a few standard packages.
}, 60_000);

/** A run directory of its own, a copy of `template`. With `fixture` it is the run of a scenario
 * that builds a worker world: the world.json fixture_write writes and the bare remote. Without
 * it, it is the run of a scenario that builds none, which has neither — rig.sh writes the world
 * and opens the remote together (worker_fixture), so no run of this rig has one and not the
 * other. No world routes `gh pr diff` (rig.sh's round_world): the diff is the run's own
 * remote's. No world routes the submit of a review either: the stand-in answers that from what
 * the run has already done (gh-standin.ts's reviewSubmission). */
function runDir(options: { fixture: boolean }): { dir: string; first: string; second: string } {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-"));
  made.push(dir);
  cpSync(template, dir, { recursive: true });
  if (!options.fixture) {
    rmSync(path.join(dir, "remote.git"), { recursive: true });
    rmSync(path.join(dir, "world.json"));
  }
  return { dir, ...heads };
}

const headOf = (dir: string) =>
  spawnChecked(
    ["bun", STANDIN, "gh", "pr", "view", "1001", "--json", "headRefOid", "--jq", ".headRefOid"],
    {
      env: callEnv(dir),
    }
  )
    .stdout.toString()
    .trim();

/** `gh api --method POST <route>` with `rest` after it, in the run under `dir`: the create of a
 * review and the submit of a pending one, as the reviewer's prompt makes them. */
const apiPost = (dir: string, route: string, ...rest: string[]) =>
  spawnChecked(["bun", STANDIN, "gh", "api", "--method", "POST", route, ...rest], {
    env: callEnv(dir),
  });
const REVIEWS = "repos/example/widgets/pulls/1001/reviews";
/** The REST reply under the first comment of the approval's first thread (rig.sh's
 * approved_world), which GitHub files in a `COMMENTED` review of its own. */
const REVIEW_REPLIES = "repos/example/widgets/pulls/1001/comments/7001/replies";
/** The body GitHub answers a 404 with, as gh prints it on stdout before `gh: Not Found (HTTP 404)`
 * on stderr (gh-standin.ts's `notFoundBody`), with no line end after it: for an endpoint GitHub
 * does not serve, naming its REST reference, indented with `\r\n` line ends; for an object a
 * served endpoint does not hold, naming that endpoint's own page (`documentation`), on one line. */
const notFoundOf = (documentation?: string) =>
  documentation === undefined
    ? '{\r\n  "message": "Not Found",\r\n  "documentation_url": "https://docs.github.com/rest",\r\n  "status": "404"\r\n}'
    : JSON.stringify({ message: "Not Found", documentation_url: documentation, status: "404" });
const GITHUB_404 = notFoundOf();
/** The create of a pending review: a body naming no `event`, which is what leaves one. */
function createPending(dir: string, review: object): void {
  const body = path.join(dir, "pending.json");
  writeFileSync(body, JSON.stringify(review));
  expect(apiPost(dir, REVIEWS, "--input", body).exitCode).toBe(0);
}

// Catches: a world that invents an approval at no head. GitHub's `POST …/pulls/<n>/reviews/<id>/
// events` submits a review that is still pending: an already-submitted id answers 422, and a
// pending one answers the review carrying the head it was left at. The rig's create route matched
// that path too and answered `{id, html_url}` — a submission with no `commit_id` — and the score,
// which reads the last submission as the one that stands, failed a run whose reviewer had
// approved correctly at the head its push made (threads-closed's head arm of 2026-11-11, one run of
// fifty-six). The artefact moves a measurement both ways: it failed that run, and in the defect
// control it passes one, since a reviewer that approved wrongly and then sent `REQUEST_CHANGES`
// to this endpoint had that change request answered as a submission and read as the verdict that
// stands — the one verdict that control passes.
test("a submit of a review that is no longer pending is refused, as GitHub refuses it", () => {
  const { dir, first } = runDir({ fixture: true });
  const body = path.join(dir, "review.json");
  writeFileSync(body, JSON.stringify({ commit_id: first, event: "APPROVE", body: "approved" }));
  expect(apiPost(dir, REVIEWS, "--input", body).exitCode).toBe(0);
  const again = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(again.exitCode).toBe(1);
  expect(again.stderr.toString()).toContain("422");
  expect(again.stdout.toString()).not.toContain("6001");
});

// Catches: a stand-in that refuses every submit, including the one GitHub answers; one that asks
// for a review id no world of this rig gives, since review_world answers a create 6001 and
// approved_world 5002; and an answer carrying the state the review had rather than the state it
// was submitted in. A review created with no `event` is pending, and the submit of that one
// answers the review itself — GitHub's whole review (`pull-request-review`, which the row walking
// every REST route holds to GitHub's description) — carrying the head it was created at and the
// state GitHub leaves it in, which is what keeps the two-step route an agent may take readable as
// the submission it is.
test("a submit of the run's pending review answers its id, its head and the state submitted", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=REQUEST_CHANGES");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "CHANGES_REQUESTED",
    body: "two findings",
  });
});

// Catches: a stand-in that leaves a review pending after the call that submitted it, so a round
// that submitted the same review twice is answered 200 twice and the score reads two
// submissions where GitHub recorded one. A submitted review is no longer pending.
test("a second submit of the review this run submitted is refused", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  expect(apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE").exitCode).toBe(0);
  const again = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(again.exitCode).toBe(1);
  expect(again.stderr.toString()).toContain("Can not submit a review that is not pending.");
});

// Catches: a create's body read one way here and another by the score, which is how a create the
// score counted as a pending review left this stand-in refusing the call that submitted it. One
// parser reads both (score.ts's `sentBody`), so a body written as fields leaves the pending
// review a body written into a file does; and a create naming no `commit_id` is pending at the
// pull request's own head, which is the head GitHub creates a review at.
test("a create's fields leave a pending review, and one naming no head takes the pull request's", () => {
  const written = runDir({ fixture: true });
  expect(apiPost(written.dir, REVIEWS, "-f", `commit_id=${written.first}`).exitCode).toBe(0);
  const submit = apiPost(written.dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({ commit_id: written.first });
  const headless = runDir({ fixture: true });
  createPending(headless.dir, { body: "two findings" });
  const atHead = apiPost(headless.dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(atHead.exitCode).toBe(0);
  expect(JSON.parse(atHead.stdout.toString())).toMatchObject({ commit_id: headless.first });
});

// `gh api --help`: with `--input`, the field flags go to the query string, so the body of this
// create is the file alone and `-f event=APPROVE` is a query parameter. GitHub leaves the
// review pending, and so does this stand-in. Catches: a reader that merges the two, which left
// no pending review here and refused the call that submitted it 422, while the score read the
// same call as an approval — a round told its review was pending by GitHub and submitted by the
// row that scored it.
test("a create's --input is its whole body, and the fields beside it are the query string", () => {
  const { dir, first } = runDir({ fixture: true });
  const body = path.join(dir, "pending.json");
  writeFileSync(body, JSON.stringify({ commit_id: first, body: "two findings" }));
  const created = apiPost(dir, REVIEWS, "--input", body, "-f", "event=APPROVE");
  expect(created.exitCode).toBe(0);
  expect(JSON.parse(created.stdout.toString())).toMatchObject({ state: "PENDING" });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// `--input=<file>` is the same flag written against its value, which gh takes as it takes
// `--input <file>`. Catches: a recorder that reads its flags by exact match, which stored the
// file of the second spelling and not of the first, so the body reader found nothing under the
// name the call gave and read this create as one that named no head — a pending review at the
// pull request's own tip rather than at the commit the body names, and a submit answered with
// an approval at a head the reviewer never approved.
test("a create whose --input is written against the flag is read from that file too", () => {
  const { dir, second } = runDir({ fixture: true });
  const body = path.join(dir, "pending.json");
  // A commit the pull request's branch does not point at, so a create read as one that named no
  // head is answered the tip instead and this row fails.
  writeFileSync(body, JSON.stringify({ commit_id: second, body: "two findings" }));
  expect(apiPost(dir, REVIEWS, `--input=${body}`).exitCode).toBe(0);
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: second,
    state: "APPROVED",
  });
});

// A create that sends no body at all creates a review with nothing in it on GitHub, which is a
// pending review, and the call that submits it is answered 200. Catches: a stand-in that reads
// a bodyless create as no create, which recorded no pending review for it and refused that
// submit 422 — a refusal GitHub does not make, which is the same defect as an invented success.
test("a create with no body leaves a pending review, at the pull request's own head", () => {
  const { dir, first } = runDir({ fixture: true });
  const created = apiPost(dir, REVIEWS);
  expect(created.exitCode).toBe(0);
  // And the create is answered with that review as GitHub holds it: pending, at the head the
  // pull request was at, which the body named none of.
  expect(JSON.parse(created.stdout.toString())).toMatchObject({
    id: 6001,
    state: "PENDING",
    commit_id: first,
  });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// What GitHub answers a create with is the review it created, carrying the state it is in, the
// head it was created at and its body: `PENDING` where the create named no event, and the state
// the event leaves it in where it named one; `""` for a create that sent no body; and GitHub's
// own for that id on this pull request: its node id, its whole user, its pages and its links, and
// no `submitted_at` while it is pending. Catches: a world's frozen `{id, html_url}` answered as it
// stands, which told a reviewer nothing about whether its approval had been submitted — measured
// in the threads-closed head arm, where a round read that answer back and submitted a second time
// — a create answered without its body, which left a reviewer reading its own footer back on
// null and exiting 1 (round17-forty/reviewer-text-findings-defect-head-4/calls.jsonl:29), and a
// review short of the fields GitHub's description requires (INFRA-764's read search).
test("a create is answered with the review's state and the head it was created at", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const pending = apiPost(dir, REVIEWS, "-f", `commit_id=${second}`);
  const page = "https://example.invalid/pull/1001#pullrequestreview-6001";
  const pull = "https://api.github.com/repos/example/widgets/pulls/1001";
  expect(JSON.parse(pending.stdout.toString())).toEqual({
    id: 6001,
    node_id: "PRR_kwDOAAAAAc4AAE6001",
    user: expect.objectContaining({ login: WORLD_LOGIN, type: "Bot" }),
    body: "",
    state: "PENDING",
    html_url: page,
    pull_request_url: pull,
    author_association: "NONE",
    is_copilot_code_review: false,
    copilot_code_review_effort: null,
    _links: { html: { href: page }, pull_request: { href: pull } },
    commit_id: second,
  });
  const approved = apiPost(dir, REVIEWS, "-f", `commit_id=${first}`, "-f", "event=APPROVE");
  expect(JSON.parse(approved.stdout.toString())).toMatchObject({
    state: "APPROVED",
    commit_id: first,
  });
});

// GitHub reads a POST's parameters from its JSON body and names no query parameter of the create
// of a review, so a create whose path carries a query string is the create all the same
// (gh-call.ts's `reviewsRoute`). Catches: the create of `…/reviews?x=1` answered with the create
// route's frozen `{id:6001}` and exit 0, keeping no review: no state, no head, nothing in the
// reviews list or the record (INFRA-764's class search of writes, 2026-11-18).
test("a create whose path carries a query string creates the review", () => {
  const { dir, first } = runDir({ fixture: true });
  const created = ghCall(
    dir,
    ...["api", "-X", "POST", `${REVIEWS}?x=1`, "-f", `commit_id=${first}`],
    ...["-f", "event=APPROVE", "-f", "body=approved"]
  );
  expect([created.code, JSON.parse(created.out)]).toEqual([
    0,
    expect.objectContaining({ id: 6001, state: "APPROVED", commit_id: first, body: "approved" }),
  ]);
  expect(ghCall(dir, "api", REVIEWS, "--jq", "[.[] | [.id, .state]] | @json").out).toBe(
    '[[6001,"APPROVED"]]'
  );
  expect(recordedCalls(dir)[0]).toMatchObject({
    handler: "review create",
    review: { state: "APPROVED", head: first },
  });
});

// GitHub reads a submit's event from the POST's JSON body and names no query parameter of
// "Submit a review for a pull request", so a submit whose path carries a query string submits the
// pending review all the same (gh-call.ts's `submitsReview`). Catches: the submit of
// `…/reviews/<id>/events?x=1` refused 404 as a write no answer of the world models, its review
// left pending (INFRA-764's class search of writes, 2026-11-18).
test("a submit whose path carries a query string submits the pending review", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submitted = apiPost(dir, `${REVIEWS}/6001/events?x=1`, "-f", "event=REQUEST_CHANGES");
  expect([submitted.exitCode, JSON.parse(submitted.stdout.toString())]).toEqual([
    0,
    expect.objectContaining({ id: 6001, state: "CHANGES_REQUESTED", commit_id: first }),
  ]);
  expect(ghCall(dir, "api", `${REVIEWS}/6001`, "--jq", ".state").out).toBe("CHANGES_REQUESTED");
  expect(recordedCalls(dir)[1]).toMatchObject({
    handler: "review submit",
    review: { state: "CHANGES_REQUESTED", head: first },
  });
});

// GitHub gives every review it creates an id of its own: the world's create route names the
// first (6001, rig.sh's fixture_write), and each create after it the next (gh-threads.ts's
// `createdReviewId`). Catches: every create answered with the route's one id, so the second
// replaced the first in every read by id, in the REST list and in `gh pr view`, while the review
// decision counted both (INFRA-764's class search of writes, 2026-11-18).
test("each create is a review of its own, with its own id at every read", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const create = (...fields: string[]) =>
    JSON.parse(apiPost(dir, REVIEWS, ...fields).stdout.toString());
  expect(create("-f", `commit_id=${first}`, "-f", "event=APPROVE")).toMatchObject({
    id: 6001,
    html_url: "https://example.invalid/pull/1001#pullrequestreview-6001",
  });
  expect(
    create("-f", `commit_id=${second}`, "-f", "event=REQUEST_CHANGES", "-f", "body=two findings")
  ).toMatchObject({
    id: 6002,
    node_id: "PRR_kwDOAAAAAc4AAE6002",
    html_url: "https://example.invalid/pull/1001#pullrequestreview-6002",
  });
  const byId = (id: number) =>
    ghCall(dir, "api", `${REVIEWS}/${id}`, "--jq", "[.id, .state, .commit_id] | @json").out;
  expect([byId(6001), byId(6002)]).toEqual([
    JSON.stringify([6001, "APPROVED", first]),
    JSON.stringify([6002, "CHANGES_REQUESTED", second]),
  ]);
  expect(ghCall(dir, "api", REVIEWS, "--jq", "[.[] | [.id, .state]] | @json").out).toBe(
    '[[6001,"APPROVED"],[6002,"CHANGES_REQUESTED"]]'
  );
  expect(
    ghCall(
      dir,
      "pr",
      "view",
      "1001",
      "--json",
      "reviews",
      "--jq",
      "[.reviews[] | [.id, .state]] | @json"
    ).out
  ).toBe('[["PRR_kwDOAAAAAc4AAE6001","APPROVED"],["PRR_kwDOAAAAAc4AAE6002","CHANGES_REQUESTED"]]');
});

// Catches: a reader of the run's record that takes its opening line for a call. The record a
// submit reads is never empty — `openRecord` writes `{at, opened}` into it before any call of
// the run is answered — and that line carries no `argv` at all: a reader of `call.argv[0]` with
// no fallback throws a TypeError on it, which is a stack trace where the agent expected GitHub's
// own refusal. A run whose only line is that one has created no review, so it has none pending,
// and the submit is refused with the 422 and nothing else on stderr.
test("a submit that is the run's first gh call is refused, not thrown", () => {
  const { dir } = runDir({ fixture: true });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(1);
  expect(submit.stderr.toString()).toBe(
    "gh: Unprocessable Entity (HTTP 422)\nCan not submit a review that is not pending.\n"
  );
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  expect(recorded[0]).toMatchObject({ opened: "calls.jsonl" });
  expect(recorded[0]).not.toHaveProperty("argv");
  expect(recorded).toHaveLength(2);
});

/** `gh <args…>` in the run under `dir`, with its status, its stdout as gh wrote it and its
 * stderr: what a pinned read's text answer is held to line for line, the tab a row of `gh pr
 * checks` ends with among them. */
const ghRaw = (dir: string, ...args: string[]) => {
  const made = spawnChecked(["bun", STANDIN, "gh", ...args], {
    env: callEnv(dir),
  });
  return {
    code: made.exitCode,
    out: made.stdout.toString(),
    err: made.stderr.toString(),
  };
};
/** `gh <args…>` as `ghRaw` runs it, its stdout trimmed: the rows below read a refusal's own text
 * as well as the answer. */
const ghCall = (dir: string, ...args: string[]) => {
  const made = ghRaw(dir, ...args);
  return { ...made, out: made.out.trim() };
};

// The rows below are the stand-in's own behaviour, varied in a run directory this file writes.
// rig.test.ts asks the same questions of the worlds rig.sh builds, through their real routes in
// the order the stand-in matches them, and neither row is a copy of the other: a refusal that
// lives here can still be answered ahead of by a world's route, which is how this endpoint came
// to be answered `{id:6001}` with no head on it.
// Catches: a stand-in that answers whatever id a submit names once the run has a review
// pending, which would hand a round that submitted another review an approval at the head the
// pending one was created at — a review GitHub never wrote. The id is the one the create's own
// answer carried, and GitHub answers every other 422.
test("a submit of an id that is not the pending review's is refused", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const other = apiPost(dir, `${REVIEWS}/5003/events`, "-f", "event=APPROVE");
  expect(other.exitCode).toBe(1);
  expect(other.stderr.toString()).toContain("Can not submit a review that is not pending.");
  expect(other.stdout.toString()).toBe("");
  // And the review the run did leave pending is still pending after that refusal.
  const submitted = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submitted.exitCode).toBe(0);
  expect(JSON.parse(submitted.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// GitHub keeps one pending review per user and pull request, and refuses the create of a second
// while one stands: 422, "User can only have one pending review per pull request". Catches: a
// stand-in that takes the second create and makes it the pending review, which answered a round
// that read the head again and created a second review 200, and then submitted the review
// GitHub never created, at the head the second create named. The first stays the pending one
// until it is submitted, and once it is the run has none and may create another.
test("a second pending create while one stands is refused, and the first stays the pending one", () => {
  const { dir, first, second } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const again = ghCall(
    dir,
    "api",
    "--method",
    "POST",
    REVIEWS,
    "-f",
    `commit_id=${second}`,
    "-f",
    "body=a second reading"
  );
  expect(again).toEqual({
    code: 1,
    out: "",
    err: "gh: Unprocessable Entity (HTTP 422)\nUser can only have one pending review per pull request\n",
  });
  const submitted = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=REQUEST_CHANGES");
  expect(submitted.exitCode).toBe(0);
  expect(JSON.parse(submitted.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "CHANGES_REQUESTED",
  });
  expect(apiPost(dir, REVIEWS, "-f", `commit_id=${second}`).exitCode).toBe(0);
});

/** The stand-in's own sentences for the two creates GitHub refuses 422 on their body alone
 * (gh-standin.ts's CREATE_EVENT and CREATE_BODY). */
const CREATE_EVENT =
  "gh: Unprocessable Entity (HTTP 422)\nA review is created with the event APPROVE, REQUEST_CHANGES or COMMENT, or with none to leave it pending.\n";
const CREATE_BODY =
  "gh: Unprocessable Entity (HTTP 422)\nA review with the event REQUEST_CHANGES or COMMENT is created with a body.\n";

// GitHub's create takes the events APPROVE, REQUEST_CHANGES and COMMENT, or none, and requires a
// body for the second and third (gh-call.ts's `createdState`). Catches: a stand-in that answers
// any of these 200 with a review — an `approve` or a `PENDING` answered `{id, html_url}` and exit
// 0, where GitHub answers 422 — and one that leaves a pending review behind a refused create, so
// the submit after it was answered with a review GitHub never had. An APPROVE needs no body and a
// COMMENT with one is taken. An event that is no string — a number, sent in an `--input` file —
// and one GitHub has no name for (`DISMISS`) are refused as `approve` is, and none of them leaves
// the pending review a create with no event leaves.
test("a create GitHub refuses is refused 422, and leaves no review", () => {
  const { dir, first } = runDir({ fixture: true });
  const body = path.join(dir, "changes.json");
  writeFileSync(body, JSON.stringify({ commit_id: first, event: "REQUEST_CHANGES" }));
  const numbered = path.join(dir, "numbered.json");
  writeFileSync(numbered, JSON.stringify({ commit_id: first, event: 1 }));
  const refused = [
    [["-f", "event=approve", "-f", "body=looks good"], CREATE_EVENT],
    [["-f", "event=PENDING"], CREATE_EVENT],
    [["-f", "event=DISMISS"], CREATE_EVENT],
    [["--input", numbered], CREATE_EVENT],
    [["-f", "event=REQUEST_CHANGES"], CREATE_BODY],
    [["-f", "event=COMMENT", "-f", "body="], CREATE_BODY],
    [["--input", body], CREATE_BODY],
  ] as const;
  for (const [fields, refusal] of refused)
    expect([fields, ghCall(dir, "api", "--method", "POST", REVIEWS, ...fields)]).toEqual([
      fields,
      { code: 1, out: "", err: refusal },
    ]);
  expect(recordedCalls(dir).map((call) => call.review)).toEqual(
    Array(refused.length).fill(undefined)
  );
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(1);
  expect(submit.stderr.toString()).toContain("Can not submit a review that is not pending.");
  const commented = ghCall(dir, "api", REVIEWS, "-f", "event=COMMENT", "-f", "body=a reading");
  expect([commented.code, JSON.parse(commented.out).state]).toEqual([0, "COMMENTED"]);
  const approved = ghCall(dir, "api", REVIEWS, "-f", "event=APPROVE");
  expect([approved.code, JSON.parse(approved.out).state]).toEqual([0, "APPROVED"]);
});

// GitHub's create reads a blank event as none: "by leaving this blank, you set the review action
// state to PENDING". Catches: an `event: ""` read as an event GitHub does not take — the stand-in
// answered it with no state and left no pending review, so the submit after it was refused 422,
// while the score read the same create as pending (gh-call.ts's `createdState`, which both read).
test("a create whose event is empty is pending, as one naming none is", () => {
  const { dir, first } = runDir({ fixture: true });
  const created = ghCall(dir, "api", REVIEWS, "-f", "event=", "-f", `commit_id=${first}`);
  expect(created.code).toBe(0);
  expect(JSON.parse(created.out)).toMatchObject({ state: "PENDING", commit_id: first });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// GitHub answers `GET …/pulls/<n>/reviews/<id>` with that review as it stands. Catches: the read
// of a review the run created answered by the routes — the list of reviews, or `[]` — which a
// round reading its own review back with `--jq '{id,state,commit_id}'` got as `[]` and exit 1
// (`text-only-head-3` and `defect-base-3` of review 5887659447); and a read that answers the
// state the review was created in after it was submitted. The user and the URL are the world's
// answer to the create; the state, the head and the body are the run's. An id this run did not
// create, and the world does not hold, is GitHub's 404.
test("a GET of a review this run created answers that review as it stands", () => {
  const { dir, first } = runDir({ fixture: true });
  const read = () =>
    ghCall(dir, "api", `${REVIEWS}/6001`, "--jq", "{id,state,commit_id,user:.user.login,body}");
  createPending(dir, { commit_id: first, body: "two findings" });
  expect(JSON.parse(read().out)).toEqual({
    id: 6001,
    state: "PENDING",
    commit_id: first,
    user: WORLD_LOGIN,
    body: "two findings",
  });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE", "-f", "body=ok");
  expect(submit.exitCode).toBe(0);
  expect([read().code, JSON.parse(read().out)]).toEqual([
    0,
    { id: 6001, state: "APPROVED", commit_id: first, user: WORLD_LOGIN, body: "ok" },
  ]);
  expect(ghCall(dir, "api", `${REVIEWS}/5001`)).toEqual({
    code: 1,
    out: notFoundOf("https://docs.github.com/rest/pulls/reviews#get-a-review-for-a-pull-request"),
    err: "gh: Not Found (HTTP 404)\n",
  });
});

// gh reads a `-F key=@path` field's value out of the file at `path`, and the review GitHub keeps
// carries that text. Catches: a body reader that keeps `@path` as the value — the review read
// back would carry the file's name as its body.
test("a create's -F body=@file carries the file's text", () => {
  const { dir, first } = runDir({ fixture: true });
  const notes = path.join(dir, "notes.md");
  writeFileSync(notes, "two findings\n");
  const created = ghCall(dir, "api", REVIEWS, "-f", `commit_id=${first}`, "-F", `body=@${notes}`);
  expect(created.code).toBe(0);
  expect(ghCall(dir, "api", `${REVIEWS}/6001`, "--jq", ".body")).toEqual({
    code: 0,
    out: "two findings",
    err: "",
  });
});

// gh sends `key[]=value`, `key[sub]=value` and `key[][sub]=value` fields as the nested body
// `gh api --help` describes, and a `-F key=@path` field as the text of the file at `path` (its
// stdin for `@-`), which the stand-in records beside the call. This is the reader both the
// stand-in and the score read a create's body with (gh-call.ts's `sentBody`). Catches: a
// reader that keeps `comments[][path]` as a flat key, which scored a review whose inline
// comments were written as fields `doc=false workflow=false`, and one that reads `@path` as the
// value. A `-f`/`--raw-field` value is sent as written, a file the record does not hold stays
// as written too, and a value holding `=` is everything after the first one: a reader that cut
// it at the second sent `a` for `sum=a=b`.
test("a body written as fields is gh's: nested keys, arrays of objects, and -F key=@file as the file's text", () => {
  const comments = [
    "-f",
    "comments[][path]=docs/cli.md",
    "-f",
    "comments[][line]=5",
    "--raw-field",
    "comments[][body]=false sentence",
    "--field",
    "comments[][path]=.github/workflows/greet.yml",
    "-f",
    "comments[][body]=wrong trigger",
  ];
  expect(
    sentBody({
      argv: [
        "api",
        REVIEWS,
        ...comments,
        "-F",
        "body=@notes.md",
        "-f",
        "labels[]=a",
        "-f",
        "labels[]=b",
        "-f",
        "empty[]",
        "-f",
        "nested[key]=v",
        "-f",
        "raw=@notes.md",
        "-F",
        "gone=@gone.md",
        "-f",
        "sum=a=b",
      ],
      files: { "notes.md": "two findings\n" },
    })
  ).toEqual({
    comments: [
      { path: "docs/cli.md", line: "5", body: "false sentence" },
      { path: ".github/workflows/greet.yml", body: "wrong trigger" },
    ],
    body: "two findings\n",
    labels: ["a", "b"],
    empty: [],
    nested: { key: "v" },
    raw: "@notes.md",
    gone: "@gone.md",
    sum: "a=b",
  });
  expect(sentBody({ argv: ["api", REVIEWS, "-F", "body=@-"], stdin: "from stdin" })).toEqual({
    body: "from stdin",
  });
});

// GitHub answers `PATCH repos/<o>/<r>/pulls/<n>` with the pull request as the PATCH left it, in
// its REST shape (`head.sha`, the head the pull request is at). Catches: the generic write route's
// `{id, html_url}` answered to it, which 23 recorded PATCH calls read `.body` back from as null
// and one round exited 1 on (review 5887659447, finding 6); and an answer carrying the body from
// before the edit, since the routes are read before it.
test("a PATCH of the pull request's body answers the pull request, carrying the body it left", () => {
  const { dir, first } = runDir({ fixture: true });
  const pull = "repos/example/widgets/pulls/1001";
  const patched = ghCall(dir, "api", "-X", "PATCH", pull, "-f", "body=the fixed body");
  expect(patched.code).toBe(0);
  expect(JSON.parse(patched.out)).toMatchObject({
    number: 1001,
    head: { sha: first },
    body: "the fixed body",
  });
  const input = path.join(dir, "body.json");
  writeFileSync(input, JSON.stringify({ body: "from a file" }));
  expect(ghCall(dir, "api", "--method=patch", pull, "--input", input, "--jq", ".body")).toEqual({
    code: 0,
    out: "from a file",
    err: "",
  });
  expect(ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out).toBe(
    "from a file"
  );
});

// Every body edit gh takes leaves the pull request that body, and the call's record carries it
// as `body`, which is what the score reads the round's body from (score.ts's threadsClosed takes
// the last call that kept one): the PATCH pr-body.md prescribes, `-F body=@body.md`, among them.
// Each spelling is a run of its own, so a spelling the stand-in reads as no edit leaves the body
// the world began with. The text holds an `=`, which a field's value keeps whole. Catches: the
// PATCH read under another verb, or only with or only without the leading slash; a `-F` or
// `--field` value of `@path` kept as the text `@path`; a `-f` one read out of the file it names,
// where gh sends it as written; `--input` read in one spelling of the flag; and each `gh pr
// edit` flag dropped, or read in one spelling of it — `--body-file` read as `--body` with the
// text `-file` among them. Any of these leaves a round that followed the rule with no fixed
// body, and its threads line fails.
test("every body edit gh takes leaves the pull request that body, and the record carries it", () => {
  const text = "the body after the edit, where a=b";
  const pull = "repos/example/widgets/pulls/1001";
  for (const [name, edit, wanted] of [
    ["the prescribed PATCH", (md) => ["api", "--method", "PATCH", pull, "-F", `body=@${md}`], text],
    [
      "-X PATCH, leading slash",
      () => ["api", "-X", "PATCH", `/${pull}`, "-f", `body=${text}`],
      text,
    ],
    ["--method=patch --input", (_, json) => ["api", "--method=patch", pull, "--input", json], text],
    ["--input=", (_, json) => ["api", "--method", "PATCH", pull, `--input=${json}`], text],
    ["--field @", (md) => ["api", "-X", "PATCH", pull, "--field", `body=@${md}`], text],
    ["--field= @", (md) => ["api", "-X", "PATCH", pull, `--field=body=@${md}`], text],
    ["-f @ as written", (md) => ["api", "-X", "PATCH", pull, "-f", `body=@${md}`], "@"],
    ["pr edit --body", () => ["pr", "edit", "1001", "--body", text], text],
    ["pr edit --body=", () => ["pr", "edit", "1001", `--body=${text}`], text],
    ["pr edit -b", () => ["pr", "edit", "1001", "-b", text], text],
    ["pr edit -b=", () => ["pr", "edit", "1001", `-b=${text}`], text],
    ["pr edit -F", (md) => ["pr", "edit", "1001", "-F", md], text],
    ["pr edit --body-file", (md) => ["pr", "edit", "1001", "--body-file", md], text],
    ["pr edit --body-file=", (md) => ["pr", "edit", "1001", `--body-file=${md}`], text],
  ] as [string, (md: string, json: string) => string[], string][]) {
    const { dir } = runDir({ fixture: true });
    const md = path.join(dir, "body.md");
    writeFileSync(md, text);
    const json = path.join(dir, "body.json");
    writeFileSync(json, JSON.stringify({ body: text }));
    const kept = wanted === "@" ? `@${md}` : wanted;
    const edited = ghCall(dir, ...edit(md, json));
    const read = ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body");
    expect([name, edited.code, read.out, recordedCalls(dir)[0]?.body]).toEqual([
      name,
      0,
      kept,
      kept,
    ]);
  }
  // Fourteen runs, each a world and two stand-in calls: longer than bun's five-second default.
}, 60_000);

// The gh stand-in is rig code too (the architect's ruling of 2026-11-21T14:24:55Z). It writes the
// run's fixtures.json after the run's first launch, when an agent's call edits the pull request's
// body, and launch's guard reads that file at the first launch alone (run-names.ts's
// AGENT_WRITTEN): so all such a write adds there is what the call gave it, or what the file held as
// the rig wrote it, which the first launch read. Each write an agent can make to the pull
// request's body or its reviews runs in a run of its own: the body edit in each form the stand-in
// models (`gh pr edit` with a body or a body file, and a PATCH of the pull request with a `-f`
// field, an `@file` field or an `--input` body), the GraphQL `updatePullRequest`, which no world
// models, a review's create through the REST route, and `gh pr review`, which the world refuses.
// Every string of fixtures.json after the call, each value and each key, is one the file held
// before it or one the call gave: an argument, the part of one after an `=`, a file an argument
// names (as `@<file>` or whole) and each string of the JSON such a file holds. Catches: a
// stand-in that writes a string of its own into fixtures.json on such a write, which the guard
// would not read before a later agent read it; and a body edit that keeps nothing there, which
// would leave this row holding no write at all.
test("a write the gh stand-in keeps in fixtures.json adds nothing the call did not give it", () => {
  const text = "the body after the edit, where a=b";
  const pull = "repos/example/widgets/pulls/1001";
  const strings = (value: unknown, into = new Set<string>()): Set<string> => {
    if (typeof value === "string") into.add(value);
    else if (Array.isArray(value)) for (const one of value) strings(one, into);
    else if (typeof value === "object" && value !== null)
      for (const [key, one] of Object.entries(value)) {
        into.add(key);
        strings(one, into);
      }
    return into;
  };
  const jsonOf = (content: string): unknown => {
    try {
      return JSON.parse(content);
    } catch {
      return undefined;
    }
  };
  const mutation = `mutation { updatePullRequest(input: {pullRequestId: "PR_1", body: "${text}"}) { pullRequest { body } } }`;
  /** Each write: its name, whether it keeps the body in fixtures.json, and its arguments. */
  const writes: [string, boolean, (md: string, json: string, head: string) => string[]][] = [
    ["pr edit --body", true, () => ["pr", "edit", "1001", "--body", text]],
    ["pr edit -b=", true, () => ["pr", "edit", "1001", `-b=${text}`]],
    ["pr edit --body-file", true, (md) => ["pr", "edit", "1001", "--body-file", md]],
    ["PATCH -f body=", true, () => ["api", "-X", "PATCH", pull, "-f", `body=${text}`]],
    ["PATCH -F body=@", true, (md) => ["api", "--method", "PATCH", pull, "-F", `body=@${md}`]],
    ["PATCH --input", true, (_, json) => ["api", "--method=patch", pull, "--input", json]],
    ["GraphQL updatePullRequest", false, () => ["api", "graphql", "-f", `query=${mutation}`]],
    [
      "review create",
      false,
      (_, __, head) => [
        ...["api", "-X", "POST", REVIEWS, "-f", `commit_id=${head}`],
        ...["-f", "event=APPROVE", "-f", `body=${text}`],
      ],
    ],
    ["pr review", false, () => ["pr", "review", "1001", "--approve", "--body", text]],
  ];
  const added: string[] = [];
  const kept: [string, boolean][] = [];
  for (const [name, , call] of writes) {
    const { dir, first } = runDir({ fixture: true });
    const md = path.join(dir, "body.md");
    writeFileSync(md, text);
    const json = path.join(dir, "body.json");
    writeFileSync(json, JSON.stringify({ body: text }));
    const fixtures = path.join(dir, "fixtures.json");
    const before = strings(JSON.parse(readFileSync(fixtures, "utf8")));
    const argv = call(md, json, first);
    ghCall(dir, ...argv);
    const after = strings(JSON.parse(readFileSync(fixtures, "utf8")));
    const given = new Set<string>();
    for (const arg of argv) {
      given.add(arg);
      for (let at = arg.indexOf("="); at >= 0; at = arg.indexOf("=", at + 1))
        given.add(arg.slice(at + 1));
    }
    for (const one of [...given]) {
      const named = one.startsWith("@") ? one.slice(1) : one;
      if (!path.isAbsolute(named) || !existsSync(named)) continue;
      const content = readFileSync(named, "utf8");
      given.add(content);
      strings(jsonOf(content), given);
    }
    for (const one of after)
      if (!before.has(one) && !given.has(one)) added.push(`${name}: ${JSON.stringify(one)}`);
    kept.push([name, after.has(text) && !before.has(text)]);
  }
  expect({ added, kept }).toEqual({
    added: [],
    kept: writes.map(([name, keeps]) => [name, keeps]),
  });
  // Nine runs, each a world and a stand-in call: longer than bun's five-second default.
}, 60_000);

// A `body` field on a call that is no PATCH of the pull request edits nothing: a POST of that
// path is another call, one GitHub serves no write at and the world models none of, so it is
// GitHub's 404 and the body stands. Catches: the edit read off the path and the field whatever
// the verb, which would leave a round that posted a comment there reading its comment back as
// the pull request's body; and the POST answered as a success, which the route every write fell
// to gave it.
test("a body field on a POST of the pull request edits no body", () => {
  const { dir } = runDir({ fixture: true });
  const posted = ghCall(
    dir,
    "api",
    "-X",
    "POST",
    "repos/example/widgets/pulls/1001",
    "-f",
    "body=x"
  );
  expect(posted).toEqual({ code: 1, out: GITHUB_404, err: "gh: Not Found (HTTP 404)\n" });
  expect(ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out).toBe("a body");
  expect(recordedCalls(dir)[0]).not.toHaveProperty("body");
});

// GitHub reads a PATCH's parameters from its JSON body and names no query parameter of "Update a
// pull request", so a body PATCH whose path carries a query string edits the body all the same.
// Catches: a PATCH of `…/pulls/<n>?x=1` answered with the pull request's old body and exit 0,
// keeping nothing (INFRA-764's class search of writes, 2026-11-18).
test("a body PATCH whose path carries a query string edits the body", () => {
  const { dir } = runDir({ fixture: true });
  const pull = "repos/example/widgets/pulls/1001?x=1";
  expect(
    ghCall(dir, "api", "-X", "PATCH", pull, "-f", "body=the fixed body", "--jq", ".body")
  ).toEqual({ code: 0, out: "the fixed body", err: "" });
  expect(ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out).toBe(
    "the fixed body"
  );
  expect(recordedCalls(dir)[0]?.body).toBe("the fixed body");
});

// gh's `pr edit` sends the body of every `--body` the call names, empty or not (its `Editable`
// marks a flag the call names edited, whatever its value), and GitHub sets the body it is sent.
// Not measured, since an edit is a write. Catches: `gh pr edit --body ""` read as naming no body,
// answered with the pull request's URL and exit 0 and the body left as it was (INFRA-764's class
// search of writes, 2026-11-18).
test('gh pr edit --body "" leaves the pull request an empty body', () => {
  const { dir } = runDir({ fixture: true });
  expect(ghCall(dir, "pr", "edit", "1001", "--body", "")).toEqual({
    code: 0,
    out: "https://example.invalid/pull/1001",
    err: "",
  });
  expect(ghCall(dir, "pr", "view", "1001", "--json", "body")).toEqual({
    code: 0,
    out: '{"body":""}',
    err: "",
  });
  expect(recordedCalls(dir)[0]?.body).toBe("");
});

// GitHub's REST API names a pull request's empty body `null`, where its GraphQL and gh read it as
// `""`. Measured on 2026-11-18: cybermonklabs/legion #612, whose body is empty, read `null`
// through `gh api repos/<o>/<r>/pulls/612 --jq '.body|type'`, `""` through GraphQL and `gh pr
// view --json body`, and `gh pr view` closed with `--` and an empty line. Catches: the REST pull
// request answering `""`, through the PATCH that emptied it and every read after it.
test("an empty body is null through REST and empty through GraphQL and gh", () => {
  const { dir } = runDir({ fixture: true });
  const pull = "repos/example/widgets/pulls/1001";
  expect(ghCall(dir, "api", "-X", "PATCH", pull, "-f", "body=", "--jq", ".body|type").out).toBe(
    "null"
  );
  expect(ghCall(dir, "api", pull, "--jq", ".body|type").out).toBe("null");
  const graph =
    'query=query{repository(owner:"example",name:"widgets"){pullRequest(number:1001){body}}}';
  expect(
    ghCall(dir, "api", "graphql", "-f", graph, "--jq", ".data.repository.pullRequest.body|type").out
  ).toBe("string");
  expect(ghCall(dir, "pr", "view", "1001", "--json", "body").out).toBe('{"body":""}');
  expect(String(asked(dir, "reviewer", ["pr", "view", "1001"])[1]).endsWith("\n--\n\n")).toBe(true);
});

// gh reads a flag's value as its flag parser does (spf13/pflag): a long flag written against `=`
// carries what follows it, nothing for `--body=`; a short one carries what follows its `=` where
// anything does (`-b=x` is `x`) and its whole rest otherwise, so `-b=` is `=`. A body file named by
// the empty value is no file gh can read. Catches: `--body=` and `-b=` read as the flag standing
// alone, whose value is the argument after it, so the edit named no body and was refused; and an
// edit naming no readable file answered as a success that kept nothing.
test("a flag written against = carries what follows it, as gh's flag parser reads it", () => {
  const edited = (...flags: string[]) => {
    const { dir } = runDir({ fixture: true });
    const made = ghCall(dir, "pr", "edit", "1001", ...flags);
    return [
      flags,
      made.code,
      ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out,
    ];
  };
  expect([
    edited("--body="),
    edited("-b="),
    edited("-b=fixed"),
    edited("-bfixed"),
    edited("--body-file="),
  ]).toEqual([
    [["--body="], 0, ""],
    [["-b="], 0, "="],
    [["-b=fixed"], 0, "fixed"],
    [["-bfixed"], 0, "fixed"],
    [["--body-file="], 1, "a body"],
  ]);
});

// gh hands a comment's or a review's text over in a file with `--body-file` too, and the record
// keeps that file's text beside the call, which is where the score reads what a comment says
// (score.ts's `carriesField`). Catches: a recorder that stores the file of `--input` alone, or
// reads `--body-file` in one spelling of it, so a comment written into a file reads as a call
// that said nothing.
test("a comment's --body-file is recorded with the file's text, in either spelling", () => {
  for (const flag of [
    (file: string) => ["--body-file", file],
    (file: string) => [`--body-file=${file}`],
  ]) {
    const { dir } = runDir({ fixture: true });
    const file = path.join(dir, "comment.md");
    writeFileSync(file, "the reply, which names the fix");
    expect(ghCall(dir, "pr", "comment", "1001", ...flag(file)).code).toBe(0);
    expect(recordedCalls(dir)[0]?.files).toEqual({ [file]: "the reply, which names the fix" });
  }
});

// The verb decides what the call is, in every spelling gh takes it in and by the reader the
// score reads the same call with (score.ts's `isPost`): a call naming GET is a read of that
// path whatever its fields say, so it submits nothing, and GitHub has no read there, so it is
// GitHub's 404; and the review it did not submit is still pending for the call that does, which
// is written here with the leading slash `gh api` takes either way. Catches: a GET answered as
// the submission of the pending review, which gh never sent and GitHub never answers; a GET read
// as the submit that leaves no pending review behind it, where the submit after it would be
// refused 422; and a submit read as none for its leading slash, which would then be refused as a
// write no route of the world models.
test("a GET of the submit route submits nothing, and a submit with a leading slash submits", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  for (const verb of [["-X", "GET"], ["--method=GET"], ["-XGET"]]) {
    const read = ghCall(dir, "api", ...verb, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
    expect(read.code).toBe(1);
    expect(read.out).toBe(GITHUB_404);
    expect(read.err).toBe("gh: Not Found (HTTP 404)\n");
  }
  const submitted = ghCall(
    dir,
    "api",
    "--method",
    "POST",
    `/${REVIEWS}/6001/events`,
    "-f",
    "event=APPROVE"
  );
  expect(submitted.code).toBe(0);
  expect(JSON.parse(submitted.out)).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});
// The submit written `-XPOST`, the spelling gh takes with the verb against the flag. Catches: a
// verb read in the spellings a row happened to use, where a reviewer that submitted its review
// this way would be answered from the routes — `{id:6001}` with no head on it — and leave the
// review pending behind the approval it had just made.
test("a submit written -XPOST is the submit --method POST is", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submitted = ghCall(dir, "api", "-XPOST", `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submitted.code).toBe(0);
  expect(JSON.parse(submitted.out)).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});
// A call on the submit path that gh does not send as a POST submits nothing: `-X PUT` names
// another verb, a write GitHub serves no route for there and the world models none of, and a
// call with no method and no field is the GET gh sends by default, which GitHub has no read for
// either; each is GitHub's 404, and the review stays pending for the call that does submit it.
// Catches: a verb read as "anything but GET", which would answer the PUT as the round's
// submission, one read as "a POST unless it says otherwise", which would answer the bare call
// that way, and a PUT answered as a success, which the route every write fell to gave it.
test("a PUT of the submit route and a bare one submit nothing", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const put = ghCall(dir, "api", "-X", "PUT", `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(put).toEqual({ code: 1, out: GITHUB_404, err: "gh: Not Found (HTTP 404)\n" });
  const bare = ghCall(dir, "api", `${REVIEWS}/6001/events`);
  expect(bare.code).toBe(1);
  expect(bare.err).toBe("gh: Not Found (HTTP 404)\n");
  const submitted = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submitted.exitCode).toBe(0);
  expect(JSON.parse(submitted.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});
// GitHub takes `event` on a submit and refuses the call without one, and refuses an event it
// does not know; the review stays pending either way, since nothing was submitted. Catches: a
// stand-in that leaves both to the routes, where the route every write falls to answered
// `{id:6001}` and exit 0 — a round told its review was submitted when GitHub had refused the
// call — and one that reads the body of a submit that sent none at all, which threw a
// TypeError in place of an answer. The third call is a COMMENT submit, the event whose state
// is neither of the two verdicts.
test("a submit with no event, or an event GitHub does not take, is refused and leaves the review pending", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const refusal =
    "gh: Unprocessable Entity (HTTP 422)\nA review is submitted with the event APPROVE, REQUEST_CHANGES or COMMENT.\n";
  const bodyless = ghCall(dir, "api", "--method", "POST", `${REVIEWS}/6001/events`);
  expect(bodyless.code).toBe(1);
  expect(bodyless.err).toBe(refusal);
  expect(bodyless.out).toBe("");
  const unknown = ghCall(
    dir,
    "api",
    "--method",
    "POST",
    `${REVIEWS}/6001/events`,
    "-f",
    "event=DISMISS"
  );
  expect(unknown.code).toBe(1);
  expect(unknown.err).toBe(refusal);
  // Each refused call is recorded, as every call the stand-in answers is.
  expect(
    readFileSync(path.join(dir, "calls.jsonl"), "utf8")
      .split("\n")
      .filter((line) => line !== "")
  ).toHaveLength(4);
  const commented = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=COMMENT");
  expect(commented.exitCode).toBe(0);
  expect(JSON.parse(commented.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "COMMENTED",
  });
});
// Catches: a stand-in that reads the status a submit was recorded with for whether GitHub took
// it. `--jq` runs over the answer this stand-in had already given, so a filter that fails
// leaves the call recorded with exit 1 and the review submitted all the same; a reader of that
// status left it pending, and the next submit of the same review was answered 200 a second
// time, where GitHub answers 422.
test("a submit whose --jq failed submitted the review all the same", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submitted = ghCall(
    dir,
    "api",
    "--method",
    "POST",
    `${REVIEWS}/6001/events`,
    "-f",
    "event=APPROVE",
    "--jq",
    ".[0]"
  );
  expect(submitted.code).toBe(1);
  expect(submitted.err).toContain("Cannot index object with number");
  const again = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(again.exitCode).toBe(1);
  expect(again.stderr.toString()).toContain("Can not submit a review that is not pending.");
});
// The review endpoints are the run's own repository's, as the score's readers are. Catches: a
// stand-in that reads a create of `other/widgets` as this run's pending review, which would
// answer the submit of a review this world never got with a review object and 200, and one that
// answers a submit on another repository's pull request from this run's record. Production's gh
// never reaches GitHub for either: the Legion gh wrapper refuses an owner outside the project,
// exit 2, and so does the world's.
test("a create and a submit on another repository's pull request are no review of this run", () => {
  const { dir, first } = runDir({ fixture: true });
  const other = "repos/other/widgets/pulls/1001/reviews";
  expect(apiPost(dir, other, "-f", `commit_id=${first}`).exitCode).toBe(2);
  const elsewhere = apiPost(dir, `${other}/6001/events`, "-f", "event=APPROVE");
  expect([elsewhere.exitCode, elsewhere.stdout.toString()]).toEqual([2, ""]);
  expect(elsewhere.stderr.toString()).toBe(ownerAloneRefused("other"));
  const ours = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(ours.exitCode).toBe(1);
  expect(ours.stderr.toString()).toContain("Can not submit a review that is not pending.");
});
// `gh pr list --open` is a call of the run, not rig.sh asking for the record's opening line.
// That ask is `gh --open`, the flag alone after the program word the wrapper inserts, and a
// stand-in that took the flag wherever it fell — or at the end of a call of any length —
// answered this listing with nothing, recorded no call, and left the score reading a role that
// looked for its pull request as one that made no call at all. With no `--json`, gh lists a pull
// request as one tab-separated line (gh-view.ts's `listLine`), where this row read the world's
// object as JSON before the world answered `gh pr list` as gh does (INFRA-764's ruling 2).
test("gh pr list --open is a call of the run and no ask for the opening line", () => {
  const { dir } = runDir({ fixture: true });
  const listed = ghCall(dir, "pr", "list", "--open");
  expect(listed.code).toBe(0);
  // The listing itself, so a stand-in that answered nothing fails here.
  expect(listed.out).toMatch(
    /^1001\tfeat: greet\(\) \(LWEVAL-1\)\tlegion\/LWEVAL-1\tOPEN\t\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/
  );
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
  expect(recorded).toHaveLength(2);
  expect(recorded[1]).toMatchObject({ argv: ["pr", "list", "--open"], exit: 0 });
});
// Catches: a stand-in that answers the fixture's commit after the role pushed, so every pushing
// role reads a head that is not its own and stops.
test("the pull request's head is the tip of the run's own remote", () => {
  const { dir, first, second } = runDir({ fixture: true });
  expect(headOf(dir)).toBe(first);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(headOf(dir)).toBe(second);
});

// Catches: a stand-in that refuses or empties the answer for a run with no worker fixture of its
// own, which every scenario that builds no world is.
test("a run with no worker fixture keeps the route's own head", () => {
  const { dir, first } = runDir({ fixture: false });
  expect(headOf(dir)).toBe(first);
});

// A run with no world has no review of its own, so the submit of one is no submit this stand-in
// answers from its record, and no route of the world models a submit with no review behind it:
// it is refused as GitHub refuses a write the world does not model. Catches: a stand-in that
// refuses it 422 as the submit of a review that is not pending, which reads a run that holds no
// world as one that holds a world with no review in it; and one that answers it a success, as
// the route every write fell to did.
test("a run with no worker fixture refuses the submit of a review as a write it does not model", () => {
  const { dir } = runDir({ fixture: false });
  expect(
    ghCall(dir, "api", "--method", "POST", `${REVIEWS}/6001/events`, "-f", "event=APPROVE")
  ).toEqual({ code: 1, out: GITHUB_404, err: "gh: Not Found (HTTP 404)\n" });
});

/** What the stand-in answers for `gh <args...>` in run directory `dir`, as `by` if the run names
 * an agent. */
const answer = (dir: string, args: string[], by?: string) =>
  spawnChecked(["bun", STANDIN, "gh", ...args], {
    env: callEnv(dir, by),
  })
    .stdout.toString()
    .trim();

// Catches: a stand-in that answers the diff the fixture froze after another role pushed to the
// pull request, so a reviewer reading the fix the implementer pushed sees the change without it
// and resolves nothing. The run's own remote is the fact, as it is for the head.
test("the pull request's diff is the diff of the run's own remote", () => {
  const { dir, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  git(
    remote,
    "update-ref",
    "refs/heads/main",
    git(remote, "rev-parse", "refs/heads/legion/LWEVAL-1")
  );
  expect(answer(dir, ["pr", "diff", "1001"])).toBe("");
  git(remote, "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(answer(dir, ["pr", "diff", "1001"])).toContain("+the text fix");
  expect(answer(dir, ["pr", "diff", "1001", "--name-only"])).toBe("docs.md");
});

// Catches: a stand-in that reads a world.json it cannot parse as a run with no world of its own.
// The two readings serve different answers: a run that builds no world is served the fixture's
// frozen head, which is right for it and is silence for a run whose world the rig wrote wrongly
// — a pushing role would read a head that is not its own and stop, and a reviewer would read the
// diff of a branch that is not the world's. So only a missing file is "no world", and a file
// that does not parse exits with an error naming it. The two worlds below are the ones that
// exercise `base` and `repo`, which rig.sh's fixture_write writes into every world and this
// schema defaults for neither.
test("a world.json that does not parse exits with an error, and one with no base or no repo too", () => {
  for (const text of [
    "{not json",
    JSON.stringify({ repo: "example/widgets", branch: "legion/LWEVAL-1" }),
    JSON.stringify({ branch: "legion/LWEVAL-1", base: "main" }),
  ]) {
    const { dir } = runDir({ fixture: true });
    writeFileSync(path.join(dir, "world.json"), text);
    const call = spawnChecked(["bun", STANDIN, "gh", "pr", "view", "1001"], {
      env: callEnv(dir),
    });
    expect(call.exitCode).toBe(2);
    expect(call.stderr.toString()).toContain("world.json is no world");
  }
});

// Catches: a stand-in that records no agent, so a run that boots one agent per role cannot tell
// which role made a call and the score reads one role's work as the other's. The line before
// the two calls is the one the record was opened with, which records no call at all.
test("each call records the agent the run named, and none when it names one", () => {
  const { dir } = runDir({ fixture: true });
  answer(dir, ["pr", "view", "1001"], "implementer");
  answer(dir, ["pr", "view", "1001"]);
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
  expect(recorded[0]).toMatchObject({ opened: "calls.jsonl" });
  expect(recorded.slice(1).map((call) => call.by)).toEqual(["implementer", undefined]);
});

// The gh a run's PATH resolves to runs a bundle of this stand-in (rig.sh's standins: $R/lib/gh.js,
// the run's directory set in its first line), a file the world shows and an agent can `cat`, and
// it carried the rig's names: the two variables it took its run and its agent's role in
// (SKILL_SCENARIO_RUN, SKILL_SCENARIO_AGENT) and its own name in its refusals (`gh-standin`). So
// it takes its run in GH_DATA_DIR, a variable of gh's own shape, and its role in LEGION_ROLE,
// which production names every pane's role in, and refuses as `gh`. The route table's name,
// fixtures.json, stays: the world shows $R, so `ls` shows the file by name whatever the bundle
// says, and health.ts and score.ts read it under that name in every recorded run. Catches: a
// bundle that names the rig, one that finds no run with nothing in its caller's environment, a
// call that records a role its pane did not name, and a refusal in the rig's words.
test("the gh a run's PATH resolves to names none of the rig's words, and finds its run and role", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-bundle-"));
  made.push(dir);
  const built = spawnChecked(
    [
      "bash",
      "-c",
      [fn("fail"), fn("standins"), `here='${import.meta.dir}'`, "R=$1", "standins"].join("\n"),
      "-",
      dir,
    ],
    { env: process.env }
  );
  expect([built.exitCode, built.stderr.toString()]).toEqual([0, ""]);
  // The run's own path, which the banner sets, is the world's to name (rig.sh's world_paths), so
  // it is masked: what is read is what the bundle says of its own.
  const bundle = readFileSync(path.join(dir, "lib", "gh.js"), "utf8").replaceAll(dir, "<run>");
  expect(bundle).toContain('\nprocess.env.GH_DATA_DIR = "<run>";\n');
  const words = [
    /skill.?scenario/gi,
    /stand.?in/gi,
    ...REMOVED_NAMES.map(
      (name) => new RegExp(`(?<![a-z0-9])${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`, "gi")
    ),
  ];
  expect(words.flatMap((word) => bundle.match(word) ?? [])).toEqual([]);
  const gh = (role: string | undefined, ...args: string[]) =>
    spawnChecked([path.join(dir, "bin", "gh"), ...args], {
      env: { PATH: process.env.PATH ?? "", ...(role === undefined ? {} : { LEGION_ROLE: role }) },
    });
  expect(gh(undefined, "--open").exitCode).toBe(0);
  const asked = gh("reviewer", "api", "user");
  expect([asked.exitCode, asked.stderr.toString()]).toEqual([1, "gh: Not Found (HTTP 404)\n"]);
  gh(undefined, "api", "user");
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
  expect(recorded.map((call) => [call.opened, call.by, call.argv])).toEqual([
    ["calls.jsonl", undefined, undefined],
    [undefined, "reviewer", ["api", "user"]],
    [undefined, undefined, ["api", "user"]],
  ]);
  const unset = spawnChecked(["bun", STANDIN, "gh", "api", "user"], {
    env: { PATH: process.env.PATH ?? "" },
  });
  expect([unset.exitCode, unset.stderr.toString()]).toEqual([2, "gh: GH_DATA_DIR is unset\n"]);
}, 60_000);

// Catches: a call that names github.com, gh's own default host and the host the world plays,
// refused where GitHub answers it as it answers the bare call (`gh api --hostname github.com
// user` matched no route, since the world's routes name the bare call): `--hostname` before or
// after the endpoint, and a `-R` of github.com's in its `HOST/OWNER/REPO` and URL forms. GitHub
// answers a role App's `user` with its 403 (rig.sh's fixture_write), and the named host gets the
// very same refusal. And the record keeping the call as the world answered it, so its readers
// read one repository.
test("a call that names github.com is answered as the bare call, wherever the flag stands", () => {
  const { dir } = runDir({ fixture: true });
  const user = ghCall(dir, "api", "user");
  expect(user).toEqual({
    code: 1,
    out: JSON.stringify({
      message: "Resource not accessible by integration",
      documentation_url: "https://docs.github.com/rest/users/users#get-the-authenticated-user",
      status: "403",
    }),
    err: "gh: Resource not accessible by integration (HTTP 403)\n",
  });
  expect(ghCall(dir, "api", "--hostname", "github.com", "user")).toEqual(user);
  expect(ghCall(dir, "api", "user", "--hostname=github.com")).toEqual(user);
  const view = answer(dir, ["pr", "view", "1001", "--json", "number", "--jq", ".number"]);
  expect(view).toBe("1001");
  for (const repo of [
    ["-R", "github.com/example/widgets"],
    ["--repo=https://github.com/example/widgets"],
  ])
    expect(
      answer(dir, ["pr", "view", "1001", ...repo, "--json", "number", "--jq", ".number"])
    ).toBe(view);
  expect(recordedCalls(dir).map((call) => call.argv)).toContainEqual([
    "pr",
    "view",
    "1001",
    "-R",
    "example/widgets",
    "--json",
    "number",
    "--jq",
    ".number",
  ]);
});

/** Every call line of the record under `dir`, the opening line left out. */
const recordedCalls = (dir: string) =>
  readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>)
    .filter((line) => !("opened" in line));

/** The Legion gh wrapper's refusal of a call naming `owner`, outside the project, under
 * PANE_OWNERS, as this checkout's wrapper prints it, main's since #4459: leakguard's
 * OwnerNotServedError alone, whatever form names the owner (gh-selectors.ts's `ownerRefusal`,
 * `one-line`), as the tester measured it on 2026-11-22 at 17:55Z. */
function ownerAloneRefused(owner: string): string {
  return `legion gh: the role's GitHub App is not installed on "${owner}": ${PANE_OWNERS} holds no gh directory for it\n`;
}
/** That refusal of a call naming one repository of such an owner, `repository`, and no other:
 * the same line, naming the repository's owner. */
const repositoryRefused = (repository: string) => ownerAloneRefused(repository.split("/")[0] ?? "");
/** The refusal of a call naming `owner` alone as main ff2a10ef's wrapper printed it on a Legion
 * pod on 2026-11-16 (`ownerRefusal`, `setup-error`): its own lookup of the owner's gh directory,
 * which the guard leaves it with no repository to read. */
function notOwnerRefused(owner: string): string {
  return `legion gh: "${owner}" is not an owner of this project: ${PANE_OWNERS} holds no gh directory for it\n`;
}
/** main ff2a10ef's refusal of a call naming one repository of such an owner and no other: the
 * leak guard's setup error over the wrapper's refusal of the guard's read of that repository's
 * visibility, as it printed it on a Legion pod on 2026-11-16. */
const setupErrorRefused = (repository: string) =>
  `legion gh: LEAK CHECK SETUP ERROR: the gh call: the visibility of ${repository} could not be read: gh api repos/${repository} exited 2: ${notOwnerRefused(repository.split("/")[0] ?? "")}`;
/** The read a reviewer of the rehearsal of 2026-11-16 made to check the commit the world's
 * workflow pins actions/checkout to, which the world answered `Not Found (HTTP 404)` from no
 * route; the label's wrapper refuses it, exit 2, from a pane that names no GH_CONFIG_DIR, and runs
 * it on the pane's token from one that does, which production's GitHub answers (the tester's
 * capture, pinned-actions.json). */
const TAG_READ = ["api", "repos/actions/checkout/git/refs/tags/v7.0.1"];
/** The four calls the tester measured through the real wrapper on this pod at 18:59Z on
 * 2026-11-16, which the world answered GitHub's 404 where production exits 2: one repository
 * named by its `<repository>` argument and by `-R`, and the owner alone, by `--owner` and by the
 * `<owner>` argument. Each with the line this checkout's wrapper prints (`repositoryRefused`,
 * `ownerAloneRefused`, one line since #4459), where main ff2a10ef's printed the guard's setup
 * error for the first two (`setupErrorRefused`) and its own lookup's for the last two
 * (`notOwnerRefused`). */
const MEASURED: readonly (readonly [argv: string[], line: string])[] = [
  [["repo", "view", "actions/checkout", "--json", "name"], repositoryRefused("actions/checkout")],
  [["release", "view", "v7.0.1", "-R", "actions/checkout"], repositoryRefused("actions/checkout")],
  [
    ["search", "repos", "checkout", "--owner", "actions", "--limit", "1"],
    ownerAloneRefused("actions"),
  ],
  [["repo", "list", "actions"], ownerAloneRefused("actions")],
];

/** Which line each kind of form of naming an owner gets, and the value that names `actions`
 * through it: a form naming one of its repositories gets `repositoryRefused`, one naming the owner
 * alone `ownerAloneRefused`, which this checkout's wrapper words alike; a kind naming no owner (an
 * argument of no selector's kind, a host, an option naming nothing) gets none. A query names
 * either by its qualifier. Every kind gh-selectors.ts models has a row, and a kind with none fails
 * the pin. */
const LINES_OF: Readonly<
  Record<"slot" | "option", Readonly<Record<string, readonly (readonly [string, string])[]>>>
> = {
  slot: {
    "": [],
    repository: [["actions/checkout", repositoryRefused("actions/checkout")]],
    "second-repository": [["actions/checkout", repositoryRefused("actions/checkout")]],
    owner: [["actions", ownerAloneRefused("actions")]],
    url: [["https://github.com/actions/checkout/pull/1", repositoryRefused("actions/checkout")]],
    query: [
      ["repo:actions/checkout", repositoryRefused("actions/checkout")],
      ["owner:actions", ownerAloneRefused("actions")],
    ],
    number: [["actions/checkout#1", repositoryRefused("actions/checkout")]],
    "new-repository": [["actions/checkout", repositoryRefused("actions/checkout")]],
  },
  option: {
    repository: [["actions/checkout", repositoryRefused("actions/checkout")]],
    "org-repositories": [["actions/checkout", repositoryRefused("actions/checkout")]],
    owner: [["actions", ownerAloneRefused("actions")]],
    "owner/name": [["actions/core", ownerAloneRefused("actions")]],
    url: [["https://github.com/actions/checkout/issues/1", repositoryRefused("actions/checkout")]],
    host: [],
    nothing: [],
  },
};

/** One form of the pin: the table's entry, the call naming `actions` through it, and the line. */
interface PinCase {
  readonly form: string;
  readonly argv: readonly string[];
  readonly line: string;
}

/** The pin's calls of a label, generated from its wrapper's own `table` rather than listed here:
 * for each entry (an argument name of ghSlotKinds or a leaf's own, an option of ghOptionKinds)
 * whose kind names an owner (LINES_OF), the first leaf of the label's gh (`selectors`, by path)
 * whose help spells it in that kind, called with the value naming `actions` through it and
 * nothing else — an argument after neutral ones (`1`) where the usage places it later — and the
 * line that kind gets. A number names a repository only where the leaf reads OWNER/REPO#N. An
 * entry no leaf of the label's help spells (a spelling of an older gh's the table holds too) is
 * `unreached`: no call of the label's gh can name it. */
function pinCases(
  table: GhTable,
  selectors: GhSelectors
): { cases: PinCase[]; unreached: string[] } {
  const cases: PinCase[] = [];
  const unreached: string[] = [];
  const leaves = Object.entries(selectors.leaves).sort(([a], [b]) => (a < b ? -1 : 1));
  const slotForm = (form: string, name: string, kind: string, only?: string) => {
    const lines = LINES_OF.slot[kind];
    if (lines === undefined) throw new Error(`the pin has no line for the argument kind ${kind}`);
    if (lines.length === 0) return;
    for (const [leaf, { slots, readsRepositoryNumber }] of leaves) {
      if (only !== undefined && leaf !== only) continue;
      const at = slots.findIndex(
        (slot) =>
          slot.names.includes(name) &&
          slot.unknown === "" &&
          (kind === "number" ? readsRepositoryNumber : slot.kind === kind)
      );
      const neutral = slots
        .slice(0, Math.max(at, 0))
        .every(
          (slot) => !slot.many && slot.unknown === "" && ["", "number", "url"].includes(slot.kind)
        );
      if (at < 0 || !neutral) continue;
      for (const [value, line] of lines)
        cases.push({ form, argv: [...leaf.split(" "), ...Array(at).fill("1"), value], line });
      return;
    }
    unreached.push(form);
  };
  for (const [name, { kind }] of Object.entries(table.slots))
    slotForm(`ghSlotKinds ${name} (${kind || "none"})`, name, kind);
  for (const [leaf, quirk] of Object.entries(table.quirks))
    for (const [name, { kind }] of Object.entries(quirk.slots))
      slotForm(`ghLeafQuirks ${leaf} ${name} (${kind})`, name, kind, leaf);
  for (const [key, kind] of Object.entries(table.options)) {
    const lines = LINES_OF.option[kind];
    if (lines === undefined) throw new Error(`the pin has no line for the option kind ${kind}`);
    if (lines.length === 0) continue;
    const words = key.split(" ");
    const at = words.findIndex((word) => word.startsWith("--"));
    const only = words.slice(0, at).join(" ");
    const name = (words[at] ?? "").slice(2);
    const placeholder = words.slice(at + 1).join(" ");
    const found = leaves.find(
      ([leaf, { flags }]) =>
        (only === "" ? table.options[`${leaf} ${key}`] === undefined : leaf === only) &&
        flags[name]?.placeholder === placeholder &&
        flags[name]?.selects === kind &&
        flags[name]?.uncertain === false
    );
    if (found === undefined) unreached.push(`ghOptionKinds ${key} (${kind})`);
    else
      for (const [value, line] of lines)
        cases.push({
          form: `ghOptionKinds ${key} (${kind})`,
          argv: [...found[0].split(" "), `--${name}`, value],
          line,
        });
  }
  return { cases, unreached };
}

/** `gh <args...>` asked of the stand-in in the run under `dir` as the role `by`, under `vars`: its
 * status, its stdout and its stderr. */
function asked(
  dir: string,
  by: string | undefined,
  args: readonly string[],
  vars: Record<string, string> = {}
) {
  const ran = spawnChecked(["bun", STANDIN, "gh", ...args], { env: callEnv(dir, by, vars) });
  return [ran.exitCode, ran.stdout.toString(), ran.stderr.toString()];
}

// Catches: a world that answers a call naming another owner from its routes (the 404 of no route,
// measured in the rehearsal and again by the tester at 18:59Z on 2026-11-16) where production's
// Legion gh wrapper refuses it before gh runs, exit 2, naming the owner; one that does so for a
// form of naming it the wrapper's selector table holds and the world does not read (the
// `<repository>` and `<owner>` arguments and `--owner` the tester met); one that answers a form
// with the other of the wrapper's two lines; and one whose forms are a copy here rather than the
// label's own table, which a form the wrapper learns later would leave behind. The pin runs every
// form the table of this checkout's wrapper holds, and the four measured calls among them.
test("every form the wrapper's selector table holds, naming an owner outside the project, is refused exit 2 with the wrapper's line for it", () => {
  const table = labelGhTable(CHECKOUT);
  const selectors = labelGhSelectors(CHECKOUT);
  expect(Object.keys(LINES_OF.slot).sort()).toEqual([...MODELLED.slot].sort());
  expect(Object.keys(LINES_OF.option).sort()).toEqual([...MODELLED.option].sort());
  const { cases, unreached } = pinCases(table, selectors);
  // Each kind naming an owner is a form some leaf of this checkout's gh spells: a number only where
  // the wrapper reads OWNER/REPO#N, which main's since #4424 reads for no leaf.
  const named = (type: "slot" | "option") =>
    Object.entries(LINES_OF[type])
      .filter(([, lines]) => lines.length > 0)
      .map(([kind]) => kind)
      .filter(
        (kind) =>
          type === "option" || kind !== "number" || table.patterns.ghRepositoryNumber !== undefined
      )
      .sort();
  const reached = (type: "slot" | "option") =>
    [
      ...new Set(
        cases
          .filter((one) => one.form.startsWith("ghOption") === (type === "option"))
          .map((one) => /\(([^)]*)\)$/.exec(one.form)?.[1] ?? "")
      ),
    ].sort();
  expect(reached("slot")).toEqual(named("slot"));
  expect(reached("option")).toEqual(named("option"));
  expect(cases.length).toBeGreaterThanOrEqual(30);
  // A leaf's own argument kind is always one its own help spells.
  expect(unreached.filter((form) => form.startsWith("ghLeafQuirks"))).toEqual([]);
  const { dir } = runDir({ fixture: true });
  const all = [...cases, ...MEASURED.map(([argv, line]) => ({ form: "measured", argv, line }))];
  expect(all.map(({ form, argv }) => [form, argv.join(" "), asked(dir, "reviewer", argv)])).toEqual(
    all.map(({ form, argv, line }) => [form, argv.join(" "), [2, "", line]])
  );
}, 180_000);

// Catches: OWNER/REPO#N read by one shape of the wrapper alone. Main's wrapper before #4424
// (fdce4e94d2) declared ghRepositoryNumber and read it in `agent-task view`'s number
// (readsRepositoryNumber); main's since declares neither and refuses `gh agent-task` whole. A
// reader that required the pattern refused every batch of a label at main 9aff682b (`the
// wrapper's ghRepositoryNumber: is not declared as regexp.MustCompile of one expression`), and one
// that read a number as a repository with no quirk reading it would refuse a call that wrapper
// reads no owner in. A table holding the pattern with no quirk reading it, or a quirk with no
// pattern, is neither shape, and is refused.
test("OWNER/REPO#N is read by the shape of the wrapper: its pattern and a quirk reading it before #4424, neither since", () => {
  const view = ["agent-task", "view", "actions/checkout#1"];
  const since = labelGhSelectors(CHECKOUT);
  expect(labelGhTable(CHECKOUT).patterns.ghRepositoryNumber).toBeUndefined();
  expect(since.patterns.ghRepositoryNumber).toBeUndefined();
  expect(
    Object.entries(since.leaves)
      .filter(([, leaf]) => leaf.readsRepositoryNumber)
      .map(([path]) => path)
  ).toEqual([]);
  expect(ghTargetOf(view, since, {})).toEqual({ owners: [], repo: "" });
  const pattern = "var ghRepositoryNumber = regexp.MustCompile(`^([^/\\s#]+/[^/\\s#]+)#[0-9]+$`)\n";
  const quirk = '"agent-task view": {readsRepositoryNumber: true, pullRequestFinder: true},';
  const before = labelGhSelectors(
    substituteCheckout((source) => `${withEntry(source, "ghLeafQuirks", quirk)}\n${pattern}`)
  );
  expect(before.patterns.ghRepositoryNumber).toBe("^([^/\\s#]+/[^/\\s#]+)#[0-9]+$");
  expect(ghTargetOf(view, before, {})).toEqual({
    owners: [["actions", "the issue or pull request actions/checkout#1"]],
    repo: "checkout",
  });
  expect(() => labelGhSelectors(substituteCheckout((source) => `${source}\n${pattern}`))).toThrow(
    "the wrapper declares ghRepositoryNumber and no leaf's quirk reads it (readsRepositoryNumber), which is neither shape"
  );
  expect(() =>
    labelGhSelectors(substituteCheckout((source) => withEntry(source, "ghLeafQuirks", quirk)))
  ).toThrow(
    "the wrapper's ghLeafQuirks read OWNER/REPO#N for agent-task view (readsRepositoryNumber) and it declares no ghRepositoryNumber"
  );
}, 120_000);

// Catches: an option line read by the group numbers of one shape of the wrapper's pattern. #4418
// put the boolean marker gh 2.104.0 prints (`--web[=true|false]`) third in gh_help.go's
// ghFlagLine, so a reader that kept taking the third group as the placeholder read every option of
// both labels as taking no value: `--repo` named no repository, no option form was refused and
// GH_REPO was never read beside an argument. Each shape reads its own groups, a line the wrapper
// refuses (a boolean marker and a value both) is refused, and so is a pattern of a third shape.
test("an option line is read by the shape of the wrapper's pattern: four groups before #4418, five since", () => {
  const table = labelGhTable(CHECKOUT);
  const since = optionLines(new RegExp(table.patterns.ghFlagLine), table.declared.ghFlagLine);
  const before = optionLines(
    /^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}(.*)|\s*)$/,
    "gh_help.go before #4418"
  );
  const repo = "  -R, --repo [HOST/]OWNER/REPO   Select another repository";
  for (const read of [since, before])
    expect(read(repo)).toEqual({
      short: "R",
      name: "repo",
      placeholder: "[HOST/]OWNER/REPO",
      description: "Select another repository",
    });
  expect(since("  -w, --web[=true|false]   Open in the browser")).toEqual({
    short: "w",
    name: "web",
    placeholder: "",
    description: "Open in the browser",
  });
  expect(before("  -w, --web[=true|false]   Open in the browser")).toBeUndefined();
  expect(labelGhSelectors(CHECKOUT).leaves["pr view"]?.flags.repo).toMatchObject({
    value: true,
    selects: "repository",
  });
  expect(() => since("      --web[=true|false] string   Open it")).toThrow(
    "which marks its option boolean and names a value"
  );
  expect(() => optionLines(/^--([a-z]+)$/, "gh_help.go")).toThrow("has 1 capture groups");
});

// Catches: a refusal for one role and not another; a refusal read from text a call sends (a body
// naming another owner's URL); one of the world's own owner in another case; a pane naming no
// owners directory answered as one that names it; and a world refusing for a run with no world.
test("a call naming an owner outside the world's project is refused as the Legion gh wrapper refuses it, exit 2, for every role", () => {
  const { dir } = runDir({ fixture: true });
  for (const by of ["reviewer", "implementer", "tester", "planner", "merger", undefined])
    expect(asked(dir, by, TAG_READ)).toEqual([2, "", repositoryRefused("actions/checkout")]);
  for (const args of [
    ["api", "/repos/Actions/Checkout/contents/README.md"],
    ["api", "https://api.github.com/repos/actions/checkout/tags"],
    ["pr", "view", "1", "-R", "actions/checkout"],
    ["pr", "view", "1", "--repo=github.com/actions/checkout"],
    ["pr", "view", "https://github.com/actions/checkout/pull/1"],
  ])
    expect(asked(dir, "reviewer", args)).toEqual([2, "", repositoryRefused("actions/checkout")]);
  expect(asked(dir, "reviewer", ["api", "repos/oven-sh/setup-bun/git/refs/tags/v2.2.0"])).toEqual([
    2,
    "",
    repositoryRefused("oven-sh/setup-bun"),
  ]);
  expect(asked(dir, "reviewer", ["api", "orgs/actions/repos"])).toEqual([
    2,
    "",
    ownerAloneRefused("actions"),
  ]);
  expect(
    recordedCalls(dir)
      .slice(0, 6)
      .map((call) => [call.by, call.exit])
  ).toEqual([
    ["reviewer", 2],
    ["implementer", 2],
    ["tester", 2],
    ["planner", 2],
    ["merger", 2],
    [undefined, 2],
  ]);
  const unset = spawnChecked(["bun", STANDIN, "gh", ...TAG_READ], {
    env: Object.fromEntries(
      Object.entries(callEnv(dir, "reviewer")).filter(([name]) => name !== "LEGION_GH_OWNERS_DIR")
    ),
  });
  expect([unset.exitCode, unset.stderr.toString()]).toEqual([
    2,
    "legion gh: LEGION_GH_OWNERS_DIR is unset: a Legion pane or pod sets it, and nothing else runs this gh\n",
  ]);
  // The world's own owner in another case, and an owner's URL in the text a call sends, are no
  // such call.
  for (const args of [
    ["api", "repos/EXAMPLE/widgets/pulls/1001", "--jq", ".number"],
    ["pr", "view", "1001", "-R", "Example/Widgets", "--json", "number", "--jq", ".number"],
    ["pr", "comment", "1001", "--body", "https://github.com/actions/checkout"],
  ]) {
    const [status, , err] = asked(dir, "reviewer", args);
    expect([args.join(" "), status === 2 || String(err).includes("is not an owner")]).toEqual([
      args.join(" "),
      false,
    ]);
  }
  // A run with no world names no project, and its calls are answered from the routes.
  const bare = runDir({ fixture: false }).dir;
  expect(asked(bare, undefined, TAG_READ)[0]).toBe(1);
}, 60_000);

// Catches: a world that reads no GH_REPO where the wrapper reads it, so a call naming another owner
// beside the repository gh resolves from the pane's GH_REPO (the world's) gets the one-owner line
// where production names both owners; and one reading it where the wrapper does not, which would
// give the measured `--owner` call the two-owner line. The words that name both are main
// ff2a10ef's (`olderRefusal`); this checkout's wrapper names the first owner it holds no directory
// for, whatever else the call names (the row on each label's own words).
test("a call naming an owner outside the project beside the pane's GH_REPO is refused naming both, where the wrapper reads GH_REPO", () => {
  const { dir } = runDir({ fixture: true });
  writeFileSync(
    path.join(dir, "gh-selectors.json"),
    `${JSON.stringify(labelGhSelectors(olderRefusal()))}\n`
  );
  const pane = { GH_REPO: "example/widgets" };
  expect(asked(dir, "reviewer", ["issue", "transfer", "3", "actions/checkout"], pane)).toEqual([
    2,
    "",
    "legion gh: the command names more than one owner, actions (the repository actions/checkout) and example (GH_REPO example/widgets), so it sends no token; name one repository\n",
  ]);
  expect(
    asked(
      dir,
      "reviewer",
      ["issue", "edit", "5", "--add-blocked-by", "https://github.com/actions/checkout/issues/9"],
      pane
    )
  ).toEqual([
    2,
    "",
    "legion gh: the command names more than one owner, actions (--add-blocked-by https://github.com/actions/checkout/issues/9) and example (GH_REPO example/widgets), so it sends no token; name one repository\n",
  ]);
  for (const [argv, line] of MEASURED_OLDER)
    expect(asked(dir, "reviewer", argv, pane)).toEqual([2, "", line]);
}, 120_000);

/** MEASURED's calls with the line main ff2a10ef's wrapper printed for each on 2026-11-16. */
const MEASURED_OLDER: readonly (readonly [argv: string[], line: string])[] = [
  [["repo", "view", "actions/checkout", "--json", "name"], setupErrorRefused("actions/checkout")],
  [["release", "view", "v7.0.1", "-R", "actions/checkout"], setupErrorRefused("actions/checkout")],
  [
    ["search", "repos", "checkout", "--owner", "actions", "--limit", "1"],
    notOwnerRefused("actions"),
  ],
  [["repo", "list", "actions"], notOwnerRefused("actions")],
];

/** The pane's own gh directory under PANE_OWNERS, the world's owner's, as daemon-pane.go renders a
 * role's claim: the GH_CONFIG_DIR with which main's wrapper since #4035 runs a read naming only
 * owners outside the project (gh-pane-token.ts's `runsOnPaneToken`). */
const PANE_GH = { GH_CONFIG_DIR: `${PANE_OWNERS}/example` };
/** A GraphQL query naming the owner in a field, which that wrapper runs so too
 * (gh_reads.go's ghGraphQLQueryRead). */
const OWNER_QUERY = [
  "api",
  "graphql",
  "-f",
  "query=query($owner: String!) { repositoryOwner(login: $owner) { login } }",
  "-F",
  "owner=actions",
];

/** A checkout of this one's wrapper as main's before #4035 (cac1693d) wrote it: its selectors'
 * `dir` takes no fallback, so every item the bundle holds no directory for is refused
 * (daemon-pane.ts's OWNER_ALWAYS_REFUSED); with `half`, gh.go alone is left as it is, a wrapper of
 * neither shape. */
function olderWrapper(half = false): string {
  const checkout = substituteCheckout((source) => source);
  const legion = path.join(checkout, "packages", "daemon", "cmd", "legion");
  const rewrite = (file: string, from: string, to: string) => {
    const source = readFileSync(path.join(legion, file), "utf8");
    if (!source.includes(from)) throw new Error(`${file} holds no ${JSON.stringify(from)}`);
    writeFileSync(path.join(legion, file), source.replace(from, to));
  };
  rewrite(
    "gh_selectors.go",
    "func (s *selectors) dir(dirFor func(owner, name string) (string, error), fallback string) (string, error) {",
    "func (s *selectors) dir(dirFor func(owner, name string) (string, error)) (string, error) {"
  );
  if (!half)
    rewrite(
      "gh.go",
      'fallback := ""\n\tif writesNothing || ghGraphQLQueryRead(argv) {\n\t\tfallback = env["GH_CONFIG_DIR"]\n\t}\n\tdir, err := sel.dir(dirFor, fallback)\n',
      "dir, err := sel.dir(dirFor)\n"
    );
  return checkout;
}

/** A checkout of this one's wrapper with main ff2a10ef's words for a call naming an owner its
 * bundle holds no directory for (daemon-pane.ts's OWNER_SETUP_ERROR): gitcredential.go's older
 * refusal in place of leakguard's OwnerNotServedError, and ff2a10ef's gh.go:301, its refusal of a
 * call naming two owners. With `both`, OwnerNotServedError stays as well, a wrapper of both
 * shapes. */
function olderRefusal(both = false): string {
  const checkout = substituteCheckout((source) => source);
  const legion = path.join(checkout, "packages", "daemon", "cmd", "legion");
  const credential = path.join(legion, "gitcredential.go");
  const notServed = 'return "", &leakguard.OwnerNotServedError{Owner: owner, OwnersDir: ownersDir}';
  const older =
    'return "", fmt.Errorf("%q is not an owner of this project: %s holds no gh directory for it", owner, ownersDir)';
  const source = readFileSync(credential, "utf8");
  if (!source.includes(notServed)) throw new Error(`gitcredential.go holds no ${notServed}`);
  writeFileSync(
    credential,
    both
      ? `${source}\nfunc olderRefusal() (string, error) {\n\t${older}\n}\n`
      : source.replace(notServed, older)
  );
  appendFileSync(
    path.join(legion, "gh.go"),
    '\nfunc twoOwners(listed []string) (ghTarget, error) {\n\treturn ghTarget{}, fmt.Errorf("the command names more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))\n}\n'
  );
  return checkout;
}

// Each label's wrapper refuses a call naming an owner its bundle holds no gh directory for in its
// own words, read off its own source (daemon-pane.ts's `labelGhSelectors`, `unservedOwner`; the
// architect's rulings of 2026-11-22T18:00:12Z and 18:02:26Z): main's since #4459, which this
// checkout's is, prints leakguard's OwnerNotServedError alone for the first such owner a call
// names (visibility.go, `legion gh: the role's GitHub App is not installed on "<owner>": <dir>
// holds no gh directory for it`, measured by the tester at 2026-11-22T17:55Z), and main ff2a10ef's
// prints the leak guard's setup error over its own older words, or its refusal of two owners.
// Catches: a world that prints one label's words for the other's (the 2026-11-16 words for this
// checkout's, as it did until round 34); and one that takes a source holding the words of both,
// or of neither, for either.
test("the world refuses an owner outside the project in its label's own words: since #4459 one line, and main ff2a10ef's setup error", () => {
  const { dir } = runDir({ fixture: true });
  expect(labelGhSelectors(CHECKOUT).unservedOwner).toBe("one-line");
  const named = ["issue", "transfer", "3", "actions/checkout"];
  const pane = { GH_REPO: "example/widgets" };
  for (const [argv, line] of [...MEASURED, [named, ownerAloneRefused("actions")] as const])
    expect(asked(dir, "reviewer", argv, pane)).toEqual([2, "", line]);
  const older = labelGhSelectors(olderRefusal());
  expect(older.unservedOwner).toBe("setup-error");
  writeFileSync(path.join(dir, "gh-selectors.json"), `${JSON.stringify(older)}\n`);
  for (const [argv, line] of MEASURED_OLDER)
    expect(asked(dir, "reviewer", argv, pane)).toEqual([2, "", line]);
  expect(() => labelGhSelectors(olderRefusal(true))).toThrow(
    "is no shape of the gh wrapper's refusal of an owner it holds no directory for this rig reads: it holds the markers of two shapes at once"
  );
  const neither = substituteCheckout((source) => source);
  rmSync(path.join(neither, "packages", "daemon", "internal", "leakguard"), { recursive: true });
  expect(() => labelGhSelectors(neither)).toThrow(
    "is no shape of the gh wrapper's refusal of an owner it holds no directory for this rig reads"
  );
}, 120_000);

// Main's wrapper since #4035 (cac1693d) runs a call that writes nothing, names only owners the
// pane's bundle holds no directory for, and reads no working-directory remote, with the pane's
// own GH_CONFIG_DIR rather than refusing it (gh.go's ghTargetOf, `fallback`; gh_selectors.go's
// dir), and this checkout's wrapper is of that shape. Catches: a world that still refuses such a
// read, exit 2; one that runs a write so, a read typed outside the wrapper's read grammar
// (`--web`, `gh api -i`, which legion gh refused at 2026-11-22T17:55Z), or a call naming the
// world's owner beside another, each of which that wrapper still refuses; and one that runs such
// a read for a pane naming no GH_CONFIG_DIR, whose fallback is none. Each read gets the world's
// GitHub's answer: for TAG_READ, a read round 33's agents made of an action the world's workflow
// pins, production's own (pinned-actions.json; the architect's ruling of 2026-11-22T18:00:12Z);
// for a repository the world does not hold, its 404, exit 1, as GitHub answers a token that
// cannot see it; and for the GraphQL query its answer for an owner it does not hold, which
// resolves to none.
test("on main's wrapper since #4035, a read naming only owners outside the project runs on the pane's token and gets the world's GitHub's answer", () => {
  const { dir } = runDir({ fixture: true });
  expect(labelGhSelectors(CHECKOUT).paneToken).toBeDefined();
  const [tagStatus, tagOut, tagErr] = asked(dir, "reviewer", TAG_READ, PANE_GH);
  expect([tagStatus, JSON.parse(String(tagOut)).object?.sha, tagErr]).toEqual([
    0,
    "3d3c42e5aac5ba805825da76410c181273ba90b1",
    "",
  ]);
  for (const [argv] of MEASURED) {
    const [status, , err] = asked(dir, "reviewer", argv, PANE_GH);
    expect([argv.join(" "), status, err]).toEqual([
      argv.join(" "),
      1,
      "gh: Not Found (HTTP 404)\n",
    ]);
  }
  expect(asked(dir, "reviewer", OWNER_QUERY, PANE_GH)).toEqual([
    0,
    '{"data":{"repositoryOwner":null}}',
    "",
  ]);
  expect(
    recordedCalls(dir).map((call) => [(call.argv as string[]).join(" "), call.handler])
  ).toEqual([
    [TAG_READ.join(" "), "pinned read"],
    ...MEASURED.map(([argv]) => [argv.join(" "), "unrouted"]),
    [OWNER_QUERY.join(" "), "graph"],
  ]);
  for (const [argv, line] of [
    [
      ["issue", "create", "-R", "actions/checkout", "--title", "t", "--body", "b"],
      repositoryRefused("actions/checkout"),
    ],
    [
      ["api", "-X", "POST", "repos/actions/checkout/issues", "-f", "title=t"],
      repositoryRefused("actions/checkout"),
    ],
    [["repo", "view", "actions/checkout", "--web"], repositoryRefused("actions/checkout")],
    [["api", "-X", "POST", "repos/actions/checkout/tags"], repositoryRefused("actions/checkout")],
    [["api", "-X", "get", "repos/actions/checkout/tags"], repositoryRefused("actions/checkout")],
    [["api", "-i", "repos/actions/checkout"], repositoryRefused("actions/checkout")],
    [
      ["search", "repos", "checkout", "--owner", "actions", "--owner", "example"],
      ownerAloneRefused("actions"),
    ],
  ] as const)
    expect(asked(dir, "reviewer", argv, PANE_GH)).toEqual([2, "", line]);
  expect(asked(dir, "reviewer", TAG_READ)).toEqual([2, "", repositoryRefused("actions/checkout")]);
  // A `gh api` read may name its method only as exactly GET (gh_reads.go's ghAPIReadValue); it runs
  // on the pane's token, and capture v4 holds production's answer to this read, its 30 tags.
  const [status, out, err] = asked(
    dir,
    "reviewer",
    ["api", "-X", "GET", "repos/actions/checkout/tags"],
    PANE_GH
  );
  expect([status, JSON.parse(String(out)).length, err]).toEqual([0, 30, ""]);
}, 60_000);

// Main's wrapper before #4035 refuses every call naming an owner outside the project, a read on a
// pane that names its GH_CONFIG_DIR among them, and the world reads which shape a label's wrapper
// is off its source (daemon-pane.ts's PANE_TOKEN_READ and OWNER_ALWAYS_REFUSED). Catches: a world
// that runs such a read on the pane's token whatever the label's wrapper does; and one that takes a
// source of neither shape for either.
test("on main's wrapper before #4035, a read naming only owners outside the project is refused exit 2, as every such call", () => {
  const { dir } = runDir({ fixture: true });
  const older = labelGhSelectors(olderWrapper());
  expect(older.paneToken).toBeUndefined();
  writeFileSync(path.join(dir, "gh-selectors.json"), `${JSON.stringify(older)}\n`);
  const cases: readonly (readonly [argv: string[], line: string])[] = [
    [TAG_READ, repositoryRefused("actions/checkout")],
    ...MEASURED,
  ];
  for (const [argv, line] of cases)
    expect(asked(dir, "reviewer", argv, PANE_GH)).toEqual([2, "", line]);
  expect(() => labelGhSelectors(olderWrapper(true))).toThrow(
    "is no shape of the gh wrapper's answer to a read naming only owners it holds no directory for this rig reads"
  );
}, 120_000);

// Catches: a world that answers a call of a world whose gh selectors are not there as though it
// named no other owner, which is the rig having lost the label's table; and one whose selectors
// name no words of the label's refusal of such an owner (written before the rig read them), which
// would refuse it in words no label of the world's gives.
test("a world whose gh selectors are not there answers no call, naming the file", () => {
  const { dir } = runDir({ fixture: true });
  rmSync(path.join(dir, "gh-selectors.json"));
  const [status, , err] = asked(dir, "reviewer", ["pr", "view", "1001"]);
  expect([status, err]).toEqual([
    2,
    `gh: ${dir}/gh-selectors.json is not there, so no owner of this world's project is known (rig.sh's worker_boot writes it)\n`,
  ]);
  writeFileSync(path.join(dir, "gh-selectors.json"), "{not json");
  const [refused, , why] = asked(dir, "reviewer", ["pr", "view", "1001"]);
  expect([
    refused,
    String(why).startsWith(`gh: ${dir}/gh-selectors.json cannot be read: `),
  ]).toEqual([2, true]);
  const { unservedOwner: _, ...wordless } = labelGhSelectors(CHECKOUT);
  writeFileSync(path.join(dir, "gh-selectors.json"), `${JSON.stringify(wordless)}\n`);
  expect(asked(dir, "reviewer", ["pr", "view", "1001"])).toEqual([
    2,
    "",
    `gh: ${dir}/gh-selectors.json names no words of the wrapper's refusal of an owner outside this world's project (rig.sh's worker_boot writes them, daemon-pane.ts's labelGhSelectors)\n`,
  ]);
}, 60_000);

/** A checkout holding only what a label's gh selectors are read from — the wrapper's package,
 * internal/ghconfig, internal/leakguard (whose words the wrapper refuses an owner in) and the
 * wrapper's saved gh help — copied from this one, with `edit` applied to the wrapper's gh_help.go:
 * a later wrapper's own source. */
function substituteCheckout(edit: (source: string) => string): string {
  const checkout = mkdtempSync(path.join(tmpdir(), "gh-selectors-label-"));
  made.push(checkout);
  for (const dir of ["cmd/legion", "internal/ghconfig", "internal/leakguard"]) {
    const from = path.join(DAEMON_MODULE, dir);
    const to = path.join(checkout, "packages", "daemon", dir);
    mkdirSync(to, { recursive: true });
    for (const name of readdirSync(from))
      if (name.endsWith(".go") && !name.endsWith("_test.go"))
        cpSync(path.join(from, name), path.join(to, name));
  }
  const legion = path.join(checkout, "packages", "daemon", "cmd", "legion");
  cpSync(path.join(DAEMON_MODULE, "cmd", "legion", "testdata"), path.join(legion, "testdata"), {
    recursive: true,
  });
  const help = path.join(legion, "gh_help.go");
  writeFileSync(help, edit(readFileSync(help, "utf8")));
  return checkout;
}

/** `source` with `entry` added as the first entry of its map literal `name`. */
function withEntry(source: string, name: string, entry: string): string {
  const opening = new RegExp(`^var ${name} = map\\[string\\][A-Za-z]+\\{\\n`, "m");
  if (!opening.test(source)) throw new Error(`gh_help.go declares no ${name} map literal`);
  return source.replace(opening, (found) => `${found}\t${entry}\n`);
}

// Catches: a world that reads a later wrapper's selector form it does not model as no form, which
// answers a call through it otherwise than production with nothing saying so; and one that reads
// a label's forms out of any checkout but that label's own. Each label's table is read from its
// own checkout, so a later wrapper's new kind of option, of argument or of quirk fails that label's
// read, by which services_up fails the batch (label_facts) and worker_boot the run, naming it, and
// the pin of that label with it, while a new entry of a kind the world models is read with no
// edit and is a case of that label's pin.
test("a selector form a label's wrapper holds that the world does not model fails that label's read and its pin, naming it", () => {
  const same = substituteCheckout((source) => source);
  expect(labelGhSelectors(same)).toEqual(labelGhSelectors(CHECKOUT));
  for (const [edit, refusal] of [
    [
      (source: string) => withEntry(source, "ghOptionKinds", '"--mirror string": "mirror",'),
      `the wrapper's table holds the option kind "mirror" (ghOptionKinds --mirror string), which the world's gh does not model (gh-selectors.ts's MODELLED)`,
    ],
    [
      (source: string) => withEntry(source, "ghSlotKinds", '"mirror": {kind: "mirror"},'),
      `the wrapper's table holds the argument kind "mirror" (ghSlotKinds mirror), which the world's gh does not model (gh-selectors.ts's MODELLED)`,
    ],
    [
      (source: string) => withEntry(source, "ghLeafQuirks", '"pr view": {readsMirror: true},'),
      `the wrapper's table holds the quirk "readsMirror" (ghLeafQuirks pr view), which the world's gh does not model (gh-selectors.ts's MODELLED)`,
    ],
    [
      (source: string) =>
        source.replace(
          "var ghKindRank = map[string]int{",
          'var ghKindRank = map[string]int{"mirror": 8, '
        ),
      `the wrapper's table holds the argument kind "mirror" (ghKindRank), which the world's gh does not model (gh-selectors.ts's MODELLED)`,
    ],
  ] as const) {
    const later = substituteCheckout(edit);
    expect(() => labelGhSelectors(later)).toThrow(refusal);
    const file = path.join(later, "gh-selectors.json");
    const written = spawnChecked(
      ["bun", path.join(import.meta.dir, "daemon-pane.ts"), "gh-selectors", later, file],
      { env: { ...process.env } }
    );
    expect([written.exitCode, written.stderr.toString().includes(refusal)]).toEqual([1, true]);
    expect(existsSync(file)).toBe(false);
  }
  // A new entry of a kind the world models is a form of that label's pin, and of no other's.
  const learned = substituteCheckout((source) =>
    withEntry(source, "ghOptionKinds", '"repo view --branch string": "repository",')
  );
  const form = "ghOptionKinds repo view --branch string (repository)";
  const ofLabel = (checkout: string) =>
    pinCases(labelGhTable(checkout), labelGhSelectors(checkout)).cases.filter(
      (one) => one.form === form
    );
  expect(ofLabel(learned)).toEqual([
    {
      form,
      argv: ["repo", "view", "--branch", "actions/checkout"],
      line: repositoryRefused("actions/checkout"),
    },
  ]);
  expect(ofLabel(CHECKOUT)).toEqual([]);
}, 120_000);

/** health.ts's reasons for a read of an owner outside the project the world answered otherwise
 * than production's gh answers it (`outsideAnswer`): a `gh api` read of the label's read grammar,
 * which production runs on the issue's token and GitHub answers, exit 0; and a read by any other gh
 * command, or of a label whose wrapper reads no grammar, which fails closed. */
// A read of the grammar no capture holds fails closed under the architect's ruling of
// 2026-11-23T05:52:25Z (health.ts's OUTSIDE_READ_UNCAPTURED), where it read as unanswered before.
const READ_UNCAPTURED =
  "a read of an owner outside the project in the Legion gh wrapper's read grammar that no capture of production's answer holds, so it fails closed: capture it";
const FAILS_CLOSED =
  "a read of an owner outside the project by a gh command other than a `gh api` call of the label's read grammar, which no answer of production's gh held here covers, so it fails closed";

// Catches: a health that reads the wrapper's refusal of a write naming an owner outside the
// project, exit 2, as a refusal the scenario does not declare, for any role of any worker world;
// one that takes such a write answered with any other status as production's answer; one that
// takes a read of such an owner by a gh command other than `gh api`, refused exit 2, as
// production's answer; and one that judges a refused call of a run whose label's table cannot be
// read as any refusal at all. The read fails closed by v79's rule (the tester's world-health.py,
// the architect's ruling (a) of 2026-11-22): production's wrapper runs such a read of its read
// grammar on the issue's token and GitHub answers it (`gh repo view actions/checkout --json
// name`), which the world cannot, and refuses one outside it, exit 2 (`--web`), the class this
// fails closed on, each a run replaced and never a wrong count.
test("health declares the wrapper's refusal of a write naming an owner outside the project for every role, by its exit 2, and fails a read of one by any other gh command closed", () => {
  const { dir } = runDir({ fixture: true });
  const roles = ["reviewer", "implementer", "tester"];
  const writes = [
    ["issue", "create", "-R", "actions/checkout", "--title", "t", "--body", "b"],
    ["api", "-X", "POST", "repos/actions/checkout/issues", "-f", "title=t"],
  ];
  for (const by of roles) for (const argv of writes) asked(dir, by, argv);
  expect(recordedCalls(dir).map((call) => [call.by, call.exit])).toEqual(
    roles.flatMap((by) => writes.map(() => [by, 2]))
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  const calls = path.join(dir, "calls.jsonl");
  const refused = readFileSync(calls, "utf8");
  appendFileSync(
    calls,
    `${JSON.stringify({ at: "t", as: "gh", by: "implementer", argv: writes[0], files: {}, route: null, exit: 1 })}\n`
  );
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    "gh refusal: calls.jsonl:8, `gh issue create -R actions/checkout --title t --body b` of the implementer, exited 1: the refusal of an owner outside the project with another status than the Legion gh wrapper's 2, which threads-closed does not declare for that role"
  );
  writeFileSync(calls, refused);
  for (const [argv] of MEASURED) expect(asked(dir, "reviewer", argv)[0]).toBe(2);
  expect(ghUndeclared({ dir, scenario: "tester-proof" })).toBe(
    `gh refusal: calls.jsonl:8, \`gh repo view actions/checkout --json name\` of the reviewer, exited 2: ${FAILS_CLOSED}, which tester-proof does not declare for that role`
  );
  // Selectors that cannot be read leave which refusal each was unknown; a run holding none was
  // answered by a world that refused no owner by them, and its calls are judged by their routes.
  writeFileSync(path.join(dir, "gh-selectors.json"), "{not json");
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toStartWith(
    `gh refusal unknown: calls.jsonl:2, \`gh issue create -R actions/checkout --title t --body b\` of the reviewer, exited 2, and the selectors its world refused an owner outside the project by cannot be read: ${dir}/gh-selectors.json cannot be read: `
  );
  rmSync(path.join(dir, "gh-selectors.json"));
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    "gh refusal: calls.jsonl:2, `gh issue create -R actions/checkout --title t --body b` of the reviewer, exited 2: gh: Not Found (HTTP 404) (no route of the world answers it), which threads-closed does not declare for that role"
  );
}, 60_000);

// The architect's ruling (a) of 2026-11-22 (16:36:59Z and 17:57Z), which overturns this row's
// older reading that the world's 404 to a read the label's wrapper runs on the pane's token is
// production's answer: legion gh runs a `gh api` read of its read grammar on the issue's token and
// GitHub answers it, exit 0 (the tester's capture), and refuses any other `gh api` form of another
// owner, `-i` among them, exit 2. Catches: a health that calls the world's answer from the capture,
// or its refusal of `-i`, unhealthy for any role; one that declares the world's 404 to a `gh api`
// read of the grammar, or to a read by another gh command the wrapper runs on the token; one that
// declares a write refused otherwise than exit 2; and one that holds a read of a label whose
// wrapper reads no grammar (main's before #4035) to this checkout's, rather than failing it closed.
test("health holds a read the label's wrapper runs on the pane's token to production's answer, for every role", () => {
  const { dir } = runDir({ fixture: true });
  const roles = ["reviewer", "implementer", "tester"];
  const included = ["api", "-i", ...TAG_READ.slice(1)];
  for (const by of roles) for (const argv of [TAG_READ, included]) asked(dir, by, argv, PANE_GH);
  expect(recordedCalls(dir).map((call) => [call.by, call.exit, call.handler])).toEqual(
    roles.flatMap((by) => [
      [by, 0, "pinned read"],
      [by, 2, "wrapper"],
    ])
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  const calls = path.join(dir, "calls.jsonl");
  const answered = readFileSync(calls, "utf8");
  // A read of the grammar no capture holds (capture v4 holds `…/tags`, so this one is a branch
  // list), which the world's GitHub answers 404.
  const uncaptured = ["api", "repos/actions/checkout/branches"];
  const [view] = MEASURED[0] ?? [[]];
  for (const [argv, why] of [
    [uncaptured, READ_UNCAPTURED],
    [view, FAILS_CLOSED],
  ] as const) {
    writeFileSync(calls, answered);
    expect(asked(dir, "reviewer", argv, PANE_GH)[0]).toBe(1);
    expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
      `gh refusal: calls.jsonl:8, \`gh ${argv.join(" ")}\` of the reviewer, exited 1: ${why}, which threads-closed does not declare for that role`
    );
  }
  const write = ["issue", "create", "-R", "actions/checkout", "--title", "t", "--body", "b"];
  writeFileSync(
    calls,
    `${answered}${JSON.stringify({ at: "t", as: "gh", by: "implementer", argv: write, files: {}, route: null, exit: 1 })}\n`
  );
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    `gh refusal: calls.jsonl:8, \`gh ${write.join(" ")}\` of the implementer, exited 1: the refusal of an owner outside the project with another status than the Legion gh wrapper's 2, which threads-closed does not declare for that role`
  );
  writeFileSync(calls, answered);
  writeFileSync(
    path.join(dir, "gh-selectors.json"),
    `${JSON.stringify(labelGhSelectors(olderWrapper()))}\n`
  );
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    `gh refusal: calls.jsonl:2, \`gh ${TAG_READ.join(" ")}\` of the reviewer, exited 0: ${FAILS_CLOSED}, which threads-closed does not declare for that role`
  );
}, 120_000);

// Catches (the architect's rulings of 2026-11-22T16:36:59Z and 17:57Z, after INFRA-764's round 33
// at 25ce8a43): a world that answers the reads of the pinned actions round 33's agents made with
// its own 404 (`unrouted`, or the `git/ref/` route answering another owner's ref from the world's
// remote), where production's gh answers each with the action's real ref. Production's answers:
// the tester's capture, dispatch://INFRA-764/artifact/infra-764-pinned-actions-capture-txt v2, read
// with legion gh in the tester's pane, 2026-11-22T17:55:33Z to 17:55:39Z, each exit 0. And a world
// that answers `gh api -i` of another owner, which legion gh refuses itself with exit 2 and its own
// line before any request (measured at 17:55Z), otherwise than a refusal with that status.
test("the world answers round 33's six reads of the pinned actions as production's gh answered them, and refuses `-i` of them as legion gh does", () => {
  const { dir } = runDir({ fixture: true });
  const read = (endpoint: string, filter: string) =>
    asked(dir, "reviewer", ["api", endpoint, "--jq", filter], PANE_GH);
  expect([
    read("repos/actions/checkout", ".full_name"),
    read("repos/oven-sh/setup-bun", ".full_name"),
    read("repos/actions/checkout/git/ref/tags/v7.0.1", '.object.sha + " " + .object.type'),
    read("repos/actions/checkout/git/refs/tags/v7.0.1", '.object.sha + " " + .object.type'),
    read("repos/oven-sh/setup-bun/git/ref/tags/v2.2.0", '.object.sha + " " + .object.type'),
    read("repos/oven-sh/setup-bun/git/refs/tags/v2.2.0", '.object.sha + " " + .object.type'),
  ]).toEqual([
    [0, "actions/checkout\n", ""],
    [0, "oven-sh/setup-bun\n", ""],
    [0, "3d3c42e5aac5ba805825da76410c181273ba90b1 commit\n", ""],
    [0, "3d3c42e5aac5ba805825da76410c181273ba90b1 commit\n", ""],
    [0, "0c5077e51419868618aeaa5fe8019c62421857d6 commit\n", ""],
    [0, "0c5077e51419868618aeaa5fe8019c62421857d6 commit\n", ""],
  ]);
  const [status, , line] = asked(
    dir,
    "reviewer",
    ["api", "-i", "repos/actions/checkout/git/ref/tags/v7.0.1"],
    PANE_GH
  );
  // Production's line names its own owners directory (`legion gh: the role's GitHub App is not
  // installed on "actions": /var/run/legion/gh holds no gh directory for it`), and the world's
  // wrapper is the label's (gh-standin.ts's `ownerRefusal`), so the line is held to the owner it
  // names and the status to legion gh's 2.
  expect([
    status,
    String(line).startsWith("legion gh: "),
    String(line).includes('"actions"'),
  ]).toEqual([2, true, true]);
}, 120_000);

// Catches (the architect's ruling of 2026-11-22T16:36:59Z, ruling (a)): a health that declares the
// world's 404 to a read legion gh runs on the issue's token, so a run whose world told its agent
// that the action's real, pinned tag does not exist reads healthy (INFRA-764 round 33's counted
// runs 58d6a8c7c043 and d883d38cea8a); production's gh answers that read, exit 0 (the tester's
// capture). And one that, to catch it, calls production's own answers unhealthy: the read answered
// exit 0, and `gh api -i` of the owner refused exit 2 by legion gh.
test("health calls a run unhealthy whose world answered a read of another owner otherwise than production's gh, and healthy where it answered as production does", () => {
  const { dir } = runDir({ fixture: true });
  // A read of the world's own pull request opens the record, as an agent's first call does.
  expect(
    asked(dir, "reviewer", ["api", "repos/example/widgets/pulls/1001", "--jq", ".number"])[0]
  ).toBe(0);
  const calls = path.join(dir, "calls.jsonl");
  const opened = readFileSync(calls, "utf8");
  const recorded = (argv: readonly string[], exit: number) =>
    `${JSON.stringify({ at: "t", as: "gh", by: "reviewer", argv, files: {}, route: null, handler: "unrouted", exit })}\n`;
  writeFileSync(
    calls,
    opened +
      recorded(TAG_READ, 0) +
      recorded([...TAG_READ, "--jq", ".object.sha"], 0) +
      recorded(["api", "-i", "repos/actions/checkout/git/ref/tags/v7.0.1"], 2)
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  writeFileSync(calls, opened + recorded(TAG_READ, 1));
  expect(ghUndeclared({ dir, scenario: "reviewer-text-findings" })).toStartWith(
    `gh refusal: calls.jsonl:3, \`gh ${TAG_READ.join(" ")}\` of the reviewer, exited 1`
  );
}, 120_000);

// Production's answers to a read of a tag object at the commit each pinned tag names (the
// tester's capture, dispatch://INFRA-764/artifact/infra-764-pinned-actions-capture-txt v3, read in
// a role App's pane at 2026-11-23T05:52:16Z and 05:52:17Z): both tags are lightweight, so GitHub
// holds no tag object at that commit and answers 404, and gh prints GitHub's body unformatted,
// ignores the `-q`, says `gh: Not Found (HTTP 404)` and exits 1. INFRA-764 round 34's world-health
// run a167a5bc6341 made the first read, as a reviewer's subagent checking the pins.
const TAG_OBJECT_READS = [
  [
    "api",
    "repos/actions/checkout/git/tags/3d3c42e5aac5ba805825da76410c181273ba90b1",
    "-q",
    ".object.sha",
  ],
  [
    "api",
    "repos/oven-sh/setup-bun/git/tags/0c5077e51419868618aeaa5fe8019c62421857d6",
    "-q",
    ".object.sha",
  ],
];
const TAG_OBJECT_404 =
  '{"message":"Not Found","documentation_url":"https://docs.github.com/rest/git/tags#get-a-tag","status":"404"}';

// Catches (the architect's ruling of 2026-11-23T05:52:25Z: a captured read is answered from the
// capture, exit and body): a world that answers a read the capture holds as production's 404 with
// its own 404 (round 34's a167a5bc6341 met `{"message": "Not Found", "documentation_url":
// "https://docs.github.com/rest", …}` formatted over five lines), so an agent that reads GitHub's
// documentation link or parses the body meets an answer production does not give.
test("the world answers a read the capture holds as production's 404 with the captured body and exit", () => {
  const { dir } = runDir({ fixture: true });
  for (const argv of TAG_OBJECT_READS) {
    const [status, stdout, stderr] = asked(dir, "reviewer", argv, PANE_GH);
    expect([status, String(stdout).trimEnd(), String(stderr)]).toEqual([
      1,
      TAG_OBJECT_404,
      "gh: Not Found (HTTP 404)\n",
    ]);
  }
}, 120_000);

// Catches (the architect's ruling of 2026-11-23T05:52:25Z: ruling (a) means "as production
// answers", and the capture is the record of production's answer, the 404s included):
//   - a health that calls a run unhealthy for production's own 404 (INFRA-764 round 34's
//     world-health run a167a5bc6341, voided by rig health and the tester's reader for
//     `gh api repos/actions/checkout/git/tags/3d3c42e5… -q .object.sha` answered exit 1, which
//     production's gh answers exit 1, the tester's capture v3);
//   - one that keeps the "exit 0 or unhealthy" shortcut, so the same read answered exit 0, an
//     answer production does not give, reads healthy;
//   - one that passes a read of the label's read grammar that no capture holds, where production's
//     answer is not known: it fails closed, naming the call so that it can be captured.
test("health judges a read of another owner against the capture: a captured 404 answered so is healthy, answered exit 0 is not, and an uncaptured read is not", () => {
  const { dir } = runDir({ fixture: true });
  expect(
    asked(dir, "reviewer", ["api", "repos/example/widgets/pulls/1001", "--jq", ".number"])[0]
  ).toBe(0);
  const calls = path.join(dir, "calls.jsonl");
  const opened = readFileSync(calls, "utf8");
  const next = opened.split("\n").filter((line) => line.trim() !== "").length + 1;
  const recorded = (argv: readonly string[], exit: number) =>
    `${JSON.stringify({ at: "t", as: "gh", by: "reviewer", argv, files: {}, route: null, handler: "unrouted", exit })}\n`;
  writeFileSync(calls, opened + TAG_OBJECT_READS.map((argv) => recorded(argv, 1)).join(""));
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  const [read] = TAG_OBJECT_READS;
  writeFileSync(calls, opened + recorded(read, 0));
  expect(ghUndeclared({ dir, scenario: "reviewer-text-findings" })).toStartWith(
    `gh refusal: calls.jsonl:${next}, \`gh ${read.join(" ")}\` of the reviewer, exited 0`
  );
  const uncaptured = ["api", "repos/actions/checkout/pulls/1", "--jq", ".number"];
  for (const exit of [0, 1]) {
    writeFileSync(calls, opened + recorded(uncaptured, exit));
    expect(ghUndeclared({ dir, scenario: "reviewer-text-findings" })).toStartWith(
      `gh refusal: calls.jsonl:${next}, \`gh ${uncaptured.join(" ")}\` of the reviewer, exited ${exit}`
    );
  }
}, 120_000);

// GitHub refuses every role App's read of `user` 403 (measured on 2026-11-17 from a Legion pod:
// `gh api user`, exit 1, `Resource not accessible by integration`), and the world refuses it so
// (rig.sh's fixture_write). Catches: a health that calls a run unhealthy for a role that asked
// GitHub who its token is and met production's refusal; and one that reads that 403 by its text
// alone, which is the protection read's, so declaring it for every role declared the protection
// read's 403 for the implementer too, which reads no protection.
test("health declares GitHub's 403 to a role App's read of its user for every role, and the protection's for the reviewer alone", () => {
  const { dir } = runDir({ fixture: true });
  const roles = ["reviewer", "implementer", "tester"];
  for (const by of roles) for (const user of ["user", "/user"]) asked(dir, by, ["api", user]);
  expect(recordedCalls(dir).map((call) => [call.by, call.exit])).toEqual(
    roles.flatMap((by) => [
      [by, 1],
      [by, 1],
    ])
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  asked(dir, "implementer", ["api", "repos/example/widgets/branches/main/protection"]);
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    "gh refusal: calls.jsonl:8, `gh api repos/example/widgets/branches/main/protection` of the implementer, exited 1: gh: Resource not accessible by integration (HTTP 403), which threads-closed does not declare for that role"
  );
}, 60_000);

// gh refuses a read of a pull request, a branch or a repository GitHub has not got, and a
// `--json` field it does not take, with its own line and exit 1, for any role that makes the call
// (measured on 2026-11-17 against cybermonklabs/legion; gh-standin.ts's `pullRefusal`, gh-view.ts's
// `unknownField`), and the world refuses each so and records it by name (gh-view.ts's
// PULL_NOT_FOUND and FIELD_REFUSED). Health declares both for every role (the architect's ruling
// of 2026-11-17), and a `--jq` that fails over a pull request the world did answer is no refusal.
// Catches: a health that calls a run unhealthy for a role that met production's own refusal of a
// pull request read, and one that cannot tell such a refusal from an answer; and one that passes
// a read the world refused as one it does not model (`gh pr list --search`) because a route had
// matched it, where that refusal is the world missing an answer.
test("health declares gh's refusals of a pull request, a branch, a repository or a --json field for every role", () => {
  const { dir } = runDir({ fixture: true });
  const roles = ["reviewer", "implementer", "tester"];
  for (const by of roles)
    for (const argv of [
      ["pr", "view", "999999", "--json", "number"],
      ["pr", "view", "nothing-here"],
      ["pr", "diff", "7"],
      ["pr", "list", "--repo", "example/gadgets", "--json", "number"],
      ["pr", "view", "1001", "--json", "bogus"],
      ["pr", "list", "--json", "number,bogus"],
      ["pr", "view", "1001", "--json", "number", "--jq", ".number | error"],
    ])
      asked(dir, by, argv);
  expect(recordedCalls(dir).map((call) => [call.by, call.exit, call.handler])).toEqual(
    roles.flatMap((by) => [
      ...[1, 2, 3, 4].map(() => [by, 1, "pr not found"]),
      ...[1, 2].map(() => [by, 1, "pr json field refused"]),
      [by, 1, "pr view"],
    ])
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect(ghUndeclared({ dir, scenario })).toBeUndefined();
  asked(dir, "reviewer", ["pr", "list", "--search", "is:open", "--json", "number"]);
  expect(ghUndeclared({ dir, scenario: "threads-closed" })).toBe(
    "gh refusal: calls.jsonl:23, `gh pr list --search is:open --json number` of the reviewer, exited 1: gh: Not Found (HTTP 404) (no route of the world answers it), which threads-closed does not declare for that role"
  );
}, 60_000);

/** A run directory whose remote holds, at world.json's seeded head, the world's workflow as rig.sh
 * writes it (fixture_round_files), so the repositories its `uses:` lines name and the commits they
 * pin are the fixture's; and whose world.json names the world as rig.sh's fixture_write does: its
 * repository and base (rig.sh's worker_repo and worker_base), the first issue of the world's
 * project (run-names.ts's WORLD_PROJECT) and that issue's branch. */
function workflowRun(): string {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-workflow-"));
  made.push(dir);
  const key = `${WORLD_PROJECT}-1`;
  const src = path.join(dir, "src");
  mkdirSync(path.join(src, "docs"), { recursive: true });
  git(src, "init", "-q", "-b", `legion/${key}`);
  const wrote = spawnChecked([
    "bash",
    "-c",
    [fn("fixture_round_files"), 'src=$1 fixture_round_files "A line." ""'].join("\n"),
    "-",
    src,
  ]);
  if (wrote.exitCode !== 0) throw new Error(`fixture_round_files: ${wrote.stderr.toString()}`);
  git(src, "add", "-A");
  git(src, "commit", "-qm", "the seeded head");
  const head = git(src, "rev-parse", "HEAD");
  git(dir, "clone", "-q", "--bare", src, "remote.git");
  const value = (name: string) => assignment(name).slice(`${name}=`.length);
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key,
      repo: value("worker_repo"),
      pr: 1001,
      branch: `legion/${key}`,
      base: value("worker_base"),
      head,
    })
  );
  return dir;
}

// Catches: a health that holds a GET of a repository the world's own workflow names to the
// world's hosts, where production reaches GitHub for it (the rehearsal's `curl` read of the tag
// actions/checkout is pinned to), and a HEAD of it, which reads no more; and one that allows
// wider: another repository, another host, a method other than GET or HEAD, a body, or a git
// that fetches, clones or pushes. The set is the workflow's, read from the run's remote at the
// seeded head.
test("rule 13 allows a GET or a HEAD with no body of a repository the world's workflow names on GitHub's hosts, and nothing wider", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  expect([...allowed.uses.repositories]).toEqual(["actions/checkout", "oven-sh/setup-bun"]);
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const reach = (tool: string, args: Record<string, unknown>) =>
    offWorldReach(
      [{ file: "sessions/reviewer/a.jsonl", tool, args, at: 1 }],
      loopback,
      [dir],
      allowed
    );
  const bash = (command: string) => reach("bash", { command });
  for (const command of [
    "curl -sS https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1",
    "curl -sSL -X GET 'https://api.github.com/repos/oven-sh/setup-bun/git/refs/tags/v2.2.0' | jq -r .object.sha",
    "curl -fsSL https://github.com/actions/checkout/releases/tag/v7.0.1",
    "curl -fsSL https://raw.githubusercontent.com/actions/checkout/v7.0.1/action.yml",
    "wget -qO- https://codeload.github.com/oven-sh/setup-bun/tar.gz/refs/tags/v2.2.0",
    "curl -s api.github.com/repos/Actions/Checkout/tags",
    "curl -sSI https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1",
    "curl --head https://github.com/oven-sh/setup-bun/releases/tag/v2.2.0",
    "curl -s -X HEAD https://api.github.com/repos/actions/checkout",
    "wget --spider https://github.com/actions/checkout/releases/tag/v7.0.1",
  ])
    expect([command, bash(command)]).toEqual([command, undefined]);
  for (const method of [undefined, "GET", "HEAD"])
    expect(
      reach("fetch", {
        url: "https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1",
        ...(method === undefined ? {} : { method }),
      })
    ).toBeUndefined();
  const named = "actions/checkout, oven-sh/setup-bun";
  const off = (host: string, url: string) =>
    `off-world network: a bash call in sessions/reviewer/a.jsonl reaches ${host} (${url}), no host of the run's world (127.0.0.1, localhost, ::1)${
      host === "example.com"
        ? ""
        : `, and no GET or HEAD with no body or ls-remote of a repository the world's workflow names (${named})`
    }`;
  const tag = "https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1";
  for (const command of [
    `curl -X POST ${tag}`,
    `curl -sS --request=PATCH ${tag}`,
    `curl -sS -d '{}' ${tag}`,
    `curl -sSI -d x ${tag}`,
    `curl -X DELETE ${tag}`,
    `wget --post-data=x ${tag}`,
  ])
    expect([command, bash(command)]).toEqual([command, off("api.github.com", tag)]);
  const cache = "https://api.github.com/repos/actions/cache/git/refs/tags/v4";
  expect(bash(`curl -sS ${cache}`)).toBe(off("api.github.com", cache));
  const elsewhere = "https://example.com/repos/actions/checkout/git/refs/tags/v7.0.1";
  expect(bash(`curl -sS ${elsewhere}`)).toBe(off("example.com", elsewhere));
  expect(
    reach("fetch", { url: tag, method: "POST" })?.startsWith(
      "off-world network: a fetch call in sessions/reviewer/a.jsonl reaches api.github.com"
    )
  ).toBe(true);
  // git's read of a repository's refs is allowed and its fetch, clone and push are not: measured
  // on 2026-11-16, `git ls-remote` sends the GET of info/refs and one `ls-refs` POST, while a
  // fetch posts again for the objects (health.ts's `refsRead`).
  const repository = "https://github.com/actions/checkout.git";
  expect(bash(`git ls-remote ${repository} refs/tags/v7.0.1`)).toBeUndefined();
  expect(bash("git -c protocol.version=0 ls-remote https://github.com/actions/checkout")).toBe(
    undefined
  );
  for (const command of [
    `git fetch --depth 1 ${repository} refs/tags/v7.0.1`,
    `git clone ${repository}`,
    `git push ${repository} main`,
  ])
    expect([command, bash(command)]).toEqual([command, off("github.com", repository)]);
  expect(bash("git ls-remote https://github.com/actions/cache.git")).toBe(
    off("github.com", "https://github.com/actions/cache.git")
  );
  // A run whose world holds no workflow allows none.
  const none = workflowUses(runDir({ fixture: true }).dir);
  expect([none.repositories.size, none.commits.size]).toEqual([0, 0]);
});

// The two calls rule 13 failed in the preflight at 08d2ba6b, byte for byte as the reviewers'
// subagents made them (QuintessentialSnipe in run e117b6918e11, PanickyTermite in b4a28e5c7cfc):
// each GETs the tags of the two actions the world's workflow pins through a `for` loop's variable,
// the first through assignments its `%%` and `#` trim too, which the architect ruled part of the
// world.
const SNIPE = [
  'for ref in "actions/checkout:v7.0.1:3d3c42e5aac5ba805825da76410c181273ba90b1" "oven-sh/setup-bun:v2.2.0:0c5077e51419868618aeaa5fe8019c62421857d6"; do',
  `  repo=\${ref%%:*}; rest=\${ref#*:}; tag=\${rest%%:*}; sha=\${rest#*:};`,
  '  echo "== $repo $tag claimed=$sha";',
  `  curl -fsSL "https://api.github.com/repos/$repo/git/refs/tags/$tag" 2>/dev/null | grep -E '"sha"|"type"|"url"' | head;`,
  '  echo "--- resolve annotated if present";',
  "done",
].join("\n");
const TERMITE = `for repo in actions/checkout oven-sh/setup-bun; do echo "=== $repo ==="; curl -s "https://api.github.com/repos/$repo/tags?per_page=100" 2>/dev/null | grep -E '"name"|"sha"' | head -20; done 2>&1 | head -60`;
// The two calls of the reviewer's subagent VastArmadillo in run 68bb8f4055ff of the launch at
// c6aa29aa, byte for byte: each GETs the ref of the tag of each action the world's workflow pins,
// through a `for` loop's variable `ref`. Rule 13 failed the run on the first, whose grep pattern
// names `"ref"` in single quotes, which it read as a place that may set the variable: it read the
// URL as `…/repos/${ref}`, whose repository it could not tell.
const ARMADILLO = `for ref in "actions/checkout/git/refs/tags/v7.0.1" "oven-sh/setup-bun/git/refs/tags/v2.2.0"; do echo "== $ref =="; curl -s "https://api.github.com/repos/\${ref}" | grep -E '"sha"|"message"|"ref"|"object"' ; done 2>&1`;
const ARMADILLO_TYPES = `for ref in "actions/checkout/git/refs/tags/v7.0.1" "oven-sh/setup-bun/git/refs/tags/v2.2.0"; do echo "== $ref =="; curl -s "https://api.github.com/repos/\${ref}" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["object"]["type"],d["object"]["sha"])'; done 2>&1`;

// Catches: a variable a `for` loop over literal words or a command of assignments alone sets to a
// literal left unread, so a GET of a repository the world's workflow names through one fails the
// run (the four recorded calls, one naming the loop's variable in a grep pattern, a single-quoted
// word a process of its own is given; a `${…}`; a loop in a loop); one word of a loop's list
// judged for all of them, in either order; and a variable read wider than the shell sets it: a
// loop over what is not literal words (a command's output, a glob, a variable, a quoted
// substitution); a word the shell reads as more than itself (a quote in it); a value from a
// command's output or of a variable the reader does not hold, even after a literal; one that
// `read` or `export` sets; a prefix assignment, whose `$repo` the shell expands before it assigns;
// one after `&&`, inside a bracket or an `if`; a loop's variable after its loop (its value from
// before the loop kept), and what its body set; a pattern of more than literal characters and
// `*`; one a single-quoted word may set where a builtin, a function or an alias reads the word
// (`[[ -v`, `time -p eval`, a function that runs `builtin "$@"`, an alias by its name or through
// an expansion, each measured to set it with bash 5.2.37). And a call that holds what may set any
// variable unread: `source`, `eval` (bare or after `command`), a `declare` given an expansion or
// `-i`, a name an option's letters run into (`read -arepo`), a variable the shell sets itself
// (REPLY), IFS, a comment; and a here-document, whose body is read at its place in the call's
// text. And a GET whose repository holds an expansion the reader holds no value of, named so in
// place of no GET of a repository the world's workflow names.
test("rule 13 reads a variable a literal for loop or a literal assignment sets, every value of it, and no other", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const file = "sessions/reviewer/a.jsonl";
  const bash = (command: string) =>
    offWorldReach([{ file, tool: "bash", args: { command }, at: 1 }], loopback, [dir], allowed);
  const api = "https://api.github.com/repos";
  const reaches = (url: string) =>
    `off-world network: a bash call in ${file} reaches api.github.com (${url}), no host of the run's world (127.0.0.1, localhost, ::1), and`;
  const off = (url: string) =>
    `${reaches(url)} no GET or HEAD with no body or ls-remote of a repository the world's workflow names (actions/checkout, oven-sh/setup-bun)`;
  const unexpanded = (url: string, expansion: string) =>
    `${reaches(url)} its URL holds ${expansion}, which the reader cannot expand`;
  const unread = unexpanded(`${api}/$repo`, "$repo");
  const unreadTags = unexpanded(`${api}/$repo/tags`, "$repo");
  const unreadRef = unexpanded(`${api}/\${ref}/tags`, `\${ref}`);
  const heredoc = `python3 - <<'EOF'\nimport urllib.request; urllib.request.urlopen('https://example.com/x')\nEOF`;
  const rows: [command: string, reason: string | undefined][] = [
    [SNIPE, undefined],
    [TERMITE, undefined],
    [ARMADILLO, undefined],
    [ARMADILLO_TYPES, undefined],
    [
      ARMADILLO.replace("oven-sh/setup-bun/git/refs/tags/v2.2.0", "evil/repo/git/refs/tags/v1"),
      off(`${api}/evil/repo/git/refs/tags/v1`),
    ],
    [
      `ref=$(cat refs); curl -s "${api}/\${ref}" | grep -E '"sha"|"ref"'`,
      unexpanded(`${api}/\${ref}`, `\${ref}`),
    ],
    [
      `ref=$REF; curl -s "${api}/\${ref}" | grep -E '"sha"|"ref"'`,
      unexpanded(`${api}/\${ref}`, `\${ref}`),
    ],
    [
      `for ref in actions/checkout; do [[ -v 'a[ref=1]' ]]; curl -s "${api}/\${ref}/tags"; done`,
      unreadRef,
    ],
    [
      `for ref in actions/checkout; do time -p eval 'ref=evil/fork'; curl -s "${api}/\${ref}/tags"; done`,
      unreadRef,
    ],
    [
      `f() { builtin "$@"; }; for ref in actions/checkout; do f eval 'ref=evil/fork'; curl -s "${api}/\${ref}/tags"; done`,
      unreadRef,
    ],
    [
      `shopt -s expand_aliases; alias grep=eval\nfor ref in actions/checkout; do grep 'ref=evil/fork'; curl -s "${api}/\${ref}/tags"; done`,
      unreadRef,
    ],
    [
      `shopt -s expand_aliases; a=alias; $a grep=eval\nfor ref in actions/checkout; do grep 'ref=evil/fork'; curl -s "${api}/\${ref}/tags"; done`,
      unreadRef,
    ],
    [`repo=actions/checkout; curl -s ${api}/$repo`, undefined],
    [
      `repo=oven-sh/setup-bun\ncurl -sI "https://github.com/\${repo}/releases/tag/v2.2.0"`,
      undefined,
    ],
    [
      `for owner in actions; do for name in checkout; do curl -s "${api}/$owner/$name/tags"; done; done`,
      undefined,
    ],
    [
      `for repo in actions/checkout evil/repo; do curl -s "${api}/$repo/tags"; done`,
      off(`${api}/evil/repo/tags`),
    ],
    [
      `for repo in evil/repo actions/checkout; do curl -s "${api}/$repo/tags"; done`,
      off(`${api}/evil/repo/tags`),
    ],
    [`repo=evil/repo; curl -s ${api}/$repo`, off(`${api}/evil/repo`)],
    [
      `for repo in actions/checkout oven-sh/setup-bun; do curl -s -X POST "${api}/$repo/issues"; done`,
      off(`${api}/actions/checkout/issues`),
    ],
    [`for repo in $(cat repos); do curl -s "${api}/$repo/tags"; done`, unreadTags],
    [`for repo in *; do curl -s "${api}/$repo/tags"; done`, unreadTags],
    [`for repo in $REPOS; do curl -s "${api}/$repo/tags"; done`, unreadTags],
    [`for repo in actions/checkout "$(cat f)"; do curl -s "${api}/$repo/tags"; done`, unreadTags],
    [`for repo in "actions/checkout'"; do curl -s "${api}/$repo"; done`, unread],
    [`repo=$(cat f); curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; repo=$(cat f); curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; repo=$REPO; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; read repo < f; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; export repo=evil/fork; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout curl -s ${api}/$repo`, unread],
    [`repo=evil/fork; false && repo=actions/checkout; curl -s ${api}/$repo`, unread],
    [`repo=evil/fork; true; (true; repo=actions/checkout; true); curl -s ${api}/$repo`, unread],
    [
      `repo=evil/fork; if false; then true; repo=actions/checkout; fi; curl -s ${api}/$repo`,
      unread,
    ],
    [`for repo in actions/checkout; do :; done; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; for repo in evil/fork; do :; done; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; for r in x; do repo=evil/fork; done; curl -s ${api}/$repo`, unread],
    [`v=actions/checkout; curl -s ${api}/\${v%[t]}`, unexpanded(`${api}/\${v%[t]}`, `\${v%[t]}`)],
    [`v=actions/checkout; curl -s ${api}/\${v%?t}`, unexpanded(`${api}/\${v%?t}`, `\${v%?t}`)],
    [`repo=actions/checkout; source ./env.sh; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; c=po; eval "re$c=evil/fork"; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; c=po; command eval "re$c=evil/fork"; curl -s ${api}/$repo`, unread],
    [`n=re; repo=actions/checkout; declare "\${n}po=evil/fork"; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; declare -i n; n=0,repo=1; curl -s ${api}/$repo`, unread],
    [`repo=actions/checkout; read -arepo < f; curl -s ${api}/$repo`, unread],
    [
      `REPLY=actions/checkout; read -r < /dev/null; curl -s ${api}/$REPLY`,
      unexpanded(`${api}/$REPLY`, "$REPLY"),
    ],
    [`IFS=/; for repo in actions/checkout; do curl -s ${api}/$repo; done`, unread],
    [`repo=evil/fork\n# ; repo=actions/checkout\ncurl -s ${api}/$repo`, unread],
    [
      `for repo in actions/checkout oven-sh/setup-bun; do echo $repo; done; ${heredoc}`,
      `off-world network: a bash call in ${file} reaches example.com (https://example.com/x) in a here-document python3 runs, no host of the run's world (127.0.0.1, localhost, ::1)`,
    ],
  ];
  expect(rows.map(([command]) => [command, bash(command)])).toEqual(rows);
});

// The call of the reviewer's subagent SurvivingTortoise in INFRA-764 round 27's run 584a6e8817eb
// (reviewer-text-findings defect head, at efea8554), byte for byte: it GETs the ref of the tag of
// each action the world's workflow pins, each URL a word of a literal `for` list, echoed and then
// handed to curl through the loop's variable. Rule 13 failed the run on the list word and the
// echo, and the world-health check refused the batch.
const TORTOISE = `set -o pipefail; for url in "https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1" "https://api.github.com/repos/oven-sh/setup-bun/git/refs/tags/v2.2.0"; do echo "== $url =="; curl -s "$url" 2>/dev/null | grep -E '"sha"|"ref"|message' | head -5; done`;

// Catches (the architect's ruling of 2026-11-20T20:07Z on rule 13): a health that reads every URL
// in a command's text as a reach, so a URL only printed (an `echo` or a `printf` to the terminal or
// into grep, a loop's list word that flows to a GET of a repository the world's workflow names)
// fails a run that touched no network, which TORTOISE did; and one that reads a URL only where a
// command names a fetcher, so a URL that flows where the reader cannot follow it passes unread: an
// echo piped into `sh` or into `xargs curl`, and a curl or a wget that takes its URLs from a file
// (`-K`, `-i`). A loop over a repository the workflow names none of stays off-world.
test("rule 13 reads a URL as a reach only where a command hands it to something that fetches, and fails closed where the reader cannot follow it", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const file = "sessions/reviewer/a.jsonl";
  const bash = (command: string) =>
    offWorldReach([{ file, tool: "bash", args: { command }, at: 1 }], loopback, [dir], allowed);
  const api = "https://api.github.com/repos";
  const reachesNothing = [
    `echo "== ${api}/actions/checkout/git/refs/tags/v7.0.1 =="`,
    `printf '%s\\n' "${api}/evil/repo"`,
    `echo "${api}/evil/repo" | grep evil`,
    TORTOISE,
  ];
  const offWorld = [
    `echo "curl -s ${api}/actions/checkout/tags" | sh`,
    `echo ${api}/actions/checkout/tags | xargs curl -s`,
    `for url in "${api}/evil/repo/tags"; do curl -s "$url"; done`,
    "curl -s -K urls.txt",
    "wget -q -i urls.txt",
  ];
  const refused = (command: string) =>
    bash(command)?.startsWith(`off-world network: a bash call in ${file} `) ?? false;
  expect({
    reachesNothing: reachesNothing.map((command) => [command, bash(command)]),
    offWorld: offWorld.map((command) => [command, refused(command)]),
  }).toEqual({
    reachesNothing: reachesNothing.map((command) => [command, undefined]),
    offWorld: offWorld.map((command) => [command, true]),
  });
});

// Catches (the architect's ruling of 2026-11-20T21:30Z on rule 13): a health that reads a
// fetcher's host only where the text names one, so a fetcher whose URL is a variable the reader
// cannot expand, and no literal host (`x=$(gh api … --jq .html_url); curl -s "$x"`, `wget -q
// "$URL"`, `curl -s "$URL/tags"`, `git ls-remote "$repo"`), a command substitution, or a program's
// fetch of an expression reaches a host nobody read and passes; and one that refuses wider: a
// variable in a header (`-H "…$TOKEN"`), an output file (`-o "$out"`) or a revision (`git log
// "$sha"`), none of which names a host, and a variable of the run's pane, which the reader reads
// with its value, allowed where that value is the world's host and refused where it is not, until
// the text sets it to what the reader cannot expand.
test("rule 13 refuses a fetcher whose URL holds a variable the reader cannot expand and no host, and reads the pane's variables by their value", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const file = "sessions/reviewer/a.jsonl";
  const pane = new Map([
    ["DISPATCH_URL", "http://127.0.0.1:4101"],
    ["MIRROR_URL", "https://mirror.example.com"],
  ]);
  const reach = (tool: string, args: Record<string, unknown>) =>
    offWorldReach([{ file, tool, args, at: 1 }], loopback, [dir], allowed, () => undefined, pane);
  const unread = (tool: string, url: string, why: string, via = "") =>
    `off-world network: a ${tool} call in ${file} reaches the host ${url} names (${url})${via}, ${why} the reader cannot expand, so its host is not known to be one of the run's world`;
  const api = "https://api.github.com/repos/actions/checkout";
  const bashRows: [command: string, reason: string | undefined][] = [
    [
      'x=$(gh api repos/actions/checkout --jq .html_url); curl -s "$x"',
      unread("bash", "$x", "curl's URL is $x, which"),
    ],
    ['wget -q "$URL"', unread("bash", "$URL", "wget's URL is $URL, which")],
    ['curl -s "$URL/tags"', unread("bash", "$URL/tags", "curl's URL is $URL/tags, whose $URL")],
    [
      `wget -qO- "\${base}/x"`,
      unread("bash", `\${base}/x`, `wget's URL is \${base}/x, whose \${base}`),
    ],
    ['git clone "$remote"', unread("bash", "$remote", "git's remote is $remote, which")],
    ['git ls-remote "$u"', unread("bash", "$u", "git's remote is $u, which")],
    ['git ls-remote "$repo"', unread("bash", "$repo", "git's remote is $repo, which")],
    [
      'curl -s "$(gh api repos/actions/checkout --jq .html_url)"',
      unread(
        "bash",
        "$(gh api repos/actions/checkout --jq .html_url)",
        "curl's URL is $(gh api repos/actions/checkout --jq .html_url), which"
      ),
    ],
    [
      `python3 -c 'import sys, urllib.request; urllib.request.urlopen(sys.argv[1])' "$x"`,
      unread(
        "bash",
        "sys.argv[1]",
        "urllib.request.urlopen's URL is sys.argv[1], which",
        " in the program python3 runs"
      ),
    ],
    [
      `python3 - "$x" <<'EOF'\nimport sys, urllib.request\nurllib.request.urlopen(sys.argv[1])\nEOF`,
      unread(
        "bash",
        "sys.argv[1]",
        "urllib.request.urlopen's URL is sys.argv[1], which",
        " in a here-document python3 runs"
      ),
    ],
    [`curl -H "Authorization: $TOKEN" ${api}`, undefined],
    [`curl -o "$out" ${api}`, undefined],
    ['git log "$sha"', undefined],
    ["git status", undefined],
    ['curl -s "$DISPATCH_URL/api/v1/whoami"', undefined],
    [
      'curl -s "$MIRROR_URL/repos/actions/checkout"',
      `off-world network: a bash call in ${file} reaches mirror.example.com (https://mirror.example.com/repos/actions/checkout), no host of the run's world (127.0.0.1, localhost, ::1)`,
    ],
    [
      'DISPATCH_URL=$(cat f); curl -s "$DISPATCH_URL/x"',
      unread("bash", "$DISPATCH_URL/x", "curl's URL is $DISPATCH_URL/x, whose $DISPATCH_URL"),
    ],
  ];
  const cells: [language: string, code: string, reason: string | undefined][] = [
    ["js", "const r = await fetch(url)", unread("eval", "url", "fetch's URL is url, which")],
    [
      "py",
      "import urllib.request\nurllib.request.urlopen(u)",
      unread("eval", "u", "urllib.request.urlopen's URL is u, which"),
    ],
    [
      "js",
      "await tool.fetch({url: u})",
      unread(
        "eval",
        "{url: u}",
        "tool.fetch's URL is {url: u}, which",
        " in the cell's tool.fetch call"
      ),
    ],
    ["js", 'await fetch("http://127.0.0.1:4101/api/v1/whoami")', undefined],
  ];
  expect({
    bash: bashRows.map(([command]) => [command, reach("bash", { command })]),
    eval: cells.map(([language, code]) => [language, code, reach("eval", { language, code })]),
  }).toEqual({ bash: bashRows, eval: cells });
});

// Catches (the architect's rulings of 2026-11-20T20:07Z and 21:30Z on rule 13): a health that
// lets a fetcher xargs runs pass where nothing in the text gives it its URL (`echo "$x" | xargs
// curl -s`, `cat remotes.txt | xargs git clone`, an `-a` file, an `-I` string in its URL), and one
// that refuses wider: a literal URL piped into xargs, which is judged as any URL in the text is
// (refused for a repository the workflow names none of, allowed for the world's host), an xargs
// that gives a fetcher no URL (`xargs git push origin`, `-I{}` in an output file) or runs no
// fetcher.
test("rule 13 refuses a fetcher xargs gives a URL from items the reader does not read, and judges a literal URL piped into it", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const file = "sessions/reviewer/a.jsonl";
  const bash = (command: string) =>
    offWorldReach([{ file, tool: "bash", args: { command }, at: 1 }], loopback, [dir], allowed);
  const input = "<xargs's input>";
  const unread = (url: string, why: string) =>
    `off-world network: a bash call in ${file} reaches the host ${url} names (${url}), ${why} the reader cannot expand, so its host is not known to be one of the run's world`;
  const api = "https://api.github.com/repos";
  const whoami = "http://127.0.0.1:4101/api/v1/whoami";
  const rows: [command: string, reason: string | undefined][] = [
    ['echo "$x" | xargs curl -s', unread(input, `curl's URL is ${input}, which`)],
    [
      "gh api repos/actions/checkout --jq .html_url | xargs wget -q",
      unread(input, `wget's URL is ${input}, which`),
    ],
    ["cat remotes.txt | xargs git clone", unread(input, `git's remote is ${input}, which`)],
    ["xargs -a urls.txt curl -s", unread(input, `curl's URL is ${input}, which`)],
    ["cat urls.txt | xargs -n1 timeout 5 curl -s", unread(input, `curl's URL is ${input}, which`)],
    [
      `printf '%s\\n' "$repo" | xargs -I{} curl -s "{}/tags"`,
      unread(`${input}/tags`, `curl's URL is ${input}/tags, whose ${input}`),
    ],
    [
      `echo ${api}/evil/repo/tags | xargs curl -s`,
      `off-world network: a bash call in ${file} reaches api.github.com (${api}/evil/repo/tags), no host of the run's world (127.0.0.1, localhost, ::1), and no GET or HEAD with no body or ls-remote of a repository the world's workflow names (actions/checkout, oven-sh/setup-bun)`,
    ],
    [`echo ${whoami} | xargs curl -s`, undefined],
    [
      'for u in http://127.0.0.1:4101/a http://127.0.0.1:4101/b; do echo "$u"; done | xargs curl -s',
      undefined,
    ],
    ["cat refspecs.txt | xargs git push origin", undefined],
    [`ls | xargs -I{} curl -s -o {}.json ${whoami}`, undefined],
    ["git ls-files | xargs grep -l TODO", undefined],
  ];
  expect(rows.map(([command]) => [command, bash(command)])).toEqual(rows);
});

// Catches (the architect's ruling of 2026-11-20T21:30Z on rule 13): a health that reads a
// fetcher's words only up to where SHELL_COMMAND ends them, so a URL given in a backquoted
// substitution passes (``curl -s `cmd` ``), one given in an unquoted `$(…)` is named by its `$`
// alone, and a fetcher inside a command substitution (`body=$(curl -s "$URL")`, `echo "$(wget
// -qO- "$u")"`) is not read as a command; and one that refuses wider: a fetcher inside a
// substitution given a literal URL of the world, or a substitution in an output file's place.
test("rule 13 reads a command substitution as one word of a fetcher's, and each fetcher inside one", () => {
  const dir = workflowRun();
  const allowed = offWorldAllowed({ dir, label: "head" });
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const file = "sessions/reviewer/a.jsonl";
  const bash = (command: string) =>
    offWorldReach([{ file, tool: "bash", args: { command }, at: 1 }], loopback, [dir], allowed);
  const unread = (fetcher: string, url: string) =>
    `off-world network: a bash call in ${file} reaches the host ${url} names (${url}), ${fetcher}'s URL is ${url}, which the reader cannot expand, so its host is not known to be one of the run's world`;
  const whoami = "http://127.0.0.1:4101/api/v1/whoami";
  const backquoted = "`gh api repos/actions/checkout --jq .html_url`";
  const substituted = "$(gh api repos/actions/checkout --jq .html_url)";
  const rows: [command: string, reason: string | undefined][] = [
    [`curl -s ${backquoted}`, unread("curl", backquoted)],
    ["wget -q `cat url.txt`", unread("wget", "`cat url.txt`")],
    [`curl -s ${substituted}`, unread("curl", substituted)],
    ['body=$(curl -s "$URL"); echo "$body" | jq .', unread("curl", "$URL")],
    ['echo "$(wget -qO- "$u")"', unread("wget", "$u")],
    [`echo \`curl -s ${whoami}\``, undefined],
    [`x=$(curl -s ${whoami}); echo "$x"`, undefined],
    [`curl -o $(mktemp) ${whoami}`, undefined],
  ];
  expect(rows.map(([command]) => [command, bash(command)])).toEqual(rows);
});

// Catches (the architect's ruling of 2026-11-22T01:44:54Z on rule 13, which the tester's
// world-health.py holds too, tools v73): a health that calls a web search off the world for naming
// no repository or commit the world's workflow names, where a production reviewer may search for
// what it works on (round 32's c8ef305c3eab searched `Bun.spawnSync default stderr stdout pipe
// inherit`; the rehearsal of 2026-11-16, the two actions the workflow pins). And one that allows
// wider, on each of the five things that carry the world off it or bring the rig's code in: a word
// of the world's own; a word of the measurement, by the definition rule 17 reads an agent's
// sentence by; a string of 16 or more characters a route of fixtures.json answers with on stdout,
// in any case, but the workflow's repositories and pins and every message or code of GitHub's own
// that the rig's GitHub sources hold (the architect's ruling of 2026-11-22T04:06:48Z), a seeded
// body that holds such a message staying the world's; a commit of the run's own remote by 7 or
// more of its leading digits; and a result holding a page of cybermonklabs/legion or
// sjawhar/legion. The repositories, the commits, the world's words, the remote's commits and the
// answers are the run's own, read from its remote, its world.json and its fixtures.json.
test("rule 13 allows a web search that carries nothing of the world or the measurement and came back with no page of the rig's code, whatever else it names", () => {
  const dir = workflowRun();
  const BODY = "Adds `greet(name)` and `bun greet.ts <name>`.";
  // The base's rules and its protection's refusal, as rig.sh's fixture_write writes those routes,
  // GitHub's codes and message; a seeded pull request's body and a comment that holds `Not
  // Found`; and a route answering with a repository and a commit the workflow names, which no
  // route of the world answers with today: a search about the workflow may carry those.
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      routes: [
        {
          match: "^gh api .*/rules/branches/main",
          methods: ["GET"],
          stdout: [
            { type: "deletion", ruleset_id: 1 },
            { type: "non_fast_forward", ruleset_id: 1 },
            { type: "required_linear_history", ruleset_id: 1 },
          ],
        },
        {
          match: "^gh api .*/branches/main/protection",
          methods: ["GET"],
          exit: 1,
          stdout: { message: "Resource not accessible by integration", status: "403" },
        },
        { match: "^gh pr view", stdout: { number: 1001, body: BODY } },
        {
          match: "^gh api .*/issues/1001/comments",
          stdout: [{ body: "greet prints Not Found when the name is blank." }],
        },
        {
          match: "^gh api .*/contents/.github/workflows",
          stdout: { uses: ["Actions/Checkout", "3d3c42e5aac5ba805825da76410c181273ba90b1"] },
        },
      ],
    })
  );
  const allowed = offWorldAllowed({ dir, label: "head" });
  const head = JSON.parse(readFileSync(path.join(dir, "world.json"), "utf8")).head as string;
  expect([[...allowed.uses.commits], allowed.answers, allowed.remote]).toEqual([
    ["3d3c42e5aac5ba805825da76410c181273ba90b1", "0c5077e51419868618aeaa5fe8019c62421857d6"],
    [BODY.toLowerCase(), "greet prints not found when the name is blank."],
    [head],
  ]);
  const file = "sessions/reviewer/sub/agent.jsonl";
  const loopback = new Set(["127.0.0.1", "localhost", "::1"]);
  const search = (query: string, result: string, world = allowed) =>
    offWorldReach(
      [{ file, tool: "web_search", args: { query }, at: 1, result }],
      loopback,
      [dir],
      world
    );
  const searched = `off-world network: a web_search call in ${file} searches the web, which no host of the run's world serves`;
  const own = (word: string) =>
    `${searched}, and its query names "${word}", a word of the world's own`;
  const measured = (word: string) =>
    `${searched}, and its query names "${word}", a word of the measurement`;
  const answered = (text: string) =>
    `${searched}, and its query carries "${text}", a string the world's gh stand-in answers with (fixtures.json)`;
  const committed = (word: string) =>
    `${searched}, and its query names ${word}, a commit of the run's own remote`;
  const paged = (page: string) =>
    `${searched}, and it came back with ${page}, a page of the repository under test`;
  const docs = "[1] Spawn | Bun Docs\n    https://bun.com/docs/runtime/child-process\n";
  const rows: [query: string, result: string, reason: string | undefined][] = [
    ["actions/checkout 3d3c42e5aac5ba805825da76410c181273ba90b1 v7.0.1", "", undefined],
    ["oven-sh/setup-bun commit 0c5077e51419868618aeaa5fe8019c62421857d6 v2.2.0", "", undefined],
    ["actions/checkout v7.0.1 head commit", "", undefined],
    ["actions/checkout defect", "", undefined],
    ["actions/checkout base ref", "", undefined],
    ["actions/checkout main", "", undefined],
    ["actions/checkout@3d3c42e", "", undefined],
    ["release notes of Actions/Checkout.", "", undefined],
    ["commit 3d3c42e", "", undefined],
    ["commit 3d3c42", "", undefined],
    ["actions/checkout-evil v7.0.1", "", undefined],
    ["actions/checkout.js v7.0.1", "", undefined],
    ["legion review threads", "", undefined],
    ["Bun.spawnSync default stderr stdout pipe inherit", docs, undefined],
    ["deletion rule of a ruleset", "", undefined],
    [`${head.slice(0, 6)} bun`, "", undefined],
    [
      "bun spawn docs",
      "https://github.com/oven-sh/bun/blob/bun-v1.3.14/docs/runtime/child-process.mdx",
      undefined,
    ],
    ["bun spawn docs", "https://github.com/cybermonklabs/legion-tools", undefined],
    ["text-only head", "", measured("text-only")],
    ["threads-closed review rules", "", measured("threads-closed")],
    ["actions/checkout text-only head", "", measured("text-only")],
    ["actions/checkout defect head", "", measured("defect head")],
    ["actions/checkout head defect", "", measured("head defect")],
    ["actions/checkout threads-closed", "", measured("threads-closed")],
    ["actions/checkout skill-scenarios", "", measured("skill-scenario")],
    ["fernbrook widgets greet", "", own("fernbrook")],
    ["actions/checkout fernbrook/widgets", "", own("fernbrook/widgets")],
    ["oven-sh/setup-bun widgets", "", own("widgets")],
    ["actions/checkout WID-1", "", own("wid-1")],
    ["actions/checkout WID", "", own("wid")],
    ["3d3c42e5aac5ba805825da76410c181273ba90b1 fernbrook", "", own("fernbrook")],
    ["3d3c42e5aac5ba805825da76410c181273ba90b1 WID-1", "", own("wid-1")],
    ["non_fast_forward", "", undefined],
    ["NON_FAST_FORWARD ruleset github", "", undefined],
    ["actions/checkout required_linear_history", "", undefined],
    ["Resource not accessible by integration github app", "", undefined],
    ["Not Found github api", "", undefined],
    [BODY, "", answered(BODY.toLowerCase())],
    [
      "Greet prints Not Found when the name is blank. bun",
      "",
      answered("greet prints not found when the name is blank."),
    ],
    [`${head.slice(0, 7)} bun`, "", committed(head.slice(0, 7))],
    [`commit ${head}`, "", committed(head)],
    [
      "bun spawn docs",
      "https://github.com/cybermonklabs/legion/blob/main/README.md",
      paged("https://github.com/cybermonklabs/legion"),
    ],
    [
      "bun spawn docs",
      "raw.githubusercontent.com/sjawhar/legion/main/README.md",
      paged("raw.githubusercontent.com/sjawhar/legion/"),
    ],
    [
      "bun spawn docs",
      "https://api.github.com/repos/cybermonklabs/legion/pulls/2827",
      paged("https://api.github.com/repos/cybermonklabs/legion"),
    ],
    [
      "bun spawn docs",
      "https://cybermonklabs.github.io/legion/",
      paged("https://cybermonklabs.github.io/legion"),
    ],
    [
      "bun spawn docs",
      "see https://GitHub.com/CyberMonkLabs/Legion/issues",
      paged("https://GitHub.com/CyberMonkLabs/Legion"),
    ],
  ];
  expect(rows.map(([query, result]) => [query, result, search(query, result)])).toEqual(rows);
  // A run whose world holds no workflow is held to the same five things, and this query carries
  // none of them.
  const none = offWorldAllowed({ dir: runDir({ fixture: true }).dir, label: "head" });
  expect(
    search("actions/checkout 3d3c42e5aac5ba805825da76410c181273ba90b1 v7.0.1", "", none)
  ).toBeUndefined();
});

// Catches: a health whose push-guard rule takes the Legion gh wrapper's refusal of an owner
// outside the project for the push guard's own, since main ff2a10ef's line carries `LEAK CHECK
// SETUP ERROR` after its `legion gh: ` prefix (`olderRefusal`): every reviewer that read an
// action's tag on such a label would be unhealthy for a refusal it met as production gives it.
// The line is the stand-in's own, shown to the agent as its tool's result, and its call is in the
// record: `gh api -i` of the action's tag, a form outside the wrapper's read grammar, which the
// wrapper refuses exit 2 and rule 7 takes as production's (ruling (a)); the push-guard rule stays
// silent on it, rule 7 declares it, and the guard's own line at the start of a line still fires.
test("the wrapper's owner refusal a tool result shows is no push-guard refusal, and rule 7 declares it", () => {
  const { dir } = runDir({ fixture: true });
  writeFileSync(
    path.join(dir, "gh-selectors.json"),
    `${JSON.stringify(labelGhSelectors(olderRefusal()))}\n`
  );
  const included = ["api", "-i", ...TAG_READ.slice(1)];
  const refused = spawnChecked(["bun", STANDIN, "gh", ...included], {
    env: callEnv(dir, "reviewer"),
  });
  const line = refused.stderr.toString();
  expect([refused.exitCode, line]).toEqual([2, setupErrorRefused("actions/checkout")]);
  const transcript = (result: string) => {
    mkdirSync(path.join(dir, "sessions", "reviewer"), { recursive: true });
    writeFileSync(
      path.join(dir, "sessions", "reviewer", "session.jsonl"),
      [
        {
          type: "message",
          timestamp: "2026-11-16T18:00:00.000Z",
          message: {
            role: "assistant",
            content: [
              {
                type: "toolCall",
                name: "bash",
                arguments: { command: `gh ${included.join(" ")}` },
              },
            ],
          },
        },
        {
          type: "message",
          timestamp: "2026-11-16T18:00:01.000Z",
          message: {
            role: "toolResult",
            toolName: "bash",
            content: [{ type: "text", text: result }],
          },
        },
      ]
        .map((entry) => `${JSON.stringify(entry)}\n`)
        .join("")
    );
    return loadRun(dir, "reviewer-text-findings", "head");
  };
  const shown = transcript(`${line}\n[exit 2]`);
  expect(shown.results.map((result) => result.text)).toEqual([`${line}\n[exit 2]`]);
  expect(guardRefusal(shown)).toBeUndefined();
  expect(ghUndeclared(shown)).toBeUndefined();
  const guard =
    "LEAK CHECK SETUP ERROR: the gh call: the visibility of actions/checkout could not be read";
  expect(guardRefusal(transcript(guard))).toBe(
    `push guard: sessions/reviewer/session.jsonl holds the guard's refusal "${guard}"`
  );
}, 60_000);

// Catches: a record that leaves the daemon stand-in to read the run's reviews a second way. It
// moves the issue on a review of the head (daemon-standin.ts's `roundOutcome`), and takes a
// review of the review App by the session its body's Legion footer names (`takenForRound`), and
// reading the calls again itself would be a third reader of one call beside this stand-in and
// the score; so each call that left a review records the state, the head and the body this
// stand-in answered it with — a create and the submit of a pending one, whose body is the
// pending review's where the submit sends none — and no other call records one: a
// `gh pr review`, which the world does not model and refuses (gh's 404, no review on GitHub), on
// the world's pull request or on another repository's, among them.
test("each call that leaves a review records the state, head and body it was answered with, and no other call does", () => {
  const { dir, first } = runDir({ fixture: true });
  answer(dir, ["pr", "review", "1001", "--approve"]);
  const body = path.join(dir, "review.json");
  writeFileSync(body, JSON.stringify({ commit_id: first, event: "APPROVE", body: "approved" }));
  expect(apiPost(dir, REVIEWS, "--input", body).exitCode).toBe(0);
  createPending(dir, { commit_id: first, body: "two findings" });
  // The run's second review, which GitHub gives the id after the first's.
  expect(apiPost(dir, `${REVIEWS}/6002/events`, "-f", "event=REQUEST_CHANGES").exitCode).toBe(0);
  answer(dir, ["pr", "review", "1001", "--approve"]);
  answer(dir, ["pr", "review", "1001", "-R", "other/widgets", "--approve"]);
  answer(dir, ["pr", "view", "1001"]);
  expect(recordedCalls(dir).map((call) => call.review)).toEqual([
    undefined,
    { state: "APPROVED", head: first, body: "approved" },
    { state: "PENDING", head: first, body: "two findings" },
    { state: "CHANGES_REQUESTED", head: first, body: "two findings" },
    undefined,
    undefined,
    undefined,
  ]);
});

// Catches: checks frozen into the world, which read green on a head whose check the world's
// check-runs route says is still running, and a running check that follows the pull request
// onto the next head. Every reader of the head's checks is answered from the world's check-runs
// answer for the head the run's remote is at: `gh pr checks`, which gh exits 8 on while a check
// is pending, and the pull request's `statusCheckRollup`. Its merge state is the world's for
// that head (`mergeStates`), in gh's spelling and the REST one, once its base's rules are met: the
// head is approved, and the next head reports every check the base requires, its code-scanning
// tool's `CodeQL` among them, where the world's base holds an unapproved head or a head short of
// a required check `BLOCKED` (INFRA-764's ruling of 2026-11-18).
test("the head's checks are the world's check runs on the remote's tip, for gh pr checks and the rollup", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const fixtures = JSON.parse(readFileSync(path.join(dir, "fixtures.json"), "utf8"));
  const run = (name: string, status: string) => ({
    name,
    status,
    conclusion: status === "completed" ? "success" : null,
    html_url: `https://example.invalid/${name}`,
  });
  fixtures.routes.unshift(
    {
      match: `^gh api .*commits/${first.slice(0, 7)}[0-9a-f]*/check-runs`,
      stdout: { check_runs: [run("test", "in_progress"), run("lint", "completed")] },
    },
    {
      match: "^gh api .*check-runs",
      stdout: {
        check_runs: [
          run("test", "completed"),
          run("lint", "completed"),
          run("CodeQL", "completed"),
        ],
      },
    }
  );
  fixtures.mergeStates = { [first]: "BLOCKED" };
  fixtures.routes = fixtures.routes.map((route: { match: string; stdout?: unknown }) =>
    route.match === "^gh pr view" || route.match === "^gh api .*pulls/1001"
      ? {
          ...route,
          stdout: {
            ...(route.stdout as object),
            statusCheckRollup: [],
            mergeStateStatus: "CLEAN",
            mergeable_state: "clean",
          },
        }
      : route
  );
  writeFileSync(path.join(dir, "fixtures.json"), JSON.stringify(fixtures));
  const checks = () =>
    spawnChecked(["bun", STANDIN, "gh", "pr", "checks", "1001"], {
      env: callEnv(dir),
    });
  const merge = () => [
    answer(dir, ["pr", "view", "1001", "--json", "mergeStateStatus", "--jq", ".mergeStateStatus"]),
    answer(dir, ["api", "repos/example/widgets/pulls/1001", "--jq", ".mergeable_state"]),
  ];
  const rollup = () =>
    answer(dir, [
      "pr",
      "view",
      "1001",
      "--json",
      "statusCheckRollup",
      "--jq",
      '[.statusCheckRollup[].status] | join(",")',
    ]);
  expect(
    apiPost(dir, REVIEWS, "-f", `commit_id=${first}`, "-f", "event=APPROVE", "-f", "body=ok")
      .exitCode
  ).toBe(0);
  const pending = checks();
  // In gh's form (gh-standin.ts's `prChecks`): by name, `0` for a check with no time it ran, and
  // the empty description a check run carries.
  expect([pending.exitCode, pending.stdout.toString()]).toEqual([
    8,
    "lint\tpass\t0\thttps://example.invalid/lint\t\ntest\tpending\t0\thttps://example.invalid/test\t\n",
  ]);
  expect(rollup()).toBe("IN_PROGRESS,COMPLETED");
  expect(merge()).toEqual(["BLOCKED", "blocked"]);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(checks().exitCode).toBe(0);
  expect(rollup()).toBe("COMPLETED,COMPLETED,COMPLETED");
  expect(merge()).toEqual(["CLEAN", "clean"]);
});

// gh prints a pull request's checks to a pipe as five fields a tab apart — the name, the bucket,
// the time it ran as Go writes a duration, the link, and the description, which a check run has
// none of — failed checks first, then by name; and refuses a head that reports none. Measured on
// 2026-11-18 (prod-reads-19.json, item 25): #4326 printed `changes\tpass\t8s\t<link>\t`,
// `dispatch-two-builds / dispatch-two-builds\tpass\t10m52s\t<link>\t` and each skipped job
// `<name>\tskipping\t0\t<link>\t`, exit 0; #2827, whose head reported none, `no checks reported
// on the '<branch>' branch` on stderr and nothing on stdout, exit 1. Catches: the world's
// `<seconds>s` for every time, a line with no description, a failure listed after a pass, and a
// head with no check answered with nothing and exit 0.
test("gh pr checks prints gh's lines for the head's checks, and gh's refusal for a head with none", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const file = path.join(dir, "fixtures.json");
  const fixtures = JSON.parse(readFileSync(file, "utf8"));
  const run = (name: string, conclusion: string, completed: string) => ({
    name,
    status: "completed",
    conclusion,
    started_at: "2026-11-18T08:00:00Z",
    completed_at: `2026-11-18T${completed}Z`,
    html_url: `https://example.invalid/${name}`,
  });
  fixtures.routes.unshift(
    {
      match: `^gh api .*commits/${first}/check-runs`,
      stdout: {
        check_runs: [
          run("lint", "success", "08:10:52"),
          run("changes", "success", "08:00:08"),
          run("docs", "skipped", "08:00:00"),
          run("test", "failure", "09:00:05"),
        ],
      },
    },
    { match: "^gh api .*check-runs", stdout: { check_runs: [] } }
  );
  writeFileSync(file, JSON.stringify(fixtures));
  const line = (name: string, bucket: string, time: string) =>
    `${name}\t${bucket}\t${time}\thttps://example.invalid/${name}\t\n`;
  expect(asked(dir, "reviewer", ["pr", "checks", "1001"])).toEqual([
    1,
    [
      line("test", "fail", "1h0m5s"),
      line("changes", "pass", "8s"),
      line("docs", "skipping", "0"),
      line("lint", "pass", "10m52s"),
    ].join(""),
    "",
  ]);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(asked(dir, "reviewer", ["pr", "checks", "1001"])).toEqual([
    1,
    "",
    "no checks reported on the 'legion/LWEVAL-1' branch\n",
  ]);
});

// gh buckets a check by its conclusion once it completed and its status before (gh 2.98.0's
// pkg/cmd/pr/checks/aggregate.go and output.go, read on 2026-11-18): a success passes; a skip and
// a neutral conclusion skip; a failure, a time-out and an action required fail; a cancel is a
// bucket of its own, printed `fail` to a pipe and counted as neither failed nor pending; every
// other state (`stale`, `startup_failure`, a status not yet completed) is pending. It exits 1 once
// one failed, else 8 while one is pending, else 0. Catches: a neutral conclusion read as a pass,
// a stale or start-up failure read as a failure, and a cancel that exits 1.
test("gh pr checks buckets every state of a check as gh does", () => {
  const { dir } = runDir({ fixture: true });
  const file = path.join(dir, "fixtures.json");
  const held = JSON.parse(readFileSync(file, "utf8"));
  const checksOf = (runs: object[]) => {
    const routes = [
      { match: "^gh api .*check-runs", stdout: { check_runs: runs } },
      ...held.routes,
    ];
    writeFileSync(file, JSON.stringify({ ...held, routes }));
    return asked(dir, "reviewer", ["pr", "checks", "1001"]);
  };
  const run = (name: string, status: string, conclusion: string | null = null) => ({
    name,
    status,
    conclusion,
    html_url: `https://example.invalid/${name}`,
  });
  const done = (name: string, conclusion: string) => run(name, "completed", conclusion);
  const line = (name: string, bucket: string) =>
    `${name}\t${bucket}\t0\thttps://example.invalid/${name}\t\n`;
  expect(
    checksOf([
      done("a-success", "success"),
      done("b-neutral", "neutral"),
      done("c-skipped", "skipped"),
      done("d-failure", "failure"),
      done("e-timed-out", "timed_out"),
      done("f-action", "action_required"),
      done("g-cancelled", "cancelled"),
      done("h-stale", "stale"),
      done("i-startup", "startup_failure"),
      run("j-queued", "queued"),
      run("k-running", "in_progress"),
    ])
  ).toEqual([
    1,
    [
      line("d-failure", "fail"),
      line("e-timed-out", "fail"),
      line("f-action", "fail"),
      line("a-success", "pass"),
      line("b-neutral", "skipping"),
      line("c-skipped", "skipping"),
      line("g-cancelled", "fail"),
      line("h-stale", "pending"),
      line("i-startup", "pending"),
      line("j-queued", "pending"),
      line("k-running", "pending"),
    ].join(""),
    "",
  ]);
  expect(checksOf([done("b-neutral", "neutral"), run("j-queued", "queued")])).toEqual([
    8,
    line("b-neutral", "skipping") + line("j-queued", "pending"),
    "",
  ]);
  expect(checksOf([done("a-success", "success"), done("g-cancelled", "cancelled")])).toEqual([
    0,
    line("a-success", "pass") + line("g-cancelled", "fail"),
    "",
  ]);
});

// gh prints `gh auth status` to a pipe for the account its hosts.yml names: the host, the
// account and the file, the account active, the protocol git uses, and the token masked as gh
// 2.98.0 masks one (pkg/cmd/auth/status's `maskToken`: `ghs_`, then a `*` for each character
// after it); an installation token holds no scopes, so no scope line. Measured on 2026-11-18
// from two INFRA issue pods (prod-reads-19.json, item 21, production-shapes.json's `gh auth
// status`): exit 0, nothing on stderr, and `github.com`, `  ✓ Logged in to github.com account
// x-access-token (<GH_CONFIG_DIR>/hosts.yml)`, `  - Active account: true`, `  - Git operations
// protocol: https`, `  - Token: ghs_` and its `*`s. The world writes the token line from the
// shape of the token the rig gives every pane (daemon-pane.ts's RIG_GH_TOKEN) and reads no token,
// so a hosts.yml holding another names none of it. Catches: the world's
// `{"login":"widgets-reviewer[bot]"}`, which gh never prints, to every role; a stand-in that
// reads the pane's token (CodeQL's alert at the wip head: the token's text flowed into the
// answer), which prints its length or its text; and a flag the world does not model
// (`--show-token`, `--json`) answered all the same. Production answers `-t` and `--show-token`,
// alone or among joined short flags, and `gh auth token` with the token itself, which no stand-in
// reads, so the world refuses each (the architect's ruling of 2026-11-18T11:34Z), and health's
// rule 21 names the run that asked (`credentialPrinted`); catches one of them answered.
test("gh auth status prints gh's text for the pane's own gh configuration, and no token", () => {
  const { dir } = runDir({ fixture: true });
  const config = path.join(dir, "pane-gh");
  mkdirSync(config);
  const token = `ghs_${fixtureSecret("pane")}`;
  // The file as the daemon renders it for a role's claim (packages/daemon's `ghconfig.Hosts`),
  // holding a token of another length than the rig's, random as a real one is.
  writeFileSync(
    path.join(config, "hosts.yml"),
    [
      ...["github.com:", `    oauth_token: ${token}`, "    user: x-access-token"],
      ...["    git_protocol: https", "    users:", "        x-access-token:"],
      `            oauth_token: ${token}`,
      "",
    ].join("\n")
  );
  const status = [
    "github.com",
    `  ✓ Logged in to github.com account x-access-token (${config}/hosts.yml)`,
    "  - Active account: true",
    "  - Git operations protocol: https",
    `  - Token: ghs_${"*".repeat(RIG_GH_TOKEN.length - "ghs_".length)}`,
    "",
  ].join("\n");
  const read = pinnedRead("gh auth status");
  const pane = { GH_CONFIG_DIR: config };
  for (const by of ["implementer", "reviewer"]) {
    const got = asked(dir, by, ["auth", "status"], pane);
    expect([by, ...got, shapeDiff(got[1], read)]).toEqual([
      by,
      read.exit,
      status,
      read.stderr,
      SAME_SHAPE,
    ]);
  }
  for (const flag of [["--show-token"], ["-t"], ["-at"], ["-ta"], ["--json", "hosts"]])
    expect([flag, ...asked(dir, "reviewer", ["auth", "status", ...flag], pane)]).toEqual([
      flag,
      1,
      "",
      "gh: Not Found (HTTP 404)\n",
    ]);
  expect(asked(dir, "implementer", ["auth", "token"], pane)).toEqual([
    1,
    "",
    "gh: Not Found (HTTP 404)\n",
  ]);
  expect(readFileSync(path.join(dir, "calls.jsonl"), "utf8")).not.toContain(token.slice(4));
});

/** gh's refusals of `gh run view` and `gh run watch` naming no run where no terminal can pick one,
 * each its line and the command's usage, as measured on 2026-11-18 (the implementer's readings,
 * m/run-view-noid.err and m/run-watch-noid.err), byte for byte. */
const RUN_VIEW_USAGE = `run or job ID required when not running interactively

Usage:  gh run view [<run-id>] [flags]

Flags:
  -a, --attempt uint      The attempt number of the workflow run
      --exit-status       Exit with non-zero status if run failed
  -j, --job string        View a specific job ID from a run
  -q, --jq expression     Filter JSON output using a jq expression
      --json fields       Output JSON with the specified fields
      --log               View full log for either a run or specific job
      --log-failed        View the log for any failed steps in a run or specific job
  -t, --template string   Format JSON output using a Go template; see "gh help formatting"
  -v, --verbose           Show job steps
  -w, --web               Open run in the browser
  
`;
const RUN_WATCH_USAGE = `run ID required when not running interactively

Usage:  gh run watch <run-id> [flags]

Flags:
      --compact        Show only relevant/failed steps
      --exit-status    Exit with non-zero status if run fails
  -i, --interval int   Refresh interval in seconds (default 3)
  
`;

// The world holds no workflow run, so gh refuses a `gh run view` or `gh run watch` of one as
// production does: GitHub's 404 for the run, at the URL gh read it at, on the repository the
// call names; and, naming none, gh's refusal and the command's usage, each exit 1 with nothing on
// stdout. Measured on 2026-11-18: `gh run view 1 --repo <o>/<r>` and `gh run watch 1 --repo
// <o>/<r>` printed `failed to get run: HTTP 404: Not Found
// (https://api.github.com/repos/<o>/<r>/actions/runs/1?exclude_pull_requests=true)`
// (prod-reads-19.json, items 26 and 27), and the two with no id RUN_VIEW_USAGE and
// RUN_WATCH_USAGE (implementer-reads.json). Catches: the first line of each alone, which is what
// the world printed, and a run named among flags read as none.
test("gh run view and gh run watch are gh's refusals of a run the world has not got, and of none named", () => {
  const { dir } = runDir({ fixture: true });
  const notFound = (id: string) =>
    `failed to get run: HTTP 404: Not Found (https://api.github.com/repos/example/widgets/actions/runs/${id}?exclude_pull_requests=true)\n`;
  expect([
    asked(dir, "reviewer", ["run", "view", "1"]),
    asked(dir, "implementer", ["run", "watch", "1", "--exit-status"]),
    asked(dir, "reviewer", ["run", "view", "-R", "example/widgets", "--log-failed", "7"]),
    asked(dir, "reviewer", ["run", "view"]),
    asked(dir, "implementer", ["run", "watch", "--repo", "example/widgets", "-i", "5"]),
  ]).toEqual([
    [1, "", notFound("1")],
    [1, "", notFound("1")],
    [1, "", notFound("7")],
    [1, "", RUN_VIEW_USAGE],
    [1, "", RUN_WATCH_USAGE],
  ]);
});

// gh prints the answer of a `gh api` call no `--jq` reads as GitHub sent it, with no line end of
// its own, a paginated answer's pages one after another (a list's made one list), and an error's
// body the same; what `--jq` prints, and the JSON of gh's own commands, end with one. Measured on
// 2026-11-18 from INFRA issue pods with gh 2.98.0 against cybermonklabs/legion: the viewer by `gh
// api graphql` and `gh api /graphql` (implementer-reads.json, its raw files), a REST pull
// request, `--paginate` of a list and of a GraphQL connection, `--slurp`, a 403's and a 401's
// body, and the 404 of an endpoint GitHub does not serve (indented, `\r\n` line ends) and of an
// object a served one has not got (one line) each ended in its last `}` or `]`; `--jq .number`,
// `gh pr view --json number` and `gh pr list --json number` ended in a line end, and so did the
// diff media type of a comparison, which is the diff's own. Catches: the world's line end after
// every answer and between pages, and its 404 bodies indented with `\n` line ends.
test("gh api prints GitHub's answer as it came, and --jq and gh's own JSON end with a line end", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const out = (...args: string[]) => String(asked(dir, "reviewer", args)[1]);
  const viewer = `{"data":{"viewer":{"login":"${rigValue("world_review_app")}[bot]"}}}`;
  expect(out("api", "graphql", "-f", "query=query{viewer{login}}")).toBe(viewer);
  expect(out("api", "/graphql", "-f", "query=query{viewer{login}}")).toBe(viewer);
  const pull = out("api", "repos/example/widgets/pulls/1001");
  expect([pull.at(0), pull.at(-1)]).toEqual(["{", "}"]);
  expect(out("api", "repos/example/widgets/pulls/1001", "--jq", ".number")).toBe("1001\n");
  expect(out("pr", "view", "1001", "--json", "number")).toBe('{"number":1001}\n');
  expect(out("pr", "list", "--repo", "example/widgets", "--json", "number")).toBe(
    '[{"number":1001}]\n'
  );
  // Correct's world holds one thread, the workflow's; a review comment of the run's own opens a
  // second, so the connection pages twice at `first:1`.
  expect(
    asked(dir, "reviewer", [
      ...["api", "--method", "POST", "repos/example/widgets/pulls/1001/comments"],
      ...["-f", "body=This sentence says what the CLI does.", "-f", `commit_id=${builtHead(dir)}`],
      ...["-f", "path=docs/cli.md", "-F", "line=1"],
    ])[0]
  ).toBe(0);
  const paged = out(
    ...["api", "graphql", "--paginate", "-f"],
    'query=query($endCursor:String){repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewThreads(first:1,after:$endCursor){nodes{id}pageInfo{hasNextPage endCursor}}}}}'
  );
  expect([paged.split("}{").length, paged.includes("\n"), paged.at(-1)]).toEqual([2, false, "}"]);
  for (const body of ["a first note", "a second note"])
    expect(asked(dir, "reviewer", ["pr", "comment", "1001", "--body", body])[0]).toBe(0);
  const comments = "repos/example/widgets/issues/1001/comments?per_page=1";
  const listed = out("api", "--paginate", comments);
  expect([JSON.parse(listed).length, listed.at(-1)]).toEqual([2, "]"]);
  const slurped = out("api", "--paginate", "--slurp", comments);
  expect([JSON.parse(slurped).map((page: unknown[]) => page.length), slurped.at(-1)]).toEqual([
    [1, 1],
    "]",
  ]);
  expect(asked(dir, "reviewer", ["api", "user"])).toEqual([
    1,
    JSON.stringify({
      message: "Resource not accessible by integration",
      documentation_url: "https://docs.github.com/rest/users/users#get-the-authenticated-user",
      status: "403",
    }),
    "gh: Resource not accessible by integration (HTTP 403)\n",
  ]);
  expect(out("api", "repos/example/widgets/no-such-endpoint")).toBe(GITHUB_404);
  expect(out("api", `${REVIEWS}/1`)).toBe(
    notFoundOf("https://docs.github.com/rest/pulls/reviews#get-a-review-for-a-pull-request")
  );
  const diff = out(
    ...["api", "repos/example/widgets/compare/main...legion/LWEVAL-1"],
    ...["-H", "Accept: application/vnd.github.diff"]
  );
  expect([diff.startsWith("diff --git "), diff.endsWith("\n"), diff.endsWith("\n\n")]).toEqual([
    true,
    true,
    false,
  ]);
}, 60_000);

// Catches: a record nothing opens, where a run whose agents called no gh and no bun leaves no
// calls.jsonl and scores as the rig error of a record nothing wrote rather than as the run that
// called nothing (score.ts's `unscored`). rig.sh's worker_boot asks for that line with `--open`
// through the pane's own gh, whose wrapper inserts the program word (`gh-standin.ts gh --open`),
// before its agent runs, as it starts the publish recorder, which writes its own `recording`
// line; a second ask adds nothing, since the record is already open.
test("--open opens the record with one line and records no call", () => {
  const { dir } = runDir({ fixture: true });
  const open = () =>
    spawnChecked(["bun", STANDIN, "gh", "--open"], {
      env: callEnv(dir),
    });
  const recorded = () =>
    readFileSync(path.join(dir, "calls.jsonl"), "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line));
  expect(open().exitCode).toBe(0);
  expect(recorded()).toMatchObject([{ opened: "calls.jsonl" }]);
  expect(recorded()[0]).not.toHaveProperty("argv");
  expect(open().exitCode).toBe(0);
  expect(recorded()).toHaveLength(1);
  // The call after it is recorded under that line, and opens nothing of its own.
  answer(dir, ["pr", "view", "1001"]);
  expect(recorded()).toHaveLength(2);
  expect(recorded()[1]).toMatchObject({ as: "gh", argv: ["pr", "view", "1001"], exit: 0 });
});

// Catches: a stand-in with no answer for `gh api repos/<o>/<r>/compare/<a>...<b>`, which the rule
// has the implementer run to show its push changes no code that runs (review-threads.md step 2).
// The real GitHub API answers that form — measured on 2026-11-10, `gh api
// repos/cybermonklabs/legion/compare/756789f8...main --jq '{status,ahead_by,behind_by}'` exited 0
// — while this stand-in answered `gh: Not Found (HTTP 404)`, and the implementer's comparison
// failed in all 10 head runs of that batch of the threads-closed case that `correct` replaced. The
// two heads a comparison names are the run's own, so it is read from the run's remote and no
// route serves it.
test("a comparison of two commits is read from the run's own remote", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const compared = answer(dir, [
    "api",
    `repos/example/widgets/compare/${first}...${second}`,
    "--jq",
    "[.status, .ahead_by, .behind_by, (.files | length), .files[0].filename] | @tsv",
  ]);
  expect(compared).toBe(["ahead", 1, 0, 1, "docs.md"].join("\t"));
  // The other way round is the same comparison read from the other side, as GitHub reads it.
  expect(
    answer(dir, [
      "api",
      `repos/example/widgets/compare/${second}...${first}`,
      "--jq",
      "[.status, .ahead_by, .behind_by, (.files | length)] | @tsv",
    ])
  ).toBe(["behind", 0, 1, 0].join("\t"));
  // A comparison of one commit with itself changes nothing, which is what the fix's own
  // comparison looks like when the implementer names the same head twice.
  expect(
    answer(dir, [
      "api",
      `repos/example/widgets/compare/${first}...${first}`,
      "--jq",
      "[.status, .ahead_by, .behind_by, (.files | length)] | @tsv",
    ])
  ).toBe(["identical", 0, 0, 0].join("\t"));
});

// Catches: a comparison answered out of the run's world for refs the remote has not got, which
// would hand an agent counts of a comparison nobody made. GitHub answers 404 for a comparison of
// commits a repository does not hold, and so does the stand-in, itself (gh-standin.ts's
// `comparison`), with GitHub's 404 body of a comparison; the row of two dots holds that body.
test("a comparison the run's remote cannot resolve is 404, as GitHub answers it", () => {
  const { dir } = runDir({ fixture: true });
  const call = spawnChecked(
    ["bun", STANDIN, "gh", "api", "repos/example/widgets/compare/deadbeef...cafed00d"],
    { env: callEnv(dir) }
  );
  expect(call.exitCode).toBe(1);
  expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
});

// Catches: a stand-in with no answer for `gh api repos/<o>/<r>/commits/<sha>`, GitHub's read of
// one commit. In INFRA-764's round 29 (run 94caf2e09844, threads-closed correct base, at 446a02fb)
// the reviewer read the commit the implementer pushed that way, to see its author and parents,
// and the stand-in answered `gh: Not Found (HTTP 404)` for a commit the run's own remote holds,
// which the world-health check refused. GitHub answers that read with the commit, its parents,
// files and stats, for a full SHA and for a short one alike, and answers 422 `No commit found
// for SHA` for one the repository does not hold — measured on 2026-11-21, `gh api
// repos/cybermonklabs/legion/commits/bbf69791…` exited 0 with one parent and 3 files, the same
// call naming `bbf69791` answered the full SHA, and one naming forty `deadbeef` digits exited 1
// with that 422.
test("a commit is read from the run's own remote, as GitHub answers one", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const read = (ref: string) =>
    answer(dir, [
      "api",
      `repos/example/widgets/commits/${ref}`,
      "--jq",
      "[.sha, (.parents | length), .parents[0].sha, (.files | length), .files[0].filename] | @tsv",
    ]);
  const want = [second, 1, first, 1, "docs.md"].join("\t");
  expect(read(second)).toBe(want);
  expect(read(second.slice(0, 8))).toBe(want);
  const missing = spawnChecked(
    ["bun", STANDIN, "gh", "api", `repos/example/widgets/commits/${"deadbeef".repeat(5)}`],
    { env: callEnv(dir) }
  );
  expect(missing.exitCode).toBe(1);
  expect(missing.stderr.toString()).toContain(
    `gh: No commit found for SHA: ${"deadbeef".repeat(5)} (HTTP 422)`
  );
});

/** The answer GitHub's description requires of the world's REST route `route`, from
 * rest-required.json: the operation it is held to (its key) and the fields `answer` lacks. */
function heldTo(route: string, answer: unknown): [string | undefined, string[]] {
  const operation = restOperation(route);
  if (operation?.schema === undefined) throw new Error(`the extract holds no schema of ${route}`);
  return [operation.key, absentFields(answer, operation.schema)];
}

// Catches: a read of one commit that is not GitHub's "Get a commit" whole: short of a field its
// description requires (its `stats`, a file's `blob_url`), its files other than the diff from its
// first parent, a root commit answered with no files where GitHub lists each file it adds, a
// branch whose name holds a slash (or that slash written `%2F`) read as no ref where GitHub
// answers its tip, and the 422 of a ref the remote has not got answered without GitHub's body on
// stdout. Each was measured on 2026-11-21 against cybermonklabs/legion, and gh-standin.ts's
// `commitOf` names each read and its `Date:`.
test("a commit's read is GitHub's whole, by any ref GitHub reads, its files the diff from its first parent", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const repo = "repos/example/widgets";
  const read = (ref: string) => JSON.parse(ghCall(dir, "api", `${repo}/commits/${ref}`).out);
  const filesOf = (commit: { files: Record<string, unknown>[] }) =>
    commit.files.map((file) => [
      file.filename,
      file.status,
      file.additions,
      file.deletions,
      file.changes,
      file.patch,
    ]);
  const fix = read(second);
  expect(heldTo("GET repos/{owner}/{repo}/commits/{ref+}", fix)).toEqual([
    "GET /repos/{owner}/{repo}/commits/{ref}",
    [],
  ]);
  expect([fix.stats, filesOf(fix)]).toEqual([
    { total: 1, additions: 1, deletions: 0 },
    [["docs.md", "added", 1, 0, 1, "@@ -0,0 +1 @@\n+the text fix"]],
  ]);
  const root = read(first);
  expect([root.parents, root.stats, filesOf(root)]).toEqual([
    [],
    { total: 1, additions: 1, deletions: 0 },
    [["greet.ts", "added", 1, 0, 1, "@@ -0,0 +1 @@\n+export const greet = () => 1;"]],
  ]);
  expect([read("legion/LWEVAL-1").sha, read("legion%2FLWEVAL-1").sha]).toEqual([first, first]);
  const remote = path.join(dir, "remote.git");
  const media = (type: string, ref: string) =>
    ghRaw(dir, "api", "-H", `Accept: application/vnd.github.${type}`, `${repo}/commits/${ref}`).out;
  expect(media("sha", second.slice(0, 8))).toBe(second);
  expect(media("diff", second)).toBe(`${git(remote, "diff", "--no-renames", first, second)}\n`);
  for (const ref of [first, second])
    expect([ref, media("patch", ref)]).toEqual([
      ref,
      `${git(remote, "format-patch", "--stdout", "-1", "--no-signature", "--no-renames", ref)}\n`,
    ]);
  expect(media("patch", second)).toStartWith(`From ${second} Mon Sep 17 00:00:00 2001\n`);
  // A commit of two files, pushed over the branch, its files paged and its stats whole.
  const work = path.join(dir, "two-files");
  git(dir, "clone", "-q", "-b", "legion/LWEVAL-1", remote, work);
  writeFileSync(path.join(work, "a.md"), "a\n");
  writeFileSync(path.join(work, "b.md"), "b\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "two files");
  git(work, "push", "-q", "origin", "legion/LWEVAL-1");
  const two = git(work, "rev-parse", "HEAD");
  const page = (...argv: string[]) =>
    ghCall(dir, "api", ...argv, "--jq", '"\\(.stats.total) \\([.files[].filename] | join(","))"')
      .out;
  expect(page(`${repo}/commits/${two}`)).toBe("2 a.md,b.md");
  expect(page(`${repo}/commits/${two}?per_page=1`)).toBe("2 a.md");
  expect(page(`${repo}/commits/${two}?per_page=1&page=2`)).toBe("2 b.md");
  expect(page(`${repo}/commits/${two}?page=2`)).toBe("2");
  expect(page("--paginate", `${repo}/commits/${two}?per_page=1`)).toBe("2 a.md\n2 b.md");
  // A commit's own endpoints are not the commit: its pull requests, which the gh world serves
  // no answer for, stay the 404 of no route, and its check runs and its status their own routes'.
  expect(ghCall(dir, "api", `${repo}/commits/${second}/pulls`)).toEqual({
    code: 1,
    out: GITHUB_404,
    err: "gh: Not Found (HTTP 404)\n",
  });
  for (const own of ["check-runs", "status"])
    ghCall(dir, "api", `${repo}/commits/${second}/${own}`);
  expect(
    recordedCalls(dir)
      .slice(-3)
      .map((call) => call.handler)
  ).toEqual(["unrouted", "check-runs", "combined-status"]);
  expect(ghCall(dir, "api", `${repo}/commits/nosuchref`)).toEqual({
    code: 1,
    out: JSON.stringify({
      message: "No commit found for SHA: nosuchref",
      documentation_url: "https://docs.github.com/rest/commits/commits#get-a-commit",
      status: "422",
    }),
    err: "gh: No commit found for SHA: nosuchref (HTTP 422)\n",
  });
});

// Catches: a list of commits that is not GitHub's "List commits": read from another ref than the
// one `sha` names (in the query string, or as the field gh sends there on a GET), or from no
// default branch where it names none; oldest first; not paged by `per_page` and `page`; an item
// short of a field GitHub requires, or carrying the `files` a list item has not got; a ref the
// remote has not got answered anything but GitHub's 404 of the list; and a filter the world does
// not model (`path`) answered as though the list were filtered, where it is refused as a call
// the world does not model. gh-standin.ts's `commitList` names the reads of 2026-11-21.
test("the commits a ref reaches are listed newest first from the run's own remote, paged as GitHub pages them", () => {
  const { dir, first, second } = runDir({ fixture: true });
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/main", first);
  const repo = "repos/example/widgets";
  const shas = (...argv: string[]) =>
    ghCall(dir, "api", ...argv, "--jq", '[.[].sha] | join(" ")').out;
  expect(shas(`${repo}/commits?sha=${second}`)).toBe(`${second} ${first}`);
  expect(shas(`${repo}/commits?sha=${second.slice(0, 8)}&per_page=1&page=2`)).toBe(first);
  expect(shas("--paginate", `${repo}/commits?sha=${second}&per_page=1`)).toBe(
    `${second}\n${first}`
  );
  expect(shas("-X", "GET", `${repo}/commits`, "-f", `sha=${second}`)).toBe(`${second} ${first}`);
  expect(shas(`${repo}/commits`)).toBe(first);
  const listed = JSON.parse(ghCall(dir, "api", `${repo}/commits?sha=${second}`).out);
  expect(heldTo("GET repos/{owner}/{repo}/commits", listed)).toEqual([
    "GET /repos/{owner}/{repo}/commits",
    [],
  ]);
  expect(listed.map((item: Record<string, unknown>) => "files" in item)).toEqual([false, false]);
  expect(ghCall(dir, "api", `${repo}/commits?sha=nosuchref`)).toEqual({
    code: 1,
    out: notFoundOf("https://docs.github.com/rest/commits/commits#list-commits"),
    err: "gh: Not Found (HTTP 404)\n",
  });
  expect(ghCall(dir, "api", `${repo}/commits?sha=${second}&path=docs.md`)).toEqual({
    code: 1,
    out: GITHUB_404,
    err: "gh: Not Found (HTTP 404)\n",
  });
  expect(recordedCalls(dir).map((call) => call.handler)).toEqual([
    ...Array(7).fill("commits"),
    "unrouted",
  ]);
});

// Catches: a commit's git data that is not GitHub's "Get a commit object": short of a field its
// description requires, in another order of keys than GitHub's, its parents linked to the commits
// API rather than the git-data one, and a short SHA or a branch answered as the commit, where
// GitHub reads a full SHA there alone and answers 404 for anything else: measured on 2026-11-21
// with `gh api -i` against cybermonklabs/legion, `git/commits/bbf697` (`Date: Sat, 21 Nov 2026
// 08:42:59 GMT`) and `git/commits/main` (08:43:00 GMT) each answered `HTTP/2.0 404 Not Found`,
// its body naming "Get a commit object", where the full SHA answered 200 (08:42:58 GMT).
test("a commit's git data is read by its full SHA alone, as GitHub reads it", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  git(remote, "update-ref", "refs/heads/main", first);
  const repo = "repos/example/widgets";
  const api = "https://api.github.com/repos/example/widgets";
  const got = JSON.parse(ghCall(dir, "api", `${repo}/git/commits/${second}`).out);
  expect(heldTo("GET repos/{owner}/{repo}/git/commits/{sha}", got)).toEqual([
    "GET /repos/{owner}/{repo}/git/commits/{commit_sha}",
    [],
  ]);
  const tree = git(remote, "rev-parse", `${second}^{tree}`);
  // The message's first line: a pod's git configuration may add trailers below it.
  const subject = String(got.message).split("\n")[0];
  expect([Object.keys(got), got.sha, got.url, got.tree, subject, got.parents]).toEqual([
    [
      ...["sha", "node_id", "url", "html_url", "author", "committer", "tree", "message"],
      ...["parents", "verification"],
    ],
    second,
    `${api}/git/commits/${second}`,
    { sha: tree, url: `${api}/git/trees/${tree}` },
    "docs: the text fix",
    [
      {
        sha: first,
        url: `${api}/git/commits/${first}`,
        html_url: `https://github.com/example/widgets/commit/${first}`,
      },
    ],
  ]);
  for (const ref of [second.slice(0, 8), "main", "deadbeef".repeat(5)])
    expect([ref, ghCall(dir, "api", `${repo}/git/commits/${ref}`)]).toEqual([
      ref,
      {
        code: 1,
        out: notFoundOf("https://docs.github.com/rest/git/commits#get-a-commit-object"),
        err: "gh: Not Found (HTTP 404)\n",
      },
    ]);
});

// Catches: refs that are not GitHub's: `git/refs/<ref>` answered as a list where it names one ref
// whole (GitHub answers that ref), or as 404 where it is a prefix of names (GitHub lists the refs
// it starts); a ref of the run's remote missing from `git/refs`; the two refs GitHub keeps for the
// pull request missing, `refs/pull/<n>/head` at the branch's tip and `refs/pull/<n>/merge` at the
// pull request's test merge; a 404 whose body names another page than GitHub's; and a ref or a
// list short of a field GitHub's description requires, a prefix's list held to the list
// operation (rest-required.ts's ANSWERED_AS names the reads of 2026-11-21 behind each).
test("the refs are GitHub's, the remote's and the pull request's, one by its name and a list by a prefix", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const remote = path.join(dir, "remote.git");
  const base = git(remote, "rev-parse", "refs/heads/main");
  const repo = "repos/example/widgets";
  const merge = ghCall(dir, "api", `${repo}/pulls/1001`, "--jq", ".merge_commit_sha").out;
  expect(merge).toMatch(/^[0-9a-f]{40}$/);
  const refs = (path: string) =>
    JSON.parse(ghCall(dir, "api", `${repo}/${path}`, "--jq", "[.[] | [.ref, .object.sha]]").out);
  expect(refs("git/refs")).toEqual([
    ["refs/heads/legion/LWEVAL-1", head],
    ["refs/heads/main", base],
    ["refs/pull/1001/head", head],
    ["refs/pull/1001/merge", merge],
  ]);
  expect(refs("git/refs/heads")).toEqual([
    ["refs/heads/legion/LWEVAL-1", head],
    ["refs/heads/main", base],
  ]);
  expect(refs("git/refs/heads/ma")).toEqual([["refs/heads/main", base]]);
  const one = (path: string) => JSON.parse(ghCall(dir, "api", `${repo}/${path}`).out);
  for (const path of ["git/refs/heads/main", "git/ref/heads/main"])
    expect([path, one(path)]).toEqual([
      path,
      {
        ref: "refs/heads/main",
        node_id: `REF_kwDOAAAAAc${Buffer.from("refs/heads/main").toString("base64url")}`,
        url: "https://api.github.com/repos/example/widgets/git/refs/heads/main",
        object: {
          sha: base,
          type: "commit",
          url: `https://api.github.com/repos/example/widgets/git/commits/${base}`,
        },
      },
    ]);
  expect([
    one("git/ref/pull/1001/head").object.sha,
    one("git/refs/pull/1001/merge").object.sha,
  ]).toEqual([head, merge]);
  expect(heldTo("GET repos/{owner}/{repo}/git/refs/{ref+}", one("git/refs/heads/main"))).toEqual([
    "GET /repos/{owner}/{repo}/git/ref/{ref}",
    [],
  ]);
  for (const path of ["git/refs", "git/refs/heads"])
    expect([path, heldTo("GET repos/{owner}/{repo}/git/refs", one(path))]).toEqual([
      path,
      ["GET /repos/{owner}/{repo}/git/matching-refs/{ref}", []],
    ]);
  const refused = (docs: string) => ({
    code: 1,
    out: notFoundOf(`https://docs.github.com/rest/git/refs#${docs}`),
    err: "gh: Not Found (HTTP 404)\n",
  });
  expect(ghCall(dir, "api", `${repo}/git/refs/heads/nosuch`)).toEqual(
    refused("get-all-references-in-a-namespace")
  );
  expect(ghCall(dir, "api", `${repo}/git/ref/heads/ma`)).toEqual(refused("get-a-reference"));
}, 60_000);

// Catches: a stand-in with no answer for `gh api repos/<o>/<r>/pulls/<n>/commits` and `…/files`,
// GitHub's lists of a pull request's commits and files, which it answered `gh: Not Found (HTTP
// 404)` with the body of an endpoint GitHub does not serve; commits other than the ones the head
// holds and the base does not, or newest first, where GitHub lists them oldest first; an item
// short of a field GitHub requires, or carrying a commit's `stats` or `files`, which a list item
// has not got; files other than the comparison's of the base and the head, which the read of a
// commit and the comparison list through one reading (`diffFiles`); a list not paged by `per_page`
// and `page`; and a number the world holds no pull request of answered otherwise than GitHub's 404
// of the list, its body naming the list's page, which health reads as GitHub's own refusal.
// gh-standin.ts's `pullList` names the reads of 2026-11-21 behind each.
test("a pull request's commits and files are listed from the run's own remote, as GitHub lists them", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  git(remote, "update-ref", "refs/heads/main", first);
  git(remote, "update-ref", "refs/heads/legion/LWEVAL-1", second);
  const work = path.join(dir, "two-more");
  git(dir, "clone", "-q", "-b", "legion/LWEVAL-1", remote, work);
  for (const name of ["b.md", "a.md"]) {
    writeFileSync(path.join(work, name), `${name}\n`);
    git(work, "add", "-A");
    git(work, "commit", "-qm", `add ${name}`);
  }
  git(work, "push", "-q", "origin", "legion/LWEVAL-1");
  const fourth = git(work, "rev-parse", "HEAD");
  const third = git(work, "rev-parse", "HEAD^");
  const repo = "repos/example/widgets";
  const listed = (jq: string, ...argv: string[]) => ghCall(dir, "api", ...argv, "--jq", jq).out;
  const shas = '[.[].sha] | join(" ")';
  const names = '[.[].filename] | join(" ")';
  expect([
    listed(shas, `${repo}/pulls/1001/commits`),
    listed(shas, `${repo}/pulls/1001/commits?per_page=2&page=2`),
    listed(shas, "--paginate", `${repo}/pulls/1001/commits?per_page=2`),
    listed(names, `${repo}/pulls/1001/files`),
    listed(names, `${repo}/pulls/1001/files?per_page=1&page=3`),
    listed(names, "--paginate", `${repo}/pulls/1001/files?per_page=2`),
  ]).toEqual([
    `${second} ${third} ${fourth}`,
    fourth,
    `${second} ${third}\n${fourth}`,
    "a.md b.md docs.md",
    "docs.md",
    "a.md b.md\ndocs.md",
  ]);
  const commits = JSON.parse(ghCall(dir, "api", `${repo}/pulls/1001/commits`).out);
  const files = JSON.parse(ghCall(dir, "api", `${repo}/pulls/1001/files`).out);
  const compared = JSON.parse(
    ghCall(dir, "api", `${repo}/compare/main...legion/LWEVAL-1`, "--jq", ".files").out
  );
  expect([
    heldTo("GET repos/{owner}/{repo}/pulls/{number}/commits", commits),
    commits.map((item: Record<string, unknown>) => "stats" in item || "files" in item),
    heldTo("GET repos/{owner}/{repo}/pulls/{number}/files", files),
    files,
  ]).toEqual([
    ["GET /repos/{owner}/{repo}/pulls/{pull_number}/commits", []],
    [false, false, false],
    ["GET /repos/{owner}/{repo}/pulls/{pull_number}/files", []],
    compared,
  ]);
  const refused = (docs: string) => ({
    code: 1,
    out: notFoundOf(`https://docs.github.com/rest/pulls/pulls#${docs}`),
    err: "gh: Not Found (HTTP 404)\n",
  });
  expect([
    ghCall(dir, "api", `${repo}/pulls/999999/commits`),
    ghCall(dir, "api", `${repo}/pulls/999999/files`),
  ]).toEqual([refused("list-commits-on-a-pull-request"), refused("list-pull-requests-files")]);
  expect(
    recordedCalls(dir)
      .slice(-2)
      .map((call) => [call.exit, call.handler])
  ).toEqual([
    [1, "pull-commits"],
    [1, "pull-files"],
  ]);
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect([scenario, ghUndeclared({ dir, scenario })]).toEqual([scenario, undefined]);
});

/** Production's answers to `gh api -i`, as production-headers.json pins them: each read's status
 * line, its headers by name in the order gh printed them and what gh printed after them, and the
 * permission production named for each route's read. */
const PRODUCTION_HEADERS = JSON.parse(
  readFileSync(path.join(import.meta.dir, "production-headers.json"), "utf8")
) as {
  reads: Record<string, { status: string; headers: Record<string, string>; after_headers: string }>;
  permissions: Record<string, { value: string | null; date: string }>;
};
/** The headers whose values describe one answer: the world writes its own, never production's. */
const ANSWER_HEADERS = [
  "Date",
  "Etag",
  "Last-Modified",
  "X-Github-Request-Id",
  "X-Ratelimit-Remaining",
  "X-Ratelimit-Reset",
  "X-Ratelimit-Used",
];
/** What gh printed for a `gh api -i` call: its status line, its headers by name, and its body. */
interface Included {
  status: string;
  headers: [string, string][];
  body: string;
}
/** `out`, what gh printed for a `gh api -i` call, read as gh prints it: the status line ends in
 * `\n`, each header's line in `\r\n`, and a line of `\r\n` alone ends them, the body after it. */
function included(out: string): Included {
  const status = out.slice(0, out.indexOf("\n"));
  const end = out.indexOf("\r\n\r\n");
  const headers = out
    .slice(status.length + 1, end)
    .split("\r\n")
    .map((line): [string, string] => [
      line.slice(0, line.indexOf(": ")),
      line.slice(line.indexOf(": ") + 2),
    ]);
  return { status, headers, body: out.slice(end + 4) };
}
/** The REST `gh api` calls of the run under `dir` GitHub counts against the core rate limit of
 * the pane's App (gh-standin.ts's `headerBlock`): from a pane naming no role, each answered. */
const ratedCalls = (dir: string) =>
  recordedCalls(dir).filter((call) => {
    const argv = call.argv as string[];
    return (
      argv[0] === "api" &&
      !argv.includes("graphql") &&
      call.by === undefined &&
      !["unrouted", "wrapper"].includes(String(call.handler))
    );
  }).length;

// Catches: a `gh api -i` (`--include`) answered with the body alone, where gh prints GitHub's
// status line and its headers before it — the census of INFRA-764's runs found 14 such calls in 15
// runs, round 29's run 2d821d91b790 calls.jsonl:2 among them; a header production sends the
// repository's read with missing, or one it does not send written; a header of GitHub's service
// with another value than production's; a value that describes the answer other than the world's
// own (its `Date` the call's, its `Etag` a validator of the world's body, its `Last-Modified` the
// world's repository's `updated_at`, a request id in GitHub's form, and the rate limit of the
// pane's App counted from the run's own calls); and `--jq` run over the headers, or over nothing.
// Production's two reads, each with its `Date:`, are production-headers.json's `repository` and
// `repository --jq .visibility`.
test("gh api --include prints GitHub's status line and headers before the body, --jq run over the body alone", () => {
  const dir = pinnedWorldCopy();
  const repository = "repos/example/widgets";
  const plain = ghRaw(dir, "api", repository).out;
  const rated = ratedCalls(dir);
  const answers = [
    ghRaw(dir, "api", "--include", repository, "--jq", ".visibility").out,
    ghRaw(dir, "api", "-i", repository).out,
  ].map(included);
  const calls = recordedCalls(dir);
  const firstRated = calls.find((call) => (call.argv as string[])[0] === "api")?.at;
  const pins = [
    PRODUCTION_HEADERS.reads["repository --jq .visibility"],
    PRODUCTION_HEADERS.reads.repository,
  ];
  const sha = new Bun.CryptoHasher("sha256").update(plain).digest("hex");
  expect(
    answers.map(({ status, headers }, index) => {
      const pin = pins[index]?.headers ?? {};
      return [
        status,
        headers.map(([name]) => name),
        headers.filter(([name, value]) => !ANSWER_HEADERS.includes(name) && value !== pin[name]),
        Object.fromEntries(headers.filter(([name]) => ANSWER_HEADERS.includes(name))),
      ];
    })
  ).toEqual(
    pins.map((pin, index) => [
      pin?.status,
      Object.keys(pin?.headers ?? {}),
      [],
      {
        Date: new Date(String(calls.at(index - 2)?.at)).toUTCString(),
        Etag: `W/"${sha}"`,
        "Last-Modified": new Date(JSON.parse(plain).updated_at).toUTCString(),
        "X-Github-Request-Id": expect.stringMatching(/^[0-9A-F]{4}(:[0-9A-F]{6}){3}:[0-9A-F]{8}$/),
        "X-Ratelimit-Remaining": String(15_000 - rated - index - 1),
        "X-Ratelimit-Reset": String(Math.floor(Date.parse(String(firstRated)) / 1000) + 3600),
        "X-Ratelimit-Used": String(rated + index + 1),
      },
    ])
  );
  expect(answers.map(({ body }) => body)).toEqual([pins[0]?.after_headers, plain]);
}, 60_000);

// Catches: a route the stand-in serves that prints no status line with `--include`, or another
// status than GitHub's: a create of a comment answered `200 OK` where GitHub's description names
// `201 Created`, a refusal printed with a success's status line, a 404 of an endpoint GitHub does
// not serve with a rate limit GitHub does not count it in; a read's permission other than the one
// production named for it (production-headers.json's `permissions`), or one named for a write,
// which no pod measures; a GraphQL answer with REST's media type, resource or caching; headers out
// of the byte order gh prints them in; the body after them other than the call's own; a list's
// `Link` to its other pages missing, or not in GitHub's form (production-headers.json's `list`,
// gh-standin.ts's `linkOf`); and `--paginate` printing a list's pages as one, where gh with
// `--include` prints each page after its own headers.
test("every route the stand-in serves prints its status line and headers with --include", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const base = git(path.join(dir, "remote.git"), "rev-parse", "refs/heads/main");
  const reason: Record<number, string> = {
    200: "OK",
    201: "Created",
    403: "Forbidden",
    404: "Not Found",
  };
  const headerOf = (answer: Included, name: string) =>
    answer.headers.find(([one]) => one === name)?.[1];
  const verdicts = restCalls(head, base).map(([route, argv]) => {
    const answer = included(ghRaw(dir, "api", "-i", ...argv.slice(1)).out);
    const names = answer.headers.map(([name]) => name);
    return [
      route,
      answer.status,
      names.join() === [...names].sort().join(),
      typeof answerOf(answer.body),
      headerOf(answer, "X-Accepted-Github-Permissions"),
    ];
  });
  expect(verdicts).toEqual(
    restCalls(head, base).map(([route, , refusal]) => {
      const code =
        refusal === undefined
          ? (restOperation(route)?.status ?? 0)
          : Number(/\(HTTP (\d{3})\)/.exec(refusal)?.[1]);
      return [
        route,
        `HTTP/2.0 ${code} ${reason[code]}`,
        true,
        "object",
        PRODUCTION_HEADERS.permissions[route]?.value ?? undefined,
      ];
    })
  );
  const threads =
    'query=query{repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewThreads(first:20){nodes{isResolved}}}}}';
  const graphql = included(ghRaw(dir, "api", "-i", "graphql", "-f", threads).out);
  const unrouted = included(ghRaw(dir, "api", "-i", "repos/example/widgets/no-such-endpoint").out);
  expect([
    graphql.status,
    ...["X-Github-Media-Type", "X-Ratelimit-Resource", "X-Ratelimit-Limit"].map((name) =>
      headerOf(graphql, name)
    ),
    ...["Etag", "Cache-Control", "X-Accepted-Github-Permissions"].map((name) =>
      headerOf(graphql, name)
    ),
    JSON.parse(graphql.body).data.repository.pullRequest.reviewThreads.nodes.length > 0,
    unrouted.status,
    unrouted.headers.filter(([name]) => /^X-Ratelimit|^X-Accepted|^X-Github-Api/.test(name)),
    unrouted.body,
  ]).toEqual([
    "HTTP/2.0 200 OK",
    "github.v4; format=json",
    "graphql",
    "10000",
    undefined,
    undefined,
    undefined,
    true,
    "HTTP/2.0 404 Not Found",
    [],
    GITHUB_404,
  ]);
  const commits = `repos/example/widgets/commits?sha=${head}&per_page=1`;
  const total = Number(
    ghCall(dir, "api", `repos/example/widgets/commits?sha=${head}`, "--jq", "length").out
  );
  const link = (page: number) =>
    `<https://api.github.com/repositories/1/commits?sha=${head}&per_page=1&page=${page}>`;
  const first = included(ghRaw(dir, "api", "-i", commits).out);
  expect([total > 2, headerOf(first, "Link")]).toEqual([
    true,
    `${link(2)}; rel="next", ${link(total)}; rel="last"`,
  ]);
  const paged = ghRaw(dir, "api", "-i", "--paginate", commits, "--jq", ".[].sha").out;
  expect([paged.split("HTTP/2.0 200 OK\n").length - 1, paged.endsWith("\n")]).toEqual([
    total,
    true,
  ]);
}, 120_000);

// Catches: a review comment of the pull request's list or of a review's without
// `copilot_code_review_severity`, which production gives each, null for a comment no Copilot
// review made: measured on 2026-11-21, `gh api repos/cybermonklabs/legion/pulls/2827/comments --jq
// '.[0] | keys'` named it (`Date: Sat, 21 Nov 2026 10:21:12 GMT`), and `gh api -i` of
// `…/pulls/2827/comments?per_page=1` (10:21:02 GMT) and of
// `…/pulls/2827/reviews/5816301660/comments?per_page=1` (10:24:42 GMT) each answered it null.
test("each review comment carries copilot_code_review_severity, null, as production gives it", () => {
  const dir = pinnedWorldCopy();
  const severities = (endpoint: string) =>
    JSON.parse(
      ghCall(
        dir,
        "api",
        endpoint,
        "--jq",
        '[.[] | [has("copilot_code_review_severity"), .copilot_code_review_severity]]'
      ).out
    );
  const pulls = severities("repos/example/widgets/pulls/1001/comments");
  const review = severities("repos/example/widgets/pulls/1001/reviews/5001/comments");
  expect([pulls.length > 0, review.length > 0]).toEqual([true, true]);
  expect([pulls, review]).toEqual([pulls.map(() => [true, null]), review.map(() => [true, null])]);
}, 60_000);

// Catches, each a shape INFRA-764's round 25 made, against today's stand-in: `gh pr view <n> --json
// files --jq '.files[].path'` with no `--repo`, the pull request of the checkout's branch, answered
// with no `files` or with paths other than the pull request's files, which GitHub's REST list of
// them names (`pulls/<n>/files`); and a GraphQL query written inline with its owner, name and number
// as literals and no variables, `reviewThreads(first:20)` with each thread's `isResolved` and its
// first five comments' bodies and authors, answered otherwise than the same query sent with
// variables, or with a field it did not select.
test("round 25's pr view of the files and its inline thread query are answered as GitHub answers them", () => {
  const dir = pinnedWorldCopy();
  const files = ghCall(dir, "pr", "view", "1001", "--json", "files", "--jq", ".files[].path");
  const listed = ghCall(
    dir,
    "api",
    "repos/example/widgets/pulls/1001/files",
    "--jq",
    ".[].filename"
  );
  expect([files.code, files.err, files.out.split("\n").length > 0]).toEqual([0, "", true]);
  expect(files.out).toBe(listed.out);
  const selection =
    "reviewThreads(first:20){nodes{isResolved comments(first:5){nodes{body author{login}}}}}";
  const inline = ghCall(
    dir,
    "api",
    "graphql",
    "-f",
    `query=query{repository(owner:"example",name:"widgets"){pullRequest(number:1001){${selection}}}}`
  );
  const variables = ghCall(
    dir,
    ...["api", "graphql", "-F", "o=example", "-F", "r=widgets", "-F", "n=1001", "-f"],
    `query=query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){${selection}}}}`
  );
  const nodes = JSON.parse(inline.out).data.repository.pullRequest.reviewThreads.nodes;
  expect([inline.code, inline.err, inline.out]).toEqual([0, "", variables.out]);
  expect([
    nodes.length > 0,
    [
      ...new Set(
        nodes.flatMap((node: { comments: { nodes: Record<string, unknown>[] } }) => [
          JSON.stringify(Object.keys(node)),
          ...node.comments.nodes.map((comment) => JSON.stringify(Object.keys(comment))),
        ])
      ),
    ],
  ]).toEqual([true, ['["isResolved","comments"]', '["body","author"]']]);
}, 60_000);

// Catches: a list of the branches that is not GitHub's "List branches": a branch of the remote
// missing, one short of a field GitHub's description requires, every branch protected or none
// where the world's base alone is (by its ruleset), and a `protected` parameter that filters
// nothing. gh-standin.ts's `branchList` names the reads of 2026-11-21.
test("the branches are the remote's, the base alone protected, filtered by `protected` as GitHub filters them", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const base = git(path.join(dir, "remote.git"), "rev-parse", "refs/heads/main");
  const repo = "repos/example/widgets";
  const branches = (query: string) =>
    JSON.parse(
      ghCall(
        dir,
        "api",
        `${repo}/branches${query}`,
        "--jq",
        "[.[] | [.name, .commit.sha, .protected]]"
      ).out
    );
  expect(branches("")).toEqual([
    ["legion/LWEVAL-1", head, false],
    ["main", base, true],
  ]);
  expect(branches("?protected=true")).toEqual([["main", base, true]]);
  expect(branches("?protected=false")).toEqual([["legion/LWEVAL-1", head, false]]);
  expect(branches("?per_page=1&page=2")).toEqual([["main", base, true]]);
  const listed = JSON.parse(ghCall(dir, "api", `${repo}/branches`).out);
  expect(heldTo("GET repos/{owner}/{repo}/branches", listed)).toEqual([
    "GET /repos/{owner}/{repo}/branches",
    [],
  ]);
  expect(listed[1]).toEqual({
    name: "main",
    commit: { sha: base, url: `https://api.github.com/repos/example/widgets/commits/${base}` },
    protected: true,
  });
}, 60_000);

// Catches: a tree that is not GitHub's "Get a tree": its `sha` the tree's where the path names a
// commit (GitHub answers the commit's), a short SHA answered as written (GitHub answers the full
// one), a branch with a slash read as no tree, entries short of a field GitHub's description
// requires or with a size on a tree, `recursive` read as off for `0` (GitHub recurses for any
// value) or recursing without the trees on the way, an object that is no tree answered, and a
// 404 naming another page than GitHub's. gh-standin.ts's `treeOf` names the reads of 2026-11-21.
test("a tree is read from the run's own remote by any object GitHub reads, recursive for any value", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  const repo = "repos/example/widgets";
  const api = "https://api.github.com/repos/example/widgets";
  const read = (named: string) => JSON.parse(ghCall(dir, "api", `${repo}/git/trees/${named}`).out);
  const tree = git(remote, "rev-parse", `${second}^{tree}`);
  const blob = (file: string) => git(remote, "rev-parse", `${second}:${file}`);
  const whole = read(second);
  expect(heldTo("GET repos/{owner}/{repo}/git/trees/{sha+}", whole)).toEqual([
    "GET /repos/{owner}/{repo}/git/trees/{tree_sha}",
    [],
  ]);
  expect(whole).toEqual({
    sha: second,
    url: `${api}/git/trees/${second}`,
    tree: ["docs.md", "greet.ts"].map((file) => ({
      path: file,
      mode: "100644",
      type: "blob",
      sha: blob(file),
      size: file === "docs.md" ? 13 : 30,
      url: `${api}/git/blobs/${blob(file)}`,
    })),
    truncated: false,
  });
  expect([read(tree.slice(0, 8)).sha, read("legion/LWEVAL-1").sha]).toEqual([tree, first]);
  const world = worldDir("approved_world", "", { worldCase: "correct" });
  const paths = (query: string) =>
    ghCall(
      world,
      "api",
      `${repo}/git/trees/${builtHead(world)}${query}`,
      "--jq",
      '[.tree[] | select(.path | startswith(".github")) | "\\(.type) \\(.path) \\(has("size"))"]'
    ).out;
  expect(JSON.parse(paths(""))).toEqual(["tree .github false"]);
  for (const query of ["?recursive=1", "?recursive=0"])
    expect([query, JSON.parse(paths(query))]).toEqual([
      query,
      [
        "tree .github false",
        "tree .github/workflows false",
        "blob .github/workflows/greet.yml true",
      ],
    ]);
  for (const named of ["deadbeef".repeat(5), blob("docs.md")])
    expect([named, ghCall(dir, "api", `${repo}/git/trees/${named}`)]).toEqual([
      named,
      {
        code: 1,
        out: notFoundOf("https://docs.github.com/rest/git/trees#get-a-tree"),
        err: "gh: Not Found (HTTP 404)\n",
      },
    ]);
}, 60_000);

// Catches: a list of the tags that is not GitHub's "List repository tags": anything but an empty
// list for a remote that holds none, as no world's does; a tag of the remote missing; an
// annotated tag naming its tag object where GitHub names the commit; another order than GitHub's,
// newest version first (`v1.10` before `v1.9`); an item short of a field GitHub's description
// requires; and an annotated tag's ref read as its commit, where GitHub's refs name the tag
// object. gh-standin.ts's `tagList` names the reads of 2026-11-21.
test("the tags are the remote's, newest version first, each with the commit it names", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  const repo = "repos/example/widgets";
  expect(ghCall(dir, "api", `${repo}/tags`)).toEqual({ code: 0, out: "[]", err: "" });
  git(remote, "tag", "v1.0", first);
  git(remote, "tag", "v1.9", first);
  git(remote, "tag", "-a", "v1.10", "-m", "a release", second);
  const listed = JSON.parse(ghCall(dir, "api", `${repo}/tags`).out);
  expect(heldTo("GET repos/{owner}/{repo}/tags", listed)).toEqual([
    "GET /repos/{owner}/{repo}/tags",
    [],
  ]);
  expect(
    listed.map((tag: { name: string; commit: { sha: string } }) => [tag.name, tag.commit.sha])
  ).toEqual([
    ["v1.10", second],
    ["v1.9", first],
    ["v1.0", first],
  ]);
  expect(listed[0]).toMatchObject({
    zipball_url: "https://api.github.com/repos/example/widgets/zipball/refs/tags/v1.10",
    tarball_url: "https://api.github.com/repos/example/widgets/tarball/refs/tags/v1.10",
  });
  expect(
    JSON.parse(
      ghCall(dir, "api", `${repo}/git/ref/tags/v1.10`, "--jq", "[.object.type, .object.sha]").out
    )
  ).toEqual(["tag", git(remote, "rev-parse", "refs/tags/v1.10")]);
});

// Catches: a comparison GitHub does not read answered otherwise than GitHub answers it: two dots
// (`<base>..<head>`) and a ref the remote has not got each answered by no route, with the 404 body
// of an endpoint GitHub does not serve, where GitHub answers its 404 of a comparison, its body
// naming "Compare two commits": measured on 2026-11-21, `gh api -i
// repos/cybermonklabs/legion/compare/446a02fb..bbf69791` answered `HTTP/2.0 404 Not Found` (`Date:
// Sat, 21 Nov 2026 08:43:19 GMT`) where the same two commits written with three dots answered 200
// (08:43:20 GMT), and `compare/nosuchbranch-xyz...main` answered that 404 (08:43:22 GMT).
test("a comparison of two dots, or of a ref the remote has not got, is GitHub's 404 of a comparison", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const refused = {
    code: 1,
    out: notFoundOf("https://docs.github.com/rest/commits/commits#compare-two-commits"),
    err: "gh: Not Found (HTTP 404)\n",
  };
  const baseheads = [
    `${first}..${second}`,
    "legion/LWEVAL-1..legion/LWEVAL-1",
    "deadbeef...cafed00d",
  ];
  for (const basehead of [...baseheads, `${first}...deadbeef`])
    expect([basehead, ghCall(dir, "api", `repos/example/widgets/compare/${basehead}`)]).toEqual([
      basehead,
      refused,
    ]);
  expect(recordedCalls(dir).map((call) => call.handler)).toEqual(Array(4).fill("comparison"));
  expect(
    ghCall(dir, "api", `repos/example/widgets/compare/${first}...${second}`, "--jq", ".status").out
  ).toBe("ahead");
});

// Catches: a health that reads GitHub's own refusal of a read of the run's remote as the world
// missing an answer, which voids the run: the 422 of a commit the remote has not got, the 404 of
// a short SHA's git data, of a tree, of a ref, of a list from a ref the remote has not got and of
// a comparison of two dots, each GitHub's answer, and a `--jq` that fails over a commit the world
// answered. In INFRA-764's round 29 the world-health check voided run 94caf2e09844 on the
// reviewer's read of a commit its remote held (calls.jsonl:8). A call a route matched is judged by
// its route, and a comparison by health.ts's OWN_READ, which takes two dots as it takes three.
test("health reads GitHub's refusals of the remote's commits, trees, refs and comparisons as GitHub's", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const repo = "repos/example/widgets";
  const reads = [
    ["api", `${repo}/commits/${second}`, "--jq", ".sha"],
    ["api", `${repo}/commits/${"deadbeef".repeat(5)}`],
    ["api", `${repo}/commits/${second}`, "--jq", ".files[0] | error"],
    ["api", `${repo}/commits?sha=nosuchref`],
    ["api", `${repo}/git/commits/${second.slice(0, 8)}`],
    ["api", `${repo}/git/trees/${"deadbeef".repeat(5)}`],
    ["api", `${repo}/git/refs/heads/nosuch`],
    ["api", `${repo}/git/ref/heads/nosuch`],
    ["api", `${repo}/compare/${first}..${second}`],
  ];
  for (const by of ["reviewer", "implementer"]) for (const argv of reads) asked(dir, by, argv);
  expect(recordedCalls(dir).map((call) => [call.exit, call.handler])).toEqual(
    ["reviewer", "implementer"].flatMap(() => [
      [0, "commit"],
      [1, "commit"],
      [1, "commit"],
      [1, "commits"],
      [1, "git-commit"],
      [1, "git-tree"],
      [1, "git-ref"],
      [1, "git-ref"],
      [1, "comparison"],
    ])
  );
  for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
    expect([scenario, ghUndeclared({ dir, scenario })]).toEqual([scenario, undefined]);
});

// Catches: a health that reads a run whose push left the issue branch at another history as
// healthy, where GitHub keeps a test merge of a head that does not merge and the world answers
// none (class-search.json's residual, health.ts's rule 24); and one that calls a fix pushed over
// the branch, which holds the base's tip, unhealthy. The world's remote takes the force-push of
// such a head: measured on 2026-11-21 against a copy of run 94caf2e09844's remote (health.ts's
// `unmergedHead` names the command and its answer); here the push's line is written as the
// remote's post-receive hook writes one.
test("health names a push that leaves the issue branch without the base's tip, and passes a fix pushed over it", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const remote = path.join(dir, "remote.git");
  const log = path.join(dir, "pushes.log");
  const fix = pushFix(dir, "docs/cli.md", "a fix\n");
  expect(readFileSync(log, "utf8")).toContain(` refs/heads/legion/LWEVAL-1 `);
  expect(unmergedHead({ dir })).toBeUndefined();
  const orphan = git(remote, "commit-tree", `${fix}^{tree}`, "-m", "another history");
  appendFileSync(log, `2026-11-21T09:29:09.375Z refs/heads/legion/LWEVAL-1 ${fix} ${orphan}\n`);
  const pushed = readFileSync(log, "utf8").trimEnd().split("\n").length;
  expect(unmergedHead({ dir })).toBe(
    `unmerged head: pushes.log:${pushed} pushed ${orphan} to refs/heads/legion/LWEVAL-1, whose history does not hold main's tip, so it may not merge, and GitHub keeps the last test merge of such a head where the world answers none`
  );
}, 60_000);

// Catches: a comparison read off the path alone, whatever verb the call names. The comparison
// is GitHub's read of two refs, and a call that writes to that path is another call: GitHub
// serves no write there and the world models none, so it is GitHub's 404, and a stand-in that
// answered it with the two refs' diff would tell a caller a write it never made had succeeded.
// The verb is read here as the score reads it on the same call (score.ts's `methodOf`), so each
// spelling gh takes is that write: written against the flag, in lower case, and as a field with
// no method at all, which gh sends as a POST; and a PATCH and a PUT, the other two verbs the
// route every write fell to answered with `{id:6001}`, so a comparison or a success answered for
// any of them fails here too.
test("a POST to the compare path is no comparison", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const range = `repos/example/widgets/compare/${first}...${second}`;
  expect(answer(dir, ["api", range, "--jq", ".status"])).toBe("ahead");
  for (const verb of [
    ["--method", "POST"],
    ["--method=post"],
    ["-X", "PATCH"],
    ["-X", "PUT"],
    ["-XPOST"],
    ["-f", "per_page=100"],
  ])
    expect([verb, ghCall(dir, "api", ...verb, range)]).toEqual([
      verb,
      { code: 1, out: GITHUB_404, err: "gh: Not Found (HTTP 404)\n" },
    ]);
});

// Catches: a comparison whose files carry no `patch`, which is the field a reviewer reads the
// change out of. It was absent through the batch of 2026-11-10 — a reviewer that asked for the
// patch got `null` and one fell back to `jj file show`, working around the rig on the read step
// 3 of review-threads.md rests on. GitHub's `patch` is the file's hunks alone, with no
// `diff --git` or `index` header above them.
test("each file of a comparison carries the hunks of its own diff", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const patch = answer(dir, [
    "api",
    `repos/example/widgets/compare/${first}...${second}`,
    "--jq",
    ".files[0].patch",
  ]);
  expect(patch).toStartWith("@@ -0,0 +1 @@");
  expect(patch).toContain("+the text fix");
});

// Catches: a stand-in with no answer for the diff media type, which the API documents for this
// same comparison and which a reviewer asks for when it wants the change rather than the
// counts. The answer is the diff itself, never the JSON. Catches too a stand-in that knows one
// spelling of that type: GitHub answers `application/vnd.github.diff`, which is what its
// current documentation prints, as it answers the versioned `application/vnd.github.v3.diff`,
// so a reviewer asking in the shorter one would be answered the counts it did not ask for.
test("a comparison asked for as a diff is the unified diff, in either spelling of the type", () => {
  const { dir, first, second } = runDir({ fixture: true });
  for (const type of ["application/vnd.github.v3.diff", "application/vnd.github.diff"]) {
    const diff = answer(dir, [
      "api",
      "-H",
      `Accept: ${type}`,
      `repos/example/widgets/compare/${first}...${second}`,
    ]);
    expect(diff).toStartWith("diff --git a/docs.md b/docs.md");
    expect(diff).toContain("+the text fix");
  }
});

// Catches: a compare ref handed to `git` as it was written. A ref is argv, and `git` reads a
// leading `-` as an option wherever it falls: measured on 2026-11-11, `git -C <bare> diff
// --no-renames "--output=<path>...<sha>"` created the file `<path>...<sha>` while it parsed
// its options and only then failed for want of a work tree, so the ref wrote a file of the
// caller's choosing. The diff media type is the call that reaches `git diff` first. The
// stand-in refuses such a ref instead, and the call falls to the routes, whose 404 is GitHub's
// answer for a ref it has not got. Either side of the comparison is read that way: the base is
// the side that starts the argument `git` sees and so the side that writes the file, and the
// head is refused beside it rather than left to whatever `git` makes of a range ending in an
// option.
test("a compare ref that reads as an option is refused, not run, on either side", () => {
  const { dir, first } = runDir({ fixture: true });
  for (const [name, refs] of [
    ["base", (option: string) => `${option}...${first}`],
    ["head", (option: string) => `${first}...${option}`],
  ] as [string, (option: string) => string][]) {
    const written = path.join(dir, `ref-wrote-this-${name}`);
    const call = spawnChecked(
      [
        "bun",
        STANDIN,
        "gh",
        "api",
        "-H",
        "Accept: application/vnd.github.v3.diff",
        `repos/example/widgets/compare/${refs(`--output=${written}`)}`,
      ],
      { env: callEnv(dir) }
    );
    expect(call.exitCode).toBe(1);
    expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
    expect(readdirSync(dir).filter((entry) => entry.startsWith("ref-wrote-this"))).toEqual([]);
  }
});

// And the head half of that guard, through the call that reads the counts rather than the diff.
// Catches: the head left unchecked on the ground that only a base can be read as an option,
// which `git diff` makes true and `git rev-list` does not: the counts are read with `rev-list
// --count ${head}..${base}`, which puts the head first, and `git rev-list --count
// "--output=pwned..main"` exits 129 and creates the file `pwned..main` — measured on a scratch
// repository on 2026-11-12. So a compare of `main...--output=<path>` with no media type writes
// `<path>..main`, which is the file write the guard's own comment says it prevents. The call is
// refused before any `git` runs, and falls to the routes, whose 404 is GitHub's answer for a
// ref it has not got.
test("a compare head that reads as an option writes no file through the counts either", () => {
  const { dir } = runDir({ fixture: true });
  const written = path.join(dir, "counts-wrote-this");
  const call = spawnChecked(
    ["bun", STANDIN, "gh", "api", `repos/example/widgets/compare/main...--output=${written}`],
    { env: callEnv(dir) }
  );
  expect(call.exitCode).toBe(1);
  expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
  expect(readdirSync(dir).filter((entry) => entry.startsWith("counts-wrote-this"))).toEqual([]);
});

// Catches: a stand-in that answers a listing with the fixture's own object untouched. `gh pr
// list` answers a list of the objects `gh pr view` answers one of (rig.sh's fixture_write), and
// the worker skill has a role with no pull request number find it that way, so a role that
// listed the pull request after a fix push read the head the fixture froze — the very head the
// rule has it compare its push against — and a listing after a body edit read the body before
// it. Every rule over an answer's object is a rule over a listed one.
test("a listing's elements each carry the run's own head, the kept body and the chosen fields", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const listed = (fields: string) => answer(dir, ["pr", "list", "--json", fields, "--jq", "@json"]);
  expect(JSON.parse(listed("number,headRefOid"))).toEqual([{ number: 1001, headRefOid: first }]);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(JSON.parse(listed("headRefOid"))).toEqual([{ headRefOid: second }]);
  answer(dir, ["pr", "edit", "1001", "--body", "the edited body"]);
  expect(JSON.parse(listed("body"))).toEqual([{ body: "the edited body" }]);
});

/** The stderr and the exit the routes a POST falls to carry, as a world writes a refusal for the
 * call it wrote a route for (rig.sh's fixture_write gives the base-protection route GitHub's own
 * 404 and exit 1). The routes are chosen by the verbs they answer (`methods`), which is how
 * fixture_write names a POST route: selected by the text of their regex, no route of a world
 * ever got the refusal, and the row below could not fail. Each write a world models has its own
 * POST route, the submit's among them. */
function routeRefusal(dir: string, text: string): void {
  const file = path.join(dir, "fixtures.json");
  const fixtures = JSON.parse(readFileSync(file, "utf8")) as {
    routes: { match: string; methods?: string[]; stderr?: string; exit?: number }[];
  };
  const posts = fixtures.routes.filter((route) => route.methods?.includes("POST"));
  expect(posts.map((route) => route.match)).toEqual([
    "^gh api .*pulls/1001/reviews([ ?]|$)",
    "^gh api .*pulls/1001/reviews/[0-9]+/events([ ?]|$)",
    "^gh api .*pulls/1001/comments([ ?]|$)",
    "^gh api .*pulls/1001/comments/[0-9]+/replies([ ?]|$)",
    "^gh api .*issues/1001/comments([ ?]|$)",
  ]);
  for (const route of posts) Object.assign(route, { stderr: text, exit: 1 });
  writeFileSync(file, JSON.stringify(fixtures));
}

// Catches: a stand-in that prints the matched route's stderr beside its own answer to a submit,
// or exits as that route does. The submit of a pending review is answered from what this run
// has already done and not from a route, so the text and the status of whatever route the call
// fell to — the generic POST, written for the calls that create things — belong to another
// call: a reviewer that submitted its pending review would read a refusal on stderr beside the
// review object, or an exit of 1, and read the approval it had just made as a call that failed.
test("a submit answered from the run's own record prints no route's stderr and exits 0", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  routeRefusal(dir, "gh: Unprocessable Entity (HTTP 422)\nthe route's own refusal\n");
  const submit = ghCall(
    dir,
    "api",
    "--method",
    "POST",
    `${REVIEWS}/6001/events`,
    "-f",
    "event=APPROVE"
  );
  expect(submit.code).toBe(0);
  expect(submit.err).toBe("");
  expect(JSON.parse(submit.out)).toMatchObject({ id: 6001, commit_id: first, state: "APPROVED" });
});

// Catches: a stand-in that reads a create's body out of a file alone. `gh api --input -` takes
// the body on stdin, which is how an agent sends one it built in the same command, often as the
// several lines a formatted JSON object is written over; the stand-in reads it there, records
// what it read (`stdin`) and reads the create through the score's own `sentBody`. One blind to
// stdin leaves that create no pending review, and the call that submits it is refused 422 — the
// round's own approval answered as a submission GitHub never took.
test("a create whose body is on stdin as JSON lines leaves a pending review", () => {
  const { dir, first } = runDir({ fixture: true });
  const created = spawnChecked(
    ["bun", STANDIN, "gh", "api", "--method", "POST", REVIEWS, "--input", "-"],
    {
      env: callEnv(dir),
      stdin: new TextEncoder().encode(
        `${JSON.stringify({ commit_id: first, body: "two findings" }, null, 2)}\n`
      ),
    }
  );
  expect(created.exitCode).toBe(0);
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toMatchObject({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// Catches: a create route matched on a path a call merely starts with. The create of a review
// is `…/pulls/<n>/reviews` and nothing below it: a POST of `…/pulls/<n>/reviews/<id>/comments`
// is no write GitHub serves (it lists that review's comments, and takes none), so it is GitHub's
// 404, creates no review and leaves none pending. A stand-in that read it as a create would
// answer the next submit with a review object at the head that comment was posted at — the
// approval at a head nobody approved that this endpoint exists to stop.
test("a POST under a review's own path is no create of a pending review", () => {
  const { dir } = runDir({ fixture: true });
  const comment = apiPost(dir, `${REVIEWS}/5002/comments`, "-f", "body=the sentence still stands");
  expect(comment.exitCode).toBe(1);
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(1);
  expect(submit.stderr.toString()).toContain("Can not submit a review that is not pending.");
  expect(submit.stdout.toString()).toBe("");
});

// Catches: a comparison whose files all carry the whole range's diff as their `patch`. GitHub's
// `patch` is that file's own hunks, and the reviewer reads the change file by file: a push of
// two files whose every entry held both files' hunks would have it read a change to a file it
// is not looking at, and the one-file comparison above cannot tell the two readings apart.
test("each file of a comparison carries its own hunks and no other file's", () => {
  const { dir, first } = runDir({ fixture: true });
  const work = path.join(dir, "two-files");
  git(dir, "clone", "-q", path.join(dir, "remote.git"), work);
  writeFileSync(path.join(work, "docs.md"), "the text fix\n");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 2;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "docs: a fix in each file");
  const both = git(work, "rev-parse", "HEAD");
  git(work, "push", "-q", "origin", `HEAD:refs/heads/legion/LWEVAL-1`);
  const patches = JSON.parse(
    answer(dir, [
      "api",
      `repos/example/widgets/compare/${first}...${both}`,
      "--jq",
      "[.files[] | {filename, patch}] | @json",
    ])
  ) as { filename: string; patch: string }[];
  expect(patches.map((file) => file.filename)).toEqual(["docs.md", "greet.ts"]);
  expect(patches[0]?.patch).toStartWith("@@ -0,0 +1 @@");
  expect(patches[0]?.patch).toContain("+the text fix");
  expect(patches[0]?.patch).not.toContain("greet");
  expect(patches[1]?.patch).toContain("+export const greet = () => 2;");
  expect(patches[1]?.patch).not.toContain("the text fix");
});

// Catches: an answer whose `headRefOid` is the run's own head while the `head.sha` beside it
// is still the fixture's. `gh api repos/<o>/<r>/pulls/<n>` answers the REST shape, where the
// head is `head.sha`, and rig.sh's worlds carry both fields on the one object: a pushing role
// reading `.head.sha` after its push — which the worker skill has every one of them do — would
// read the commit before its own and stop, as two runs of 2026-11-06 did on `headRefOid`.
test("a pull request read through the api carries the run's own head in head.sha too", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const sha = () => answer(dir, ["api", "repos/example/widgets/pulls/1001", "--jq", ".head.sha"]);
  expect(sha()).toBe(first);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(sha()).toBe(second);
});

/** The top-level values of rig.sh a world's functions read: the repository's own developer, the
 * two Apps the roles act as, the login the run's token acts as, the round's two findings and the
 * issue workspace's remote helper. */
const WORLD_VALUES = [
  "world_author_name",
  "world_author_email",
  "world_implement_app",
  "world_implement_app_id",
  "world_review_app",
  "world_review_app_id",
  "world_login",
  "text_finding_doc_line",
  "text_finding_trigger",
  "cli_example_line",
  "world_remote_vcs",
];
/** WORLD (with GUARD) built for real under a run directory of its own by rig.sh's own world
 * functions, as rig.test.ts builds its worlds: `fixture_cli` read whole, here-documents and all
 * (test-programs.ts's `scriptDefinition`), so the CLI and its tests are the rig's own and a
 * review comment on them stands on a line of the pull request's diff, as the world's seeded one
 * does; `fixture_workspace`, which makes the issue workspace with jj and which no gh answer
 * reads, stubbed unless `workspace` asks for it; and this checkout's gh selectors written in as
 * worker_boot writes a label's (`writeGhSelectors`). `worldCase` is the case the world's function
 * builds (worker_fixture passes it): for approved_world the threads-closed case (rig.sh's own
 * refusal names the world that is given none), `correct`, the reviewer's commit at the head with
 * one thread left on the workflow file, `accept`, its commit with none, or `code-defect`, with the
 * guard `dropped`; for review_world `last-push`, text-only's world on a base whose rules require
 * the last push approved by another, or none. `env` is the operator's own environment the build
 * runs in, over this process's. */
function worldDir(
  world: string,
  guard = "",
  {
    workspace = false,
    env = {},
    worldCase = "",
  }: { workspace?: boolean; env?: Record<string, string>; worldCase?: string } = {}
): string {
  const dir = mkdtempSync(path.join(tmpdir(), `gh-standin-${world}-`));
  made.push(dir);
  const steps = [
    "fixture_open",
    "fixture_git",
    "fixture_docs_before",
    "fixture_first_commit",
    "fixture_round_files",
    "fixture_head_commit",
    "fixture_implement",
    "fixture_test_handoff",
    "fixture_checks",
    "fixture_body",
    "fixture_write",
    "concluded_checks",
    "base_ruleset_routes",
    "resolution_protection_routes",
    "round_world",
    "fixture_reviewer_commit",
    "role_identity",
    "role_env",
    ...(workspace
      ? ["fixture_workspace", "workspace_form", "world_remote", "world_remote_app"]
      : []),
    world,
  ];
  const built = spawnChecked(
    [
      "bash",
      "-c",
      [
        fn("fail"),
        ...steps.map(fn),
        scriptDefinition(readFileSync(RIG, "utf8"), "fixture_cli"),
        ...(workspace ? [] : ["fixture_workspace() { :; }"]),
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        ...WORLD_VALUES.map(assignment),
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      "-",
      dir,
      guard,
      worldCase,
    ],
    {
      env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1", ...env },
    }
  );
  if (built.exitCode !== 0) throw new Error(`${world}: ${built.stderr.toString()}`);
  writeGhSelectors(dir);
  return dir;
}
/** The pull request's head the world under `dir` was built at, read by the one schema of its
 * world.json (gh-call.ts's `readWorld`). */
function builtHead(dir: string): string {
  const read = readWorld(dir);
  if (!("world" in read)) throw new Error(read.why);
  return read.world.head;
}
/** Pushes a commit that writes `text` into `file` over the issue branch's tip to the run's own
 * remote, as a role pushes its fix, so the remote's own hook logs the push; the commit. With `app`
 * the commit is authored and committed as that App's bot (`botOf`), as a role's pane commits
 * (rig.sh's role_env), whatever identity this process's environment carries, so the world links
 * the pushed head to that App's account (rest-objects.ts's `pusherOf`). */
function pushFix(dir: string, file: string, text: string, app?: "implement" | "review"): string {
  const work = path.join(dir, "fix-work");
  git(dir, "clone", "-q", "-b", "legion/LWEVAL-1", path.join(dir, "remote.git"), work);
  writeFileSync(path.join(work, file), text);
  git(work, "add", "-A");
  if (app === undefined) git(work, "commit", "-qm", "docs: the text fix");
  else {
    const [, name = "", email = ""] = /^(.*) <(.*)>$/.exec(botOf(app)) ?? [];
    const committed = spawnChecked(["git", "-C", work, "commit", "-qm", "docs: the text fix"], {
      env: {
        ...process.env,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_AUTHOR_NAME: name,
        GIT_AUTHOR_EMAIL: email,
        GIT_COMMITTER_NAME: name,
        GIT_COMMITTER_EMAIL: email,
      },
    });
    if (committed.exitCode !== 0) throw new Error(`git commit: ${committed.stderr.toString()}`);
  }
  git(work, "push", "-q", "origin", "legion/LWEVAL-1");
  return git(work, "rev-parse", "HEAD");
}
/** `gh api graphql` with QUERY and the three variables a role sends with the threads query
 * (`-F o=example -F r=widgets -F n=1001`), then REST. */
const threadsQuery = (query: string, ...rest: string[]) => [
  "api",
  "graphql",
  "-f",
  `query=${query}`,
  "-F",
  "o=example",
  "-F",
  "r=widgets",
  "-F",
  "n=1001",
  ...rest,
];

// GitHub keeps what a run did: a thread a review's inline comment opens is the pull request's
// from then on, a resolution GitHub answered `true` reads back resolved, and a reply is its
// thread's last comment. Measured in INFRA-764 round 24 (dispatch://INFRA-764/artifact/
// infra-764-r24-names-scan-partial-txt): reviewers that resolved a thread or opened their own
// read the listing back as the fixture froze it — "the mock's graphql `reviewThreads` only
// surfaces the seeded style-bot thread and doesn't persist my reply/resolution or new threads"
// (reviewer-text-findings-text-only-head-1), "doesn't reflect my new threads or the resolve
// (which returned `true`)" (-head-4) — and named the world a mock in 29 of 36 runs. Every call
// is the recorded shape: the create of reviewer-text-findings-text-only-head-1/calls.jsonl:21
// and its listing at :22, the resolve at :29, and threads-closed-text-fixes-head-1/calls.jsonl's
// listing at :33, reply at :9 and comment read at :3. Catches: the world's threads answered as
// the fixture froze them, a resolve answered `true` and read back unresolved, a reply answered
// with an id and found under no thread, and a resolve of a thread the world has not got
// answered `true`, where GitHub answers NOT_FOUND.
test("GitHub keeps the threads: one a review opens is listed, a resolve reads back resolved, a reply is its thread's last comment", () => {
  const dir = worldDir("review_world");
  const head = builtHead(dir);
  const doc = 'The doc says the CLI "prints the name exactly as it was given, spaces and all,"';
  const trigger = 'The comment says the workflow runs "on every push to main"';
  const input = path.join(dir, "body.json");
  writeFileSync(
    input,
    JSON.stringify({
      commit_id: head,
      event: "APPROVE",
      body: `Reviewed PR #1001 at head ${head}.`,
      comments: [
        {
          path: "docs/cli.md",
          line: 5,
          side: "RIGHT",
          start_line: null,
          position: null,
          body: doc,
        },
        {
          path: ".github/workflows/greet.yml",
          line: 1,
          side: "RIGHT",
          start_line: null,
          position: null,
          body: trigger,
        },
      ],
    })
  );
  const create = ghCall(
    dir,
    ...["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews", "--input", input],
    ...["--jq", "{id:.id,state:.state,commit_id:.commit_id}"]
  );
  expect([create.code, JSON.parse(create.out)]).toEqual([
    0,
    { id: 6001, state: "APPROVED", commit_id: head },
  ]);
  const listing = ghCall(
    dir,
    ...threadsQuery(
      "query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved path line comments(first:1){nodes{author{login} body}}}}}}}"
    ).toSpliced(2, 0, "--paginate")
  );
  const node = (id: string, file: string, line: number, login: string, body: unknown) => ({
    id,
    isResolved: false,
    path: file,
    line,
    comments: { nodes: [{ author: { login }, body }] },
  });
  expect([listing.code, JSON.parse(listing.out)]).toEqual([
    0,
    {
      data: {
        repository: {
          pullRequest: {
            reviewThreads: {
              pageInfo: { hasNextPage: false, endCursor: expect.any(String) },
              nodes: [
                node(
                  "PRRT_kwDOrig0001",
                  "greet.test.ts",
                  4,
                  "style-bot",
                  expect.stringContaining("greets by name")
                ),
                node("PRRT_kwDOAAAAAc4AAD9001", "docs/cli.md", 5, WORLD_AUTHOR, doc),
                node(
                  "PRRT_kwDOAAAAAc4AAD9002",
                  ".github/workflows/greet.yml",
                  1,
                  WORLD_AUTHOR,
                  trigger
                ),
              ],
            },
          },
        },
      },
    },
  ]);
  const resolve = (id: string) =>
    ghCall(
      dir,
      "api",
      "graphql",
      "-f",
      "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
      "-F",
      `id=${id}`
    );
  expect(resolve("PRRT_kwDOAAAAAc4AAD9001")).toEqual({
    code: 0,
    out: JSON.stringify({ data: { resolveReviewThread: { thread: { isResolved: true } } } }),
    err: "",
  });
  const states = () =>
    ghCall(
      dir,
      ...threadsQuery(
        "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved}}}}}",
        "--jq",
        ".data.repository.pullRequest.reviewThreads.nodes[] | [.id, .isResolved] | @tsv"
      )
    ).out;
  expect(states()).toBe(
    "PRRT_kwDOrig0001\tfalse\nPRRT_kwDOAAAAAc4AAD9001\ttrue\nPRRT_kwDOAAAAAc4AAD9002\tfalse"
  );
  const fixed = `Fixed in ${head}: the workflow comment now names only the pull_request trigger the file carries.`;
  const reply = ghCall(
    dir,
    "api",
    "graphql",
    "-f",
    "query=mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id}}}",
    "-F",
    "t=PRRT_kwDOAAAAAc4AAD9002",
    "-F",
    `b=${fixed}`,
    "--jq",
    ".data.addPullRequestReviewThreadReply.comment.id"
  );
  expect(reply).toEqual({ code: 0, out: "PRRC_kwDOAAAAAc4AAC9003", err: "" });
  const last = ghCall(
    dir,
    ...threadsQuery(
      "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}",
      "--jq",
      ".data.repository.pullRequest.reviewThreads.nodes[2] | [.id, .comments.nodes[0].author.login, .comments.nodes[0].body] | @tsv"
    )
  );
  expect(last.out).toBe(`PRRT_kwDOAAAAAc4AAD9002\t${WORLD_AUTHOR}\t${fixed}`);
  // A thread the world has not got is GitHub's NOT_FOUND, and resolves nothing.
  expect(resolve("PRRT_kwDOfix001")).toEqual({
    code: 1,
    out: JSON.stringify({
      data: { resolveReviewThread: null },
      errors: [
        {
          type: "NOT_FOUND",
          path: ["resolveReviewThread"],
          message: "Could not resolve to a node with the global id of 'PRRT_kwDOfix001'.",
        },
      ],
    }),
    err: "gh: Could not resolve to a node with the global id of 'PRRT_kwDOfix001'.\n",
  });
  expect(states()).toBe(
    "PRRT_kwDOrig0001\tfalse\nPRRT_kwDOAAAAAc4AAD9001\ttrue\nPRRT_kwDOAAAAAc4AAD9002\tfalse"
  );
}, 60_000);

// A review thread GitHub holds is made of review comments, each with the `databaseId` that is its
// id in the REST API, and the REST reply route answers a reply to one and files it in that
// thread. Measured in INFRA-764 round 24: "The thread comment has no databaseId, path, or line,
// so it looks like a synthetic seeded thread rather than a real review comment, which means the
// REST reply route won't work" (reviewer-text-findings-defect-base-1), and "REST shows no
// comments (synthetic rig thread)" (-defect-base-2). The REST listing is the recorded
// threads-closed-text-fixes-base-1/calls.jsonl:44 and reviewer-text-findings-text-only-head-1/
// calls.jsonl:26; the threads read is reviewer-text-findings-text-only-head-1/calls.jsonl:22's
// with the fields that sentence names; the reply is GitHub's REST form of it, which no recorded
// run could make for want of the id. Catches: a seeded thread whose comment carries no
// databaseId, a REST listing empty of the world's own thread, a REST reply answered with an id
// of no comment and filed in no thread, and a reply to a comment GitHub has not got answered 200.
test("a thread's comment carries its databaseId and its REST comment, and a REST reply lands in its thread", () => {
  const dir = worldDir("review_world");
  const head = builtHead(dir);
  const comments = () =>
    ghCall(
      dir,
      "api",
      "repos/example/widgets/pulls/1001/comments",
      "--jq",
      ".[] | [.id, .path, .line, .user.login, .in_reply_to_id] | @tsv"
    );
  expect(comments()).toEqual({ code: 0, out: "7001\tgreet.test.ts\t4\tstyle-bot[bot]", err: "" });
  const ids = ghCall(
    dir,
    ...threadsQuery(
      "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved path line comments(first:20){nodes{databaseId path line author{login} body}}}}}}}",
      "--jq",
      ".data.repository.pullRequest.reviewThreads.nodes[] | .comments.nodes[] | [.databaseId, .path, .line] | @tsv"
    )
  );
  expect(ids.out).toBe("7001\tgreet.test.ts\t4");
  const answer = "Not a defect: `greets by name` and `trims the name` name different cases.";
  const reply = ghCall(
    dir,
    ...["api", "--method", "POST", "repos/example/widgets/pulls/1001/comments/7001/replies"],
    ...["-f", `body=${answer}`, "--jq", "[.id, .in_reply_to_id, .path, .line, .commit_id, .body]"]
  );
  expect([reply.code, JSON.parse(reply.out)]).toEqual([
    0,
    [9001, 7001, "greet.test.ts", 4, head, answer],
  ]);
  expect(comments().out).toBe(
    `7001\tgreet.test.ts\t4\tstyle-bot[bot]\t\n9001\tgreet.test.ts\t4\t${WORLD_LOGIN}\t7001`
  );
  const thread = ghCall(
    dir,
    ...threadsQuery(
      "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id comments(last:1){totalCount nodes{databaseId author{login} body}}}}}}}",
      "--jq",
      ".data.repository.pullRequest.reviewThreads.nodes[0].comments | [.totalCount, .nodes[0].databaseId, .nodes[0].body] | @tsv"
    )
  );
  expect(thread.out).toBe(`2\t9001\t${answer}`);
  // The review the bot's comment belongs to is the pull request's, as GitHub lists it.
  expect(
    ghCall(dir, "api", "repos/example/widgets/pulls/1001/reviews/4001/comments", "--jq", ".[].id")
      .out
  ).toBe("7001");
  // A comment GitHub has not got takes no reply: 404, and nothing is filed.
  const nowhere = ghCall(
    dir,
    ...["api", "--method", "POST", "repos/example/widgets/pulls/1001/comments/7/replies"],
    ...["-f", "body=fixed"]
  );
  expect(nowhere).toEqual({
    code: 1,
    out: notFoundOf(
      "https://docs.github.com/rest/pulls/comments#create-a-reply-for-a-review-comment"
    ),
    err: "gh: Not Found (HTTP 404)\n",
  });
  expect(comments().out.split("\n")).toHaveLength(2);
}, 60_000);

// GitHub's "Update a review for a pull request" (`PUT /repos/{owner}/{repo}/pulls/{pull_number}/
// reviews/{review_id}`, `body` required) changes the review's body and answers the review itself:
// the same id, state and commit. A write to an endpoint GitHub does not serve is its 404. Measured
// in INFRA-764 round 25 (runs/1e2a0aeced4b, threads-closed-text-fixes-head-4, calls.jsonl:23): the
// reviewer's `gh api --method PUT repos/fernbrook/widgets/pulls/1001/reviews/5002 --input u.json`
// with a new body was answered `{"id":6001,"state":null,"commit_id":null}` with exit 0 by the
// catch-all write route, the review kept its old body, and the agent wrote "the PUT returned id
// 6001 but doesn't appear in the list, which suggests it didn't actually change 5002's body in
// this harness". Catches: a review update answered with an id of no review, a body that does not
// change, a review GitHub has not got answered 200, and any write the world does not model
// answered as a success rather than refused.
test("a review update changes that review's body and answers it, and a write GitHub does not serve is its 404", () => {
  const dir = worldDir("review_world");
  const review = () =>
    ghCall(
      dir,
      "api",
      "repos/example/widgets/pulls/1001/reviews/4001",
      "--jq",
      "[.id, .state, .commit_id, .body]"
    );
  const before = review();
  expect(before.code).toBe(0);
  const [id, state, commit] = JSON.parse(before.out) as [number, string, string, string];
  expect(id).toBe(4001);
  const body = "Approving the fix head. Both threads are resolved.";
  const updated = ghCall(
    dir,
    ...["api", "--method", "PUT", "repos/example/widgets/pulls/1001/reviews/4001"],
    ...["-f", `body=${body}`, "--jq", "[.id, .state, .commit_id, .body]"]
  );
  expect([updated.code, JSON.parse(updated.out || "null")]).toEqual([
    0,
    [4001, state, commit, body],
  ]);
  expect(JSON.parse(review().out)).toEqual([4001, state, commit, body]);
  // A review GitHub has not got takes no update.
  const missing = ghCall(
    dir,
    ...["api", "--method", "PUT", "repos/example/widgets/pulls/1001/reviews/4999"],
    ...["-f", "body=x"]
  );
  expect([missing.code, missing.err]).toEqual([1, "gh: Not Found (HTTP 404)\n"]);
  // A write to an endpoint GitHub does not serve is refused, in each verb a write is made with.
  for (const method of ["POST", "PATCH", "PUT"]) {
    const unserved = ghCall(
      dir,
      ...["api", "--method", method, "repos/example/widgets/pulls/1001/no-such-endpoint"],
      ...["-f", "body=x"]
    );
    expect([method, unserved.code, unserved.err]).toEqual([
      method,
      1,
      "gh: Not Found (HTTP 404)\n",
    ]);
  }
}, 60_000);

// The update of a review the run made is the same write as the update of one the world held
// (GitHub's "Update a review for a pull request"): the body changes at every later read of that
// review, and the answer is the review in GitHub's shape. Measured in INFRA-764 round 25
// (runs/1e2a0aeced4b, threads-closed-text-fixes-head-4, calls.jsonl:23): the reviewer updated
// the approval it had just made, 5002, whose body read back unchanged. Catches: an update kept
// for the world's reviews alone, so the review the run made reads its old body back; an answer
// short of GitHub's fields (node id, links, author association); an empty body answered 200;
// and a record that does not say which route answered the update, which every later read of the
// review replays.
test("a review update of a review the run made changes its body at every read, answered in GitHub's shape", () => {
  const { dir, first } = runDir({ fixture: true });
  expect(
    apiPost(dir, REVIEWS, "-f", `commit_id=${first}`, "-f", "event=APPROVE", "-f", "body=ok")
      .exitCode
  ).toBe(0);
  const body = "Approved at the fix head; both threads are resolved.";
  const updated = ghCall(dir, "api", "--method", "PUT", `${REVIEWS}/6001`, "-f", `body=${body}`);
  expect(updated.code).toBe(0);
  const html = "https://example.invalid/pull/1001#pullrequestreview-6001";
  const pull = "https://api.github.com/repos/example/widgets/pulls/1001";
  expect(JSON.parse(updated.out)).toEqual({
    id: 6001,
    node_id: "PRR_kwDOAAAAAc4AAE6001",
    user: expect.objectContaining({ login: WORLD_LOGIN, type: "Bot" }),
    body,
    state: "APPROVED",
    html_url: html,
    pull_request_url: pull,
    _links: { html: { href: html }, pull_request: { href: pull } },
    submitted_at: expect.any(String),
    commit_id: first,
    author_association: "NONE",
    is_copilot_code_review: false,
    copilot_code_review_effort: null,
  });
  expect(ghCall(dir, "api", `${REVIEWS}/6001`, "--jq", ".body").out).toBe(body);
  expect(ghCall(dir, "api", REVIEWS, "--jq", ".[].body").out).toBe(body);
  const empty = ghCall(dir, "api", "--method", "PUT", `${REVIEWS}/6001`, "-f", "body=");
  expect(empty).toEqual({
    code: 1,
    out: "",
    err: "gh: Unprocessable Entity (HTTP 422)\nA review is updated with a body.\n",
  });
  expect(ghCall(dir, "api", `${REVIEWS}/6001`, "--jq", ".body").out).toBe(body);
  expect(recordedCalls(dir).map((call) => [call.handler, call.exit])).toEqual([
    ["review create", 0],
    ["review-update", 0],
    ["review read", 0],
    ["reviews", 0],
    ["review-update", 1],
    ["review read", 0],
  ]);
}, 60_000);

// GitHub files a REST reply to a reply in that reply's own thread, and names the thread's first
// comment as the one it replies to. Measured on 2026-11-17 at 17:28Z on cybermonklabs/legion#2827:
// `POST …/pulls/2827/comments/4505026306/replies`, 4505026306 itself a reply to 4504802154,
// answered exit 0 with a comment whose `in_reply_to_id` was 4504802154. Catches: a world that
// refuses a reply to a reply, a refusal production does not give (the docs' "Replies to replies
// are not supported" is none); one that files it with `in_reply_to_id` naming the reply; and one
// whose GraphQL `replyTo` names anything but the thread's first comment.
test("a REST reply to a reply is filed in its thread, replying to the thread's first comment", () => {
  const dir = worldDir("review_world");
  const reply = (to: number) =>
    ghCall(
      dir,
      ...["api", "--method", "POST", `repos/example/widgets/pulls/1001/comments/${to}/replies`],
      ...["-f", "body=Not a defect: the two tests name different cases."],
      ...["--jq", "[.id, .in_reply_to_id] | @csv"]
    );
  expect(reply(7001)).toEqual({ code: 0, out: "9001,7001", err: "" });
  expect(reply(9001)).toEqual({ code: 0, out: "9002,7001", err: "" });
  expect(
    ghCall(
      dir,
      ...["api", "repos/example/widgets/pulls/1001/comments"],
      ...["--jq", "[.[] | [.id, .in_reply_to_id]] | @json"]
    ).out
  ).toBe("[[7001,null],[9001,7001],[9002,7001]]");
  const replies = ghCall(
    dir,
    ...threadsQuery(
      "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{comments(first:20){nodes{databaseId replyTo{databaseId}}}}}}}}",
      "--jq",
      "[.data.repository.pullRequest.reviewThreads.nodes[0].comments.nodes[] | [.databaseId, .replyTo.databaseId]] | @json"
    )
  );
  expect(replies.out).toBe("[[7001,null],[9001,7001],[9002,7001]]");
}, 60_000);

// A write the world does not model is refused as GitHub refuses an endpoint it does not serve,
// changes nothing, and is recorded as answered by nothing (`handler` `unrouted`), which `rig.sh
// health` names (its unmodelled-write rule). Catches each default that answered such a write a
// success: the route `gh pr (edit|comment|review|ready)`, which answered any of those on any pull
// request with any flags and a comment URL of no comment; read routes, which took DELETE and,
// with the catch-all gone, POST; the PATCH route, which took a PATCH of the state as an edit of
// the body; a write route reading another repository's pull request of the same number; a
// mutation the world's graph holds no answer for; and `gh pr merge`, `close`, `create` and
// `ready`, which the world never modelled.
test("every write the world does not model is refused 404, changes nothing, and is recorded unrouted", () => {
  const dir = worldDir("review_world");
  const prBody = () => ghCall(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out;
  const before = prBody();
  const unmodelled: [string[], string][] = [
    [["pr", "comment", "999", "--body", "a note"], ""],
    [["pr", "comment", "1001", "--edit-last", "--body", "a note"], ""],
    [["pr", "review", "1001", "--approve"], ""],
    [["pr", "review", "1001", "--comment", "--body", "a note"], ""],
    [["pr", "ready", "1001"], ""],
    [["pr", "merge", "1001", "--squash"], ""],
    [["pr", "close", "1001"], ""],
    [["pr", "create", "--title", "t", "--body", "b"], ""],
    [["pr", "edit", "1001", "--title", "a new title"], ""],
    [["pr", "edit", "1001", "--body", "a new body", "--add-label", "bug"], ""],
    [["api", "-X", "DELETE", `${REVIEWS}/4001`], GITHUB_404],
    [["api", "-X", "POST", `${REVIEWS}/4001`, "-f", "body=x"], GITHUB_404],
    [["api", "-X", "PATCH", "repos/example/widgets/pulls/1001", "-f", "state=closed"], GITHUB_404],
    [
      ["api", "-X", "POST", "repos/example/other/pulls/1001/reviews", "-f", "event=APPROVE"],
      GITHUB_404,
    ],
    [
      [
        "api",
        "-X",
        "POST",
        "repos/example/widgets/git/refs",
        "-f",
        "ref=refs/heads/x",
        "-f",
        "sha=0",
      ],
      GITHUB_404,
    ],
    [
      [
        "api",
        "graphql",
        "-f",
        'query=mutation{minimizeComment(input:{subjectId:"PRRC_kwDOAAAAAc4AAC7001",classifier:OUTDATED}){minimizedComment{isMinimized}}}',
      ],
      GITHUB_404,
    ],
  ];
  for (const [argv, out] of unmodelled)
    expect([argv, ghCall(dir, ...argv)]).toEqual([
      argv,
      { code: 1, out, err: "gh: Not Found (HTTP 404)\n" },
    ]);
  expect(prBody()).toBe(before);
  expect(ghCall(dir, "api", `${REVIEWS}/4001`, "--jq", ".id").out).toBe("4001");
  expect(
    recordedCalls(dir)
      .filter((call) => call.exit === 1)
      .map((call) => [call.route, call.handler])
  ).toEqual(unmodelled.map(() => [null, "unrouted"]));
}, 60_000);

// GitHub's GraphQL endpoint runs the `query` of the body it is posted, whichever path gh posts it
// by: gh reads `gh api graphql` as a GraphQL call of its own, and posts `gh api /graphql` and `gh
// api 'graphql?…'` as it posts any path, each field a key of the body, which the endpoint reads
// no variable from (gh-threads.ts's `graphqlOf`), printing GitHub's answer, errors and all, with
// exit 0. Measured on 2026-11-18 from an INFRA issue pod (implementer-reads.json): both spellings
// answered the viewer as `gh api graphql` does, and `gh api /graphql -f query='query($n:Int!){…}'
// -F n=2827` was answered GitHub's refusal of the variable it was not sent, exit 0. Catches: a
// mutation sent the second way answered empty and exit 0 whatever it named, the graph never read
// (INFRA-764's class search of writes, 2026-11-18); a field of that call read as a variable; its
// errors read as gh's failure; and a mutation the world does not model taken that way.
test("a mutation sent to /graphql or graphql?… is answered as GitHub's GraphQL endpoint answers it", () => {
  const dir = worldDir("review_world");
  const thread = "PRRT_kwDOrig0001";
  const asVariable =
    "mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}";
  expect(ghCall(dir, "api", "/graphql", "-f", `query=${asVariable}`, "-f", `id=${thread}`)).toEqual(
    {
      code: 0,
      out: JSON.stringify({
        errors: [
          {
            extensions: {
              value: null,
              problems: [{ path: [], explanation: "Expected value to not be null" }],
            },
            locations: [{ line: 1, column: 10 }],
            message: "Variable $id of type ID! was provided invalid value",
          },
        ],
      }),
      err: "",
    }
  );
  const resolve = `mutation{resolveReviewThread(input:{threadId:"${thread}"}){thread{id isResolved}}}`;
  expect(ghCall(dir, "api", "/graphql", "-f", `query=${resolve}`)).toEqual({
    code: 0,
    out: JSON.stringify({
      data: { resolveReviewThread: { thread: { id: thread, isResolved: true } } },
    }),
    err: "",
  });
  const reply = `mutation{addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:"${thread}",body:"fixed"}){comment{body}}}`;
  expect(ghCall(dir, "api", "graphql?x=1", "-f", `query=${reply}`)).toEqual({
    code: 0,
    out: JSON.stringify({
      data: { addPullRequestReviewThreadReply: { comment: { body: "fixed" } } },
    }),
    err: "",
  });
  const minimize =
    'mutation{minimizeComment(input:{subjectId:"PRRC_kwDOAAAAAc4AAC7001",classifier:OUTDATED}){minimizedComment{isMinimized}}}';
  expect(ghCall(dir, "api", "/graphql", "-f", `query=${minimize}`)).toEqual({
    code: 1,
    out: GITHUB_404,
    err: "gh: Not Found (HTTP 404)\n",
  });
  const threads =
    'query=query{repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewThreads(first:10){nodes{id isResolved comments(last:1){nodes{body}}}}}}}';
  const listed = ghCall(
    dir,
    ...["api", "graphql", "-f", threads, "--jq"],
    "[.data.repository.pullRequest.reviewThreads.nodes[] | [.id, .isResolved, .comments.nodes[0].body]] | @json"
  );
  expect(listed.out).toBe(JSON.stringify([[thread, true, "fixed"]]));
}, 60_000);

// GitHub answers a comment on the pull request's conversation with the comment it made, and `gh
// pr comment` prints that comment's URL; the world keeps each (gh-standin.ts's `conversation`)
// and numbers them from 8001 in the order the run made them, as every read of the conversation
// lists them. The worker skill posts a conversation comment with `gh pr comment`
// (skills/legion-worker/SKILL.md). Catches: a comment answered with a URL of no comment — the
// shared route printed `#issuecomment-1` whatever the run had posted — or the REST create
// answered `{id:6001}`, an id no read of the conversation lists; and an empty comment answered
// 200, which GitHub refuses.
test("gh pr comment and the REST create of a conversation comment answer the comment the conversation keeps", () => {
  const { dir } = runDir({ fixture: true });
  expect(ghCall(dir, "pr", "comment", "1001", "--body", "a note")).toEqual({
    code: 0,
    out: "https://example.invalid/pull/1001#issuecomment-8001",
    err: "",
  });
  const posted = ghCall(
    dir,
    ...["api", "-X", "POST", "repos/example/widgets/issues/1001/comments", "-f", "body=a second"],
    ...["--jq", "[.id, .html_url, .body]"]
  );
  expect([posted.code, JSON.parse(posted.out)]).toEqual([
    0,
    [8002, "https://example.invalid/pull/1001#issuecomment-8002", "a second"],
  ]);
  expect(
    ghCall(dir, "api", "repos/example/widgets/issues/1001/comments", "--jq", "[.[].id] | @csv").out
  ).toBe("8001,8002");
  expect(
    ghCall(dir, "api", "-X", "POST", "repos/example/widgets/issues/1001/comments", "-f", "body=")
  ).toEqual({
    code: 1,
    out: "",
    err: "gh: Unprocessable Entity (HTTP 422)\nA comment on the conversation is created with a body.\n",
  });
}, 60_000);

// `gh pr view --json files` is one of gh's own JSON fields for a pull request: the files the pull
// request changes, each with its `path`, `additions` and `deletions`. Measured in INFRA-764 round
// 25 (runs/2b44667b6a7a, reviewer-text-findings-defect-base-2, calls.jsonl:10): the reviewer's
// `gh pr view 1001 --repo fernbrook/widgets --json files --jq '.files[].path'` was answered with
// no `files` at all, and jq exited 1 with "Cannot iterate over null", where GitHub lists the
// files. Catches: a field gh answers for every pull request answered as null, and a list that is
// not the files of the pull request's diff as the run's own remote holds it.
test("pr view's files are the files the pull request's diff changes", () => {
  const dir = worldDir("review_world");
  const names = ghCall(dir, "pr", "diff", "1001", "--repo", "example/widgets", "--name-only");
  expect(names.code).toBe(0);
  expect(names.out).not.toBe("");
  const files = ghCall(
    dir,
    ...["pr", "view", "1001", "--repo", "example/widgets", "--json", "files"],
    ...["--jq", ".files[].path"]
  );
  expect([files.code, files.out, files.err]).toEqual([0, names.out, ""]);
}, 60_000);

/** `gh pr view`'s reviews as a row reads them. */
type ViewedReview = {
  id: string;
  author: { login: string };
  state: string;
  body: string;
  commit: { oid: string };
  submittedAt: string | null;
};
// `gh pr view --json reviews` is gh's reading of the pull request's reviews through GitHub's
// GraphQL: each review's node id, its author by the login GraphQL gives it (an App's bot without
// `[bot]`), its state, body and time, and the commit it stands at as `commit.oid`. Measured in
// INFRA-764 round 25: the world answered the REST review its fixture froze (an integer `id`,
// `commit_id`, `user`), and agents reading `.reviews[].commit.oid` read null (the architect's
// ruling 1). `latestReviews` is GitHub's reading of the same reviews: the last of each author but
// the pull request's own, which gh reads with no id and no commit (gh-view.ts's `latestReviews`).
// The world's review here, correct's `COMMENT` review, has its body updated, the run makes a
// review of its own, and its reply to a thread is filed by GitHub in a `COMMENTED` review of its
// own (id 6_000_000 plus the reply's databaseId, the first a run makes being 9001). Catches: a
// review in REST's shape; a list frozen at the world's seed, missing the review the run made, the
// one GitHub filed its reply in, or the body the run gave the world's review; and `latestReviews`
// that is `reviews`, holds an author's earlier review, or names an id and a commit gh does not
// read.
test("pr view's reviews are gh's: every review GitHub holds, and latestReviews its reading of them", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  for (const argv of [
    ["api", "-X", "PUT", `${REVIEWS}/5001`, "-f", "body=One thread left, and updated."],
    [
      ...["api", "-X", "POST", REVIEWS, "-f", `commit_id=${head}`],
      ...["-f", "event=REQUEST_CHANGES", "-f", "body=One more finding."],
    ],
    ["api", "-X", "POST", `${REVIEW_REPLIES}`, "-f", "body=done"],
  ])
    expect([argv, ghCall(dir, ...argv).code]).toEqual([argv, 0]);
  const read = pinnedRead("gh pr view reviews");
  const got = pinnedCall(dir, read, { number: "1001" });
  // gh reads a review's `url` only past its first page of 100 reviews, which production's 137
  // reach and no world does (gh-view.ts's `reviewNode`).
  expect([got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
    0,
    "",
    { ...SAME_SHAPE, missing: [".reviews[].url"] },
  ]);
  const { reviews }: { reviews: ViewedReview[] } = JSON.parse(got.out);
  expect(
    reviews.map(({ id, author, state, body, commit }) => [
      id,
      author.login,
      state,
      body,
      commit.oid,
    ])
  ).toEqual([
    ["PRR_kwDOAAAAAc4AAE5001", "legion-review", "COMMENTED", "One thread left, and updated.", head],
    ["PRR_kwDOAAAAAc4AAE5002", "widgets-reviewer", "CHANGES_REQUESTED", "One more finding.", head],
    ["PRR_kwDOAAAAAc4AAE6009001", "widgets-reviewer", "COMMENTED", "", head],
  ]);
  expect(reviews.every(({ submittedAt }) => typeof submittedAt === "string")).toBe(true);
  const latest: { latestReviews: ViewedReview[] } = JSON.parse(
    ghCall(dir, "pr", "view", "1001", "--json", "latestReviews").out
  );
  expect(
    latest.latestReviews.map(({ id, author, state, commit }) => [
      id,
      author.login,
      state,
      commit.oid,
    ])
  ).toEqual([
    ["", "legion-review", "COMMENTED", ""],
    ["", "widgets-reviewer", "COMMENTED", ""],
  ]);
}, 60_000);

// GitHub shows a review still pending to the account that wrote it alone: the reviewer's pending
// review, and the thread its inline comment opens, are not in the implementer's reviews, its `gh
// pr view`, its GraphQL threads or its read of that review by id, while the tester, which calls
// GitHub with the review App's token as the reviewer does (production's `AppRoleFor`, rig.sh's
// role_identity), sees both; once submitted, the review is every account's. A read by id from
// another App is answered the review read's own 404 (which 404 GitHub gives there is not
// measured: no pending review of another account can be found to read). Catches: a pending
// review in every role's reviews (the world before the architect's ruling of 2026-11-17), at any
// one of those reads, or kept from the App that wrote it; and an App per role that is not the
// one production gives it.
test("a pending review is the App's that wrote it alone, until it is submitted", () => {
  for (const role of ["implementer", "merger", "reviewer", "tester", "planner", "architect"]) {
    const identity = spawnChecked([
      "bash",
      "-c",
      [fn("role_identity"), ...WORLD_VALUES.map(assignment), `role_identity ${role}`].join("\n"),
    ]).stdout.toString();
    const app = identity.startsWith(rigValue("world_implement_app")) ? "implement" : "review";
    expect([role, roleApp(role)]).toEqual([role, app]);
  }
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const input = path.join(dir, "pending.json");
  writeFileSync(
    input,
    JSON.stringify({
      commit_id: head,
      body: "two findings",
      comments: [{ path: "greet.ts", line: 1, side: "RIGHT", body: "a line note" }],
    })
  );
  expect(asked(dir, "reviewer", ["api", "-X", "POST", REVIEWS, "--input", input])[0]).toBe(0);
  const threads = `query=query{repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewThreads(first:10){nodes{id}}}}}`;
  const seen = (by: string) => [
    asked(dir, by, ["api", REVIEWS, "--jq", "[.[] | [.id, .state]] | @json"]).slice(0, 2),
    asked(dir, by, [
      "pr",
      "view",
      "1001",
      "--json",
      "reviews",
      "--jq",
      "[.reviews[].state] | @json",
    ])[1],
    asked(dir, by, ["api", `${REVIEWS}/5002`, "--jq", ".state"]),
    asked(dir, by, [
      "api",
      "graphql",
      "-f",
      threads,
      "--jq",
      ".data.repository.pullRequest.reviewThreads.nodes | length",
    ])[1],
  ];
  // Correct's world holds one thread, the workflow's, on the reviewer's `COMMENT` review 5001, and
  // the pending review's comment opens a second.
  const pending = [
    [0, '[[5001,"COMMENTED"],[5002,"PENDING"]]\n'],
    '["COMMENTED","PENDING"]\n',
    [0, "PENDING\n", ""],
    "2\n",
  ];
  expect(seen("reviewer")).toEqual(pending);
  expect(seen("tester")).toEqual(pending);
  expect(seen("implementer")).toEqual([
    [0, '[[5001,"COMMENTED"]]\n'],
    '["COMMENTED"]\n',
    [
      1,
      notFoundOf("https://docs.github.com/rest/pulls/reviews#get-a-review-for-a-pull-request"),
      "gh: Not Found (HTTP 404)\n",
    ],
    "1\n",
  ]);
  expect(
    asked(dir, "reviewer", [
      "api",
      "-X",
      "POST",
      `${REVIEWS}/5002/events`,
      "-f",
      "event=APPROVE",
    ])[0]
  ).toBe(0);
  expect(seen("implementer")).toEqual([
    [0, '[[5001,"COMMENTED"],[5002,"APPROVED"]]\n'],
    '["COMMENTED","APPROVED"]\n',
    [0, "APPROVED\n", ""],
    "2\n",
  ]);
}, 60_000);

// GitHub answers each installation token as its own App's bot: the GraphQL viewer, and the
// author of every comment a call of that token makes, is the implement App's bot for the
// implementer and the merger and the review App's for every other role (rest-objects.ts's
// `roleApp`, world.json's `logins`). Measured on 2026-11-18 from two INFRA issue pods: `gh api
// graphql -f query='{viewer{login}}'` read the review App's bot with the tester's token
// (prod-reads-19.json) and the implement App's with the implementer's (implementer-reads.json).
// Catches: every role read as the review App's bot, the one the world's graph names, so an
// implementer that told its own comments by the viewer read them as the reviewer's.
test("the viewer and the author of what a call writes are the bot of the calling role's App", () => {
  const { dir } = runDir({ fixture: true });
  const implement = `${rigValue("world_implement_app")}[bot]`;
  const review = `${rigValue("world_review_app")}[bot]`;
  const viewer = [
    "api",
    "graphql",
    "-f",
    "query=query{viewer{login}}",
    "--jq",
    ".data.viewer.login",
  ];
  expect(
    ["implementer", "merger", "reviewer", "tester", "architect"].map((role) => [
      role,
      answer(dir, viewer, role),
    ])
  ).toEqual([
    ["implementer", implement],
    ["merger", implement],
    ["reviewer", review],
    ["tester", review],
    ["architect", review],
  ]);
  answer(dir, ["pr", "comment", "1001", "--body", "from the implementer"], "implementer");
  const posted = ["api", "-X", "POST", "repos/example/widgets/issues/1001/comments"];
  expect(
    answer(dir, [...posted, "-f", "body=from the reviewer", "--jq", ".user.login"], "reviewer")
  ).toBe(review);
  expect(
    answer(dir, [
      "api",
      "repos/example/widgets/issues/1001/comments",
      "--jq",
      "[.[].user.login] | @json",
    ])
  ).toBe(JSON.stringify([implement, review]));
  const authored = ["pr", "view", "1001", "--json", "comments", "--jq"];
  expect(
    answer(
      dir,
      [...authored, "[.comments[] | [.author.login, .viewerDidAuthor]] | @json"],
      "implementer"
    )
  ).toBe(
    JSON.stringify([
      [rigValue("world_implement_app"), true],
      [rigValue("world_review_app"), false],
    ])
  );
});

// GitHub's GraphQL names a bot under `author` as a `Bot` by its App's slug, with no `[bot]`, and
// under `viewer` and a thread's `resolvedBy` as a `User` with it; its REST API names it with it
// too. Measured on 2026-11-18 against cybermonklabs/legion's merged pull requests: a resolved
// thread's first comment's author `{__typename: "Bot", login: "legion-reviewer"}`, its
// `resolvedBy` `{__typename: "User", login: "legion-implementer[bot]"}`; the REST comments'
// `user.login` `legion-reviewer[bot]` and `gh pr view --json comments` `legion-implementer` (the
// production reads). Catches: a thread comment's author named with its `[bot]`, the world's own
// and a reply the run made alike, so a role that compared it to the viewer read them as one
// account; and a REST comment that lost its `[bot]` with it.
test("a thread comment's author is GraphQL's Bot by its slug, the viewer, resolvedBy and REST name it with its [bot]", () => {
  const dir = worldDir("review_world");
  const thread = "PRRT_kwDOrig0001";
  const review = rigValue("world_review_app");
  expect(
    asked(dir, "reviewer", [
      ...["api", "-X", "POST", "repos/example/widgets/pulls/1001/comments/7001/replies"],
      ...["-f", "body=named each test for its case", "--jq", ".user.login"],
    ])
  ).toEqual([0, `${review}[bot]\n`, ""]);
  const resolve = `query=mutation{resolveReviewThread(input:{threadId:"${thread}"}){thread{resolvedBy{__typename login}}}}`;
  expect(answer(dir, ["api", "graphql", "-f", resolve], "reviewer")).toBe(
    JSON.stringify({
      data: {
        resolveReviewThread: {
          thread: { resolvedBy: { __typename: "User", login: `${review}[bot]` } },
        },
      },
    })
  );
  const threads =
    'query=query{viewer{login} repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewThreads(first:10){nodes{resolvedBy{login} comments(first:10){nodes{author{__typename login}}}}}}}}';
  expect(JSON.parse(answer(dir, ["api", "graphql", "-f", threads], "reviewer"))).toEqual({
    data: {
      viewer: { login: `${review}[bot]` },
      repository: {
        pullRequest: {
          reviewThreads: {
            nodes: [
              {
                resolvedBy: { login: `${review}[bot]` },
                comments: {
                  nodes: [
                    { author: { __typename: "Bot", login: "style-bot" } },
                    { author: { __typename: "Bot", login: review } },
                  ],
                },
              },
            ],
          },
        },
      },
    },
  });
  expect(
    answer(dir, [
      "api",
      "repos/example/widgets/pulls/1001/comments",
      "--jq",
      "[.[].user | [.login, .type]] | @json",
    ])
  ).toBe(
    JSON.stringify([
      ["style-bot[bot]", "Bot"],
      [`${review}[bot]`, "Bot"],
    ])
  );
}, 60_000);

// With no `--json`, gh prints its own text of the pull request when stdout is no terminal, never
// the object: measured on 2026-11-17 (the production reads' prod-pr-view.txt, `gh pr view 2827`),
// thirteen `<name>:\t<value>` lines — the title, the state, the author's GraphQL login, the
// labels, the assignees, the reviewers (the last review of each, the pull request's author's
// own left out, by name), the projects, the milestone, the number, the URL, the lines added and
// removed, auto-merge — then `--` and the body. With `--comments` it prints the conversation's
// comments and the reviews gh shows, in the order they were made, and nothing of the pull request
// itself (prod-pr-view-comments.txt): a review GitHub filed a reply in, `COMMENTED` with no body, is
// not shown. The world is code-defect's, whose reviewer's approval with its body stands on the
// pull request (correct's reviewer approved nothing, and its `COMMENT` review has no body, so gh
// shows it in neither). Catches: the world's object printed as JSON for either (the architect's
// ruling 2); a line named or placed otherwise than gh's; and `--comments` printing the pull
// request's lines, a comment with a review's status, or the review a reply is filed in.
test("pr view without --json prints gh's text, and --comments the comments and the reviews gh shows", () => {
  const dir = worldDir("approved_world", "dropped", { worldCase: "code-defect" });
  const head = builtHead(dir);
  const raw = (...argv: string[]) => {
    const made = spawnChecked(["bun", STANDIN, "gh", ...argv], { env: callEnv(dir) });
    return [made.exitCode, made.stdout.toString(), made.stderr.toString()];
  };
  const facts: { additions: number; deletions: number; body: string } = JSON.parse(
    ghCall(dir, "pr", "view", "1001", "--json", "additions,deletions,body").out
  );
  expect(facts.additions).toBeGreaterThan(0);
  expect(raw("pr", "view", "1001")).toEqual([
    0,
    [
      "title:\tfeat: greet() (LWEVAL-1)",
      "state:\tOPEN",
      "author:\twidgets-implementer",
      "labels:\t",
      "assignees:\t",
      "reviewers:\tlegion-review (Approved)",
      "projects:\t",
      "milestone:\t",
      "number:\t1001",
      "url:\thttps://github.com/example/widgets/pull/1001",
      `additions:\t${facts.additions}`,
      `deletions:\t${facts.deletions}`,
      "auto-merge:\tdisabled",
      "--",
      `${facts.body}\n`,
    ].join("\n"),
    "",
  ]);
  for (const argv of [
    ["pr", "comment", "1001", "--body", "a note on the change"],
    [
      ...["api", "-X", "POST", REVIEWS, "-f", `commit_id=${head}`],
      ...["-f", "event=REQUEST_CHANGES", "-f", "body=One more finding."],
    ],
    ["api", "-X", "POST", REVIEW_REPLIES, "-f", "body=done"],
  ])
    expect([argv, ghCall(dir, ...argv).code]).toEqual([argv, 0]);
  const entry = (author: string, status: string, body: string) =>
    `author:\t${author}\nassociation:\tnone\nedited:\tfalse\nstatus:\t${status}\n--\n${body}\n--\n`;
  expect(raw("pr", "view", "1001", "--comments")).toEqual([
    0,
    [
      entry(
        "legion-review",
        "approved",
        "Approved at this head. The open threads are text findings and are the work the merge waits for."
      ),
      entry("widgets-reviewer", "none", "a note on the change"),
      entry("widgets-reviewer", "changes requested", "One more finding."),
    ].join(""),
    "",
  ]);
  expect(raw("pr", "view", "1001")[1]).toContain(
    "reviewers:\tlegion-review (Approved), widgets-reviewer (Commented)\n"
  );
}, 60_000);

// `gh pr view --json comments` is gh's reading of the conversation through GitHub's GraphQL: every
// comment, the world's and the run's (posted by `gh pr comment` or the REST create), each with
// gh's eleven fields (production-shapes.json's `gh pr view comments`), the same comment the REST
// conversation answers (the architect's ruling 7). Catches: the six fields the world answered a
// run's comment with (no `includesCreatedEdit`, `isMinimized`, `minimizedReason`,
// `reactionGroups`, `viewerDidAuthor`), a `minimizedReason` of null where gh reads "", an author
// by its REST login, a comment another answer gives another id, link or time, and a comment the
// run posted missing.
test("pr view's comments are gh's, each the conversation's own comment", () => {
  const dir = worldDir("review_world");
  for (const argv of [
    ["pr", "comment", "1001", "--body", "a note on the change"],
    ["api", "-X", "POST", "repos/example/widgets/issues/1001/comments", "-f", "body=and another"],
  ])
    expect([argv, ghCall(dir, ...argv).code]).toEqual([argv, 0]);
  const read = pinnedRead("gh pr view comments");
  const got = pinnedCall(dir, read, { number: "1001" });
  expect([got.code, got.err, shapeDiff(got.answer, read)]).toEqual([0, "", SAME_SHAPE]);
  const rest = JSON.parse(ghCall(dir, "api", "repos/example/widgets/issues/1001/comments").out);
  expect(JSON.parse(got.out).comments).toEqual(
    rest.map((comment: Record<string, string>) => ({
      id: comment.node_id,
      author: { login: "widgets-reviewer" },
      authorAssociation: comment.author_association,
      body: comment.body,
      createdAt: comment.created_at,
      includesCreatedEdit: false,
      isMinimized: false,
      minimizedReason: "",
      reactionGroups: [],
      url: comment.html_url,
      viewerDidAuthor: true,
    }))
  );
  expect(rest.map((comment: { body: string }) => comment.body)).toEqual([
    "a note on the change",
    "and another",
  ]);
}, 60_000);

// gh checks every field `--json` names against the ones it takes before it asks GitHub anything,
// and refuses the first it does not take with its own text on stderr, exit 1: measured on
// 2026-11-17 for `gh pr view 2827 --repo cybermonklabs/legion --json bogus`, `… --json
// number,bogus,other` (naming `bogus` alone) and `gh pr list --repo cybermonklabs/legion --json
// bogus` (gh-view.ts's `unknownField`). Every field it takes, the world serves. Catches: a field gh
// refuses answered as an object without it, and one gh takes answered by its absence.
test("pr view and pr list refuse a --json field gh does not take, and serve every field it does", () => {
  const { dir } = runDir({ fixture: true });
  const refusal = `Unknown JSON field: "bogus"\nAvailable fields:\n${PR_FIELDS.map((field) => `  ${field}\n`).join("")}`;
  for (const argv of [
    ["pr", "view", "1001", "--json", "bogus"],
    ["pr", "view", "1001", "--json", "number,bogus,other"],
    ["pr", "list", "--json", "bogus"],
  ])
    expect([argv, ghCall(dir, ...argv)]).toEqual([argv, { code: 1, out: "", err: refusal }]);
  const all = ghCall(dir, "pr", "view", "1001", "--json", PR_FIELDS.join(","));
  expect([all.code, all.err, Object.keys(JSON.parse(all.out))]).toEqual([0, "", [...PR_FIELDS]]);
}, 60_000);

// gh asks GitHub for the pull request a `gh pr view` or `gh pr diff` names, and GitHub has none of
// a number or a branch the world has not got, nor a repository: measured on 2026-11-17 against
// cybermonklabs/legion, each printed on stderr with exit 1 and nothing on stdout (gh-standin.ts's
// `pullRefusal`). Catches: the world's pull request answered for any number, branch or
// repository a call names, and a refusal in other words than gh's.
test("pr view and pr diff of a pull request or a repository the world has not got are refused as gh refuses them", () => {
  const { dir } = runDir({ fixture: true });
  const missing = (number: number) =>
    `GraphQL: Could not resolve to a PullRequest with the number of ${number}. (repository.pullRequest)\n`;
  for (const [argv, err] of [
    [["pr", "view", "999999", "--json", "number"], missing(999999)],
    [["pr", "view", "#7"], missing(7)],
    [["pr", "view", "https://github.com/example/widgets/pull/999"], missing(999)],
    [["pr", "diff", "999999", "--repo", "example/widgets"], missing(999999)],
    [["pr", "view", "nothing-here"], 'no pull requests found for branch "nothing-here"\n'],
    [
      ["pr", "view", "1001", "--repo", "example/gadgets"],
      "GraphQL: Could not resolve to a Repository with the name 'example/gadgets'. (repository)\n",
    ],
    [
      ["pr", "list", "--repo", "example/gadgets", "--json", "number"],
      "GraphQL: Could not resolve to a Repository with the name 'example/gadgets'. (repository)\n",
    ],
  ] as const)
    expect([argv, ghCall(dir, ...argv)]).toEqual([argv, { code: 1, out: "", err }]);
  for (const named of [
    "1001",
    "#1001",
    "legion/LWEVAL-1",
    "https://github.com/example/widgets/pull/1001",
  ])
    expect([named, ghCall(dir, "pr", "view", named, "--json", "number").out]).toEqual([
      named,
      '{"number":1001}',
    ]);
}, 60_000);

// gh lists the open pull requests its filters match, and nothing — `[]` with `--json`, no line
// without it, exit 0 — where none does: measured on 2026-11-17 against cybermonklabs/legion,
// `--head` and `--base` name a branch, `--head <owner>:<branch>` matches nothing, `--state` is open
// unless it names closed, merged or all, and `--author` names an App's bot as `app/<slug>` or
// `<slug>[bot]` and never by its slug alone (gh-standin.ts's `listsPull`). The world's one pull
// request is open, on `legion/LWEVAL-1` against `main`, opened by the implement App, with no
// label, no assignee and no draft. A filter the world does not read (`--search`) is refused as a
// call it does not model. Catches: every listing answered with the world's pull request whatever
// it filters by (the route the world wrote for `gh pr list`), and a filter read otherwise than gh.
test("pr list lists the world's pull request where its filters match it, and nothing where they do not", () => {
  const dir = worldDir("review_world");
  const listed = (...filters: string[]) =>
    ghCall(dir, "pr", "list", "--repo", "example/widgets", ...filters, "--json", "number").out;
  for (const filters of [
    [],
    ["--head", "legion/LWEVAL-1", "--state", "open"],
    ["-B", "main", "-s", "all"],
    ["--author", "app/widgets-implementer"],
    ["--author", "widgets-implementer[bot]"],
    ["--app", "widgets-implementer"],
  ])
    expect([filters, listed(...filters)]).toEqual([filters, '[{"number":1001}]']);
  for (const filters of [
    ["--head", "legion/WID-1"],
    ["--head", "example:legion/LWEVAL-1"],
    ["--base", "trunk"],
    ["--state", "closed"],
    ["--state", "merged"],
    ["--author", "widgets-implementer"],
    ["--author", "app/widgets-reviewer"],
    ["--app", "widgets-reviewer"],
    ["--label", "bug"],
    ["--assignee", "robin-hale"],
    ["--draft"],
  ])
    expect([filters, listed(...filters)]).toEqual([filters, "[]"]);
  expect(ghCall(dir, "pr", "list", "--head", "legion/WID-1")).toEqual({
    code: 0,
    out: "",
    err: "",
  });
  expect(ghCall(dir, "pr", "list", "--search", "is:open", "--json", "number")).toEqual({
    code: 1,
    out: "",
    err: "gh: Not Found (HTTP 404)\n",
  });
}, 60_000);

// Each pinned read of a gh command of its own — `gh pr view`, `gh pr list`, `gh pr diff`, `gh pr
// checks`, `gh run view` and `gh run watch` — made against the world in production's own words,
// its answer held to production's shape, exit and stderr (the architect's ruling d): every one
// INFRA-764 round 25's runs made, read against cybermonklabs/legion #2827 at about 15:40Z on
// 2026-11-17, and every one the world served with no pin, read on 2026-11-18 at about 08:45Z
// against #2827, open, and #4326, merged (the tester's prod-reads-19.json), and at 08:55Z (the
// implementer's readings), production-shapes.ts's `laterReads`. The world holds a review of the
// run's, its reply's review and a comment on the conversation, so no list a read names is empty;
// `<app>` is the implement App's slug, the App whose pull requests production's `--author` read
// listed, and `<branch>` the world's issue branch. Catches, for each read: a field gh answers
// missing or in another kind, a field gh never prints, gh's text answered as JSON, and an exit or
// a line gh does not give. A text answer is held to production's line shapes too
// (production-shapes.ts's `lineShapes`), read as gh wrote it, untrimmed (`ghRaw`), each from a
// reading of production's whole answer (the tester read every text read again on 2026-11-18 at
// about 17:25Z, prod-text-reads.json): an empty or a wrong-format text, a `gh pr checks` row of
// four fields, a view with no `--`, and a line in a shape production's text has not got. A
// difference of state is named with the reading's own reason (PINNED_GH_STATE), and a reading of
// #2827 in a state no world's pull request is in is held to production's reading of the same
// command on #4326, which is in the world's (`against`). One more is of state for every read of
// `reviews`: gh reads a review's `url` only past its first 100 reviews, which production's 137
// reach.
const PINNED_GH = pinnedReads()
  .filter(({ name }) => /^gh (pr|run) /.test(name))
  .map(({ name }) => name);
/** Why a diff of the world's pull request differs in its line shapes from production's: #4326's
 * changes files that were there and removes lines of them (the tester's readings of 2026-11-18),
 * and the world's adds its files (`new file mode`) and removes no line of a file it changes. */
const WORLD_DIFF_STATE =
  "#4326's diff changes files that were there and removes lines of them; the world's pull request adds its files and removes no line";
/** The line shapes a diff of the world's pull request differs in from #4326's (WORLD_DIFF_STATE). */
const WORLD_DIFF = { missing: ["line -<text>"], extra: ["line new file mode <mode>"] };
/** Why #2827's diff reads are held to #4326's: read whole again on 2026-11-18 at 17:22Z, GitHub
 * refused #2827's diff (`HTTP 406: Sorry, the diff exceeded the maximum number of lines (20000)`)
 * and gh printed none of it, a state no world's pull request of a few files is in. */
const DIFF_PAST_LIMIT =
  "#2827's diff was past GitHub's 20000-line limit when read whole, so GitHub refused it (HTTP 406) and gh printed nothing; the world's pull request changes a few files, as #4326 does";
/** The reads of PINNED_GH whose answer differs from the world's by the state of the pull request
 * production read, each with the reason its reading gives: `against`, production's reading of the
 * same command on a pull request in the state every world's is in, which the world's answer is
 * held to instead; `kinds`, the kinds the two answers differ in; or `missing` and `extra`, the
 * line shapes of a text answer one holds and the other does not. */
const PINNED_GH_STATE: Readonly<
  Record<
    string,
    { against?: string; kinds?: string[]; missing?: string[]; extra?: string[]; state: string }
  >
> = {
  "gh pr view --json potentialMergeCommit": {
    kinds: [".potentialMergeCommit: null in production, object in the world"],
    state:
      "#2827 conflicted with its base when read (`mergeable` CONFLICTING and `mergeStateStatus` DIRTY, read beside it at 08:44Z), so GitHub held no test merge of it; the world's head merges into its base cleanly, and its test merge is the commit the REST `merge_commit_sha` names (rest-objects.ts's `mergeCommit`)",
  },
  "gh pr view --json statusCheckRollup": {
    against: "gh pr view --json statusCheckRollup (a pull request with check runs)",
    state:
      "#2827's head had reported no check run when read, and every world's head reports its concluded checks (rig.sh's concluded_checks); the tester read #4326's rollup for its check runs",
  },
  "gh pr checks (open)": {
    against: "gh pr checks (merged, all concluded)",
    state:
      "#2827's head had reported no check run when read, so gh refused it, and every world's head reports its concluded checks (rig.sh's concluded_checks), as #4326's had",
  },
  ...Object.fromEntries(
    (
      [
        ["gh pr diff -", "gh pr diff"],
        ["gh pr diff ---color", "gh pr diff --color never (small)"],
      ] as const
    ).map(([name, against]) => [
      name,
      { against, ...WORLD_DIFF, state: `${DIFF_PAST_LIMIT}; ${WORLD_DIFF_STATE}` },
    ])
  ),
  "gh pr diff ---name-only": { against: "gh pr diff --name-only", state: DIFF_PAST_LIMIT },
  ...Object.fromEntries(
    ["gh pr diff", "gh pr diff --color never (small)"].map((name) => [
      name,
      { ...WORLD_DIFF, state: WORLD_DIFF_STATE },
    ])
  ),
};
/** Where the pinned reads' world is kept once it is built, with the run's writes in it, for each
 * row to copy: one build for the reads, where a build each would be most of the suite. */
const pinnedCache = temporaryDirectory("gh-standin-pinned");
function pinnedWorldCopy(): string {
  const kept = path.join(pinnedCache, "world");
  if (!existsSync(kept)) {
    const built = worldDir("approved_world", "", { worldCase: "correct" });
    const head = builtHead(built);
    for (const argv of [
      ["pr", "comment", "1001", "--body", "a note on the change"],
      [
        ...["api", "-X", "POST", REVIEWS, "-f", `commit_id=${head}`],
        ...["-f", "event=COMMENT", "-f", "body=One note."],
      ],
      ["api", "-X", "POST", REVIEW_REPLIES, "-f", "body=done"],
    ])
      expect([argv, ghCall(built, ...argv).code]).toEqual([argv, 0]);
    cpSync(built, kept, { recursive: true });
  }
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-pinned-"));
  made.push(dir);
  cpSync(kept, dir, { recursive: true });
  return dir;
}
for (const name of PINNED_GH)
  test(`a gh command's read answers in production's shape: ${name}`, () => {
    const read = pinnedRead(name);
    const state = PINNED_GH_STATE[name];
    const held = state?.against === undefined ? read : pinnedRead(state.against);
    const values = {
      owner: "example",
      repo: "widgets",
      number: "1001",
      app: rigValue("world_implement_app"),
      branch: "legion/LWEVAL-1",
    };
    const got = ghRaw(pinnedWorldCopy(), ...argvOf(read, values));
    const missing = ".reviews[]" in held.shape ? [".reviews[].url"] : [];
    expect([got.code, got.err, shapeDiff(answerOf(got.out), held)]).toEqual([
      held.exit,
      stderrOf(held, values),
      {
        missing: [...missing, ...(state?.missing ?? [])],
        extra: state?.extra ?? [],
        kinds: state?.kinds ?? [],
      },
    ]);
  }, 60_000);

// #4326's text views, read whole on 2026-11-18 (the tester's prod-text-reads.json), are of a pull
// request with no comment on its conversation, the state a world's pull request is in until an
// agent posts one: code-defect's approved_world holds the reviewer's approval alone (correct's
// `COMMENT` review has no body, which gh's `--comments` does not show). The loop above holds them
// after the run's comment and reviews; this row holds them before any write. Catches: a view, or
// a `--comments` list of a pull request with no conversation comment, that prints a line in a
// shape #4326's has not got or leaves out one it has.
test("a pull request no agent has commented on is viewed in #4326's line shapes", () => {
  const dir = worldDir("approved_world", "dropped", { worldCase: "code-defect" });
  expect(ghCall(dir, "api", "repos/example/widgets/issues/1001/comments").out).toBe("[]");
  for (const name of ["gh pr view (text, small)", "gh pr view --comments (text, small)"]) {
    const read = pinnedRead(name);
    const got = pinnedCall(dir, read, { number: "1001" });
    expect([name, got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
      name,
      read.exit,
      read.stderr,
      SAME_SHAPE,
    ]);
  }
}, 60_000);

// GitHub refuses a query that is no GraphQL document with its parser's message and the line and
// column of the token it stopped at, and a query selecting a field its schema's type has not got
// with its validator's error, each with no data; gh prints GitHub's body as it came, with no line
// end, then `gh: ` and each message on a line of its own, and exits 1. Measured on 2026-11-17
// against cybermonklabs/legion (the production reads' prod-gql-error.txt and .err for the first):
// the query of eight `{` and seven `}` stopped at its last `}`, column 176 there and 171 here,
// whose owner and repository are five characters shorter; and for each case below the same
// query of production's ran there, its column five more than here: a string no quote closes (at
// 51), a field `PullRequest` has not got with one `PullRequestReviewThread` has not got (87 and
// 125), a `{` where a `)` belongs (53), a `}` too many (89), and a call that sends no query. A
// mutation GitHub refuses runs none of itself. Catches (the architect's ruling 10): the world's
// own sentence for a refusal (`Parse error: the query is no GraphQL document`), a refusal with no
// `locations` or another place in it, a field the world answers null where GitHub refuses the
// query, gh's `gh: ` before each of two messages, a line end gh does not print, and a refused
// mutation that resolves its thread all the same.
test("a query GitHub refuses is refused with GitHub's message and the place it names", () => {
  const dir = worldDir("review_world");
  const repo = 'repository(owner:"example",name:"widgets")';
  const parse = (line: number, column: number, message: string) => ({
    message: `${message} at [${line}, ${column}]`,
    locations: [{ line, column }],
  });
  const unknown = (path: string[], type: string, field: string, column: number) => ({
    path: ["query", "repository", "pullRequest", ...path, field],
    extensions: { code: "undefinedField", typeName: type, fieldName: field },
    locations: [{ line: 1, column }],
    message: `Field '${field}' doesn't exist on type '${type}'`,
  });
  const noName = 'Expected NAME, actual: (none) ("")';
  for (const [query, errors] of [
    [
      `query{${repo}{pullRequest(number:1001){reviewThreads(first:20){nodes{id isResolved path comments(first:5){nodes{body author{login}}}}}}}`,
      [parse(1, 171, noName)],
    ],
    [
      'query{repository(owner:"example,name:"widgets"){pullRequest(number:1001){number}}}',
      [
        {
          message: "Expected string or block string, but it was malformed",
          locations: [{ line: 1, column: 46 }],
        },
      ],
    ],
    [
      `query{${repo}{pullRequest(number:1001){number bogus reviewThreads(first:1){nodes{id nope}}}}}`,
      [
        unknown([], "PullRequest", "bogus", 82),
        unknown(["reviewThreads", "nodes"], "PullRequestReviewThread", "nope", 120),
      ],
    ],
    [
      'query{repository(owner:"example",name:"widgets"{pullRequest(number:1001){number}}}',
      [parse(1, 48, 'Expected NAME, actual: LCURLY ("{")')],
    ],
    [
      `query{${repo}{pullRequest(number:1001){number}}}}`,
      [
        parse(
          1,
          84,
          'Expected one of SCHEMA, SCALAR, TYPE, ENUM, INPUT, UNION, INTERFACE, actual: RCURLY ("}")'
        ),
      ],
    ],
    [`query{\n  ${repo}{\n    name\n`, [parse(3, 5, noName)]],
    ["", [{ message: "A query attribute must be specified and must be a string." }]],
  ] as const) {
    const made = spawnChecked(["bun", STANDIN, "gh", "api", "graphql", "-f", `query=${query}`], {
      env: callEnv(dir),
    });
    expect([query, made.exitCode, made.stdout.toString(), made.stderr.toString()]).toEqual([
      query,
      1,
      JSON.stringify({ errors }),
      `gh: ${errors.map((error) => error.message).join("\n")}\n`,
    ]);
  }
  const thread = '"PRRT_kwDOrig0001"';
  const refused = ghCall(
    dir,
    ...["api", "graphql", "-f"],
    `query=mutation{resolveReviewThread(input:{threadId:${thread}}){thread{isResolved nope}}}`
  );
  expect([refused.code, refused.err]).toEqual([
    1,
    "gh: Field 'nope' doesn't exist on type 'PullRequestReviewThread'\n",
  ]);
  const listed = ghCall(
    dir,
    ...["api", "graphql", "-f"],
    `query=query{${repo}{pullRequest(number:1001){reviewThreads(first:10){nodes{id isResolved}}}}}`,
    ...["--jq", ".data.repository.pullRequest.reviewThreads.nodes[] | [.id, .isResolved] | @tsv"]
  );
  expect(listed).toEqual({ code: 0, out: "PRRT_kwDOrig0001\tfalse", err: "" });
}, 60_000);

// Each pinned GraphQL read made against the world in production's own words, as the gh reads
// above are, its answer held to production's shape, exit and stderr: the 20 distinct queries of
// review threads INFRA-764 round 25's runs made, read against #2827 on 2026-11-17, and those the
// world served with no pin, read on 2026-11-18 (the viewer by every spelling of the endpoint, the
// repository's and the pull request's fields, the timeline, and a variable sent or not sent). A
// `--paginate` read is held as its pages, the form production's was kept in: #2827's threads,
// more than 100, came on two pages, which gh printed back to back, and the world's fewer threads
// come on one. The one difference is a place GitHub names in a query that names the repository:
// production's owner and repository are five characters longer than the world's, so a column
// after them is five more there (PINNED_GRAPHQL_PLACE). Catches, for each read: a field the query
// selects answered missing or in another kind, a field it does not select, a connection's
// `pageInfo` lost, pages printed otherwise than gh prints them, a refusal with no `locations` or
// `extensions`, and an exit or a line gh does not give.
const PINNED_GRAPHQL = pinnedReads()
  .filter(({ name }) => /^gh api \/?graphql/.test(name))
  .map(({ name }) => name);
/** The place a pinned GraphQL refusal names, as production's stderr gave it and as the world's,
 * by the read: GitHub's column of the query's end, after the repository's owner and name. */
const PINNED_GRAPHQL_PLACE: Readonly<Record<string, readonly [string, string]>> = {
  'gh api graphql query{repository(owner:"…",name:"…"){pullRequest(number:N){reviewThreads(first:N){nodes{id isResolved path comments(first:N){nodes{body author{login}}}}}}}':
    ["at [1, 176]", "at [1, 171]"],
};
for (const name of PINNED_GRAPHQL)
  test(`a GraphQL read answers in production's shape: ${name}`, () => {
    const read = pinnedRead(name);
    const values = { owner: "example", repo: "widgets", number: "1001" };
    const got = ghCall(pinnedWorldCopy(), ...argvOf(read, values));
    const answer = read.command.includes("--paginate") ? pagesOf(got.out) : answerOf(got.out);
    const [there, here] = PINNED_GRAPHQL_PLACE[name] ?? [];
    const stderr = stderrOf(read, values);
    expect([got.code, got.err, shapeDiff(answer, read)]).toEqual([
      read.exit,
      there === undefined || here === undefined ? stderr : stderr.replace(there, here),
      SAME_SHAPE,
    ]);
  }, 60_000);

// GitHub answers `gh api repos/<o>/<r>/contents/<path>?ref=<sha>` with the file at that commit:
// its own bytes under the raw media type, and GitHub's JSON of it otherwise. The first call is
// the tester's, word for word but the head (INFRA-764 round 24,
// not-counted-r24/threads-closed-text-fixes-head-3/calls.jsonl:16): the reviewer re-read the
// fixed file at the implementer's head, was refused 404 by a world that answered no contents
// read, and wrote "Content API 404s are just the eval sandbox"; that refusal voided 2 of the 7
// launches of the arm. Catches: the read refused, a file read from anywhere but the run's own
// remote at that commit (the fix push changes it), a raw read given a newline the file has not
// got, JSON whose content or sha is not the blob's, a ref or a path the remote has not got
// answered 200, and a route the world serves with no evidence (gh-route-evidence.ts).
test("the contents read answers the file at the commit from the run's own remote, raw or as GitHub's JSON", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const raw = (sha: string, file = "greet.ts") => [
    "api",
    `repos/example/widgets/contents/${file}?ref=${sha}`,
    "-H",
    "Accept: application/vnd.github.raw",
  ];
  const bytes = (sha: string, file = "greet.ts") =>
    spawnChecked([
      "git",
      "-C",
      path.join(dir, "remote.git"),
      "show",
      `${sha}:${file}`,
    ]).stdout.toString();
  const read = (argv: string[]) => {
    const made = spawnChecked(["bun", STANDIN, "gh", ...argv], {
      env: callEnv(dir),
    });
    return { code: made.exitCode, out: made.stdout.toString(), err: made.stderr.toString() };
  };
  expect(read(raw(head))).toEqual({ code: 0, out: bytes(head), err: "" });
  const fix = pushFix(dir, "greet.ts", "export const greet = (name: string) => name.trim();");
  expect(read(raw(fix))).toEqual({
    code: 0,
    out: "export const greet = (name: string) => name.trim();",
    err: "",
  });
  expect(read(raw(head)).out).toBe(bytes(head));
  expect(JSON.parse(read(raw(fix).slice(0, 2)).out)).toMatchObject({
    type: "file",
    encoding: "base64",
    name: "greet.ts",
    path: "greet.ts",
    sha: git(path.join(dir, "remote.git"), "rev-parse", `${fix}:greet.ts`),
    size: 51,
    download_url: `https://raw.githubusercontent.com/example/widgets/${fix}/greet.ts`,
    content: expect.stringMatching(/^([A-Za-z0-9+/=]{1,60}\n)+$/),
  });
  expect(read([...raw(fix).slice(0, 2), "--jq", '.content | gsub("\\n"; "") | @base64d']).out).toBe(
    `${bytes(fix)}\n`
  );
  // A directory is the list of what it holds; the branch, the default branch with no ref.
  expect(
    read(["api", "repos/example/widgets/contents/docs?ref=legion/LWEVAL-1", "--jq", ".[].path"])
  ).toEqual({ code: 0, out: "docs/cli.md\n", err: "" });
  const base = git(path.join(dir, "remote.git"), "rev-parse", "main:docs/cli.md");
  expect(base).not.toBe(git(path.join(dir, "remote.git"), "rev-parse", `${head}:docs/cli.md`));
  expect(
    read(["api", "repos/example/widgets/contents/docs/cli.md", "--jq", ".sha"]).out.trim()
  ).toBe(base);
  expect(read(raw("0123456789abcdef0123456789abcdef01234567"))).toEqual({
    code: 1,
    out: "",
    err: "gh: No commit found for the ref 0123456789abcdef0123456789abcdef01234567 (HTTP 404)\n",
  });
  expect(read(raw(fix, "nothing.ts"))).toEqual({
    code: 1,
    // This row reads stdout as gh wrote it, which ends in the body's `}` with no line end.
    out: notFoundOf("https://docs.github.com/rest/repos/contents#get-repository-content"),
    err: "gh: Not Found (HTTP 404)\n",
  });
  const route = worldRouteOf(routeOf(raw(head))?.key ?? "")?.route ?? "";
  expect({ route, evidence: ROUTE_EVIDENCE[route] }).toMatchObject({
    route: "GET repos/{owner}/{repo}/contents/{path+}",
    evidence: { run: "not-counted-r24/threads-closed-text-fixes-head-3/calls.jsonl:16" },
  });
}, 60_000);

// GitHub reads a contents call that names no `ref` on the repository's default branch, and
// `--jq .content` prints the base64 of the file the caller then decodes. Measured by the tester's
// cross-check at 19e19657: of forty runs, two were unhealthy on a refused contents read, the
// second `gh api repos/example/widgets/contents/.github/workflows/greet.yml --jq .content`, no
// `ref` at all. The world's workflow is the change's own, on the issue branch and not yet on the
// default branch, so that call is GitHub's 404 there, as it would be on GitHub, and the same read
// at the head (`?ref=<head>`) answers the workflow; a file the default branch holds answers that
// branch's bytes, not the head's. Catches: a no-ref read refused, or answered from the head or
// from no branch at all; and a `content` that is not the base64 of the file.
test("a contents read with no ref answers the default branch's file, and .content is its base64", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const remote = path.join(dir, "remote.git");
  const content = (endpoint: string) =>
    ghCall(dir, "api", `repos/example/widgets/contents/${endpoint}`, "--jq", ".content");
  const decoded = (read: { out: string }) => Buffer.from(read.out, "base64").toString();
  const shown = (ref: string, file: string) =>
    spawnChecked(["git", "-C", remote, "show", `${ref}:${file}`]).stdout.toString();
  const workflow = content(".github/workflows/greet.yml");
  expect(workflow).toEqual({
    code: 1,
    out: notFoundOf("https://docs.github.com/rest/repos/contents#get-repository-content"),
    err: "gh: Not Found (HTTP 404)\n",
  });
  const atHead = content(`.github/workflows/greet.yml?ref=${head}`);
  expect([atHead.code, decoded(atHead)]).toEqual([0, shown(head, ".github/workflows/greet.yml")]);
  const docs = content("docs/cli.md");
  expect([docs.code, decoded(docs)]).toEqual([0, shown("main", "docs/cli.md")]);
  expect(decoded(docs)).not.toBe(shown(head, "docs/cli.md"));
}, 60_000);

// GitHub holds a head whose base requires conversation resolution `BLOCKED` while any review
// thread is open, and reports it `CLEAN` once the run resolves the last one; a thread the run
// opens holds it again. A base that requires no resolution is moved by no thread. The review
// world's head is approved first: before an approval its base's approval rule holds it `BLOCKED`
// whatever its threads (INFRA-764's ruling of 2026-11-18). Catches: a
// merge state frozen into the world, which read `BLOCKED` however many threads a run resolved —
// the one state of this change's subject a run can observe, an approval left with threads open
// and an implementer that resolves them — and one that blocks a base that requires nothing. The
// reads are the merge gate's (`gh pr view <n> --repo <o>/<r> --json mergeable,mergeStateStatus`),
// the REST `mergeable_state` and merger.md's GraphQL query; the resolve is the recorded
// reviewer-text-findings-text-only-head-1/calls.jsonl:29. The daemon stand-in moves the record on
// the same state (daemon-standin.test.ts, "an approval waits in reviewing while a thread …").
test("the merge state follows the threads: BLOCKED until the last one the base requires resolved is", () => {
  const states = (dir: string) => [
    ghCall(
      dir,
      ...["pr", "view", "1001", "--repo", "example/widgets"],
      ...["--json", "mergeable,mergeStateStatus", "--jq", ".mergeStateStatus"]
    ).out,
    ghCall(dir, "api", "repos/example/widgets/pulls/1001", "--jq", ".mergeable_state").out,
    ghCall(
      dir,
      "api",
      "graphql",
      "-f",
      "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){mergeStateStatus}}}",
      "-f",
      "owner=example",
      "-f",
      "name=widgets",
      "-F",
      "number=1001",
      "--jq",
      ".data.repository.pullRequest.mergeStateStatus"
    ).out,
  ];
  const resolve = (dir: string, id: string) =>
    ghCall(
      dir,
      "api",
      "graphql",
      "-f",
      "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
      "-F",
      `id=${id}`
    ).code;
  const comment = (dir: string) => {
    const input = path.join(dir, "comment-review.json");
    writeFileSync(
      input,
      JSON.stringify({
        commit_id: builtHead(dir),
        event: "COMMENT",
        body: "one note",
        comments: [{ path: "docs/cli.md", line: 5, side: "RIGHT", body: "a line note" }],
      })
    );
    return ghCall(
      dir,
      ...["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews", "--input", input]
    ).code;
  };
  const blocked = ["BLOCKED", "blocked", "BLOCKED"];
  const clean = ["CLEAN", "clean", "CLEAN"];
  const guarded = worldDir("review_world");
  expect(
    ghCall(
      guarded,
      ...["api", "--method", "POST", REVIEWS, "-f", `commit_id=${builtHead(guarded)}`],
      ...["-f", "event=APPROVE", "-f", "body=approved"]
    ).code
  ).toBe(0);
  expect(states(guarded)).toEqual(blocked);
  expect(resolve(guarded, "PRRT_kwDOrig0001")).toBe(0);
  expect(states(guarded)).toEqual(clean);
  expect(comment(guarded)).toBe(0);
  expect(states(guarded)).toEqual(blocked);
  expect(resolve(guarded, "PRRT_kwDOAAAAAc4AAD9001")).toBe(0);
  expect(states(guarded)).toEqual(clean);
  // A base that requires no resolution: its open thread holds nothing, nor does a new one.
  const open = worldDir("approved_world", "dropped", { worldCase: "code-defect" });
  expect(states(open)).toEqual(clean);
  expect(comment(open)).toBe(0);
  expect(states(open)).toEqual(clean);
}, 120_000);

// GitHub answers the base branch's rulesets with the checks it requires, and a head's check runs
// as runs of that head. Measured in INFRA-764 round 24: "Head checks green (`test`, `lint` both
// success); no required-check config in this rig" (reviewer-text-findings-text-only-base-3), "the
// rig doesn't require any specific checks" (-defect-base-3), and a reviewer that read its own
// push's checks four times to see "whether CI goes green, since a local rig might never report
// checks back" (-defect-base-4; reviewer-text-findings-defect-base-1/calls.jsonl:23 to :26 in
// the preflight). The reads are the merge gate's, as recorded at
// reviewer-text-findings-defect-base-1/calls.jsonl:18, :20 and :21. A ruleset alone protects every
// world's base, as production's (production-shapes.json), so the rulesets read names the checks
// and the branch's classic protection names none. Catches: a base that names no required check
// at the rulesets read, which a reviewer that reads the rulesets alone takes for a base requiring
// nothing; a base that requires review threads resolved in a world that requires no more than
// its checks; check runs that name no head, sit on no page of GitHub's and ran ten months
// before the commit they report on, read at the world's head and at the head a push made; and a
// head with no check of the tool the base's code-scanning rule names, or one GitHub Actions
// reported, where production's heads carry GitHub Advanced Security's `CodeQL` check with its
// title and summary, linked to GitHub's own page of the run (measured on 2026-11-18, #4346).
test("the base requires the head's checks, and the head's checks report on that head as GitHub reports them", () => {
  const rules = [
    "api",
    "--paginate",
    "repos/example/widgets/rules/branches/main",
    "--jq",
    '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context',
  ];
  const protection = [
    "api",
    "repos/example/widgets/branches/main",
    "--jq",
    ".protection.required_status_checks.checks[].context",
  ];
  const resolution = [
    "api",
    "repos/example/widgets/rules/branches/main",
    "--jq",
    '[.[] | select(.type=="pull_request") | .parameters.required_review_thread_resolution] | @json',
  ];
  const tools = [
    "api",
    "repos/example/widgets/rules/branches/main",
    "--jq",
    '.[] | select(.type=="code_scanning") | .parameters.code_scanning_tools[].tool',
  ];
  for (const [world, guard, worldCase] of [
    ["tester_proof_world", "", ""],
    ["approved_world", "dropped", "code-defect"],
    ["review_world", "", ""],
  ] as const) {
    const dir = worldDir(world, guard, { worldCase });
    expect([world, ghCall(dir, ...rules), ghCall(dir, ...protection)]).toEqual([
      world,
      { code: 0, out: "test\nlint", err: "" },
      { code: 0, out: "", err: "" },
    ]);
    expect(ghCall(dir, ...resolution).out).toBe(JSON.stringify([world === "review_world"]));
    expect(ghCall(dir, ...tools)).toEqual({ code: 0, out: "CodeQL", err: "" });
  }
  const dir = worldDir("approved_world", "dropped", { worldCase: "code-defect" });
  const head = builtHead(dir);
  const runsOf = (sha: string) =>
    ghCall(
      dir,
      ...["api", `repos/example/widgets/commits/${sha}/check-runs`, "--paginate"],
      ...["--jq", ".check_runs[] | [.name, .status, .conclusion] | @tsv"]
    );
  // Each run as GitHub reports it: on the commit read, on GitHub's page of it, with the app's
  // own link (GitHub's page itself for the code-scanning tool's check), started no earlier than
  // the commit was made and ended after it started, by its App, with what that App wrote.
  const reportOf = (sha: string) => {
    const made = git(path.join(dir, "remote.git"), "show", "-s", "--format=%ct", sha);
    const facts = `[.check_runs[] | [.head_sha, .html_url, .details_url, (.started_at | fromdateiso8601) >= ${made}, (.completed_at | fromdateiso8601) > (.started_at | fromdateiso8601), .app.slug, .app.id, .output.title, .details_url == .html_url]]`;
    const read = ghCall(
      dir,
      "api",
      `repos/example/widgets/commits/${sha}/check-runs`,
      "--jq",
      facts
    );
    return [read.code, JSON.parse(read.out)];
  };
  const page = expect.stringMatching(/^https:\/\/github\.com\/example\/widgets\/runs\/\d+$/);
  for (const sha of [head, pushFix(dir, "docs/cli.md", "the text fix\n")]) {
    expect(runsOf(sha)).toEqual({
      code: 0,
      out: "test\tcompleted\tsuccess\nlint\tcompleted\tsuccess\nCodeQL\tcompleted\tsuccess",
      err: "",
    });
    const scanned = "No new alerts in code changed by this pull request";
    expect(reportOf(sha)).toEqual([
      0,
      [
        [sha, page, "https://example.invalid/2", true, true, "github-actions", 15368, null, false],
        [sha, page, "https://example.invalid/1", true, true, "github-actions", 15368, null, false],
        [sha, page, page, true, true, "github-advanced-security", 57789, scanned, true],
      ],
    ]);
  }
}, 120_000);

// The reads of a base branch a ruleset alone protects, each held to production's own answer to
// it: production-shapes.json, which production-shapes.ts derives from the reads of
// cybermonklabs/legion's `main` made from a Legion issue pod with the role App token its gh gives
// (at the pin's `read_at`). Each row makes production's own command with the world's values in
// it (`argvOf`) and compares the world's answer with the pin — its exit, its stderr and its shape
// (`shapeDiff`) — and then the world's own values in it. The rows build a world whose base
// requires its checks alone (tester_proof_world) and one whose base requires review threads
// resolved too (review_world, rig.sh's resolution_protection_routes).

// Catches: a shape that reads a list's variants as one (a `deletion` rule's lone `type` and a
// `pull_request` rule's parameters under one `[]`), or one list of items per variant where the
// items share a shape; and a comparison that reports a path below one whose kind differs, below
// an array the world holds empty or a variant of a list it holds none of, or each path under a
// missing one rather than that one.
test("a shape reads a list per variant where its items differ, and a difference names the highest path", () => {
  const rules = [
    { type: "deletion" },
    { type: "pull_request", parameters: { count: 1, methods: ["squash"] } },
  ];
  expect(shapeOf(rules)).toEqual({
    ".": "array",
    ".[type=deletion]": "object",
    ".[type=deletion].type": "string",
    ".[type=pull_request]": "object",
    ".[type=pull_request].type": "string",
    ".[type=pull_request].parameters": "object",
    ".[type=pull_request].parameters.count": "number",
    ".[type=pull_request].parameters.methods": "array",
    ".[type=pull_request].parameters.methods[]": "string",
  });
  expect(
    shapeOf([
      { type: "a", n: 1 },
      { type: "b", n: 2 },
    ])
  ).toEqual({ ".": "array", ".[]": "object", ".[].type": "string", ".[].n": "number" });
  const read = {
    shape: shapeOf({
      commit: { sha: "s", author: { login: "l" } },
      parents: [{ sha: "p" }],
      rules,
      more: 1,
    }),
  };
  const world = {
    commit: { author: null },
    parents: [],
    rules: [{ type: "pull_request", parameters: { count: "1", methods: [] } }],
    extra: { a: 1 },
  };
  expect(shapeDiff(world, read)).toEqual({
    missing: [".commit.sha", ".more"],
    extra: [".extra"],
    kinds: [
      ".commit.author: object in production, null in the world",
      ".rules[type=pull_request].parameters.count: number in production, string in the world",
    ],
  });
});

// Catches: a pin that keeps a value of production's reads — a repository, a pull request's
// number, a head, a review's or a comment's id, a file's path, a ruleset's id in a read's name —
// and a derivation that writes a value it was not told of into the pin rather than refusing to.
// The reads here are this row's own, in the two layouts of the production reads INFRA-764 made.
test("the pin is derived with production's values as placeholders, and refused where one would stay", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "production-shapes-"));
  made.push(dir);
  const rulesets = path.join(dir, "rulesets");
  const prs = path.join(dir, "prs");
  mkdirSync(rulesets);
  mkdirSync(prs);
  writeFileSync(
    path.join(rulesets, "README.txt"),
    "Reads of a branch, read at 2026-01-02T03:04:05Z.\n"
  );
  for (const [name, answer, err, exit] of [
    ["rulesets", [{ id: 4242, name: "main" }], "", 0],
    ["ruleset-4242", { id: 4242, name: "main", rules: [{ type: "deletion" }] }, "", 0],
    [
      "branches-main-protection",
      { message: "Resource not accessible by integration" },
      "gh: x\n",
      1,
    ],
  ] as const) {
    writeFileSync(path.join(rulesets, `${name}.json`), JSON.stringify(answer));
    writeFileSync(path.join(rulesets, `${name}.err`), err);
    writeFileSync(path.join(rulesets, `${name}.exit`), `${exit}\n`);
  }
  writeFileSync(
    path.join(prs, "README.txt"),
    "Reads of a pull request (read 2026-01-02 about 03:00Z; cut).\n"
  );
  const [head, base] = ["a".repeat(40), "b".repeat(40)];
  const reads = {
    substitutions: {
      repo: "example/widgets -> octo-org/octo-repo",
      pr: "1001 -> 31337",
      head,
      base,
      review: "777777777",
      comment: "888888888",
    },
    reads: [
      ["repos/octo-org/octo-repo/pulls/31337/reviews/777777777/comments"],
      [`repos/octo-org/octo-repo/compare/${base}...${head}`],
      [`repos/octo-org/octo-repo/contents/src/app.ts?ref=${head}`],
      ["graphql", "-f", 'query=query{repository(owner:"octo-org",name:"octo-repo"){id}}'],
      ["graphql", "-F", "o=octo-org", "-F", "r=octo-repo", "-F", "n=31337"],
      ["repos/octo-org/octo-repo/pulls/31337/comments/888888888/replies"],
    ].map((argv, index) => ({
      kind: "gh api GET",
      key: `read-${index}`,
      command: ["gh", "api", ...argv],
      exit: 0,
      stderr: "",
      answer: { id: 777777777, title: "a title of the pull request" },
    })),
  };
  const file = path.join(prs, "reads.json");
  writeFileSync(file, JSON.stringify(reads));
  expect(
    derive(rulesets, file).reads.map(({ name, command, read_at }) => [name, command, read_at])
  ).toEqual([
    [
      "branches-main-protection",
      ["gh", "api", "repos/<owner>/<repo>/branches/main/protection"],
      "2026-01-02T03:04:05Z",
    ],
    ["ruleset-<id>", ["gh", "api", "repos/<owner>/<repo>/rulesets/<id>"], "2026-01-02T03:04:05Z"],
    ["rulesets", ["gh", "api", "repos/<owner>/<repo>/rulesets"], "2026-01-02T03:04:05Z"],
    ...[
      ["repos/<owner>/<repo>/pulls/<number>/reviews/<review>/comments"],
      ["repos/<owner>/<repo>/compare/<base>...<head>"],
      ["repos/<owner>/<repo>/contents/<path>?ref=<head>"],
      ["graphql", "-f", 'query=query{repository(owner:"<owner>",name:"<repo>"){id}}'],
      ["graphql", "-F", "o=<owner>", "-F", "r=<repo>", "-F", "n=<number>"],
      ["repos/<owner>/<repo>/pulls/<number>/comments/<comment>/replies"],
    ].map((argv, index) => [
      `gh api GET read-${index}`,
      ["gh", "api", ...argv],
      "2026-01-02 about 03:00Z",
    ]),
  ]);
  // A value of an answer, which no substitution names, written into a command.
  reads.reads[0]?.command.push("a title of the pull request");
  writeFileSync(file, JSON.stringify(reads));
  expect(() => derive(rulesets, file)).toThrow(
    'the pin would hold production\'s values: "a title of the pull request"'
  );
});

// Catches: a pin of production's `read_record` that keeps a name of the one issue it was read
// from — the roles that issue had run, the pairs of phases it had moved between, which would
// hold every world to that issue's history — or a value of it that the record uses as a key (a
// map by issue key or by session), which is how a shape, holding no value, can still carry one.
test("the record is pinned by its shape with its roles and moves as lists, and refused where a key is a value", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "production-shapes-"));
  made.push(dir);
  const rulesets = path.join(dir, "rulesets");
  const prs = path.join(dir, "prs");
  mkdirSync(rulesets);
  mkdirSync(prs);
  writeFileSync(path.join(rulesets, "README.txt"), "Reads, read at 2026-01-02T03:04:05Z.\n");
  writeFileSync(path.join(rulesets, "rulesets.json"), "[]");
  writeFileSync(path.join(rulesets, "rulesets.err"), "");
  writeFileSync(path.join(rulesets, "rulesets.exit"), "0\n");
  writeFileSync(path.join(prs, "README.txt"), "Reads (read 2026-01-02 about 03:00Z; cut).\n");
  const reads = path.join(prs, "reads.json");
  // Production values no word of a pin spells, so the derivation's own check passes them.
  const substitutions = {
    repo: "octo-org/octo-repo",
    pr: "31337",
    head: "a".repeat(40),
    base: "b".repeat(40),
    review: "777777777",
    comment: "888888888",
  };
  writeFileSync(reads, JSON.stringify({ substitutions, reads: [] }));
  const record = {
    key: "OCTO-31337",
    workers: {
      implementer: { pass: 2, backwardMoves: { "reviewing-implementing": 1 } },
      reviewer: { pass: 0, claim: { session: "0190a1b2-0000-7000-8000-000000000001" } },
    },
  };
  const file = path.join(dir, "record.json");
  const note = "One read_record, read 2026-01-02T03:04:05Z from an issue pod.";
  writeFileSync(file, JSON.stringify({ note, record }));
  expect(derive(rulesets, reads, file).reads.at(-1)).toEqual({
    name: RECORD_READ,
    command: [RECORD_READ],
    read_at: "2026-01-02T03:04:05Z",
    exit: 0,
    stderr: "",
    shape: {
      ".": "object",
      ".key": "string",
      ".workers": "array",
      ".workers[]": "object",
      ".workers[].pass": "number",
      ".workers[].backwardMoves": "array",
      ".workers[].backwardMoves[]": "number",
      ".workers[].claim": "object",
      ".workers[].claim.session": "string",
    },
  });
  // A map the record keys by one of its own values: the issue's key.
  writeFileSync(
    file,
    JSON.stringify({ note, record: { children: { "OCTO-31338": { pass: 1 } } } })
  );
  expect(() => derive(rulesets, reads, file)).toThrow(
    "the pin would hold production's values: .children.OCTO-31338, .children.OCTO-31338.pass"
  );
  // A record whose note says nothing of when it was read is no dated read.
  writeFileSync(file, JSON.stringify({ note: "One read_record.", record }));
  expect(() => derive(rulesets, reads, file)).toThrow("names no time it was read");
});

// A text answer is shaped line by line (production-shapes.ts's `lineShapes`), each layout by its
// command, so that an empty or a wrong-format text no longer passes a pin that held it to the
// kind `string` alone. Catches: a body line read as a `<key>:\t` line or a `--` inside a body
// read as the view's own; a comment entry after a body not read as one; rows counted by number
// rather than read as one shape; a value kept in a shape (a key's value, a host, a login, a
// path, a line of a diff); a line no rule reads given a shape that says nothing of it; and a
// comparison that passes an empty text or a wrong-format one.
test("a text answer is shaped line by line, values as placeholders, and compared as a set of shapes", () => {
  const view = ["gh", "pr", "view", "<number>"];
  expect(
    lineShapes("title:\tA title\nlabels:\t\n--\nThe body.\nnot:\ta key\n--\nstill the body\n", view)
  ).toEqual(["title:\t<value>", "labels:\t<value>", "--", "<body>"]);
  expect(
    lineShapes(
      "author:\ta\nedited:\tfalse\n--\nOne.\n--\nauthor:\tb\nedited:\ttrue\n--\nTwo.\n--\n",
      [...view, "--comments"]
    )
  ).toEqual(["author:\t<value>", "edited:\t<value>", "--", "<body>"]);
  expect(
    lineShapes("1\tA\tb\tOPEN\tt\n2\tB\tc\tOPEN\tt\n", ["gh", "pr", "list", "--limit", "3"])
  ).toEqual(["<5 tab-separated fields>"]);
  expect(
    lineShapes("c\tpass\t8s\thttps://x\t\nlone\n", ["gh", "pr", "checks", "<number>"])
  ).toEqual(["<5 tab-separated fields>", "<unread line>"]);
  expect(lineShapes("a.ts\nb/c.ts\n", ["gh", "pr", "diff", "<number>", "--name-only"])).toEqual([
    "<path>",
  ]);
  expect(
    lineShapes(
      "github.com\n  ✓ Logged in to github.com account someone (/a/hosts.yml)\n  - Token: ghs_****\n",
      ["gh", "auth", "status"]
    )
  ).toEqual(["<host>", "  ✓ Logged in to <host> account <login> (<source>)", "  - Token: <value>"]);
  expect(
    lineShapes(
      [
        "diff --git a/x.ts b/x.ts",
        "new file mode 100644",
        "index 0000000..1234567",
        "--- /dev/null",
        "+++ b/x.ts",
        "@@ -0,0 +1,2 @@ context",
        "+one",
        "+two",
        "\\ No newline at end of file",
        "",
      ].join("\n"),
      ["gh", "pr", "diff", "<number>"]
    )
  ).toEqual([
    "diff --git a/<path> b/<path>",
    "new file mode <mode>",
    "index <blob>..<blob>",
    "--- <path>",
    "+++ <path>",
    "@@ -<line> +<line> @@",
    "+<text>",
    "\\ No newline at end of file",
  ]);
  const read = {
    shape: { ".": "string" },
    command: view,
    lines: ["title:\t<value>", "--", "<body>"],
  };
  expect(shapeDiff("title:\tx\n--\nbody\n", read)).toEqual(SAME_SHAPE);
  expect(shapeDiff("", read)).toEqual({
    missing: ["line title:\t<value>", "line --", "line <body>"],
    extra: [],
    kinds: [],
  });
  expect(shapeDiff("title:\tx\nstate:\tOPEN\n--\nbody\n", read)).toEqual({
    missing: [],
    extra: ["line state:\t<value>"],
    kinds: [],
  });
});

// Catches: a pin that keeps a text answer's kind alone, with no line shapes; a reading cut short
// pinned at all — one its set's README says was cut, or one of 600 characters with no line end,
// the length round 25's store cut at — rather than refused, naming it; a reading again whole (a
// later set's read with no `item` of the same command, a `--repo` naming the set's repository
// aside) that does not replace the cut one, or replaces it under another name, or the replaced
// reading's when and lines kept; a read of its own (one with an `item`) taken for a reading
// again; a line no rule reads pinned as a shape that says nothing of it; and a line shape that
// keeps a value of production's (a key that is the repository's owner), which the derivation's
// leak check refuses as it refuses any other.
test("the pin keeps a text answer's line shapes from a whole reading, refusing one cut short, an unread line or a value in one", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "production-shapes-"));
  made.push(dir);
  const rulesets = path.join(dir, "rulesets");
  const prs = path.join(dir, "prs");
  mkdirSync(rulesets);
  mkdirSync(prs);
  writeFileSync(path.join(rulesets, "README.txt"), "Reads, read at 2026-01-02T03:04:05Z.\n");
  writeFileSync(path.join(rulesets, "rulesets.json"), "[]");
  writeFileSync(path.join(rulesets, "rulesets.err"), "");
  writeFileSync(path.join(rulesets, "rulesets.exit"), "0\n");
  writeFileSync(
    path.join(prs, "README.txt"),
    "Reads (read 2026-01-02 about 03:00Z; text answers cut at 40 characters).\n"
  );
  const reads = path.join(prs, "reads.json");
  const substitutions = {
    repo: "octo-org/octo-repo",
    pr: "31337",
    head: "a".repeat(40),
    base: "b".repeat(40),
    review: "777777777",
    comment: "888888888",
  };
  const write = (answers: Record<string, [string[], string]>) =>
    writeFileSync(
      reads,
      JSON.stringify({
        substitutions,
        reads: Object.entries(answers).map(([key, [flags, answer]]) => ({
          kind: "gh pr view",
          key,
          command: ["gh", "pr", "view", "31337", ...flags],
          exit: 0,
          stderr: "",
          answer,
        })),
      })
    );
  // Forty characters or more, the length its README says its text answers were cut at: cut, in
  // its fourth line, whose `lab` no rule reads.
  write({
    whole: [[], "title:\tAn issue\n--\nOne.\n"],
    cut: [["--comments"], "author:\tsomeone\nassociation:\tnone\nedited:\tfalse\nlab"],
  });
  const refusal = (name: string, length: number) =>
    `${name}'s answer is cut short at ${length} characters, so lines of it were never read: read it again whole`;
  expect(() => derive(rulesets, reads)).toThrow(refusal("gh pr view cut", 51));
  // The same read read again whole, through `--repo`, as the tester read #2827's views.
  const later = path.join(prs, "later.json");
  const again = {
    name: "pr view --comments (text)",
    command: ["gh", "pr", "view", "31337", "--repo", "octo-org/octo-repo", "--comments"],
    read_at: "2026-01-03T04:05:06Z",
    exit: 0,
    stderr: "",
    stdout: "author:\tsomeone\nassociation:\tnone\nedited:\tfalse\nstatus:\tnone\n--\nOne.\n--\n",
  };
  const readAgain = (...readings: object[]) =>
    writeFileSync(
      later,
      JSON.stringify({
        source: "readings again",
        repo: "octo-org/octo-repo",
        pull_requests: { open: "31337" },
        reads: readings,
      })
    );
  readAgain(again);
  expect(
    derive(rulesets, reads, undefined, [later]).reads.map(({ name, command, read_at, lines }) => [
      name,
      command.join(" "),
      read_at,
      lines,
    ])
  ).toEqual([
    ["rulesets", "gh api repos/<owner>/<repo>/rulesets", "2026-01-02T03:04:05Z", undefined],
    [
      "gh pr view whole",
      "gh pr view <number>",
      "2026-01-02 about 03:00Z",
      ["title:\t<value>", "--", "<body>"],
    ],
    [
      "gh pr view cut",
      "gh pr view <number> --repo <owner>/<repo> --comments",
      "2026-01-03T04:05:06Z",
      [
        "author:\t<value>",
        "association:\t<value>",
        "edited:\t<value>",
        "status:\t<value>",
        "--",
        "<body>",
      ],
    ],
  ]);
  // A read of its own, of the unpinned item it names, replaces no reading: the cut one stands.
  const { stdout, ...own } = again;
  readAgain({ ...own, item: "1", answer: stdout });
  expect(() => derive(rulesets, reads, undefined, [later])).toThrow(refusal("gh pr view cut", 51));
  // A text of 600 characters with no line end is cut short whatever set holds it, here a read of
  // its own; with its line end it is whole.
  const long = (text: string) => ({ ...own, name: "pr view (long)", item: "2", answer: text });
  readAgain(again, long(`title:\t${"x".repeat(593)}`));
  expect(() => derive(rulesets, reads, undefined, [later])).toThrow(
    refusal("gh pr view (long)", 600)
  );
  readAgain(again, long(`title:\t${"x".repeat(592)}\n`));
  expect(derive(rulesets, reads, undefined, [later]).reads.at(-1)?.lines).toEqual([
    "title:\t<value>",
  ]);
  write({ unread: [[], "Title Of It\n"] });
  expect(() => derive(rulesets, reads)).toThrow(
    "a line of gh pr view unread's answer is in no shape its layout reads (<unread line>)"
  );
  write({ valued: [[], "octo-org:\tx\n"] });
  expect(() => derive(rulesets, reads)).toThrow("the pin would hold production's values: octo-org");
});
/** A shape no different from production's. */
const SAME_SHAPE = { missing: [], extra: [], kinds: [] };
/** What the world under `dir` answers `read`'s own command with, the world's repository and
 * `values` in its placeholders: its exit, stdout and stderr, and the stdout read as the pin keeps
 * production's (`answerOf`). */
function pinnedCall(dir: string, read: PinnedRead, values: Record<string, string> = {}) {
  const got = ghRaw(dir, ...argvOf(read, { owner: "example", repo: "widgets", ...values }));
  return { ...got, answer: answerOf(got.out) };
}
/** A gh call's stdout as the pin keeps production's answer: the JSON it holds, or its text where
 * it holds none (gh's text views, a diff, a refusal's empty stdout), whose shape is a string's. */
function answerOf(out: string): unknown {
  try {
    return JSON.parse(out);
  } catch {
    return out;
  }
}
/** The pages a `gh api --paginate` call printed back to back (`{…}{…}`, gh's form of a paged
 * GraphQL answer), as a list: the form production's paged answers are pinned in, each page one
 * item. */
function pagesOf(out: string): unknown[] {
  const pages: unknown[] = [];
  let [depth, start, quoted, escaped] = [0, 0, false, false];
  for (let at = 0; at < out.length; at += 1) {
    const character = out[at];
    if (quoted) {
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') quoted = false;
    } else if (character === '"') quoted = true;
    else if (character === "{" || character === "[") depth += 1;
    else if ((character === "}" || character === "]") && --depth === 0) {
      pages.push(JSON.parse(out.slice(start, at + 1)));
      start = at + 1;
    }
  }
  return pages;
}
/** The worlds whose base's rules the rows below read, each a world function and, after a space,
 * the case it is built for (`worldDir`'s `worldCase`). */
const RULED_WORLDS = ["tester_proof_world", "review_world", "review_world last-push"] as const;
type RuledWorld = (typeof RULED_WORLDS)[number];
/** The world `world` names, built (`worldDir`). */
function ruledWorldDir(world: RuledWorld): string {
  const [name = "", worldCase = ""] = world.split(" ");
  return worldDir(name, "", { worldCase });
}
/** The rules each world's one ruleset holds (rig.sh's base_ruleset_routes): production's seven, in
 * production's order, at production's values (cybermonklabs/legion ruleset 12331919, read on
 * 2026-11-19T05:35:00Z: one approval, no approval dismissed on a push and none asked of the last
 * push, a code owner's review required, as it has been since 2026-11-18T18:19:22Z, an
 * extra approval for Copilot's unattributed changes, squash the one merge method, its merge
 * queue's settings and its code-scanning thresholds), but for review threads resolved in the
 * review round's worlds alone (resolution_protection_routes), the last push's approval asked of
 * another in last-push's world alone (review_world's case), the checks GitHub Actions reports on
 * the world's head (integration 15368) and the tool whose check that head reports, `CodeQL`. */
function rulesOf(world: RuledWorld): object[] {
  return [
    { type: "deletion" },
    { type: "non_fast_forward" },
    { type: "required_linear_history" },
    {
      type: "pull_request",
      parameters: {
        required_approving_review_count: 1,
        dismiss_stale_reviews_on_push: false,
        required_reviewers: [],
        require_code_owner_review: true,
        dismissal_restriction: { enabled: false, allowed_actors: [] },
        require_last_push_approval: world === "review_world last-push",
        required_review_thread_resolution: world !== "tester_proof_world",
        require_extra_approval_for_unattributed_changes: true,
        allowed_merge_methods: ["squash"],
      },
    },
    {
      type: "required_status_checks",
      parameters: {
        strict_required_status_checks_policy: false,
        do_not_enforce_on_create: false,
        required_status_checks: [
          { context: "test", integration_id: 15368 },
          { context: "lint", integration_id: 15368 },
        ],
      },
    },
    {
      type: "merge_queue",
      parameters: {
        merge_method: "SQUASH",
        max_entries_to_build: 5,
        min_entries_to_merge: 1,
        max_entries_to_merge: 4,
        min_entries_to_merge_wait_minutes: 0,
        grouping_strategy: "ALLGREEN",
        check_response_timeout_minutes: 180,
      },
    },
    {
      type: "code_scanning",
      parameters: {
        code_scanning_tools: [
          { tool: "CodeQL", security_alerts_threshold: "high_or_higher", alerts_threshold: "none" },
        ],
      },
    },
  ];
}
/** The rule types production's answer to `read` holds in its list at `list` (`.` the answer, `.rules`
 * a ruleset's), in production's order: the variants the pin reads that list by. */
function pinnedTypes(read: PinnedRead, list: "." | ".rules"): string[] {
  const variant = new RegExp(`^${list === "." ? "\\." : "\\.rules"}\\[type=(\\w+)\\]$`);
  return Object.keys(read.shape).flatMap((at) => variant.exec(at)?.[1] ?? []);
}
/** The `type` of each rule of `rules`, a list of rules as a world answered it. */
function ruleTypes(rules: unknown): unknown[] {
  return Array.isArray(rules) ? rules.map((rule) => rule?.type) : [];
}

// Catches: the rules read in a shape GitHub never gives — a rule without the ruleset it comes
// from, a parameter in another kind or of another key — and a base whose rules name no required
// check, which a reviewer that reads the rulesets alone takes for a base requiring none: the
// classic protection named the checks and the rules named nothing (INFRA-764's ruling of
// 2026-11-17). Each of production's seven rules is held to production's rule of its type, and none
// may be absent: a world holding two of them passed the shape row, since a variant a list holds
// none of is compared no further (INFRA-764's ruling of 2026-11-18). Each world's rules are its
// ruleset's, the thread-resolution rule's `true` the review round's worlds' alone, and the last
// push's approval asked of another (`require_last_push_approval: true`) last-push's alone.
test("the base branch's rules answer as production's, naming the checks every base requires", () => {
  const read = pinnedRead("rules-branches-main");
  expect(pinnedTypes(read, ".")).toEqual([
    "deletion",
    "non_fast_forward",
    "required_linear_history",
    "pull_request",
    "required_status_checks",
    "merge_queue",
    "code_scanning",
  ]);
  for (const world of RULED_WORLDS) {
    const got = pinnedCall(ruledWorldDir(world), read);
    expect([world, got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
      world,
      read.exit,
      read.stderr,
      SAME_SHAPE,
    ]);
    expect([world, ruleTypes(got.answer)]).toEqual([world, pinnedTypes(read, ".")]);
    const source = { ruleset_source_type: "Repository", ruleset_source: "example/widgets" };
    expect([world, got.answer]).toEqual([
      world,
      rulesOf(world).map((rule) => ({ ...rule, ...source, ruleset_id: 1 })),
    ]);
  }
}, 60_000);

// Catches: a world with no list of its rulesets, which a reviewer reaches from the `ruleset_id`
// each rule names and was refused 404 where production answers, and a list in another shape.
test("the repository's rulesets answer as production's: the one ruleset on the base", () => {
  const read = pinnedRead("rulesets");
  const got = pinnedCall(worldDir("tester_proof_world"), read);
  expect([got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
    read.exit,
    read.stderr,
    SAME_SHAPE,
  ]);
  expect(got.answer).toMatchObject([
    { id: 1, name: "main", target: "branch", source: "example/widgets", enforcement: "active" },
  ]);
}, 60_000);

// Catches: a ruleset read in another shape, one whose rules are not the rules the branch is under
// or lack one of production's, and a world that answers any id with its ruleset, where GitHub
// answers an id the repository has no ruleset of with its 404 (measured on 2026-11-17: `gh api
// repos/cybermonklabs/legion/rulesets/1`, GitHub's error body, `gh: Not Found (HTTP 404)`, exit
// 1), in the shape of every GitHub error body (the pinned 403's).
test("the base's ruleset answers as production's by its id, and GitHub's 404 for another", () => {
  const read = pinnedRead("ruleset-<id>");
  for (const world of RULED_WORLDS) {
    const dir = ruledWorldDir(world);
    const got = pinnedCall(dir, read, { id: "1" });
    expect([world, got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
      world,
      read.exit,
      read.stderr,
      SAME_SHAPE,
    ]);
    const { answer } = got;
    const rules = typeof answer === "object" && answer !== null && "rules" in answer;
    expect([world, ruleTypes(rules ? answer.rules : undefined)]).toEqual([
      world,
      pinnedTypes(read, ".rules"),
    ]);
    expect([world, got.answer]).toMatchObject([
      world,
      {
        id: 1,
        name: "main",
        conditions: { ref_name: { exclude: [], include: ["~DEFAULT_BRANCH"] } },
        rules: rulesOf(world),
      },
    ]);
    const other = pinnedCall(dir, read, { id: "2" });
    const error = pinnedRead("branches-main-protection");
    expect([world, other.code, other.err, shapeDiff(other.answer, error), other.answer]).toEqual([
      world,
      1,
      "gh: Not Found (HTTP 404)\n",
      SAME_SHAPE,
      {
        message: "Not Found",
        documentation_url: "https://docs.github.com/rest/repos/rules#get-a-repository-ruleset",
        status: "404",
      },
    ]);
  }
}, 60_000);

// Catches: the base branch answered with no `commit`, as the world answered it before, or with
// the classic protection a ruleset-protected base has not got; and a head GitHub could not give
// in production's kinds, its author and committer linked to no account and its verification
// null, as a commit by an address no account holds and one pushed unsigned are. The branch's head
// is the base's head on the run's remote, its developer's commit, signed with the key the
// developer's account holds, which the world's GitHub holds by the address the commit carries
// (rig.sh's fixture_first_commit); and the base's first commit has no parent, so its `parents`
// are compared no further.
test("the base branch answers as production's, at its head, protected by its ruleset alone", () => {
  const read = pinnedRead("branches-main");
  const dir = worldDir("tester_proof_world");
  const got = pinnedCall(dir, read);
  expect([got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
    read.exit,
    read.stderr,
    SAME_SHAPE,
  ]);
  const remote = path.join(dir, "remote.git");
  const base = git(remote, "rev-parse", "refs/heads/main");
  // The time git holds, as GitHub writes one: UTC, to the second.
  const when = (format: string) =>
    new Date(Number(git(remote, "show", "-s", `--format=${format}`, base)) * 1000)
      .toISOString()
      .replace(/\.\d{3}Z$/, "Z");
  const email = rigValue("world_author_email");
  // The developer's login: its address's local part, each dot a dash (GitHub's logins hold none).
  const login = (email.split("@")[0] ?? "").replaceAll(".", "-");
  const account = { login, type: "User", html_url: `https://github.com/${login}` };
  const identity = { name: rigValue("world_author_name"), email };
  expect(got.answer).toMatchObject({
    name: "main",
    commit: {
      sha: base,
      commit: {
        author: { ...identity, date: when("%at") },
        committer: { ...identity, date: when("%ct") },
        message: git(remote, "show", "-s", "--format=%B", base),
        tree: { sha: git(remote, "show", "-s", "--format=%T", base) },
        verification: {
          verified: true,
          reason: "valid",
          signature: expect.stringMatching(
            /^-----BEGIN SSH SIGNATURE-----\n[\s\S]+\n-----END SSH SIGNATURE-----\n$/
          ),
          payload: `${git(remote, "cat-file", "commit", base)
            .replace(/^gpgsig .*\n(?: .*\n)*/m, "")
            .trim()}\n`,
          verified_at: when("%ct"),
        },
      },
      author: account,
      committer: account,
      parents: [],
    },
    protected: true,
    protection: {
      enabled: false,
      required_status_checks: { enforcement_level: "off", contexts: [], checks: [] },
    },
    protection_url: "https://api.github.com/repos/example/widgets/branches/main/protection",
  });
}, 60_000);

// Catches: the classic protection answered as a token that may read a repository's
// administration is, where GitHub refuses a role App 403; and that 403 handed to a call's `--jq`,
// which printed jq's own error and never GitHub's, where gh prints the error body as GitHub sent
// it and the 403 line (measured on 2026-11-17: `gh api
// repos/cybermonklabs/legion/branches/main/protection --jq '.required_status_checks.contexts[]'`).
test("the base branch's protection is GitHub's 403 to a role App, --jq or none", () => {
  const read = pinnedRead("branches-main-protection");
  const dir = worldDir("tester_proof_world");
  const got = pinnedCall(dir, read);
  expect([got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
    read.exit,
    read.stderr,
    SAME_SHAPE,
  ]);
  const body = {
    message: "Resource not accessible by integration",
    documentation_url:
      "https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
    status: "403",
  };
  expect(got.answer).toEqual(body);
  const filtered = ghCall(
    dir,
    ...argvOf(read, { owner: "example", repo: "widgets" }),
    "--jq",
    ".required_status_checks.contexts[]"
  );
  expect(filtered).toEqual({ code: read.exit, out: JSON.stringify(body), err: read.stderr });
}, 60_000);

// The REST routes the world serves, each held to the fields GitHub's REST API description marks
// required in its success answer: rest-required.json, which rest-required.ts derives from
// github/rest-api-description at DESCRIPTION_COMMIT (`descriptions/api.github.com/
// api.github.com.json`) and reads an answer against as INFRA-764's read search reads it
// (`absentFields`: through `$ref`, every part of an `allOf`, the closest of a `oneOf` or an
// `anyOf`, and every item of an array). The search found the world's repository short of 79 of
// them, its pull request of 37, every user it named of 17 and every review comment of its
// `diff_hunk` (2026-11-17). github-standin.test.ts holds the HTTP stand-in's routes to the same
// extract.

// Catches: an extract that reads GitHub's description otherwise than the tester's search does —
// a `$ref` not followed, a response named by `$ref` not followed, an `allOf` read by one part, a
// `oneOf` read by its first variant where another is closer, an array's items not read or read on
// a value that is no object, a field required below an object not reached, a cycle of schemas
// followed for ever — one that keeps a schema nothing below requires or declares, which would hold
// the world to no field and swell the extract; one that names a route's operation by another path
// or status; and a route the description has no operation of written into the extract rather than
// refused, by name. And, of the key set (`keyDiff`): a field the description declares nowhere at
// its level not named extra, at the top, below a field, or in an array's items; a field another
// part of an `allOf` declares named extra; an object that takes other fields
// (`additionalProperties`) or declares none held to a key set. The description is this row's own,
// in GitHub's layout.
test("the extract keeps each served operation's required and declared fields as GitHub's description reads them", () => {
  const ref = (name: string) => ({ $ref: `#/components/schemas/${name}` });
  const json = (schema: object) => ({ content: { "application/json": { schema } } });
  const description = {
    paths: {
      "/things/{thing_id}": {
        get: { operationId: "things/get", responses: { "404": {}, "200": json(ref("thing")) } },
      },
      "/things": {
        post: {
          operationId: "things/create",
          responses: { "201": { $ref: "#/components/responses/made" } },
        },
      },
    },
    components: {
      responses: { made: json({ type: "array", items: ref("named") }) },
      schemas: {
        thing: {
          allOf: [
            ref("named"),
            {
              required: ["owner"],
              properties: {
                owner: { oneOf: [ref("user"), ref("bot")] },
                next: ref("thing"),
                note: { type: "string" },
                meta: ref("loose"),
                labels: ref("open"),
              },
            },
          ],
        },
        named: { required: ["name"], properties: { name: { type: "string" } } },
        user: { required: ["login", "id"] },
        bot: { required: ["slug"] },
        loose: { properties: { a: { type: "string" } } },
        open: { properties: { a: { type: "string" } }, additionalProperties: true },
      },
    },
  };
  const extract = deriveExtract(description, [
    { route: "GET things/{id}" },
    { route: "POST things" },
  ]);
  expect(extract.routes).toEqual({
    "GET things/{id}": "GET /things/{thing_id}",
    "POST things": "POST /things",
  });
  expect(extract.operations).toEqual({
    "GET /things/{thing_id}": {
      operation_id: "things/get",
      status: 200,
      media: "application/json",
      schema: { ref: "thing" },
    },
    "POST /things": {
      operation_id: "things/create",
      status: 201,
      media: "application/json",
      schema: { items: { ref: "named" } },
    },
  });
  expect(extract.schemas).toEqual({
    bot: { required: ["slug"] },
    loose: { keys: ["a"] },
    named: { required: ["name"], keys: ["name"] },
    thing: {
      allOf: [
        { ref: "named" },
        {
          required: ["owner"],
          keys: ["owner", "next", "note", "meta", "labels"],
          properties: {
            owner: { oneOf: [{ ref: "user" }, { ref: "bot" }] },
            next: { ref: "thing" },
            meta: { ref: "loose" },
          },
        },
      ],
    },
    user: { required: ["login", "id"] },
  });
  const absent = (value: unknown, key: string) =>
    absentFields(value, extract.operations[key]?.schema ?? {}, extract.schemas);
  const get = "GET /things/{thing_id}";
  expect(absent({ name: "a", owner: { slug: "b" }, next: { owner: { login: "c" } } }, get)).toEqual(
    [".next.name", ".next.owner.id"]
  );
  expect(absent({}, get)).toEqual([".name", ".owner"]);
  expect(absent([{ name: "a" }, {}, "x"], "POST /things")).toEqual(["[].name"]);
  const keys = (value: unknown, key: string) =>
    keyDiff(value, extract.operations[key]?.schema ?? {}, extract.schemas);
  expect(
    keys(
      {
        name: "a",
        owner: { slug: "b", more: 1 },
        meta: { a: "x", b: 2 },
        labels: { c: 3 },
        stray: 1,
      },
      get
    )
  ).toEqual({ missing: [], extra: [".stray", ".meta.b"] });
  expect(keys({ next: { name: "n", owner: { slug: "s" }, other: 1 } }, get)).toEqual({
    missing: [".name", ".owner"],
    extra: [".next.other"],
  });
  expect(keys([{ name: "a", x: 1 }, { name: "b" }], "POST /things")).toEqual({
    missing: [],
    extra: ["[].x"],
  });
  expect(() => deriveExtract(description, [{ route: "GET nothing/{id}" }])).toThrow(
    "no operation of the description is GET nothing/{id}"
  );
});

/** One call of each REST route of WORLD_ROUTES, in the order the row makes them, in
 * approved_world, whose reviews (5001, and 5002 for a create), threads (comments from 7001) and
 * files reach every route: the writes first, so the reads after them read what the writes left.
 * A route GitHub answers a role App with a refusal and no success — the protection of a branch
 * and `user` (its 403), and the jobs of a run the world holds none of (its 404) — names that
 * refusal. */
function restCalls(head: string, base: string): [string, string[], string?][] {
  const repo = "repos/example/widgets";
  const pull = `${repo}/pulls/1001`;
  const post = (path: string, ...fields: string[]) => ["api", "-X", "POST", path, ...fields];
  const refused = "gh: Resource not accessible by integration (HTTP 403)\n";
  return [
    [
      "POST repos/{owner}/{repo}/issues/{number}/comments",
      post(`${repo}/issues/1001/comments`, "-f", "body=a note"),
    ],
    [
      "POST repos/{owner}/{repo}/pulls/{number}/comments",
      post(
        `${pull}/comments`,
        "-f",
        "body=a line note",
        "-f",
        `commit_id=${head}`,
        "-f",
        "path=docs/cli.md",
        "-F",
        "line=5"
      ),
    ],
    [
      "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies",
      post(`${pull}/comments/7001/replies`, "-f", "body=a reply"),
    ],
    [
      "POST repos/{owner}/{repo}/pulls/{number}/reviews",
      post(`${pull}/reviews`, "-f", `commit_id=${head}`, "-f", "body=pending"),
    ],
    [
      "POST repos/{owner}/{repo}/pulls/{number}/reviews/{id}/events",
      post(`${pull}/reviews/5002/events`, "-f", "event=COMMENT"),
    ],
    [
      "PUT repos/{owner}/{repo}/pulls/{number}/reviews/{id}",
      ["api", "-X", "PUT", `${pull}/reviews/5001`, "-f", "body=updated"],
    ],
    [
      "PATCH repos/{owner}/{repo}/pulls/{number}",
      ["api", "-X", "PATCH", pull, "-f", "body=a body"],
    ],
    ["GET repos/{owner}/{repo}", ["api", repo]],
    ["GET repos/{owner}/{repo}/pulls/{number}", ["api", pull]],
    ["GET repos/{owner}/{repo}/pulls/{number}/reviews", ["api", `${pull}/reviews`]],
    ["GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}", ["api", `${pull}/reviews/5001`]],
    [
      "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments",
      ["api", `${pull}/reviews/5001/comments`],
    ],
    ["GET repos/{owner}/{repo}/pulls/{number}/comments", ["api", `${pull}/comments`]],
    ["GET repos/{owner}/{repo}/issues/{number}/comments", ["api", `${repo}/issues/1001/comments`]],
    [
      "GET repos/{owner}/{repo}/commits/{ref}/check-runs",
      ["api", `${repo}/commits/${head}/check-runs`],
    ],
    ["GET repos/{owner}/{repo}/commits/{ref}/status", ["api", `${repo}/commits/${head}/status`]],
    ["GET repos/{owner}/{repo}/actions/runs", ["api", `${repo}/actions/runs`]],
    [
      "GET repos/{owner}/{repo}/actions/runs/{run_id}/jobs",
      ["api", `${repo}/actions/runs/1/jobs`],
      "gh: Not Found (HTTP 404)\n",
    ],
    ["GET repos/{owner}/{repo}/rules/branches/{branch+}", ["api", `${repo}/rules/branches/main`]],
    ["GET repos/{owner}/{repo}/rulesets", ["api", `${repo}/rulesets`]],
    ["GET repos/{owner}/{repo}/rulesets/{id}", ["api", `${repo}/rulesets/1`]],
    [
      "GET repos/{owner}/{repo}/branches/{branch}/protection",
      ["api", `${repo}/branches/main/protection`],
      refused,
    ],
    ["GET repos/{owner}/{repo}/branches/{branch+}", ["api", `${repo}/branches/main`]],
    ["GET repos/{owner}/{repo}/compare/{basehead+}", ["api", `${repo}/compare/${base}...${head}`]],
    ["GET repos/{owner}/{repo}/git/ref/{ref+}", ["api", `${repo}/git/ref/heads/main`]],
    ["GET repos/{owner}/{repo}/contents/{path+}", ["api", `${repo}/contents/greet.ts?ref=${head}`]],
    ["GET repos/{owner}/{repo}/contents/{path+}", ["api", `${repo}/contents/docs?ref=${head}`]],
    ["GET user", ["api", "user"], refused],
    ["GET repos/{owner}/{repo}/commits/{ref+}", ["api", `${repo}/commits/${head}`]],
    ["GET repos/{owner}/{repo}/commits", ["api", `${repo}/commits?sha=${head}`]],
    ["GET repos/{owner}/{repo}/git/commits/{sha}", ["api", `${repo}/git/commits/${head}`]],
    ["GET repos/{owner}/{repo}/git/refs", ["api", `${repo}/git/refs`]],
    ["GET repos/{owner}/{repo}/git/refs/{ref+}", ["api", `${repo}/git/refs/heads/main`]],
    ["GET repos/{owner}/{repo}/branches", ["api", `${repo}/branches`]],
    ["GET repos/{owner}/{repo}/git/trees/{sha+}", ["api", `${repo}/git/trees/${head}?recursive=1`]],
    ["GET repos/{owner}/{repo}/tags", ["api", `${repo}/tags`]],
    ["GET repos/{owner}/{repo}/pulls/{number}/commits", ["api", `${pull}/commits`]],
    ["GET repos/{owner}/{repo}/pulls/{number}/files", ["api", `${pull}/files`]],
  ];
}

// Catches: a REST route the world serves whose answer lacks a field GitHub's description requires
// of its operation's success — a user that is a login alone, a review with no `_links`, a review
// comment with no `diff_hunk`, a comparison with no `commits`, a check run with no
// `pull_requests`, a commit status's repository that is a name alone, a ref with no `node_id` —
// read as the tester's search reads it; a route the world serves that the extract holds no
// operation of, named, so a route added to the world without its pin fails here; a route of
// WORLD_ROUTES this row makes no call of; and a route GitHub refuses a role App answered with a
// success, or refused in another shape than GitHub's error body. The extract is the one of
// github/rest-api-description at c9c95fe34ced, which this row names.
test("every REST route the world serves answers every field GitHub's description requires", () => {
  expect([extractCommit(), DESCRIPTION_COMMIT]).toEqual([
    "c9c95fe34ced05fe076001f5c55704742bc8e7f8",
    "c9c95fe34ced05fe076001f5c55704742bc8e7f8",
  ]);
  expect(
    servedRoutes()
      .map(({ route }) => route)
      .filter((route) => restOperation(route) === undefined)
  ).toEqual([]);
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const base = git(path.join(dir, "remote.git"), "rev-parse", "refs/heads/main");
  const calls = restCalls(head, base);
  expect([...new Set(calls.map(([route]) => route))].sort()).toEqual(
    WORLD_ROUTES.map(({ route }) => route)
      .filter((route) => /^(GET|POST|PUT|PATCH|DELETE) /.test(route))
      .sort()
  );
  const error = pinnedRead("branches-main-protection");
  const verdicts = calls.map(([route, argv, refusal]) => {
    const got = ghCall(dir, ...argv);
    const answer: unknown = JSON.parse(got.out);
    if (refusal !== undefined)
      return [
        route,
        got.code,
        got.err === refusal ? "GitHub's refusal" : got.err,
        shapeDiff(answer, error),
      ];
    const schema = restOperation(route)?.schema;
    return [
      route,
      got.code,
      got.err,
      schema === undefined ? "no schema" : absentFields(answer, schema),
    ];
  });
  expect(verdicts).toEqual(
    calls.map(([route, , refusal]) =>
      refusal === undefined ? [route, 0, "", []] : [route, 1, "GitHub's refusal", SAME_SHAPE]
    )
  );
}, 120_000);

// The key set of each REST route the world serves: every field its answer holds, at every level,
// against GitHub's own for that endpoint. Where production's answer to the route is pinned
// (production-shapes.json), its key set is that answer's (`shapeDiff`'s missing and extra paths);
// for every other route, the description's (rest-required.json: every field required there, and
// no field the description declares nowhere, `keyDiff`). A key the world answers that GitHub's key
// set lacks, or lacks where the key set holds it, is declared with its evidence
// (KEY_SET_DECLARED), or the route is wrong.

/** The pinned read each REST route's answers are held to, one for each of its calls in the order
 * `restCalls` makes them; a route this names none of is held to the description. */
const KEY_SET_PINS: Readonly<Record<string, readonly string[]>> = {
  "GET repos/{owner}/{repo}": ["gh api GET repos/fernbrook/widgets"],
  "GET repos/{owner}/{repo}/pulls/{number}": ["gh api GET repos/fernbrook/widgets/pulls/{n}"],
  "GET repos/{owner}/{repo}/pulls/{number}/reviews": [
    "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews",
  ],
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}": [
    "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews/{n}",
  ],
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments": [
    "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews/{n}/comments",
  ],
  "GET repos/{owner}/{repo}/pulls/{number}/comments": [
    "gh api GET repos/fernbrook/widgets/pulls/{n}/comments",
  ],
  "GET repos/{owner}/{repo}/issues/{number}/comments": [
    "gh api GET repos/fernbrook/widgets/issues/{n}/comments",
  ],
  "GET repos/{owner}/{repo}/commits/{ref}/check-runs": [
    "gh api GET repos/fernbrook/widgets/commits/{sha}/check-runs",
  ],
  "GET repos/{owner}/{repo}/commits/{ref}/status": [
    "gh api GET repos/fernbrook/widgets/commits/{sha}/status",
  ],
  "GET repos/{owner}/{repo}/actions/runs": ["gh api GET actions/runs"],
  "GET repos/{owner}/{repo}/actions/runs/{run_id}/jobs": [
    "gh api GET actions/runs/{id}/jobs (missing run)",
  ],
  "GET repos/{owner}/{repo}/rules/branches/{branch+}": ["rules-branches-main"],
  "GET repos/{owner}/{repo}/rulesets": ["rulesets"],
  "GET repos/{owner}/{repo}/rulesets/{id}": ["ruleset-<id>"],
  "GET repos/{owner}/{repo}/branches/{branch}/protection": ["branches-main-protection"],
  "GET repos/{owner}/{repo}/branches/{branch+}": ["branches-main"],
  "GET repos/{owner}/{repo}/compare/{basehead+}": [
    "gh api GET repos/fernbrook/widgets/compare/{sha}...{sha}",
  ],
  "GET repos/{owner}/{repo}/git/ref/{ref+}": ["gh api GET git/ref/{ref}"],
  "GET repos/{owner}/{repo}/contents/{path+}": [
    "gh api GET repos/fernbrook/widgets/contents/greet.ts",
    "gh api GET contents directory",
  ],
  "GET user": ["gh api user"],
};

/** The keys a route's answer differs from GitHub's key set in, each declared with its evidence:
 * `extra`, a key the world answers that the key set lacks, and `missing`, one it lacks that the
 * key set holds, as the paths the comparison names them by. */
const KEY_SET_DECLARED: Readonly<
  Record<string, { extra?: string[]; missing?: string[]; evidence: string }>
> = {
  "GET repos/{owner}/{repo}/pulls/{number}/comments": {
    extra: [".[].copilot_code_review_severity"],
    evidence:
      "production gives every review comment `copilot_code_review_severity`, which the pin of 2026-11-17 does not hold: `gh api repos/cybermonklabs/legion/pulls/2827/comments --jq '.[0] | keys'` named it (Date: Sat, 21 Nov 2026 10:21:12 GMT), and `gh api -i …/pulls/2827/comments?per_page=1` answered it null (10:21:02 GMT)",
  },
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments": {
    extra: [".[].copilot_code_review_severity"],
    evidence:
      "production gives a review's comments `copilot_code_review_severity`, which the pin of 2026-11-17 does not hold: `gh api -i repos/cybermonklabs/legion/pulls/2827/reviews/5816301660/comments?per_page=1` answered it null (Date: Sat, 21 Nov 2026 10:24:42 GMT)",
  },
  "POST repos/{owner}/{repo}/pulls/{number}/comments": {
    extra: [".copilot_code_review_severity"],
    evidence:
      "the create answers the review comment it made, the object production's reads of a review comment give with `copilot_code_review_severity`, which the description at c9c95fe3 does not declare (`gh api -i repos/cybermonklabs/legion/pulls/2827/comments?per_page=1`, Date: Sat, 21 Nov 2026 10:21:02 GMT); a create is a write, which no pod makes in production to read its own answer",
  },
  "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies": {
    extra: [".copilot_code_review_severity"],
    evidence:
      "the reply answers the review comment it made, the object production's reads of a review comment give with `copilot_code_review_severity`, which the description at c9c95fe3 does not declare (`gh api -i repos/cybermonklabs/legion/pulls/2827/comments?per_page=1`, Date: Sat, 21 Nov 2026 10:21:02 GMT); a reply is a write, which no pod makes in production to read its own answer",
  },
};

/** The world's answer to each call `restCalls` makes, by route, read once in one approved_world
 * (as correct builds it), the writes first: what every key-set row compares. */
let keySetAnswers: Map<string, unknown[]> | undefined;
function answersOf(route: string): unknown[] {
  if (keySetAnswers === undefined) {
    const dir = worldDir("approved_world", "", { worldCase: "correct" });
    const head = builtHead(dir);
    const base = git(path.join(dir, "remote.git"), "rev-parse", "refs/heads/main");
    keySetAnswers = new Map();
    for (const [called, argv] of restCalls(head, base))
      keySetAnswers.set(called, [
        ...(keySetAnswers.get(called) ?? []),
        JSON.parse(ghCall(dir, ...argv).out),
      ]);
  }
  return keySetAnswers.get(route) ?? [];
}

/** Every REST route the world serves (WORLD_ROUTES of an HTTP verb). */
const REST_ROUTES = WORLD_ROUTES.map(({ route }) => route).filter((route) =>
  /^(GET|POST|PUT|PATCH|DELETE) /.test(route)
);

// Catches: a REST route added to the world with no key set to hold its answer to — no pinned read
// of production's and no operation of GitHub's description in the extract — which the key-set
// rows below would hold to nothing; and a route those rows make no call of.
test("every REST route the stand-in serves has GitHub's key set to hold its answer to", () => {
  expect(
    REST_ROUTES.filter(
      (route) => KEY_SET_PINS[route] === undefined && restOperation(route)?.schema === undefined
    )
  ).toEqual([]);
  expect(REST_ROUTES.filter((route) => answersOf(route).length === 0)).toEqual([]);
  expect(Object.keys(KEY_SET_DECLARED).filter((route) => !REST_ROUTES.includes(route))).toEqual([]);
}, 120_000);

// Catches, one row per REST route the world serves: an answer holding a key GitHub's answer to the
// route does not, or lacking one it holds, at any level — production's pinned answer where it is
// pinned, the description's declared and required fields where it is not — that no declaration
// names with its evidence; and a declaration of a difference the answer no longer makes.
for (const route of REST_ROUTES)
  test(`the key set of ${route} is GitHub's`, () => {
    const pins = KEY_SET_PINS[route];
    const schema = restOperation(route)?.schema;
    const answers = answersOf(route);
    const declared = KEY_SET_DECLARED[route];
    expect([route, answers.length > 0, pins !== undefined || schema !== undefined]).toEqual([
      route,
      true,
      true,
    ]);
    expect(
      answers.map((answer, index) => {
        const pin = pins?.[index];
        if (pin === undefined) return keyDiff(answer, schema ?? {});
        const { missing, extra } = shapeDiff(answer, pinnedRead(pin));
        return { missing, extra };
      })
    ).toEqual(
      answers.map(() => ({ missing: declared?.missing ?? [], extra: declared?.extra ?? [] }))
    );
  }, 120_000);

/** Why the world's review comments hold a key the pin of 2026-11-17 does not: production added
 * it after that read (KEY_SET_DECLARED names the reads of 2026-11-21 that hold it). */
const SEVERITY_STATE =
  "production has given every review comment `copilot_code_review_severity`, null for one no Copilot review made, since the pin was read: `gh api -i repos/cybermonklabs/legion/pulls/2827/comments?per_page=1` (Date: Sat, 21 Nov 2026 10:21:02 GMT) and `…/pulls/2827/reviews/5816301660/comments?per_page=1` (10:24:42 GMT) each answered it null";
/** The REST reads production-shapes.json pins that the rows above do not hold, each read with
 * production's own command (`argvOf`) in each of `worlds` (approved_world, as correct builds it,
 * unless it names others)
 * after `before`, the calls that leave the world holding what production's answer held items of
 * (a conversation comment, a reply), and `kinds`, the kinds production's answer and the world's
 * differ in for a reason of state, which `state` names. The pull request's comments are read in
 * review_world too, whose seeded comment stands on greet.test.ts, the CLI's tests as rig.sh's
 * fixture_cli writes them. The reads of 2026-11-18 (the tester's prod-reads-19.json) are held
 * too: the workflow runs, a run's jobs the repository has not got (GitHub's 404), a comparison as
 * a diff, the base's ref, a directory as JSON and under the raw media type, which GitHub answers
 * a directory as JSON all the same, and the App's read of its user (GitHub's 403). Production's
 * directory was `.github/actions`, which holds directories alone, so the world's is `.github`,
 * which holds `workflows` alone: a file's `download_url` is a string and a directory's null. A
 * review comment carries `copilot_code_review_severity`, which production added after the pin of
 * 2026-11-17 was read (SEVERITY_STATE). */
const PINNED_REST: readonly {
  name: string;
  worlds?: string[];
  path?: string;
  before?: string[][];
  kinds?: string[];
  missing?: string[];
  extra?: string[];
  state?: string;
}[] = [
  { name: "gh api GET repos/fernbrook/widgets" },
  { name: "gh api GET repos/fernbrook/widgets/pulls/{n}" },
  { name: "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews" },
  { name: "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews/{n}" },
  {
    name: "gh api GET repos/fernbrook/widgets/pulls/{n}/reviews/{n}/comments",
    extra: [".[].copilot_code_review_severity"],
    state: SEVERITY_STATE,
  },
  ...[
    "gh api GET repos/fernbrook/widgets/pulls/{n}/comments",
    "gh api GET repos/{owner}/{repo}/pulls/{n}/comments",
  ].map((name) => ({
    name,
    worlds: ["approved_world", "review_world"],
    before: [
      [
        "api",
        "-X",
        "POST",
        "repos/example/widgets/pulls/1001/comments/7001/replies",
        "-f",
        "body=done",
      ],
    ],
    extra: [".[].copilot_code_review_severity"],
    state: SEVERITY_STATE,
  })),
  {
    name: "gh api GET repos/fernbrook/widgets/issues/{n}/comments",
    before: [["pr", "comment", "1001", "--body", "a note"]],
  },
  {
    name: "gh api GET repos/fernbrook/widgets/commits/{sha}/check-runs",
    kinds: [
      ".check_runs[].conclusion: null in production, string in the world",
      ".check_runs[].completed_at: null in production, string in the world",
      ".check_runs[].output.title: null in production, null|string in the world",
      ".check_runs[].output.summary: null in production, null|string in the world",
    ],
    state:
      "production's checks were still running when it was read, and the world's have concluded (rig.sh's concluded_checks); production's read was a workflow's jobs alone, which write no output, and the world's head reports its code-scanning tool's check too, whose App writes a title and a summary, as production's CodeQL check does (measured on 2026-11-18)",
  },
  { name: "gh api GET repos/fernbrook/widgets/commits/{sha}/status" },
  { name: "gh api GET repos/fernbrook/widgets/compare/{sha}...{sha}" },
  { name: "gh api GET repos/fernbrook/widgets/contents/greet.ts", path: "greet.ts" },
  {
    name: "gh api GET repos/fernbrook/widgets/contents/.github/workflows/greet.yml",
    path: ".github/workflows/greet.yml",
  },
  { name: "gh api GET repos/fernbrook/widgets/rules/branches/main" },
  { name: "gh api GET repos/fernbrook/widgets/branches/main" },
  { name: "gh api GET actions/runs" },
  { name: "gh api GET actions/runs/{id}/jobs (missing run)" },
  {
    name: "gh api GET compare diff media",
    missing: ["line -<text>"],
    extra: ["line new file mode <mode>"],
    state: WORLD_DIFF_STATE,
  },
  { name: "gh api GET git/ref/{ref}" },
  { name: "gh api GET contents directory", path: ".github" },
  { name: "gh api GET contents directory raw", path: ".github" },
  { name: "gh api user" },
];

// Catches, each read held to production's whole answer (every path production's holds, no path it
// does not, no kind it does not give): the repository short of its URLs, its times, its counts, its
// license or its organization; the pull request in gh's own `pr view` shape (a string `id`,
// `mergeable` `MERGEABLE`, `headRefOid` and no `head.repo`); a review carrying the `author` gh's
// view gives and GitHub's REST does not, or no `node_id`, `_links` or `author_association`; a
// review's comments in the shape of the pull request's (`line`, `side`, `start_line`, which
// GitHub's older shape of a review's comments has not got) or with no `diff_hunk` and
// `position`; a reply that names no review, where GitHub files it in one of its own; a
// conversation comment with no `url`, `reactions` or the App that made it; a check run with no
// `pull_requests` or its App a name alone; a commit status's repository that is a name alone; a
// comparison with no commits, links or file URLs; a file or a branch in another shape; a list of
// workflow runs or a ref in another shape; a refusal of a run's jobs or of the user in another
// body than GitHub's error; a diff answered as JSON; and a directory's entries in another shape.
for (const pin of PINNED_REST)
  test(`a REST read answers in production's shape: ${pin.name}`, () => {
    const read = pinnedRead(pin.name);
    for (const world of pin.worlds ?? ["approved_world"]) {
      const dir = worldDir(world, "", {
        worldCase: world === "approved_world" ? "correct" : "",
      });
      const head = builtHead(dir);
      const base = git(path.join(dir, "remote.git"), "rev-parse", "refs/heads/main");
      for (const argv of pin.before ?? [])
        expect([world, argv, ghCall(dir, ...argv).code]).toEqual([world, argv, 0]);
      const got = pinnedCall(dir, read, {
        number: "1001",
        review: "5001",
        head,
        base,
        path: pin.path ?? "",
      });
      expect([world, got.code, got.err, shapeDiff(got.answer, read)]).toEqual([
        world,
        read.exit,
        read.stderr,
        { missing: pin.missing ?? [], extra: pin.extra ?? [], kinds: pin.kinds ?? [] },
      ]);
    }
  }, 60_000);

/** The reads the run's GitHub stand-in (github-standin.ts, which a run's Dispatch reads GitHub
 * through) answers, each held to the pinned read of production's that asks GitHub the same:
 * `path` asked with `accept`, the world's head and pull request in it, and `kinds`, the kinds the
 * two answers differ in for a reason of state, which `state` names. Two are the gh stand-in's
 * routes too, and their pins the same (`GET /repos/{owner}/{repo}` and `GET /repos/{owner}/{repo}/
 * pulls/{number}`); two are Dispatch's alone, held to the implementer's readings of 2026-11-18
 * (implementer-reads.json, items 33 and 35): a commit by its SHA under the SHA media type, and
 * the pull requests of a commit. The fifth route a run's Dispatch asks, `GET /repos/{owner}/
 * {repo}/installation`, is read with the App's JWT, which no pod holds: an installation token is
 * refused it (`gh api repos/cybermonklabs/legion/installation`, exit 1, `gh: A JSON web token
 * could not be decoded (HTTP 401)`, measured at about 08:47Z on 2026-11-18), so it has no pinned
 * read and is held to rest-required.json alone (github-standin.test.ts); and the token's mint is
 * no read. */
const PINNED_HTTP: readonly {
  name: string;
  path: (head: string) => string;
  accept?: string;
  kinds?: string[];
  state?: string;
}[] = [
  { name: "gh api GET repos/fernbrook/widgets", path: () => "/repos/example/widgets" },
  {
    name: "gh api GET repos/fernbrook/widgets/pulls/{n}",
    path: () => "/repos/example/widgets/pulls/1001",
  },
  {
    name: "gh api GET commits/{sha} sha media",
    path: (head) => `/repos/example/widgets/commits/${head}`,
    accept: "application/vnd.github.sha",
  },
  {
    name: "gh api GET commits/{sha}/pulls",
    path: (head) => `/repos/example/widgets/commits/${head}/pulls`,
    kinds: [
      ".[].closed_at: string in production, null in the world",
      ".[].merged_at: string in production, null in the world",
    ],
    state:
      "the implementer read the pull requests of #4326's head, which was merged, so its pull request's `closed_at` and `merged_at` were times; the world's pull request is open, and GitHub answers an open one's null, as its REST read of the open #2827 holds them (`gh api GET repos/fernbrook/widgets/pulls/{n}`)",
  },
];
// Catches: the run's GitHub answering Dispatch a repository, a pull request, a commit or a
// commit's pull requests in another shape than production's GitHub does — a field missing or in
// another kind, one GitHub never gives, a SHA answered as a JSON object. Before this row the
// stand-in's answers were held to GitHub's required fields alone (github-standin.test.ts), from a
// world that row writes by hand; this one serves rig.sh's own world.
test("each read of the run's GitHub stand-in answers in production's shape", async () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const head = builtHead(dir);
  const child = Bun.spawn(
    ["bun", path.join(import.meta.dir, "github-standin.ts"), dir, "example/widgets", "main"],
    { stdout: "pipe", stderr: "pipe" }
  );
  try {
    const reader = child.stdout.getReader();
    let log = "";
    while (!/"listening":"127\.0\.0\.1:\d+"/.test(log)) {
      const { value, done } = await reader.read();
      if (done)
        throw new Error(`github-standin.ts exited: ${await new Response(child.stderr).text()}`);
      log += new TextDecoder().decode(value);
    }
    const origin = `http://127.0.0.1:${/"listening":"127\.0\.0\.1:(\d+)"/.exec(log)?.[1]}`;
    const ask = async (method: string, route: string, bearer: string, accept?: string) => {
      const answered = await fetch(`${origin}${route}`, {
        method,
        headers: {
          Accept: accept ?? "application/vnd.github+json",
          Authorization: `Bearer ${bearer}`,
        },
      });
      // Read as the client reads it: JSON where the answer says it is (`Content-Type`).
      const text = await answered.text();
      const json = answered.headers.get("content-type")?.includes("json") === true;
      return { status: answered.status, answer: json ? JSON.parse(text) : text };
    };
    // An App's JWT's shape, as Dispatch signs one, for the two calls that mint a token.
    const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJJdjEuZTJlZmFrZSJ9.c2lnbmVk";
    const installation = await ask("GET", "/repos/example/widgets/installation", jwt);
    const minted = await ask(
      "POST",
      `/app/installations/${installation.answer.id}/access_tokens`,
      jwt
    );
    const token = String(minted.answer.token);
    const verdicts: unknown[] = [];
    for (const pin of PINNED_HTTP) {
      const got = await ask("GET", pin.path(head), token, pin.accept);
      verdicts.push([pin.name, got.status, shapeDiff(got.answer, pinnedRead(pin.name))]);
    }
    expect(verdicts).toEqual(
      PINNED_HTTP.map(({ name, kinds }) => [name, 200, { ...SAME_SHAPE, kinds: kinds ?? [] }])
    );
  } finally {
    child.kill();
  }
}, 60_000);

// Every read production-shapes.json pins is compared with the world's answer by a row: one of this
// file's tables (PINNED_GH, PINNED_GRAPHQL, PINNED_REST, PINNED_HTTP), each of whose reads a row
// makes, or a row of a test file of this directory that names it, `pinnedRead("<name>")` (the
// ruleset reads above, `gh auth status`, the issue record in daemon-standin.test.ts). Catches a
// pin no row reaches, which is a claim no row holds: from 2026-11-17 to 2026-11-18 the 20 GraphQL
// reads of round 25 were pinned, and the README counted them held, while no row compared one.
test("every read the pin holds is compared by a row", () => {
  const named = readdirSync(import.meta.dir)
    .filter((file) => file.endsWith(".test.ts"))
    .flatMap((file) =>
      [
        ...readFileSync(path.join(import.meta.dir, file), "utf8").matchAll(
          /\bpinnedRead\(\s*("(?:[^"\\\n]|\\.)*"|RECORD_READ)\s*\)/g
        ),
      ].map(([, name = ""]) => (name === "RECORD_READ" ? RECORD_READ : JSON.parse(name)))
    );
  const held = new Set([
    ...PINNED_GH,
    ...PINNED_GRAPHQL,
    ...PINNED_REST.map(({ name }) => name),
    ...PINNED_HTTP.map(({ name }) => name),
    ...named,
  ]);
  expect(
    pinnedReads()
      .map(({ name }) => name)
      .filter((name) => !held.has(name))
  ).toEqual([]);
});

// Catches (the architect's ruling 4 of 2026-11-17): `GET repos/<repo>/pulls/<n>` answered with
// gh's `pr view` object, and a REST pull request that says something else than the world's other
// answers: a node id, a URL, a title, a body, a state, a branch, a head or a merge state other
// than `pr view`'s at the same moment (after a push moved the head and a body edit changed the
// body); a base that is not the base branch's tip; an author other than the bot whose commit
// opened the branch, as the comparison links that commit to it; and a test merge GitHub could not
// hold: no commit, or one that does not merge the head into the base.
test("the REST pull request is GitHub's object, saying what pr view says", () => {
  const dir = worldDir("approved_world", "", { worldCase: "correct" });
  const remote = path.join(dir, "remote.git");
  const base = git(remote, "rev-parse", "refs/heads/main");
  const tip = pushFix(dir, "docs/cli.md", "the text fix\n");
  expect(
    ghCall(dir, "api", "-X", "PATCH", "repos/example/widgets/pulls/1001", "-f", "body=edited").code
  ).toBe(0);
  const view = JSON.parse(
    ghCall(
      dir,
      "pr",
      "view",
      "1001",
      "--json",
      "id,number,title,body,state,isDraft,headRefName,headRefOid,baseRefName,url,mergeable,mergeStateStatus"
    ).out
  );
  const rest = JSON.parse(ghCall(dir, "api", "repos/example/widgets/pulls/1001").out);
  expect({
    node_id: rest.node_id,
    number: rest.number,
    title: rest.title,
    body: rest.body,
    state: rest.state,
    draft: rest.draft,
    head: [rest.head.ref, rest.head.sha],
    base: [rest.base.ref, rest.base.sha],
    html_url: rest.html_url,
    mergeable: rest.mergeable,
    mergeable_state: rest.mergeable_state,
  }).toEqual({
    node_id: view.id,
    number: view.number,
    title: view.title,
    body: "edited",
    state: view.state.toLowerCase(),
    draft: view.isDraft,
    head: [view.headRefName, tip],
    base: [view.baseRefName, base],
    html_url: view.url,
    mergeable: view.mergeable === "MERGEABLE",
    mergeable_state: view.mergeStateStatus.toLowerCase(),
  });
  expect(view.headRefOid).toBe(tip);
  const compared = JSON.parse(
    ghCall(dir, "api", `repos/example/widgets/compare/${base}...${tip}`).out
  );
  expect(rest.user).toEqual(compared.commits[0].author);
  expect(rest.user).toMatchObject({ login: "widgets-implementer[bot]", type: "Bot" });
  expect(git(remote, "show", "-s", "--format=%P", rest.merge_commit_sha)).toBe(`${base} ${tip}`);
  expect(git(remote, "rev-parse", `${rest.merge_commit_sha}^{tree}`)).toBe(
    git(remote, "merge-tree", "--write-tree", base, tip)
  );
  expect(rest.commits).toBe(Number(git(remote, "rev-list", "--count", `${base}..${tip}`)));
}, 60_000);

// Catches: a merge state that reads the base's classic protection, which a base a ruleset alone
// protects holds none of, so that no thread ever held a head `BLOCKED`; one that blocks a base
// whose rules require no resolution, or whose one thread is resolved or is a pending review's;
// and one that blocks a head the world holds in a state of its own.
test("the merge state is BLOCKED by an open thread where the base's rules require threads resolved, and by nothing else", () => {
  const world = {
    key: "LWEVAL-1",
    repo: "example/widgets",
    pr: 1001,
    branch: "legion/LWEVAL-1",
    base: "main",
    head: "0123456789abcdef0123",
  };
  const ruled = (rules: object[]) => ({
    world,
    routes: [{ match: "^gh api .*/rules/branches/main", methods: ["GET"], stdout: rules }],
  });
  const pullRequest = (resolution: boolean) => ({
    type: "pull_request",
    parameters: {
      required_approving_review_count: 0,
      required_review_thread_resolution: resolution,
    },
  });
  const checks = { type: "required_status_checks", parameters: { required_status_checks: [] } };
  const thread = (isResolved: boolean, state = "SUBMITTED") => ({
    list: [{ id: "PRRT_kwDOrig0001", isResolved, comments: [{ databaseId: 7001, state }] }],
    next: 7002,
    reviews: 0,
  });
  const head = world.head;
  expect([
    mergeStateOf(ruled([pullRequest(true), checks]), thread(false), {}, head, []),
    mergeStateOf(ruled([pullRequest(true), checks]), thread(true), {}, head, []),
    mergeStateOf(ruled([pullRequest(true), checks]), thread(false, "PENDING"), {}, head, []),
    mergeStateOf(ruled([pullRequest(false), checks]), thread(false), {}, head, []),
    mergeStateOf(ruled([checks]), thread(false), {}, head, []),
    mergeStateOf({ world, routes: [] }, thread(false), {}, head, []),
    mergeStateOf(ruled([pullRequest(true), checks]), thread(false), { [head]: "DIRTY" }, head, []),
  ]).toEqual(["BLOCKED", "CLEAN", "CLEAN", "CLEAN", "CLEAN", "CLEAN", "DIRTY"]);
});

// GitHub decides a pull request's review and holds its merge state by its base's rules: one
// approval required, two checks GitHub Actions reports, and code scanning by CodeQL, as
// production's `main` requires them (rig.sh's base_ruleset_routes). Measured on 2026-11-18 over
// cybermonklabs/legion's open pull requests against `main`: no approval read `REVIEW_REQUIRED` and
// `BLOCKED`, a change request `CHANGES_REQUESTED` and `BLOCKED`, an approval with `ci-ok` not yet
// reported `BLOCKED`, and an approval with every required check and `CodeQL` successful `CLEAN`
// and `MERGEABLE`; on a branch no rule protects a change request read `CHANGES_REQUESTED` and
// `CLEAN`, a pull request with no review `""`; a `DIRTY` one `CONFLICTING`, an `UNKNOWN` one
// `UNKNOWN`. Catches: a decision and a state frozen into the world, `CLEAN` before any review; a
// comment or a pending review read as a decision; a later approval of another reviewer read as
// lifting a change request that stands; an approval dismissed by a push, where the base dismisses
// none (`dismiss_stale_reviews_on_push` false); a required check that is still running, failed,
// never reported or reported by another App read as passed, and a skipped one read as failed; a
// code-scanning tool whose check failed or never reported read as met; a base that requires no
// approval holding a pull request `BLOCKED` on its review; and a `mergeable` that does not follow
// the merge state.
test("the review decision and the merge state follow the base's approvals, its required checks and its code-scanning tool", () => {
  const world = {
    key: "LWEVAL-1",
    repo: "example/widgets",
    pr: 1001,
    branch: "legion/LWEVAL-1",
    base: "main",
    head: "0123456789abcdef0123",
  };
  const head = world.head;
  const run = (name: string, conclusion: string | null, app = 15368) => ({
    name,
    status: conclusion === null ? "in_progress" : "completed",
    conclusion,
    app: { id: app },
  });
  const green = [run("test", "success"), run("lint", "success"), run("CodeQL", "success", 57789)];
  const based = (runs = green, held: object[] = [], approvals = 1) => ({
    world,
    routes: [
      {
        match: "^gh api .*/rules/branches/main",
        methods: ["GET"],
        stdout: [
          {
            type: "pull_request",
            parameters: {
              required_approving_review_count: approvals,
              dismiss_stale_reviews_on_push: false,
              require_last_push_approval: false,
              required_review_thread_resolution: false,
            },
          },
          {
            type: "required_status_checks",
            parameters: {
              required_status_checks: [
                { context: "test", integration_id: 15368 },
                { context: "lint", integration_id: 15368 },
              ],
            },
          },
          { type: "code_scanning", parameters: { code_scanning_tools: [{ tool: "CodeQL" }] } },
        ],
      },
      { match: "^gh api .*/check-runs", methods: ["GET"], stdout: { check_runs: runs } },
      { match: "^gh api .*pulls/1001/reviews", methods: ["GET"], stdout: held },
      {
        match: "^gh api .*graphql",
        graph: { query: { viewer: { login: "widgets-reviewer[bot]" } } },
      },
    ],
  });
  const reviewed = (...states: string[]): Recorded[] =>
    states.map((state) => ({ argv: ["api"], review: { state, head } }));
  const none = { list: [], next: 7001, reviews: 0 };
  const status = (at: ThreadWorld, calls: Recorded[], tip = head) => [
    reviewDecisionOf(at, calls),
    mergeStateOf(at, none, {}, tip, calls),
  ];
  const other = [{ state: "APPROVED", commit_id: head, user: { login: "legion-review[bot]" } }];
  const pushed = "fedcba9876543210fedc";
  expect({
    "before review": status(based(), []),
    "a comment": status(based(), reviewed("COMMENTED")),
    "a pending review": status(based(), reviewed("PENDING")),
    "a change request": status(based(), reviewed("CHANGES_REQUESTED")),
    "a change request, then an approval": status(
      based(),
      reviewed("CHANGES_REQUESTED", "APPROVED")
    ),
    "an approval, then a comment": status(based(), reviewed("APPROVED", "COMMENTED")),
    "an approval, then a push": status(based(), reviewed("APPROVED"), pushed),
    "another reviewer's approval, then a change request": status(
      based(green, other),
      reviewed("CHANGES_REQUESTED")
    ),
    "another reviewer's approval": status(based(green, other), []),
    "approved, test running": status(
      based([run("test", null), ...green.slice(1)]),
      reviewed("APPROVED")
    ),
    "approved, test failed": status(
      based([run("test", "failure"), ...green.slice(1)]),
      reviewed("APPROVED")
    ),
    "approved, lint never reported": status(
      based([run("test", "success"), run("CodeQL", "success", 57789)]),
      reviewed("APPROVED")
    ),
    "approved, lint by another App": status(
      based([
        run("test", "success"),
        run("lint", "success", 57789),
        run("CodeQL", "success", 57789),
      ]),
      reviewed("APPROVED")
    ),
    "approved, test skipped": status(
      based([run("test", "skipped"), ...green.slice(1)]),
      reviewed("APPROVED")
    ),
    "approved, CodeQL failed": status(
      based([...green.slice(0, 2), run("CodeQL", "failure", 57789)]),
      reviewed("APPROVED")
    ),
    "approved, CodeQL never reported": status(based(green.slice(0, 2)), reviewed("APPROVED")),
    "no approval required, none given": status(based(green, [], 0), []),
    "no approval required, a change request": status(
      based(green, [], 0),
      reviewed("CHANGES_REQUESTED")
    ),
    "a conflict, no review": [
      reviewDecisionOf(based(), []),
      mergeStateOf(based(), none, { [head]: "DIRTY" }, head, []),
    ],
  }).toEqual({
    "before review": ["REVIEW_REQUIRED", "BLOCKED"],
    "a comment": ["REVIEW_REQUIRED", "BLOCKED"],
    "a pending review": ["REVIEW_REQUIRED", "BLOCKED"],
    "a change request": ["CHANGES_REQUESTED", "BLOCKED"],
    "a change request, then an approval": ["APPROVED", "CLEAN"],
    "an approval, then a comment": ["APPROVED", "CLEAN"],
    "an approval, then a push": ["APPROVED", "CLEAN"],
    "another reviewer's approval, then a change request": ["CHANGES_REQUESTED", "BLOCKED"],
    "another reviewer's approval": ["APPROVED", "CLEAN"],
    "approved, test running": ["APPROVED", "BLOCKED"],
    "approved, test failed": ["APPROVED", "BLOCKED"],
    "approved, lint never reported": ["APPROVED", "BLOCKED"],
    "approved, lint by another App": ["APPROVED", "BLOCKED"],
    "approved, test skipped": ["APPROVED", "CLEAN"],
    "approved, CodeQL failed": ["APPROVED", "BLOCKED"],
    "approved, CodeQL never reported": ["APPROVED", "BLOCKED"],
    "no approval required, none given": [null, "CLEAN"],
    "no approval required, a change request": ["CHANGES_REQUESTED", "CLEAN"],
    "a conflict, no review": ["REVIEW_REQUIRED", "DIRTY"],
  });
  expect(["DIRTY", "UNKNOWN", "CLEAN", "BLOCKED", "BEHIND", "UNSTABLE"].map(mergeableOf)).toEqual([
    "CONFLICTING",
    "UNKNOWN",
    "MERGEABLE",
    "MERGEABLE",
    "MERGEABLE",
    "MERGEABLE",
  ]);
});

// What gh, GraphQL and the REST pull request each say of the world's pull request at each step of
// a round, from the world's own routes and the run's own calls: its review decision, its merge
// state and whether it merges. Catches: a state frozen into the world, which read `CLEAN` before
// any review and after a change request where production's base, requiring an approval, reads
// `REVIEW_REQUIRED` and `BLOCKED`, then `CHANGES_REQUESTED` and `BLOCKED`; an approval that does
// not lift the approval rule, or that lifts the thread rule; a push after the approval that sets
// it aside, where GitHub keeps it (`dismiss_stale_reviews_on_push` false), so that the fix head
// whose threads are resolved reads `APPROVED` and `CLEAN`; and one reader saying another state
// than the others. The approved, green, resolved head reads `CLEAN` and `MERGEABLE`, as
// production's #4346, #4334, #4328 and #4325 read on 2026-11-18 with their merge queue on.
test("each reader of the pull request says GitHub's decision and merge state at every step of a round", () => {
  const read = (dir: string) => {
    const gh = ghCall(
      dir,
      "pr",
      "view",
      "1001",
      "--json",
      "reviewDecision,mergeStateStatus,mergeable"
    );
    const graphql = ghCall(
      dir,
      "api",
      "graphql",
      "-f",
      'query={repository(owner:"example",name:"widgets"){pullRequest(number:1001){reviewDecision mergeStateStatus mergeable}}}',
      "--jq",
      ".data.repository.pullRequest"
    );
    const rest = ghCall(
      dir,
      "api",
      "repos/example/widgets/pulls/1001",
      "--jq",
      "{mergeable, mergeable_state}"
    );
    return {
      codes: [gh.code, graphql.code, rest.code],
      gh: JSON.parse(gh.out),
      graphql: JSON.parse(graphql.out),
      rest: JSON.parse(rest.out),
    };
  };
  const said = (decision: string, state: string) => ({
    codes: [0, 0, 0],
    gh: { reviewDecision: decision, mergeStateStatus: state, mergeable: "MERGEABLE" },
    graphql: { reviewDecision: decision, mergeStateStatus: state, mergeable: "MERGEABLE" },
    rest: { mergeable: true, mergeable_state: state.toLowerCase() },
  });
  const review = (dir: string, event: string) =>
    ghCall(
      dir,
      ...["api", "--method", "POST", REVIEWS, "-f", `commit_id=${builtHead(dir)}`],
      ...["-f", `event=${event}`, "-f", "body=the round's review"]
    ).code;
  const round = worldDir("review_world");
  expect(read(round)).toEqual(said("REVIEW_REQUIRED", "BLOCKED"));
  expect(review(round, "REQUEST_CHANGES")).toBe(0);
  expect(read(round)).toEqual(said("CHANGES_REQUESTED", "BLOCKED"));
  expect(review(round, "APPROVE")).toBe(0);
  // The approval stands and the base requires review threads resolved: the bot's is open.
  expect(read(round)).toEqual(said("APPROVED", "BLOCKED"));
  expect(
    ghCall(
      round,
      ...["api", "graphql", "-f"],
      "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
      ...["-F", "id=PRRT_kwDOrig0001"]
    ).code
  ).toBe(0);
  expect(read(round)).toEqual(said("APPROVED", "CLEAN"));
  const fix = pushFix(round, "docs/cli.md", "the text fix\n");
  expect(
    ghCall(round, "pr", "view", "1001", "--json", "headRefOid", "--jq", ".headRefOid").out
  ).toBe(fix);
  expect(read(round)).toEqual(said("APPROVED", "CLEAN"));
  // A base that requires no resolution: the approval alone lifts its rules.
  const tester = worldDir("tester_proof_world");
  expect(read(tester)).toEqual(said("REVIEW_REQUIRED", "BLOCKED"));
  expect(review(tester, "APPROVE")).toBe(0);
  expect(read(tester)).toEqual(said("APPROVED", "CLEAN"));
}, 120_000);

/** What the reviewer's gh reads of the pull request of the world under `dir`: the review
 * decision and the merge state `gh pr view` prints, and the REST pull request's
 * `mergeable_state`. */
function decidedBy(dir: string): string[] {
  const viewed = asked(dir, "reviewer", [
    "pr",
    "view",
    "1001",
    "--json",
    "reviewDecision,mergeStateStatus",
  ]);
  const rest = asked(dir, "reviewer", [
    "api",
    "repos/example/widgets/pulls/1001",
    "--jq",
    ".mergeable_state",
  ]);
  const { reviewDecision, mergeStateStatus } = JSON.parse(String(viewed[1]));
  return [reviewDecision, mergeStateStatus, String(rest[1]).trim()];
}
/** The reviewer's approval of `sha` in the review world under `dir`, then its resolution of the
 * bot's seeded thread (rig.sh's review_world), the one other rule of the base that holds the head
 * `BLOCKED`: each call's status. */
function approvedAndResolved(dir: string, sha: string): unknown[] {
  return [
    asked(dir, "reviewer", [
      ...["api", "--method", "POST", REVIEWS, "-f", `commit_id=${sha}`],
      ...["-f", "event=APPROVE", "-f", "body=Approved."],
    ])[0],
    asked(dir, "reviewer", [
      ...["api", "graphql", "-f"],
      "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
      ...["-F", "id=PRRT_kwDOrig0001"],
    ])[0],
  ];
}

// GitHub's last-push rule, as its REST description of the rulesets API's `pull_request` rule
// gives it: "Whether the most recent reviewable push must be approved by someone other than the
// person who pushed it" (gh-threads.ts's `reviewDecisionOf`). last-push's base alone sets it
// (rig.sh's review_world), and its head is the implementer's push, so the reviewer's approval is
// another's and lifts the approval rule, and with the bot's thread resolved the head is `CLEAN`.
// Catches: a rule that refuses every approval, so that last-push's reviewer, which approves the
// head as it stands (Verification v47's k3), could lift nothing.
test("on last-push's base an approval by a login other than the head's last pusher counts", () => {
  const dir = worldDir("review_world", "", { worldCase: "last-push" });
  const head = builtHead(dir);
  expect(pusherOf(dir, head)).toBe(`${rigValue("world_implement_app")}[bot]`);
  expect(decidedBy(dir)).toEqual(["REVIEW_REQUIRED", "BLOCKED", "blocked"]);
  expect(approvedAndResolved(dir, head)).toEqual([0, 0]);
  expect(decidedBy(dir)).toEqual(["APPROVED", "CLEAN", "clean"]);
}, 120_000);

// The rule's other half: an approval by the account that made the head's last push counts for
// nothing. The reviewer pushes its own text fix as the review App's bot, which the world links the
// pushed head to (rest-objects.ts's `pusherOf`), and approves it: on last-push's base the decision
// stays `REVIEW_REQUIRED` and the head `BLOCKED` with every thread resolved, where text-only's
// base, which asks no approval of the last push, as production's asks none, takes the same
// approval. Catches: the rule read as no rule, the pusher read off another commit than the head,
// and the rule set on a base other than last-push's.
test("on last-push's base an approval by the login that made the head's last push counts for nothing", () => {
  const ruled = worldDir("review_world", "", { worldCase: "last-push" });
  const fix = pushFix(ruled, "docs/cli.md", "the text fix\n", "review");
  expect(pusherOf(ruled, fix)).toBe(`${rigValue("world_review_app")}[bot]`);
  expect(approvedAndResolved(ruled, fix)).toEqual([0, 0]);
  expect(decidedBy(ruled)).toEqual(["REVIEW_REQUIRED", "BLOCKED", "blocked"]);
  const unruled = worldDir("review_world");
  const own = pushFix(unruled, "docs/cli.md", "the text fix\n", "review");
  expect(approvedAndResolved(unruled, own)).toEqual([0, 0]);
  expect(decidedBy(unruled)).toEqual(["APPROVED", "CLEAN", "clean"]);
}, 120_000);

// GitHub's code owner review: "any pull request that modifies content with a code owner must be
// approved by that code owner" (docs.github.com, Available rules for rulesets, read 2026-11-19),
// and content has a code owner only where the base branch's `.github/CODEOWNERS`, `CODEOWNERS`
// or `docs/CODEOWNERS` names one (About code owners). Production's `main` has required it since
// 2026-11-18T18:19:22Z (ruleset 12331919, read 2026-11-19T05:35:00Z), so every world's base does
// (rig.sh's base_ruleset_routes), and no world's repository holds a CODEOWNERS file (INFRA-764's
// ruling of 2026-11-19): the rule asks no approval of its own, and one approval lifts the
// approval rule of a round whose diff changes docs/cli.md (gh-threads.ts's `reviewDecisionOf`).
// Catches: the parameter read as anything but GitHub's rule — as one more approval, or as an
// owner's approval no world can give — which holds an approved world `REVIEW_REQUIRED` and
// `BLOCKED` where production's approval lifts the rule; and a world whose base holds a CODEOWNERS
// file, whose owners nothing of the world reads.
test("the base requires a code owner's review, and with no CODEOWNERS file the approval alone lifts the rule", () => {
  const round = worldDir("review_world");
  const rules = JSON.parse(ghCall(round, "api", "repos/example/widgets/rules/branches/main").out);
  expect(rules.find((rule: { type: string }) => rule.type === "pull_request")).toMatchObject({
    parameters: { required_approving_review_count: 1, require_code_owner_review: true },
  });
  const remote = path.join(round, "remote.git");
  const locations = [".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"];
  expect(git(remote, "ls-tree", "--name-only", "main", "--", ...locations)).toBe("");
  expect(ghCall(round, "pr", "diff", "1001", "--name-only").out).toContain("docs/cli.md\n");
  const state = () =>
    JSON.parse(
      ghCall(round, "pr", "view", "1001", "--json", "reviewDecision,mergeStateStatus").out
    );
  expect(state()).toEqual({ reviewDecision: "REVIEW_REQUIRED", mergeStateStatus: "BLOCKED" });
  const approved = ghCall(
    round,
    ...["api", "--method", "POST", REVIEWS, "-f", `commit_id=${builtHead(round)}`],
    ...["-f", "event=APPROVE", "-f", "body=Approved."]
  );
  expect(approved.code).toBe(0);
  // The base requires review threads resolved too, and the bot's thread is open.
  expect(state()).toEqual({ reviewDecision: "APPROVED", mergeStateStatus: "BLOCKED" });
  const resolved = ghCall(
    round,
    ...["api", "graphql", "-f"],
    "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
    ...["-F", "id=PRRT_kwDOrig0001"]
  );
  expect(resolved.code).toBe(0);
  expect(state()).toEqual({ reviewDecision: "APPROVED", mergeStateStatus: "CLEAN" });
}, 120_000);

// GitHub refuses every push that updates or deletes a base its ruleset protects, naming each rule
// the push breaks, and takes a push to any other branch: production's `main` ruleset holds
// `deletion`, `non_fast_forward` and `required_linear_history` beside a pull request rule, two
// required checks and a merge queue, which every update of the base breaks (measured for a plain
// push on 2026-11-12, INFRA-2713's sandbox: `GH013`, `Changes must be made through a pull
// request.`, `Required status check "ci" is expected.`, `Changes must be made through the merge
// queue`; the branch rules' own lines are GitHub's form as public reports print it, unmeasured).
// Catches: a world's remote that takes a force push, a deletion or a merge commit onto its base,
// each a success production never gives; one that refuses them as git's own hook does,
// `(pre-receive hook declined)`, which no GitHub push prints; one that names a rule the push
// does not break, or not one it does; and one that refuses a push to the issue branch.
test("the remote refuses a force push, a deletion and a merge commit to the base as GitHub does, and takes the issue branch's push", () => {
  const dir = worldDir("tester_proof_world");
  const remote = path.join(dir, "remote.git");
  const work = path.join(dir, "rules-work");
  git(dir, "clone", "-q", remote, work);
  const base = git(remote, "rev-parse", "refs/heads/main");
  const push = (...args: string[]) => {
    const made = spawnChecked(
      [
        "git",
        "-c",
        "user.name=t",
        "-c",
        "user.email=t@example.invalid",
        "-C",
        work,
        "push",
        ...args,
      ],
      { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
    );
    const lines = made.stderr
      .toString()
      .split("\n")
      .filter((line) => line.startsWith("remote:") || line.startsWith(" ! "))
      .map((line) => line.trimEnd());
    return { code: made.exitCode, lines };
  };
  const heading = [
    "remote: error: GH013: Repository rule violations found for refs/heads/main.",
    "remote: Review all repository rules at http://github.com/example/widgets/rules?ref=refs%2Fheads%2Fmain",
    "remote:",
  ];
  const through = [
    "remote: - Changes must be made through a pull request.",
    "remote:",
    "remote: - 2 of 2 required status checks are expected.",
    "remote:",
    "remote: - Changes must be made through the merge queue",
    "remote:",
  ];
  const rejected = (refs: string) =>
    ` ! [remote rejected] ${refs} (push declined due to repository rule violations)`;
  git(work, "commit", "-q", "--allow-empty", "-m", "a direct change");
  expect(push("origin", "HEAD:main")).toEqual({
    code: 1,
    lines: [...heading, ...through, rejected("HEAD -> main")],
  });
  git(work, "checkout", "-q", "--orphan", "rewritten");
  git(work, "commit", "-q", "--allow-empty", "-m", "a rewritten history");
  expect(push("--force", "origin", "rewritten:main")).toEqual({
    code: 1,
    lines: [
      ...heading,
      "remote: - Cannot force-push to this branch",
      "remote:",
      ...through,
      rejected("rewritten -> main"),
    ],
  });
  git(work, "checkout", "-q", "-b", "side", base);
  git(work, "commit", "-q", "--allow-empty", "-m", "a side change");
  git(work, "checkout", "-q", "-b", "merged", base);
  git(work, "commit", "-q", "--allow-empty", "-m", "a main change");
  git(work, "merge", "-q", "--no-ff", "-m", "merge the side change", "side");
  const merge = git(work, "rev-parse", "HEAD");
  expect(push("origin", "merged:main")).toEqual({
    code: 1,
    lines: [
      ...heading,
      "remote: - This branch must not contain merge commits.",
      "remote:   Found 1 violation:",
      "remote:",
      `remote:   ${merge}`,
      "remote:",
      ...through,
      rejected("merged -> main"),
    ],
  });
  expect(push("origin", ":main")).toEqual({
    code: 1,
    lines: [...heading, "remote: - Cannot delete this branch", "remote:", rejected("main")],
  });
  expect(git(remote, "rev-parse", "refs/heads/main")).toBe(base);
  git(work, "checkout", "-q", "-b", "fix", "origin/legion/LWEVAL-1");
  git(work, "commit", "-q", "--allow-empty", "-m", "the fix");
  expect(push("origin", "fix:legion/LWEVAL-1")).toEqual({ code: 0, lines: [] });
  expect(git(remote, "rev-parse", "refs/heads/legion/LWEVAL-1")).toBe(
    git(work, "rev-parse", "HEAD")
  );
}, 60_000);

/** rig.sh's own top-level value of `name`, as its assignment line writes it, unquoted. */
const rigValue = (name: string) =>
  assignment(name)
    .replace(/^[^=]+=/, "")
    .replace(/^'(.*)'$/, "$1");
/** The identity production's daemon gives a pane acting as the `implement` or the `review` App
 * (appauth.go's `botIdentity`): the App's bot login, and the noreply address of its bot user,
 * read off rig.sh's two Apps. */
function botOf(app: "implement" | "review"): string {
  const slug = rigValue(`world_${app}_app`);
  return `${slug}[bot] <${rigValue(`world_${app}_app_id`)}+${slug}[bot]@users.noreply.github.com>`;
}
/** The identity of the operator a run is started by, in the six variables a Legion pod carries
 * its own App in (runtime.go's `GitIdentity.Env`): a run that carried it into the world's history
 * would hand every agent the identity of the pod that ran the rig. */
const OPERATOR = Object.fromEntries(
  ["JJ_USER", "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"]
    .map((name) => [name, "operator-app[bot]"])
    .concat(
      ["JJ_EMAIL", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"].map((name) => [
        name,
        "1+operator-app[bot]@users.noreply.github.com",
      ])
    )
);

// Production gives each role its App's bot identity, `<slug>[bot]` and
// `<id>+<slug>[bot]@users.noreply.github.com` (packages/daemon/internal/appauth's `AppRoleFor` and
// `botIdentity`), so the implementer's commit on an issue branch is the implement App's, and the
// default branch's history is the repository's own. Measured in INFRA-764 round 24: every seeded
// commit and every pane wrote as one developer, and agents read that as "the harness looks like
// it's using robin.hale as the default committer" (preflight-r24/reviewer-text-findings-defect-
// base-1); and round 24's preflight seeded its commits as the pod's own App, the operator's
// GIT_AUTHOR_* winning over the world's `-c user.name`. Catches: a seeded commit of one author,
// the implementer's commit not the implement App's, the reviewer's own text commit (correct's,
// rig.sh's fixture_reviewer_commit) not the review App's, and the operator's identity in the
// world's history.
test("a seeded commit carries the identity of the role that made it, and none of the operator's", () => {
  const dir = worldDir("approved_world", "", { env: OPERATOR, worldCase: "correct" });
  const author = `${rigValue("world_author_name")} <${rigValue("world_author_email")}>`;
  // Every commit of the world carries one date (rig.sh's fixture_git), so the log is read child
  // before parent, which a date order leaves to chance.
  expect(
    git(
      path.join(dir, "remote.git"),
      ...["log", "--all", "--topo-order", "--format=%s|%an <%ae>|%cn <%ce>"]
    ).split("\n")
  ).toEqual([
    `docs: say what the CLI does with the spaces around the name|${botOf("review")}|${botOf("review")}`,
    `feat: greet(), a greeting helper for the widgets CLI (LWEVAL-1)|${botOf("implement")}|${botOf("implement")}`,
    `init|${author}|${author}`,
  ]);
}, 60_000);

// The brainstorm run's to-do CLI is a repository of its own developer's, and its one commit
// carries that developer, given as fixture_git gives a world's: round 24's preflight seeded the
// widgets worlds as the pod's own App, the operator's GIT_AUTHOR_* winning over `-c user.name`,
// and run_brainstorm committed the same way. Catches: the operator's identity in the to-do CLI's
// history, as author or as committer.
test("the brainstorm repository's commit carries its developer, and none of the operator's", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-brainstorm-"));
  made.push(dir);
  mkdirSync(path.join(dir, "cwd"));
  const built = spawnChecked(
    [
      "bash",
      "-c",
      [
        fn("run_brainstorm"),
        "own_dispatch() { :; }",
        "dispatch_session() { :; }",
        ...["world_author_name", "world_author_email"].map(assignment),
        "R=$1",
        "run_brainstorm brainstorm-1",
      ].join("\n"),
      "-",
      dir,
    ],
    { env: { ...process.env, ...OPERATOR, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  const author = `${rigValue("world_author_name")} <${rigValue("world_author_email")}>`;
  expect([
    built.exitCode,
    built.stderr.toString(),
    git(path.join(dir, "cwd"), "log", "--format=%s|%an <%ae>|%cn <%ce>"),
  ]).toEqual([0, "", `A tiny to-do CLI|${author}|${author}`]);
}, 60_000);

// A pane's commit carries its role's App, author and committer alike, with jj and with git, as
// production's runtime gives a pane the six variables of its App (runtime.go's
// `GitIdentity.Env`) and adopts the shared workspace's working copy for each role at its start
// (packages/daemon/internal/shim's `runAdoption`: an undescribed working copy takes the role's
// author, a described one is kept and the role starts a new one). jj sets a change's author when
// it makes the change, so without the adoption the reviewer's prose commit carried the author of
// the change the implementer's commit left. Catches: every role one identity, a role's commit
// authored by the role before it or by the fixture, the operator's identity, a git commit of the
// pane under any other identity, and a described working copy rewritten to the next role.
test("a pane's commit carries its role's App as author and committer, and two roles differ", () => {
  const dir = worldDir("approved_world", "", {
    workspace: true,
    env: OPERATOR,
    worldCase: "correct",
  });
  writeFileSync(path.join(dir, "jj.toml"), '[ui]\npaginate = "never"\n');
  const shown =
    'author.name() ++ " <" ++ author.email() ++ ">|" ++ committer.name() ++ " <" ++ committer.email() ++ ">"';
  const pane = spawnChecked(
    [
      "bash",
      "-c",
      [
        ...["role_identity", "role_env", "adopt_workspace"].map(fn),
        ...WORLD_VALUES.map(assignment),
        "R=$1",
        // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own array expansion
        'as_role() { local identity; mapfile -t identity < <(role_env "$1"); shift; (cd "$R/ws" && env "${identity[@]}" JJ_CONFIG="$R/jj.toml" "$@"); }',
        `show() { as_role "$1" jj log --no-graph -r "$2" -T '${shown} ++ "\\n"'; }`,
        'commit() { adopt_workspace "$1" && printf "%s\\n" "$1" >"$R/ws/$1.md" && as_role "$1" jj commit -m "docs: $1" >/dev/null 2>&1 && show "$1" @-; }',
        "commit implementer && commit reviewer",
        'as_role reviewer jj describe -m "wip" >/dev/null 2>&1 && adopt_workspace implementer && show implementer @- && show implementer @',
        'git clone -q "$R/remote.git" "$R/git-pane" && mapfile -t identity < <(role_env tester)',
        // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own array expansion
        'env "${identity[@]}" git -C "$R/git-pane" commit --allow-empty -qm "a tester note" && git -C "$R/git-pane" log -1 --format="%an <%ae>|%cn <%ce>"',
      ].join("\n"),
      "-",
      dir,
    ],
    { env: { ...process.env, ...OPERATOR, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  const implementer = `${botOf("implement")}|${botOf("implement")}`;
  const reviewer = `${botOf("review")}|${botOf("review")}`;
  expect(botOf("implement")).not.toBe(botOf("review"));
  expect([pane.exitCode, pane.stdout.toString().trim().split("\n")]).toEqual([
    0,
    [implementer, reviewer, reviewer, `${botOf("implement")}|${botOf("implement")}`, reviewer],
  ]);
}, 60_000);

const CHECKOUT = path.resolve(import.meta.dir, "../../../..");

// Every row below runs gh-standin.ts as a scenario's agent does, a process of its own, against a
// run directory holding the row's own fixtures.json, which the stand-in reads from GH_DATA_DIR.
const work = temporaryDirectory("gh-standin");
let runs = 0;

/** A new run directory whose fixtures.json holds `routes`. */
function runWith(routes: object[]): string {
  const run = path.join(work, `run-${++runs}`);
  mkdirSync(run);
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
  return run;
}

/** `gh <args>`, answered by the stand-in from the fixtures of `run`. */
function gh(run: string, ...args: string[]) {
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    { env: { PATH: process.env.PATH ?? "", GH_DATA_DIR: run } }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

// `--json` picks the fields it names of each object of a list answer, as `gh pr list --json` does,
// and of an object answer, and `--jq` reads the picked list.
test("--json picks the named fields of each object of a list answer and of an object answer", () => {
  const run = runWith([
    {
      match: "^gh pr list",
      stdout: [
        { number: 7, url: "https://example.invalid/7", body: "seven" },
        { number: 8, url: "https://example.invalid/8", body: "eight" },
      ],
    },
    {
      match: "^gh pr view",
      stdout: { number: 7, url: "https://example.invalid/7", body: "seven" },
    },
  ]);
  expect(gh(run, "pr", "list", "--json", "number,url")).toEqual({
    code: 0,
    stdout:
      '[{"number":7,"url":"https://example.invalid/7"},{"number":8,"url":"https://example.invalid/8"}]\n',
    stderr: "",
  });
  expect(gh(run, "pr", "list", "--json", "number", "--jq", ".[].number").stdout).toBe("7\n8\n");
  expect(gh(run, "pr", "view", "7", "--json", "url").stdout).toBe(
    '{"url":"https://example.invalid/7"}\n'
  );
});

// A pull request body edit reaches every object of the pull request, in a list answer as in an
// object answer, so the next `gh pr list --json body` reads the new body, as GitHub's would; an
// answer that is not the pull request's (one carrying no `headRefOid`) is answered as it was.
test("a body edit gives each object of a list answer that has a body the new body", () => {
  const head = "099b05dc19f7f7e5058e791d16ae05f0b123a2cc";
  const run = runWith([
    { match: "^gh pr edit", stdout: "https://example.invalid/7" },
    { match: "^gh pr list", stdout: [{ number: 7, headRefOid: head, body: "old", state: "OPEN" }] },
    { match: "^gh pr view", stdout: { number: 7, headRefOid: head, body: "old", state: "OPEN" } },
    { match: "^gh api .*comments", stdout: [{ id: 1 }] },
  ]);
  // This stand-in takes a body edit of a world's own pull request alone (gh-standin.ts's
  // `prWriteModelled`) and lists that pull request while it is open (`listsPull`), so the run
  // holds a world whose pull request this is, and the selectors its gh refuses an owner by.
  writeFileSync(
    path.join(run, "world.json"),
    JSON.stringify({
      key: "LWEVAL-1",
      repo: "fernbrook/widgets",
      pr: 7,
      branch: "legion/LWEVAL-1",
      base: "main",
      head,
    })
  );
  writeGhSelectors(run);
  expect(gh(run, "pr", "edit", "7", "--body", "new").code).toBe(0);
  expect(gh(run, "pr", "list", "--json", "number,body").stdout).toBe(
    '[{"number":7,"body":"new"}]\n'
  );
  expect(gh(run, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"new"}\n');
  // gh 2.98.0 prints a `gh api` body with no line end (measured 2026-11-18, gh-standin.ts:3455).
  expect(gh(run, "api", "repos/fernbrook/widgets/issues/7/comments").stdout).toBe('[{"id":1}]');
  const edit = readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .trimEnd()
    .split("\n")
    .find((line) => line.includes('"argv"'));
  expect(JSON.parse(edit ?? "{}")).toMatchObject({
    argv: ["pr", "edit", "7", "--body", "new"],
    body: "new",
    exit: 0,
  });
});
