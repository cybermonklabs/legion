import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// GitHub answers the pull request's head as the commit its branch points at, and the worker skill
// has every pushing role read that head after its own push and stop when it is not its commit. A
// stand-in that answers the fixture's first commit for ever stalls every such role: measured on
// 2026-11-06, two runs of the loop that follows an approval pushed their text fixes, read the
// head, found the fixture's
// commit and ended with a WAITING line instead of completing. So the stand-in reads the tip of the
// run's own bare remote at the call, and serves the pull request's diff from it too.
const STANDIN = path.join(import.meta.dir, "gh-standin.ts");
const made: string[] = [];

afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function git(dir: string, ...args: string[]): string {
  const result = Bun.spawnSync(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** A run directory with the pull-request route of rig.sh's worker_fixture. With `fixture` it is
 * the run of a scenario that builds a worker world: a world.json and a bare remote of its own
 * whose issue branch holds two commits. Without it, it is the run of a scenario that builds
 * none, which has neither — rig.sh writes the world and opens the remote together
 * (worker_fixture), so no run of this rig has one and not the other. */
function runDir(options: { fixture: boolean }): { dir: string; first: string; second: string } {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-"));
  made.push(dir);
  const work = path.join(dir, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  const first = git(work, "rev-parse", "HEAD");
  writeFileSync(path.join(work, "docs.md"), "the text fix\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "docs: the text fix");
  const second = git(work, "rev-parse", "HEAD");
  git(work, "update-ref", "refs/heads/legion/LWEVAL-1", first);
  if (options.fixture) {
    git(dir, "clone", "-q", "--bare", work, path.join(dir, "remote.git"));
    git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", first);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: "example/widgets",
        branch: "legion/LWEVAL-1",
        base: "main",
        head: first,
      })
    );
  }
  // The routes rig.sh's fixture_write writes, in the shapes it writes them: the pull request as
  // one object for `gh pr view`, the same object in a list for `gh pr list`, which is GitHub's
  // answer to a listing of the open pull requests of a branch, the edit route a body edit
  // needs a match of before the stand-in keeps the new body, and the route every call that
  // creates something falls to, whose id is what a create's answer carries here — 6001, as in
  // review_world, where approved_world routes a create of its own ahead of it with 5002. That
  // route is chosen by the call's verb and by no regex of its own (`methods`), as fixture_write
  // chooses it. No
  // route for `gh pr diff`, as no world of rig.sh writes one (rig.sh's round_world): the diff is
  // the run's own remote's. No route for the submit of a review either, which no world routes
  // now: the stand-in answers that from what the run has already done (gh-standin.ts's
  // reviewSubmission).
  const pull = { number: 1001, headRefOid: first, head: { sha: first }, body: "a body" };
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      routes: [
        { match: "^gh pr view", stdout: pull },
        { match: "^gh pr list", stdout: [pull] },
        { match: "^gh pr edit", stdout: "https://example.invalid/pull/1001#issuecomment-1" },
        {
          match: "^gh api ",
          methods: ["POST", "PATCH", "PUT"],
          stdout: { id: 6001, html_url: "https://example.invalid/pull/1001#issuecomment-6001" },
        },
        // The reviews already on the pull request, which rig.sh's fixture_write answers with an
        // array too: the route a read of that path falls to, under the POST route above, so a
        // call that names GET is answered here and a call that submits is not.
        { match: "^gh api .*pulls/1001/reviews", stdout: [] },
        // The pull request read through the api, which rig.sh's fixture_write writes last of
        // its pull-request routes, under the reviews route above: the same object, whose head
        // the REST shape carries as `head.sha` beside `headRefOid`.
        { match: "^gh api .*pulls/1001", stdout: pull },
      ],
    })
  );
  return { dir, first, second };
}

const headOf = (dir: string) =>
  Bun.spawnSync(
    ["bun", STANDIN, "gh", "pr", "view", "1001", "--json", "headRefOid", "--jq", ".headRefOid"],
    {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
    }
  )
    .stdout.toString()
    .trim();

/** `gh api --method POST <route>` with `rest` after it, in the run under `dir`: the create of a
 * review and the submit of a pending one, as the reviewer's prompt makes them. */
const apiPost = (dir: string, route: string, ...rest: string[]) =>
  Bun.spawnSync(["bun", STANDIN, "gh", "api", "--method", "POST", route, ...rest], {
    env: { ...process.env, SKILL_SCENARIO_RUN: dir },
  });
const REVIEWS = "repos/example/widgets/pulls/1001/reviews";
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
// approved correctly at the head its push made (the text-fixes head arm of 2026-11-11, one run of
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
// answers the review itself, carrying the head it was created at and the state GitHub leaves it
// in — which is what keeps the two-step route an agent may take readable as the submission it is.
test("a submit of the run's pending review answers its id, its head and the state submitted", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=REQUEST_CHANGES");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
    state: "CHANGES_REQUESTED",
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
  expect(JSON.parse(submit.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
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
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// What GitHub answers a create with is the review it created, carrying the state it is in and
// the head it was created at: `PENDING` where the create named no event, and the state the
// event leaves it in where it named one. Catches: a world's frozen `{id, html_url}` answered as
// it stands, which told a reviewer nothing about whether its approval had been submitted —
// measured in the threads-closed head arm, where a round read that answer back and submitted a
// second time.
test("a create is answered with the review's state and the head it was created at", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const pending = apiPost(dir, REVIEWS, "-f", `commit_id=${second}`);
  expect(JSON.parse(pending.stdout.toString())).toEqual({
    id: 6001,
    html_url: "https://example.invalid/pull/1001#issuecomment-6001",
    state: "PENDING",
    commit_id: second,
  });
  const approved = apiPost(dir, REVIEWS, "-f", `commit_id=${first}`, "-f", "event=APPROVE");
  expect(JSON.parse(approved.stdout.toString())).toMatchObject({
    state: "APPROVED",
    commit_id: first,
  });
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

/** `gh <args…>` in the run under `dir`, with its status, its stdout and its stderr: the rows
 * below read a refusal's own text as well as the answer. */
const ghCall = (dir: string, ...args: string[]) => {
  const made = Bun.spawnSync(["bun", STANDIN, "gh", ...args], {
    env: { ...process.env, SKILL_SCENARIO_RUN: dir },
  });
  return {
    code: made.exitCode,
    out: made.stdout.toString().trim(),
    err: made.stderr.toString(),
  };
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
  expect(JSON.parse(submitted.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// Catches: a stand-in that keeps the first create as the run's pending review, where a round
// that read the head again and created a second review would have its submit answered at the
// head it had before that reading. The pending review is the last create that named no event,
// as it is on GitHub, where each create leaves a review of its own and this rig's worlds answer
// every create with one id.
test("two pending creates leave the last one, and the submit answers the head it carries", () => {
  const { dir, first, second } = runDir({ fixture: true });
  createPending(dir, { commit_id: second, body: "a first reading" });
  createPending(dir, { commit_id: first, body: "two findings" });
  const submitted = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=REQUEST_CHANGES");
  expect(submitted.exitCode).toBe(0);
  expect(JSON.parse(submitted.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
    state: "CHANGES_REQUESTED",
  });
});

// The verb decides what the call is, in every spelling gh takes it in and by the reader the
// score reads the same call with (score.ts's `isPost`): a call naming GET is a read of that
// path whatever its fields say, so it submits nothing and falls to the routes, where the
// listing of the pull request's reviews answers it; and the review it did not submit is still
// pending for the call that does, which is written here with the leading slash `gh api` takes
// either way. Catches: a GET answered as the submission of the pending review, which gh never
// sent and GitHub never answers; a GET read as the submit that leaves no pending review behind
// it, where the submit after it would be refused 422; and a submit read as none for its
// leading slash, which the generic POST route would then answer `{id:6001}` with no head on
// it — the approval at no head this endpoint exists to stop.
test("a GET of the submit route submits nothing, and a submit with a leading slash submits", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  for (const verb of [["-X", "GET"], ["--method=GET"], ["-XGET"]]) {
    const read = ghCall(dir, "api", ...verb, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
    expect(read.code).toBe(0);
    expect(read.out).toBe("[]");
    expect(read.err).toBe("");
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
  expect(JSON.parse(submitted.out)).toEqual({ id: 6001, commit_id: first, state: "APPROVED" });
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
  expect(JSON.parse(submitted.out)).toEqual({ id: 6001, commit_id: first, state: "APPROVED" });
});
// A call on the submit path that gh does not send as a POST submits nothing: `-X PUT` names
// another verb, and a call with no method and no field is the GET gh sends by default. Both
// fall to the routes — the first to the route every write falls to, the second to the listing
// of the reviews — and the review stays pending for the call that does submit it. Catches: a
// verb read as "anything but GET", which would answer the PUT as the round's submission, and
// one read as "a POST unless it says otherwise", which would answer the bare call that way.
test("a PUT of the submit route and a bare one submit nothing", () => {
  const { dir, first } = runDir({ fixture: true });
  createPending(dir, { commit_id: first, body: "two findings" });
  const put = ghCall(dir, "api", "-X", "PUT", `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(put.code).toBe(0);
  expect(JSON.parse(put.out)).toEqual({
    id: 6001,
    html_url: "https://example.invalid/pull/1001#issuecomment-6001",
  });
  const bare = ghCall(dir, "api", `${REVIEWS}/6001/events`);
  expect(bare.code).toBe(0);
  expect(bare.out).toBe("[]");
  const submitted = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submitted.exitCode).toBe(0);
  expect(JSON.parse(submitted.stdout.toString())).toEqual({
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
  expect(JSON.parse(commented.stdout.toString())).toEqual({
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
// answers a submit on another repository's pull request from this run's record.
test("a create and a submit on another repository's pull request are no review of this run", () => {
  const { dir, first } = runDir({ fixture: true });
  const other = "repos/other/widgets/pulls/1001/reviews";
  expect(apiPost(dir, other, "-f", `commit_id=${first}`).exitCode).toBe(0);
  const elsewhere = apiPost(dir, `${other}/6001/events`, "-f", "event=APPROVE");
  expect(elsewhere.exitCode).toBe(0);
  expect(JSON.parse(elsewhere.stdout.toString())).toEqual({
    id: 6001,
    html_url: "https://example.invalid/pull/1001#issuecomment-6001",
  });
  const ours = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(ours.exitCode).toBe(1);
  expect(ours.stderr.toString()).toContain("Can not submit a review that is not pending.");
});
// `gh pr list --open` is a call of the run, not rig.sh asking for the record's opening line.
// That ask is `--open` on its own, in the two forms worker_boot can reach this file by, and a
// stand-in that took the flag wherever it fell — or at the end of a call of any length —
// answered this listing with nothing, recorded no call, and left the score reading a role that
// looked for its pull request as one that made no call at all.
test("gh pr list --open is a call of the run and no ask for the opening line", () => {
  const { dir, first } = runDir({ fixture: true });
  const listed = ghCall(dir, "pr", "list", "--open");
  expect(listed.code).toBe(0);
  expect(JSON.parse(listed.out)).toEqual([
    { number: 1001, headRefOid: first, head: { sha: first }, body: "a body" },
  ]);
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

/** What the stand-in answers for `gh <args...>` in run directory `dir`, as `by` if the run names
 * an agent. */
const answer = (dir: string, args: string[], by?: string) =>
  Bun.spawnSync(["bun", STANDIN, "gh", ...args], {
    env: {
      ...process.env,
      SKILL_SCENARIO_RUN: dir,
      ...(by === undefined ? {} : { SKILL_SCENARIO_AGENT: by }),
    },
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
    const call = Bun.spawnSync(["bun", STANDIN, "gh", "pr", "view", "1001"], {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
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

// Catches: a record nothing opens, where a run whose agents called no gh and no bun leaves no
// calls.jsonl and scores as the rig error of a record nothing wrote rather than as the run that
// called nothing (score.ts's `unscored`). rig.sh's worker_boot asks for that line with `--open`
// before its agent runs, as it starts the publish recorder, which writes its own `recording`
// line; a second ask adds nothing, since the record is already open.
test("--open opens the record with one line and records no call", () => {
  const { dir } = runDir({ fixture: true });
  const open = () =>
    Bun.spawnSync(["bun", STANDIN, "--open"], { env: { ...process.env, SKILL_SCENARIO_RUN: dir } });
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
// failed in all 10 text-fixes head runs of that batch. The two heads a comparison names are the
// run's own, so it is read from the run's remote and no route serves it.
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
// commits a repository does not hold, and so does the stand-in, through the routes.
test("a comparison the run's remote cannot resolve is 404, as GitHub answers it", () => {
  const { dir } = runDir({ fixture: true });
  const call = Bun.spawnSync(
    ["bun", STANDIN, "gh", "api", "repos/example/widgets/compare/deadbeef...cafed00d"],
    { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
  );
  expect(call.exitCode).toBe(1);
  expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
});

// Catches: a comparison read off the path alone, whatever verb the call names. The comparison
// is GitHub's read of two refs, and a call that writes to that path is another call: it falls
// to the routes, where the route every write falls to answers it, and a stand-in that answered
// it with the two refs' diff would tell a caller a write it never made had succeeded. The verb
// is read here as the score reads it on the same call (score.ts's `methodOf`), so each spelling
// gh takes is that write: written against the flag, in lower case, and as a field with no
// method at all, which gh sends as a POST.
test("a POST to the compare path is no comparison", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const range = `repos/example/widgets/compare/${first}...${second}`;
  expect(answer(dir, ["api", range, "--jq", ".status"])).toBe("ahead");
  for (const verb of [
    ["--method", "POST"],
    ["--method=post"],
    ["-XPOST"],
    ["-f", "per_page=100"],
  ]) {
    const posted = ghCall(dir, "api", ...verb, range);
    expect(posted.code).toBe(0);
    expect(JSON.parse(posted.out)).toEqual({
      id: 6001,
      html_url: "https://example.invalid/pull/1001#issuecomment-6001",
    });
  }
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
    const call = Bun.spawnSync(
      [
        "bun",
        STANDIN,
        "gh",
        "api",
        "-H",
        "Accept: application/vnd.github.v3.diff",
        `repos/example/widgets/compare/${refs(`--output=${written}`)}`,
      ],
      { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
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
  const call = Bun.spawnSync(
    ["bun", STANDIN, "gh", "api", `repos/example/widgets/compare/main...--output=${written}`],
    { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
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

/** The stderr the route a call falls to carries, as a world writes one for the call it wrote
 * that route for (rig.sh's fixture_write gives the base-protection route GitHub's own 404). */
function routeStderr(dir: string, text: string): void {
  const file = path.join(dir, "fixtures.json");
  const fixtures = JSON.parse(readFileSync(file, "utf8")) as {
    routes: { match: string; stderr?: string }[];
  };
  for (const route of fixtures.routes) if (route.match.includes("POST")) route.stderr = text;
  writeFileSync(file, JSON.stringify(fixtures));
}

// Catches: a stand-in that prints the matched route's stderr beside its own answer to a submit.
// The submit of a pending review is answered from what this run has already done and not from
// a route, so the text of whatever route the call fell to — the generic POST, written for the
// calls that create things — belongs to another call: a reviewer that submitted its pending
// review would read a refusal on stderr beside the review object and the 0 it exited with, and
// read the approval it had just made as a call that failed.
test("a submit answered from the run's own record prints no route's stderr", () => {
  const { dir, first } = runDir({ fixture: true });
  routeStderr(dir, "gh: Unprocessable Entity (HTTP 422)\nthe route's own refusal\n");
  createPending(dir, { commit_id: first, body: "two findings" });
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
  expect(JSON.parse(submit.out)).toEqual({ id: 6001, commit_id: first, state: "APPROVED" });
});

// Catches: a stand-in that reads a create's body out of a file alone. `gh api --input -` takes
// the body on stdin, which is how an agent sends one it built in the same command, often as the
// several lines a formatted JSON object is written over; the stand-in reads it there, records
// what it read (`stdin`) and reads the create through the score's own `sentBody`. One blind to
// stdin leaves that create no pending review, and the call that submits it is refused 422 — the
// round's own approval answered as a submission GitHub never took.
test("a create whose body is on stdin as JSON lines leaves a pending review", () => {
  const { dir, first } = runDir({ fixture: true });
  const created = Bun.spawnSync(
    ["bun", STANDIN, "gh", "api", "--method", "POST", REVIEWS, "--input", "-"],
    {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
      stdin: new TextEncoder().encode(
        `${JSON.stringify({ commit_id: first, body: "two findings" }, null, 2)}\n`
      ),
    }
  );
  expect(created.exitCode).toBe(0);
  const submit = apiPost(dir, `${REVIEWS}/6001/events`, "-f", "event=APPROVE");
  expect(submit.exitCode).toBe(0);
  expect(JSON.parse(submit.stdout.toString())).toEqual({
    id: 6001,
    commit_id: first,
    state: "APPROVED",
  });
});

// Catches: a create route matched on a path a call merely starts with. The create of a review
// is `…/pulls/<n>/reviews` and nothing below it: `…/pulls/<n>/reviews/<id>/comments` posts a
// comment under a review of its own, which creates no review and leaves none pending. A
// stand-in that read it as a create would answer the next submit with a review object at the
// head that comment was posted at — the approval at a head nobody approved that this endpoint
// exists to stop.
test("a POST under a review's own path is no create of a pending review", () => {
  const { dir } = runDir({ fixture: true });
  const comment = apiPost(dir, `${REVIEWS}/5002/comments`, "-f", "body=the sentence still stands");
  expect(comment.exitCode).toBe(0);
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
