import { afterAll, describe, expect, test } from "bun:test";
import {
  type Agent,
  type ArtifactBlock,
  DISPATCH_AS_OF_ABSENT_HEADER,
  DISPATCH_AS_OF_HEADER,
  DISPATCH_AS_OF_NOT_SERVED,
  DISPATCH_INVALID_AS_OF,
  type PostHistoryRead,
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
  artifactBody,
  askBody,
  askReadBody,
  commentBody,
  coverageBody,
  ISSUE,
  issueBody,
  issueResultsBody,
  messageBody,
  versionBody,
} from "./dispatch-command-surface-responses";
import { fakeSearchResponse } from "./fake-search-response";
import { WHOAMI_NO_CUT } from "./whoami-stand-in";

// The calls of each read command under `--as-of` (dispatch://INFRA-4620). The plan's table
// "Routes and their kinds" gives each route a read command calls one kind. A route of kind
// "answers" takes `as_of` and answers as of the cut. A route of kind "no history" answers only the
// present. Whoami describes the caller now. Each row below runs one form of one command with
// `--as-of` against a stand-in Dispatch that records each request. The row fails for each call to
// an "answers" route that does not send the cut as `as_of`, for each call to a "no history" route,
// and when whoami is not the first call (decision 6: the cut of a token wins over `--as-of`). Each
// row also runs its form without `--as-of` first, and the two runs must call each route the table
// says the form calls: so the stand-in answers enough for the command to reach the form.

const makeDirectory = temporaryDirectories("dispatch-as-of-calls");

/** The fixed cut each row sends with `--as-of`. */
const CUT = "2026-11-10T12:00:00Z";

/** The host session the command runs in: whoami's own. */
const SESSION = WHOAMI_NO_CUT.session;
/** The session that holds the issue's claim and its route. */
const HOLDER = "ses-holder";
/** The session whose conversation holds an upload. */
const AGENT_SESSION = "ses-agent";
const PROJECT = "CORE";
/** The external reference that resolves to the issue. */
const EXTERNAL = "owner/repo#12";

const SPEC = ARTIFACT_ID;
const DESIGN = "d0c00001-0000-4000-8000-000000000001";
const NOTES = "f11e0001-0000-4000-8000-000000000001";
const SHOT = "1a6e0001-0000-4000-8000-000000000001";
const RUNBOOK = "d0c00002-0000-4000-8000-000000000002";
const DIAGRAM = "1a6e0002-0000-4000-8000-000000000002";
const SCREEN = "1a6e0003-0000-4000-8000-000000000003";

const ASK = "a5c00001-0000-4000-8000-000000000001";
const DOCUMENT_ASK = "a5c00002-0000-4000-8000-000000000002";
/** The id of the `:::ask` block of the issue's ask. */
const ASK_BLOCK = "release-choice";
const ASK_OPTION = "opt-ship";
const COMMENT = "c0e00001-0000-4000-8000-000000000001";
const DOCUMENT_COMMENT = "c0e00002-0000-4000-8000-000000000002";
const ISSUE_PICTURE_COMMENT = "c0e00003-0000-4000-8000-000000000003";
const PROJECT_PICTURE_COMMENT = "c0e00004-0000-4000-8000-000000000004";
const AGENT_PICTURE_COMMENT = "c0e00005-0000-4000-8000-000000000005";
const MESSAGE = "ae500001-0000-4000-8000-000000000001";

/** The eight hex characters that start `id`: a prefix that names one item here. */
function prefix(id: string): string {
  return id.slice(0, 8);
}

/** The bytes of a PNG picture, as far as a reader sniffs them. */
const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0]);

const ON_ISSUE = { issue_key: ISSUE, project: "DSP", session_id: null };
const ON_PROJECT = { issue_key: null, project: PROJECT, session_id: null };
const ON_AGENT = { issue_key: null, project: "", session_id: AGENT_SESSION };

const PICTURE_VERSIONS = [{ ...versionBody(), mime: "image/png", size: PNG.length }];

function artifact(
  id: string,
  name: string,
  kind: string,
  owner: { readonly issue_key: string | null; readonly project: string },
  versions: readonly object[] = [versionBody()]
) {
  const slug = name.replace(/\./g, "-");
  return {
    ...artifactBody(name, kind),
    ...owner,
    id,
    slug,
    ref_key: `${owner.issue_key ?? owner.project}/${slug}`,
    versions,
  };
}

const ISSUE_ARTIFACTS = [
  artifact(SPEC, "spec.md", "doc", ON_ISSUE),
  artifact(DESIGN, "design.md", "doc", ON_ISSUE),
  artifact(NOTES, "notes.txt", "file", ON_ISSUE, [
    { ...versionBody(), mime: "text/plain", size: 7 },
  ]),
  artifact(SHOT, "shot.png", "image", ON_ISSUE, PICTURE_VERSIONS),
];
const PROJECT_ARTIFACTS = [
  artifact(RUNBOOK, "runbook.md", "doc", ON_PROJECT),
  artifact(DIAGRAM, "diagram.png", "image", ON_PROJECT, PICTURE_VERSIONS),
];
const AGENT_UPLOAD = artifact(SCREEN, "screen.png", "image", ON_AGENT, PICTURE_VERSIONS);

/** The issue's ask: a question with one option, written as an `:::ask` block of the spec. */
const ISSUE_ASK = {
  ...askBody(),
  id: ASK,
  options: [{ id: ASK_OPTION, label: "Ship it", description: "" }],
  block_id: ASK_BLOCK,
  block_artifact: { id: SPEC, primary: true, slug: "spec-md" },
};
const PROJECT_ASK = { ...askBody(), id: DOCUMENT_ASK, issue_key: null, artifact_id: RUNBOOK };
const ASKS = [ISSUE_ASK, PROJECT_ASK];

function comment(id: string, fields: Record<string, unknown> = {}) {
  return { ...commentBody(), id, ...fields };
}

/** A post that shows one picture: the text a read reads the picture's address from. */
function showing(name: string, address: string): string {
  return `The picture.\n\n![${name}](${address})`;
}

const ISSUE_COMMENTS = [
  comment(COMMENT),
  comment(ISSUE_PICTURE_COMMENT, {
    body: showing("shot.png", `dispatch://${ISSUE}/artifact/shot-png@v1`),
  }),
  comment(PROJECT_PICTURE_COMMENT, {
    body: showing("diagram.png", `dispatch://${PROJECT}/artifact/diagram-png@v1`),
  }),
  comment(AGENT_PICTURE_COMMENT, {
    body: showing("screen.png", `dispatch://agent/${AGENT_SESSION}/artifact/screen-png@v1`),
  }),
];
const PROJECT_COMMENT = comment(DOCUMENT_COMMENT, { issue_key: null, artifact_id: RUNBOOK });
const COMMENTS = [...ISSUE_COMMENTS, PROJECT_COMMENT];

const MESSAGE_READ = { message: { ...messageBody(), id: MESSAGE }, replies: [] };

/** The issue's events: its creation, then a comment and a message whose bodies the events cut,
 *  so a read of the events reads each post whole. */
const EVENTS = [
  { type: "issue.created", payload: { status: "todo" } },
  { type: "comment.created", payload: { id: COMMENT, body: "The first", body_truncated: true } },
  { type: "message.created", payload: { id: MESSAGE, body: "The first", body_truncated: true } },
].map((event, index) => ({
  id: 100 + index,
  seq: index + 1,
  issue_key: ISSUE,
  actor,
  created_at: AT,
  ...event,
}));

/** `GET /api/v1/issues/{key}`: a session holds the claim and the route, so the present read asks
 *  the agent list (`GET /api/v1/agents`) for that session's name and liveness. */
function issueRead() {
  return {
    ...issueBody(ISSUE),
    claim: { actor: { kind: "session", id: HOLDER }, at: AT },
    route: "role:implementer",
    route_status: "live",
    route_holder: HOLDER,
    primary_artifact_id: SPEC,
    plan_artifact_id: null,
    last_seq: EVENTS.length,
    artifacts: ISSUE_ARTIFACTS,
    open_asks: [ISSUE_ASK],
  };
}

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
  title: "Implementer",
};

const HISTORY = {
  deleted_at: null,
  deleted_by: null,
  entries: [
    {
      body: "The words before the edit.",
      edited_by: { kind: "session", id: HOLDER },
      edited_at: AT,
    },
  ],
} satisfies PostHistoryRead;

/** The spec's table: a header row and one row, of one cell each. */
const SPEC_TABLE = "| Mode |\n| --- |\n| native |";
const SPEC_MARKDOWN = `# Spec\n\nThe workspace opens natively.\n\n${SPEC_TABLE}\n`;
const RUNBOOK_MARKDOWN = "# Runbook\n\nRestart the listener.\n";

/** A document's heading block: the first six UTF-8 bytes of its markdown. */
function headingBlock(id: string) {
  return {
    id,
    type: "heading",
    from: 0,
    to: 6,
    token: `sha256:${id}`,
    references: { comments: 0, asks: 0 },
    attributes: [],
  };
}
const SPEC_BLOCK = headingBlock("intro");
const RUNBOOK_BLOCK = headingBlock("steps");

/** The spec's table block, its rows and its cells. */
const SPEC_TABLE_BLOCK: ArtifactBlock = {
  id: "modes",
  type: "table",
  from: SPEC_MARKDOWN.indexOf(SPEC_TABLE),
  to: SPEC_MARKDOWN.indexOf(SPEC_TABLE) + SPEC_TABLE.length,
  token: "sha256:modes",
  references: { comments: 0, asks: 0 },
  attributes: [],
  table: {
    rows: [
      {
        id: "r-0",
        index: 0,
        cells: [{ id: "c-0-0", row: 0, column: 0, header: "Mode", text: "Mode" }],
      },
      {
        id: "r-1",
        index: 1,
        cells: [{ id: "c-1-0", row: 1, column: 0, header: "Mode", text: "native" }],
      },
    ],
  },
};

/** Each document's markdown and its blocks, by artifact id. */
const DOCUMENTS = new Map<string, { readonly markdown: string; readonly blocks: ArtifactBlock[] }>([
  [SPEC, { markdown: SPEC_MARKDOWN, blocks: [SPEC_BLOCK, SPEC_TABLE_BLOCK] }],
  [RUNBOOK, { markdown: RUNBOOK_MARKDOWN, blocks: [RUNBOOK_BLOCK] }],
]);

/**
 * `blocks` as the text route gives them at a cut, and the members its answer names absent: the
 * anchored counts of each block, which only the reference graph records, and the ids of each
 * table's rows and cells, which no index records (`blocksAtTheCut` and `tableAtTheCut` in
 * packages/envoy/internal/dispatch/api/document_as_of.go). Every other member stands as at the
 * present, since nothing in this world changed after the cut.
 */
function blocksAtTheCut(blocks: readonly ArtifactBlock[]): {
  readonly blocks: object[];
  readonly absent: string[];
} {
  const absent = new Set<string>();
  const kept = blocks.map(({ references: _references, table, ...block }) => {
    absent.add("/blocks/*/references");
    if (table === undefined) return block;
    const rows = (table.rows ?? []).map(({ id: _row, cells, ...row }) => {
      absent.add("/blocks/*/table/rows/*/id");
      return {
        ...row,
        cells: (cells ?? []).map(({ id: _cell, ...cell }) => {
          absent.add("/blocks/*/table/rows/*/cells/*/id");
          return cell;
        }),
      };
    });
    return { ...block, table: { rows } };
  });
  return { blocks: kept, absent: [...absent] };
}

const OPEN_ASKS = {
  session_id: SESSION,
  as_of: AT,
  opened_since: false,
  count: 0,
  waiting_on_human: 0,
  waiting_on_agent: 0,
  asks: [],
};

/** A route's kind, as the plan's table names it. */
type Kind = "answers" | "no history" | "caller now";

/** One row of the plan's table "Routes and their kinds". */
interface TableRow {
  /** The route, or the routes that one row of the table names together. */
  readonly routes: readonly string[];
  readonly kind: Kind;
  /** Each form below that calls the route, with `--as-of` and without it. A route of kind
   *  "no history" names none: a form must call it only without `--as-of`. */
  readonly calledBy?: readonly string[];
  /** Each form below that calls the route with `--as-of` only: its read at the cut reads what its
   *  read of the present reads from another route. */
  readonly calledAtACutBy?: readonly string[];
  /** Each form below that calls the route without `--as-of` only, for the reason
   *  `calledAtACutBy` gives. */
  readonly calledAtThePresentBy?: readonly string[];
}

/**
 * The plan's table, route by route, with the forms of the rows below that call each route. A
 * route the command calls that this table does not name holds no history, as the plan says of
 * every GET route the table does not list.
 */
const TABLE: readonly TableRow[] = [
  {
    routes: ["GET /api/v1/issues/{key}"],
    kind: "answers",
    calledBy: [
      "read: an issue a session claims and routes",
      "read: an issue named owner/repo#n",
      "read: an issue's log, with bodies its events cut",
      "read: an issue's children",
      "read: an issue's comments",
      "read: an issue document's summary",
      "doc-read: an issue's primary document",
      "doc-read: an issue named owner/repo#n",
      "doc-read: --blocks",
      "doc-read: --version",
      "doc-read: an uploaded file",
    ],
    // A block read at a cut reads the block from its document (`calledAtACutBy` of the text
    // route), which it finds through the issue.
    calledAtACutBy: ["read: a block of an issue document"],
  },
  { routes: ["GET /api/v1/issues"], kind: "answers" },
  {
    routes: ["GET /api/v1/issues/resolve"],
    kind: "answers",
    calledBy: [
      "read: an issue named owner/repo#n",
      "doc-read: an issue named owner/repo#n",
      "results: an issue named owner/repo#n",
    ],
  },
  {
    routes: ["GET /api/v1/issues/{key}/events"],
    kind: "answers",
    calledBy: [
      "read: an issue a session claims and routes",
      "read: an issue named owner/repo#n",
      "read: an issue's log, with bodies its events cut",
    ],
  },
  { routes: ["GET /api/v1/artifacts/{id}/events"], kind: "answers" },
  { routes: ["GET /api/v1/events/log"], kind: "answers" },
  {
    routes: ["GET /api/v1/issues/{key}/comments"],
    kind: "answers",
    calledBy: [
      "read: an issue's comment by an id prefix",
      "read: an issue's comments",
      "doc-read: an issue's primary document",
      "doc-read: an issue named owner/repo#n",
      "doc-read: --version",
    ],
  },
  {
    routes: ["GET /api/v1/artifacts/{id}/comments"],
    kind: "answers",
    calledBy: [
      "read: a project document's comment by an id prefix",
      "doc-read: a project document",
    ],
  },
  {
    routes: ["GET /api/v1/comments/{id}"],
    kind: "answers",
    calledBy: [
      "read: an issue a session claims and routes",
      "read: an issue named owner/repo#n",
      "read: an issue's log, with bodies its events cut",
      "read: an issue's comment by its whole id",
      "read: an issue's comment by an id prefix",
      "read: a project document's comment by its whole id",
      "read: a project document's comment by an id prefix",
      "read: a picture at an issue address",
      "read: a picture at a project address",
      "read: a picture at an agent address",
      "read: --history of a comment",
    ],
  },
  {
    routes: ["GET /api/v1/issues/{key}/messages"],
    kind: "answers",
    calledBy: ["read: --history of a message by an id prefix"],
  },
  {
    routes: ["GET /api/v1/comments/{id}/history"],
    kind: "answers",
    calledBy: ["read: --history of a comment"],
  },
  {
    routes: ["GET /api/v1/messages/{id}/history"],
    kind: "answers",
    calledBy: [
      "read: --history of a message by its whole id",
      "read: --history of a message by an id prefix",
    ],
  },
  {
    routes: ["GET /api/v1/issues/{key}/messages/{id}"],
    kind: "answers",
    calledBy: [
      "read: an issue a session claims and routes",
      "read: an issue named owner/repo#n",
      "read: an issue's log, with bodies its events cut",
      "read: an issue's message",
    ],
  },
  {
    routes: ["GET /api/v1/messages/{id}"],
    kind: "answers",
    calledBy: [
      "read: --message",
      "read: --history of a message by its whole id",
      "read: --history of a message by an id prefix",
    ],
  },
  {
    routes: ["GET /api/v1/issues/{key}/asks"],
    kind: "answers",
    calledBy: ["read: an issue's ask by an id prefix", "read: an issue's ask by its block id"],
  },
  {
    routes: ["GET /api/v1/artifacts/{id}/asks"],
    kind: "answers",
    calledBy: ["read: a project document's ask by an id prefix", "doc-read: a project document"],
  },
  {
    routes: ["GET /api/v1/asks/{id}"],
    kind: "answers",
    calledBy: [
      "read: an issue's ask by its whole id",
      "read: an issue's ask by an id prefix",
      "read: an issue's ask by its block id",
      "read: a project document's ask by its whole id",
      "read: a project document's ask by an id prefix",
      "read: a part of an ask",
    ],
  },
  { routes: ["GET /api/v1/issues/{key}/artifacts"], kind: "answers" },
  {
    routes: ["GET /api/v1/issues/{key}/artifacts/{slug}"],
    kind: "answers",
    calledBy: ["read: a picture at an issue address"],
  },
  {
    routes: ["GET /api/v1/projects/{key}/artifacts"],
    kind: "answers",
    calledBy: [
      "read: --project",
      "read: a project document's summary",
      "doc-read: a project document",
    ],
  },
  {
    routes: ["GET /api/v1/projects/{key}/artifacts/{slug}"],
    kind: "answers",
    calledBy: [
      "read: a project document's ask by its whole id",
      "read: a project document's ask by an id prefix",
      "read: a project document's comment by its whole id",
      "read: a project document's comment by an id prefix",
      "read: a project document's summary",
      "read: a picture at a project address",
      "doc-read: a project document",
    ],
    calledAtACutBy: ["read: a block of a project document"],
  },
  {
    routes: ["GET /api/v1/agents/{session_id}/artifacts/{slug}"],
    kind: "answers",
    calledBy: ["read: a picture at an agent address", "doc-read: an agent's upload"],
  },
  { routes: ["GET /api/v1/artifacts/{id}"], kind: "answers" },
  {
    routes: ["GET /api/v1/artifacts/{id}/text"],
    kind: "answers",
    calledBy: [
      "doc-read: an issue's primary document",
      "doc-read: an issue named owner/repo#n",
      "doc-read: --blocks",
      "doc-read: a project document",
    ],
    // A block read at a cut reads its block and the markdown the block's range indexes from one
    // answer at the cut; the read of the present reads the graph's node and the block route.
    calledAtACutBy: ["read: a block of an issue document", "read: a block of a project document"],
  },
  {
    routes: ["GET /api/v1/artifacts/{id}/versions/{number}"],
    kind: "answers",
    calledBy: [
      "read: a picture at an issue address",
      "read: a picture at a project address",
      "read: a picture at an agent address",
      "doc-read: --version",
      "doc-read: an uploaded file",
      "doc-read: an agent's upload",
    ],
  },
  {
    routes: ["GET /api/v1/issues/{key}/artifacts/{slug}/blocks"],
    kind: "answers",
    calledAtThePresentBy: ["read: a block of an issue document"],
  },
  {
    routes: ["GET /api/v1/projects/{key}/artifacts/{slug}/blocks"],
    kind: "answers",
    calledAtThePresentBy: ["read: a block of a project document"],
  },
  {
    routes: ["GET /api/v1/issues/{key}/results"],
    kind: "answers",
    calledBy: ["results: an issue named owner/repo#n"],
  },
  {
    routes: ["GET /api/v1/search", "GET /api/v1/search/results"],
    kind: "answers",
    calledBy: ["search: a query"],
  },
  {
    routes: ["GET /api/v1/agents/{session_id}/answers"],
    kind: "answers",
    calledBy: ["answers: this session's answers"],
  },
  {
    routes: ["GET /api/v1/asks/open"],
    kind: "answers",
    calledBy: ["open-asks: this session's asks", "open-asks: --project"],
  },
  { routes: ["GET /api/v1/inbox"], kind: "answers", calledBy: ["inbox: the person's Inbox"] },
  { routes: ["GET /api/v1/whoami"], kind: "caller now" },
  // Without `--as-of`: the graph sections of every read and each block read.
  { routes: ["GET /api/v1/references"], kind: "no history" },
  // Without `--as-of`: an issue read, the next three too.
  { routes: ["GET /api/v1/issues/{key}/references"], kind: "no history" },
  { routes: ["GET /api/v1/issues/{key}/coverage"], kind: "no history" },
  // Session liveness: an issue read where a session holds the claim or the route.
  { routes: ["GET /api/v1/agents"], kind: "no history" },
  // Without `--as-of`: the Inbox, the next one too.
  { routes: ["GET /api/v1/credential-requests"], kind: "no history" },
  { routes: ["GET /api/v1/me/replies"], kind: "no history" },
  { routes: ["GET /api/v1/search/coverage", "GET /api/v1/me/inbox"], kind: "no history" },
];

/** The seven read commands that take `--as-of` (spec R3). */
const READ_COMMANDS = ["answers", "doc-read", "inbox", "open-asks", "read", "results", "search"];

/** One (command, form) of the table: its name, `<command>: <form>`, and the argv that reaches it,
 *  `--as-of` left out. */
interface Row {
  readonly form: string;
  readonly argv: readonly string[];
}

const ROWS: readonly Row[] = [
  { form: "read: an issue a session claims and routes", argv: ["read", "--issue", ISSUE] },
  { form: "read: an issue named owner/repo#n", argv: ["read", "--issue", EXTERNAL] },
  {
    form: "read: an issue's log, with bodies its events cut",
    argv: ["read", "--ref", `dispatch://${ISSUE}/log`],
  },
  { form: "read: an issue's children", argv: ["read", "--ref", `dispatch://${ISSUE}/children`] },
  { form: "read: an issue's comments", argv: ["read", "--ref", `dispatch://${ISSUE}/comments`] },
  { form: "read: --message", argv: ["read", "--message", MESSAGE] },
  {
    form: "read: an issue's ask by its whole id",
    argv: ["read", "--ref", `dispatch://${ISSUE}/ask/${ASK}`],
  },
  {
    form: "read: an issue's ask by an id prefix",
    argv: ["read", "--ref", `dispatch://${ISSUE}/ask/${prefix(ASK)}`],
  },
  {
    form: "read: an issue's ask by its block id",
    argv: ["read", "--ref", `dispatch://${ISSUE}/ask/${ASK_BLOCK}`],
  },
  {
    form: "read: a project document's ask by its whole id",
    argv: ["read", "--ref", `dispatch://${PROJECT}/artifact/runbook-md/ask/${DOCUMENT_ASK}`],
  },
  {
    form: "read: a project document's ask by an id prefix",
    argv: [
      "read",
      "--ref",
      `dispatch://${PROJECT}/artifact/runbook-md/ask/${prefix(DOCUMENT_ASK)}`,
    ],
  },
  {
    form: "read: an issue's comment by its whole id",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${COMMENT}`],
  },
  {
    form: "read: an issue's comment by an id prefix",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${prefix(COMMENT)}`],
  },
  {
    form: "read: a project document's comment by its whole id",
    argv: [
      "read",
      "--ref",
      `dispatch://${PROJECT}/artifact/runbook-md/comment/${DOCUMENT_COMMENT}`,
    ],
  },
  {
    form: "read: a project document's comment by an id prefix",
    argv: [
      "read",
      "--ref",
      `dispatch://${PROJECT}/artifact/runbook-md/comment/${prefix(DOCUMENT_COMMENT)}`,
    ],
  },
  {
    form: "read: an issue's message",
    argv: ["read", "--ref", `dispatch://${ISSUE}/message/${MESSAGE}`],
  },
  {
    form: "read: a block of an issue document",
    argv: ["read", "--ref", `dispatch://${ISSUE}/spec#b-${SPEC_BLOCK.id}`],
  },
  {
    form: "read: a block of a project document",
    argv: ["read", "--ref", `dispatch://${PROJECT}/artifact/runbook-md#b-${RUNBOOK_BLOCK.id}`],
  },
  {
    form: "read: a part of an ask",
    argv: ["read", "--ref", `dispatch://${ISSUE}/ask/${ASK}#b-${ASK_OPTION}`],
  },
  { form: "read: --project", argv: ["read", "--project", PROJECT] },
  {
    form: "read: an issue document's summary",
    argv: ["read", "--issue", ISSUE, "--artifact", "design.md"],
  },
  {
    form: "read: a project document's summary",
    argv: ["read", "--project", PROJECT, "--artifact", "runbook.md"],
  },
  {
    form: "read: a picture at an issue address",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${ISSUE_PICTURE_COMMENT}`],
  },
  {
    form: "read: a picture at a project address",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${PROJECT_PICTURE_COMMENT}`],
  },
  {
    form: "read: a picture at an agent address",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${AGENT_PICTURE_COMMENT}`],
  },
  {
    form: "read: --history of a comment",
    argv: ["read", "--ref", `dispatch://${ISSUE}/comment/${COMMENT}`, "--history"],
  },
  {
    form: "read: --history of a message by its whole id",
    argv: ["read", "--ref", `dispatch://${ISSUE}/message/${MESSAGE}`, "--history"],
  },
  {
    form: "read: --history of a message by an id prefix",
    argv: ["read", "--ref", `dispatch://${ISSUE}/message/${prefix(MESSAGE)}`, "--history"],
  },
  { form: "doc-read: an issue's primary document", argv: ["doc-read", "--issue", ISSUE] },
  { form: "doc-read: an issue named owner/repo#n", argv: ["doc-read", "--issue", EXTERNAL] },
  { form: "doc-read: --blocks", argv: ["doc-read", "--issue", ISSUE, "--blocks"] },
  { form: "doc-read: --version", argv: ["doc-read", "--issue", ISSUE, "--version", "1"] },
  {
    form: "doc-read: an uploaded file",
    argv: ["doc-read", "--issue", ISSUE, "--artifact", "notes.txt"],
  },
  {
    form: "doc-read: an agent's upload",
    argv: ["doc-read", "--ref", `dispatch://agent/${AGENT_SESSION}/artifact/screen-png@v1`],
  },
  {
    form: "doc-read: a project document",
    argv: ["doc-read", "--project", PROJECT, "--artifact", "runbook.md"],
  },
  { form: "search: a query", argv: ["search", "--query", "astrolabe"] },
  { form: "answers: this session's answers", argv: ["answers"] },
  { form: "open-asks: this session's asks", argv: ["open-asks"] },
  { form: "open-asks: --project", argv: ["open-asks", "--project", PROJECT] },
  { form: "inbox: the person's Inbox", argv: ["inbox"] },
  { form: "results: an issue named owner/repo#n", argv: ["results", "--issue", EXTERNAL] },
];

/** A request's route as the table names it, its kind, and the values of its path's parameters. */
interface MatchedRoute {
  readonly route: string;
  readonly kind: Kind;
  readonly params: Readonly<Record<string, string>>;
}

/** The route of the table that `method` and `pathname` reach: of every route whose literal
 *  segments the path holds, the one with the most literal segments, so `/api/v1/asks/open` is
 *  not taken for `/api/v1/asks/{id}`. */
function matchRoute(method: string, pathname: string): MatchedRoute | undefined {
  const segments = pathname.split("/").slice(1).map(decodeURIComponent);
  let best: { readonly matched: MatchedRoute; readonly literals: number } | undefined;
  for (const { routes, kind } of TABLE) {
    for (const route of routes) {
      const [routeMethod, path = ""] = route.split(" ");
      const parts = path.split("/").slice(1);
      if (routeMethod !== method || parts.length !== segments.length) continue;
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
        best = { matched: { route, kind, params }, literals };
      }
    }
  }
  return best?.matched;
}

/** An RFC 3339 time as microseconds since the epoch, cut to microseconds as Dispatch cuts it, or
 *  undefined for any other text. */
function instant(text: string): number | undefined {
  const match =
    /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?([Zz]|[+-]\d{2}:\d{2})$/.exec(
      text
    );
  if (match === null) return undefined;
  const [, year, month, day, hour, minute, second, fraction = "", zone = "Z"] = match;
  const offset =
    zone.toUpperCase() === "Z"
      ? 0
      : (zone.startsWith("-") ? -1 : 1) * (Number(zone.slice(1, 3)) * 60 + Number(zone.slice(4)));
  const ms =
    Date.UTC(
      Number(year),
      Number(month) - 1,
      Number(day),
      Number(hour),
      Number(minute),
      Number(second)
    ) -
    offset * 60_000;
  return ms * 1000 + Number(fraction.padEnd(6, "0").slice(0, 6));
}

/** A time as `Dispatch-As-Of` names it: RFC 3339 in UTC with six fractional digits. */
function headerTime(microseconds: number): string {
  const seconds = new Date(Math.floor(microseconds / 1_000_000) * 1000).toISOString().slice(0, 19);
  return `${seconds}.${String(microseconds % 1_000_000).padStart(6, "0")}Z`;
}

function refusal(status: number, code: string, error: string): Response {
  return Response.json({ code, error }, { status });
}

const NOT_FOUND = () => refusal(404, "NOT_FOUND", "not found");

/** The answer of `found`, or Dispatch's 404 where nothing was found. */
function answer(found: unknown): Response {
  return found === undefined ? NOT_FOUND() : Response.json(found);
}

/** What Dispatch answers on `route` for the world above, at the present or, where `atACut`, as of
 *  a cut: the same answer, since nothing in it changed after the cut, but for what the records at
 *  the cut do not give of a document's blocks (`blocksAtTheCut`). */
function answerFor(
  route: string,
  params: Readonly<Record<string, string>>,
  query: URLSearchParams,
  atACut: boolean
): Response {
  const id = params.id ?? "";
  switch (route) {
    case "GET /api/v1/whoami":
      // A token that carries no cut: the command reads the past at `--as-of` alone.
      return Response.json(WHOAMI_NO_CUT);
    case "GET /api/v1/issues/resolve":
      return answer(query.get("ref") === EXTERNAL ? { key: ISSUE } : undefined);
    case "GET /api/v1/issues/{key}":
      return answer(params.key === ISSUE ? issueRead() : undefined);
    case "GET /api/v1/issues/{key}/events": {
      const after = Number(query.get("after") ?? 0);
      const limit = Number(query.get("limit") ?? 200);
      return Response.json(EVENTS.filter((event) => event.seq > after).slice(0, limit));
    }
    case "GET /api/v1/issues/{key}/comments":
      return Response.json(ISSUE_COMMENTS);
    case "GET /api/v1/artifacts/{id}/comments":
      return Response.json(id === RUNBOOK ? [PROJECT_COMMENT] : []);
    case "GET /api/v1/comments/{id}": {
      const found = COMMENTS.find((candidate) => candidate.id === id);
      return answer(found === undefined ? undefined : { comment: found, replies: [] });
    }
    case "GET /api/v1/comments/{id}/history":
      return answer(COMMENTS.some((candidate) => candidate.id === id) ? HISTORY : undefined);
    case "GET /api/v1/issues/{key}/messages":
      return Response.json([MESSAGE_READ]);
    case "GET /api/v1/issues/{key}/messages/{id}":
    case "GET /api/v1/messages/{id}":
      return answer(id === MESSAGE ? MESSAGE_READ : undefined);
    case "GET /api/v1/messages/{id}/history":
      return answer(id === MESSAGE ? HISTORY : undefined);
    case "GET /api/v1/issues/{key}/asks":
      return Response.json([ISSUE_ASK]);
    case "GET /api/v1/artifacts/{id}/asks":
      return Response.json(id === RUNBOOK ? [PROJECT_ASK] : []);
    case "GET /api/v1/asks/{id}": {
      const found = ASKS.find((candidate) => candidate.id === id);
      return answer(found === undefined ? undefined : { ...askReadBody(), ask: found });
    }
    case "GET /api/v1/issues/{key}/artifacts/{slug}":
      return answer(ISSUE_ARTIFACTS.find((candidate) => candidate.slug === params.slug));
    case "GET /api/v1/projects/{key}/artifacts":
      return Response.json(PROJECT_ARTIFACTS);
    case "GET /api/v1/projects/{key}/artifacts/{slug}":
      // The route takes a slug, then a filename.
      return answer(
        PROJECT_ARTIFACTS.find((candidate) => candidate.slug === params.slug) ??
          PROJECT_ARTIFACTS.find((candidate) => candidate.name === params.slug)
      );
    case "GET /api/v1/agents/{session_id}/artifacts/{slug}":
      return answer(
        params.session_id === AGENT_SESSION && params.slug === AGENT_UPLOAD.slug
          ? AGENT_UPLOAD
          : undefined
      );
    case "GET /api/v1/artifacts/{id}/text": {
      const document = DOCUMENTS.get(id);
      if (document === undefined) return NOT_FOUND();
      const text = { markdown: document.markdown, token: `sha256:${id}`, version: 1 };
      if (query.get("blocks") !== "true") return Response.json(text);
      if (!atACut) return Response.json({ ...text, blocks: document.blocks });
      const { blocks, absent } = blocksAtTheCut(document.blocks);
      const answered = Response.json({ ...text, blocks });
      answered.headers.set(DISPATCH_AS_OF_ABSENT_HEADER, absent.join(","));
      return answered;
    }
    case "GET /api/v1/artifacts/{id}/versions/{number}": {
      if (params.number !== "1") return NOT_FOUND();
      const document = DOCUMENTS.get(id);
      if (document !== undefined) {
        return Response.json({ ...versionBody(), markdown: document.markdown });
      }
      // A file's version is its stored bytes, under the MIME type it was stored with.
      if (id === NOTES) {
        return new Response("Notes.\n", { headers: { "Content-Type": "text/plain" } });
      }
      if (id === SHOT || id === DIAGRAM || id === SCREEN) {
        return new Response(PNG, { headers: { "Content-Type": "image/png" } });
      }
      return NOT_FOUND();
    }
    case "GET /api/v1/issues/{key}/artifacts/{slug}/blocks":
      return Response.json([SPEC_BLOCK]);
    case "GET /api/v1/projects/{key}/artifacts/{slug}/blocks":
      return Response.json([RUNBOOK_BLOCK]);
    case "GET /api/v1/issues/{key}/results":
      return Response.json(issueResultsBody());
    case "GET /api/v1/search":
    case "GET /api/v1/search/results":
      return Response.json(fakeSearchResponse([]));
    case "GET /api/v1/agents/{session_id}/answers":
      return Response.json({ session_id: params.session_id, since: AT, total: 0, items: [] });
    case "GET /api/v1/asks/open":
      return Response.json(OPEN_ASKS);
    case "GET /api/v1/inbox":
      return Response.json([]);
    case "GET /api/v1/references":
      return Response.json({
        node: { kind: "issue", id: query.get("to") ?? query.get("from") ?? "" },
        edges: [],
      });
    case "GET /api/v1/issues/{key}/references":
      return Response.json({ members: [], truncated: false });
    case "GET /api/v1/issues/{key}/coverage":
      return Response.json(coverageBody());
    case "GET /api/v1/agents":
      return Response.json([HOLDER_AGENT]);
    case "GET /api/v1/credential-requests":
      return Response.json({ pending: [] });
    case "GET /api/v1/me/replies":
      return Response.json({ rows: [], total: 0, limit: 0, offset: 0, as_of: AT });
  }
  return Response.json({ error: `the stand-in serves no answer on ${route}` }, { status: 500 });
}

/** One request the stand-in took. */
interface Call {
  /** The request as a failure names it: method, path and query. */
  readonly text: string;
  readonly method: string;
  /** The table's route it reached and that route's kind, or undefined where the table names
   *  none. */
  readonly route: string | undefined;
  readonly kind: Kind | undefined;
  /** Each `as_of` value its query sent. */
  readonly asOf: readonly string[];
}

/** Each run's requests, by the `Authorization` header of the bearer the run is handed: a request
 *  that a run leaves in flight when it ends goes to that run's list, never to the next run's. */
const callsByBearer = new Map<string, Call[]>();

/** Dispatch as the routes answer: each route's answer, and on a route of kind "answers" asked
 *  with `as_of` the `Dispatch-As-Of` header naming that cut. A route of another kind refuses
 *  `as_of`, and an `as_of` that is no time is refused, as Dispatch refuses them. */
const server = Bun.serve({
  port: 0,
  fetch(request) {
    const url = new URL(request.url);
    const calls = callsByBearer.get(request.headers.get("Authorization") ?? "");
    if (calls === undefined) return refusal(401, "UNAUTHORIZED", "the bearer is no run's");
    const matched = matchRoute(request.method, url.pathname);
    const asOf = url.searchParams.getAll("as_of");
    calls.push({
      text: `${request.method} ${url.pathname}${url.search}`,
      method: request.method,
      route: matched?.route,
      kind: matched?.kind,
      asOf,
    });
    if (matched === undefined) {
      return Response.json(
        { error: `the stand-in serves no route for ${request.method} ${url.pathname}` },
        { status: 500 }
      );
    }
    if (asOf.length === 0) {
      return answerFor(matched.route, matched.params, url.searchParams, false);
    }
    if (matched.kind !== "answers") {
      return refusal(
        400,
        DISPATCH_AS_OF_NOT_SERVED,
        `as_of: ${matched.route} holds no history; it answers the present only`
      );
    }
    const cut = asOf.length === 1 ? instant(asOf[0] ?? "") : undefined;
    if (cut === undefined) {
      return refusal(400, DISPATCH_INVALID_AS_OF, `as_of: ${asOf.join(", ")} is not one time`);
    }
    const answered = answerFor(matched.route, matched.params, url.searchParams, true);
    answered.headers.set(DISPATCH_AS_OF_HEADER, headerTime(cut));
    return answered;
  },
});

afterAll(() => server.stop(true));

/** What one run of the command did: its exit, its output and the requests it made, in order. */
interface Run {
  readonly code: number;
  readonly output: string;
  readonly calls: readonly Call[];
}

/** Runs `dispatch <argv>` as a fresh process of the host session would, against the stand-in. */
async function run(argv: readonly string[]): Promise<Run> {
  const bearer = fixtureSecret("dsp");
  const calls: Call[] = [];
  callsByBearer.set(`Bearer ${bearer}`, calls);
  resetAdviceMemory();
  forgetShownPictures(SESSION);
  const stateDir = makeDirectory();
  let output = "";
  const write = (text: string) => {
    output += text;
  };
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
    { stdout: write, stderr: write, readText: () => "", cwd: stateDir }
  );
  return { code, output, calls: [...calls] };
}

/** The first lines of a run's output, on one line. */
function said(output: string): string {
  return output.trim().split("\n").slice(0, 4).join(" / ");
}

/** The routes the table says `row`'s form calls in `ran`, a run with `--as-of` where `atACut`,
 *  that `ran` did not call. */
function missedRoutes(row: Row, ran: Run, atACut: boolean): string[] {
  return TABLE.filter(
    (entry) =>
      entry.calledBy?.includes(row.form) === true ||
      (atACut ? entry.calledAtACutBy : entry.calledAtThePresentBy)?.includes(row.form) === true
  )
    .filter((entry) => !ran.calls.some((call) => entry.routes.includes(call.route ?? "")))
    .map((entry) => `${entry.routes.join(" or ")} was not called, so the form was not reached`);
}

/** The routes the table says `row`'s form calls only in the other run, with `--as-of` or without
 *  it, that `ran`, a run with `--as-of` where `atACut`, called. */
function strayRoutes(row: Row, ran: Run, atACut: boolean): string[] {
  return TABLE.filter(
    (entry) =>
      (atACut ? entry.calledAtThePresentBy : entry.calledAtACutBy)?.includes(row.form) === true
  )
    .filter((entry) => ran.calls.some((call) => entry.routes.includes(call.route ?? "")))
    .map(
      (entry) =>
        `${entry.routes.join(" or ")} was called, which the form calls ${atACut ? "without" : "with"} --as-of only`
    );
}

/** Why one call of a run under `--as-of` breaks the plan's table, or undefined for none. */
function callProblem(call: Call): string | undefined {
  if (call.method !== "GET") return `${call.text}: a read command wrote`;
  if (call.kind === undefined) {
    return `${call.text}: the plan's table names no such route, so it holds no history, and it was called`;
  }
  if (call.kind === "no history") return `${call.text}: a route of kind "no history" was called`;
  if (call.kind === "caller now") {
    return call.asOf.length === 0
      ? undefined
      : `${call.text}: whoami answers the caller now, and it refuses as_of`;
  }
  const [sent, ...more] = call.asOf;
  if (sent === undefined) {
    return `${call.text}: a route of kind "answers" was called without as_of=${CUT}`;
  }
  if (more.length > 0) return `${call.text}: as_of was sent ${call.asOf.length} times`;
  if (instant(sent) !== instant(CUT)) {
    return `${call.text}: as_of=${sent} is not the cut ${CUT}`;
  }
  return undefined;
}

/** What the run of `row` without `--as-of` shows of the stand-in: that it answers enough for the
 *  command to reach the form. */
function presentProblems(row: Row, ran: Run): string[] {
  return [
    ...(ran.code === 0 ? [] : [`exit ${ran.code}: ${said(ran.output)}`]),
    ...missedRoutes(row, ran, false),
    ...strayRoutes(row, ran, false),
  ].map((problem) => `without --as-of: ${problem}`);
}

/** What the run of `row` with `--as-of` breaks: each call named. */
function asOfProblems(row: Row, ran: Run): string[] {
  if (ran.calls.length === 0 && ran.code !== 0) {
    return [
      `dispatch ${[...row.argv, "--as-of", CUT].join(" ")} made no call and exited ${ran.code}: ${said(ran.output)}`,
    ];
  }
  const [first] = ran.calls;
  return [
    ...(first?.route === "GET /api/v1/whoami"
      ? []
      : [
          first === undefined
            ? "no call was made, so GET /api/v1/whoami was not read first"
            : `the first call was ${first.text}, not GET /api/v1/whoami`,
        ]),
    ...ran.calls.flatMap((call) => callProblem(call) ?? []),
    ...missedRoutes(row, ran, true),
    ...strayRoutes(row, ran, true),
    ...(ran.code === 0 ? [] : [`exit ${ran.code}: ${said(ran.output)}`]),
  ];
}

describe("each read command's calls under --as-of", () => {
  test("the rows are the (command, form) pairs the plan's table names, one row each", () => {
    const named = [
      ...new Set(
        TABLE.flatMap((entry) => [
          ...(entry.calledBy ?? []),
          ...(entry.calledAtACutBy ?? []),
          ...(entry.calledAtThePresentBy ?? []),
        ])
      ),
    ].sort();
    expect(ROWS).toHaveLength(named.length);
    expect(ROWS.map((row) => row.form).sort()).toEqual(named);
    expect(ROWS.filter((row) => !row.form.startsWith(`${row.argv[0]}: `))).toEqual([]);
    expect([...new Set(ROWS.map((row) => row.argv[0]))].sort()).toEqual(READ_COMMANDS);
  });

  for (const row of ROWS) {
    test(row.form, async () => {
      const present = presentProblems(row, await run(row.argv));
      const past = asOfProblems(row, await run([...row.argv, "--as-of", CUT]));
      expect([...present, ...past]).toEqual([]);
    });
  }
});
