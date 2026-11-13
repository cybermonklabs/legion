// How gh reads the call it is handed: the value a flag carries, the verb it sends, the fields and
// the body, and the two review endpoints of a pull request. The gh stand-in answers a call by
// these readings and score.ts scores that same call by them, so one spelling of one verb and one
// body is read one way by the world and by the score: a call the stand-in answered as a
// submission and the score read as a read of the route — or the other way round — is a run scored
// against a world that answered it something else.
//
// It is a module of its own rather than part of the scorer because the stand-in is the `gh` a
// worker pane's PATH resolves to: every gh call an agent makes runs it, and importing the scorer
// for these readings loaded its 4,900 lines and all of its imports on each one.

/** Whether `arg` is the reviews route of `repo`'s pull request: `repos/<owner>/<repo>/pulls/<n>/
 * reviews`, with or without the leading slash `gh api` takes either way, and of that repository
 * alone. It is the create of a review as a POST and the read of the reviews already there as a
 * GET. A route read without the repository counts a review submitted anywhere as this round's
 * verdict, and a review of another repository's pull request approves nothing of this one: the
 * deep pass of 2026-11-10 moved a recorded submission to
 * `repos/sjawhar/legion-smoke/pulls/1/reviews` and the run still scored one. The stand-in matches
 * its routes on the pull request's number alone, so it answers such a call as GitHub would answer
 * the right one, and the agent is never told: a review this rule reads off another repository's
 * pull request is no review of this round at all. Which runs submitted to a repository their
 * world has not got, where those names came from, and why every part of the rig now reads the
 * world's own: rig.sh's worker world comment. */
export function reviewsRoute(arg: string, repo: string): boolean {
  return /^\/?repos\/(.+)\/pulls\/\d+\/reviews$/.exec(arg)?.[1] === repo;
}

/** The id `arg` submits on `repo`'s pull request, or undefined when it submits none:
 * `repos/<owner>/<repo>/pulls/<n>/reviews/<id>/events`, the second call of the two-step route
 * GitHub documents beside the one-step one — a POST to `…/reviews` whose body names no event
 * creates a pending review, and this call submits it with the verdict. The gh stand-in serves
 * both, the create from the world's route and this call from the review's own state, which is
 * the stand-in's own answer and no world's (gh-standin.ts's `reviewSubmission`), so a round can
 * take that route and be told nothing. A reader of the create alone scored such a round
 * `event=""`, which prints as
 * `event=none`: the row of a round that submitted nothing, which is the one thing that round
 * did not do. None of the 40 runs recorded on 2026-11-10 took it; the reviewer's prompt
 * prescribes the one-step POST. The repository is read as the create's is, and for the same
 * reason: a submit of another repository's review submits nothing of this round's. */
export function submitsReview(arg: string, repo: string): string | undefined {
  const named = /^\/?repos\/(.+)\/pulls\/\d+\/reviews\/(\d+)\/events$/.exec(arg);
  return named?.[1] === repo ? named[2] : undefined;
}

/** The text `flag` carries in `arg`, or undefined when `arg` is not that flag: "" where the flag
 * stands alone and its value is the argument after it, and the value where it is written against
 * the flag — `--flag=value`, and `-Xvalue` for a short flag, both of which gh takes. */
function written(arg: string, flag: string): string | undefined {
  if (arg === flag) return "";
  if (arg.startsWith(`${flag}=`)) return arg.slice(flag.length + 1);
  return !flag.startsWith("--") && arg.startsWith(flag) && arg.length > flag.length
    ? arg.slice(flag.length)
    : undefined;
}

/** Whether `argv` carries any of `flags`, in any of the spellings gh takes it in (`written`). */
function carries(argv: readonly string[], flags: readonly string[]): boolean {
  return argv.some((arg) => flags.some((flag) => written(arg, flag) !== undefined));
}

/** The value `argv[index]` carries as one of `flags`, or undefined when that argument is none of
 * them: the argument after the flag where it stands alone, and the text written against it
 * otherwise. A caller walking the call once reads each flag's value with this — the gh stand-in
 * records the file of every `--input`, `--body-file` and `-F key=@path` it reads that way — so a
 * spelling gh takes is a spelling the record holds. */
export function valueAt(
  argv: readonly string[],
  index: number,
  flags: readonly string[]
): string | undefined {
  for (const flag of flags) {
    const value = written(argv[index] ?? "", flag);
    if (value !== undefined) return value === "" ? argv[index + 1] : value;
  }
  return undefined;
}

/** The value the last of `flags` carries in `argv`, or undefined when the call names none of
 * them: the argument after the flag, which is how gh takes `--method GET`, `-X GET`,
 * `-R owner/repo` and `--repo owner/repo`, or the value written against the flag itself
 * (`--method=GET`, `-XGET`, `--repo=owner/repo`), which gh takes just as well.
 *
 * The last and not the first, since that is what gh does with a flag written twice: its flag
 * parser sets the value each time it reads one, so `gh pr review -R other/x -R example/widgets`
 * reviews example/widgets, and a reader of the first took such a call for a review of a
 * repository the round never touched. */
export function flagValue(argv: readonly string[], flags: readonly string[]): string | undefined {
  let last: string | undefined;
  for (const index of argv.keys()) {
    const value = valueAt(argv, index, flags);
    if (value !== undefined) last = value;
  }
  return last;
}

/** The flags gh names a call's verb with, the flags it takes one field by, and those two with
 * the flag it reads a whole body from: the body of a call is its `--input` where it names one,
 * and its fields otherwise (`sentBody`). */
const METHOD_FLAGS = ["-X", "--method"];
export const FIELD_FLAGS = ["-f", "-F", "--field", "--raw-field"];
const BODY_FLAGS = [...FIELD_FLAGS, "--input"];

/** The verb gh sends `argv` with: the method it names when it names one — `-X POST`, `-XPOST`,
 * `--method POST` or `--method=POST`, at any position and in any case, as gh reads it, and the
 * last of them where it names more than one — and otherwise `POST` when it carries a body and
 * `GET` when it carries none, since gh sends a call with a field or an input as a POST and one
 * with neither as a GET.
 *
 * The gh stand-in reads the call it is about to answer with this same function, for its own
 * answers and for the route it chooses (gh-standin.ts), so one spelling of one verb is read one
 * way by the world and by the score: a stand-in that answered a call as a submission the score
 * then read as a read of the route would leave a submission no row counts, and the other way
 * round would count one the world never made. */
export function methodOf(argv: readonly string[]): string {
  if (carries(argv, METHOD_FLAGS)) return (flagValue(argv, METHOD_FLAGS) ?? "").toUpperCase();
  return carries(argv, BODY_FLAGS) ? "POST" : "GET";
}

/** Whether `argv` is a POST, which is the call that submits something rather than reading it
 * (`methodOf`). */
export function isPost(argv: readonly string[]): boolean {
  return methodOf(argv) === "POST";
}

/** Each field `argv` carries, as the key and the value its `key=value` names. The record keeps
 * the call as it was written, so a `-F key=@path`, whose value gh reads out of the file at
 * `path`, reads here as `@path`; that file's own text is recorded beside the argv, under its
 * path (gh-standin.ts). */
function fieldsOf(argv: readonly string[]): Map<string, string> {
  const fields = new Map<string, string>();
  for (const index of argv.keys()) {
    const pair = valueAt(argv, index, FIELD_FLAGS);
    if (pair === undefined) continue;
    const [key = "", ...value] = pair.split("=");
    fields.set(key, value.join("="));
  }
  return fields;
}

/** The JSON body `call` sent, or undefined when it sent none: the object `--input <file>` holds
 * (the stand-in records the file it read) or `--input -` (its stdin), and the fields the call
 * carries where it names no `--input` at all.
 *
 * The two are never merged, since gh does not merge them: with `--input`, `gh api --help` says
 * the field flags go to the query string, so `--input r.json -f event=APPROVE` sends the file
 * as the body and `event=APPROVE` as a query parameter. A reader that merged them counted that
 * call an APPROVE where GitHub left the review pending — a false pass — and the gh stand-in,
 * reading the same create through this function, recorded no pending review for it and refused
 * the submit that followed.
 *
 * A body that is not JSON, or JSON that is no object, holds nothing this can read: the call did
 * send one, and the row says what the submission holds rather than making a rig error of a
 * round whose verdict is known. An `--input` whose text the record does not hold is read the
 * same way.
 *
 * The gh stand-in reads a create's body through this same function (gh-standin.ts's
 * `pendingReview`), since a create the world read as pending and the score read as a verdict —
 * or the other way round — is a run scored against a world that answered it something else. */
export function sentBody(call: {
  argv: readonly string[];
  stdin?: string | undefined;
  files?: Record<string, string> | undefined;
}): Record<string, unknown> | undefined {
  const input = flagValue(call.argv, ["--input"]);
  const text = input === undefined ? undefined : input === "-" ? call.stdin : call.files?.[input];
  const fields = fieldsOf(call.argv);
  if (input === undefined && fields.size === 0) return undefined;
  let parsed: unknown;
  try {
    parsed = text === undefined ? {} : JSON.parse(text);
  } catch {
    parsed = {};
  }
  const body: Record<string, unknown> =
    typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)
      ? { ...(parsed as Record<string, unknown>) }
      : {};
  if (input === undefined) for (const [key, value] of fields) body[key] = value;
  return body;
}

/** One route of a world's `fixtures.json` (rig.sh's fixture_write), as far as choosing it and
 * reading its answer go: the regex it matches the whole call against, the verbs it answers when
 * it answers some and not others, and its answer. */
export interface Route {
  readonly match: string;
  readonly methods?: readonly string[] | undefined;
  readonly stdout?: unknown;
}

/** The route of `routes` that answers `<program> <argv...>`: the first whose verbs, when it names
 * any, include the call's (`methodOf`) and whose regex matches the whole call. The gh stand-in
 * answers every call by this rule, and the daemon stand-in reads what the world reports about
 * the pull request by it too (daemon-standin.ts's `roundOutcome`), so the world the agent was
 * answered from and the world the record moves on are one world. */
export function routeFor<R extends Route>(
  routes: readonly R[],
  argv: readonly string[],
  program = "gh"
): R | undefined {
  const verb = methodOf(argv);
  const joined = `${program} ${argv.join(" ")}`;
  return routes.find(
    (candidate) =>
      (candidate.methods === undefined || candidate.methods.includes(verb)) &&
      new RegExp(candidate.match, "s").test(joined)
  );
}

/** One check run as GitHub's check-runs route reports it: its name, `queued`, `in_progress` or
 * `completed`, and the conclusion a completed one came to (`null` until then). */
export interface CheckRun {
  readonly name: string;
  readonly status: string;
  readonly conclusion: string | null;
  readonly html_url?: string;
  readonly started_at?: string;
  readonly completed_at?: string;
}

/** The check runs the world reports on `head` of `repo`: its answer to
 * `gh api repos/<repo>/commits/<head>/check-runs` (`routeFor`), or none when that answer holds no
 * `check_runs` list. A world says what a head's checks are with a route of that head's own
 * ahead of the one every other head takes, so the head the pull request is at decides what every
 * reader of its checks is told. The daemon stand-in moves on no check run: main's engine moves
 * on the merge state (`mergeStateOn`). */
export function checkRunsOn(routes: readonly Route[], repo: string, head: string): CheckRun[] {
  const answer = routeFor(routes, ["api", `repos/${repo}/commits/${head}/check-runs`])?.stdout;
  const runs =
    typeof answer === "object" && answer !== null && "check_runs" in answer
      ? answer.check_runs
      : undefined;
  if (!Array.isArray(runs)) return [];
  return runs.flatMap((run: unknown) =>
    typeof run === "object" &&
    run !== null &&
    "name" in run &&
    typeof run.name === "string" &&
    "status" in run &&
    typeof run.status === "string"
      ? [
          {
            ...(run as Record<string, unknown>),
            name: run.name,
            status: run.status,
            conclusion:
              "conclusion" in run && typeof run.conclusion === "string" ? run.conclusion : null,
          } as CheckRun,
        ]
      : []
  );
}

/** The merge state GitHub reports for the pull request while it is at `head`, in GitHub's own
 * names (`CLEAN`, `UNSTABLE`, `HAS_HOOKS`, `BLOCKED`, `BEHIND`, `DIRTY`, `UNKNOWN`): the state the
 * world's `mergeStates` names for that head — keyed by the head's SHA or a prefix of seven
 * digits or more — or `CLEAN` for a head the world names none for. The world writes it in its
 * fixtures.json beside its routes (rig.sh's fixture_write), since GitHub reports it of the pull
 * request as it stands and no route of a commit answers it. The gh stand-in answers the pull
 * request's `mergeStateStatus` and `mergeable_state` with it, and the daemon stand-in moves the
 * record on it, as main's engine moves on `record.MergeState` (daemon-standin.ts's
 * `roundOutcome`). */
export function mergeStateOn(mergeStates: unknown, head: string): string {
  if (typeof mergeStates === "object" && mergeStates !== null)
    for (const [named, state] of Object.entries(mergeStates))
      if (named.length >= 7 && head.startsWith(named) && typeof state === "string") return state;
  return "CLEAN";
}
