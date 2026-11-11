#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, the agent that made the call when the run names one
// ($SKILL_SCENARIO_AGENT, a run that boots one agent per role), and the pull request body it
// kept, if any), then answered from the
// first route in $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "gh <argv...>". Three
// answers are the run's own remote rather than a route's, since each is a fact of the run and no
// frozen answer could tell one run from another: the pull request's head, which a route carries
// and this answer replaces, and its diff and the comparison of two commits, which need no route
// at all (`branchDiff`, `comparison`). A fourth answer is this file's own and no world's: the
// submit of a review (`…/reviews/<id>/events`), which GitHub answers from the review's own
// state (`reviewSubmission`). A call no route matches and no answer of this file's own serves
// prints `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere.
// `--json a,b` picks fields and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr
// edit --body`, `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an
// object that has a `body` then answers with the new one, as GitHub's next read would, and the
// call's record carries it as `body`.
//
// The record opens with one line of its own, `{at, opened}`, written the first time this
// stand-in is invoked, as record-publishes.ts writes its `recording` line. rig.sh asks for that
// line alone before each worker run's agent starts (`--open`, its worker_boot), and it asks
// through the `gh` the pane's PATH resolves to rather than by this file's path, so the line says
// what the score reads it as: this run's own gh reached the stand-in. So a
// run whose
// agents called no gh and no bun leaves the record there holding that line and nothing else —
// the reading "no call" — while a record nothing wrote is absent, which is the rig error it has
// always been (score.ts's `unscored`). No score reads the line itself.
//   gh-standin.ts gh <args...>
//   gh-standin.ts gh --open   (what `$R/bin/gh --open` runs: the wrapper inserts the program word)
//   gh-standin.ts --open
import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";
// How a call's verb and its body are read is the score's own reader, imported rather than
// written again here: a call this stand-in answered as a submission and the score read as a
// read of the route — or the other way round — is a run scored against a world that answered it
// something else.
import { isPost, sentBody } from "./score";

/** rig.sh's worker_fixture writes each route as {match, stdout?, stderr?, exit?}. */
const Fixtures = z.looseObject({
  routes: z.array(
    z.object({
      match: z.string(),
      stdout: z.unknown().optional(),
      stderr: z.string().optional(),
      exit: z.number().optional(),
    })
  ),
});

/** The part of rig.sh's <run>/world.json this stand-in reads: the issue branch of the run's own
 * bare remote, whose tip is the pull request's head, and the branch it is open against, which is
 * the other side of its diff. Neither has a default: rig.sh's fixture_write writes both into every
 * world it builds, so a world missing one is a world this stand-in cannot serve. */
const World = z.looseObject({ branch: z.string(), base: z.string() });

const run = process.env.SKILL_SCENARIO_RUN;
if (!run) {
  console.error("gh-standin: SKILL_SCENARIO_RUN is unset");
  process.exit(2);
}
/** Opens the record with this stand-in's own line, which says it was there, and does nothing to
 * a record that is already open. */
function openRecord(): void {
  if (existsSync(`${run}/calls.jsonl`)) return;
  appendFileSync(
    `${run}/calls.jsonl`,
    `${JSON.stringify({ at: new Date().toISOString(), opened: "calls.jsonl" })}\n`
  );
}
const invocation = Bun.argv.slice(2);
// `--open` is rig.sh asking for that line alone, before any agent of the run has made a call:
// `$R/bin/gh --open`, the stand-in reached the way the pane's PATH reaches it, where the wrapper
// inserts the program word, and `gh-standin.ts --open` by this file's path. It is that flag on
// its own in either form, so no call of an agent's is taken for it — and `gh --open` is no
// command gh itself answers.
if (invocation.at(-1) === "--open" && invocation.length <= 2) {
  openRecord();
  process.exit(0);
}
const [as = "gh", ...args] = invocation;
const at = new Date().toISOString();
openRecord();
const fixtures = Fixtures.parse(JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8")));
const routes = fixtures.routes;

let stdin: string | undefined;
const readStdin = async () => {
  stdin ??= await new Response(Bun.stdin.stream()).text();
  return stdin;
};
const files: Record<string, string> = {};
const readSource = async (source: string) =>
  source === "-" ? readStdin() : readFileSync(source, "utf8");
// A pull request body edit: `gh pr edit` takes it from `-b/--body` or `-F/--body-file`, a
// `gh api -X PATCH repos/<o>/<r>/pulls/<n>` from a `body` field or an `--input` JSON object.
const prEdit = as === "gh" && args[0] === "pr" && args[1] === "edit";
const prPatch =
  as === "gh" &&
  args[0] === "api" &&
  /(^|\s)(-X|--method)\s?PATCH(\s|$)/.test(args.join(" ")) &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+$/.test(arg));
let body: string | undefined;
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (prEdit && (arg === "-b" || arg === "--body")) body = next;
  else if (prEdit && (arg === "-F" || arg === "--body-file")) {
    files[next] = await readSource(next);
    body = files[next];
  } else if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field" || arg === "-f" || arg === "--raw-field") {
    const [key = "", ...rest] = next.split("=");
    let value = rest.join("=");
    if ((arg === "-F" || arg === "--field") && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    if (prPatch && key === "body") body = value;
  }
}
const inputIndex = args.indexOf("--input");
const input = inputIndex >= 0 ? args[inputIndex + 1] : undefined;
if (prPatch && input !== undefined) {
  const parsed: unknown = JSON.parse(files[input] ?? "null");
  if (
    typeof parsed === "object" &&
    parsed !== null &&
    "body" in parsed &&
    typeof parsed.body === "string"
  ) {
    body = parsed.body;
  }
}
const joined = `${as} ${args.join(" ")}`;
const route = routes.find((candidate) => new RegExp(candidate.match, "s").test(joined));
/** Records the call with the status it exits with, then exits. `by` is the agent the run gave
 * this call's environment, so a run that boots one agent per role has each call say which role
 * made it; a run that names none records no `by`. */
function exit(status: number): never {
  const by = process.env.SKILL_SCENARIO_AGENT;
  appendFileSync(
    `${run}/calls.jsonl`,
    `${JSON.stringify({ at, as, by, argv: args, files, stdin, route: route?.match ?? null, body: kept, exit: status })}\n`
  );
  process.exit(status);
}
/** The run's world (rig.sh's worker_fixture), or undefined when the run has none: a scenario
 * that builds no world writes no world.json, and a call of its own reads the fixture's answer.
 * Only a missing file is "no world". A world.json that does not parse is the rig's own fault, and
 * reading it as none would serve the fixture's frozen head and diff to every call: a pushing role
 * would read a head that is not its own and stop, and a reviewer would read the diff without the
 * fix it was sent to read, with nothing anywhere saying why. So it exits with an error instead. */
function worldOf(): z.infer<typeof World> | undefined {
  if (!existsSync(`${run}/world.json`)) return undefined;
  const text = readFileSync(`${run}/world.json`, "utf8");
  let why = "";
  try {
    const read = World.safeParse(JSON.parse(text));
    if (read.success) return read.data;
    why = read.error.message;
  } catch (error) {
    why = String(error);
  }
  console.error(`gh-standin: ${run}/world.json is no world: ${why}`);
  process.exit(2);
}
/** What `git -C <run>/remote.git <args...>` prints, or undefined when the command fails: the
 * run's own bare remote, read at the call. A run with no remote of its own fails every call. */
function fromRemote(...args: string[]): string | undefined {
  const read = Bun.spawnSync(["git", "-C", `${run}/remote.git`, ...args]);
  return read.exitCode === 0 ? read.stdout.toString().trim() : undefined;
}
// What GitHub answers for the pull request's head is the commit its branch points at, so a route
// whose object carries `headRefOid` answers the tip of the run's own remote rather than the
// fixture's first commit: a role that pushes and then reads the head, which the worker skill has
// every pushing role do, must see its own commit. The tip is read from the run's bare remote at
// the call, and a remote the run has not got leaves the fixture's value as it is.
function branchTip(): string | undefined {
  const world = worldOf();
  return world === undefined ? undefined : fromRemote("rev-parse", `refs/heads/${world.branch}`);
}
// What GitHub serves for `gh pr diff` is the pull request's diff as it stands, so the answer is
// the diff of the run's own remote at the call, the issue branch against the branch it is open
// against, rather than a diff frozen into the fixture, which would read the same whatever the
// run pushed. A role that reads the pull request after another role pushed to it — the reviewer,
// reading the fix the implementer pushed after an approval that left text threads open — must
// see what was pushed. `--name-only` answers the names alone, as `gh pr diff --name-only` does.
// A run with no world of its own has no remote to read, and its call falls through to the
// routes, where no world of rig.sh writes a `gh pr diff` (its round_world), so what such a call
// gets is the 404 below.
function branchDiff(nameOnly: boolean): string | undefined {
  const world = worldOf();
  if (world === undefined) return undefined;
  return fromRemote("diff", ...(nameOnly ? ["--name-only"] : []), world.base, world.branch);
}
/** `repos/<owner>/<repo>/compare/<base>...<head>`, with the query string `gh api` lets a caller
 * append. A ref may carry dots, and `...` is the separator, so each side is everything up to the
 * last `...`. */
const COMPARE = /^\/?repos\/[^/]+\/[^/]+\/compare\/([^?]+)\.\.\.([^?]+)(?:\?.*)?$/;
/** GitHub's submit endpoint of one review, and the create endpoint of a review of one pull
 * request: `…/pulls/<n>/reviews/<id>/events` and `…/pulls/<n>/reviews`. */
const SUBMIT_REVIEW = /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+\/reviews\/(\d+)\/events$/;
const CREATE_REVIEW = /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+\/reviews$/;
/** Whether the call asks for the comparison as a diff rather than as JSON: `gh api -H "Accept:
 * application/vnd.github.v3.diff"`, the media type the API documents for a diff, and the
 * `vnd.github.diff` spelling GitHub takes for the same answer. */
const DIFF_MEDIA_TYPE = /application\/vnd\.github(?:\.v3)?\.diff/;
/** A ref this stand-in will hand `git`. A compare ref is argv the caller wrote, and `git` reads
 * a leading `-` as an option wherever it falls, so a ref such as `--output=<path>...main` would
 * write a file; `--end-of-options`, which `git diff` does not take, cannot close that, so a ref
 * that starts with `-` is refused instead. There is no privilege boundary in this rig — the
 * agent that writes the ref already runs shell commands of its own — so this is a guard against
 * a ref that reads as an option and no sandbox. A refused ref is no comparison of this world,
 * and the call falls through to the routes, whose 404 is GitHub's answer for a ref it has
 * not got. */
const REF = /^[^-]/;
/** What GitHub answers for `gh api repos/<o>/<r>/compare/<base>...<head>`, read from the run's
 * own remote at the call, or undefined when this call is no such comparison or the remote cannot
 * resolve both refs. The rule has the implementer read that comparison to show its push changes
 * no code that runs (`skills/legion-worker/references/review-threads.md`, step 2), and the real
 * GitHub API answers it: measured on 2026-11-10, `gh api
 * repos/cybermonklabs/legion/compare/756789f8...main` printed `status`, `ahead_by`, `behind_by`
 * and 300 files and exited 0, while this stand-in answered `gh: Not Found (HTTP 404)` and the
 * implementer's comparison failed in all 10 text-fixes head runs of the batch before this. No
 * route serves it and none should: an answer frozen into the fixture would read the same
 * whatever the run pushed, where the two heads a comparison names are the run's own. `ahead_by`
 * and `behind_by` are the commits each side holds that the other has not, `status` is GitHub's
 * word for that pair, and `files` is the diff from the merge base, which is the change the head
 * carries, each file carrying the hunks of its own diff as `patch`, which is the field the
 * reviewer reads a change out of a comparison with and which this answered as `null` through
 * the recorded batch of 2026-11-10: a reviewer that asked for `.patch` got nothing and one fell
 * back to `jj file show`, working around the rig on the read step 3 of review-threads.md rests
 * on. The patch of each file is read with a `git diff` of its own path, since splitting one
 * combined diff on its `diff --git` headers would have to undo git's own quoting of a path; a
 * world of this rig holds four changed files. A call that asks for the diff media type
 * (DIFF_MEDIA_TYPE) is answered with the unified diff itself, as the API answers that `Accept`
 * header, and never with the JSON. Renames are not detected, so a moved file reads as one
 * added and one removed, as
 * GitHub's own answer does when it finds no rename. A ref the remote cannot resolve is no
 * comparison of this world, and the call falls through to the routes, whose 404 is GitHub's own
 * answer for a comparison of refs a repository has not got. */
function comparison(): string | Record<string, unknown> | undefined {
  if (as !== "gh" || args[0] !== "api") return undefined;
  if (/(^|\s)(-X|--method)\s?(POST|PATCH|PUT|DELETE)(\s|$)/.test(joined)) return undefined;
  const named = args.map((arg) => COMPARE.exec(arg)).find((match) => match !== null);
  const [, base = "", head = ""] = named ?? [];
  if (base === "" || head === "") return undefined;
  if (!REF.test(base) || !REF.test(head)) return undefined;
  const range = [`${base}...${head}`];
  // The diff media type asks for the comparison as the diff itself, which is what the run's own
  // remote prints for the range; a remote that cannot resolve the two refs prints nothing and
  // the call falls through to the routes.
  if (DIFF_MEDIA_TYPE.test(joined)) return fromRemote("diff", "--no-renames", ...range);
  const ahead = fromRemote("rev-list", "--count", `${base}..${head}`);
  const behind = fromRemote("rev-list", "--count", `${head}..${base}`);
  const counts = fromRemote("diff", "--numstat", "--no-renames", ...range);
  const letters = fromRemote("diff", "--name-status", "--no-renames", ...range);
  if (ahead === undefined || behind === undefined) return undefined;
  if (counts === undefined || letters === undefined) return undefined;
  const aheadBy = Number(ahead);
  const behindBy = Number(behind);
  const status =
    aheadBy === 0 && behindBy === 0
      ? "identical"
      : behindBy === 0
        ? "ahead"
        : aheadBy === 0
          ? "behind"
          : "diverged";
  const words: Record<string, string> = { A: "added", M: "modified", D: "removed" };
  const statusOf = new Map(
    letters
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => {
        const [letter = "", file = ""] = line.split("\t");
        return [file, words[letter.slice(0, 1)] ?? "modified"] as const;
      })
  );
  const files = counts
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const [added = "", deleted = "", filename = ""] = line.split("\t");
      // A binary file's counts are `-` in numstat, and GitHub reports 0 for one.
      const additions = Number(added) || 0;
      const deletions = Number(deleted) || 0;
      // GitHub's `patch` is the file's hunks alone, with no `diff --git` or `index` header, and
      // it is absent for a file git prints no hunk for, as GitHub leaves it out for a binary.
      const diff = fromRemote("diff", "--no-renames", ...range, "--", filename) ?? "";
      const hunks = diff.search(/^@@/m);
      return {
        filename,
        status: statusOf.get(filename) ?? "modified",
        additions,
        deletions,
        changes: additions + deletions,
        ...(hunks < 0 ? {} : { patch: diff.slice(hunks) }),
      };
    });
  return { status, ahead_by: aheadBy, behind_by: behindBy, total_commits: aheadBy, files };
}
/** The comparison this call asks for, when it asks for one the run's remote can answer. It is
 * read before the route check, since no route answers a comparison and a 404 there reads to the
 * agent as a repository that does not hold the two commits its own run made. */
const compared = comparison();
/** What this stand-in answers a submit with: the review GitHub answers a pending one with, or
 * the 422 it answers every other. */
type Submitted = { review: Record<string, unknown> } | { refusal: string };
/** GitHub's own text for the submit of a review that is no longer pending. */
const NOT_PENDING =
  "gh: Unprocessable Entity (HTTP 422)\nCan not submit a review that is not pending.\n";
/** The state GitHub leaves a review in for each event a submit names, which is what its answer
 * and every later read of that review carry. A submit naming any other event, or none, is left
 * to the routes: what GitHub answers there has not been measured, and an invented refusal is the
 * same defect as an invented success. */
const SUBMITTED_STATE: Record<string, string> = {
  APPROVE: "APPROVED",
  REQUEST_CHANGES: "CHANGES_REQUESTED",
  COMMENT: "COMMENTED",
};
/** `POST …/pulls/<n>/reviews/<id>/events`, GitHub's submit of a review that is still pending.
 *
 * It is answered here rather than by a route because the answer turns on what this run has
 * already done: GitHub answers a pending review with the review itself, carrying the head it was
 * left at and the state the submission leaves it in, and every other id with 422. Almost every
 * review of these worlds is submitted by the call that created it — the create carries an
 * `event` — so a run reaches this endpoint by submitting a review that is no longer pending, and
 * 422 is the answer. A run that created a review without an event left a pending one, and that
 * is the case the review object answers, once.
 *
 * Measured on 2026-11-11, in the text-fixes head arm: a reviewer approved correctly at the head
 * its push made, then made this call again; the create route matched it and answered a review
 * object **with no `commit_id`** — an approval at no head, which GitHub never writes. The score
 * reads the last submission as the one that stands, found no head on it, and failed a run whose
 * reviewer had done its work. The world answering what GitHub refuses is this tree's recurring
 * defect, and the fix belongs here rather than in a reader taught to ignore a malformed
 * submission, which would hide the next invention.
 *
 * The refusal is this stand-in's and no world's, so it covers every world: `review_world` routed
 * no such call, and its generic POST route answered one `{id:6001}` and exit 0 — a reviewer that
 * approved wrongly and then sent `REQUEST_CHANGES` to this endpoint had that change request read
 * as the verdict that stands, which passes the defect control the arm exists to fail. A head
 * check in the score would close that one and open another: a `gh pr review --request-changes`
 * submission names no head at all, so every correct run that takes it would fail. */
function reviewSubmission(): Submitted | undefined {
  const submit = args
    .map((arg) => SUBMIT_REVIEW.exec(arg))
    .find((match): match is RegExpExecArray => match !== null);
  if (as !== "gh" || args[0] !== "api" || submit === undefined || !isPost(args)) return undefined;
  const pending = pendingReview();
  if (pending === undefined || pending.id !== submit[1]) return { refusal: NOT_PENDING };
  const body = sentBody({ argv: args, stdin, files }) ?? {};
  const state = SUBMITTED_STATE[typeof body.event === "string" ? body.event : ""];
  if (state === undefined) return undefined;
  return { review: { id: Number(submit[1]), commit_id: pending.head, state } };
}
/** The review this run has left pending, or `undefined` when it has none: the id GitHub gave it
 * and the head it was created at. A create whose body names no `event` leaves a pending review,
 * the call that submits it leaves none — a submitted review is no longer pending, so a second
 * submit of it is refused as the first submit of any other id is — and a run that created none
 * has none. The record this reads is the stand-in's own, `<run>/calls.jsonl`, which a submit
 * that is the run's first gh call finds unwritten.
 *
 * The id is the one the create's own answer carried, read from the route the stand-in matched
 * that call to (`answeredId`), since the worlds answer a create with ids of their own: 5002 in
 * `approved_world` and 6001 in `review_world` (rig.sh). An id written here would answer for one
 * world and refuse the other.
 *
 * The head is the create's `commit_id`, or the pull request's own head where the create named
 * none, which is the head GitHub creates a review at. A run with no remote of its own has no
 * such head, and a review object naming none is the invention above, so that create leaves no
 * pending review here. */
function pendingReview(): { id: string; head: string } | undefined {
  if (!existsSync(`${run}/calls.jsonl`)) return undefined;
  let pending: { id: string; head: string } | undefined;
  for (const line of readFileSync(`${run}/calls.jsonl`, "utf8").split("\n")) {
    if (line.trim() === "") continue;
    const call = JSON.parse(line) as {
      argv?: string[];
      files?: Record<string, string>;
      stdin?: string;
      route?: string | null;
      exit?: number;
    };
    const argv = call.argv ?? [];
    if (argv[0] !== "api" || !isPost(argv)) continue;
    const submitted = argv
      .map((arg) => SUBMIT_REVIEW.exec(arg))
      .find((match): match is RegExpExecArray => match !== null);
    if (submitted !== undefined) {
      if (call.exit === 0 && pending?.id === submitted[1]) pending = undefined;
      continue;
    }
    if (!argv.some((arg) => CREATE_REVIEW.test(arg))) continue;
    const body = sentBody({ argv, stdin: call.stdin, files: call.files });
    if (body === undefined || "event" in body) continue;
    const id = answeredId(call.route);
    const head = typeof body.commit_id === "string" ? body.commit_id : branchTip();
    if (id !== undefined && head !== undefined) pending = { id, head };
  }
  return pending;
}
/** The review id the answer a recorded call got carried: the `id` of the route the stand-in
 * matched it to, which the record names (`route`, the route's own regex). */
function answeredId(match: string | null | undefined): string | undefined {
  if (match === null || match === undefined) return undefined;
  const stdout = routes.find((candidate) => candidate.match === match)?.stdout;
  if (typeof stdout !== "object" || stdout === null || Array.isArray(stdout)) return undefined;
  const id = (stdout as Record<string, unknown>).id;
  return typeof id === "number" || typeof id === "string" ? String(id) : undefined;
}
const submitted = reviewSubmission();
/** The pull request's diff, where this call asks for one the run's remote can answer. It is read
 * before the route check for the same reason: rig.sh's worlds carry no `gh pr diff` route, since
 * a diff frozen into a fixture would read the same whatever the run pushed, and a 404 here reads
 * to the agent as a pull request carrying no change at all. */
const diffed =
  as === "gh" && args[0] === "pr" && args[1] === "diff"
    ? branchDiff(args.includes("--name-only"))
    : undefined;
if (!route && compared === undefined && diffed === undefined && submitted === undefined) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  exit(1);
}
/** `answer` with `shape` applied to it, or to each of its elements when the answer is a list.
 * `gh pr list` answers the open pull requests as a list of the objects `gh pr view` answers one
 * of (rig.sh's fixture_write), so every rule over an answer's object is a rule over a listed
 * one: while the head, the field selection and the kept body reached the object alone, a role
 * that listed the pull request after a fix push read the head the fixture froze, and a listing
 * after a body edit read the body before it. A value that is no object is its own answer. */
function eachAnswer(answer: unknown, shape: (one: Record<string, unknown>) => unknown): unknown {
  const one = (value: unknown) =>
    typeof value === "object" && value !== null && !Array.isArray(value)
      ? shape(value as Record<string, unknown>)
      : value;
  return Array.isArray(answer) ? answer.map(one) : one(answer);
}
if (body !== undefined) {
  const edited = routes.map((candidate) => {
    const stdout = eachAnswer(candidate.stdout, (one) => ("body" in one ? { ...one, body } : one));
    return stdout === candidate.stdout ? candidate : { ...candidate, stdout };
  });
  writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
  kept = body;
}
// The comparison, where the call asked for one, is the answer; a submit is this stand-in's own
// answer and stands over whatever route matched it, with the review for the one review this run
// left pending and nothing printed for the 422 every other submit gets; every other call is its
// route's.
let output: unknown;
if (compared !== undefined) output = compared;
else if (submitted !== undefined) output = "review" in submitted ? submitted.review : "";
else output = route?.stdout ?? "";
// What GitHub answers for a pull request's head is read from the run's own remote, and only for
// an answer that carries one: the tip is a `git` call, which a call answered by any other route
// never makes.
if (
  (Array.isArray(output) ? output : [output]).some(
    (one) => typeof one === "object" && one !== null && "headRefOid" in one
  )
) {
  const tip = branchTip();
  if (tip !== undefined)
    output = eachAnswer(output, (one) => {
      if (!("headRefOid" in one)) return one;
      const nested =
        "head" in one && typeof one.head === "object" && one.head !== null
          ? { head: { ...one.head, sha: tip } }
          : {};
      return { ...one, headRefOid: tip, ...nested };
    });
}
// An empty diff is GitHub's own answer for a pull request whose branch carries no change, so it
// stands as the answer rather than falling back to the route's.
if (diffed !== undefined) output = diffed === "" ? "" : `${diffed}\n`;
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0) {
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  output = eachAnswer(output, (one) =>
    Object.fromEntries(fields.filter((field) => field in one).map((field) => [field, one[field]]))
  );
}
let text = typeof output === "string" ? output : JSON.stringify(output);
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;
if (filter !== undefined) {
  const jq = Bun.spawnSync(["jq", "-r", filter], { stdin: new TextEncoder().encode(text) });
  text = jq.stdout.toString();
  if (jq.exitCode !== 0) {
    process.stderr.write(jq.stderr.toString());
    exit(1);
  }
}
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route?.stderr && submitted === undefined) process.stderr.write(route.stderr);
// A submit this stand-in answered is GitHub's own answer, whatever the route behind it says: the
// review and 200 for the one review this run left pending, and 422 for every other, which is
// every review of these worlds but one a run left pending itself.
if (submitted !== undefined && "refusal" in submitted) process.stderr.write(submitted.refusal);
exit(submitted === undefined ? (route?.exit ?? 0) : "refusal" in submitted ? 1 : 0);
