import {
  type Agent,
  DISPATCH_AS_OF_ABSENT_HEADER,
  DISPATCH_AS_OF_HEADER,
  DISPATCH_AS_OF_TOKEN_NOT_SERVED,
  type SearchResult,
  type SessionAnswer,
  type WhoamiAgent,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetShownPictures } from "../dispatch-picture-tools";
import {
  ARTIFACT_ID,
  AT,
  actor,
  askBody,
  askReadBody,
  commentBody,
  coverageBody,
  ISSUE,
  issueBody,
  issueResultsBody,
} from "./dispatch-command-surface-responses";
import { fakeSearchResponse } from "./fake-search-response";
import { WHOAMI_NO_CUT } from "./whoami-stand-in";

// A stand-in Dispatch for the read commands at a cut (dispatch://INFRA-4620, R3). It answers one
// world: the routes that one form of each of the seven read commands calls. A call that sends
// `as_of` to a route of kind "answers" is answered as the world stood at that cut, with the cut
// in `Dispatch-As-Of` and each field the world leaves out at a cut in `Dispatch-As-Of-Absent`, as
// Dispatch answers it. A call with no `as_of` is answered at the present, on the clock the test
// sets (`setSystemTime`): every time the world names counts from the one or the other, so a
// command that counts from its own clock at a cut prints other ages and windows. A run whose
// token carries a cut (`whoamiCut`) is answered as Dispatch answers such a token (decision 6):
// every call of kind "answers" at the token's cut, whatever `as_of` it sends, and every call of
// kind "no history" 403 `AS_OF_TOKEN_NOT_SERVED`. Each run records every request it makes, by the
// bearer the run is handed.

/** The host session the command runs in, and the person its token acts for: whoami's own. */
const SESSION = WHOAMI_NO_CUT.session;
export const OWNER = WHOAMI_NO_CUT.owner;
/** The session that holds the issue's claim and its route, and its title. */
const HOLDER = "ses-holder";
export const HOLDER_TITLE = "Implementer";

const COMMENT = "c0e00001-0000-4000-8000-000000000001";
const REPLY = "c0e00002-0000-4000-8000-000000000002";
const ASK = "a5c00001-0000-4000-8000-000000000001";
const ASK_REPLY = "c0e00003-0000-4000-8000-000000000003";

const HOUR = 3_600_000;

/** A route's kind, as the plan's table "Routes and their kinds" names it. */
export type RouteKind = "answers" | "no history" | "caller now";

/** The routes of kind "answers" of the plan's table. Every other GET route but whoami holds no
 *  history. */
const ANSWERS_ROUTES = [
  "/api/v1/issues/{key}",
  "/api/v1/issues",
  "/api/v1/issues/resolve",
  "/api/v1/issues/{key}/events",
  "/api/v1/artifacts/{id}/events",
  "/api/v1/events/log",
  "/api/v1/issues/{key}/comments",
  "/api/v1/artifacts/{id}/comments",
  "/api/v1/comments/{id}",
  "/api/v1/issues/{key}/messages",
  "/api/v1/comments/{id}/history",
  "/api/v1/messages/{id}/history",
  "/api/v1/issues/{key}/messages/{id}",
  "/api/v1/messages/{id}",
  "/api/v1/issues/{key}/asks",
  "/api/v1/artifacts/{id}/asks",
  "/api/v1/asks/{id}",
  "/api/v1/issues/{key}/artifacts",
  "/api/v1/issues/{key}/artifacts/{slug}",
  "/api/v1/projects/{key}/artifacts",
  "/api/v1/projects/{key}/artifacts/{slug}",
  "/api/v1/agents/{session_id}/artifacts/{slug}",
  "/api/v1/artifacts/{id}",
  "/api/v1/artifacts/{id}/text",
  "/api/v1/artifacts/{id}/versions/{number}",
  "/api/v1/issues/{key}/artifacts/{slug}/blocks",
  "/api/v1/projects/{key}/artifacts/{slug}/blocks",
  "/api/v1/issues/{key}/results",
  "/api/v1/search",
  "/api/v1/search/results",
  "/api/v1/agents/{session_id}/answers",
  "/api/v1/asks/open",
  "/api/v1/inbox",
];

/** The routes this world answers that hold no history. */
const NO_HISTORY_ROUTES = [
  "/api/v1/references",
  "/api/v1/issues/{key}/references",
  "/api/v1/issues/{key}/coverage",
  "/api/v1/agents",
  "/api/v1/credential-requests",
  "/api/v1/me/replies",
];

const WHOAMI_ROUTE = "/api/v1/whoami";

interface MatchedRoute {
  /** The route as the table names it, the method first. */
  readonly route: string;
  readonly kind: RouteKind;
  readonly params: Readonly<Record<string, string>>;
}

/** The route `pathname` reaches: of every route whose literal segments the path holds, the one
 *  with the most, so `/api/v1/asks/open` is not taken for `/api/v1/asks/{id}`. A GET the table
 *  names no route for holds no history. */
function matchRoute(method: string, pathname: string): MatchedRoute {
  const segments = pathname.split("/").slice(1).map(decodeURIComponent);
  let best: { readonly matched: MatchedRoute; readonly literals: number } | undefined;
  const tables: [readonly string[], RouteKind][] = [
    [ANSWERS_ROUTES, "answers"],
    [NO_HISTORY_ROUTES, "no history"],
    [[WHOAMI_ROUTE], "caller now"],
  ];
  for (const [paths, kind] of tables) {
    for (const path of paths) {
      const parts = path.split("/").slice(1);
      if (parts.length !== segments.length) continue;
      const params: Record<string, string> = {};
      let literals = 0;
      const fits = parts.every((part, index) => {
        const segment = segments[index] ?? "";
        if (part.startsWith("{")) {
          params[part.slice(1, -1)] = segment;
          return true;
        }
        literals += 1;
        return part === segment;
      });
      if (fits && (best === undefined || literals > best.literals)) {
        best = { matched: { route: `${method} ${path}`, kind, params }, literals };
      }
    }
  }
  return best?.matched ?? { route: `${method} ${pathname}`, kind: "no history", params: {} };
}

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

/** The instant an `as_of` names, in epoch milliseconds, as Dispatch reads it: Go's RFC 3339
 *  parse, which takes a comma before the fraction of a second as it takes a point
 *  (`2026-11-10T12:00:00,5Z`). */
function cutTime(asOf: string): number {
  return Date.parse(asOf.replace(/(T\d{2}:\d{2}:\d{2}),/, "$1."));
}

/** The time a read is answered at: the cut the call sent, or the present on the test's clock. */
interface When {
  readonly ms: number;
  readonly cut: string | undefined;
}

/** What the world answers on one route: a body, and the fields it leaves out at a cut. */
interface Answered {
  readonly body: unknown;
  readonly absent?: readonly string[];
}

/** `value` with each of `keys` removed where the read is at a cut. */
function presentOnly<T extends Record<string, unknown>>(
  value: T,
  when: When,
  keys: readonly string[]
): T {
  if (when.cut === undefined) return value;
  const kept: Record<string, unknown> = { ...value };
  for (const key of keys) delete kept[key];
  return kept as T;
}

/** The fields an issue read leaves out at a cut (the plan's list "Absent fields, by route"). */
const ISSUE_ABSENT = [
  "/route_status",
  "/route_holder",
  "/route_status_reason",
  "/holder_state",
  "/last_seen",
  "/takeable_at",
  "/design_gates",
  "/referenced_by_count",
  "/components/ids",
  "/components/unknown",
  "/open_asks/*/question",
  "/open_asks/*/options",
  "/open_asks/*/multiple",
  "/open_asks/*/urgency",
];

/** `GET /api/v1/issues/{key}`: a session holds the claim and the route, the issue names one
 *  component, and it carries one open ask anchored on the spec. At a cut the route's state, the
 *  holder's liveness, the component split, and the ask's wording (as where a restore changed it
 *  with no event) are left out. */
function issueRead(when: When): Answered {
  const openAsk = presentOnly(
    {
      ...askBody(),
      id: ASK,
      question: "Which way should the release go?",
      anchor: {
        artifact_id: ARTIFACT_ID,
        block_id: null,
        mark_id: "m1",
        orphaned: false,
        quote: "The workspace opens natively.",
        version: 1,
      },
    },
    when,
    ["question", "options", "multiple", "urgency"]
  );
  const issue = {
    ...issueBody(ISSUE),
    claim: { actor: { kind: "session", id: HOLDER }, at: AT },
    route: "role:implementer",
    route_status: "live",
    route_holder: HOLDER,
    holder_state: "live",
    last_seq: 1,
    components: presentOnly(
      { mode: "explicit", ids: ["cli"], unknown: [], reason: null, inherited_from: null },
      when,
      ["ids", "unknown"]
    ),
    open_asks: [openAsk],
  };
  return {
    body: presentOnly(issue, when, [
      "route_status",
      "route_holder",
      "holder_state",
      "referenced_by_count",
    ]),
    absent: ISSUE_ABSENT,
  };
}

const EVENTS = [
  {
    id: 100,
    seq: 1,
    issue_key: ISSUE,
    actor,
    created_at: AT,
    type: "issue.created",
    payload: { status: "todo" },
  },
];

const HOLDER_AGENT: Agent = {
  capabilities: [],
  dir: "/workspace",
  host: "omp",
  last_activity: null,
  last_seen: Date.parse(AT),
  machine_id: "machine-1",
  open_asks: 0,
  roles: ["implementer"],
  session_id: HOLDER,
  state: "live",
  title: HOLDER_TITLE,
};

/** `GET /api/v1/comments/{id}`: a comment and a reply to one of its blocks. At a cut the
 *  comment's body is left out, as for a long comment edited since, and so is the block the reply
 *  answers: whether it was removed and its text, as for a comment changed since. */
function commentRead(when: When): Answered {
  const block = {
    block_id: "b1",
    node: `comment/${COMMENT}#b1`,
    ref: `dispatch://${ISSUE}/comment/${COMMENT}#b-b1`,
    removed: false,
    text: "The quoted words.",
  };
  return {
    body: {
      comment: presentOnly(
        { ...commentBody(), id: COMMENT, body: "The words before the long edit." },
        when,
        ["body"]
      ),
      replies: [
        {
          ...commentBody(),
          id: REPLY,
          reply_to: COMMENT,
          body: "A reply.",
          reply_block: presentOnly(block, when, ["removed", "text"]),
        },
      ],
    },
    absent: ["/comment/body", "/replies/*/reply_block/removed", "/replies/*/reply_block/text"],
  };
}

/** `GET /api/v1/asks/{id}`: a question with one option and one reply in its thread. At a cut the
 *  ask's wording (as where a restore changed it with no event) and the reply's body (as for a long
 *  reply edited since) are left out. */
function askRead(when: When): Answered {
  const ask = {
    ...askBody(),
    id: ASK,
    options: [{ label: "Ship it" }],
  };
  const reply = {
    ...commentBody(),
    id: ASK_REPLY,
    ask_id: ASK,
    body: "The words of the reply before its edit.",
  };
  return {
    body: {
      ...askReadBody(),
      ask: presentOnly(ask, when, ["question", "options", "multiple", "urgency"]),
      replies: [presentOnly(reply, when, ["body"])],
    },
    absent: ["/ask/question", "/ask/options", "/ask/multiple", "/ask/urgency", "/replies/*/body"],
  };
}

/** `GET /api/v1/issues`: the project's one issue, as one page. A session holds its claim, and its
 *  route reaches nobody. At a cut where the route reaches is left out, and so is the component
 *  split. */
function issueListing(when: When): Answered {
  const row = {
    key: ISSUE,
    title: "Native workspace",
    status: "todo",
    priority: 1,
    labels: ["infra"],
    parent: null,
    open_asks: 0,
    claim: { actor: { kind: "session", id: HOLDER }, at: AT },
    route: "role:implementer",
    route_status: "no_holder",
    route_holder: null,
    route_status_reason: null,
    updated_at: AT,
    progress: { tasks: null, children: null },
    components: presentOnly(
      { mode: "explicit", ids: ["cli"], unknown: [], reason: null, inherited_from: null },
      when,
      ["ids", "unknown"]
    ),
  };
  return {
    body: {
      issues: [presentOnly(row, when, ["route_status", "route_holder", "route_status_reason"])],
      total: 1,
      limit: 50,
      offset: 0,
    },
    absent: [
      "/issues/*/route_status",
      "/issues/*/route_holder",
      "/issues/*/route_status_reason",
      "/issues/*/components/ids",
      "/issues/*/components/unknown",
    ],
  };
}

/** `GET /api/v1/search` and `GET /api/v1/search/results`: one ask hit. At a cut its link is left
 *  out, as for an ask whose block's document changed after the cut, its snippet is left out, as
 *  for an ask whose wording a restore changed with no event, and so is the broadened query. */
function searchResults(when: When): Answered {
  const hit = {
    kind: "ask",
    owner: { kind: "issue", key: ISSUE, title: "Native workspace", status: "todo" },
    id: ASK,
    snippet: "Which way should the <mark>release</mark> go?",
    rank: 0.5,
    href: `/issues/${ISSUE}/ask/${ASK}`,
    ask: { state: "open" },
  };
  const body = fakeSearchResponse([presentOnly(hit, when, ["href", "snippet"]) as SearchResult]);
  return {
    body: when.cut === undefined ? body : { ...body, query: { operators: [] } },
    absent: ["/query/broadened", "/results/*/href", "/results/*/snippet"],
  };
}

/** The spec's table: a header row and one row of two cells. */
const SPEC_TABLE = "| Mode | Opens |\n| --- | --- |\n| native | yes |";
const SPEC_MARKDOWN = `# Spec\n\nThe workspace opens natively.\n\n${SPEC_TABLE}\n`;

/** `GET /api/v1/artifacts/{id}/text`: the spec, and with `?blocks=true` its heading, its
 *  paragraph and its table. At a cut the tokens are left out, and so is the id of the untyped
 *  paragraph, of a version no index holds; so are each block's anchored counts, which only the
 *  reference graph records, and the ids of the table's rows and cells, which no index records. */
function documentText(when: When, blocks: boolean): Answered {
  const text = presentOnly(
    { markdown: SPEC_MARKDOWN, token: "sha256:spec-token", version: 1 },
    when,
    ["token"]
  );
  if (!blocks) return { body: text, absent: ["/token"] };
  const references = { comments: 0, asks: 0 };
  const heading = { id: "intro", type: "heading", from: 0, to: 6, token: "sha256:intro" };
  const paragraph = { id: "p-1", type: "paragraph", from: 8, to: 37, token: "sha256:p-1" };
  const tableFrom = SPEC_MARKDOWN.indexOf(SPEC_TABLE);
  const cell = (row: number, column: number, header: string, words: string) =>
    presentOnly({ id: `c-${row}-${column}`, row, column, header, text: words }, when, ["id"]);
  const row = (index: number, cells: readonly object[]) =>
    presentOnly({ id: `r-${index}`, index, cells }, when, ["id"]);
  const table = {
    id: "modes",
    type: "table",
    from: tableFrom,
    to: tableFrom + SPEC_TABLE.length,
    token: "sha256:modes",
    table: {
      rows: [
        row(0, [cell(0, 0, "Mode", "Mode"), cell(0, 1, "Opens", "Opens")]),
        row(1, [cell(1, 0, "Mode", "native"), cell(1, 1, "Opens", "yes")]),
      ],
    },
  };
  return {
    body: {
      ...text,
      blocks: [
        presentOnly({ ...heading, references }, when, ["token", "references"]),
        presentOnly({ ...paragraph, references }, when, ["token", "id", "references"]),
        presentOnly({ ...table, references }, when, ["token", "references"]),
      ],
    },
    absent: [
      "/token",
      "/blocks/*/token",
      "/blocks/*/embedding",
      "/blocks/*/id",
      "/blocks/*/references",
      "/blocks/*/table/rows/*/id",
      "/blocks/*/table/rows/*/cells/*/id",
    ],
  };
}

/** `GET /api/v1/issues/{key}/results`: the one check and its result. At a cut what GitHub says
 *  now is left out, and so are the check's version and digest, of a version no index holds. */
function results(when: When): Answered {
  const body = issueResultsBody();
  return {
    body: {
      ...body,
      checks: body.checks.map((check) =>
        presentOnly(
          {
            ...check,
            results: check.results.map((result) =>
              presentOnly({ ...result, currency: { answer: "current" } }, when, [
                "commit_stands",
                "check_words_moved",
                "currency",
              ])
            ),
          },
          when,
          ["compared_with", "compared_nothing", "pass_stands", "ci", "version", "sha256"]
        )
      ),
    },
    absent: [
      "/checks/*/compared_with",
      "/checks/*/compared_nothing",
      "/checks/*/pass_stands",
      "/checks/*/ci",
      "/checks/*/results/*/commit_stands",
      "/checks/*/results/*/currency",
      "/checks/*/version",
      "/checks/*/sha256",
      "/checks/*/results/*/check_words_moved",
    ],
  };
}

function sessionMessage(id: string, at: number, text: string): SessionAnswer {
  return {
    kind: "message",
    relation: "sent_you_a_message",
    id,
    at: iso(at),
    author: { kind: "user", id: "cy@example.com" },
    text,
    owner: null,
    ref: null,
    href: `/agents/${SESSION}/live`,
  };
}

/** `GET /api/v1/agents/{session_id}/answers`: an answer on an ask an hour before the read, inside
 *  the 72-hour window that ends at it (its ask's question left out at a cut, as where a restore
 *  changed its wording with no event), and a message 80 hours before it, outside. */
function answers(when: When): Answered {
  const answered: SessionAnswer = {
    kind: "answer",
    relation: "answered_your_ask",
    id: ASK,
    at: iso(when.ms - HOUR),
    author: { kind: "user", id: "cy@example.com" },
    text: "The words of an hour before.",
    selected: ["Ship it"],
    ask: presentOnly({ id: ASK, question: "Which way should the release go?" }, when, ["question"]),
    owner: { issue: { key: ISSUE, title: "Native workspace" } },
    ref: `dispatch://${ISSUE}/ask/${ASK}`,
    href: `/issues/${ISSUE}/asks/${ASK}`,
  };
  return {
    body: {
      session_id: SESSION,
      since: iso(when.ms - 72 * HOUR),
      total: 2,
      items: [
        answered,
        sessionMessage("m-old", when.ms - 80 * HOUR, "The words of 80 hours before."),
      ],
    },
    absent: ["/items/*/ask/question"],
  };
}

/** When the two open asks were opened: fixed, so each age counts from the read's time. */
export const ASK_OPENED = "2026-11-01T09:30:00Z";

function openAsk(id: string, waitingOn: string, when: When) {
  return {
    id,
    kind: "question",
    question: `Which way, ${id}?`,
    priority: 1,
    urgency: "med",
    created_at: ASK_OPENED,
    age_seconds: Math.floor((when.ms - Date.parse(ASK_OPENED)) / 1000),
    human_replied: false,
    last_reply: null,
    owner: { issue: { key: ISSUE, title: "Native workspace" } },
    ref: `dispatch://${ISSUE}/ask/${id}`,
    waiting_on: waitingOn,
    waiting_on_person: OWNER,
  };
}

/** `GET /api/v1/asks/open`: one ask waiting on the person and one on the agent. At a cut the
 *  second one's person rests on its project's default, which the records cannot give. */
function openAsks(when: When): Answered {
  const asks = [
    openAsk("ask-human", "human", when),
    presentOnly(openAsk("ask-agent", "agent", when), when, ["waiting_on_person"]),
  ];
  return {
    body: {
      session_id: SESSION,
      as_of: iso(when.ms),
      opened_since: false,
      count: asks.length,
      waiting_on_human: 1,
      waiting_on_agent: 1,
      asks,
    },
    absent: ["/asks/*/waiting_on_person"],
  };
}

function inboxRow(id: string, key: string, waitingOn: string, snoozedUntil: number | null) {
  return {
    id,
    issue_key: key,
    issue: { key, title: `Issue ${key}`, assignee: OWNER },
    author: { kind: "session", id: "ses-9" },
    kind: "question",
    question: `Question of ${key}?`,
    options: [],
    multiple: false,
    urgency: "med",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 1,
    created_at: AT,
    edited_at: null,
    priority: 1,
    waiting_on: waitingOn,
    snoozed_until: snoozedUntil === null ? null : iso(snoozedUntil),
    thread: { replies: [], edits: [], answers: [], followers: [] },
  };
}

/** `GET /api/v1/inbox`: two asks that wait on the person, one the person deferred to a day after
 *  the read and one whose deferral ended a day before it, and one an agent owes. At a cut no
 *  snooze is known, so each is listed under whose turn it is, and the ask the agent owes has its
 *  wording left out, as where a restore changed it with no event. */
function inbox(when: When): Answered {
  const rows = [
    inboxRow("11111111-1111-4111-8111-111111111111", "DSP-11", "human", null),
    inboxRow("22222222-2222-4222-8222-222222222222", "DSP-12", "human", when.ms + 24 * HOUR),
    inboxRow("33333333-3333-4333-8333-333333333333", "DSP-13", "human", when.ms - 24 * HOUR),
    presentOnly(inboxRow("44444444-4444-4444-8444-444444444444", "DSP-14", "agent", null), when, [
      "question",
      "options",
      "multiple",
      "urgency",
    ]),
  ];
  return {
    body: rows.map((row) => presentOnly(row, when, ["snoozed_until"])),
    absent: ["/*/snoozed_until", "/*/question", "/*/options", "/*/multiple", "/*/urgency"],
  };
}

/** `GET /api/v1/me/replies`: one thread whose newest reply is 5 minutes and a half old. */
function myReplies(now: number): unknown {
  const rows = [
    {
      kind: "comment",
      thread_id: "66666666-6666-4666-8666-666666666666",
      ref: "/issues/DSP-16/comments/66666666-6666-4666-8666-666666666666",
      subject: "The staging box lost its disk again",
      issue: { key: "DSP-16", title: "Staging keeps falling over" },
      priority: 2,
      project: "DSP",
      read_through: iso(now - 30 * 60_000),
      unread: true,
      unread_count: 1,
      reply: {
        id: "99999999-9999-4999-8999-999999999999",
        kind: "reply",
        author: { kind: "user", id: "bob@example.com" },
        first_line: "I swapped the disk.",
        created_at: iso(now - 5 * 60_000 - 30_000),
      },
    },
  ];
  return { rows, total: rows.length, limit: 0, offset: 0, as_of: iso(now) };
}

/** The world's answer on `route`, at `when`. */
function answerFor(
  route: string,
  params: Readonly<Record<string, string>>,
  query: URLSearchParams,
  when: When
): Answered | undefined {
  switch (route) {
    case "GET /api/v1/issues":
      return issueListing(when);
    case "GET /api/v1/issues/{key}":
      return params.key === ISSUE ? issueRead(when) : undefined;
    case "GET /api/v1/issues/{key}/events": {
      const after = Number(query.get("after") ?? 0);
      return { body: EVENTS.filter((event) => event.seq > after) };
    }
    case "GET /api/v1/issues/{key}/comments":
      return { body: [] };
    case "GET /api/v1/comments/{id}":
      return params.id === COMMENT ? commentRead(when) : undefined;
    case "GET /api/v1/asks/{id}":
      return params.id === ASK ? askRead(when) : undefined;
    case "GET /api/v1/artifacts/{id}/text":
      return documentText(when, query.get("blocks") === "true");
    case "GET /api/v1/issues/{key}/results":
      return results(when);
    case "GET /api/v1/search":
    case "GET /api/v1/search/results":
      return searchResults(when);
    case "GET /api/v1/agents/{session_id}/answers":
      return answers(when);
    case "GET /api/v1/asks/open":
      return openAsks(when);
    case "GET /api/v1/inbox":
      return inbox(when);
    case "GET /api/v1/references":
      return {
        body: {
          node: { kind: "issue", id: query.get("to") ?? query.get("from") ?? "" },
          edges: [],
        },
      };
    case "GET /api/v1/issues/{key}/references":
      return { body: { members: [], truncated: false } };
    case "GET /api/v1/issues/{key}/coverage":
      return { body: coverageBody() };
    case "GET /api/v1/agents":
      return { body: [HOLDER_AGENT] };
    case "GET /api/v1/credential-requests":
      return { body: { pending: [] } };
    case "GET /api/v1/me/replies":
      return { body: myReplies(when.ms) };
  }
  return undefined;
}

/** One request the stand-in took. */
export interface Call {
  /** The request as a failure names it: method, path and query. */
  readonly text: string;
  readonly method: string;
  readonly route: string;
  readonly kind: RouteKind;
  /** Each `as_of` value its query sent. */
  readonly asOf: readonly string[];
}

/** How one run's stand-in answers, beside the world. */
export interface StandInOptions {
  /** The cut whoami names, as for a token that carries one, which every answer then answers at;
   *  none when unset. */
  readonly whoamiCut?: string;
  /** Whether an answer to a call that sent `as_of` names the cut: false is a Dispatch older than
   *  `as_of`, which ignores the parameter and answers the present. True when unset. */
  readonly acknowledges?: boolean;
  /** The answer in place of the world's to a call of kind "answers" read at a cut, where it gives
   *  one: a refusal. */
  readonly refuseAtCut?: () => Response | undefined;
}

/** What one run of the command did: its exit, its output and the requests it made, in order. */
export interface Run {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  readonly calls: readonly Call[];
}

/** Dispatch's refusal: its status, its code and its message. */
export function refusal(status: number, code: string, error: string): Response {
  return Response.json({ code, error }, { status });
}

/** The stand-in Dispatch, and a way to run `dispatch <argv>` against it. */
export interface AsOfStandIn {
  run(argv: readonly string[], options?: StandInOptions): Promise<Run>;
  stop(): void;
}

export function startAsOfStandIn(name: string): AsOfStandIn {
  const makeDirectory = temporaryDirectories(name);
  const runs = new Map<string, { readonly calls: Call[]; readonly options: StandInOptions }>();
  const server = Bun.serve({
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      const held = runs.get(request.headers.get("Authorization") ?? "");
      if (held === undefined) return refusal(401, "UNAUTHORIZED", "the bearer is no run's");
      const { route, kind, params } = matchRoute(request.method, url.pathname);
      const asOf = url.searchParams.getAll("as_of");
      const call: Call = {
        text: `${request.method} ${url.pathname}${url.search}`,
        method: request.method,
        route,
        kind,
        asOf,
      };
      held.calls.push(call);
      const { options } = held;
      if (kind === "caller now") {
        return Response.json({
          ...WHOAMI_NO_CUT,
          cut: options.whoamiCut ?? null,
        } satisfies WhoamiAgent);
      }
      // A token's cut wins over any `as_of` the call sends, and a route that holds no history
      // refuses the token, naming its cut (decision 6).
      const tokenCut = options.whoamiCut;
      if (tokenCut !== undefined && kind === "no history") {
        return refusal(
          403,
          DISPATCH_AS_OF_TOKEN_NOT_SERVED,
          `this token reads Dispatch as of ${tokenCut}; ${route} holds no past answer`
        );
      }
      const cut = tokenCut ?? asOf[0];
      const acknowledged =
        cut !== undefined && kind === "answers" && options.acknowledges !== false;
      if (acknowledged) {
        const refused = options.refuseAtCut?.();
        if (refused !== undefined) return refused;
      }
      const when: When = acknowledged
        ? { ms: cutTime(cut), cut }
        : { ms: Date.now(), cut: undefined };
      const answered = answerFor(route, params, url.searchParams, when);
      if (answered === undefined) {
        return refusal(404, "NOT_FOUND", `the stand-in holds nothing at ${route}`);
      }
      const response = Response.json(answered.body);
      if (acknowledged) {
        // The cut as Dispatch names it: RFC 3339 in UTC, with six fractional digits.
        response.headers.set(
          DISPATCH_AS_OF_HEADER,
          new Date(when.ms).toISOString().replace(/Z$/, "000Z")
        );
        if ((answered.absent ?? []).length > 0) {
          response.headers.set(DISPATCH_AS_OF_ABSENT_HEADER, (answered.absent ?? []).join(","));
        }
      }
      return response;
    },
  });
  return {
    async run(argv, options = {}) {
      const bearer = fixtureSecret("dsp");
      const calls: Call[] = [];
      runs.set(`Bearer ${bearer}`, { calls, options });
      resetAdviceMemory();
      forgetShownPictures(SESSION);
      const stateDir = makeDirectory();
      let stdout = "";
      let stderr = "";
      const code = await runDispatchCli(
        argv,
        {
          HOME: stateDir,
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
          DISPATCH_TOKEN: bearer,
          DISPATCH_STATE_DIR: stateDir,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: SESSION,
        },
        {
          stdout: (text) => {
            stdout += text;
          },
          stderr: (text) => {
            stderr += text;
          },
          readText: () => "",
          cwd: stateDir,
        }
      );
      return { code, stdout, stderr, calls: [...calls] };
    },
    stop() {
      server.stop(true);
    },
  };
}

/** The forms of the seven read commands (spec R3) that the stand-in's world answers. */
export const READ_FORMS = {
  issue: ["read", "--issue", ISSUE],
  comment: ["read", "--ref", `dispatch://${ISSUE}/comment/${COMMENT}`],
  ask: ["read", "--ref", `dispatch://${ISSUE}/ask/${ASK}`],
  document: ["doc-read", "--issue", ISSUE],
  blocks: ["doc-read", "--issue", ISSUE, "--blocks"],
  search: ["search", "--query", "astrolabe"],
  answers: ["answers"],
  openAsks: ["open-asks"],
  inbox: ["inbox"],
  results: ["results", "--issue", ISSUE],
} as const satisfies Record<string, readonly string[]>;

/** One form of each of the seven read commands. */
export const READ_COMMANDS = [
  READ_FORMS.issue,
  READ_FORMS.blocks,
  READ_FORMS.search,
  READ_FORMS.answers,
  READ_FORMS.openAsks,
  READ_FORMS.inbox,
  READ_FORMS.results,
] as const;

/** Why the calls of a run that reads at `cut` break the plan's table, each call named: whoami
 *  first and with no `as_of`, the cut as the one `as_of` of every call of kind "answers", and no
 *  call of kind "no history". */
export function callsAtCutProblems(run: Run, cut: string): string[] {
  const [first] = run.calls;
  return [
    ...(first?.kind === "caller now"
      ? []
      : [`the first call was ${first?.text ?? "none"}, not GET /api/v1/whoami`]),
    ...run.calls.flatMap((call) => {
      if (call.method !== "GET") return [`${call.text}: a read command wrote`];
      if (call.kind === "no history") return [`${call.text}: a route that holds no history`];
      if (call.kind === "caller now") {
        return call.asOf.length === 0 ? [] : [`${call.text}: whoami was sent as_of`];
      }
      const [sent, ...more] = call.asOf;
      if (sent === undefined) return [`${call.text}: sent no as_of, not ${cut}`];
      if (more.length > 0) return [`${call.text}: sent as_of ${call.asOf.length} times`];
      return cutTime(sent) === cutTime(cut) ? [] : [`${call.text}: as_of=${sent}, not ${cut}`];
    }),
  ];
}

/** The routes of `kind` a run called, each once, sorted. */
export function routesCalled(run: Run, kind: RouteKind): string[] {
  return [
    ...new Set(run.calls.filter((call) => call.kind === kind).map((call) => call.route)),
  ].sort();
}

/** The parts the `Left out at <cut>:` line of `output` names, sorted, or undefined where no such
 *  line is printed. */
export function leftOutParts(output: string, cut: string): string[] | undefined {
  const head = `Left out at ${cut}: `;
  const line = output.split("\n").find((candidate) => candidate.startsWith(head));
  return line?.slice(head.length).split(", ").sort();
}
