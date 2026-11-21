// How gh reads the call it is handed: the value a flag carries, the verb it sends, the fields and
// the body, and the two review endpoints of a pull request. The gh stand-in answers a call by
// these readings and score.ts scores that same call by them, so one spelling of one verb and one
// body is read one way by the world and by the score: a call the stand-in answered as a
// submission and the score read as a read of the route — or the other way round — is a run scored
// against a world that answered it something else. The world the run keeps (`World`) is read
// here for the same reason: every program of a run reads it through this one schema.
//
// It is a module of its own rather than part of the scorer because the stand-in is the `gh` a
// worker pane's PATH resolves to: every gh call an agent makes runs it, and importing the scorer
// for these readings loaded its 4,900 lines and all of its imports on each one.
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** rig.sh's <run>/world.json, as its fixture_write writes it into every world it builds: the
 * run's own issue `key`, the repository the world is on, the number of its pull request, the
 * issue branch of the run's own bare remote, the branch the pull request is open against, and
 * `head`, the pull request's head when the run began, the implementer's code commit. This is the
 * one schema of that file: the gh stand-in, the daemon stand-in, the pane composer
 * (worker-pane.ts) and the scorer each read it here, so no part of the run reads a world another
 * part would refuse. No key of those six has a default: fixture_write writes all six, so a world
 * missing one is a world the rig wrote wrongly, which every reader refuses rather than guess.
 * `logins` is the bot of each role App the world's roles call GitHub as (rest-objects.ts's
 * `roleApp`: `implement`, `review`), which every answer naming the account a call's token acts as
 * reads (gh-threads.ts's `loginOf`); a world.json written before it named them reads every role as
 * the world's GraphQL viewer, the review App's bot, as every world then answered. */
export const World = z.looseObject({
  key: z.string(),
  repo: z.string(),
  pr: z.number(),
  branch: z.string(),
  base: z.string(),
  head: z.string(),
  logins: z.object({ implement: z.string(), review: z.string() }).optional(),
});
export type World = z.infer<typeof World>;

/** `runDir`'s world.json (`World`), or why it is none: a file that is missing, is no JSON, or
 * lacks a key. It throws nothing, so each reader says what a file that is no world means for
 * it: a program of the run exits (`worldOf`), and the scorer makes it the run's rig error. */
export function readWorld(runDir: string): { world: World } | { why: string } {
  let text: unknown;
  try {
    text = JSON.parse(readFileSync(path.join(runDir, "world.json"), "utf8"));
  } catch (error) {
    return { why: error instanceof Error ? error.message : String(error) };
  }
  const read = World.safeParse(text);
  return read.success ? { world: read.data } : { why: read.error.message };
}

/** The world `runDir` holds, for a program of the run that cannot work without it. A file that
 * is no world ends the program with 2, naming the file, why, and `program`, the caller's own
 * name, since a reader of the run's output has to know which program stopped. */
export function worldOf(runDir: string, program: string): World {
  const read = readWorld(runDir);
  if ("world" in read) return read.world;
  console.error(`${program}: ${runDir}/world.json is no world: ${read.why}`);
  process.exit(2);
}

/** Whether `arg` is the reviews route of `repo`'s pull request: `repos/<owner>/<repo>/pulls/<n>/
 * reviews`, with or without the leading slash `gh api` takes either way and the query string it
 * lets a caller append, and of that repository alone. It is the create of a review as a POST and
 * the read of the reviews already there as a GET. A route read without the repository counts a
 * review submitted anywhere as this round's verdict, and a review of another repository's pull
 * request approves nothing of this one: the deep pass of 2026-11-10 moved a recorded submission
 * to `repos/sjawhar/legion-smoke/pulls/1/reviews` and the run still scored one. The stand-in
 * matches its routes on the pull request's number alone, so it answers such a call as GitHub
 * would answer the right one, and the agent is never told: a review this rule reads off another
 * repository's pull request is no review of this round at all. Which runs submitted to a
 * repository their world has not got, where those names came from, and why every part of the rig
 * now reads the world's own: rig.sh's worker world comment.
 *
 * A query string on a POST is the create all the same: GitHub's REST reference reads a POST's
 * parameters from its JSON body ("For POST, PATCH, PUT, and DELETE requests, parameters not
 * included in the URL should be encoded as JSON", *Getting started with the REST API*) and names
 * no query parameter of this endpoint, and gh sends the call's fields as that body whatever the
 * path carries. Not measured, since a create is a write: GitHub's documented behaviour. Read
 * with the path alone, a create of `…/reviews?x=1` was answered the route's frozen `{id:6001}` and
 * exit 0 and kept no review (INFRA-764's class search of writes, 2026-11-18). */
export function reviewsRoute(arg: string, repo: string): boolean {
  return /^\/?repos\/(.+)\/pulls\/\d+\/reviews(?:\?.*)?$/.exec(arg)?.[1] === repo;
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
 * reason: a submit of another repository's review submits nothing of this round's. A query string
 * on the path is the submit all the same, as it is on the create (`reviewsRoute`): GitHub reads
 * the event from the POST's JSON body and names no query parameter of "Submit a review for a pull
 * request". Read with the path alone, a submit of `…/reviews/<id>/events?x=1` was refused 404 as a
 * write no answer of the world models (INFRA-764's class search of writes, 2026-11-18). */
export function submitsReview(arg: string, repo: string): string | undefined {
  const named = /^\/?repos\/(.+)\/pulls\/\d+\/reviews\/(\d+)\/events(?:\?.*)?$/.exec(arg);
  return named?.[1] === repo ? named[2] : undefined;
}

/** The value `flag` carries in `arg` as gh's flag parser (spf13/pflag's `parseLongArg` and
 * `parseSingleShortArg`) reads it: `null` where the flag stands alone and takes the argument after
 * it, the text after `=` for a long flag (`--body=` is the empty value), and for a short flag the
 * text after `=` where any follows it (`-b=x` is `x`) and its whole rest otherwise (`-bx` is `x`,
 * and `-b=` is `=`); undefined where `arg` is not that flag. `--body=` and `-b=` were read as the
 * flag standing alone, its value the argument after it, so `gh pr edit 1001 --body=` named no body
 * and was refused 404 as a write the world does not model (found on 2026-11-18 in review of
 * INFRA-764's empty body edit). */
function written(arg: string, flag: string): string | null | undefined {
  if (arg === flag) return null;
  if (flag.startsWith("--"))
    return arg.startsWith(`${flag}=`) ? arg.slice(flag.length + 1) : undefined;
  if (!arg.startsWith(flag)) return undefined;
  const rest = arg.slice(flag.length);
  return rest.length > 1 && rest.startsWith("=") ? rest.slice(1) : rest;
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
    if (value !== undefined) return value === null ? argv[index + 1] : value;
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
 * and its fields otherwise (`sentBody`). Of the field flags, `-F` and `--field` are the typed
 * ones, whose `@path` value gh reads out of the file at `path` (`@-`, its stdin); `-f` and
 * `--raw-field` send `@path` as it is written. */
const METHOD_FLAGS = ["-X", "--method"];
export const FIELD_FLAGS = ["-f", "-F", "--field", "--raw-field"];
export const FILE_FIELD_FLAGS = ["-F", "--field"];
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

/** Every method `argv` names, as written and in order, in each spelling `methodOf` reads: the
 * Legion gh wrapper admits a `gh api` read only where each is exactly `GET` (gh_reads.go's
 * ghAPIReadValue, gh-selectors.ts's `readGrammar`), which the verb gh sends, the last of them in
 * any case, does not say. */
export function methodsOf(argv: readonly string[]): string[] {
  return [...argv.keys()].flatMap((index) => valueAt(argv, index, METHOD_FLAGS) ?? []);
}

/** Whether `argv` is a POST, which is the call that submits something rather than reading it
 * (`methodOf`). */
export function isPost(argv: readonly string[]): boolean {
  return methodOf(argv) === "POST";
}

/** The flags of `gh api` that take the argument after them as their value, when written alone. */
const API_VALUE_FLAGS = [
  "-X",
  "--method",
  "-f",
  "-F",
  "--field",
  "--raw-field",
  "-H",
  "--header",
  "-q",
  "--jq",
  "-t",
  "--template",
  "--input",
  "--hostname",
  "-p",
  "--preview",
  "--cache",
];

/** The endpoint `gh api <argv...>` names: its first argument that is no flag and no flag's value,
 * or undefined for a call that names none. */
export function endpointOf(argv: readonly string[]): string | undefined {
  for (let index = 1; index < argv.length; index++) {
    const arg = argv[index] ?? "";
    if (arg.startsWith("-")) {
      if (API_VALUE_FLAGS.includes(arg)) index++;
      continue;
    }
    return arg;
  }
  return undefined;
}

/** The path `gh api <argv...>` sends to: its endpoint (`endpointOf`) without the leading slash
 * gh takes either way and without its query string, "" for a call that names none. */
export function endpointPath(argv: readonly string[]): string {
  return (endpointOf(argv) ?? "").replace(/^\//, "").split("?")[0] ?? "";
}

/** The flags of `gh run view` and `gh run watch` that take the argument after them as their
 * value, as gh 2.98.0's `--help` of each names them; every other flag of the two takes none. */
const RUN_VALUE_FLAGS = [
  ...["-R", "--repo", "-a", "--attempt", "-j", "--job", "-q", "--jq", "--json"],
  ...["-t", "--template", "-i", "--interval"],
];

/** The run a `gh run view` or `gh run watch` names: its first argument after the command that is
 * no flag and no flag's value, or undefined for a call that names none, which gh refuses where no
 * terminal can pick one (gh-standin.ts's `runRefusal`, health.ts's NO_RUN). */
export function runNamed(argv: readonly string[]): string | undefined {
  for (let index = 2; index < argv.length; index++) {
    const arg = argv[index] ?? "";
    if (arg.startsWith("-")) {
      if (RUN_VALUE_FLAGS.includes(arg)) index++;
      continue;
    }
    return arg;
  }
  return undefined;
}

/** A JSON object as a body's fields build it, with no prototype: a key the call names is the
 * caller's text, and `__proto__[x]=y` must name a field and never the prototype of every object
 * of this process. */
type Fields = Record<string, unknown>;
const fieldsObject = (): Fields => Object.create(null) as Fields;
const isFields = (value: unknown): value is Fields =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The keys gh nests a field's name into (`gh api --help`: "key[subkey]=value" for a nested
 * object, "key[]=value" for an array): `comments[][path]` is `comments`, then `""` for an
 * array, then `path`. A name with no bracket, or one that opens with one, is its own key. */
function keyPath(name: string): string[] {
  const open = name.indexOf("[");
  if (open <= 0) return [name];
  const inner = [...name.slice(open).matchAll(/\[([^\]]*)\]/g)].map((found) => found[1] ?? "");
  return [name.slice(0, open), ...inner];
}

/** The object under `key` in `into`, made there when it holds none. */
function objectUnder(into: Fields, key: string): Fields {
  const found = into[key];
  if (isFields(found)) return found;
  const made = fieldsObject();
  into[key] = made;
  return made;
}

/** The object of the array under `key` in `into` that the field `next` goes into: the array's
 * last object while that one has no `next` yet, and a new object at the array's end otherwise.
 * This is how gh 2.98.0 builds `-f 'comments[][path]=a' -f 'comments[][body]=x' -f
 * 'comments[][path]=b'` into two comments, `{path: a, body: x}` and `{path: b}`. */
function objectInList(into: Fields, key: string, next: string): Fields {
  const list = Array.isArray(into[key]) ? (into[key] as unknown[]) : [];
  const last = list.at(-1);
  into[key] = list;
  if (isFields(last) && !Object.hasOwn(last, next)) return last;
  const made = fieldsObject();
  list.push(made);
  return made;
}

/** Puts `value` at `keys` (`keyPath`) in `body`, as gh puts a field: under each key a nested
 * object, under each `[]` an array, and a `key[]` with no `=` an empty array. A name with no
 * `=` and no `[]` keeps the empty value it has always been read with here. */
function place(body: Fields, keys: readonly string[], value: string | undefined): void {
  let into = body;
  let listed = false;
  let key: string | undefined;
  for (const next of keys) {
    if (next === "") {
      listed = true;
      continue;
    }
    if (key !== undefined) {
      into = listed ? objectInList(into, key, next) : objectUnder(into, key);
      listed = false;
    }
    key = next;
  }
  if (key === undefined) return;
  if (!listed) {
    into[key] = value ?? "";
    return;
  }
  const list = Array.isArray(into[key]) ? (into[key] as unknown[]) : [];
  into[key] = value === undefined ? list : [...list, value];
}

/** The body the fields of `call` build, as gh sends it, or undefined when the call carries no
 * field. Each `key=value` is placed where its key names (`keyPath`, `place`), so a review whose
 * inline comments are written `-f 'comments[][path]=docs/cli.md' -f 'comments[][line]=5'` sends
 * an array of comment objects, which is what GitHub creates the inline comments from; read as
 * flat keys, that review carried no comment at all. A `-F key=@path` is the text of the file at
 * `path` where the record holds it (gh-standin.ts records each file it reads, `@-` under `-`
 * beside the stdin), and `@path` as written where it does not. */
function fieldsOf(call: {
  argv: readonly string[];
  stdin?: string | undefined;
  files?: Record<string, string> | undefined;
}): Fields | undefined {
  let body: Fields | undefined;
  for (const index of call.argv.keys()) {
    const pair = valueAt(call.argv, index, FIELD_FLAGS);
    if (pair === undefined) continue;
    body ??= fieldsObject();
    const equals = pair.indexOf("=");
    const name = equals < 0 ? pair : pair.slice(0, equals);
    let value = equals < 0 ? undefined : pair.slice(equals + 1);
    if (value?.startsWith("@") && valueAt(call.argv, index, FILE_FIELD_FLAGS) !== undefined) {
      const source = value.slice(1);
      value = call.files?.[source] ?? (source === "-" ? call.stdin : undefined) ?? value;
    }
    place(body, keyPath(name), value);
  }
  return body;
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
 * `reviewCreate`), since a create the world read as pending and the score read as a verdict —
 * or the other way round — is a run scored against a world that answered it something else. */
export function sentBody(call: {
  argv: readonly string[];
  stdin?: string | undefined;
  files?: Record<string, string> | undefined;
}): Record<string, unknown> | undefined {
  const input = flagValue(call.argv, ["--input"]);
  if (input === undefined) return fieldsOf(call);
  const text = input === "-" ? call.stdin : call.files?.[input];
  let parsed: unknown;
  try {
    parsed = text === undefined ? {} : JSON.parse(text);
  } catch {
    parsed = {};
  }
  return isFields(parsed) ? { ...parsed } : {};
}

/** The state GitHub leaves a review in for each event it takes. */
const SUBMITTED_STATE: Readonly<Record<string, string>> = {
  APPROVE: "APPROVED",
  REQUEST_CHANGES: "CHANGES_REQUESTED",
  COMMENT: "COMMENTED",
};

/** The state GitHub leaves a review in for `event`, on a create and on a submit
 * (`…/reviews/<id>/events`), and undefined for every value GitHub does not take as an event:
 * `approve`, `PENDING` and a value that is no string among them. An own property alone, so an
 * `event` named `toString` is no event either. */
export function submittedState(event: unknown): string | undefined {
  return typeof event === "string" && Object.hasOwn(SUBMITTED_STATE, event)
    ? SUBMITTED_STATE[event]
    : undefined;
}

/** The state the create of a review whose body is `body` leaves that review in, or undefined
 * when GitHub refuses the create 422. GitHub's "Create a review for a pull request" takes the
 * events `APPROVE`, `REQUEST_CHANGES` and `COMMENT`, and "by leaving this blank, you set the
 * review action state to `PENDING`": so a body naming no `event`, one whose `event` is `null`
 * or `""`, and a create that sends no body at all leave the review `PENDING`; any other event is
 * refused; and the same page makes `body` required for `REQUEST_CHANGES` and `COMMENT`, so
 * either with no body, or an empty one, is refused too.
 *
 * The gh stand-in answers a create by this (gh-standin.ts's `reviewCreate`) and score.ts reads
 * the recorded create by it (`reviewOf`): they read `event: ""` two ways while each had a rule
 * of its own, pending in the score and refused in the world. */
export function createdState(body: Record<string, unknown>): string | undefined {
  const event = body.event ?? "";
  if (event === "") return "PENDING";
  const state = submittedState(event);
  if (state === undefined || event === "APPROVE") return state;
  return typeof body.body === "string" && body.body !== "" ? state : undefined;
}

/** The head the create of a review whose body is `body` creates it at: the `commit_id` it names,
 * and `tip`, the pull request's head when the create was made, where it names none (GitHub's
 * page: "Defaults to the most recent commit in the pull request when you do not specify a
 * value"). An empty `commit_id` names none. The stand-in passes the tip of the run's remote at
 * the call, and the score the head the run's pushes had left the branch at when the call was
 * recorded: with no tip of its own the score read such a create at `""`, so a two-step approval
 * whose create named no head failed `namesHead` where the world had answered it at the head. */
export function createdHead(
  body: Record<string, unknown>,
  tip: string | undefined
): string | undefined {
  return typeof body.commit_id === "string" && body.commit_id !== "" ? body.commit_id : tip;
}

/** One route of a world's `fixtures.json` (rig.sh's fixture_write), as far as choosing it and
 * reading its answer go: the regex it matches the whole call against, the verbs it answers when
 * it answers some and not others, the path it answers when it answers one path and no other,
 * and its answer. */
export interface Route {
  readonly match: string;
  readonly methods?: readonly string[] | undefined;
  readonly endpoint?: string | undefined;
  readonly stdout?: unknown;
}

/** The route of `routes` that answers `<program> <argv...>`: the first whose verbs, when it names
 * any, include the call's (`methodOf`), whose path, when it names one, is the whole of the path
 * a `gh api` call sends to (`endpointPath`), and whose regex matches the whole call. The gh
 * stand-in answers every call by this rule, and the daemon stand-in reads what the world reports
 * about the pull request by it too (daemon-standin.ts's `roundOutcome`), so the world the agent
 * was answered from and the world the record moves on are one world.
 *
 * A route's regex reads the call's whole text, so on its own it answers a call whose field merely
 * holds the path it names: `gh api graphql` matched every call whose text said "graphql", and a
 * write route matched a write to another repository's pull request of the same number. A route
 * that writes names the one path it models (`endpoint`, read whole), so it answers that write
 * and no other (rig.sh's fixture_write). */
export function routeFor<R extends Route>(
  routes: readonly R[],
  argv: readonly string[],
  program = "gh"
): R | undefined {
  const verb = methodOf(argv);
  const joined = `${program} ${argv.join(" ")}`;
  const path = program === "gh" && argv[0] === "api" ? endpointPath(argv) : undefined;
  return routes.find(
    (candidate) =>
      (candidate.methods === undefined || candidate.methods.includes(verb)) &&
      (candidate.endpoint === undefined ||
        (path !== undefined && new RegExp(`^(?:${candidate.endpoint})$`).test(path))) &&
      new RegExp(candidate.match, "s").test(joined)
  );
}

/** One check run as GitHub's check-runs route reports it: its name, `queued`, `in_progress` or
 * `completed`, the conclusion a completed one came to (`null` until then), the App that ran it,
 * by its id (`app`, rig.sh's concluded_checks; GitHub Actions where the world names none,
 * rest-objects.ts's `checkRunApp`), and the title and summary that App wrote of it (`output`),
 * where it wrote any. */
export interface CheckRun {
  readonly name: string;
  readonly status: string;
  readonly conclusion: string | null;
  readonly app?: unknown;
  readonly output?: unknown;
  readonly html_url?: string;
  readonly started_at?: string;
  readonly completed_at?: string;
}

/** The check runs the world reports on `head` of `repo`: its answer to
 * `gh api repos/<repo>/commits/<head>/check-runs` (`routeFor`), or none when that answer holds no
 * `check_runs` list. A world says what a head's checks are with a route of that head's own
 * ahead of the one every other head takes, so the head the pull request is at decides what every
 * reader of its checks is told. The merge state reads them too, a check the base requires holding
 * the head `BLOCKED` until it passes (gh-threads.ts's `mergeStateOf`), and the daemon stand-in
 * moves on that merge state alone, as main's engine does. */
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

/** The merge state GitHub reports for the pull request while it is at `head` for a reason no rule
 * of its base decides, in GitHub's own names (`CLEAN`, `UNSTABLE`, `HAS_HOOKS`, `BEHIND`, `DIRTY`,
 * `UNKNOWN`): the state the world's `mergeStates` names for that head — keyed by the head's SHA
 * or a prefix of seven digits or more — or `CLEAN` for a head the world names none for. The world
 * writes it in its fixtures.json beside its routes (rig.sh's fixture_write), since GitHub reports
 * it of the pull request as it stands and no route of a commit answers it. A rule of the base
 * that is unmet holds a state GitHub would merge in `BLOCKED` (gh-threads.ts's `mergeStateOf`),
 * which the gh stand-in answers the pull request's `mergeStateStatus` and `mergeable_state` with
 * and the daemon stand-in moves the record on, as main's engine moves on `record.MergeState`
 * (daemon-standin.ts's `roundOutcome`). */
export function mergeStateOn(mergeStates: unknown, head: string): string {
  if (typeof mergeStates === "object" && mergeStates !== null)
    for (const [named, state] of Object.entries(mergeStates))
      if (named.length >= 7 && head.startsWith(named) && typeof state === "string") return state;
  return "CLEAN";
}
