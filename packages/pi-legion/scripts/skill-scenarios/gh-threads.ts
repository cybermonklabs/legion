// The pull request's review threads as GitHub holds them during a run, and the review decision
// and merge state the base's rules leave the pull request in: the world's own threads (rig.sh's
// fixture_write writes them into the world's GraphQL graph) and reviews, then the effect of every
// call the run's record holds, replayed in the order the run made them. GitHub keeps what a run
// did, so a thread the run opened is listed, a resolution it made reads `isResolved: true`, a
// reply it posted is its thread's last comment, an approval it submitted counts toward the
// approvals its base requires, and a base that requires conversation resolution holds the pull
// request `BLOCKED` until the last thread is resolved. The gh stand-in answers every read and
// write of a thread, and every read of the pull request's decision and merge state, from this
// state (gh-standin.ts), and the daemon stand-in moves the record on the merge state it leaves
// (daemon-standin.ts's `roundOutcome`), so the world an agent was answered from and the world the
// record moves on are one world, read by one replay. A module of its own, as gh-call.ts is: both
// stand-ins import it, and the gh stand-in is a program that answers a call when it is loaded.
import { readFileSync } from "node:fs";
import {
  type CheckRun,
  checkRunsOn,
  endpointOf,
  endpointPath,
  FIELD_FLAGS,
  FILE_FIELD_FLAGS,
  flagValue,
  isPost,
  mergeStateOn,
  type Route,
  reviewsRoute,
  routeFor,
  sentBody,
  submitsReview,
  valueAt,
  type World,
} from "./gh-call";
import { argumentsOf, type GraphError, isObject, type Operation, readGraphql } from "./gh-routes";
import { checkRunApp, roleApp } from "./rest-objects";

/** A JSON object of the world's, as GitHub's schema names it: a review thread, a review comment,
 * the answer of a call. */
export type Json = Record<string, unknown>;

/** One call as the gh stand-in records it (its `exit`): the call it is answering now and each call
 * the run's record holds are read by the same readers (`graphqlOf`, `threadCall`), so what a call
 * did to the world is what it is answered with and what every later read of the record finds.
 * `handler` is what answered it (gh-standin.ts's `handler`), which a record from before the
 * stand-in kept it does not hold. */
export interface Recorded {
  readonly at?: string;
  readonly as?: string;
  readonly argv: readonly string[];
  readonly files?: Record<string, string> | undefined;
  readonly stdin?: string | undefined;
  readonly route?: string | null;
  readonly review?: { state: string; head: string; body?: string } | undefined;
  readonly handler?: string;
  /** The role whose pane made the call (gh-standin.ts's `by`), none where the pane named none. */
  readonly by?: string;
  readonly exit?: number;
}

/** Every call of the record at `record`, oldest first: the gh stand-in's own `<run>/calls.jsonl`,
 * whose first line is the opening line that stand-in wrote before any call of the run was
 * answered and which is no call of an agent's, so a line with no `argv` is passed over. */
export function recordedCalls(record: string): Recorded[] {
  return readFileSync(record, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as Partial<Recorded>)
    .flatMap((call) => (Array.isArray(call.argv) ? [{ ...call, argv: call.argv }] : []));
}

/** The GraphQL operation a `gh api graphql` call sends, and its variables: the query its `query`
 * field or its `--input` body holds, read by gh-routes.ts's `readGraphql`, on the parser routeOf
 * reads a call's route by (`parseDocument`), and every other field as a variable — a `-F` typed
 * as gh types one (`true`, `false`, `null`, an integer, the text of an `@file`), a `-f` as
 * written — or an `--input`'s own `variables`. `operation` is undefined for a query GitHub
 * refuses, which it runs none of — `refused` holds its errors — and for GraphQL the world does
 * not read.
 *
 * A call to the endpoint written otherwise — `gh api /graphql`, `gh api 'graphql?…'` — is no
 * GraphQL call of gh's (gh reads one only where the endpoint is `graphql` itself) but a POST of
 * its fields to GitHub's GraphQL endpoint, which runs the `query` field of that body and reads
 * its variables from the body's `variables` alone, so a field gh would have sent as a variable
 * is none. Measured on 2026-11-18 from an INFRA issue pod (implementer-reads.json): `gh api
 * /graphql -f query='query{viewer{login}}'` and `gh api 'graphql?x=1' …` answered the viewer as
 * `gh api graphql` does, and `gh api /graphql -f query='query($n:Int!){…}' -F n=2827` was answered
 * `Variable $n of type Int! was provided invalid value`. */
export function graphqlOf(call: Recorded): {
  operation: Operation | undefined;
  variables: Record<string, unknown>;
  refused?: GraphError[];
} {
  const { query, variables } = graphqlSent(call);
  const read = readGraphql(query);
  if ("operation" in read) return { operation: read.operation, variables };
  return "refused" in read
    ? { operation: undefined, variables, refused: read.refused }
    : { operation: undefined, variables };
}

/** The GraphQL document a `gh api graphql` call sends (`graphqlOf` says how it is read), its
 * variables, and the `operationName` it names, which gh sends beside the query and not as a
 * variable: the field of that name, or an `--input`'s own. */
export function graphqlSent(call: Recorded): {
  query: string;
  variables: Record<string, unknown>;
  operationName: string | undefined;
} {
  const { argv } = call;
  const sources = call.files ?? {};
  let query = "";
  let variables: Record<string, unknown> = {};
  let operationName: unknown;
  if (flagValue(argv, ["--input"]) !== undefined || endpointOf(argv) !== "graphql") {
    const sent = sentBody(call) ?? {};
    if (typeof sent.query === "string") query = sent.query;
    if (isObject(sent.variables)) variables = sent.variables;
    operationName = sent.operationName;
  } else
    for (const index of argv.keys()) {
      const pair = valueAt(argv, index, FIELD_FLAGS);
      if (pair === undefined) continue;
      const equals = pair.indexOf("=");
      const name = equals < 0 ? pair : pair.slice(0, equals);
      const text = equals < 0 ? "" : pair.slice(equals + 1);
      let value: unknown = text;
      if (valueAt(argv, index, FILE_FIELD_FLAGS) !== undefined) {
        if (text.startsWith("@")) value = sources[text.slice(1)] ?? "";
        else if (text === "true" || text === "false") value = text === "true";
        else if (text === "null") value = null;
        else if (/^-?\d+$/.test(text)) value = Number(text);
      }
      if (name === "query") query = String(value);
      else variables[name] = value;
      if (name === "operationName") operationName = value;
    }
  return {
    query,
    variables,
    operationName: typeof operationName === "string" ? operationName : undefined,
  };
}

/** GitHub's 404, for a read of something the world has not got. */
export const NOT_FOUND = "gh: Not Found (HTTP 404)\n";
/** The pages of GitHub's REST reference that name the two thread writes whose 404 the world
 * answers for a comment it has not got, which GitHub's 404 body names as its
 * `documentation_url` (gh-standin.ts's `notFoundBody`). */
const REPLY_DOCS =
  "https://docs.github.com/rest/pulls/comments#create-a-reply-for-a-review-comment";
const COMMENT_DOCS =
  "https://docs.github.com/rest/pulls/comments#create-a-review-comment-for-a-pull-request";
/** The create of a review comment, or of a reply, with no body, and the create of a comment that
 * opens a thread naming no `commit_id`, no `path`, or no `line` where it is no comment on the
 * whole file (`subject_type: file`) and names no `position`: the fields GitHub's "Create a review
 * comment for a pull request" requires. GitHub answers both 422; each sentence is the stand-in's
 * own, as its other 422 sentences are, since GitHub's exact wording is not measured here. */
const COMMENT_BODY =
  "gh: Unprocessable Entity (HTTP 422)\nA review comment is created with a body.\n";
const COMMENT_FIELDS =
  "gh: Unprocessable Entity (HTTP 422)\nA review comment that opens a thread names its commit_id, its path and its line.\n";

/** One route of a world's `fixtures.json`, as far as the threads read it: a route's answer, and
 * the graph the world's GraphQL route holds. */
export type ThreadRoute = Route & { readonly graph?: unknown };
/** What the threads are read against: the world's routes and its world.json, undefined for a run
 * that builds no world, whose calls write no thread. */
export interface ThreadWorld {
  readonly routes: readonly ThreadRoute[];
  readonly world: World | undefined;
}
/** The login the run's token acts as: the world's GraphQL `viewer`, which GitHub answers a role
 * App's token with its bot's login, where its REST `user` is GitHub's 403 to an App (measured on
 * 2026-11-17 from a Legion pod: `gh api user` exit 1, `Resource not accessible by integration`;
 * `gh api graphql -f query='query{viewer{login}}'` the App's bot). It is the review App's bot,
 * which a call naming no role, and a world naming no App's (`loginOf`), acts as. */
export function worldLogin(routes: readonly ThreadRoute[]): string {
  const graph = routes.find((route) => isObject(route.graph))?.graph;
  const query = isObject(graph) && isObject(graph.query) ? graph.query : {};
  return isObject(query.viewer) && typeof query.viewer.login === "string" ? query.viewer.login : "";
}
/** The login a call of `role`'s pane acts as: the bot of the App whose installation token that
 * pane holds (rest-objects.ts's `roleApp`: the implement App for the implementer and the merger,
 * the review App for every other role), as world.json names each App's (`logins`), and the
 * world's own (`worldLogin`) for a call naming no role or a world naming no App's. GitHub answers
 * each token as its own App's bot — measured on 2026-11-18 from two INFRA issue pods, `gh api
 * graphql -f query='{viewer{login}}'` read the review App's bot with the tester's token
 * (prod-reads-19.json) and the implement App's with the implementer's (implementer-reads.json) —
 * so the viewer a call reads, and the author of every comment, reply, resolution and review a
 * call makes, is that bot. Before this every role read the review App's bot, so an implementer
 * that told its own replies by the viewer read them as the reviewer's. */
export function loginOf(at: ThreadWorld, role: string | undefined): string {
  const app = roleApp(role);
  return (app === "" ? undefined : at.world?.logins?.[app]) ?? worldLogin(at.routes);
}
/** The account of REST login `login` as GitHub's GraphQL names it under an `author` (an `Actor`):
 * a bot by its App's slug, with no `[bot]`, as a `Bot`, and a user by its login, as a `User`.
 * GitHub's `viewer` and a thread's `resolvedBy` are `User`s, which name a bot with its `[bot]`
 * (`loginOf`). Measured on 2026-11-18 against cybermonklabs/legion's merged pull requests: each
 * resolved thread's first comment's `author` was `{__typename: "Bot", login: "legion-reviewer"}`
 * and its `resolvedBy` `{__typename: "User", login: "legion-implementer[bot]"}`; the REST
 * comment's `user.login` is `legion-reviewer[bot]` (the production reads). The world named a
 * thread comment's author with its `[bot]`, which told a role that compared it to the viewer the
 * two were one account, as production does not. */
export function actorOf(login: string): Json {
  return login.endsWith("[bot]")
    ? { __typename: "Bot", login: login.slice(0, -"[bot]".length) }
    : { __typename: "User", login };
}
/** The REST login of `actor`, an account as GitHub's GraphQL names it (`actorOf`): a `Bot`'s with
 * its `[bot]`, any other's as it is. */
export function restLogin(actor: unknown): string {
  if (!isObject(actor)) return "";
  const login = String(actor.login ?? "");
  return actor.__typename === "Bot" ? `${login}[bot]` : login;
}
/** The pull request as the world's `gh pr view` route holds it, or an empty object. */
export function heldPullRequest(routes: readonly ThreadRoute[]): Json {
  const held = routeFor(routes, ["pr", "view"])?.stdout;
  return isObject(held) ? held : {};
}
/** The comments `calls` posted on the pull request's conversation, oldest first: each `gh pr
 * comment` of the world's repository and each POST of `repos/<repo>/issues/<n>/comments` exiting
 * 0, with the body it posted, when it posted it and the role whose pane posted it (`by`, whose App
 * GitHub names the comment's author, `loginOf`). The gh stand-in answers the conversation from
 * these and the HTTP stand-in counts them on the pull request, so the two read one conversation. */
export function conversationOf(
  world: World | undefined,
  calls: readonly Recorded[]
): { body: string; at: string; by: string | undefined }[] {
  if (world === undefined) return [];
  const path = `repos/${world.repo}/issues/${world.pr}/comments`;
  const posted: { body: string; at: string; by: string | undefined }[] = [];
  for (const call of calls) {
    const { argv } = call;
    if (call.as !== "gh" || call.exit !== 0) continue;
    let text: string | undefined;
    if (argv[0] === "pr" && argv[1] === "comment") {
      const named = flagValue(argv, ["-R", "--repo"]);
      if (named !== undefined && named !== world.repo) continue;
      const file = flagValue(argv, ["-F", "--body-file"]);
      text =
        flagValue(argv, ["-b", "--body"]) ??
        (file === undefined ? undefined : file === "-" ? call.stdin : call.files?.[file]);
    } else if (
      argv[0] === "api" &&
      isPost(argv) &&
      argv.some((arg) => arg.replace(/^\//, "").split("?")[0] === path)
    ) {
      const sent = sentBody({ argv, stdin: call.stdin, files: call.files }) ?? {};
      text = typeof sent.body === "string" ? sent.body : undefined;
    }
    if (text !== undefined) posted.push({ body: text, at: call.at ?? "", by: call.by });
  }
  return posted;
}
/** The object the route a recorded call was matched to answers with, which the record names
 * (`route`, the route's own regex), or undefined when it answered no object. */
export function routeAnswer(
  routes: readonly ThreadRoute[],
  match: string | null | undefined
): Json | undefined {
  if (match === null || match === undefined) return undefined;
  const stdout = routes.find((candidate) => candidate.match === match)?.stdout;
  return isObject(stdout) ? stdout : undefined;
}

/** The node ids GitHub gives the review threads, the review comments and the reviews the run
 * makes: each type's own prefix, then a tail that names this repository, then the comment's (or
 * the review's) databaseId. */
const THREAD_NODE = "PRRT_kwDOAAAAAc4AAD";
const COMMENT_NODE = "PRRC_kwDOAAAAAc4AAC";
export const REVIEW_NODE = "PRR_kwDOAAAAAc4AAE";
/** Where the ids of the reviews GitHub files a comment made outside a review in begin: every
 * review comment is a review's, and GitHub files one made alone — a comment of its own, a reply —
 * in a review of its own, `COMMENTED` with no body, whose id the comment names (production's
 * replies each carry a `pull_request_review_id`, production-shapes.json's `…/pulls/{n}/comments`,
 * where the world's answered null). Its id is this plus the comment's databaseId. */
export const OWN_REVIEW = 6_000_000;
/** The databaseId of the first review comment the run makes, where the world seeds none above
 * it: rig.sh's fixture_write numbers the comments of the world's own threads from 7001. */
const FIRST_COMMENT = 9001;
/** The pull request's review threads as they stand (`threadState`), each in the shape GitHub's
 * GraphQL answers a `PullRequestReviewThread` in, with its comments as `PullRequestReviewComment`s,
 * the databaseId the next comment the run makes is given, and how many reviews the run has
 * created so far (`createsReview`), which numbers the next one (`createdReviewId`). */
export interface Threads {
  readonly list: Json[];
  next: number;
  reviews: number;
}
/** A number GitHub reads out of a field: a JSON number, or the digits a `-f` field sends as text,
 * which gh sends as a string and GitHub reads as the number it names. */
function numberOf(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isInteger(value)) return value;
  return typeof value === "string" && /^\d+$/.test(value) ? Number(value) : undefined;
}
/** The comments of `thread`, oldest first. */
export function commentsOf(thread: Json): Json[] {
  return Array.isArray(thread.comments) ? thread.comments.filter(isObject) : [];
}
/** The commit a review comment of the world stands on. */
function commitOf(comment: Json): string {
  return isObject(comment.commit) ? String(comment.commit.oid ?? "") : "";
}
/** The world's own review threads, as its graph holds them (rig.sh's fixture_write: each in
 * GitHub's GraphQL shape, each comment carrying its databaseId, its REST comment's id), copied,
 * so the calls replayed over them change no route of the world. */
function seededThreads(routes: readonly ThreadRoute[]): Json[] {
  const graph = routes.find((one) => one.graph !== undefined)?.graph;
  const query = isObject(graph) && isObject(graph.query) ? graph.query : {};
  const repository = isObject(query.repository) ? query.repository : {};
  const pullRequest = isObject(repository.pullRequest) ? repository.pullRequest : {};
  const threads = pullRequest.reviewThreads;
  return Array.isArray(threads) ? structuredClone(threads.filter(isObject)) : [];
}
/** The pull request's review threads as GitHub holds them after `calls`: the world's own
 * (`seededThreads`), then the effect of each call (`threadCall`), in the order the run made them.
 * Measured in round 24 of INFRA-764: with the threads the fixture froze, every reviewer that
 * resolved a thread or opened its own read the listing back unchanged and wrote that the world
 * was a mock (reviewer-text-findings-text-only-head-1, -4 and -5 among 29 of 36 runs whose agents
 * named the test). Each call is replayed by the reader that answered it, so what a call was
 * answered with and what a later read finds are one state. */
export function threadState(at: ThreadWorld, calls: readonly Recorded[]): Threads {
  const list = seededThreads(at.routes);
  const seeded = list.flatMap(commentsOf).map((comment) => numberOf(comment.databaseId) ?? 0);
  const state: Threads = { list, next: Math.max(FIRST_COMMENT - 1, ...seeded) + 1, reviews: 0 };
  for (const call of calls) threadCall(at, state, call);
  return state;
}
/** Whether `call` created a review on `repo`'s pull request: a POST of its reviews route
 * (gh-call.ts's `reviewsRoute`) the gh stand-in answered with a review (the record's `review`). A
 * create GitHub refuses left none, and a submit is no create. */
export function createsReview(call: Recorded, repo: string): boolean {
  return (
    call.review !== undefined &&
    isPost(call.argv) &&
    call.argv.some((arg) => reviewsRoute(arg, repo)) &&
    !call.argv.some((arg) => submitsReview(arg, repo) !== undefined)
  );
}
/** The id GitHub gives the review `call` creates, the run having created `earlier` before it: the
 * id the world's create route answers with (rig.sh's fixture_write: 6001, approved_world's 5002),
 * counted on by one for each review the run created before, since GitHub gives every review an id
 * of its own. Every create once took the route's one id, so a second create replaced the first in
 * every read by id, in the REST list and in `gh pr view`, while the review decision counted both
 * (INFRA-764's class search of writes, 2026-11-18). Undefined where the route answers no id. */
export function createdReviewId(
  at: ThreadWorld,
  call: Pick<Recorded, "route">,
  earlier: number
): number | undefined {
  const base = numberOf(routeAnswer(at.routes, call.route)?.id);
  return base === undefined ? undefined : base + earlier;
}
/** A review comment the run makes, as GitHub's GraphQL answers one: its two ids, its author (the
 * bot of the App the call's role acts as, `loginOf`, as GraphQL names it, `actorOf`), where it
 * stands, when it was made, the review it belongs to — the one it was made in, or the review of
 * its own GitHub files it in (OWN_REVIEW) — the comment it replies to and the commit it is on. A
 * comment of a pending review is pending until that review is submitted. */
function newComment(
  at: ThreadWorld,
  state: Threads,
  made: {
    body: string;
    path: unknown;
    line: number | null;
    commit: string;
    review: number | undefined;
    replyTo: Json | undefined;
    at: string;
    by: string | undefined;
    pending: boolean;
  }
): Json {
  const databaseId = state.next++;
  const review = made.review ?? OWN_REVIEW + databaseId;
  const url = String(heldPullRequest(at.routes).url ?? "");
  return {
    __typename: "PullRequestReviewComment",
    id: `${COMMENT_NODE}${databaseId}`,
    databaseId,
    fullDatabaseId: String(databaseId),
    body: made.body,
    author: actorOf(loginOf(at, made.by)),
    authorAssociation: "NONE",
    path: made.path,
    line: made.line,
    originalLine: made.line,
    url: `${url}#discussion_r${databaseId}`,
    createdAt: made.at,
    updatedAt: made.at,
    publishedAt: made.at,
    state: made.pending ? "PENDING" : "SUBMITTED",
    outdated: false,
    isMinimized: false,
    replyTo:
      made.replyTo === undefined
        ? null
        : { id: made.replyTo.id, databaseId: made.replyTo.databaseId },
    commit: { oid: made.commit },
    originalCommit: { oid: made.commit },
    pullRequestReview: { id: `${REVIEW_NODE}${review}`, databaseId: review },
  };
}
/** The thread `comment` opens, unresolved, on the line and the side it was made on. */
function newThread(comment: Json, side: unknown, startLine: number | null): Json {
  return {
    __typename: "PullRequestReviewThread",
    id: `${THREAD_NODE}${String(comment.databaseId)}`,
    isResolved: false,
    isOutdated: false,
    isCollapsed: false,
    path: comment.path,
    line: comment.line,
    originalLine: comment.line,
    startLine,
    originalStartLine: startLine,
    diffSide: typeof side === "string" ? side : "RIGHT",
    startDiffSide: null,
    subjectType: comment.line === null ? "FILE" : "LINE",
    resolvedBy: null,
    viewerCanResolve: true,
    viewerCanUnresolve: false,
    viewerCanReply: true,
    comments: [comment],
  };
}
/** The reply `body` the run posts in `thread`: the thread's last comment from now on, on the line
 * and the commit the thread stands on, replying to the thread's first comment whichever comment
 * of the thread the call named. GitHub files a reply to a reply in the reply's own thread and
 * names the thread's first comment as the one it replies to (`in_reply_to_id`, GraphQL's
 * `replyTo`): measured on 2026-11-17 at 17:28Z on cybermonklabs/legion#2827, `POST …/pulls/2827/
 * comments/4505026306/replies`, 4505026306 itself a reply to 4504802154, answered exit 0 with a
 * comment whose `in_reply_to_id` was 4504802154. The docs' "Replies to replies are not supported"
 * is no refusal GitHub gives, and a world that refused one gave the agent a refusal production
 * does not. The `in_reply_to` of `POST …/pulls/<n>/comments` is read the same way, which is not
 * measured. */
function replyIn(
  at: ThreadWorld,
  state: Threads,
  thread: Json,
  body: string,
  call: Recorded
): Json {
  const first = commentsOf(thread)[0];
  const comment = newComment(at, state, {
    body,
    path: thread.path,
    line: numberOf(thread.line) ?? null,
    commit: first === undefined ? "" : commitOf(first),
    review: undefined,
    replyTo: first,
    at: call.at ?? "",
    by: call.by,
    pending: false,
  });
  thread.comments = [...commentsOf(thread), comment];
  return comment;
}
/** The thread holding the review comment whose databaseId is `id`, and that comment. */
function threadOfComment(
  state: Threads,
  id: number | undefined
): { thread: Json; comment: Json } | undefined {
  for (const thread of state.list) {
    const comment = commentsOf(thread).find((one) => numberOf(one.databaseId) === id);
    if (comment !== undefined) return { thread, comment };
  }
  return undefined;
}
/** The answer of one field of a GraphQL mutation on a thread: the payload of the write it made;
 * null with GitHub's NOT_FOUND where the thread it names is none of the world's, whose type and
 * path are GitHub's and whose sentence is the stand-in's own, since GitHub's exact wording is not
 * measured here; or undefined for a field this state does not answer, which the world's graph
 * answers. */
export interface FieldAnswer {
  readonly alias: string;
  readonly payload: Json | null | undefined;
  readonly error?: { type: string; path: string[]; message: string };
}
/** What a call on the review threads was answered with: the REST comment it made (the thread it
 * stands in and the comment, as GraphQL holds them, which gh-standin.ts's `reviewCommentJson`
 * answers in GitHub's REST shape), a refusal, or one answer per field of a GraphQL mutation. */
export type ThreadAnswer =
  | { readonly rest: { readonly thread: Json; readonly comment: Json } }
  | { readonly refused: string; readonly documentation?: string }
  | { readonly fields: readonly FieldAnswer[] };
/** Applies `call` to `state` and says what GitHub answered it, or undefined for a call that
 * writes no thread (and for every call of a run with no world). Each call is read as GitHub
 * takes it:
 *   - a review create the world answered with a review (the record's `review`): each inline
 *     comment it sent opens a thread, the review's own, pending while the review is;
 *   - the submit of that review: its pending comments are submitted;
 *   - `POST …/pulls/<n>/comments`: a reply where it names `in_reply_to`, and a thread of its own
 *     otherwise, which takes a `commit_id`, a `path` and a `line` (or `subject_type: file`, or a
 *     `position`), each refused 422 without what it takes (COMMENT_BODY, COMMENT_FIELDS);
 *   - `POST …/pulls/<n>/comments/<id>/replies`: a reply in the thread of the comment `<id>`,
 *     which GitHub answers 404 where no thread holds it; a reply to a reply is filed in that
 *     thread, replying to its first comment (`replyIn`);
 *   - `resolveReviewThread` and `addPullRequestReviewThreadReply`: the thread the input names,
 *     resolved or carrying the reply, or GitHub's NOT_FOUND where no thread of the world is it.
 * A call no route answered (the record's `route` null) did nothing, and neither did a GraphQL
 * call GitHub refuses whole for a variable it declares and does not send. */
export function threadCall(
  at: ThreadWorld,
  state: Threads,
  call: Recorded
): ThreadAnswer | undefined {
  const { world } = at;
  if (world === undefined || (call.as ?? "gh") !== "gh" || call.argv[0] !== "api") return undefined;
  const { argv } = call;
  const { repo, pr } = world;
  const made = call.at ?? "";
  const endpoint = endpointPath(argv);
  if (call.review !== undefined) {
    const submits = argv.map((arg) => submitsReview(arg, repo)).find((id) => id !== undefined);
    if (submits !== undefined) {
      for (const comment of state.list.flatMap(commentsOf))
        if (
          comment.state === "PENDING" &&
          isObject(comment.pullRequestReview) &&
          String(comment.pullRequestReview.databaseId) === submits
        )
          comment.state = "SUBMITTED";
      return undefined;
    }
    if (!createsReview(call, repo)) return undefined;
    const review = createdReviewId(at, call, state.reviews++);
    const sent = sentBody(call) ?? {};
    for (const one of Array.isArray(sent.comments) ? sent.comments.filter(isObject) : []) {
      const comment = newComment(at, state, {
        body: typeof one.body === "string" ? one.body : "",
        path: one.path,
        line: numberOf(one.line) ?? null,
        commit: call.review.head,
        review,
        replyTo: undefined,
        at: made,
        by: call.by,
        pending: call.review.state === "PENDING",
      });
      state.list.push(newThread(comment, one.side, numberOf(one.start_line) ?? null));
    }
    return undefined;
  }
  if (call.route === null) return undefined;
  const pulls = `repos/${repo}/pulls/${pr}`;
  if (isPost(argv) && endpoint === `${pulls}/comments`) {
    const sent = sentBody(call) ?? {};
    const body = typeof sent.body === "string" ? sent.body : "";
    if (body === "") return { refused: COMMENT_BODY };
    if (sent.in_reply_to !== undefined && sent.in_reply_to !== null) {
      const found = threadOfComment(state, numberOf(sent.in_reply_to));
      if (found === undefined) return { refused: NOT_FOUND, documentation: COMMENT_DOCS };
      const reply = replyIn(at, state, found.thread, body, call);
      return { rest: { thread: found.thread, comment: reply } };
    }
    const commit = typeof sent.commit_id === "string" ? sent.commit_id : "";
    const path = typeof sent.path === "string" ? sent.path : "";
    const line = numberOf(sent.line) ?? null;
    if (
      commit === "" ||
      path === "" ||
      (line === null && sent.subject_type !== "file" && sent.position === undefined)
    )
      return { refused: COMMENT_FIELDS };
    const comment = newComment(at, state, {
      body,
      path,
      line,
      commit,
      review: undefined,
      replyTo: undefined,
      at: made,
      by: call.by,
      pending: false,
    });
    const thread = newThread(comment, sent.side, numberOf(sent.start_line) ?? null);
    state.list.push(thread);
    return { rest: { thread, comment } };
  }
  const replied = /^repos\/(.+)\/pulls\/(\d+)\/comments\/(\d+)\/replies$/.exec(endpoint);
  if (isPost(argv) && replied?.[1] === repo && Number(replied[2]) === pr) {
    const found = threadOfComment(state, Number(replied[3]));
    if (found === undefined) return { refused: NOT_FOUND, documentation: REPLY_DOCS };
    const sent = sentBody(call) ?? {};
    const body = typeof sent.body === "string" ? sent.body : "";
    if (body === "") return { refused: COMMENT_BODY };
    const reply = replyIn(at, state, found.thread, body, call);
    return { rest: { thread: found.thread, comment: reply } };
  }
  if (endpoint !== "graphql") return undefined;
  const { operation, variables } = graphqlOf(call);
  if (operation?.type !== "mutation") return undefined;
  if (
    operation.variables.some(
      (one) =>
        one.type.endsWith("!") &&
        (variables[one.name] === undefined || variables[one.name] === null)
    )
  )
    return undefined;
  const fields: FieldAnswer[] = [];
  for (const selection of operation.selections) {
    if (selection.kind !== "field") continue;
    const alias = selection.alias ?? selection.name;
    const input = argumentsOf(selection, variables).input;
    const given = isObject(input) ? input : {};
    const resolves = selection.name === "resolveReviewThread";
    if (!resolves && selection.name !== "addPullRequestReviewThreadReply") {
      fields.push({ alias, payload: undefined });
      continue;
    }
    const named = resolves ? given.threadId : given.pullRequestReviewThreadId;
    const thread = state.list.find((one) => one.id === named);
    if (thread === undefined) {
      fields.push({
        alias,
        payload: null,
        error: {
          type: "NOT_FOUND",
          path: [alias],
          message: `Could not resolve to a node with the global id of '${String(named)}'.`,
        },
      });
      continue;
    }
    const clientMutationId = given.clientMutationId ?? null;
    if (resolves) {
      Object.assign(thread, {
        isResolved: true,
        isCollapsed: true,
        // A `User`, which names a bot with its `[bot]`, unlike a comment's author (`actorOf`).
        resolvedBy: { __typename: "User", login: loginOf(at, call.by) },
        viewerCanResolve: false,
        viewerCanUnresolve: true,
      });
      fields.push({ alias, payload: { clientMutationId, thread } });
    } else {
      const body = typeof given.body === "string" ? given.body : "";
      fields.push({
        alias,
        payload: { clientMutationId, comment: replyIn(at, state, thread, body, call) },
      });
    }
  }
  return { fields };
}

/** The rules in force on the world's base, as its answer to `rules/branches/<base>` lists them
 * (rig.sh's base_ruleset_routes), none for a run with no world. The base is read where GitHub
 * enforces it, its rules, since a ruleset alone protects it and a read of the classic
 * protection's fields finds none. */
function baseRules(at: ThreadWorld): Json[] {
  const { world } = at;
  if (world === undefined) return [];
  const rules = routeFor(at.routes, [
    "api",
    `repos/${world.repo}/rules/branches/${world.base}`,
  ])?.stdout;
  return Array.isArray(rules) ? rules.filter(isObject) : [];
}
/** The parameters of the base's rule of `type` (`{}` for a rule that takes none), or undefined
 * where its rules hold none of that type. */
function ruleParameters(rules: readonly Json[], type: string): Json | undefined {
  const rule = rules.find((one) => one.type === type);
  if (rule === undefined) return undefined;
  return isObject(rule.parameters) ? rule.parameters : {};
}

/** One review of the pull request as GitHub counts it toward the review decision: the login that
 * submitted it, its state and the head it stands at, and, for a review the run made, the App its
 * pane called GitHub as (rest-objects.ts's `roleApp`) and the body it was answered with, whose
 * Legion footer the daemon reads (`made`; a record from before the stand-in kept the body holds
 * "" for it). A review the world held before the run has no `made`. */
export interface SubmittedReview {
  readonly login: string;
  readonly state: string;
  readonly head: string;
  readonly made?: { readonly app: string; readonly body: string };
}
/** The pull request's reviews, oldest first: each the world held (its reviews route's answer,
 * rig.sh's fixture_write), by its author, then each the run's record holds, in the state, at
 * the head and with the body the gh stand-in answered its create or its submit with
 * (`Recorded`'s `review`), by the bot of the App the role that made it acts as (`loginOf`), the
 * user the world answers each review the run creates with. A pending create and its submit are
 * two entries, of which the submit alone decides anything. */
export function submittedReviews(at: ThreadWorld, calls: readonly Recorded[]): SubmittedReview[] {
  const { world } = at;
  const listed =
    world === undefined
      ? undefined
      : routeFor(at.routes, ["api", `repos/${world.repo}/pulls/${world.pr}/reviews`])?.stdout;
  return [
    ...(Array.isArray(listed) ? listed.filter(isObject) : []).map((review) => ({
      login: isObject(review.user) ? String(review.user.login ?? "") : "",
      state: String(review.state ?? ""),
      head: String(review.commit_id ?? ""),
    })),
    ...calls.flatMap(({ review, by }) =>
      review === undefined
        ? []
        : [
            {
              login: loginOf(at, by),
              state: review.state,
              head: review.head,
              made: { app: roleApp(by), body: review.body ?? "" },
            },
          ]
    ),
  ];
}
/** The states a review decides its reviewer's standing with: a comment and a pending review
 * leave the review before them standing. */
export const DECIDES = ["APPROVED", "CHANGES_REQUESTED"];
/** GitHub's review decision on the pull request (GraphQL's `reviewDecision`, which gh prints as
 * `""` where it is null), from each reviewer's last review that decides (`DECIDES`), at whatever
 * head it stands, since the base dismisses no approval on a push (`dismiss_stale_reviews_on_push`,
 * false on every world's base, rig.sh's base_ruleset_routes): `CHANGES_REQUESTED` while any change
 * request stands; else, where the base's `pull_request` rule requires approvals
 * (`required_approving_review_count`), `APPROVED` once that many stand and `REVIEW_REQUIRED` until
 * then; and where nothing requires one, `APPROVED` once one stands and null before. Measured on
 * 2026-11-18 at 04:18Z over cybermonklabs/legion's 60 newest open pull requests (`gh pr list
 * --json number,baseRefName,reviewDecision,mergeStateStatus,mergeable`): against `main`, whose
 * ruleset requires one approval, each read `REVIEW_REQUIRED`, `CHANGES_REQUESTED` or `APPROVED`;
 * against a branch no ruleset protects, `""` (#4376, #4373, #4370), `APPROVED` (#4320) or
 * `CHANGES_REQUESTED` (#4355), never `REVIEW_REQUIRED`.
 *
 * Where the rule's `require_last_push_approval` is true (last-push's base alone, rig.sh's
 * review_world; production's is false), an approval by `pusher`, the login that made the head's
 * last push (rest-objects.ts's `pusherOf`), is not counted: "Whether the most recent reviewable
 * push must be approved by someone other than the person who pushed it" (the description of
 * `repository-rule-pull-request` in github/rest-api-description at rest-required.ts's
 * DESCRIPTION_COMMIT). A `pusher` of "", no account of the world, leaves every approval counted.
 *
 * The rule's `require_code_owner_review` asks no approval of its own in any world. GitHub's rule
 * is that "any pull request that modifies content with a code owner must be approved by that code
 * owner" (docs.github.com, Available rules for rulesets, "Require a pull request before merging",
 * read 2026-11-19), and content has a code owner only where a CODEOWNERS file names one: the
 * first of `.github/CODEOWNERS`, `CODEOWNERS` and `docs/CODEOWNERS` the pull request's base
 * branch holds (About code owners, "CODEOWNERS file location"). No world's repository holds one
 * (INFRA-764's ruling of 2026-11-19), so the parameter, production's `true` since
 * 2026-11-18T18:19:22Z (ruleset 12331919, read 2026-11-19T05:35:00Z) and every world's base's
 * (rig.sh's base_ruleset_routes), leaves the decision to the approvals the count asks for, as
 * `false` would. */
export function reviewDecisionOf(
  at: ThreadWorld,
  calls: readonly Recorded[],
  pusher = ""
): string | null {
  const latest = new Map<string, string>();
  for (const review of submittedReviews(at, calls))
    if (DECIDES.includes(review.state)) latest.set(review.login, review.state);
  if ([...latest.values()].includes("CHANGES_REQUESTED")) return "CHANGES_REQUESTED";
  const rule = ruleParameters(baseRules(at), "pull_request");
  const lastPush = rule?.require_last_push_approval === true && pusher !== "";
  const approvals = [...latest.keys()].filter((login) => !(lastPush && login === pusher)).length;
  const required = numberOf(rule?.required_approving_review_count) ?? 0;
  if (required > 0) return approvals >= required ? "APPROVED" : "REVIEW_REQUIRED";
  return approvals > 0 ? "APPROVED" : null;
}

/** The conclusions GitHub takes a required status check as passed with. */
const CHECK_PASSED = ["success", "neutral", "skipped"];
/** Whether the head's check runs `runs` hold one named `name` — by the App whose id is `app`,
 * where the rule names one — that GitHub takes as passed: the last such run, completed with one
 * of `conclusions`. A run still going, one that failed and one never reported are none. */
function passed(
  runs: readonly CheckRun[],
  name: string,
  conclusions: readonly string[],
  app?: number
): boolean {
  const run = runs.findLast(
    (one) => one.name === name && (app === undefined || checkRunApp(one).id === app)
  );
  return run?.status === "completed" && conclusions.includes(run.conclusion ?? "");
}
/** The merge states GitHub reports of a pull request it would merge but for a rule of its base,
 * which a rule unmet holds `BLOCKED`. A conflict (`DIRTY`), a base the head is behind (`BEHIND`)
 * and a state GitHub has not computed (`UNKNOWN`) are each the world's own, which no rule moves:
 * #4374 and #4361 of the measurement below read `REVIEW_REQUIRED` and `DIRTY`. */
const MERGEABLE_BUT_FOR_RULES = ["CLEAN", "UNSTABLE", "HAS_HOOKS"];
/** Whether GitHub shows `thread` open on the pull request: unresolved, and holding a comment it
 * has published (a pending review's comments alone are shown to their writer and nobody else).
 * The thread rule of a base holds its head `BLOCKED` on such a thread (`mergeStateOf`), and
 * score.ts reads a finding left open by it. */
export function shownOpen(thread: Json): boolean {
  return (
    thread.isResolved !== true && commentsOf(thread).some((comment) => comment.state !== "PENDING")
  );
}
/** The merge state GitHub reports for the pull request at `head`, with its threads `threads` and
 * the run's record `calls`: the world's own state for that head (`mergeStates`, gh-call.ts's
 * `mergeStateOn`), held `BLOCKED` while a rule of its base (`baseRules`) that a merge must meet is
 * unmet:
 *   - `pull_request`: fewer approvals stand than it requires, or a change request stands
 *     (`reviewDecisionOf`, whose code owner review asks no approval of its own where no
 *     CODEOWNERS file names an owner, as in every world, and which counts no approval by
 *     `pusher`, the login that made the head's last push, where the rule asks the last push
 *     approved by another); and, where it requires review threads resolved
 *     (`required_review_thread_resolution`), a thread is shown open (`shownOpen`). GitHub moves a
 *     head so protected from `BLOCKED` to `CLEAN` when the last such thread is resolved, and a
 *     world whose state never moved told every agent that resolving a thread changes nothing;
 *   - `required_status_checks`: a check it names (`context`, by the App its `integration_id`
 *     names) has no run on the head that GitHub takes as passed (`passed`);
 *   - `code_scanning`: a tool it names has no check run of the tool's name on the head that
 *     concluded `success`, the check GitHub reports the tool's analysis of the head with
 *     (production's `CodeQL`, GitHub Advanced Security's: rest-objects.ts).
 * Measured on 2026-11-18 at 04:19Z on cybermonklabs/legion's open pull requests against `main`,
 * whose ruleset requires one approval, the checks `ci-ok` and `pr-title`, and CodeQL's code
 * scanning: #4346, #4334, #4328 and #4325, approved, with `ci-ok`, `pr-title` and `CodeQL`
 * successful and every other check successful or skipped, read `CLEAN` and `MERGEABLE`, the
 * state its merge queue takes them in; #4359, approved with `ci-ok` not yet reported, `BLOCKED`;
 * each `REVIEW_REQUIRED` or `CHANGES_REQUESTED` one `BLOCKED`.
 * Not modelled: a merge state that differs by its viewer, where the world answers one merge state
 * to every viewer, since GitHub's rule behind the per-viewer answer is not known. Production gave
 * the review App `BLOCKED` and the implement App `CLEAN` on cybermonklabs/legion#4461 (reads of
 * 12:55Z to 13:00Z on 2026-11-19, head `21dc91c7`, carrying three review App commits), which
 * then merged through the queue at 14:02:33Z as `73e417f113`. A reviewer and the implementer see
 * different states in production only where a reviewer reads the merge state after its own
 * approval: a residual (the architect's ruling of 2026-11-19T14:07Z, class-search.json). */
export function mergeStateOf(
  at: ThreadWorld,
  threads: Threads,
  mergeStates: unknown,
  head: string,
  calls: readonly Recorded[],
  pusher = ""
): string {
  const own = mergeStateOn(mergeStates, head);
  const { world } = at;
  if (world === undefined || !MERGEABLE_BUT_FOR_RULES.includes(own)) return own;
  const rules = baseRules(at);
  const runs = checkRunsOn(at.routes, world.repo, head);
  const review = ruleParameters(rules, "pull_request");
  const approvals = numberOf(review?.required_approving_review_count) ?? 0;
  const checks = ruleParameters(rules, "required_status_checks")?.required_status_checks;
  const tools = ruleParameters(rules, "code_scanning")?.code_scanning_tools;
  const open = threads.list.some(shownOpen);
  const unmet =
    (approvals > 0 && reviewDecisionOf(at, calls, pusher) !== "APPROVED") ||
    (review?.required_review_thread_resolution === true && open) ||
    (Array.isArray(checks) &&
      checks.some(
        (check) =>
          !isObject(check) ||
          !passed(runs, String(check.context), CHECK_PASSED, numberOf(check.integration_id))
      )) ||
    (Array.isArray(tools) &&
      tools.some((tool) => !isObject(tool) || !passed(runs, String(tool.tool), ["success"])));
  return unmet ? "BLOCKED" : own;
}
/** Whether GitHub merges the pull request in the merge state `state`, as gh and GraphQL name it
 * (`mergeable`): `CONFLICTING` in a conflict (`DIRTY`), `UNKNOWN` while GitHub has not computed
 * the state, and `MERGEABLE` in every other. Measured on 2026-11-18 at 04:18Z over
 * cybermonklabs/legion's 60 newest open pull requests: each `DIRTY` one `CONFLICTING`, each
 * `UNKNOWN` one `UNKNOWN`, and each `BLOCKED`, `CLEAN` or `UNSTABLE` one `MERGEABLE`. */
export function mergeableOf(state: string): string {
  if (state === "DIRTY") return "CONFLICTING";
  return state === "UNKNOWN" ? "UNKNOWN" : "MERGEABLE";
}
