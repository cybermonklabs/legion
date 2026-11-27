#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $GH_DATA_DIR/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, the agent that made the call when its pane names one ($LEGION_ROLE,
// which production gives every pane and a run that boots one agent per role gives each), the
// pull request body it kept, if any, the review it left, if any: its state and head, as
// answered, and what answered it, `handler`: a route of the world, one of this file's own
// answers, the Legion gh wrapper's refusal, or `unrouted`, the 404 of a call nothing models,
// which `rig.sh health` names for a write), then answered from the
// first route in $GH_DATA_DIR/fixtures.json that answers "gh <argv...>" (gh-call.ts's
// `routeFor`: its verbs, the one path it writes where it is a write's, and its regex). Three
// answers are the run's own remote rather than a route's, since each is a fact of the run and no
// frozen answer could tell one run from another: the pull request's head, which a route carries
// and this answer replaces, and its diff and the comparison of two commits, which need no route
// at all (`branchDiff`, `comparison`). The checks of that head (`gh pr checks` and the pull
// request's `statusCheckRollup`) are the world's check-runs answer for the head the remote is at,
// reported on that head as GitHub reports a check run (`headRuns`, `reportedRuns`), and its review
// decision, merge state and mergeability (`reviewDecision`, `mergeStateStatus`, `mergeable`,
// `mergeable_state`) are computed as GitHub computes them from the base's rules, the reviews the
// world held and the run left, those checks and the threads as the run left them (gh-threads.ts's
// `reviewDecisionOf`, `mergeStateOf`, `mergeableOf`). The rest of a review's life is this
// file's own and no world's, since GitHub answers it from what the run has done: the create of a
// review, refused 422 where GitHub refuses it and otherwise answered with the state, head and
// body the call gives it (`reviewCreate`); a `gh pr review`, refused where gh refuses its flags
// and otherwise a review at the pull request's head with the event and body it names
// (`reviewPullRequest`); the submit of a review (`…/reviews/<id>/events`), answered
// from the review's own state (`reviewSubmission`); and the read of one review this run created
// (`reviewRead`). A route that names an `answer` is answered from what the run did — the reviews
// it created and their inline comments, the comments it posted, a ref or a commit's combined
// status read on its remote, a commit's check runs reported on that commit, a file or a
// directory at a ref of its remote, a branch with the commit it points at there, and the base's
// ruleset by its id (`fromRun`) — and a route that holds a `graph` answers GraphQL
// from it, paged as GitHub pages a connection (`graphAnswer`, with gh-routes.ts's parser and
// executor). The pull request's review threads are GitHub's state and so the run's: the world's
// threads, then the effect of every call the record holds on them — the threads a review's
// inline comments or a comment of its own open, each reply, by GraphQL or REST, and each
// resolution (gh-threads.ts's `threadState`) — and every reader of a thread or of a review
// comment, GraphQL or REST, reads that one state, while a write on a thread is answered from it
// (`threadCall`). A
// REST read's list is paged by `per_page` and `page`, and `--paginate` asks for every page, as gh
// does (`restPages`). A call no route matches and no answer of this file's own serves prints
// GitHub's 404 — `gh: Not Found (HTTP 404)` on stderr and, for a `gh api` call, GitHub's JSON
// error body on stdout, as gh prints both — and exits 1, and so does a GraphQL mutation the
// world's graph holds no answer for and a `gh pr edit` or `gh pr comment` the world does not
// model: a write the world does not model is refused, never answered as a success that changed
// nothing. A route that refuses a `gh api` call — the 403 of a role App's read of a branch's
// protection — prints GitHub's error body and gh's line for it, as gh prints an HTTP error, with
// no `--jq` run over the body (`refuseWith`). Nothing is sent anywhere.
// `--json a,b` picks those fields of the answer, or of each object of a list answer (`gh pr
// list`), and `--jq`/`-q` runs jq over each page, as gh does; `--slurp` prints the pages as one
// list. A pull request body edit (`gh pr edit --body`, `--body-file`, or a PATCH
// through `gh api`) is kept: every answer of the pull request then carries the new one, the
// edit's own answer among them, as GitHub's would, and the call's record carries it as `body`.
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
import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";
// How a call's verb, its flags and its body are read, and the two review endpoints: one reader
// for this stand-in and for the score, imported rather than written again here. A call this
// stand-in answered as a submission and the score read as a read of the route — or the other
// way round — is a run scored against a world that answered it something else.
import {
  type CheckRun,
  checkRunsOn,
  createdHead,
  createdState,
  endpointOf,
  endpointPath,
  FIELD_FLAGS,
  FILE_FIELD_FLAGS,
  flagValue,
  isPost,
  isPrReview,
  methodOf,
  REVIEW_FLAGS_REFUSED,
  reviewCreateRoute,
  reviewFlagEvent,
  reviewsRoute,
  routeFor,
  runNamed,
  sentBody,
  submitsReview,
  submittedState,
  valueAt,
  worldOf,
} from "./gh-call";
// The Legion gh wrapper's reading of the owners a call names, by its own selector table, and of
// the calls it runs on the pane's own token: one reading for this stand-in's refusal and health's
// judgement of it.
import { capturedRead, runsOnPaneToken } from "./gh-pane-token";
// The route a call reaches (routeOf, worldRouteOf) and the executor that answers a GraphQL query
// from the world's graph, both on gh-routes.ts's one reading of a query (`parseDocument`).
import {
  executeGraphql,
  type GraphAnswer,
  type GraphError,
  isObject,
  type Operation,
  routeOf,
  worldRouteOf,
} from "./gh-routes";
import {
  type GhSelectors,
  ghTargetOf,
  ownerRefusal,
  RUN_SELECTORS,
  runSelectors,
} from "./gh-selectors";
// The pull request's review threads as the run has left them, and the review decision and merge
// state its base's rules leave it in: one replay of the record for this stand-in and the daemon
// stand-in.
import {
  commentsOf,
  conversationOf,
  createdReviewId,
  createsReview,
  graphqlOf,
  heldPullRequest,
  type Json,
  loginOf,
  mergeableOf,
  mergeStateOf,
  NOT_FOUND,
  OWN_REVIEW,
  REVIEW_NODE,
  recordedCalls,
  restLogin,
  reviewDecisionOf,
  routeAnswer,
  type ThreadWorld,
  threadCall,
  threadState,
} from "./gh-threads";
// What gh prints for a pull request — the fields `--json` takes, the objects it reads a review and
// a comment as, its text views — from the pull request as this stand-in reads the world's.
import {
  authorOf,
  commentList,
  commentNode,
  FIELD_REFUSED,
  graphqlLogin,
  latestReviews,
  listLine,
  type PrField,
  PULL_NOT_FOUND,
  preview,
  reviewNode,
  unknownField,
} from "./gh-view";
// GitHub's REST objects more than one route carries — an account, an App, the repository, the
// pull request — built one way for this stand-in and the HTTP one Dispatch reads.
import {
  accountsOf,
  appJson,
  checkRunApp,
  committerJson,
  githubInstant,
  githubTime,
  type PullFacts,
  pullRequestJson,
  pusherOf,
  reactionsJson,
  repositoryJson,
  roleApp,
  userJson,
} from "./rest-objects";
// The calls record's name, the line it is opened with, and a read kept once (`once`): a fact of
// the world that several of the pull request's fields read, each a read of the record or remote.
import { CALLS, once, openingLine } from "./run-records";

/** rig.sh's worker_fixture writes each route as {match, methods?, endpoint?, stdout?, stderr?,
 * exit?, answer?, graph?}.
 * `methods` is the verbs that route answers, where it answers some and not others: the create of
 * a review is the POST of the path a GET of it reads the reviews from. A route that names none
 * answers whatever verb reaches it. The verb is read as the score reads it (`methodOf`) and
 * never by a regex over the call: a world that asked for `-X POST` written out sent a create
 * written `-f commit_id=<head>`, `--method=POST` or `-XPOST` to its GET route, recorded no
 * pending review for it, and refused the submit that followed where GitHub answers 200.
 * `endpoint` is the one path a write's route writes, read whole (gh-call.ts's `routeFor`), so a
 * write to another path, another repository or another pull request matches none.
 * `answer` names a reading of the run's own record or remote this stand-in answers the route
 * with (`fromRun`), `stdout` being what the world held before the run; `graph` is the world's
 * state as GitHub's GraphQL schema names it, which a GraphQL call is answered from
 * (`graphAnswer`). */
const Fixtures = z.looseObject({
  routes: z.array(
    z.object({
      match: z.string(),
      methods: z.array(z.string()).optional(),
      endpoint: z.string().optional(),
      stdout: z.unknown().optional(),
      stderr: z.string().optional(),
      exit: z.number().optional(),
      answer: z.string().optional(),
      graph: z.record(z.string(), z.unknown()).optional(),
    })
  ),
});

// The run's directory, named in a variable of gh's own shape (its GH_CONFIG_DIR) rather than one
// naming the rig, since the bundle a run's PATH resolves `gh` to sets it in its first line and an
// agent can read that file. The words this push removes from everything an agent reads
// (run-names.ts's REMOVED_NAMES, and this file's own) are in neither the variable nor a refusal.
const run = process.env.GH_DATA_DIR;
if (!run) {
  console.error("gh: GH_DATA_DIR is unset");
  process.exit(2);
}
/** This run's record of every call this stand-in answered, which it opens and reads back (the
 * pending review the run left) and the score reads after it. */
const RECORD = `${run}/${CALLS}`;
/** Opens the record with this stand-in's own line, which says it was there, and does nothing to
 * a record that is already open. */
function openRecord(): void {
  if (existsSync(RECORD)) return;
  appendFileSync(RECORD, openingLine(CALLS));
}
const invocation = Bun.argv.slice(2);
// `--open` is rig.sh asking for that line alone, before any agent of the run has made a call:
// `$R/bin/gh --open`, the stand-in reached the way the pane's PATH reaches it, where the wrapper
// inserts the program word. It is that flag on its own after that word, so no call of an
// agent's is taken for it — and `gh --open` is no command gh itself answers.
if (invocation.length === 2 && invocation[0] === "gh" && invocation[1] === "--open") {
  openRecord();
  process.exit(0);
}
/** The repository `repo` names when gh reads it as one of github.com, the host the world plays:
 * `github.com/OWNER/REPO` or `https://github.com/OWNER/REPO` (gh's `[HOST/]OWNER/REPO`, and the
 * URL it takes), as OWNER/REPO; `repo` as it stands for any other. */
function onGithub(repo: string): string {
  return /^(?:https?:\/\/)?github\.com\/([^/\s]+\/[^/\s]+?)(?:\.git)?\/?$/i.exec(repo)?.[1] ?? repo;
}
/** A gh call as the world answers it: `--hostname github.com` (in either spelling) left out, and
 * a `-R`/`--repo` of github.com's (`onGithub`) given as OWNER/REPO, wherever in the argv the flag
 * stands. github.com is gh's own default host, so GitHub answers such a call as it answers the
 * bare one, and every route of the world, and every reader of the record, reads it so: the
 * record keeps this argv. A call naming any other host is left as it stands, and no route of the
 * world answers it, as GitHub's never would. */
function asGithub(argv: readonly string[]): string[] {
  const read: string[] = [];
  for (let at = 0; at < argv.length; at++) {
    const word = argv[at] ?? "";
    if (word === "--hostname" && argv[at + 1]?.toLowerCase() === "github.com") at++;
    else if (word.toLowerCase() === "--hostname=github.com") continue;
    else if ((word === "-R" || word === "--repo") && at + 1 < argv.length)
      read.push(word, onGithub(argv[++at] ?? ""));
    else {
      const joined = /^(-R|--repo=)(.+)$/.exec(word);
      read.push(joined === null ? word : `${joined[1]}${onGithub(joined[2] ?? "")}`);
    }
  }
  return read;
}
const [as = "gh", ...given] = invocation;
const args = as === "gh" ? asGithub(given) : given;
const at = new Date().toISOString();
openRecord();
const fixtures = Fixtures.parse(JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8")));
const routes = fixtures.routes;
/** The run's world (gh-call.ts's `World`, the one schema every program of the run reads it
 * by), or undefined when the run has none. This stand-in is the one reader that answers with no
 * world rather than exiting: a scenario that builds no world writes no world.json, and its calls
 * are answered from the routes alone, with the fixture's own head. Only a missing file is "no
 * world". A world.json that is there and is no world is the rig's own fault, and reading it as
 * none would serve the fixture's frozen head and diff to every call: a pushing role would read a
 * head that is not its own and stop, and a reviewer would read the diff without the fix it was
 * sent to read, with nothing anywhere saying why. So that file ends the call with 2, as it ends
 * every other program of the run (`worldOf`). */
const world = existsSync(`${run}/world.json`) ? worldOf(run, "gh") : undefined;
/** The world's GitHub accounts (rig.sh's fixture_first_commit), which every user an answer names
 * is read from (rest-objects.ts's `userJson`). */
const accounts = accountsOf(run);

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
// The verb is the one the score reads (`methodOf`), so `--method=patch` is the PATCH `-X PATCH`
// is. The path may carry a query string, which GitHub's PATCH does not read: its REST reference
// reads a PATCH's parameters from the JSON body and names no query parameter of "Update a pull
// request" (GitHub's documented behaviour; a write is not measured). Read with the path alone, a
// PATCH of `…/pulls/<n>?x=1` sending a body was answered the old body and exit 0 and kept nothing
// (INFRA-764's class search of writes, 2026-11-18).
const prEdit = as === "gh" && args[0] === "pr" && args[1] === "edit";
/** A `gh pr comment`, whose `-F/--body-file` names the file of the comment it posts, which the
 * record keeps so the conversation it leaves can be read back (`conversation`). */
const prComment = as === "gh" && args[0] === "pr" && args[1] === "comment";
/** A `gh pr review`, whose `-F/--body-file` names the file of the review's body, which the record
 * keeps so the review it leaves is read with its body (`reviewPullRequest`, score.ts's
 * `reviewOf`). */
const prReview = as === "gh" && isPrReview(args);
const prPatch =
  as === "gh" &&
  args[0] === "api" &&
  methodOf(args) === "PATCH" &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+(?:\?.*)?$/.test(arg));
let body: string | undefined;
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
/** The flags each file and each field is read by: `gh pr edit` takes the body's text by
 * `-b/--body` and its file by `-F/--body-file`, while the `-F` of a `gh api` call is a field
 * whose `@path` value is read out of a file, and `--input`/`--body-file` name a whole body's
 * file. Each is read in every spelling gh takes it in (`valueAt`), the spellings the body
 * reader reads too: read by exact match, an `--input=<file>` written against the flag was never
 * stored here, and `sentBody` then read the create that named it as a body of nothing. */
const PR_BODY_FLAGS = ["-b", "--body"];
const PR_BODY_FILE_FLAGS = ["-F", "--body-file"];
const INPUT_FLAGS = ["--input", "--body-file"];
for (const index of args.keys()) {
  const edited = prEdit ? valueAt(args, index, PR_BODY_FLAGS) : undefined;
  const bodyFile = prEdit ? valueAt(args, index, PR_BODY_FILE_FLAGS) : undefined;
  const commentFile = prComment || prReview ? valueAt(args, index, PR_BODY_FILE_FLAGS) : undefined;
  const source = valueAt(args, index, INPUT_FLAGS);
  const field = valueAt(args, index, FIELD_FLAGS);
  // A flag with nothing after it, and one written against an empty value, names no file and no
  // field. `gh pr edit --body ""` names the empty body, which gh sends and GitHub keeps: gh's
  // `pr edit` sends the body of every `--body` the call names, empty or not (its `Editable`
  // marks a flag the call names edited, whatever its value), and GitHub's `updatePullRequest`
  // sets the body it is sent. Not measured, since an edit is a write. The empty body was read as
  // none, and the edit was answered the pull request's URL and exit 0 with the body unchanged.
  if (edited !== undefined) body = edited;
  else if (bodyFile) {
    files[bodyFile] = await readSource(bodyFile);
    body = files[bodyFile];
  } else if (commentFile) files[commentFile] = await readSource(commentFile);
  else if (source) files[source] = await readSource(source);
  else if (field) {
    const [key = "", ...rest] = field.split("=");
    let value = rest.join("=");
    if (valueAt(args, index, FILE_FIELD_FLAGS) !== undefined && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    if (prPatch && key === "body") body = value;
  }
}
const input = flagValue(args, ["--input"]);
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
/** The verb gh sends this call with, which the routes that answer one verb and not another are
 * chosen by (`Fixtures`). */
const verb = methodOf(args);
/** The GraphQL operation this call sends to GitHub's GraphQL endpoint, in any spelling of it:
 * `gh api graphql`, which gh reads as a GraphQL call of its own, and `gh api /graphql` or
 * `graphql?…`, which gh posts as any other path and GitHub runs all the same (gh-threads.ts's
 * `graphqlOf`, which reads each as gh sends it). A mutation sent the second way was answered
 * empty and exit 0 whatever it named, the graph never read, so one the world does not model
 * changed nothing as a success (INFRA-764's class search of writes, 2026-11-18). */
const graphqlCall =
  as === "gh" && args[0] === "api" && endpointPath(args) === "graphql"
    ? graphqlOf({ argv: args, files, stdin })
    : undefined;
/** Whether gh reads this call as GraphQL of its own (`gh api graphql`): it pages a query that
 * takes `$endCursor`, and it reads GitHub's `errors` as a failure, printing each message and
 * exiting 1. Posted to the endpoint by another spelling, the call is a REST call to gh, which
 * prints GitHub's answer, errors and all, and exits 0: measured on 2026-11-18
 * (implementer-reads.json), a query naming a variable it was not sent printed GitHub's errors
 * with exit 0 through `gh api /graphql` and with exit 1 and `gh: <message>` through `gh api
 * graphql`. */
const graphMode = graphqlCall !== undefined && endpointOf(args) === "graphql";
/** The route `routeFor` chooses for this call by that verb, the path it writes and the call's
 * whole text (the rule the daemon stand-in reads this world by too). */
const matched = routeFor(routes, args, as);
/** The flags `gh pr edit` and `gh pr comment` take their repository by. */
const REPO_FLAGS = ["-R", "--repo"];
/** Whether this `gh pr edit` or `gh pr comment` is one the world models: of the world's pull
 * request — named by its number, `#<number>`, its URL or its branch, or by nothing, which gh reads
 * as the pull request of the checkout's branch, the world's — on the world's repository where it
 * names one, carrying the body (`-b/--body` or `-F/--body-file`, in any spelling gh takes,
 * `valueAt`) and no other flag. GitHub answers every other edit (a title, a base, a label, a
 * reviewer) and every other comment (`--edit-last`, `--delete-last`) with a change this world
 * does not keep, so each is refused as a write the world does not model, where the route for
 * `gh pr (edit|comment|review|ready)` answered any of them, on any pull request, with a comment
 * URL of no comment. */
function prWriteModelled(): boolean {
  if (world === undefined) return false;
  let bodied = false;
  let selector: string | undefined;
  for (let index = 2; index < args.length; index++) {
    const arg = args[index] ?? "";
    const body = valueAt(args, index, [...PR_BODY_FLAGS, ...PR_BODY_FILE_FLAGS]);
    const repo = valueAt(args, index, REPO_FLAGS);
    // A body file named by the empty value (`--body-file=`) is no file gh can read, so the edit
    // is no edit the world takes (gh's `pr edit` fails to read it).
    if (valueAt(args, index, PR_BODY_FILE_FLAGS) === "") return false;
    if (body !== undefined || repo !== undefined) {
      if (repo !== undefined && repo !== world.repo) return false;
      bodied ||= body !== undefined;
      if ([...PR_BODY_FLAGS, ...PR_BODY_FILE_FLAGS, ...REPO_FLAGS].includes(arg)) index++;
      continue;
    }
    if (arg.startsWith("-") || selector !== undefined) return false;
    selector = arg;
  }
  const named = [String(world.pr), `#${world.pr}`, heldPullRequest(routes).url, world.branch];
  return bodied && (selector === undefined || named.includes(selector));
}
/** Whether this call is a write the route it matched takes the verb and the path of and the world
 * still does not model, which GitHub answers with a change the world would not keep: a mutation
 * one of whose fields the world's graph holds no answer for; a `gh pr edit` or `gh pr comment`
 * the world does not model (`prWriteModelled`); and a PATCH of the pull request that sends
 * anything but its body, such as `state=closed`, which closes it on GitHub. */
function unmodelledWrite(): boolean {
  if (matched === undefined) return false;
  if (matched.graph !== undefined && graphqlCall?.operation?.type === "mutation")
    return !graphqlCall.operation.selections.every(
      (selection) =>
        selection.kind === "field" &&
        isObject(matched.graph?.mutation) &&
        selection.name in matched.graph.mutation
    );
  if (prEdit || prComment) return !prWriteModelled();
  if (verb === "PATCH" && matched.methods?.includes("PATCH") === true) {
    const sent = sentBody({ argv: args, stdin, files }) ?? {};
    return Object.keys(sent).join() !== "body" || typeof sent.body !== "string";
  }
  return false;
}
/** A `gh pr review` as gh's flag parser reads it (gh 2.98.0's pkg/cmd/pr/review/review.go): the
 * events its flags name (gh-call.ts's `reviewFlagEvent`), each once, as a flag written twice is
 * set once; whether it names a body (`-b/--body`, the empty one too, as gh's `Flags().Changed`
 * reads it) and the last it names; the last body file it names (`-F/--body-file`, the empty value
 * naming none, as gh reads it); the last repository it names (`-R/--repo`); and the pull requests
 * it names. */
type ReviewCall = {
  events: Set<string>;
  bodied: boolean;
  body: string;
  file: string;
  repo: string;
  named: string[];
};
/** This call read as a `gh pr review` of a run with a world (ReviewCall), or undefined for every
 * other call, and for a `gh pr review` naming anything else — a flag gh takes and the world does
 * not model (`--help`, `--approve=false`, `-ab`), a flag with no value after it, or a second pull
 * request, each of which gh refuses in words not measured here — which is refused as a call the
 * world does not model (`unrouted`). */
function reviewCall(): ReviewCall | undefined {
  if (!prReview || world === undefined) return undefined;
  const read: ReviewCall = {
    events: new Set(),
    bodied: false,
    body: "",
    file: "",
    repo: "",
    named: [],
  };
  for (let index = 2; index < args.length; index++) {
    const arg = args[index] ?? "";
    const event = reviewFlagEvent(arg);
    const body = valueAt(args, index, PR_BODY_FLAGS);
    const file = valueAt(args, index, PR_BODY_FILE_FLAGS);
    const repo = valueAt(args, index, REPO_FLAGS);
    if (event !== undefined) read.events.add(event);
    else if (body !== undefined) {
      read.bodied = true;
      read.body = body;
    } else if (file !== undefined) read.file = file;
    else if (repo !== undefined) read.repo = repo;
    else if (arg.startsWith("-")) return undefined;
    else read.named.push(arg);
    if ([...PR_BODY_FLAGS, ...PR_BODY_FILE_FLAGS, ...REPO_FLAGS].includes(arg)) index++;
  }
  return read.named.length > 1 ? undefined : read;
}
/** This call as a `gh pr review` the world models (`reviewCall`), undefined for every other. */
const reviewed = reviewCall();
/** The route that answers this call: the one chosen, except for a write the world does not model
 * on it (`unmodelledWrite`). GitHub answers such a write and the world does not, so it is refused
 * as every call the world has no answer for is, and `rig.sh health` names it. A `gh pr review`
 * the world models is answered by the world's create of a review (gh-call.ts's
 * `reviewCreateRoute`), whose answer is the review GitHub makes on the pull request
 * (`reviewPullRequest`): no route's regex reads the call, which gh sends as GraphQL of its own. */
const route =
  reviewed !== undefined && world !== undefined
    ? reviewCreateRoute(routes, world.repo, world.pr)
    : unmodelledWrite()
      ? undefined
      : matched;
/** The review this call left on the pull request, as this stand-in answered it: its state, the
 * head it stands at and the body it carries. Set for a create the world answered with a review,
 * for a `gh pr review` it took (`reviewPullRequest`) and for a submit of the pending one;
 * undefined for every other call. The daemon stand-in reads
 * the run's reviews from these and from nothing else (daemon-standin.ts's `roundOutcome`), so the
 * approval the record moves on is the one the world answered the agent with, read once, here,
 * and its body is the one whose Legion footer the daemon reads (intake's `LegionSession`). */
let answered: { state: string; head: string; body: string } | undefined;
/** What answered this call, which the record keeps (`handler`): `route`, the route the record
 * names, with its own answer; the name of a reading of the run that route names (its `answer`,
 * `fromRun`) or of this file's own answer (`comparison`, `pr view`, `pr list`, `pr diff`, `pr
 * checks`, `pr review`, `review submit`, `review create`, `review read`, `thread`, `graph`,
 * `pinned read`); gh's own refusal of a read of the pull request (gh-view.ts's PULL_NOT_FOUND and
 * FIELD_REFUSED), which `rig.sh health` declares for every role, and of a `gh pr review` whose
 * flags make no one review (gh-call.ts's REVIEW_FLAGS_REFUSED), which it declares for the
 * reviewer; `wrapper`, the Legion gh wrapper's refusal; or
 * `unrouted`, the 404 of a call nothing of the world models, which `rig.sh health` names when
 * that call is a write (its unmodelled-write rule). Each answer sets it where it is decided,
 * before anything there can refuse the call. */
let handler = "unrouted";
/** Records the call with the status it exits with, then exits. `by` is the role the call's pane
 * names (LEGION_ROLE, as production names a pane's role), so a run that boots one agent per role
 * has each call say which role made it; a pane that names none records no `by`. A call that left
 * a review records it (`answered`) whatever its status: a submit answered 200 whose `--jq` then
 * failed exits 1, and GitHub holds the review all the same. */
function exit(status: number): never {
  const by = process.env.LEGION_ROLE || undefined;
  appendFileSync(
    RECORD,
    `${JSON.stringify({ at, as, by, argv: args, files, stdin, route: route?.match ?? null, body: kept, review: answered, handler, exit: status })}\n`
  );
  process.exit(status);
}
/** Whether the label's wrapper ran this call, which names only owners outside the world's
 * project, on the pane's own token (`runsOnPaneToken`), so it reached GitHub. */
let onPaneToken = false;
/** The Legion gh wrapper's refusal of a call naming an owner outside the world's project, which
 * production's gh never reaches GitHub for: read by the wrapper's own selector table of the run's
 * label (gh-selectors.ts's `ghTargetOf`, over the selectors rig.sh's worker_boot wrote into the
 * run) and refused with the line that label's wrapper prints for that form of naming it
 * (`ownerRefusal`, in the words the selectors read off the label's source, `unservedOwner`), exit
 * 2, for every role, whatever the route behind it. The pane's GH_REPO is read where the
 * wrapper reads it. A pane that names no LEGION_GH_OWNERS_DIR is given the wrapper's refusal of
 * that, its first line, exit 2 too. Where the label's wrapper runs such a call on the pane's own
 * token instead (main's since #4035: a call that writes nothing, naming only owners outside the
 * project, from a pane that names its GH_CONFIG_DIR, gh-pane-token.ts's `runsOnPaneToken`), the
 * call goes on to the world's GitHub as any other does (`onPaneToken`), and gets what it answers
 * for that owner: production's own answer to a read agents made of an action the world's workflow
 * pins (`pinned`, the capture's), and GitHub's 404 for any other repository the world does not
 * hold, as GitHub answers a token that cannot see it. A run with no world names no project; a
 * world whose selectors cannot be read, or name no words of the wrapper's refusal, is the rig's
 * fault, and the call ends with 2, naming the file, as a world.json that is no world ends it.
 *
 * The wrapper's refusal of a write whose repository GitHub answers 404 to the leak guard's read of
 * its visibility, exit 2 before gh runs, saying that GitHub answers 404 for the repository (main's
 * since #4551, leakguard's `NotFound`, where the guard's setup error refused it before), is not
 * modelled: a write naming another repository of the world's owner passes the owner refusal
 * below, meets no route (a write's route reads its endpoint whole) and is answered `unrouted`,
 * gh's 404 and exit 1. A residual of the class search (the architect's ruling of
 * 2026-11-20T10:05Z, class-search.json): a run that makes such a write is unhealthy from that
 * write on (health.ts's rule 20, `unmodelledWrite`), and an unhealthy run is replaced. No other
 * write meets the refusal: the world's own repository exists, so the guard's read of it is
 * answered `public` (rig.sh's fixture_write); a write naming an owner outside the world's project
 * is refused exit 2 below, before any route reads it (`ownerRefusal`); and the world's repository
 * is no fork (fixture_write's `fork:false`), so the git credential helper's answer for a fork
 * (cmd/legion's `forkUpstreamDir`) is never asked. */
if (as === "gh" && world !== undefined) {
  handler = "wrapper";
  let selectors: GhSelectors | undefined;
  try {
    selectors = runSelectors(run);
  } catch (error) {
    process.stderr.write(`gh: ${error instanceof Error ? error.message : String(error)}\n`);
    exit(2);
  }
  if (selectors === undefined) {
    process.stderr.write(
      `gh: ${run}/${RUN_SELECTORS} is not there, so no owner of this world's project is known (rig.sh's worker_boot writes it)\n`
    );
    exit(2);
  }
  if (selectors.unservedOwner === undefined) {
    process.stderr.write(
      `gh: ${run}/${RUN_SELECTORS} names no words of the wrapper's refusal of an owner outside this world's project (rig.sh's worker_boot writes them, daemon-pane.ts's labelGhSelectors)\n`
    );
    exit(2);
  }
  const owners = process.env.LEGION_GH_OWNERS_DIR;
  const refusal = ownerRefusal(
    ghTargetOf(args, selectors, process.env),
    world.repo,
    owners ?? "",
    selectors.unservedOwner
  );
  if (refusal !== undefined && !owners) {
    process.stderr.write(
      "legion gh: LEGION_GH_OWNERS_DIR is unset: a Legion pane or pod sets it, and nothing else runs this gh\n"
    );
    exit(2);
  }
  if (refusal !== undefined && !runsOnPaneToken(args, selectors, world.repo, process.env)) {
    process.stderr.write(refusal);
    exit(2);
  }
  onPaneToken = refusal !== undefined;
  handler = "unrouted";
}
/** Production's answer to this call where it is a `gh api` read the capture holds
 * (gh-pane-token.ts's `capturedRead`) and the label's wrapper ran it on the pane's token
 * (`onPaneToken`): the architect's rulings of 2026-11-22T17:49:24Z and 18:00:12Z, which answer
 * those reads, in every form legion gh runs that way, from the capture in place of the world's
 * 404, and of 2026-11-23T05:52:25Z, under which a captured 404 is answered so too, with its body,
 * its stderr and its exit, where the world printed its own 404 (round 34's a167a5bc6341 met it
 * formatted over five lines, naming no page of GitHub's REST reference). The `--jq`, `--paginate`
 * and `--slurp` of a read answered exit 0 read its body below, as every answer's do; gh prints a
 * refused read's body as GitHub sent it and runs no `--jq` over it (`refuseWith`). Undefined for
 * any other call: a form the wrapper refuses (`-i`) is refused above, and no other read of
 * another owner is modelled. */
const pinned = onPaneToken && args[0] === "api" ? capturedRead(args) : undefined;
/** What `git -C <run>/remote.git <args...>` prints, or undefined when the command fails: the
 * run's own bare remote, read at the call. A run with no remote of its own fails every call. */
function fromRemote(...args: string[]): string | undefined {
  const read = Bun.spawnSync(["git", "-C", `${run}/remote.git`, ...args]);
  return read.exitCode === 0 ? read.stdout.toString().trim() : undefined;
}
/** What `git -C <run>/remote.git <args...>` writes to its stdout, byte for byte, or undefined
 * when the command fails: the read of a file's own bytes, whose trailing newline is the file's. */
function remoteBytes(...args: string[]): Buffer | undefined {
  const read = Bun.spawnSync(["git", "-C", `${run}/remote.git`, ...args]);
  return read.exitCode === 0 ? Buffer.from(read.stdout) : undefined;
}
// What GitHub answers for the pull request's head is the commit its branch points at, so a route
// whose object carries `headRefOid` answers the tip of the run's own remote rather than the
// fixture's first commit: a role that pushes and then reads the head, which the worker skill has
// every pushing role do, must see its own commit. The tip is read from the run's bare remote at
// the call, and a remote the run has not got leaves the fixture's value as it is.
function branchTip(): string | undefined {
  return world === undefined ? undefined : fromRemote("rev-parse", `refs/heads/${world.branch}`);
}
// What GitHub serves for `gh pr diff` is the pull request's diff as it stands, so the answer is
// the diff of the run's own remote at the call, the issue branch against where it left the branch
// it is open against (GitHub's diff of a pull request is from the merge base, `base...head`),
// rather than a diff frozen into the fixture, which would read the same whatever the run pushed.
// A role that reads the pull request after another role pushed to it — the reviewer, reading the
// fix the implementer pushed after an approval that left text threads open — must see what was
// pushed. `--name-only` answers the names alone, as `gh pr diff --name-only` does, and the files
// `gh pr view --json files` lists are this diff's (`prFiles`). A run with no world of its own has
// no remote to read, and its call falls through to the routes, where no world of rig.sh writes a
// `gh pr diff` (its round_world), so what such a call gets is the 404 below.
function branchDiff(...options: string[]): string | undefined {
  if (world === undefined) return undefined;
  return fromRemote("diff", ...options, `${world.base}...${world.branch}`);
}
/** `repos/<owner>/<repo>/compare/<basehead>`, with the query string `gh api` lets a caller
 * append, where the basehead holds two dots or more: `<base>...<head>`, the comparison GitHub
 * reads, and `<base>..<head>`, which GitHub answers 404 (`comparison`). */
const COMPARE = /^\/?repos\/[^/]+\/[^/]+\/compare\/([^?]*\.\.[^?]*)(?:\?.*)?$/;
/** The two refs of a basehead GitHub compares, `<base>...<head>`. A ref may carry dots, and `...`
 * is the separator, so the base is everything up to the last `...`. */
const BASEHEAD = /^(.+)\.\.\.(.+)$/;
/** The page of GitHub's REST reference its 404 of a comparison names (`notFoundBody`). */
const COMPARE_DOCS = "https://docs.github.com/rest/commits/commits#compare-two-commits";
/** Whether the call asks for the comparison as a diff rather than as JSON: `gh api -H "Accept:
 * application/vnd.github.v3.diff"`, the media type the API documents for a diff, and the
 * `vnd.github.diff` spelling GitHub takes for the same answer. */
const DIFF_MEDIA_TYPE = /application\/vnd\.github(?:\.v3)?\.diff/;
/** A ref this stand-in will hand `git`. A ref is argv the caller wrote, and `git` reads a leading
 * `-` as an option wherever it falls, so a ref such as `--output=<path>...main` would write a
 * file; `--end-of-options`, which `git diff` does not take, cannot close that, so a ref that
 * starts with `-` is refused instead. There is no privilege boundary in this rig — the agent that
 * writes the ref already runs shell commands of its own — so this is a guard against a ref that
 * reads as an option and no sandbox. A refused ref is one the remote does not hold, and each read
 * answers it as GitHub answers a ref it has not got. */
const REF = /^[^-]/;
/** What GitHub answers for `gh api repos/<o>/<r>/compare/<base>...<head>`, read from the run's
 * own remote at the call, or undefined when this call is no such comparison or the run has no
 * world. The rule has the implementer read that comparison to show its push changes
 * no code that runs (`skills/legion-worker/references/review-threads.md`, step 2), and the real
 * GitHub API answers it: measured on 2026-11-10, `gh api
 * repos/cybermonklabs/legion/compare/756789f8...main` printed `status`, `ahead_by`, `behind_by`
 * and 300 files and exited 0, while this stand-in answered `gh: Not Found (HTTP 404)` and the
 * implementer's comparison failed in all 10 head runs of the batch before this of the
 * threads-closed case that `correct` replaced. No
 * route serves it and none should: an answer frozen into the fixture would read the same
 * whatever the run pushed, where the two heads a comparison names are the run's own. `ahead_by`
 * and `behind_by` are the commits each side holds that the other has not, `status` is GitHub's
 * word for that pair, and `files` is the diff from the merge base, which is the change the head
 * carries, each file carrying the hunks of its own diff as `patch`, which is the field the
 * reviewer reads a change out of a comparison with and which this answered as `null` through
 * the recorded batch of 2026-11-10: a reviewer that asked for `.patch` got nothing and one fell
 * back to `jj file show`, working around the rig on the read step 3 of review-threads.md rests
 * on. The files are the comparison's diff from the merge base (`diffFiles`). A call that asks for
 * the diff media type (DIFF_MEDIA_TYPE) is answered with the unified diff itself, as the API
 * answers that `Accept` header, and never with the JSON.
 *
 * A comparison the remote cannot answer is GitHub's 404 of one, its body naming "Compare two
 * commits" (COMPARE_DOCS): a ref the remote cannot resolve, one REF refuses, and a basehead of two
 * dots, which GitHub does not read as a comparison. Measured on 2026-11-21, `gh api -i
 * repos/cybermonklabs/legion/compare/446a02fb..bbf69791` answered `HTTP/2.0 404 Not Found` (its
 * `Date: Sat, 21 Nov 2026 08:43:19 GMT`) with that body, where the same two commits written
 * `446a02fb...bbf69791` answered 200 (08:43:20 GMT), and `compare/nosuchbranch-xyz...main`
 * answered the same 404 (08:43:22 GMT). The world answered the first and the last from no route,
 * `unrouted`, with the 404 body of an endpoint GitHub does not serve. */
function comparison(): string | Record<string, unknown> | undefined {
  if (as !== "gh" || args[0] !== "api") return undefined;
  // A comparison is GitHub's read of two refs, so a call that writes to that path is another
  // call and falls to the routes. The verb is the one the score reads (`methodOf`), never a
  // regex of this file's own: a write written `-XPOST`, `--method=post` or as a field with no
  // method was answered here with the two refs' diff, which told its caller a write it never
  // made had succeeded.
  if (verb !== "GET") return undefined;
  const basehead = args.map((arg) => COMPARE.exec(arg)?.[1]).find((named) => named !== undefined);
  if (basehead === undefined || world === undefined) return undefined;
  const unanswered: () => never = () => {
    handler = "comparison";
    refuse(NOT_FOUND, COMPARE_DOCS);
  };
  const [, base = "", head = ""] = BASEHEAD.exec(basehead) ?? [];
  if (!REF.test(base) || !REF.test(head)) unanswered();
  const range = [`${base}...${head}`];
  // The diff media type asks for the comparison as the diff itself, which is what the run's own
  // remote prints for the range, to its last line end, as GitHub's body ends (measured on
  // 2026-11-18: the diff media type of a compare and of a pull request of cybermonklabs/legion each
  // ended in `\n`, which gh printed as it came); a remote that cannot resolve the two refs prints
  // nothing, and the call is GitHub's 404.
  if (DIFF_MEDIA_TYPE.test(joined))
    return remoteBytes("diff", "--no-renames", ...range)?.toString() ?? unanswered();
  const ahead = fromRemote("rev-list", "--count", `${base}..${head}`);
  const behind = fromRemote("rev-list", "--count", `${head}..${base}`);
  const baseSha = fromRemote("rev-parse", "--verify", "--quiet", `${base}^{commit}`);
  const headSha = fromRemote("rev-parse", "--verify", "--quiet", `${head}^{commit}`);
  const mergeBase = fromRemote("merge-base", base, head);
  if (ahead === undefined || behind === undefined) unanswered();
  if (baseSha === undefined || headSha === undefined || mergeBase === undefined) unanswered();
  const files = diffFiles(range, headSha, mergeBase) ?? unanswered();
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
  const repo = world.repo;
  const owner = repo.split("/")[0] ?? "";
  // The two refs as the call named them, as GitHub's own URLs name them, and its permalink by the
  // two commits, each its owner's and seven digits long.
  const html = `https://github.com/${repo}/compare/${base}...${head}`;
  const commits = (fromRemote("rev-list", "--reverse", `${base}..${head}`) ?? "")
    .split("\n")
    .filter((sha) => sha !== "");
  return {
    url: `https://api.github.com/repos/${repo}/compare/${base}...${head}`,
    html_url: html,
    permalink_url: `https://github.com/${repo}/compare/${owner}:${baseSha.slice(0, 7)}...${owner}:${headSha.slice(0, 7)}`,
    diff_url: `${html}.diff`,
    patch_url: `${html}.patch`,
    base_commit: commitJson(baseSha),
    merge_base_commit: commitJson(mergeBase),
    status,
    ahead_by: aheadBy,
    behind_by: behindBy,
    total_commits: aheadBy,
    commits: commits.map(commitJson),
    files,
  };
}
/** The files of the diff `range` (`git diff`'s arguments: `<base>...<head>` for a comparison, a
 * commit's first parent and the commit for a commit) on the run's own remote, as GitHub's
 * `diff-entry` lists each, with the blob it is at `at`, or for a file the range removed the one it
 * was at `before`; undefined where the remote cannot read the range. Each file carries the hunks
 * of its own diff as `patch`, read with a `git diff` of its own path, since splitting one combined
 * diff on its `diff --git` headers would have to undo git's own quoting of a path; a world of this
 * rig holds four changed files. Renames are not detected, so a moved file reads as one added and
 * one removed, as GitHub's own answer does when it finds no rename. */
function diffFiles(range: readonly string[], at: string, before: string): Json[] | undefined {
  const counts = fromRemote("diff", "--numstat", "--no-renames", ...range);
  const letters = fromRemote("diff", "--name-status", "--no-renames", ...range);
  if (counts === undefined || letters === undefined) return undefined;
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
  const repo = world?.repo ?? "";
  return counts
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
      const fileStatus = statusOf.get(filename) ?? "modified";
      const side = fileStatus === "removed" ? before : at;
      const encoded = filename.split("/").map(encodeURIComponent).join("/");
      return {
        sha: fromRemote("rev-parse", `${side}:${filename}`) ?? "",
        filename,
        status: fileStatus,
        additions,
        deletions,
        changes: additions + deletions,
        blob_url: `https://github.com/${repo}/blob/${at}/${encoded}`,
        raw_url: `https://github.com/${repo}/raw/${at}/${encoded}`,
        contents_url: `https://api.github.com/repos/${repo}/contents/${encoded}?ref=${at}`,
        ...(hunks < 0 ? {} : { patch: diff.slice(hunks) }),
      };
    });
}
/** Whether gh prints the status line and the headers of GitHub's answer before its body: `gh api
 * -i` (`--include`), which the census of INFRA-764's runs found 14 calls of in 15 runs (round 29's
 * run 2d821d91b790, calls.jsonl:2, among them), each answered the body alone where gh prints both. */
const include =
  as === "gh" && args[0] === "api" && args.some((arg) => /^(?:-i|--include)$/.test(arg));
/** The reason phrase Go's net/http gives each status the world answers with, which gh prints after
 * the protocol (`resp.Status`). */
const STATUS_TEXT: Readonly<Record<number, string>> = {
  200: "OK",
  201: "Created",
  403: "Forbidden",
  404: "Not Found",
  422: "Unprocessable Entity",
};
/** The headers GitHub sends with every answer, REST or GraphQL, served or not, each a fact of
 * GitHub's service and none of one request: production's values, as the pin holds them
 * (production-headers.json's reads, made with `gh api -i` against cybermonklabs/legion on
 * 2026-11-21, each with its `Date:`). `X-Github-Edge-Region` is the edge GitHub answers this
 * cluster's pods from, which a world's agent, on the same pods, would be answered from. */
const SERVICE_HEADERS: Readonly<Record<string, string>> = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Expose-Headers":
    "ETag, Link, Location, Retry-After, X-GitHub-OTP, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Used, X-RateLimit-Resource, X-RateLimit-Reset, X-OAuth-Scopes, X-Accepted-OAuth-Scopes, X-Poll-Interval, X-GitHub-Media-Type, X-GitHub-SSO, X-GitHub-Request-Id, Deprecation, Sunset, Warning",
  "Content-Security-Policy": "default-src 'none'",
  "Referrer-Policy": "origin-when-cross-origin, strict-origin-when-cross-origin",
  Server: "github.com",
  "Strict-Transport-Security": "max-age=31536000; includeSubdomains; preload",
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "deny",
  "X-Github-Edge-Region": "westus2",
  "X-Xss-Protection": "0",
};
/** The permission GitHub names an App's read of each route needs (`X-Accepted-Github-Permissions`),
 * as production answered each read of the pin's (production-headers.json's `permissions`, each
 * with its `Date:`); null for a read it answered with none (`git/refs`). A route this table holds
 * nothing of — the world's writes, which no pod may make in production to measure — is answered
 * with no such header, a declared residual of the pin (the README's pin section). */
const ACCEPTED_PERMISSIONS: Readonly<Record<string, string | null>> = {
  "GET repos/{owner}/{repo}": "metadata=read",
  "GET repos/{owner}/{repo}/pulls/{number}": "pull_requests=read; contents=read",
  "GET repos/{owner}/{repo}/pulls/{number}/commits": "pull_requests=read",
  "GET repos/{owner}/{repo}/pulls/{number}/files": "pull_requests=read",
  "GET repos/{owner}/{repo}/pulls/{number}/reviews": "pull_requests=read",
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}": "pull_requests=read",
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments": "pull_requests=read",
  "GET repos/{owner}/{repo}/pulls/{number}/comments": "pull_requests=read",
  "GET repos/{owner}/{repo}/issues/{number}/comments": "issues=read; pull_requests=read",
  "GET repos/{owner}/{repo}/commits/{ref}/check-runs": "checks=read",
  "GET repos/{owner}/{repo}/commits/{ref}/status": "statuses=read",
  "GET repos/{owner}/{repo}/commits/{ref+}": "contents=read",
  "GET repos/{owner}/{repo}/commits": "contents=read",
  "GET repos/{owner}/{repo}/actions/runs": "actions=read",
  "GET repos/{owner}/{repo}/actions/runs/{run_id}/jobs": "actions=read",
  "GET repos/{owner}/{repo}/rules/branches/{branch+}": "metadata=read",
  "GET repos/{owner}/{repo}/rulesets": "metadata=read",
  "GET repos/{owner}/{repo}/rulesets/{id}": "metadata=read",
  "GET repos/{owner}/{repo}/branches/{branch}/protection": "administration=read",
  "GET repos/{owner}/{repo}/branches/{branch+}": "contents=read",
  "GET repos/{owner}/{repo}/branches": "contents=read",
  "GET repos/{owner}/{repo}/tags": "metadata=read",
  "GET repos/{owner}/{repo}/compare/{basehead+}": "contents=read",
  "GET repos/{owner}/{repo}/git/ref/{ref+}": "contents=read",
  "GET repos/{owner}/{repo}/git/refs": null,
  "GET repos/{owner}/{repo}/git/refs/{ref+}": "contents=read",
  "GET repos/{owner}/{repo}/git/commits/{sha}": "contents=read",
  "GET repos/{owner}/{repo}/git/trees/{sha+}": "contents=read",
  "GET repos/{owner}/{repo}/contents/{path+}": "contents=read",
  "GET user": "allows_permissionless_access=true",
};
/** The route of WORLD_ROUTES this call is, read only where the call asks for the headers
 * (`include`), and undefined for one no row answers. */
const worldRoute = include ? worldRouteOf(routeOf(args)?.key ?? "")?.route : undefined;
/** The world's REST routes whose success GitHub answers `201 Created`, as its description names
 * each operation's success (rest-required.json's `status`, which gh-standin.test.ts's row of the
 * status lines holds this to): the creates of a review comment, of a reply and of a conversation
 * comment. Every other success the world answers is `200 OK`, a GraphQL answer's among them. */
const CREATED_ROUTES: Readonly<Record<string, true>> = {
  "POST repos/{owner}/{repo}/pulls/{number}/comments": true,
  "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies": true,
  "POST repos/{owner}/{repo}/issues/{number}/comments": true,
};
/** How a REST list this call answers was paged (`restPages`): the first page it printed, the size
 * of a page and the length of the whole list; undefined for an answer that is no paged list. */
let paging: { first: number; perPage: number; total: number } | undefined;
/** The status line and the headers gh prints before the body of GitHub's answer to `gh api -i`,
 * as gh 2.98.0 prints them (pkg/cmd/api's `processResponse`): the protocol and the status on a
 * line ended `\n`, then each header as `Name: value` in the byte order of the names, each line
 * ended `\r\n`, then `\r\n` alone; the empty string for a call that asks for none. `status` is the
 * answer's, `body` the bytes GitHub sends (the page's, before any `--jq`), `media` the text media
 * type a call asked for (`diff`, `patch`, `sha`, `raw`), `page` the page of a paged list it is
 * (from 0) and `modified` the time the answer's object last changed, where the world knows it.
 *
 * The headers are the ones production sent the same class of answer, measured on 2026-11-21 with
 * `gh api -i` against cybermonklabs/legion (production-headers.json, each read with its `Date:`):
 * every answer's service headers (SERVICE_HEADERS) and its `Date`, its `Content-Type` and its
 * `X-Github-Media-Type`, and its `X-Github-Request-Id`; a served REST answer's
 * `X-Github-Api-Version-Selected` and `X-Accepted-Github-Permissions` (ACCEPTED_PERMISSIONS); a
 * served answer's rate limit; a REST success's `Cache-Control`, `Etag` and `Vary`, an error's or a
 * GraphQL answer's own `Vary`; a text answer's `Content-Length`; a list's `Link` to its other
 * pages, as GitHub writes it, the repository by its id (`repositories/<id>/…`) and the query
 * string the call sent with `page` set; and `Last-Modified`, which production sent for the
 * repository, as its `updated_at`, and for the pull request and its commits and files, as the pull
 * request's. An endpoint GitHub does not serve was answered with no rate limit, no API version and
 * no permission (`repos/cybermonklabs/legion/no-such-endpoint`, `Date: Sat, 21 Nov 2026 10:35:20
 * GMT`); a GraphQL answer with no `Cache-Control`, `Etag`, API version or permission
 * (`graphql`, 10:21:11 GMT).
 *
 * Each value that describes the answer is the world's: `Date` the call's time, `Etag` a weak
 * validator of the world's own body (production's is no hash of the body it sent: its repository's
 * 6814 bytes hash to cbc29673… and its ETag was W/"3019da26…"), `X-Github-Request-Id` one of the
 * world's in GitHub's form, and the rate limit the role App's: production's limit for the
 * resource (15000 a core hour and 10000 a GraphQL hour for a role App), used by every `gh api`
 * call of that resource the run's record holds from the call's role and GitHub answered, this one
 * counted, and reset an hour after the first of them. `Last-Modified` for every other route
 * (production sent one for the commits, refs, trees, tags and contents, at times the world's
 * objects do not give: `git/ref/heads/main` answered the repository's `updated_at` and
 * `git/refs/heads/main` the commit's, at 10:24:58 and 10:25:00 GMT) and
 * `X-Accepted-Github-Permissions` for a write are written by no answer: declared residuals of the
 * pin (the README's pin section). */
function headerBlock(
  status: number,
  body: string,
  { media, page = 0, modified }: { media?: string; page?: number; modified?: unknown } = {}
): string {
  if (!include) return "";
  const graphql = graphqlCall !== undefined;
  const served = handler !== "unrouted";
  const json = media === undefined;
  const headers: Record<string, string> = {
    ...SERVICE_HEADERS,
    Date: new Date(at).toUTCString(),
    "Content-Type": json
      ? "application/json; charset=utf-8"
      : `application/vnd.github.${media}; charset=utf-8`,
    "X-Github-Media-Type": graphql
      ? "github.v4; format=json"
      : json
        ? "github.v3; format=json"
        : `github.v3; param=${media}`,
    "X-Github-Request-Id": new Bun.CryptoHasher("sha256")
      .update(`${run}\0${at}\0${args.join("\0")}`)
      .digest("hex")
      .toUpperCase()
      .replace(/^(.{4})(.{6})(.{6})(.{6})(.{8}).*$/, "$1:$2:$3:$4:$5"),
    Vary:
      status < 400 && !graphql
        ? "Accept, Authorization, Cookie, X-GitHub-OTP,Accept-Encoding, Accept, X-Requested-With"
        : "Accept-Encoding, Accept, X-Requested-With",
  };
  if (!graphql && served) headers["X-Github-Api-Version-Selected"] = "2022-11-28";
  const permission = worldRoute === undefined ? undefined : ACCEPTED_PERMISSIONS[worldRoute];
  if (!graphql && served && typeof permission === "string")
    headers["X-Accepted-Github-Permissions"] = permission;
  if (status < 400 && !graphql) {
    headers["Cache-Control"] = "private, max-age=60, s-maxage=60";
    headers.Etag = `W/"${new Bun.CryptoHasher("sha256").update(body).digest("hex")}"`;
  }
  if (!json) headers["Content-Length"] = String(Buffer.byteLength(body));
  if (status < 400 && typeof modified === "string")
    headers["Last-Modified"] = new Date(modified).toUTCString();
  if (served) {
    const resource = graphql ? "graphql" : "core";
    const limit = graphql ? 10_000 : 15_000;
    const by = process.env.LEGION_ROLE || undefined;
    const counted = recordedCalls(RECORD).filter(
      (call) =>
        call.as === "gh" &&
        call.argv[0] === "api" &&
        call.by === by &&
        !["unrouted", "wrapper"].includes(call.handler ?? "") &&
        (endpointPath(call.argv) === "graphql") === graphql
    );
    const since = Date.parse(counted[0]?.at ?? at);
    headers["X-Ratelimit-Limit"] = String(limit);
    headers["X-Ratelimit-Remaining"] = String(limit - counted.length - 1);
    headers["X-Ratelimit-Reset"] = String(Math.floor(since / 1000) + 3600);
    headers["X-Ratelimit-Resource"] = resource;
    headers["X-Ratelimit-Used"] = String(counted.length + 1);
  }
  const link = paging === undefined ? undefined : linkOf(paging.first + page, paging);
  if (link !== undefined) headers.Link = link;
  return [
    `HTTP/2.0 ${status} ${STATUS_TEXT[status] ?? ""}\n`,
    ...Object.keys(headers)
      .sort()
      .map((name) => `${name}: ${headers[name]}\r\n`),
    "\r\n",
  ].join("");
}
/** GitHub's `Link` header of page `page` of a paged list: the previous page and the next, the
 * last and the first, each where it is another page, in GitHub's order, each the call's own URL on
 * the repository by its id with `page` set where the query string names it, or added after it.
 * Measured on 2026-11-21 against cybermonklabs/legion: `pulls/2827/files?per_page=2&page=2` linked
 * `…/repositories/1146927990/pulls/2827/files?per_page=2&page=1` as `prev`, then `page=3` as `next`,
 * `page=58` as `last` and `page=1` as `first` (`Date: Sat, 21 Nov 2026 10:25:05 GMT`);
 * `?page=2&per_page=2` the same with `page` first (10:25:06 GMT); `?per_page=2&page=58` `prev` and
 * `first` alone (10:25:07 GMT); and `pulls/2827/comments?per_page=1`, the first page, `next` and
 * `last` alone (10:21:02 GMT). */
function linkOf(page: number, { perPage, total }: { perPage: number; total: number }) {
  const last = Math.max(1, Math.ceil(total / perPage));
  const rest = endpointPath(args).replace(/^repos\/[^/]+\/[^/]+/, "");
  const id = String(heldRepository().id ?? "");
  const url = (to: number) => {
    const query = new URLSearchParams([...parameters()]);
    query.set("page", String(to));
    return `<https://api.github.com/repositories/${id}${rest}?${query}>`;
  };
  const links = [
    ...(page > 1 ? [`${url(page - 1)}; rel="prev"`] : []),
    ...(page < last ? [`${url(page + 1)}; rel="next"`, `${url(last)}; rel="last"`] : []),
    ...(page > 1 ? [`${url(1)}; rel="first"`] : []),
  ];
  return links.length === 0 ? undefined : links.join(", ");
}
/** The HTTP status gh names in its line for GitHub's error (`… (HTTP 404)`), or undefined for a
 * line that names none. */
function httpStatusOf(text: string): number | undefined {
  const named = /\(HTTP (\d{3})\)\n?$/m.exec(text)?.[1];
  return named === undefined ? undefined : Number(named);
}
/** The comparison this call asks for, when it asks for one the run's remote can answer. It is
 * read before the route check, since no route answers a comparison and a 404 there reads to the
 * agent as a repository that does not hold the two commits its own run made. */
const compared = comparison();
/** Refuses this call with GitHub's own text and its own status, where that refusal is decided:
 * the text goes to stderr as the call's whole answer and the record keeps the status, as the 404
 * below does. The answer the rest of this file shapes is the answer of a call that was taken, so
 * a refusal carried through it would have to be stepped around at each turn of that pipeline.
 * GitHub's 404 of a `gh api` call carries its JSON error body too (`notFoundBody`), which gh
 * prints on stdout before it reports the status; `documentation` is the page the body names. */
function refuse(text: string, documentation?: string): never {
  const body =
    text === NOT_FOUND && as === "gh" && args[0] === "api" ? notFoundBody(documentation) : "";
  const status = httpStatusOf(text);
  if (status !== undefined) process.stdout.write(headerBlock(status, body));
  process.stdout.write(body);
  process.stderr.write(text);
  exit(1);
}
/** The body GitHub answers a 404 with, as gh prints it on stdout before `gh: Not Found (HTTP 404)`
 * on stderr: GitHub's bytes, with no line end after them, which gh copies as they came. For an
 * endpoint GitHub does not serve, the body is indented with `\r\n` line ends and names GitHub's
 * REST reference (`documentation` omitted); for an object a served endpoint does not hold, it is
 * one line naming that endpoint's own page (`documentation`: a missing review comment's reply
 * names "Create a reply for a review comment"). Measured on 2026-11-18 from an INFRA issue pod
 * with gh 2.98.0: `gh api repos/cybermonklabs/legion/no-such-endpoint` printed the first form and
 * `gh api repos/cybermonklabs/legion/pulls/2827/reviews/1` the second (naming "Get a review for a
 * pull request"), each exit 1 and ending in its `}`; the world printed both indented with `\n`
 * line ends and one more after the last. */
function notFoundBody(documentation?: string): string {
  if (documentation !== undefined)
    return JSON.stringify({
      message: "Not Found",
      documentation_url: documentation,
      status: "404",
    });
  return '{\r\n  "message": "Not Found",\r\n  "documentation_url": "https://docs.github.com/rest",\r\n  "status": "404"\r\n}';
}
/** Refuses this call as gh prints an HTTP error GitHub answered `gh api` with: `body`, GitHub's
 * error body, on stdout as GitHub sent it, on one line with no line end after it, `text`, gh's
 * line for it, on stderr, and `status`. gh runs no `--jq` over an error's body: measured on
 * 2026-11-17 from a Legion issue pod with the role App token its gh gives, `gh api
 * repos/cybermonklabs/legion/branches/main/protection --jq '.required_status_checks.contexts[]'`
 * printed GitHub's 403 body on one line, `gh: Resource not accessible by integration (HTTP 403)`
 * on stderr, and exited 1, and `gh api repos/cybermonklabs/legion/rulesets/1` (no ruleset of that
 * repository) printed `{"message":"Not
 * Found","documentation_url":"https://docs.github.com/rest/repos/rules#get-a-repository-ruleset",
 * "status":"404"}` on one line and `gh: Not Found (HTTP 404)`; on 2026-11-18 that read and `gh api
 * repos/cybermonklabs/legion/installation` (a 401) each ended in the body's `}`, no line end after
 * it (the implementer's readings, m/installation.out). A body of none prints nothing. */
function refuseWith(body: unknown, text: string, status: number): never {
  const sent = body === undefined ? "" : JSON.stringify(body);
  const http = httpStatusOf(text);
  if (http !== undefined) process.stdout.write(headerBlock(http, sent));
  process.stdout.write(sent);
  process.stderr.write(text);
  exit(status);
}
/** GitHub's 404 of a ruleset the repository has not got, as `refuseWith` measured it. */
const NO_RULESET = {
  message: "Not Found",
  documentation_url: "https://docs.github.com/rest/repos/rules#get-a-repository-ruleset",
  status: "404",
};
/** The pages of GitHub's REST reference whose endpoints this world serves and answers 404 for an
 * object it does not hold, which GitHub's 404 body names (`notFoundBody`). */
const REVIEW_DOCS = "https://docs.github.com/rest/pulls/reviews#get-a-review-for-a-pull-request";
const REVIEW_COMMENTS_DOCS =
  "https://docs.github.com/rest/pulls/reviews#list-comments-for-a-pull-request-review";
const REVIEW_UPDATE_DOCS =
  "https://docs.github.com/rest/pulls/reviews#update-a-review-for-a-pull-request";
const REF_DOCS = "https://docs.github.com/rest/git/refs#get-a-reference";
const CONTENTS_DOCS = "https://docs.github.com/rest/repos/contents#get-repository-content";
const COMMITS_DOCS = "https://docs.github.com/rest/commits/commits#list-commits";
const GIT_COMMIT_DOCS = "https://docs.github.com/rest/git/commits#get-a-commit-object";
const REFS_DOCS = "https://docs.github.com/rest/git/refs#get-all-references-in-a-namespace";
const TREE_DOCS = "https://docs.github.com/rest/git/trees#get-a-tree";
const PULL_COMMITS_DOCS = "https://docs.github.com/rest/pulls/pulls#list-commits-on-a-pull-request";
const PULL_FILES_DOCS = "https://docs.github.com/rest/pulls/pulls#list-pull-requests-files";
/** GitHub's 422 of a commit its repository does not hold, as `gh api -i
 * repos/cybermonklabs/legion/commits/<forty deadbeef digits>` answered it on 2026-11-21 (`Date:
 * Sat, 21 Nov 2026 08:42:54 GMT`): its body on stdout and gh's line on stderr (`refuseWith`). */
function noCommit(ref: string): never {
  refuseWith(
    {
      message: `No commit found for SHA: ${ref}`,
      documentation_url: "https://docs.github.com/rest/commits/commits#get-a-commit",
      status: "422",
    },
    `gh: No commit found for SHA: ${ref} (HTTP 422)\n`,
    1
  );
}
/** GitHub's own text for the submit of a review that is no longer pending. */
const NOT_PENDING =
  "gh: Unprocessable Entity (HTTP 422)\nCan not submit a review that is not pending.\n";
/** GitHub's own text for a submit that names no event, or one it does not take. The status and
 * the refusal are GitHub's — `event` is required on that call, and it answers 422 without it,
 * which is why a submit with none must not be answered from a route — while this sentence is
 * this stand-in's own, since GitHub's exact wording for the two cases is not measured here. */
const NO_EVENT =
  "gh: Unprocessable Entity (HTTP 422)\nA review is submitted with the event APPROVE, REQUEST_CHANGES or COMMENT.\n";
/** GitHub's own text for the create of a pending review while this user has one pending on the
 * pull request already. */
const ONE_PENDING =
  "gh: Unprocessable Entity (HTTP 422)\nUser can only have one pending review per pull request\n";
/** The create of a review naming an event GitHub does not take, and the create of a
 * `REQUEST_CHANGES` or `COMMENT` review with no body (gh-call.ts's `createdState`). GitHub
 * answers both 422; each sentence is this stand-in's own, as NO_EVENT's is, since GitHub's exact
 * wording for these two is not measured here. */
const CREATE_EVENT =
  "gh: Unprocessable Entity (HTTP 422)\nA review is created with the event APPROVE, REQUEST_CHANGES or COMMENT, or with none to leave it pending.\n";
const CREATE_BODY =
  "gh: Unprocessable Entity (HTTP 422)\nA review with the event REQUEST_CHANGES or COMMENT is created with a body.\n";
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
 * Measured on 2026-11-11, in threads-closed's head arm: a reviewer approved correctly at the head
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
 * as the verdict that stands, which passes the defect control the arm exists to fail.
 *
 * A submit that names no event, or an event GitHub does not take, is refused here too (NO_EVENT)
 * rather than left to the routes: the create route answered such a call `{id}` and exit 0, so a
 * round that submitted nothing GitHub would take was told its review had been submitted. */
function reviewSubmission(): Record<string, unknown> | undefined {
  if (as !== "gh" || args[0] !== "api" || !isPost(args) || route === undefined) return undefined;
  // The repository of the run's own world: a submit on another repository's pull request is no
  // submit of this run's review, and a run that builds no world has no review of its own.
  const repo = world?.repo;
  if (repo === undefined) return undefined;
  const id = args.map((arg) => submitsReview(arg, repo)).find((named) => named !== undefined);
  if (id === undefined) return undefined;
  handler = "review submit";
  const pending = ownReviews(repo).pending;
  if (pending === undefined || pending.id !== id) refuse(NOT_PENDING);
  const body = sentBody({ argv: args, stdin, files }) ?? {};
  const state = submittedState(body.event);
  if (state === undefined) refuse(NO_EVENT);
  const text = typeof body.body === "string" && body.body !== "" ? body.body : pending.body;
  return reviewObject({ ...pending, state, body: text, at });
}
/** The role this call's pane names (LEGION_ROLE), undefined where it names none. */
const role = process.env.LEGION_ROLE || undefined;
/** The App whose token this call's pane calls GitHub with (rest-objects.ts's `roleApp`). */
const viewer = roleApp(role);
/** One review this run created on the world's pull request, as this stand-in answered it: the
 * world's answer to its create (its URL), the id GitHub gave it (gh-threads.ts's
 * `createdReviewId`), the state and the head the create — and, once it was submitted, the submit
 * — were answered with, the body it carries and when it was made, or, once it was submitted, when
 * it was submitted, the App whose token created it (`app`) and that App's bot, its author
 * (`login`). Its inline comments are threads of the pull request (`threadState`). */
type OwnReview = {
  answer: Record<string, unknown>;
  id: string;
  state: string;
  head: string;
  body: string;
  at: string;
  app: string;
  login: string;
};
/** Whether this call sees `review`: GitHub shows a review still pending to the account that wrote
 * it alone, and every other review to every account. Before this, a reviewer's pending review was
 * in every role's reviews, read by id, by `gh pr view` and through GraphQL. A read by its id from
 * another App answers the 404 of a review the pull request has not got (the review read's own,
 * `REVIEW_DOCS`): which 404 GitHub gives there is not measured, since no pending review of another
 * account can be found to read. */
function shown(review: OwnReview): boolean {
  return review.state !== "PENDING" || review.app === viewer;
}
/** Every review this run created on `repo`'s pull request, by the id it was answered with, the
 * one the App of this call has pending (`viewer`), or `undefined` when it has none, and the body
 * each review the world held was updated to, by its id, read off the run's record
 * (`recordedCalls`). GitHub keeps a pending review for each account, so each App's is its own: a
 * submit submits the pending review of the App that sends it, and a create leaves one pending for
 * the App that sends it.
 *
 * Each review is read off what this stand-in answered and recorded (`review`), and never worked
 * out again from the call: a create is a review where it was answered with one — a create GitHub
 * refuses, and one this world has no head for, recorded none — and its state and head are the
 * ones its answer carried, which is the head the pull request was at when it was made and not
 * the head it is at now. A `gh pr review` the world took is such a create, its body the one it
 * was answered with. A submit is the submit of the pending review where it was answered with
 * the review: the id was the pending one's and the event one GitHub takes. The status a call was
 * recorded with says nothing about either — a submit answered 200 whose `--jq` then failed is
 * recorded with exit 1, and GitHub has submitted the review all the same — so a reader of the
 * status left it pending and answered the next submit of it 200 twice.
 *
 * An update (`PUT …/reviews/<id>`, recorded by the route that models it, `handler`
 * `review-update`) changes the body of the review it names as it stands then — one this run
 * created, or one the world held — where GitHub took it: a body that is not empty, on a review
 * there. A record from before the world modelled the update, whose catch-all answered one and
 * changed nothing, holds no such call.
 *
 * Each create's id is the one GitHub gives it (gh-threads.ts's `createdReviewId`): the id the
 * world's create route answers with — 5002 in `approved_world`, 6001 in every other world
 * (rig.sh) — counted on by one for each review the run created before it, so no create hides
 * another. Its author is the bot of the App that created it (gh-threads.ts's `loginOf`). */
function ownReviews(repo: string): {
  byId: Map<string, OwnReview>;
  pending: OwnReview | undefined;
  bodies: Map<string, string>;
} {
  const byId = new Map<string, OwnReview>();
  const pending = new Map<string, OwnReview>();
  const bodies = new Map<string, string>();
  const heldIds = new Set(
    [...heldReviews(), ...commentReviews()].map((review) =>
      String(isObject(review) ? review.id : "")
    )
  );
  let created = 0;
  for (const call of recordedCalls(RECORD)) {
    const { argv } = call;
    const prReviewed = isPrReview(argv) && createsReview(call, repo);
    if (argv[0] !== "api" && !prReviewed) continue;
    const sent = prReviewed
      ? { body: call.review?.body }
      : (sentBody({ argv, stdin: call.stdin, files: call.files }) ?? {});
    const text = typeof sent.body === "string" && sent.body !== "" ? sent.body : undefined;
    if (call.handler === "review-update") {
      const id = /\/reviews\/(\d+)$/.exec(endpointPath(argv))?.[1] ?? "";
      const made = byId.get(id);
      if (text !== undefined && made !== undefined) made.body = text;
      else if (text !== undefined && heldIds.has(id)) bodies.set(id, text);
      continue;
    }
    if (call.review === undefined) continue;
    const app = roleApp(call.by);
    const submitted = argv.map((arg) => submitsReview(arg, repo)).find((id) => id !== undefined);
    if (submitted !== undefined) {
      const own = pending.get(app);
      if (own?.id === submitted) {
        own.state = call.review.state;
        own.body = text ?? own.body;
        own.at = call.at ?? own.at;
        pending.delete(app);
      }
      continue;
    }
    if (!createsReview(call, repo)) continue;
    const answer = routeAnswer(routes, call.route);
    const id = createdReviewId(threadWorld, call, created++);
    if (answer === undefined || id === undefined) continue;
    const review = {
      answer,
      id: String(id),
      ...call.review,
      body: text ?? "",
      at: call.at ?? "",
      app,
      login: loginOf(threadWorld, call.by),
    };
    byId.set(review.id, review);
    if (review.state === "PENDING") pending.set(app, review);
  }
  return { byId, pending: pending.get(viewer), bodies };
}
/** The reviews the world held on its pull request before the run: its reviews route's answer
 * (rig.sh's fixture_write), none where it routes none. */
function heldReviews(): unknown[] {
  const listed = routeFor(routes, [
    "api",
    `repos/${world?.repo}/pulls/${world?.pr}/reviews`,
  ])?.stdout;
  return Array.isArray(listed) ? listed : [];
}
/** The reviews GitHub filed the comments this run made outside a review in (gh-threads.ts's
 * OWN_REVIEW), one each: `COMMENTED` with no body, by the comment's author, at the commit it
 * stands on, submitted when it was made. */
function commentReviews(): Json[] {
  return threads.list.flatMap(commentsOf).flatMap((comment) => {
    const id = isObject(comment.pullRequestReview)
      ? Number(comment.pullRequestReview.databaseId)
      : 0;
    if (id < OWN_REVIEW) return [];
    const made = String(comment.createdAt ?? "");
    return [
      {
        id,
        user: { login: restLogin(comment.author) },
        body: "",
        state: "COMMENTED",
        commit_id: isObject(comment.commit) ? comment.commit.oid : "",
        submitted_at: githubInstant(made),
        at: made,
      },
    ];
  });
}
/** The pull request's review threads as this call's App sees them (`threads`, less what it may
 * not see): an inline comment of a review another App has pending is that review's, which GitHub
 * shows its author alone (`shown`), and a thread holding no other comment is not there for this
 * App at all. The state itself, which the score and the merge state read, keeps every comment. */
function shownThreads(): Json[] {
  const hidden = new Set(
    world === undefined
      ? []
      : [...ownReviews(world.repo).byId.values()]
          .filter((review) => !shown(review))
          .map((review) => review.id)
  );
  return threads.list.flatMap((thread) => {
    const comments = commentsOf(thread).filter(
      (comment) =>
        comment.state !== "PENDING" ||
        !isObject(comment.pullRequestReview) ||
        !hidden.has(String(comment.pullRequestReview.databaseId))
    );
    return comments.length === 0 ? [] : [{ ...thread, comments }];
  });
}
/** `review`, a review of the pull request, with the body it was updated to where the run updated
 * it (`bodies`, `ownReviews`), and as it was otherwise. */
function withReviewBody(review: unknown, bodies: ReadonlyMap<string, string>): unknown {
  const body = isObject(review) ? bodies.get(String(review.id)) : undefined;
  return body === undefined ? review : { ...(review as Record<string, unknown>), body };
}
/** The bodies the run updated the world's own reviews to (`ownReviews`), none for a run with no
 * world. */
function heldBodies(): Map<string, string> {
  return world === undefined ? new Map() : ownReviews(world.repo).bodies;
}
/** `POST …/pulls/<n>/reviews` on the world's own repository, GitHub's create of a review: the
 * state it leaves the review in and the head it creates it at, or undefined for every other
 * call. Both are the call's own, read by the rules score.ts reads the recorded create by
 * (gh-call.ts's `createdState` and `createdHead`), the head where the body names none being the
 * tip of the run's own remote at the call.
 *
 * A create GitHub refuses is refused here, 422, where that is decided: an event GitHub does not
 * take (`approve`, `PENDING`), a `REQUEST_CHANGES` or `COMMENT` with no body, and the create of a
 * pending review while this run has one pending already, which GitHub refuses as "User can only
 * have one pending review per pull request". The world's routes answered each of these 200 with
 * an id, and the last one replaced the pending review the round had been writing with a new one
 * at another head.
 *
 * The body is the call's own too, and GitHub answers the create with it (`body`, "" for a create
 * that sent none): a create answered without it left a reviewer reading its own footer back with
 * `--jq '(.body|capture(…))'` on null, and exit 1 on a review GitHub had taken
 * (round17-forty/reviewer-text-findings-defect-head-4/calls.jsonl:29). */
function reviewCreate(): { state: string; head: string | undefined; body: string } | undefined {
  if (as !== "gh" || args[0] !== "api" || !isPost(args) || route === undefined) return undefined;
  const repo = world?.repo;
  if (repo === undefined || !args.some((arg) => reviewsRoute(arg, repo))) return undefined;
  handler = "review create";
  const sent = sentBody({ argv: args, stdin, files }) ?? {};
  const state = createdState(sent);
  if (state === undefined)
    refuse(submittedState(sent.event) === undefined ? CREATE_EVENT : CREATE_BODY);
  if (state === "PENDING" && ownReviews(repo).pending !== undefined) refuse(ONE_PENDING);
  const body = typeof sent.body === "string" ? sent.body : "";
  return { state, head: createdHead(sent, branchTip()), body };
}
/** `GET …/pulls/<n>/reviews/<id>`, with the query string `gh api` lets a caller append: the
 * repository it names and the id it reads. */
const REVIEW_READ = /^\/?repos\/(.+)\/pulls\/\d+\/reviews\/(\d+)(?:\?.*)?$/;
/** GitHub's read of one review of the world's pull request, for a review this run created: the
 * review as it stands — the world's answer to its create, with the state, the head and the body
 * it has now — or undefined for every other call, which the routes answer. A world's routes
 * cannot answer it, since it turns on what the run did: the list route answered the read of
 * review 6001 with every review of the pull request, and review_world's with `[]`, so a round
 * that read its own review back with `--jq '{id,state,commit_id}'` failed on its own approval
 * (`text-only-head-3` and `defect-base-3` of review 5887659447). */
function reviewRead(): Record<string, unknown> | undefined {
  if (as !== "gh" || args[0] !== "api" || methodOf(args) !== "GET") return undefined;
  const repo = world?.repo;
  if (repo === undefined) return undefined;
  const id = args.map((arg) => REVIEW_READ.exec(arg)).find((named) => named?.[1] === repo)?.[2];
  const review = id === undefined ? undefined : ownReviews(repo).byId.get(id);
  return review === undefined || !shown(review) ? undefined : reviewObject(review);
}
/** A review this run created as GitHub answers it (`reviewJson`): the world's answer to its
 * create, with its own id and author, the state, the head and the body it has now, and the time
 * it was submitted once it is no longer pending. */
function reviewObject(review: OwnReview): Record<string, unknown> {
  const { answer, state, head, body, at: made } = review;
  return reviewJson({
    ...answer,
    id: Number(review.id),
    user: { login: review.login },
    state,
    commit_id: head,
    body,
    ...(state === "PENDING" ? {} : { submitted_at: githubInstant(made) }),
  });
}
/** The world the threads are read against, every call the record holds, and the pull request's
 * review threads as this call finds them: the world's, and the effect of each of those calls. */
const threadWorld: ThreadWorld = { routes, world };
const calls = recordedCalls(RECORD);
const threads = threadState(threadWorld, calls);
/** The login this call's token acts as: the bot of its pane's role App (gh-threads.ts's
 * `loginOf`), the GraphQL viewer it reads and the author of what it writes. */
const callerLogin = loginOf(threadWorld, role);
/** What GitHub computes of the pull request at `head` from its base's rules, as GraphQL names it
 * (gh-threads.ts): its review decision (null for none), its merge state and whether it merges,
 * with the account that made the head's last push (rest-objects.ts's `pusherOf`), whose own
 * approval a base asking the last push approved by another counts for nothing; that account is
 * read only where the base asks it. */
function pullStatus(head: string): {
  reviewDecision: string | null;
  mergeStateStatus: string;
  mergeable: string;
} {
  const pusher = () => pusherOf(`${run}`, head);
  const state = mergeStateOf(threadWorld, threads, fixtures.mergeStates, head, calls, pusher);
  return {
    reviewDecision: reviewDecisionOf(threadWorld, calls, pusher),
    mergeStateStatus: state,
    mergeable: mergeableOf(state),
  };
}
/** What GitHub answered this call where it writes a thread, applied to `threads`: a refusal here
 * is GitHub's, and the REST comment it made, or the payloads of its mutation, its answer. */
const threaded = threadCall(threadWorld, threads, {
  at,
  as,
  argv: args,
  files,
  stdin,
  route: route?.match ?? null,
});
if (threaded !== undefined && !("fields" in threaded)) handler = "thread";
if (threaded !== undefined && "refused" in threaded)
  refuse(threaded.refused, threaded.documentation);
const submitted = reviewSubmission();
if (submitted !== undefined)
  answered = {
    state: String(submitted.state),
    head: String(submitted.commit_id),
    body: String(submitted.body),
  };
const created = reviewCreate();
const read = reviewRead();
/** This call's answer where it is this stand-in's own and no route's: the submit of the pending
 * review, the read of a review this run created, and a REST comment the run made on a thread.
 * Each stands over whatever route the call matched. */
const own =
  submitted ??
  read ??
  (threaded !== undefined && "rest" in threaded
    ? reviewCommentJson(threaded.rest.thread, threaded.rest.comment, "pull")
    : undefined);
/** A check run as GitHub's check-runs route reports it on the commit it ran on: the world's run —
 * its name, status, conclusion, the link of the app that ran it (`details_url`), how long it ran,
 * the title and summary its App wrote, if any, and that App (`app`, by id) — with the ids GitHub
 * gives it, the commit it ran on (`head_sha`), GitHub's own page of it (`html_url`), the times it
 * ran, counted from when that commit reached the run's remote, and its App whole
 * (rest-objects.ts's `checkRunApp`). */
interface ReportedRun extends CheckRun {
  readonly id: number;
  readonly head_sha: string;
  readonly details_url: string;
  readonly [field: string]: unknown;
}
/** How long GitHub takes from a push to the start of the first check run on it, which this world
 * fixes so every head reads alike. */
const QUEUED_MS = 3000;
/** When the commit `sha` reached the run's remote, in milliseconds: the first push of it the
 * remote's post-receive hook logged (`<run>/pushes.log`, `<at> <ref> <old> <new>`, which holds
 * the run's own pushes), or, for a commit the world was built with, its committer date. */
function reachedAt(sha: string): number | undefined {
  const log = existsSync(`${run}/pushes.log`) ? readFileSync(`${run}/pushes.log`, "utf8") : "";
  const pushed = log
    .split("\n")
    .map((line) => line.split(" "))
    .find((words) => words[3] === sha)?.[0];
  const parsed = Date.parse(pushed ?? fromRemote("show", "-s", "--format=%cI", sha) ?? "");
  return Number.isNaN(parsed) ? undefined : parsed;
}
/** The world's check runs `runs`, reported on the commit `sha` as GitHub reports a check run
 * (`ReportedRun`). The world's times say how long each ran and in what order, and GitHub's are
 * those runs moved to start once the commit reached the remote and the queue let them: a world
 * whose every head reported checks run on 2026-01-01, ten months before the commit they tested,
 * left a reviewer asking whether this world's CI ran at all (INFRA-764 round 24,
 * reviewer-text-findings-text-only-base-6). The ids are the commit's own, so a head reads the same
 * runs at every read. A workflow's job writes no output, and a code-scanning tool's check carries
 * the title and summary GitHub Advanced Security wrote (production's `CodeQL`, measured on
 * 2026-11-18). */
function reportedRuns(runs: readonly CheckRun[], sha: string): ReportedRun[] {
  const repo = world?.repo ?? "";
  const reached = reachedAt(sha);
  const starts = runs
    .map((one) => Date.parse(one.started_at ?? ""))
    .filter((time) => !Number.isNaN(time));
  const origin = starts.length === 0 ? undefined : Math.min(...starts);
  const moved = (time: string | undefined) => {
    const parsed = Date.parse(time ?? "");
    if (reached === undefined || origin === undefined || Number.isNaN(parsed)) return time;
    return new Date(reached + QUEUED_MS + parsed - origin).toISOString().replace(/\.\d{3}Z$/, "Z");
  };
  const seed = Number.parseInt(sha.slice(0, 8), 16) || 0;
  // The pull requests GitHub reports a check run for: the open one whose head the commit is.
  const pulls = world !== undefined && sha === branchTip() ? [minimalPull(sha)] : [];
  return runs.map((one, index) => {
    const id = 40_000_000_000 + seed * 8 + index;
    const page = `https://github.com/${repo}/runs/${id}`;
    const api = `https://api.github.com/repos/${repo}/check-runs/${id}`;
    return {
      ...one,
      id,
      node_id: `CR_kwDOAAAAAc8AAA${id}`,
      head_sha: sha,
      external_id: "",
      url: api,
      html_url: page,
      details_url:
        "details_url" in one && typeof one.details_url === "string"
          ? one.details_url
          : (one.html_url ?? page),
      started_at: moved(one.started_at),
      completed_at: moved(one.completed_at),
      output: {
        title: isObject(one.output) ? (one.output.title ?? null) : null,
        summary: isObject(one.output) ? (one.output.summary ?? null) : null,
        text: null,
        annotations_count: 0,
        annotations_url: `${api}/annotations`,
      },
      check_suite: { id: 30_000_000_000 + seed },
      app: checkRunApp(one),
      pull_requests: pulls,
    };
  });
}
/** The world's repository as the route of `gh api repos/<repo>` holds it (rig.sh's fixture_write:
 * its settings, which rest-objects.ts's `repositoryJson` answers it from), or an empty object. */
function heldRepository(): Json {
  const held = routeFor(routes, ["api", `repos/${world?.repo ?? ""}`])?.stdout;
  return isObject(held) ? held : {};
}
/** The pull request at `head` as GitHub names it inside a check run (`pull-request-minimal`): its
 * number, its id, and its two branches, each with its tip and the repository it is in. */
function minimalPull(head: string): Json {
  const repository = heldRepository();
  const repo = world?.repo ?? "";
  const api = `https://api.github.com/repos/${repo}`;
  const side = (ref: string, sha: string) => ({
    ref,
    sha,
    repo: { id: repository.id, url: api, name: repository.name },
  });
  return {
    url: `${api}/pulls/${world?.pr}`,
    id: 2_000_000_000 + (world?.pr ?? 0),
    number: world?.pr,
    head: side(world?.branch ?? "", head),
    base: side(world?.base ?? "", fromRemote("rev-parse", `refs/heads/${world?.base}`) ?? ""),
  };
}
/** The check runs the world reports on the pull request's head as it stands, read at the call
 * from the run's own remote (`branchTip`) and the world's check-runs answer for that head
 * (`checkRunsOn`), reported as GitHub reports them (`reportedRuns`), or undefined for a run with
 * no world or no remote of its own. Every answer that reports the head's checks is built from
 * these, so a world that has a check still running on one head and none on the next — the push
 * that set an approval aside, then the head its checks concluded on — tells `gh pr checks`, the
 * pull request's own `statusCheckRollup` and the check-runs route one thing. */
function headRuns(): ReportedRun[] | undefined {
  const repo = world?.repo;
  const tip = branchTip();
  return repo === undefined || tip === undefined
    ? undefined
    : reportedRuns(checkRunsOn(routes, repo, tip), tip);
}
/** How gh writes the time a check ran (its `addRow`: the Go `time.Duration` from start to end,
 * which for GitHub's times, whole seconds, is `8s`, `10m52s`, `1h0m5s`), and `0` for one with no
 * start or end or that ran no time. */
function elapsed(started: string | undefined, completed: string | undefined): string {
  const seconds =
    started === undefined || completed === undefined
      ? 0
      : Math.round((Date.parse(completed) - Date.parse(started)) / 1000);
  if (!(seconds > 0)) return "0";
  const [hours, minutes] = [Math.floor(seconds / 3600), Math.floor(seconds / 60) % 60];
  if (hours > 0) return `${hours}h${minutes}m${seconds % 60}s`;
  return minutes > 0 ? `${minutes}m${seconds % 60}s` : `${seconds}s`;
}
/** gh's bucket for a check's state, its conclusion once it completed and its status before, in
 * GitHub's GraphQL spelling (gh 2.98.0's pkg/cmd/pr/checks/aggregate.go, `aggregateChecks`): a
 * success passes; a skip and a neutral conclusion skip; an error, a failure, a time-out and an
 * action required fail; a cancel is a bucket of its own; and every other state — `STALE`,
 * `STARTUP_FAILURE`, and each status of a check not yet completed — is pending. The world read a
 * neutral conclusion as a pass and every conclusion it did not name as a failure. */
const CHECK_BUCKETS: Readonly<Record<string, string>> = {
  SUCCESS: "pass",
  SKIPPED: "skipping",
  NEUTRAL: "skipping",
  ERROR: "fail",
  FAILURE: "fail",
  TIMED_OUT: "fail",
  ACTION_REQUIRED: "fail",
  CANCELLED: "cancel",
};
/** `gh pr checks`, as gh 2.98.0 prints it to a pipe (pkg/cmd/pr/checks's checks.go and
 * output.go): one line per check, five fields a tab apart — its name, its bucket
 * (`CHECK_BUCKETS`, a cancel printed `fail`), the time it ran (`elapsed`), its link and its
 * description, which gh reads of a status and never of a check run, so the line ends in a tab —
 * failed checks first and then by name, as gh sorts them (every other two buckets it sorts with a
 * comparison that orders neither, whose order this does not copy); and gh's own exit, 1 once a
 * check failed, else 8 while one is pending, else 0, a cancel counting as neither. A head that
 * reports no check is gh's refusal, `no checks reported on the '<branch>' branch` on stderr,
 * nothing on stdout and exit 1. Measured on 2026-11-18 (prod-reads-19.json, item 25): #4326's
 * checks printed `changes\tpass\t8s\t<link>\t`, `dispatch-two-builds / …\tpass\t10m52s\t<link>\t`
 * and each skipped job `skipping\t0`, exit 0, and #2827, whose head reported none, the refusal
 * naming its branch; the world printed `<seconds>s` and no description, and for a head with none
 * nothing and exit 0. Undefined for any other call, and for a run with no world. */
function prChecks(): { text: string; status: number } | undefined {
  if (as !== "gh" || args[0] !== "pr" || args[1] !== "checks") return undefined;
  const runs = headRuns();
  if (runs === undefined) return undefined;
  if (runs.length === 0) {
    handler = "pr checks";
    refuseWith(undefined, `no checks reported on the '${world?.branch ?? ""}' branch\n`, 1);
  }
  const lines = runs.map((run) => {
    const state = (run.status === "completed" ? (run.conclusion ?? "") : run.status).toUpperCase();
    const bucket = CHECK_BUCKETS[state] ?? "pending";
    const time = elapsed(run.started_at, run.completed_at);
    const shown = bucket === "cancel" ? "fail" : bucket;
    return { bucket, run, line: `${run.name}\t${shown}\t${time}\t${run.details_url}\t` };
  });
  const order = (one: string, other: string) => (one < other ? -1 : one > other ? 1 : 0);
  lines.sort(
    (one, other) =>
      Number(other.bucket === "fail") - Number(one.bucket === "fail") ||
      order(one.run.name, other.run.name) ||
      order(one.run.details_url, other.run.details_url)
  );
  const buckets = lines.map(({ bucket }) => bucket);
  return {
    text: lines.map(({ line }) => line).join("\n"),
    status: buckets.includes("fail") ? 1 : buckets.includes("pending") ? 8 : 0,
  };
}
const checked = prChecks();
/** The flags of `gh pr view`, `gh pr diff`, `gh pr list` and `gh pr review` that take a value, as
 * gh 2.98.0's `--help` of each names them; every other flag of the four takes none. */
const PR_VALUE_FLAGS: Readonly<Record<string, readonly string[]>> = {
  view: ["-R", "--repo", "--json", "-q", "--jq", "-t", "--template"],
  diff: ["-R", "--repo", "--color", "-e", "--exclude"],
  list: [
    ...["-R", "--repo", "--json", "-q", "--jq", "-t", "--template", "--app", "-a", "--assignee"],
    ...["-A", "--author", "-B", "--base", "-H", "--head", "-l", "--label", "-L", "--limit"],
    ...["-S", "--search", "-s", "--state"],
  ],
  review: ["-R", "--repo", "-b", "--body", "-F", "--body-file"],
};
/** A pull request's web page, which gh takes as the pull request it names, and its repository. */
const PULL_URL = /^https?:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)(?:[/?#].*)?$/i;
/** gh's refusal of a call naming a repository GitHub has not got, as it prints GitHub's error. */
const repositoryRefusal = (repo: string) =>
  `GraphQL: Could not resolve to a Repository with the name '${repo}'. (repository)\n`;
/** gh's refusal of a `gh pr view` or `gh pr diff` that names no pull request of the world, or of
 * any of the three naming another repository than the world's, or undefined for a call naming the
 * world's own: by its number (`#<number>` too), its URL or its branch (`<owner>:<branch>` too), or
 * by nothing, which gh reads as the pull request of the checkout's branch, the world's. Measured
 * on 2026-11-17 against cybermonklabs/legion, each on stderr with exit 1 and nothing on stdout:
 * `gh pr view 999999` and `gh pr diff 999999` (and `gh pr view <its URL>`) printed `GraphQL: Could
 * not resolve to a PullRequest with the number of 999999. (repository.pullRequest)`, `gh pr view
 * nothing-here` printed `no pull requests found for branch "nothing-here"`, and `--repo
 * <owner>/<no such repository>` printed `GraphQL: Could not resolve to a Repository with the name
 * '<owner>/<no such repository>'. (repository)`. The world's routes answered every one of these
 * with the world's pull request. `gh pr review` finds the pull request it names by the same
 * finder (pkg/cmd/pr/shared's `PRFinder`), so it is refused the same way (`reviewPullRequest`). */
function pullRefusal(command: string, at: NonNullable<typeof world>): string | undefined {
  const flags = PR_VALUE_FLAGS[command] ?? [];
  const named: string[] = [];
  for (let index = 2; index < args.length; index++) {
    const arg = args[index] ?? "";
    if (valueAt(args, index, flags) !== undefined) {
      if (flags.includes(arg)) index++;
    } else if (!arg.startsWith("-")) named.push(arg);
  }
  const [selector] = named;
  const url = selector === undefined ? null : PULL_URL.exec(selector);
  const repo = url?.[1] ?? onGithub(flagValue(args, REPO_FLAGS) ?? process.env.GH_REPO ?? at.repo);
  if (repo.toLowerCase() !== at.repo.toLowerCase()) return repositoryRefusal(repo);
  if (selector === undefined) return undefined;
  const number = url?.[2] ?? /^#?(\d+)$/.exec(selector)?.[1];
  if (number === undefined)
    return selector === at.branch || selector === `${at.repo.split("/")[0]}:${at.branch}`
      ? undefined
      : `no pull requests found for branch "${selector}"\n`;
  return Number(number) === at.pr
    ? undefined
    : `GraphQL: Could not resolve to a PullRequest with the number of ${Number(number)}. (repository.pullRequest)\n`;
}
/** The pull request as GitHub's REST API lists one, at the head the run's remote is at
 * (rest-objects.ts's `pullRequestJson`): what `gh pr view`'s author, times, ids and test merge are
 * read from, so they are the ones every REST answer of the world gives. */
const pullRest = once(() => {
  const held = heldPullRequest(routes);
  return pullRequestJson(
    held,
    pullFacts(branchTip() ?? String(held.headRefOid ?? world?.head ?? "")),
    "simple"
  );
});
/** The account that opened the pull request, as GitHub's REST API names it. */
const pullAuthor = () => (isObject(pullRest().user) ? (pullRest().user as Json) : {});
/** The reviews of the pull request as GitHub's GraphQL holds them (gh-view.ts's `reviewNode`):
 * every review its REST list holds (`restReviews`), in that order. */
const reviewNodes = once(() => restReviews(heldReviews()).map(reviewNode));
/** The conversation's comments as GitHub's GraphQL holds them (gh-view.ts's `commentNode`), read
 * by the login the call's token acts as (`callerLogin`): every comment its REST list holds
 * (`restIssueComments`). */
const commentNodes = once(() =>
  restIssueComments(heldIssueComments()).map((comment) => commentNode(comment, callerLogin))
);
/** The latest review of each reviewer (gh-view.ts's `latestReviews`). */
const latestNodes = () =>
  latestReviews(reviewNodes(), graphqlLogin(String(pullAuthor().login ?? "")));
/** The files the pull request's diff changes (`branchDiff`), as GitHub's GraphQL lists them
 * (`PullRequestChangedFile`): each one's path, the lines it adds and removes, and how it changes,
 * read with the renames git finds, as the diff `gh pr diff` prints is. A binary file's counts are
 * `-` in git's and 0 in GitHub's. Production's #2827 listed 170 such files, `ADDED`, `DELETED` and
 * `MODIFIED` among them (production-shapes.json's `gh pr view files`); the world's routes answered
 * none, and `--jq '.files[].path'` failed on null (INFRA-764 round 25, runs/2b44667b6a7a,
 * calls.jsonl:10). */
const prFiles = once(() => {
  const counts = (branchDiff("--numstat", "-z") ?? "").split("\0");
  const letters = (branchDiff("--name-status", "-z") ?? "").split("\0");
  const change: Readonly<Record<string, string>> = {
    A: "ADDED",
    C: "COPIED",
    D: "DELETED",
    M: "MODIFIED",
    R: "RENAMED",
    T: "CHANGED",
  };
  const kinds = new Map<string, string>();
  for (let at = 0; at < letters.length; ) {
    const letter = (letters[at] ?? "").slice(0, 1);
    // A rename or a copy names the path it came from, then the one it is at.
    const moved = letter === "R" || letter === "C";
    const file = letters[at + (moved ? 2 : 1)] ?? "";
    if (file !== "") kinds.set(file, change[letter] ?? "MODIFIED");
    at += moved ? 3 : 2;
  }
  const files: Json[] = [];
  for (let at = 0; at < counts.length; ) {
    const [added = "", deleted = "", named = ""] = (counts[at] ?? "").split("\t");
    // numstat names a moved file's two paths in the fields after its counts.
    const path = named === "" ? (counts[at + 2] ?? "") : named;
    at += named === "" ? 3 : 1;
    if (path === "") continue;
    files.push({
      path,
      additions: Number(added) || 0,
      deletions: Number(deleted) || 0,
      changeType: kinds.get(path) ?? "MODIFIED",
    });
  }
  return files;
});
/** The commits of the pull request, oldest first, as gh reads them (`gh pr view --json commits`):
 * each one's SHA, its message's first line and the rest, when it was authored and committed, and
 * its author, linked to the account its address is, as GitHub links one (an id and a login of
 * none where the world holds no such account). Measured on 2026-11-17 against #2827: each commit
 * carried `authoredDate`, `authors` (each `email`, `id`, `login` and `name`), `committedDate`,
 * `messageBody`, `messageHeadline` and `oid`, and a role App's bot is linked by its `[bot]` login
 * there. */
const prCommits = once(() => {
  if (world === undefined) return [];
  const format = ["%H", "%an", "%ae", "%at", "%ct", "%B"].join("%x00");
  const log = fromRemote(
    "log",
    "--reverse",
    `--format=${format}%x1e`,
    `refs/heads/${world.base}..refs/heads/${world.branch}`
  );
  return (log ?? "")
    .split("\x1e")
    .map((record) => record.replace(/^\n/, ""))
    .filter((record) => record !== "")
    .map((record) => {
      const [oid = "", name = "", email = "", authored = "", committed = "", message = ""] =
        record.split("\0");
      const [headline = "", ...rest] = message.split("\n");
      const account = accounts.find((one) => one.email === email);
      return {
        authoredDate: githubTime(Number(authored)),
        authors: [
          {
            email,
            id: account === undefined ? "" : userJson(account.login, accounts).node_id,
            login: account?.login ?? "",
            name,
          },
        ],
        committedDate: githubTime(Number(committed)),
        messageBody: rest.join("\n").trim(),
        messageHeadline: headline,
        oid,
      };
    });
});
/** The world's pull request as gh reads it (`gh pr view`, `gh pr list`): each of `fields` of gh's
 * own (gh-view.ts's PR_FIELDS) as it stands when the call is made, and only those, since most are
 * reads of the run's record or remote. The world's facts are the route's (rig.sh's fixture_write);
 * what the run changes is read from what it did: the head and its checks and merge state
 * (`branchTip`, `headRuns`, gh-threads.ts's `mergeStateOf`), the reviews and the conversation, the
 * files and commits of its diff, and every id, time and account the REST pull request names
 * (`pullRest`). The world's pull request has no label, assignee, milestone, project, reaction,
 * review request or issue it closes, and was opened from a branch of the repository itself, so no
 * maintainer edits it from a fork; GitHub's classic projects are gone and gh reads `projectCards`
 * as null (measured on 2026-11-17 against #2827 and #4326). */
function prView(fields: readonly string[]): Json {
  const held = heldPullRequest(routes);
  const tip = once(branchTip);
  const rest = pullRest;
  const status = once(() => pullStatus(tip() ?? String(held.headRefOid ?? "")));
  const repository = () => {
    const head = rest().head;
    return isObject(head) && isObject(head.repo) ? head.repo : {};
  };
  const sum = (key: "additions" | "deletions") =>
    prFiles().reduce((total, file) => total + Number(file[key]), 0);
  const value = (field: string): unknown => {
    switch (field) {
      case "headRefOid":
        return tip() ?? held.headRefOid;
      case "baseRefOid":
        return isObject(rest().base) ? (rest().base as Json).sha : null;
      case "mergeStateStatus":
      case "mergeable":
        return status()[field];
      // gh reads a decision GitHub answers null for as "" (rig.sh's tester-proof world held "").
      case "reviewDecision":
        return status().reviewDecision ?? "";
      case "statusCheckRollup": {
        const runs = headRuns();
        return runs === undefined
          ? (held.statusCheckRollup ?? [])
          : runs.map((run) => ({
              __typename: "CheckRun",
              name: run.name,
              status: run.status.toUpperCase(),
              conclusion: (run.conclusion ?? "").toUpperCase(),
              startedAt: run.started_at ?? null,
              completedAt: run.completed_at ?? null,
              detailsUrl: run.details_url,
              workflowName: "",
            }));
      }
      case "reviews":
        return reviewNodes();
      // gh asks GitHub for a latest review's author, association, body, state and time alone, and
      // reads its id and commit as "": measured on 2026-11-17 against #4326, `latestReviews`
      // held `"id":""` and `"commit":{"oid":""}` beside the review `reviews` names fully.
      case "latestReviews":
        return latestNodes().map((review) => ({ ...review, id: "", commit: { oid: "" } }));
      case "comments":
        return commentNodes().map((comment) => ({
          ...comment,
          minimizedReason: comment.minimizedReason ?? "",
        }));
      case "files":
        return prFiles();
      case "additions":
      case "deletions":
        return sum(field);
      case "changedFiles":
        return prFiles().length;
      case "commits":
        return prCommits();
      case "author":
        return authorOf(pullAuthor());
      case "createdAt":
        return rest().created_at;
      case "updatedAt":
        return rest().updated_at;
      case "closed":
        return held.state !== "OPEN";
      case "closedAt":
        return rest().closed_at;
      case "fullDatabaseId":
        return String(rest().id);
      case "headRepository":
        return {
          id: repository().node_id,
          name: repository().name,
          nameWithOwner: repository().full_name,
        };
      case "headRepositoryOwner": {
        const owner = isObject(repository().owner) ? (repository().owner as Json) : {};
        return { id: owner.node_id, login: owner.login };
      }
      case "potentialMergeCommit": {
        const sha = rest().merge_commit_sha;
        return typeof sha === "string" ? { oid: sha } : null;
      }
      case "maintainerCanModify":
        return false;
      case "mergeCommit":
      case "mergedBy":
      case "milestone":
      case "projectCards":
        return null;
      case "assignees":
      case "closingIssuesReferences":
      case "labels":
      case "projectItems":
      case "reactionGroups":
      case "reviewRequests":
        return [];
      default:
        return held[field] ?? null;
    }
  };
  return Object.fromEntries([...new Set(fields)].map((field) => [field, value(field)]));
}
/** The pull request fields gh's text view of one reads (gh-view.ts's `preview`), and of each one
 * `gh pr list` lists (`listLine`). */
const PREVIEW_FIELDS: readonly PrField[] = [
  "title",
  "state",
  "isDraft",
  "labels",
  "assignees",
  "reviews",
  "reviewRequests",
  "projectCards",
  "projectItems",
  "milestone",
  "number",
  "url",
  "additions",
  "deletions",
  "autoMergeRequest",
  "body",
];
const LIST_FIELDS: readonly PrField[] = [
  "number",
  "title",
  "headRefName",
  "state",
  "isDraft",
  "createdAt",
];
/** Whether `gh pr list`'s filters list the world's one pull request, or undefined for a filter
 * this world does not read (`--search`, and a `--state` gh takes none of). Each filter is gh
 * 2.98.0's: `--state` (open by default, or closed, merged or all), `--head` and `--base` the
 * branch names, `--label`, `--assignee` and `--draft`, none of which the world's pull request
 * carries, `--author` its author — an App's bot by `app/<slug>` or `<slug>[bot]`, never its slug
 * alone — and `--app` that App's slug. Measured on 2026-11-17 against cybermonklabs/legion:
 * `--head nothing-here` listed `[]`, and nothing at all without `--json`, exit 0; `--head
 * <owner>:<branch>` listed `[]` where `--head <branch>` listed the pull request, and `--state
 * closed` did not; `--author app/legion-implementer` and `--author legion-implementer[bot]` each
 * listed its pull requests and `--author legion-implementer` none. */
function listsPull(): boolean | undefined {
  if (flagValue(args, ["-S", "--search"]) !== undefined) return undefined;
  const states: Readonly<Record<string, readonly string[]>> = {
    open: ["OPEN"],
    closed: ["CLOSED"],
    merged: ["MERGED"],
    all: ["OPEN", "CLOSED", "MERGED"],
  };
  const listed = states[flagValue(args, ["-s", "--state"]) ?? "open"];
  if (listed === undefined) return undefined;
  const held = heldPullRequest(routes);
  const head = flagValue(args, ["-H", "--head"]);
  const base = flagValue(args, ["-B", "--base"]);
  if (
    !listed.includes(String(held.state)) ||
    (head !== undefined && head !== held.headRefName) ||
    (base !== undefined && base !== held.baseRefName) ||
    flagValue(args, ["-l", "--label", "-a", "--assignee"]) !== undefined ||
    (args.some((arg) => arg === "-d" || arg === "--draft") && held.isDraft !== true)
  )
    return false;
  const author = flagValue(args, ["-A", "--author"]);
  const app = flagValue(args, ["--app"]);
  if (author === undefined && app === undefined) return true;
  const login = String(pullAuthor().login ?? "");
  const slug = graphqlLogin(login);
  const bot = slug !== login;
  return (
    (app === undefined || (bot && app === slug)) &&
    (author === undefined || author === login || (bot && author === `app/${slug}`))
  );
}
/** This call's answer where it is a `gh pr view`, `gh pr list` or `gh pr diff` of a run with a
 * world: the world's pull request as gh reads it (`prView`), as JSON of the fields `--json` names,
 * which gh refuses where it names a field gh does not take (gh-view.ts's `unknownField`), or as
 * gh's text (`preview`, `commentList`, `listLine`); for `gh pr diff`, the diff (`branchDiff`).
 * A call naming no pull request of the world, or another repository, is refused as gh refuses it
 * (`pullRefusal`), and so, as one the world does not model, is a listing by a filter it does not
 * read (`listsPull`). Undefined for every other call, and for a run with no world, whose routes
 * answer it. Before this, the routes answered `gh pr view` with the object the fixture froze —
 * every review in GitHub's REST shape, its integer `id`, `commit_id` and `user`, where gh reads
 * `commit.oid` and round 25's agents read it null; no `files`; the object itself as JSON without
 * `--json` — and a call naming any pull request with the world's. */
function prRead(): { text: string } | { json: unknown } | undefined {
  const command = as === "gh" && args[0] === "pr" ? (args[1] ?? "") : "";
  if (world === undefined || !["view", "list", "diff"].includes(command)) return undefined;
  handler = `pr ${command}`;
  const refused = pullRefusal(command, world);
  if (refused !== undefined) {
    handler = PULL_NOT_FOUND;
    refuse(refused);
  }
  if (command === "diff") {
    const diff = branchDiff(...(args.includes("--name-only") ? ["--name-only"] : []));
    return diff === undefined ? undefined : { text: diff === "" ? "" : `${diff}\n` };
  }
  const json = flagValue(args, ["--json"]);
  const fields = json?.split(",") ?? [];
  const unknown = json === undefined ? undefined : unknownField(fields);
  if (unknown !== undefined) {
    handler = FIELD_REFUSED;
    refuse(unknown);
  }
  if (command === "view") {
    if (json !== undefined) return { json: prView(fields) };
    if (args.includes("-c") || args.includes("--comments"))
      return { text: commentList(prView(["comments", "reviews"])) };
    return {
      text: preview(prView(PREVIEW_FIELDS), graphqlLogin(String(pullAuthor().login ?? ""))),
    };
  }
  const lists = listsPull();
  if (lists === undefined) {
    handler = "unrouted";
    refuse(NOT_FOUND);
  }
  const pulls = lists ? [prView(json === undefined ? LIST_FIELDS : fields)] : [];
  if (json !== undefined) return { json: pulls };
  return { text: pulls.map((pull) => `${listLine(pull)}\n`).join("") };
}
const viewed = prRead();
/** gh's usage of `gh pr review`, which it prints after its refusal of a call whose flags make no
 * one review (a FlagError): the refusal's line, a blank line and this, on stderr with nothing on
 * stdout, exit 1. Measured on 2026-11-27 with gh 2.98.0, run where it could reach no network, for
 * each refusal `reviewPullRequest` gives. */
const REVIEW_USAGE = `Usage:  gh pr review [<number> | <url> | <branch>] [flags]

Flags:
  -a, --approve           Approve pull request
  -b, --body string       Specify the body of a review
  -F, --body-file file    Read body text from file (use "-" to read from standard input)
  -c, --comment           Comment on a pull request
  -r, --request-changes   Request changes on a pull request
  
`;
/** `gh pr review` of the world's pull request (`reviewed`, the call as gh reads its flags) as gh
 * 2.98.0 makes it (pkg/cmd/pr/review/review.go) and GitHub answers it. Before it reaches GitHub,
 * gh refuses, in this order: `-R` naming a repository where the call names no pull request; a
 * body and a body file both; a change request, then a comment, with no body; no event flag and no
 * body, which a terminal would prompt for and a pipe cannot; no event flag with a body; and two
 * event flags — each with its line and its usage (REVIEW_USAGE), recorded as gh-call.ts's
 * REVIEW_FLAGS_REFUSED. It then finds the pull request the call names, refused as `gh pr view`
 * refuses one (`pullRefusal`), and sends `addPullRequestReview` with the event and the body and no
 * commit, so GitHub makes a review of the pull request at the head it is at, the tip of the run's
 * remote at the call, carrying no SHA the caller chose, by the bot of the App the call's token
 * acts as; to a pipe gh prints nothing and exits 0. The review is the call's own (`answered`), so
 * every later read of the record reads it: the reviews list and `gh pr view`'s reviews
 * (`ownReviews`, with the id the world's create route numbers it by, `route`), the review
 * decision (gh-threads.ts's `submittedReviews`), the daemon stand-in and the score (score.ts's
 * `reviewOf`). A review made this way while the same App has one pending is taken as the REST
 * create of a review naming an event is taken then (`reviewCreate`, which refuses only the create
 * of a second pending one): what GitHub answers for either is not measured. Before this, the
 * world refused every `gh pr review` as a write it does not model, 404, and `rig.sh health`
 * voided the run (its rule 20). */
function reviewPullRequest(call: ReviewCall, at: NonNullable<typeof world>): never {
  handler = REVIEW_FLAGS_REFUSED;
  const usage = (refusal: string): never =>
    refuseWith(undefined, `${refusal}\n\n${REVIEW_USAGE}`, 1);
  if (call.repo !== "" && call.named.length === 0)
    usage("argument required when using the --repo flag");
  if (call.bodied && call.file !== "") usage("specify only one of `--body` or `--body-file`");
  const body = call.file === "" ? call.body : (files[call.file] ?? "");
  if (call.events.has("REQUEST_CHANGES") && body === "")
    usage("body cannot be blank for request-changes review");
  if (call.events.has("COMMENT") && body === "") usage("body cannot be blank for comment review");
  if (call.events.size === 0)
    usage(
      body === ""
        ? "--approve, --request-changes, or --comment required when not running interactively"
        : "--body unsupported without --approve, --request-changes, or --comment"
    );
  if (call.events.size > 1) usage("need exactly one of --approve, --request-changes, or --comment");
  handler = "pr review";
  const refused = pullRefusal("review", at);
  if (refused !== undefined) {
    handler = PULL_NOT_FOUND;
    refuse(refused);
  }
  const [event = ""] = call.events;
  answered = { state: submittedState(event) ?? "", head: branchTip() ?? at.head, body };
  exit(0);
}
if (reviewed !== undefined && world !== undefined && route !== undefined)
  reviewPullRequest(reviewed, world);
if (
  !route &&
  pinned === undefined &&
  compared === undefined &&
  viewed === undefined &&
  own === undefined &&
  checked === undefined
) {
  handler = "unrouted";
  if (as === "gh") refuse(NOT_FOUND);
  console.error(`unknown command: ${args.join(" ")}`);
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
/** `answer` with each object of the pull request in it carrying `edited` as its body: the object
 * a route answers with is this world's pull request when it carries the head that pull request
 * is at (`headRefOid`, which the worlds write on that object and on no other). */
function withBody(answer: unknown, edited: string): unknown {
  return eachAnswer(answer, (one) =>
    "body" in one && "headRefOid" in one ? { ...one, body: edited } : one
  );
}
// A body edit is an edit of the pull request's own body, so it is kept on the pull request's own
// answers and on no others (`withBody`). Applied to every object that has a `body` at all, it
// overwrote the body of each review the reviews route answers, so after the implementer's body
// edit the approval already on the pull request read back as the pull request's own text.
if (body !== undefined) {
  const edited = routes.map((candidate) => {
    const stdout = withBody(candidate.stdout, body);
    return stdout === candidate.stdout ? candidate : { ...candidate, stdout };
  });
  writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
  kept = body;
}
// The comparison, where the call asked for one, is the answer; an answer this stand-in gives of
// its own — a submit it took, the review itself, and the read of a review this run created —
// stands over whatever route matched it, while every other submit was refused where that was
// decided (`refuse`); every other call is its route's. The call that edited the body is
// answered with the body it left, since the route was read before the edit: GitHub answers a
// PATCH of the pull request with the pull request as that PATCH left it, and the world's route
// for it answered the body before the edit, where the route every write fell to had answered
// `{id, html_url}` and 23 recorded PATCH calls read `.body` back as null.
/** The comments this run posted on the pull request's conversation, oldest first (gh-threads.ts's
 * `conversationOf`, which the HTTP stand-in counts them by too). A declaration, so the reading of
 * the pull request above it (`prRead`) can read it. */
function conversation() {
  return conversationOf(world, recordedCalls(RECORD));
}
/** The conversation comment at `index` of `conversation()`, as GitHub's REST API answers it
 * (`issue-comment`): made by the bot of the role App whose pane posted it (`by`, gh-threads.ts's
 * `loginOf`), through that App. */
function issueComment(
  comment: { body: string; at: string; by: string | undefined },
  index: number
) {
  const id = 8001 + index;
  const url = String(heldPullRequest(routes).url ?? "");
  const api = `https://api.github.com/repos/${world?.repo}/issues`;
  const login = loginOf(threadWorld, comment.by);
  return {
    url: `${api}/comments/${id}`,
    html_url: `${url}#issuecomment-${id}`,
    issue_url: `${api}/${world?.pr}`,
    id,
    node_id: `IC_kwDOAAAAAc4AAB${id}`,
    user: userJson(login, accounts),
    created_at: githubInstant(comment.at),
    updated_at: githubInstant(comment.at),
    body: comment.body,
    author_association: "NONE",
    reactions: reactionsJson(`${api}/comments/${id}`),
    performed_via_github_app: appJson(login, world?.repo.split("/")[0] ?? "", accounts),
    minimized: null,
  };
}
/** Whether the call asks for a file's own bytes rather than the JSON that carries them: `gh api -H
 * "Accept: application/vnd.github.raw"`, GitHub's raw media type, in its versioned spelling
 * `vnd.github.v3.raw` and as `vnd.github.raw+json` too. */
const RAW_MEDIA_TYPE = /application\/vnd\.github(?:\.v3)?\.raw/;
/** Whether this call's answer is a file's own bytes, which gh prints as GitHub sent them: no
 * newline is added to a file that ends without one. */
let verbatim = false;
/** The list inside this call's answer that GitHub pages by `per_page` and `page`, with the size
 * of its page where the call names none, where the answer's own read names one (`commitOf`: a
 * commit's `files`, 300 a page); undefined for every other answer, whose list PAGED names. */
let pagedList: { key: string; size: number } | undefined;
/** GitHub's `GET repos/<o>/<r>/contents/<path>`, read on the run's own remote at the call: the
 * ref the query string names (`?ref=`, or a `-f ref=` gh sends there on a GET), the base branch
 * where it names none, resolved to a commit, and the path at that commit. A file is its own
 * bytes under the raw media type (RAW_MEDIA_TYPE) and otherwise GitHub's JSON of it, its content
 * base64 in lines of 60 as GitHub writes it, its `sha` the blob's; a directory is the JSON list of
 * what it holds. A ref the remote cannot resolve is GitHub's 404 "No commit found for the ref", and
 * a path the commit does not hold is GitHub's 404. No route freezes a file: the head a reviewer
 * reads a fixed file at is the implementer's push, and measured in INFRA-764 round 24 the
 * reviewer of threads-closed-text-fixes-head-3 read `gh api
 * repos/example/widgets/contents/greet.ts?ref=<head> -H 'Accept: application/vnd.github.raw'`,
 * was refused 404 by a world that answered no contents read, and wrote that the 404s were the
 * sandbox's; the run was void and replaced, 2 of the 7 launches of that arm. */
function contentsAt(endpoint: string): unknown {
  const repo = world?.repo ?? "";
  let file = "";
  try {
    file = (/^repos\/[^/]+\/[^/]+\/contents(?:\/(.*))?$/.exec(endpoint)?.[1] ?? "")
      .split("/")
      .filter((segment) => segment !== "")
      .map((segment) => decodeURIComponent(segment))
      .join("/");
  } catch {
    refuse(NOT_FOUND, CONTENTS_DOCS);
  }
  const query = new URLSearchParams((endpointOf(args) ?? "").split("?")[1] ?? "");
  const fields = sentBody({ argv: args, stdin, files }) ?? {};
  const ref =
    query.get("ref") ?? (typeof fields.ref === "string" ? fields.ref : (world?.base ?? ""));
  const sha = REF.test(ref)
    ? fromRemote("rev-parse", "--verify", "--quiet", `${ref}^{commit}`)
    : undefined;
  if (sha === undefined || sha === "")
    refuse(`gh: No commit found for the ref ${ref} (HTTP 404)\n`);
  const object = file === "" ? `${sha}^{tree}` : `${sha}:${file}`;
  const kind = fromRemote("cat-file", "-t", object);
  const entry = (path: string, type: string, id: string, size: number) => {
    const encoded = path.split("/").map(encodeURIComponent).join("/");
    const self = `https://api.github.com/repos/${repo}/contents/${encoded}?ref=${ref}`;
    const html = `https://github.com/${repo}/${type === "dir" ? "tree" : "blob"}/${ref}/${encoded}`;
    const git = `https://api.github.com/repos/${repo}/git/${type === "dir" ? "trees" : "blobs"}/${id}`;
    return {
      name: path.split("/").pop() ?? path,
      path,
      sha: id,
      size,
      url: self,
      html_url: html,
      git_url: git,
      download_url:
        type === "file" ? `https://raw.githubusercontent.com/${repo}/${ref}/${encoded}` : null,
      type,
      _links: { self, git, html },
    };
  };
  if (kind === "blob") {
    const bytes = remoteBytes("cat-file", "blob", object) ?? Buffer.alloc(0);
    if (RAW_MEDIA_TYPE.test(joined)) {
      verbatim = true;
      return bytes.toString();
    }
    const { _links, ...described } = entry(
      file,
      "file",
      fromRemote("rev-parse", object) ?? "",
      bytes.length
    );
    const lines = bytes.toString("base64").match(/.{1,60}/g) ?? [];
    return {
      ...described,
      content: lines.map((line) => `${line}\n`).join(""),
      encoding: "base64",
      _links,
    };
  }
  if (kind !== "tree") refuse(NOT_FOUND, CONTENTS_DOCS);
  return (fromRemote("ls-tree", "-l", "-z", object) ?? "").split("\0").flatMap((line) => {
    const [, mode = "", type = "", id = "", size = "", name = ""] =
      /^(\d+) (\w+) ([0-9a-f]+) +(\S+)\t(.*)$/s.exec(line) ?? [];
    if (name === "") return [];
    const listed =
      type === "tree"
        ? "dir"
        : type === "commit"
          ? "submodule"
          : mode === "120000"
            ? "symlink"
            : "file";
    return [entry(file === "" ? name : `${file}/${name}`, listed, id, Number(size) || 0)];
  });
}
/** The update of a review's body and the create of a conversation comment, each with no body,
 * which GitHub answers 422; each sentence is this stand-in's own, as its other 422 sentences are,
 * since GitHub's exact wording is not measured here. */
const REVIEW_BODY = "gh: Unprocessable Entity (HTTP 422)\nA review is updated with a body.\n";
const CONVERSATION_BODY =
  "gh: Unprocessable Entity (HTTP 422)\nA comment on the conversation is created with a body.\n";
/** GitHub's verification of the commit `sha` of the run's own remote, committed at `committed`:
 * for a commit that carries no signature, `unsigned` and nothing else; for one that does, the
 * signature and the payload it signs (the commit without its `gpgsig` header), verified where a
 * key the world's accounts sign with ($run/allowed-signers, rig.sh's fixture_first_commit)
 * verifies it, as git verifies it (`git verify-commit`), at the time the commit reached the
 * remote, its committer's date. */
function verificationOf(sha: string, committed: string): Record<string, unknown> {
  const [header = "", ...body] = (fromRemote("cat-file", "commit", sha) ?? "").split("\n\n");
  const lines = header.split("\n");
  const start = lines.findIndex((line) => line.startsWith("gpgsig "));
  if (start < 0)
    return {
      verified: false,
      reason: "unsigned",
      signature: null,
      payload: null,
      verified_at: null,
    };
  let end = start + 1;
  while (lines[end]?.startsWith(" ")) end++;
  // The header's value, its continuation lines each without the space that folds it.
  const signature = [
    (lines[start] ?? "").slice("gpgsig ".length),
    ...lines.slice(start + 1, end).map((line) => line.slice(1)),
  ].join("\n");
  const signers = `${run}/allowed-signers`;
  const verified =
    existsSync(signers) &&
    Bun.spawnSync([
      "git",
      "-C",
      `${run}/remote.git`,
      "-c",
      `gpg.ssh.allowedSignersFile=${signers}`,
      "verify-commit",
      sha,
    ]).exitCode === 0;
  return {
    verified,
    reason: verified ? "valid" : "unknown_key",
    signature: `${signature}\n`,
    payload: `${[...lines.slice(0, start), ...lines.slice(end)].join("\n")}\n\n${body.join("\n\n")}\n`,
    verified_at: verified ? committed : null,
  };
}
/** The commit `sha` of the run's own remote as GitHub's REST API answers one inside another
 * answer (a branch's `commit`, a comparison's commits): its author and committer as git holds
 * them, its message, tree and parents, the links GitHub gives each, its verification
 * (`verificationOf`), and the accounts GitHub links its author's and its committer's addresses to
 * (rest-objects.ts's `committerJson`): the world's base holds its developer's commits and its
 * issue branch its role Apps' bots', each an account the world holds. */
function commitJson(sha: string): Record<string, unknown> {
  const repo = world?.repo ?? "";
  const api = `https://api.github.com/repos/${repo}`;
  const html = `https://github.com/${repo}`;
  const format = ["%an", "%ae", "%at", "%cn", "%ce", "%ct", "%T", "%P", "%B"].join("%x00");
  const [
    author = "",
    email = "",
    authored = "",
    committer = "",
    address = "",
    committed = "",
    tree = "",
    parents = "",
    message = "",
  ] = (fromRemote("show", "-s", `--format=${format}`, sha) ?? "").split("\0");
  const date = (seconds: string) => githubTime(Number(seconds));
  return {
    sha,
    node_id: `C_kwDOAAAAAdoAKD${sha}`,
    commit: {
      author: { name: author, email, date: date(authored) },
      committer: { name: committer, email: address, date: date(committed) },
      message,
      tree: { sha: tree, url: `${api}/git/trees/${tree}` },
      url: `${api}/git/commits/${sha}`,
      comment_count: 0,
      verification: verificationOf(sha, date(committed)),
    },
    url: `${api}/commits/${sha}`,
    html_url: `${html}/commit/${sha}`,
    comments_url: `${api}/commits/${sha}/comments`,
    author: committerJson(email, accounts),
    committer: committerJson(address, accounts),
    parents: parents
      .split(" ")
      .filter((parent) => parent !== "")
      .map((parent) => ({
        sha: parent,
        url: `${api}/commits/${parent}`,
        html_url: `${html}/commit/${parent}`,
      })),
  };
}
/** The commit `ref` names on the run's own remote, as GitHub reads the ref of a commit: a SHA,
 * full or short, a branch, a tag or any other revision git reads; undefined where it names none
 * (and for a ref REF refuses). */
function commitAt(ref: string): string | undefined {
  if (!REF.test(ref)) return undefined;
  return fromRemote("rev-parse", "--verify", "--quiet", `${ref}^{commit}`) || undefined;
}
/** `text`, a part of the call's path, as GitHub reads it, each escape decoded (`%2F` a slash), or
 * undefined where an escape decodes to nothing. */
function decoded(text: string): string | undefined {
  try {
    return decodeURIComponent(text);
  } catch {
    return undefined;
  }
}
/** The parameters this GET sends, as GitHub reads them: the query string's, then each field gh
 * sends in it on a GET (`gh api -X GET … -f sha=main`), as `contentsAt` reads its `ref`. */
function parameters(): Map<string, string> {
  const sent = new Map(new URLSearchParams((endpointOf(args) ?? "").split("?")[1] ?? ""));
  for (const [name, value] of Object.entries(sentBody({ argv: args, stdin, files }) ?? {}))
    if (!sent.has(name)) sent.set(name, String(value));
  return sent;
}
/** GitHub's read of one commit, `GET repos/<o>/<r>/commits/<ref>`: the commit (`commitJson`), its
 * `stats` and its `files`, the diff from its first parent, or from no tree for a root commit
 * (`diffFiles`), each a file's `diff-entry`. The ref is any GitHub reads (`commitAt`), a branch
 * with slashes among them and one written with `%2F`; a short SHA is answered with the full one,
 * and a ref the remote cannot resolve is GitHub's 422 (`noCommit`). Measured on 2026-11-21 with
 * `gh api -i` against cybermonklabs/legion: `commits/bbf69791` answered 200 with the full SHA,
 * `stats` (`total`, `additions`, `deletions`) and 3 files (`Date: Sat, 21 Nov 2026 08:42:52 GMT`),
 * and forty `deadbeef` digits that 422 (08:42:54 GMT); `commits/INFRA-3353/survive` and
 * `commits/INFRA-3353%2Fsurvive` each answered the branch's tip (read at 2026-11-21T08:47:33Z).
 * Its files are paged by `per_page` and `page`, 300 a page where the call names none, as the
 * description says, and its `stats` are the whole commit's on every page (`restPages`): measured
 * with `gh api -i` on 2026-11-21, `commits/bbf69791?per_page=1` answered its first file of 3
 * (`Date: Sat, 21 Nov 2026 09:10:02 GMT`), `&page=2` its second (09:10:03 GMT), `?per_page=2&page=2`
 * its third (09:10:04 GMT) and `?page=2` none (09:10:05 GMT), each with the stats of all 3.
 *
 * Under the media types the description names for the read, the answer is text, as gh prints it:
 * the diff (DIFF_MEDIA_TYPE), the patch, and the SHA alone. Measured on 2026-11-21 with `gh api -i
 * -H 'Accept: application/vnd.github.<type>' repos/cybermonklabs/legion/commits/bbf69791`: the
 * diff (`Date: Sat, 21 Nov 2026 09:08:15 GMT`) and the patch (09:08:17 GMT) each ended in the last
 * line's `\n`, the patch the bytes of `git format-patch --stdout -1 --no-signature --no-renames`
 * of that commit but for its `index` lines' SHAs, cut to 11 digits where git cut them to 10, and
 * the SHA (09:08:19 GMT) was the forty digits with no line end. */
function commitOf(ref: string): Json | string {
  const sha = commitAt(ref) ?? noCommit(ref);
  const parent = fromRemote("rev-parse", "--verify", "--quiet", `${sha}^1`);
  const root = parent ?? fromRemote("hash-object", "-t", "tree", "/dev/null") ?? "";
  if (/application\/vnd\.github(?:\.v3)?\.sha/.test(joined)) return sha;
  if (DIFF_MEDIA_TYPE.test(joined))
    return remoteBytes("diff", "--no-renames", root, sha)?.toString() ?? "";
  if (/application\/vnd\.github(?:\.v3)?\.patch/.test(joined))
    return (
      remoteBytes(
        "format-patch",
        "--stdout",
        "-1",
        "--no-signature",
        "--no-renames",
        sha
      )?.toString() ?? ""
    );
  const files = diffFiles([root, sha], sha, parent ?? sha) ?? [];
  const sum = (key: "additions" | "deletions") =>
    files.reduce((total, file) => total + Number(file[key]), 0);
  const stats = {
    total: sum("additions") + sum("deletions"),
    additions: sum("additions"),
    deletions: sum("deletions"),
  };
  pagedList = { key: "files", size: 300 };
  return { ...commitJson(sha), stats, files };
}
/** GitHub's list of the commits `GET repos/<o>/<r>/commits` reads: newest first, from the ref its
 * `sha` parameter names, the default branch where it names none, each item the commit as GitHub
 * lists it (`commitJson`), paged by `per_page` and `page` (`restPages`); a ref the remote cannot
 * resolve is GitHub's 404 of the list (COMMITS_DOCS). GitHub filters the list by `path`,
 * `author`, `committer`, `since` and `until` too, and a call naming one is refused as a call the
 * world does not model (`unrouted`). Measured on 2026-11-21 with `gh api -i` against
 * cybermonklabs/legion: `commits?sha=bbf69791&per_page=2` listed bbf69791 and its parent, each with
 * the keys of `commitJson` and no `files` (`Date: Sat, 21 Nov 2026 08:42:55 GMT`), `&page=2` the
 * two after them (08:42:56 GMT), and `commits?sha=nosuchref` answered 404 with that body (08:42:57
 * GMT). */
function commitList(): Json[] {
  const sent = parameters();
  if ([...sent.keys()].some((name) => !["sha", "per_page", "page"].includes(name))) {
    handler = "unrouted";
    refuse(NOT_FOUND);
  }
  const sha = commitAt(sent.get("sha") ?? world?.base ?? "") ?? refuse(NOT_FOUND, COMMITS_DOCS);
  return (fromRemote("rev-list", sha) ?? "")
    .split("\n")
    .filter((one) => one !== "")
    .map(commitJson);
}
/** GitHub's git-data read of one commit, `GET repos/<o>/<r>/git/commits/<sha>`: its author and
 * committer as git holds them, its tree, its message, its parents and its verification, read as
 * `commitJson` reads them, with the git-data URLs GitHub gives. GitHub reads a full SHA there
 * alone: measured on 2026-11-21 with `gh api -i` against cybermonklabs/legion,
 * `git/commits/bbf69791ebdb755876801e270cd37d6f38696984` answered 200 (`Date: Sat, 21 Nov 2026
 * 08:42:58 GMT`), and its short SHA `bbf697` (08:42:59 GMT) and `main` (08:43:00 GMT) each its 404
 * (GIT_COMMIT_DOCS), as forty `deadbeef` digits, a commit it does not hold, did (09:00:45 GMT). */
function gitCommitOf(sha: string): Json {
  if (!/^[0-9a-f]{40}$/.test(sha) || fromRemote("cat-file", "-t", sha) !== "commit")
    refuse(NOT_FOUND, GIT_COMMIT_DOCS);
  const api = `https://api.github.com/repos/${world?.repo ?? ""}`;
  const { node_id, commit, html_url, parents } = commitJson(sha);
  const { author, committer, tree, message, verification } = isObject(commit) ? commit : {};
  return {
    sha,
    node_id,
    url: `${api}/git/commits/${sha}`,
    html_url,
    author,
    committer,
    tree,
    message,
    parents: (Array.isArray(parents) ? parents : []).map((parent) => ({
      sha: isObject(parent) ? parent.sha : "",
      url: `${api}/git/commits/${isObject(parent) ? parent.sha : ""}`,
      html_url: isObject(parent) ? parent.html_url : "",
    })),
    verification,
  };
}
/** One ref as GitHub's refs API answers it (`git-ref`): its name, its node id, its URL, and the
 * object it names with that object's git-data URL. */
function refJson(ref: string, sha: string, type: string): Json {
  const api = `https://api.github.com/repos/${world?.repo ?? ""}`;
  return {
    ref,
    node_id: `REF_kwDOAAAAAc${Buffer.from(ref).toString("base64url")}`,
    url: `${api}/git/${ref}`,
    object: { sha, type, url: `${api}/git/${type}s/${sha}` },
  };
}
/** Every ref GitHub holds for the world's repository, by name, as its refs API lists them: each
 * ref of the run's own remote at the object it names (a branch at its commit, an annotated tag at
 * its tag object), and the two refs GitHub keeps for the world's pull request beside them,
 * `refs/pull/<n>/head` at the tip of its branch and `refs/pull/<n>/merge` at its test merge (the
 * pull request's `merge_commit_sha`, rest-objects.ts's `pullRequestJson`) where it has one.
 * Measured on 2026-11-21 against cybermonklabs/legion: `gh api
 * 'repos/cybermonklabs/legion/git/refs?per_page=100&page=29'` listed `refs/pull/1/head`,
 * `refs/pull/10/head`, … after its last branch (read at 2026-11-21T08:40:07Z), and `gh api -i
 * repos/cybermonklabs/legion/git/ref/pull/4532/merge` answered 200 (`Date: Sat, 21 Nov 2026
 * 08:45:31 GMT`). The test merge of a head that does not merge into its base is not modelled:
 * GitHub keeps the last test merge it made, as it kept #4532's while the read of `pulls/4532` in
 * the same command answered `merge_commit_sha` null, and the world holds none. The base never
 * moves, but the remote takes a force-push of another history to the issue branch, so a run can
 * reach it, and a run whose push leaves a head without the base's tip is unhealthy from that push
 * on (health.ts's rule 24, `unmergedHead`; a residual of class-search.json). */
function heldRefs(): Json[] {
  const own = (
    fromRemote("for-each-ref", "--format=%(refname)%00%(objectname)%00%(objecttype)") ?? ""
  )
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const [ref = "", sha = "", type = ""] = line.split("\0");
      return refJson(ref, sha, type);
    });
  const tip = branchTip();
  const merge = world === undefined ? undefined : pullRest().merge_commit_sha;
  const pull = `refs/pull/${world?.pr}`;
  return [
    ...own,
    ...(world === undefined || tip === undefined ? [] : [refJson(`${pull}/head`, tip, "commit")]),
    ...(typeof merge === "string" ? [refJson(`${pull}/merge`, merge, "commit")] : []),
  ].sort((one, other) => (String(one.ref) < String(other.ref) ? -1 : 1));
}
/** GitHub's list of the branches, `GET repos/<o>/<r>/branches`: each branch of the run's own
 * remote with the commit it points at and whether it is protected, which the world's base is, by
 * its ruleset, and no other branch; its `protected` parameter keeps the protected ones (`true`) or
 * the others (`false`); paged by `per_page` and `page` (`restPages`). GitHub orders them by name,
 * a number in a name read as one: measured on 2026-11-21 with `gh api
 * 'repos/cybermonklabs/legion/branches?per_page=100'` (read at 2026-11-21T08:43:47Z), `INFRA-840-…`
 * stood before `INFRA-3353/survive` and `…-u9` before `…-u12`, each item `name`, `commit` (`sha`,
 * `url`) and `protected` (`Date: Sat, 21 Nov 2026 08:43:08 GMT` for `?per_page=2`), as git's
 * version sort orders them. */
function branchList(): Json[] {
  const api = `https://api.github.com/repos/${world?.repo ?? ""}`;
  const kept = parameters().get("protected");
  return (
    fromRemote(
      "for-each-ref",
      "--sort=version:refname",
      "--format=%(refname:lstrip=2)%00%(objectname)",
      "refs/heads"
    ) ?? ""
  )
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const [name = "", sha = ""] = line.split("\0");
      return {
        name,
        commit: { sha, url: `${api}/commits/${sha}` },
        protected: name === world?.base,
      };
    })
    .filter((branch) => kept === undefined || String(branch.protected) === kept);
}
/** GitHub's read of a tree, `GET repos/<o>/<r>/git/trees/<sha>`: the tree of the object the path
 * names — a tree's SHA, full or short, a commit's, or a ref — on the run's own remote, its `sha`
 * that object's, each entry its path, mode, type, SHA, a blob's size and the git-data URL of a blob
 * or a tree (no world's tree holds a submodule's `commit` entry: rig.sh writes no `.gitmodules` and
 * no gitlink); with a `recursive` parameter of any value, every entry below it too, each tree before
 * what it holds; never `truncated`, which GitHub is past 100,000 entries, more than any world's
 * tree holds. An object that is no tree and names none is GitHub's 404 (TREE_DOCS). Measured on
 * 2026-11-21 with `gh api -i` against cybermonklabs/legion: tree `5c3793be…` answered its 27
 * entries (`Date: Sat, 21 Nov 2026 08:43:09 GMT`) and so did its short SHA `5c3793be`, as its full
 * SHA (08:43:11 GMT); its commit `bbf69791…` answered them with the commit's SHA (08:43:12 GMT);
 * `?recursive=1` (08:43:13 GMT) and `?recursive=0` (08:43:15 GMT) each answered all 15055 entries;
 * forty `deadbeef` digits answered that 404 (08:43:17 GMT); and `git/trees/INFRA-3353/survive`
 * answered the branch's tip's tree (read at 2026-11-21T08:47:33Z). */
function treeOf(named: string): Json {
  const repo = world?.repo ?? "";
  const object = REF.test(named)
    ? fromRemote("rev-parse", "--verify", "--quiet", named)
    : undefined;
  const tree =
    object === undefined
      ? undefined
      : fromRemote("rev-parse", "--verify", "--quiet", `${object}^{tree}`);
  if (object === undefined || tree === undefined) refuse(NOT_FOUND, TREE_DOCS);
  const recursive = parameters().has("recursive") ? ["-r", "-t"] : [];
  const entries = (fromRemote("ls-tree", ...recursive, "-l", "-z", tree) ?? "")
    .split("\0")
    .flatMap((line) => {
      const [, mode = "", type = "", sha = "", size = "", path = ""] =
        /^(\d+) (\w+) ([0-9a-f]+) +(\S+)\t(.*)$/s.exec(line) ?? [];
      if (path === "") return [];
      const linked = type === "blob" || type === "tree";
      return [
        {
          path,
          mode,
          type,
          sha,
          ...(type === "blob" ? { size: Number(size) } : {}),
          ...(linked ? { url: `https://api.github.com/repos/${repo}/git/${type}s/${sha}` } : {}),
        },
      ];
    });
  return {
    sha: object,
    url: `https://api.github.com/repos/${repo}/git/trees/${object}`,
    tree: entries,
    truncated: false,
  };
}
/** GitHub's list of the tags, `GET repos/<o>/<r>/tags`: each tag of the run's own remote with the
 * commit it names and its archives, an empty list where the remote holds none, as no world's
 * does; paged by `per_page` and `page` (`restPages`). GitHub lists them newest version first:
 * measured on 2026-11-21 with `gh api -i 'repos/cybermonklabs/legion/tags?per_page=12'` (`Date:
 * Sat, 21 Nov 2026 09:00:44 GMT`), `v0.16.1`, `v0.16.0`, `v0.15.0`, …, `v0.13.10`, `v0.13.9`, as
 * git's version sort descending orders them, each item `name`, `zipball_url`, `tarball_url`,
 * `commit` (`sha`, `url`) and `node_id` (`Date: Sat, 21 Nov 2026 08:43:18 GMT` for `?per_page=2`). */
function tagList(): Json[] {
  const api = `https://api.github.com/repos/${world?.repo ?? ""}`;
  return (
    fromRemote(
      "for-each-ref",
      "--sort=-version:refname",
      "--format=%(refname:lstrip=2)%00%(objectname)%00%(*objectname)",
      "refs/tags"
    ) ?? ""
  )
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const [name = "", object = "", peeled = ""] = line.split("\0");
      const sha = peeled === "" ? object : peeled;
      return {
        name,
        zipball_url: `${api}/zipball/refs/tags/${name}`,
        tarball_url: `${api}/tarball/refs/tags/${name}`,
        commit: { sha, url: `${api}/commits/${sha}` },
        node_id: `REF_kwDOAAAAAc${Buffer.from(`refs/tags/${name}`).toString("base64url")}`,
      };
    });
}
/** GitHub's list of a pull request's commits, `GET repos/<o>/<r>/pulls/<n>/commits`, and of its
 * files, `…/files`: for the world's pull request, the commits its head holds and its base does
 * not, oldest first, each as GitHub lists a commit (`commitJson`), and the files of its diff from
 * its merge base, each as the comparison lists one (`diffFiles`); both paged by `per_page` and
 * `page` (`restPages`). A number the world holds no pull request of is GitHub's 404 of the list,
 * its body naming the list's page. Measured on 2026-11-21 with `gh api -i` against
 * cybermonklabs/legion: `pulls/2827/commits?per_page=2` listed f9fb3400, then 3a777f13 whose parent
 * it is, each with the keys of `commitJson` and no `stats` or `files` (`Date: Sat, 21 Nov 2026
 * 10:21:06 GMT`); `pulls/2827/files?per_page=2` its first two files by path, each a `diff-entry`
 * with its `patch` (10:21:07 GMT); and `pulls/999999/commits` (10:21:08 GMT) and
 * `pulls/999999/files` (10:21:09 GMT) each answered `HTTP/2.0 404 Not Found` with that body. */
function pullList(kind: "commits" | "files", number: string): Json[] {
  const tip = branchTip();
  if (world === undefined || number !== String(world.pr) || tip === undefined)
    refuse(NOT_FOUND, kind === "commits" ? PULL_COMMITS_DOCS : PULL_FILES_DOCS);
  const base = `refs/heads/${world.base}`;
  if (kind === "commits")
    return (fromRemote("rev-list", "--reverse", `${base}..${tip}`) ?? "")
      .split("\n")
      .filter((sha) => sha !== "")
      .map(commitJson);
  return diffFiles([`${base}...${tip}`], tip, fromRemote("merge-base", base, tip) ?? base) ?? [];
}
/** A route's answer read from what the run did (rig.sh's fixture_write names which, as `answer`),
 * with `held` the route's own `stdout`, what the world held before the run:
 *   - `repository`: the world's repository (`held`, its settings) as GitHub answers it whole
 *     (rest-objects.ts's `repositoryJson`);
 *   - `pull-request`: the pull request as it stands (`current`, with the body this call left it
 *     with) as GitHub's REST API answers it (rest-objects.ts's `pullRequestJson`), for its read
 *     and for the PATCH of its body;
 *   - `reviews`: the world's reviews, then each review this run created as it stands, each with
 *     the body the run last updated it to, as GitHub answers a review (`reviewJson`);
 *   - `review`: the world's review the path names by id, with the body the run last updated it
 *     to, or GitHub's 404 (a review this run created is `reviewRead`'s, read before the routes);
 *   - `review-comments`: every comment of the pull request's review threads as they stand
 *     (`threads`), or the ones of the review the path names, which is 404 where neither the world
 *     nor the run holds that review, each as GitHub answers it there (`reviewCommentJson`);
 *   - `issue-comments`: the world's conversation, then each comment the run posted on it;
 *   - `combined-status`: GitHub's combined status of the ref the path names, resolved on the run's
 *     own remote — the world's statuses, `state` `pending` where there are none, and 422 for a ref
 *     the remote has not got;
 *   - `git-ref`: GitHub's refs (`heldRefs`): for `git/ref/<ref>` the ref the path names, or
 *     GitHub's 404; for `git/refs/<ref>` that ref where one is named so, and otherwise the list of
 *     the refs whose names start with it, or GitHub's 404 where none does; for `git/refs`, every
 *     ref;
 *   - `commit`: GitHub's read of one commit (`commitOf`), or its 422 for a ref the remote cannot
 *     resolve (`noCommit`);
 *   - `commits`: GitHub's list of the commits a ref reaches (`commitList`);
 *   - `git-commit`: GitHub's git-data read of one commit (`gitCommitOf`), or its 404;
 *   - `branches`: GitHub's list of the branches (`branchList`);
 *   - `git-tree`: GitHub's read of a tree (`treeOf`), or its 404;
 *   - `tags`: GitHub's list of the tags (`tagList`);
 *   - `pull-commits` and `pull-files`: GitHub's lists of the pull request's commits and files
 *     (`pullList`), or its 404 for a pull request the world has not got;
 *   - `check-runs`: the world's check runs of the commit the path names, resolved on the run's own
 *     remote and reported on it as GitHub reports them (`reportedRuns`), and 422 for a ref the
 *     remote has not got;
 *   - `contents`: the file or the directory the path names at the ref the call names, on the run's
 *     own remote (`contentsAt`);
 *   - `ruleset`: the world's one ruleset (`held`) where the path names its id, and GitHub's 404
 *     for any other;
 *   - `branch`: the branch the path names, with its `commit`, the commit that branch points at on
 *     the run's own remote (`commitJson`), as GitHub answers "Get a branch"; a remote that has not
 *     got it leaves the world's answer as it is;
 *   - `review-update`: GitHub's "Update a review for a pull request" (`PUT …/reviews/<id>`), on
 *     a review this run created or one the world held (`held`): the review with the body the call
 *     sent, in the shape GitHub answers it (`reviewJson`), which every later read of it reads
 *     too (`ownReviews`); 404 for a review the pull request has not got, and 422 for an empty
 *     body;
 *   - `conversation-comment`: GitHub's create of a comment on the pull request's conversation
 *     (`POST …/issues/<n>/comments`): the comment, numbered after the ones the run posted before
 *     it (`issueComment`); 422 for an empty body;
 *   - `pr-comment`: what `gh pr comment` prints for that same create, the comment's URL;
 *   - `review-submit`, `review-comment` and `comment-reply`: the writes this file's own answers
 *     take (`reviewSubmission`, `threadCall`) in a world; a call that reaches one here was taken
 *     by neither, as in a run with no world of its own, and no route models it, so it is refused
 *     as any write the world does not model is. */
function fromRun(kind: string, held: unknown): unknown {
  const repo = world?.repo ?? "";
  const path = endpointPath(args);
  switch (kind) {
    case "repository":
      return repositoryJson(isObject(held) ? held : {}, run ?? "", accounts, "full");
    case "pull-request": {
      if (!isObject(held) || world === undefined) return held;
      const view = current(
        kept === undefined ? held : { ...held, body: kept },
        branchTip(),
        headRuns()
      );
      return pullRequestJson(view, pullFacts(String(view.headRefOid ?? world.head)), "full");
    }
    case "reviews":
      return restReviews(held);
    case "review": {
      const id = /\/reviews\/(\d+)$/.exec(path)?.[1];
      const found = [...(Array.isArray(held) ? held : []), ...commentReviews()].find(
        (review) => isObject(review) && String(review.id) === id
      );
      if (found === undefined) refuse(NOT_FOUND, REVIEW_DOCS);
      return reviewJson(withReviewBody(found, heldBodies()) as Json);
    }
    case "review-comments": {
      const id = /\/reviews\/(\d+)\/comments$/.exec(path)?.[1];
      const listed = shownThreads()
        .flatMap((thread) =>
          commentsOf(thread).map((comment) =>
            reviewCommentJson(thread, comment, id === undefined ? "pull" : "review")
          )
        )
        .sort((one, other) => Number(one.id) - Number(other.id));
      if (id === undefined) return listed;
      const own = world === undefined ? undefined : ownReviews(repo).byId.get(id);
      const known =
        (own !== undefined && shown(own)) ||
        [...heldReviews(), ...commentReviews()].some(
          (review) => isObject(review) && String(review.id) === id
        );
      if (!known) refuse(NOT_FOUND, REVIEW_COMMENTS_DOCS);
      return listed.filter((comment) => String(comment.pull_request_review_id) === id);
    }
    case "issue-comments":
      return restIssueComments(held);
    case "combined-status": {
      const ref = /\/commits\/([^/]+)\/status$/.exec(path)?.[1] ?? "";
      const sha = REF.test(ref)
        ? fromRemote("rev-parse", "--verify", "--quiet", `${ref}^{commit}`)
        : undefined;
      if (sha === undefined || sha === "")
        refuse(`gh: No commit found for SHA: ${ref} (HTTP 422)\n`);
      const statuses = isObject(held) && Array.isArray(held.statuses) ? held.statuses : [];
      const states = statuses.map((status) => (isObject(status) ? status.state : undefined));
      const state = states.some((one) => one === "error" || one === "failure")
        ? "failure"
        : statuses.length === 0 || states.includes("pending")
          ? "pending"
          : "success";
      const commit = `https://api.github.com/repos/${repo}/commits/${sha}`;
      return {
        state,
        statuses,
        sha,
        total_count: statuses.length,
        repository: repositoryJson(heldRepository(), run ?? "", accounts, "minimal"),
        commit_url: commit,
        url: `${commit}/status`,
      };
    }
    case "git-ref": {
      const [, plural = "", ref = ""] = /\/git\/ref(s?)(?:\/(.*))?$/.exec(path) ?? [];
      const refs = heldRefs();
      const named = refs.find((one) => one.ref === `refs/${ref}`);
      if (plural === "") return named ?? refuse(NOT_FOUND, REF_DOCS);
      if (ref === "") return refs;
      const matching = refs.filter((one) => String(one.ref).startsWith(`refs/${ref}`));
      if (named === undefined && matching.length === 0) refuse(NOT_FOUND, REFS_DOCS);
      return named ?? matching;
    }
    case "commit": {
      const ref = /^repos\/[^/]+\/[^/]+\/commits\/(.+)$/.exec(path)?.[1] ?? "";
      return commitOf(decoded(ref) ?? ref);
    }
    case "commits":
      return commitList();
    case "git-commit":
      return gitCommitOf(/\/git\/commits\/([^/]+)$/.exec(path)?.[1] ?? "");
    case "branches":
      return branchList();
    case "git-tree": {
      const named = /^repos\/[^/]+\/[^/]+\/git\/trees\/(.+)$/.exec(path)?.[1] ?? "";
      return treeOf(decoded(named) ?? named);
    }
    case "tags":
      return tagList();
    case "pull-commits":
    case "pull-files": {
      const number = /\/pulls\/([^/]+)\/(?:commits|files)$/.exec(path)?.[1] ?? "";
      return pullList(kind === "pull-commits" ? "commits" : "files", number);
    }
    case "check-runs": {
      const ref = /\/commits\/([^/]+)\/check-runs$/.exec(path)?.[1] ?? "";
      const sha = REF.test(ref)
        ? fromRemote("rev-parse", "--verify", "--quiet", `${ref}^{commit}`)
        : undefined;
      if (sha === undefined || sha === "")
        refuse(`gh: No commit found for SHA: ${ref} (HTTP 422)\n`);
      const runs = reportedRuns(checkRunsOn(routes, repo, sha), sha);
      return { total_count: runs.length, check_runs: runs };
    }
    case "contents":
      return contentsAt(path);
    case "ruleset": {
      const id = /\/rulesets\/([^/]+)$/.exec(path)?.[1];
      if (!isObject(held) || String(held.id) !== id) refuseWith(NO_RULESET, NOT_FOUND, 1);
      return held;
    }
    case "branch": {
      const name = /\/branches\/(.+)$/.exec(path)?.[1] ?? "";
      const sha = REF.test(name)
        ? fromRemote("rev-parse", "--verify", "--quiet", `refs/heads/${name}^{commit}`)
        : undefined;
      if (sha === undefined || sha === "" || !isObject(held)) return held;
      const { name: named, ...rest } = held;
      return { name: named, commit: commitJson(sha), ...rest };
    }
    case "review-update": {
      const id = /\/reviews\/(\d+)$/.exec(path)?.[1] ?? "";
      const own = world === undefined ? undefined : ownReviews(repo).byId.get(id);
      const made = own !== undefined && shown(own) ? own : undefined;
      const found =
        made === undefined
          ? [...(Array.isArray(held) ? held : []), ...commentReviews()].find(
              (review) => isObject(review) && String(review.id) === id
            )
          : reviewObject(made);
      if (!isObject(found)) refuse(NOT_FOUND, REVIEW_UPDATE_DOCS);
      const sent = sentBody({ argv: args, stdin, files }) ?? {};
      if (typeof sent.body !== "string" || sent.body === "") refuse(REVIEW_BODY);
      return reviewJson({ ...found, body: sent.body });
    }
    case "conversation-comment": {
      const sent = sentBody({ argv: args, stdin, files }) ?? {};
      if (typeof sent.body !== "string" || sent.body === "") refuse(CONVERSATION_BODY);
      return issueComment({ body: sent.body, at, by: role }, conversation().length);
    }
    case "pr-comment":
      return issueComment({ body: "", at, by: role }, conversation().length).html_url;
    case "auth-status":
      verbatim = true;
      return authStatus();
    case "run":
      return runRefusal();
    case "review-submit":
    case "review-comment":
    case "comment-reply":
      handler = "unrouted";
      refuse(NOT_FOUND);
  }
  return held;
}
/** The account every pane's gh configuration names: the user packages/daemon/internal/ghconfig's
 * `Hosts` writes into the hosts.yml of a role's claim (its `GitUser`). */
const PANE_GH_ACCOUNT = "x-access-token";
/** The token line of the pane's gh as gh prints it: the installation token the rig renders into
 * every pane's hosts.yml (daemon-pane.ts's `RIG_GH_TOKEN`: `ghs_` and 36 characters) masked as gh
 * 2.98.0 masks a token (pkg/cmd/auth/status's `maskToken`: a known prefix, `ghs_` among them, then
 * a `*` for each character after it). It is written from that shape alone: no stand-in reads a
 * token, so none can carry one into an answer. */
const PANE_GH_TOKEN_SHOWN = `ghs_${"*".repeat(36)}`;
/** `gh auth status` as gh 2.98.0 prints it to a pipe for the pane's own gh configuration
 * (`$GH_CONFIG_DIR/hosts.yml`, which the daemon renders for a role's claim): the host, the account
 * (PANE_GH_ACCOUNT) and the file, the account active, the protocol git uses, and the token masked
 * (PANE_GH_TOKEN_SHOWN); `-a`/`--active` prints the one account the same. An installation token
 * holds no scopes, so gh prints no `Token scopes` line for it. Measured on 2026-11-18 from two
 * INFRA issue pods (prod-reads-19.json): exit 0, nothing on stderr, and `github.com`, `  ✓ Logged
 * in to github.com account x-access-token (<GH_CONFIG_DIR>/hosts.yml)`, `  - Active account:
 * true`, `  - Git operations protocol: https`, `  - Token: ghs_***…` on stdout. The world printed
 * `{"login":"widgets-reviewer[bot]"}` to every role. The file is looked for and never read. Any
 * other flag is refused as a call the world does not model: `-t`/`--show-token`, which prints the
 * token itself, `--json`, `--hostname` of another host. A pane with no hosts.yml is the rig's own
 * fault, which ends the call with 2, naming the file. */
function authStatus(): string {
  if (args.slice(2).some((arg) => !["-a", "--active"].includes(arg))) {
    handler = "unrouted";
    refuse(NOT_FOUND);
  }
  const dir = process.env.GH_CONFIG_DIR ?? "";
  const file = `${dir.replace(/\/+$/, "")}/hosts.yml`;
  if (dir === "" || !existsSync(file)) {
    process.stderr.write(
      `gh: ${file} is not there, so gh auth status has no account to show (daemon-pane.ts's workerPane renders it)\n`
    );
    exit(2);
  }
  return [
    "github.com",
    `  ✓ Logged in to github.com account ${PANE_GH_ACCOUNT} (${file})`,
    "  - Active account: true",
    "  - Git operations protocol: https",
    `  - Token: ${PANE_GH_TOKEN_SHOWN}`,
    "",
  ].join("\n");
}
/** gh's refusal of `gh run view` and `gh run watch` naming no run, where no terminal can pick one:
 * the line and the command's usage, measured on 2026-11-18 (implementer-reads.json). */
const VIEW_ID_REQUIRED = `run or job ID required when not running interactively

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
const WATCH_ID_REQUIRED = `run ID required when not running interactively

Usage:  gh run watch <run-id> [flags]

Flags:
      --compact        Show only relevant/failed steps
      --exit-status    Exit with non-zero status if run fails
  -i, --interval int   Refresh interval in seconds (default 3)
  
`;
/** `gh run view` and `gh run watch` in a world that holds no workflow run (its checks are check
 * runs of no Actions workflow), each exit 1 with nothing on stdout: naming a run, GitHub's 404 for
 * it as gh prints it, with the URL gh read it at, on the repository the call names (`-R`, the
 * pane's GH_REPO, else the world's); naming none, gh's refusal and usage. Measured on 2026-11-18:
 * `gh run view 1` and `gh run watch 1` printed `failed to get run: HTTP 404: Not Found
 * (https://api.github.com/repos/<owner>/<repo>/actions/runs/1?exclude_pull_requests=true)`
 * (prod-reads-19.json), and the two with no id the texts above. The world printed the first line
 * of each alone. A job or an attempt (`--job`, `--attempt`) reads another endpoint, which is not
 * measured, so a call naming one is refused as a call the world does not model. */
function runRefusal(): never {
  if (flagValue(args, ["-j", "--job", "-a", "--attempt"]) !== undefined) {
    handler = "unrouted";
    refuse(NOT_FOUND);
  }
  const id = runNamed(args);
  if (id === undefined)
    refuseWith(undefined, args[1] === "watch" ? WATCH_ID_REQUIRED : VIEW_ID_REQUIRED, 1);
  const repo = onGithub(flagValue(args, REPO_FLAGS) ?? process.env.GH_REPO ?? world?.repo ?? "");
  refuseWith(
    undefined,
    `failed to get run: HTTP 404: Not Found (https://api.github.com/repos/${repo}/actions/runs/${id}?exclude_pull_requests=true)\n`,
    1
  );
}
/** What the pull request is read from at `head` (rest-objects.ts's `PullFacts`): the world's
 * repository, number and branches, the base's tip on the run's remote, and how many comments its
 * conversation (`restIssueComments`) and its review threads hold as the run leaves them. */
function pullFacts(head: string): PullFacts {
  return {
    runDir: run ?? "",
    repo: world?.repo ?? "",
    pr: world?.pr ?? 0,
    branch: world?.branch ?? "",
    base: world?.base ?? "",
    head,
    baseSha: fromRemote("rev-parse", `refs/heads/${world?.base}`) ?? "",
    repository: heldRepository(),
    accounts,
    comments: restIssueComments(heldIssueComments()).length,
    reviewComments: threads.list
      .flatMap(commentsOf)
      .filter((comment) => comment.state !== "PENDING").length,
  };
}
/** The pull request's reviews as GitHub's REST API lists them (`reviewJson`): `held`, the world's
 * own, each with the body the run last updated it to, then the run's in the order it made them —
 * the ones it created, as they stand, and the ones GitHub filed its lone comments in — each by
 * when it was made. `gh pr view`'s reviews are these (`reviewNodes`). */
function restReviews(held: unknown): Json[] {
  const repo = world?.repo ?? "";
  const bodies = heldBodies();
  const made = [
    ...(world === undefined ? [] : [...ownReviews(repo).byId.values()].filter(shown)).map(
      (review) => ({
        at: review.at,
        review: reviewObject(review),
      })
    ),
    ...commentReviews().map((review) => ({
      at: String(review.at),
      review: reviewJson(withReviewBody(review, bodies) as Json),
    })),
  ].sort((one, other) => (one.at < other.at ? -1 : one.at > other.at ? 1 : 0));
  return [
    ...(Array.isArray(held) ? held : []).map((review) =>
      reviewJson(withReviewBody(review, bodies) as Json)
    ),
    ...made.map(({ review }) => review),
  ];
}
/** The comments the world held on the pull request's conversation before the run: its
 * conversation route's answer (rig.sh's fixture_write), none where it routes none. */
function heldIssueComments(): unknown {
  return routeFor(routes, ["api", `repos/${world?.repo}/issues/${world?.pr}/comments`])?.stdout;
}
/** The pull request's conversation as GitHub's REST API lists it: `held`, the world's own, then
 * each comment the run posted (`issueComment`). `gh pr view`'s comments are these
 * (`commentNodes`). */
function restIssueComments(held: unknown): Json[] {
  return [
    ...(Array.isArray(held) ? held.filter(isObject) : []),
    ...conversation().map(issueComment),
  ];
}
/** A review of the pull request as GitHub's REST API answers one (`pull-request-review`): its id
 * and node id, its author, body, state and head, its page, the pull request it is on, the links
 * to both, its author's association, and when it was submitted, for one that is no longer
 * pending. A world's review and a review the run created carry the id, author, body, state, head
 * and, for the run's, when it was submitted; a world's review was submitted when the head it
 * stands at was made, and the rest is GitHub's own for that id on this pull request. */
function reviewJson(review: Json): Json {
  const id = Number(review.id);
  const html = `${String(heldPullRequest(routes).url ?? "")}#pullrequestreview-${id}`;
  const pull = `https://api.github.com/repos/${world?.repo}/pulls/${world?.pr}`;
  const login = isObject(review.user) ? String(review.user.login ?? "") : "";
  const head = String(review.commit_id ?? "");
  const made = fromRemote("show", "-s", "--format=%ct", head);
  const submitted =
    review.state === "PENDING"
      ? undefined
      : (review.submitted_at ?? (made === undefined ? undefined : githubTime(Number(made))));
  return {
    id,
    node_id: `${REVIEW_NODE}${id}`,
    user: userJson(login, accounts),
    body: review.body,
    state: review.state,
    html_url: html,
    pull_request_url: pull,
    author_association: "NONE",
    is_copilot_code_review: false,
    copilot_code_review_effort: null,
    _links: { html: { href: html }, pull_request: { href: pull } },
    ...(submitted === undefined ? {} : { submitted_at: submitted }),
    commit_id: head,
  };
}
/** The diff GitHub reads a review comment's place in: the pull request's diff of `file` (its
 * base's merge base with `commit`, to `commit`), from the hunk the comment's `line` of the new
 * file stands in through that line (`diff_hunk`), and that line's place in the file's diff
 * (`position`, GitHub's count of the lines below the file's first hunk header, the line under it
 * the first). A line the diff does not show has no place in it. */
function diffAt(
  commit: string,
  file: string,
  line: unknown
): { hunk: string; position: number | null } {
  const diff = fromRemote(
    "diff",
    "--no-renames",
    `refs/heads/${world?.base}...${commit}`,
    "--",
    file
  );
  const lines = (diff ?? "").split("\n");
  const first = lines.findIndex((text) => text.startsWith("@@"));
  let start = first;
  let number = 0;
  for (let index = Math.max(first, 0); first >= 0 && index < lines.length; index++) {
    const text = lines[index] ?? "";
    const header = /^@@ -\d+(?:,\d+)? \+(\d+)/.exec(text);
    if (header !== null) {
      start = index;
      number = Number(header[1]);
    } else if (!text.startsWith("-") && !text.startsWith("\\")) {
      if (number === line)
        return { hunk: lines.slice(start, index + 1).join("\n"), position: index - first };
      number++;
    }
  }
  return { hunk: "", position: null };
}
/** A review comment of `thread` as GitHub's REST API answers it, read off the comment GitHub's
 * GraphQL holds (gh-threads.ts's `newComment`, and the world's own): its id is the comment's
 * databaseId, which is what the REST reply route names, and its place in the pull request's diff
 * is read off the commit it was made on (`diffAt`). `pull` is the shape the pull request's
 * comments and a comment's create are answered in (`pull-request-review-comment`), where a reply
 * names the comment it replies to (`in_reply_to_id`); `review` the older one GitHub lists a
 * review's comments in (`review-comment`), which carries no line, side or start of its own
 * (production-shapes.json's `…/reviews/{n}/comments`). Both carry `copilot_code_review_severity`,
 * null for a comment no Copilot review made, which neither the description at c9c95fe3 nor the
 * pin of 2026-11-17 holds and production gives in both: measured on 2026-11-21, `gh api
 * repos/cybermonklabs/legion/pulls/2827/comments --jq '.[0] | keys'` named it (`Date: Sat, 21 Nov
 * 2026 10:21:12 GMT`), `gh api -i …/pulls/2827/comments?per_page=1` answered it null (10:21:02
 * GMT), and so did `gh api -i …/pulls/2827/reviews/5816301660/comments?per_page=1` (10:24:42 GMT). */
function reviewCommentJson(thread: Json, comment: Json, form: "pull" | "review"): Json {
  const api = `https://api.github.com/repos/${world?.repo ?? ""}/pulls`;
  const id = Number(comment.databaseId);
  const self = `${api}/comments/${id}`;
  const pull = `${api}/${world?.pr}`;
  const login = restLogin(comment.author);
  const commit = isObject(comment.commit) ? String(comment.commit.oid ?? "") : "";
  const original = isObject(comment.originalCommit)
    ? String(comment.originalCommit.oid ?? "")
    : commit;
  const { hunk, position } = diffAt(original, String(comment.path ?? ""), comment.originalLine);
  const shared = {
    url: self,
    pull_request_review_id: isObject(comment.pullRequestReview)
      ? Number(comment.pullRequestReview.databaseId)
      : null,
    id,
    node_id: comment.id,
    diff_hunk: hunk,
    path: comment.path,
    position,
    original_position: position,
    commit_id: commit,
    original_commit_id: original,
    user: userJson(login, accounts),
    body: comment.body,
    created_at: githubInstant(String(comment.createdAt ?? "")),
    updated_at: githubInstant(String(comment.updatedAt ?? "")),
    html_url: comment.url,
    pull_request_url: pull,
    author_association: comment.authorAssociation ?? "NONE",
    _links: { self: { href: self }, html: { href: comment.url }, pull_request: { href: pull } },
    reactions: reactionsJson(self),
    is_copilot_code_review: false,
    copilot_code_review_severity: null,
    copilot_code_review_sources: [],
  };
  if (form === "review") return shared;
  const replyTo = isObject(comment.replyTo) ? Number(comment.replyTo.databaseId) : undefined;
  return {
    ...shared,
    start_line: thread.startLine ?? null,
    original_start_line: thread.originalStartLine ?? null,
    start_side: null,
    line: comment.line ?? null,
    original_line: comment.originalLine ?? null,
    side: thread.diffSide ?? "RIGHT",
    subject_type: thread.subjectType === "FILE" ? "file" : "line",
    ...(replyTo === undefined ? {} : { in_reply_to_id: replyTo }),
  };
}
let output: unknown;
if (pinned !== undefined) {
  handler = "pinned read";
  if (pinned.exit !== 0) refuseWith(pinned.body, pinned.stderr ?? "", pinned.exit);
  output = pinned.body;
} else if (compared !== undefined) {
  output = compared;
  handler = "comparison";
} else if (own !== undefined) {
  output = own;
  handler =
    submitted !== undefined ? "review submit" : read !== undefined ? "review read" : "thread";
} else if (as === "gh" && args[0] === "api" && route?.exit !== undefined && route.exit !== 0) {
  // A route that refuses a `gh api` call is GitHub's HTTP error, which gh prints as it got it
  // (`refuseWith`): run through the pipeline below, its body was handed to `--jq`, and a filter
  // over the 403 GitHub answers a role App's read of a branch's protection with printed jq's own
  // error and never the 403 (rig.sh's fixture_write).
  handler = "route";
  refuseWith(route.stdout, route.stderr ?? "", route.exit);
} else if (route?.answer !== undefined) {
  handler = route.answer;
  output = fromRun(route.answer, route.stdout);
} else {
  output = route?.stdout ?? "";
  handler = route?.graph !== undefined ? "graph" : "route";
}
if (kept !== undefined) output = withBody(output, kept);
// What GitHub answers the create of a review with is the review it created (`reviewJson`), and
// three of its fields are the call's own and no world's (`reviewCreate`): the state, which is
// `PENDING` for a create that names no event and the state its event leaves the review in for one
// that does, the head it was created at, and its body; a review created with an event was
// submitted by that call. Its id is GitHub's for it (gh-threads.ts's `createdReviewId`: the
// world's route's id, counted on for each review the run created before), its author the bot
// whose token made the call (`callerLogin`), and the rest — its URL — the world's route's. The
// create answered with neither state nor head is what left a round reading back `{id,
// html_url}`: it could not tell whether its approval had been submitted, and it submitted again.
if (
  created !== undefined &&
  typeof output === "object" &&
  output !== null &&
  !Array.isArray(output)
) {
  handler = "review create";
  const { state, head, body: written } = created;
  const id = createdReviewId(threadWorld, { route: route?.match ?? null }, threads.reviews);
  output = reviewJson({
    ...output,
    ...(id === undefined ? {} : { id }),
    user: { login: callerLogin },
    state,
    body: written,
    ...(head === undefined ? {} : { commit_id: head }),
    ...(state === "PENDING" ? {} : { submitted_at: githubInstant(at) }),
  });
  if (head !== undefined) answered = { state, head, body: written };
}
/** The pull request `one` as it stands: at the tip of the run's own remote (`tip`, its own head
 * for a run with no remote), with the checks of that head (`runs`), and the review decision,
 * merge state and mergeability GitHub computes for it from its base's rules (`pullStatus`). A
 * `one` that is no pull request of the world (it carries no `headRefOid`) is its own answer. */
function current(
  one: Record<string, unknown>,
  tip: string | undefined,
  runs: ReportedRun[] | undefined
): Record<string, unknown> {
  if (!("headRefOid" in one)) return one;
  const head = tip ?? String(one.headRefOid);
  const rollup =
    "statusCheckRollup" in one && runs !== undefined
      ? {
          statusCheckRollup: runs.map((run) => ({
            __typename: "CheckRun",
            name: run.name,
            status: run.status.toUpperCase(),
            conclusion: (run.conclusion ?? "").toUpperCase(),
            startedAt: run.started_at ?? null,
            completedAt: run.completed_at ?? null,
            detailsUrl: run.details_url,
            workflowName: "",
          })),
        }
      : {};
  return { ...one, headRefOid: head, ...rollup, ...pullStatus(head) };
}
// What GitHub answers for a pull request's head is read from the run's own remote, and only for
// an answer that carries one: the tip is a `git` call, which a call answered by any other route
// never makes. The checks the object reports (`statusCheckRollup`) are that head's own, in the
// spelling GitHub's GraphQL gives them (`headRuns`): a rollup frozen into the world read green
// on a head whose check the check-runs route said was still running. So are its review decision
// and merge state, computed from the base's rules (`pullStatus`), in gh's spelling
// (`mergeStateStatus`, `mergeable`), which the REST pull request answers in its own
// (`mergeable_state`, `mergeable`, rest-objects.ts's `pullRequestJson`) and the daemon stand-in
// moves the record on (daemon-standin.ts's `roundOutcome`): a decision and a state frozen into
// the world read `CLEAN` before any review where production's base, which requires an approval,
// reads `REVIEW_REQUIRED` and `BLOCKED`. gh's own reads of the pull request in a world are this
// file's (`prRead`), which reads each field it is asked for and no other.
if (
  viewed === undefined &&
  (Array.isArray(output) ? output : [output]).some(
    (one) => typeof one === "object" && one !== null && "headRefOid" in one
  )
) {
  const tip = branchTip();
  const runs = headRuns();
  output = eachAnswer(output, (one) => current(one, tip, runs));
}
if (checked !== undefined) {
  output = checked.text;
  handler = "pr checks";
}
// gh's reading of the world's pull request stands over the route's frozen object, and a text of
// gh's own (its views of the pull request, the pull request's diff) is printed as gh prints it,
// to its last line end; an empty diff is GitHub's own answer for a pull request whose branch
// carries no change.
if (viewed !== undefined) {
  handler = `pr ${args[1]}`;
  output = "json" in viewed ? viewed.json : viewed.text;
  verbatim = "text" in viewed;
}
/** The `pageInfo` of the first connection `value` holds, depth first, which is the one gh follows
 * when it pages a GraphQL query. */
function pageInfoIn(value: unknown): Record<string, unknown> | undefined {
  if (Array.isArray(value)) return value.map(pageInfoIn).find((found) => found !== undefined);
  if (!isObject(value)) return undefined;
  if ("hasNextPage" in value) return value;
  return Object.values(value)
    .map(pageInfoIn)
    .find((found) => found !== undefined);
}
/** A GraphQL call answered from `graph`, the world's state as GitHub's schema names it (rig.sh's
 * fixture_write): a query GitHub refuses with its refusal alone (gh-threads.ts's `graphqlOf`, by
 * gh-routes.ts's `readGraphql`), and no data, since GitHub runs none of a query it refuses; a
 * query from its `query` root, whose pull request is the world's own as it stands (`current`) and
 * whose review threads are the threads as they stand (`threads`); a mutation from its `mutation`
 * root, each field of it on its own, a field that writes a thread answered with the payload of
 * that write (`threaded`). With `--paginate` and a query that takes `$endCursor`, gh asks again
 * with the `endCursor` of the page it was answered until that page has no next one, and so does
 * this: each page is one answer, and a world that answered every page alike would hold a
 * `--paginate` loop for ever. GraphQL this world does not read (a fragment spread, a directive, a
 * fragment definition, a document of two operations) is answered as any call nothing of the
 * world models is, GitHub's 404, which the record names `unrouted`: GitHub runs such a query, and
 * an answer of the world's own would be one GitHub never gives. */
function graphAnswer(
  graph: Record<string, unknown>,
  call: {
    operation: Operation | undefined;
    variables: Record<string, unknown>;
    refused?: GraphError[];
  }
): GraphAnswer[] {
  if (call.refused !== undefined) return [{ errors: call.refused }];
  const { operation } = call;
  if (operation === undefined) {
    handler = "unrouted";
    refuse(NOT_FOUND);
  }
  if (operation.type !== "query") {
    const given = isObject(graph.mutation) ? graph.mutation : {};
    const own = threaded !== undefined && "fields" in threaded ? threaded.fields : [];
    const data: Record<string, unknown> = {};
    const errors: NonNullable<GraphAnswer["errors"]>[number][] = [];
    const fields = operation.selections.flatMap((one) => (one.kind === "field" ? [one] : []));
    for (const [index, selection] of fields.entries()) {
      const alias = selection.alias ?? selection.name;
      const written = own[index]?.alias === alias ? own[index] : undefined;
      if (written?.error !== undefined) errors.push(written.error);
      const payload = written?.payload === undefined ? given[selection.name] : written.payload;
      const answer = executeGraphql(
        { ...operation, selections: [selection] },
        { [selection.name]: payload },
        call.variables
      );
      if (answer.data === undefined || answer.data === null) return [answer];
      Object.assign(data, answer.data);
      errors.push(...(answer.errors ?? []));
    }
    return [errors.length === 0 ? { data } : { data, errors }];
  }
  const query = isObject(graph.query) ? graph.query : {};
  const repository = isObject(query.repository) ? query.repository : {};
  const pullRequest = isObject(repository.pullRequest) ? repository.pullRequest : {};
  const now = current(heldPullRequest(routes), branchTip(), headRuns());
  // The pull request's reviews, the latest review of each reviewer, its conversation and the
  // files it changes are the run's as they stand, in GitHub's GraphQL shape (`reviewNodes`,
  // `latestNodes`, `commentNodes`, `prFiles`), each read only by a query that asks for it.
  const pullNode = Object.defineProperties(
    { ...now, ...pullRequest, reviewThreads: shownThreads() },
    {
      reviews: { get: reviewNodes, enumerable: true },
      latestReviews: { get: latestNodes, enumerable: true },
      comments: { get: commentNodes, enumerable: true },
      files: { get: prFiles, enumerable: true },
    }
  );
  // The viewer is the bot of the App whose token this call's pane holds (gh-threads.ts's
  // `loginOf`): every role read the review App's, the one the world's graph names.
  const root = {
    ...query,
    viewer: { ...(isObject(query.viewer) ? query.viewer : {}), login: callerLogin },
    repository: { ...repository, pullRequest: pullNode },
  };
  const variables = { ...call.variables };
  const pages =
    graphMode &&
    args.includes("--paginate") &&
    operation.variables.some(({ name }) => name === "endCursor");
  const answers: GraphAnswer[] = [];
  for (let page = 0; page < 100; page++) {
    const answer = executeGraphql(operation, root, variables);
    answers.push(answer);
    const info = pageInfoIn(answer.data);
    if (!pages || answer.errors !== undefined || info?.hasNextPage !== true) break;
    if (typeof info.endCursor !== "string") break;
    variables.endCursor = info.endCursor;
  }
  return answers;
}
/** The lists GitHub pages inside an object it answers: a commit's check runs, the workflow runs,
 * a combined status's statuses and a run's jobs. */
const PAGED = ["check_runs", "workflow_runs", "statuses", "jobs"];
/** A REST read's answer, paged as GitHub pages it: `per_page` items (30 unless the query string
 * names another, at most 100; for the list `pagedList` names, its size, and at most that) from the
 * page the query string names (1 unless it names another),
 * of the list the answer is or the one list it holds (`PAGED`, or `pagedList`'s); with
 * `--paginate`, that page and every page after it, which gh asks for in turn. An answer that is no
 * list is its one page. */
function restPages(answer: unknown): unknown[] {
  const query = new URLSearchParams((endpointOf(args) ?? "").split("?")[1] ?? "");
  const size = pagedList?.size ?? 30;
  const perPage = Math.min(
    Math.max(size, 100),
    Math.max(1, Number(query.get("per_page") ?? size) || size)
  );
  const first = Math.max(1, Number(query.get("page") ?? 1) || 1);
  const keys = pagedList === undefined ? PAGED : [pagedList.key];
  const key = isObject(answer) ? keys.find((name) => Array.isArray(answer[name])) : undefined;
  const list = Array.isArray(answer)
    ? answer
    : isObject(answer) && key !== undefined
      ? (answer[key] as unknown[])
      : undefined;
  if (list === undefined) return [answer];
  paging = { first, perPage, total: list.length };
  const pageOf = (page: number) => {
    const items = list.slice((page - 1) * perPage, page * perPage);
    return isObject(answer) && key !== undefined ? { ...answer, [key]: items } : items;
  };
  const pages = [pageOf(first)];
  if (args.includes("--paginate"))
    for (let page = first + 1; (page - 1) * perPage < list.length; page++) pages.push(pageOf(page));
  return pages;
}
const graphPages =
  route?.graph !== undefined && graphqlCall !== undefined
    ? graphAnswer(route.graph, graphqlCall)
    : undefined;
/** The documents gh prints for this call: one per page it was answered with — a GraphQL page, a
 * REST page — and one for every other answer; with `--slurp`, the pages as one list. */
let docs: unknown[] =
  graphPages ??
  (as === "gh" && args[0] === "api" && verb === "GET" && typeof output !== "string"
    ? restPages(output)
    : [output]);
if (args.includes("--slurp")) docs = [docs];
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0) {
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  docs = docs.map((doc) =>
    eachAnswer(doc, (one) =>
      Object.fromEntries(fields.filter((field) => field in one).map((field) => [field, one[field]]))
    )
  );
}
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;
const texts = docs.map((doc) => (typeof doc === "string" ? doc : JSON.stringify(doc)));
/** What gh prints before each page with `--include` (`headerBlock`), none without it: the
 * success's status, the operation's own (rest-required.json's: 201 for a create of a comment, 200
 * for a read), the text media type a call asked for where its answer is text, and the time the
 * answer's object last changed, where production sends it and the world knows it. */
const heads = !include
  ? []
  : texts.map((one, page) =>
      headerBlock(CREATED_ROUTES[worldRoute ?? ""] ? 201 : 200, one, {
        page,
        media:
          typeof output !== "string"
            ? undefined
            : DIFF_MEDIA_TYPE.test(joined)
              ? "diff"
              : /application\/vnd\.github(?:\.v3)?\.patch/.test(joined)
                ? "patch"
                : /application\/vnd\.github(?:\.v3)?\.sha/.test(joined)
                  ? "sha"
                  : RAW_MEDIA_TYPE.test(joined)
                    ? "raw"
                    : undefined,
        modified:
          handler === "repository" || (handler === "pull-request" && verb === "GET")
            ? isObject(output) && output.updated_at
            : handler === "pull-commits" || handler === "pull-files"
              ? pullRest().updated_at
              : undefined,
      })
    );
// gh runs `--jq` over each page on its own, and without it prints the pages of a list as the one
// list they make and every other answer's pages back to back, as they came; with `--include`, each
// page after its own status line and headers, and the pages of a list each as it came (gh reads a
// list's pages as one only where it prints no headers).
let text: string;
if (filter !== undefined) {
  text = "";
  for (const [page, one] of texts.entries()) {
    text += heads[page] ?? "";
    const jq = Bun.spawnSync(["jq", "-r", filter], { stdin: new TextEncoder().encode(one) });
    text += jq.stdout.toString();
    if (jq.exitCode !== 0) {
      if (include) process.stdout.write(text);
      process.stderr.write(jq.stderr.toString());
      exit(1);
    }
  }
} else if (include) text = texts.map((one, page) => `${heads[page] ?? ""}${one}`).join("");
else if (docs.length > 1 && docs.every(Array.isArray)) text = JSON.stringify(docs.flat());
else text = texts.join("");
// GitHub answers a GraphQL query it refuses, or finds part of it naming nothing, with `errors`
// beside whatever data it found, and gh prints its body as GitHub sent it, then `gh: ` and each
// error's message on a line of its own, and exits 1: measured on 2026-11-17, a query naming two
// pull requests GitHub has not got printed its body, then `gh: Could not resolve to a PullRequest
// with the number of 999999.` and on the next line the second message alone (and so did the two
// fields of a query its schema has not got). gh reads the errors so only where it reads the call
// as GraphQL (`graphMode`); posted by another spelling of the endpoint, the body is the whole
// answer and the call exits 0.
const graphErrors = graphMode ? (graphPages ?? []).flatMap((page) => page.errors ?? []) : [];
// gh prints the answer of a `gh api` call no `--jq` reads as GitHub sent it, with no line end of
// its own: GitHub's body ends with none, and a paginated answer's pages follow one another with
// nothing between them. So does a text of gh's own (`verbatim`). Every other answer ends with a
// line end: what `--jq` prints, and a JSON answer of gh's own commands (`gh pr view --json`, `gh
// pr list --json`), which gh encodes with one. Measured on 2026-11-18 from INFRA issue pods with
// gh 2.98.0 against cybermonklabs/legion: `gh api graphql`, `gh api /graphql` and `gh api
// 'graphql?x=1'` of the viewer (the implementer's readings, m/), a REST pull request,
// `--paginate` of a list (its pages made one list), `--paginate --slurp` (`[[…],[…]]`) and
// `--paginate` of a GraphQL connection (its six pages `{…}{…}…` on one line) each ended in its
// last `}` or `]`, while `--jq .number` ended in a line end, and so did `gh pr view --json number`
// and `gh pr list --json number`. The world ended every answer with a line end and put one
// between pages.
const asSent = filter === undefined && (verbatim || (as === "gh" && args[0] === "api"));
if (text !== "") process.stdout.write(asSent || text.endsWith("\n") ? text : `${text}\n`);
if (route?.stderr && own === undefined) process.stderr.write(route.stderr);
if (graphErrors.length > 0)
  process.stderr.write(`gh: ${graphErrors.map((error) => error.message).join("\n")}\n`);
// An answer this stand-in gave of its own is GitHub's own answer, whatever the route behind it
// says: the review and 200 for the one review this run left pending, which is every review of
// these worlds a run left pending itself, and for a review this run created. `gh pr checks`
// exits as gh does on the checks it printed.
if (checked !== undefined) exit(checked.status);
if (graphErrors.length > 0) exit(1);
exit(own === undefined ? (route?.exit ?? 0) : 0);
