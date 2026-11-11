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
// at all (`branchDiff`, `comparison`). A call no route matches and no remote serves prints
// `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere.
// `--json a,b` picks fields and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr
// edit --body`, `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an
// object that has a `body` then answers with the new one, as GitHub's next read would, and the
// call's record carries it as `body`.
//   gh-standin.ts gh <args...>
import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";

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
const [as = "gh", ...args] = Bun.argv.slice(2);
const at = new Date().toISOString();
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
// against, rather than the diff the fixture froze. A role that reads the pull request after
// another role pushed to it — the reviewer, reading the fix the implementer pushed after an
// approval that left text threads open — must see what was pushed. `--name-only` answers the
// names alone, as `gh pr diff --name-only` does. A remote the run has not got leaves the
// fixture's answer as it is.
function branchDiff(nameOnly: boolean): string | undefined {
  const world = worldOf();
  if (world === undefined) return undefined;
  return fromRemote("diff", ...(nameOnly ? ["--name-only"] : []), world.base, world.branch);
}
/** `repos/<owner>/<repo>/compare/<base>...<head>`, with the query string `gh api` lets a caller
 * append. A ref may carry dots, and `...` is the separator, so each side is everything up to the
 * last `...`. */
const COMPARE = /^\/?repos\/[^/]+\/[^/]+\/compare\/([^?]+)\.\.\.([^?]+)(?:\?.*)?$/;
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
/** The pull request's diff, where this call asks for one the run's remote can answer. It is read
 * before the route check for the same reason: rig.sh's worlds carry no `gh pr diff` route, since
 * a diff frozen into a fixture would read the same whatever the run pushed, and a 404 here reads
 * to the agent as a pull request carrying no change at all. */
const diffed =
  as === "gh" && args[0] === "pr" && args[1] === "diff"
    ? branchDiff(args.includes("--name-only"))
    : undefined;
if (!route && compared === undefined && diffed === undefined) {
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
// The comparison, where the call asked for one, is the answer; every other call is its route's.
let output: unknown = compared ?? route?.stdout ?? "";
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
if (route?.stderr) process.stderr.write(route.stderr);
exit(route?.exit ?? 0);
