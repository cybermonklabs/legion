/**
 * The constants of Dispatch's tool contract that are not a schema: the issue statuses, the ask
 * urgencies, the block units and the two limits a caller is held to, the kinds a search ranks,
 * the step and plan writes' codes and forms, and the artifact command's rule wordings, with the
 * small functions over them.
 *
 * Why they are a module of their own, beside `dispatch-tools.ts` rather than inside it. That
 * module builds the tool schemas at its top level, and a bundler may drop none of them: a value
 * is used, so the module is kept and the schemas are kept with it, zod and all. The dashboard
 * reads the statuses, the urgencies, the block units and the two limits and validates nothing,
 * and it was paying for the schema builder on every page load to have them
 * (dispatch://INFRA-1109, R13). A constant a reader needs must not drag a schema builder into a
 * browser, so the constants live here and `dispatch-tools.ts` re-exports them: an import of that
 * module, or of the package index, answers what it answered before and two names more,
 * `NAME_EXTENSION` and `MARKDOWN_EXTENSION`, which `dispatch-tools.ts` held unexported. A new constant of
 * the tool contract goes here too, rather than beside the schemas, whether or not a browser
 * reads it. What still sits in `dispatch-tools.ts`, `DISPATCH_ITEM_BLOCKS` and `SEARCH_KIND_DEPTH`
 * among them, has no reader that is not taking the schemas anyway, and each moves here the first
 * time a reader needs it without them.
 *
 * The dashboard takes a value of this module through its own subpath,
 * `@legion/contracts/dispatch-tools-constants`, never through `@legion/contracts/dispatch-tools`
 * or the package index: either would bring the schema modules with the value.
 */

// The route table is read for a type alone, which the compiler erases, so it builds no edge a
// bundler would follow; the API's constants module builds no schema either.
import type { paths } from "./dispatch-api.generated";
import { SEARCH_RESULT_KINDS } from "./dispatch-api-constants";

/** Ask urgency levels the Dispatch server accepts, in ascending order. */
export const ASK_URGENCIES = ["low", "med", "high", "blocking"] as const;

/**
 * Longest one block of a comment, comment edit, message, broadcast or ask part may be, in UTF-16
 * code units as the Dispatch server counts them. Generated into Go as
 * `contracts.DispatchBlockUnits`, which the server enforces and names in its refusal, and which
 * it publishes through `GET /api/v1/limits`, the route `dispatch limits` prints.
 */
export const DISPATCH_BLOCK_UNITS = 32000;

/**
 * Longest body an event carries whole, in UTF-16 code units, and the bound the Dispatch server
 * holds an ask's question and its options to together. Generated into Go as
 * `contracts.DispatchEventBodyMax`, which the server enforces and names in its refusal, and
 * which it publishes through `GET /api/v1/limits`, the route `dispatch limits` prints.
 */
export const DISPATCH_EVENT_BODY_MAX = 32000;

/**
 * Longest `dispatch_search` query (`GET /api/v1/search`'s `q`, trimmed), in UTF-16 units.
 * Generated into Go as `contracts.SearchQueryMax`, which the server enforces.
 *
 * The URL budget: `dispatch search` sends its query and filters in the URL (`q` beside `kind`,
 * `project`, `limit` and `offset`), and the load balancer in front of Dispatch answers a path and
 * query over 16,369 bytes with a bare `414` before the server runs. This limit and
 * `PROJECT_KEY_PATTERN` (at most 10 characters) keep the largest call the tool accepts at a
 * 9,147-byte path and query, since percent-encoding costs at most 9 bytes a unit: a call carrying
 * `kind` goes to `GET /api/v1/search/results`, 8 bytes longer, and repeats `&kind=` at most
 * `SEARCH_KINDS.length` (5) times, at most 14 bytes each, over the 9,069 bytes a call without it
 * takes. Raising either, or adding a parameter, means redoing that sum.
 */
export const SEARCH_QUERY_MAX = 1000;

/** One value of the repeated `kind` parameter, read off the route table itself rather than
 *  written out a second time: the parameter is optional, so both the parameter object and
 *  `kind` are unwrapped before the array's element is taken. It is read from
 *  `GET /api/v1/search/results`, the route that declares every member the handler reads;
 *  `GET /api/v1/search` reads `kind` too and declares it nowhere, as it does the other five a
 *  build released before them does not serve (`SEARCH_MEMBERS_EVERY_BUILD_READS`). */
type SearchKindParameter = NonNullable<
  NonNullable<paths["/api/v1/search/results"]["get"]["parameters"]["query"]>["kind"]
>[number];

/**
 * The kinds `GET /api/v1/search` ranks, which its repeated `kind` parameter names: the five the
 * search index holds, in the order the answer's fusion lists them. A party, a component or any
 * other node is read from its own route, so the search refuses a kind outside these rather than
 * answering an empty page.
 *
 * It is the list a result's own kind is read as (`SEARCH_RESULT_KINDS` in
 * `dispatch-api-constants.ts`), named once: what the route ranks and what it answers with are one
 * vocabulary. The `satisfies` is the route table's own, so a kind the CLI offers that the route
 * would refuse does not compile.
 */
export const SEARCH_KINDS = SEARCH_RESULT_KINDS satisfies readonly SearchKindParameter[];

/** Issue lifecycle statuses the Dispatch server accepts (`model.IssueStatuses`), in lifecycle order. */
export const ISSUE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

export function isIssueStatus(value: string): value is IssueStatus {
  return (ISSUE_STATUSES as readonly string[]).includes(value);
}

export type IssueStatus = (typeof ISSUE_STATUSES)[number];

/** A whole project key, as the Dispatch server creates them, generated into Go as
 *  `contracts.ProjectKeyPattern` (and held by the `projects.key` check constraint). */
export const PROJECT_KEY_PATTERN = /^[A-Z][A-Z0-9]{1,9}$/;

export const PATH_KEEPS_BYTES =
  "path (--path <file>), which stores a file whose path does not end in .md or .markdown as its bytes, unchanged";

/**
 * What `dispatch artifact` says of a name the owner already holds as a document, when the upload
 * is a document too. The command refuses it before it uploads anything, because an upload of that
 * name writes the document's next version and replaces its text. `force` is what asks for that
 * version. The refusal is the command's; the API's own guard is the document token, which
 * Dispatch checks under the write's lock where a replacement names it, the Dispatch page's own
 * upload among them. A replacement naming none is taken unchecked for now and refused 400
 * PRECONDITION_REQUIRED in a later release, so the rule names both. A name held as a file or a
 * picture is refused by fileNameExistsRule, and an upload of another kind than the name holds by
 * artifactKindConflictRule, both below.
 */
export const ARTIFACT_NAME_EXISTS_RULE =
  'An upload under the name of a document the owner already holds is refused: it writes that document\'s next version, over the text it holds. Change a document with dispatch doc-edit. To write the next version, send the upload again with force (--force) and the token of the version it replaces (--precondition-json \'{"document":"sha256:…"}\', the token dispatch doc-read prints).';

/** An artifact's kind as a refusal names it: `doc` is a document and `image` a picture; any other
 *  kind a later Dispatch states is named as it is. */
function artifactKindWord(kind: string): string {
  return { doc: "document", file: "file", image: "picture" }[kind] ?? kind;
}

/**
 * What `dispatch artifact` says of a name the owner holds as a file or a picture, when the upload
 * is of that kind too. A file holds no text dispatch doc-edit changes, so its one way to a next
 * version is the upload. The rule prints no token: a conditional write names the version its
 * writer read, so the writer sends the token its own `dispatch doc-read` printed, and a writer
 * whose read is stale is refused 409 rather than handed the newest token to overwrite it with.
 * Where the standing version states no token (`statesToken` false), an upload naming one is
 * refused - by a Dispatch from before file tokens 400 INVALID_PRECONDITION, and for a version
 * stored with no hash 409 PRECONDITION_FAILED - so the rule names `--force` alone, which both take.
 */
export function fileNameExistsRule(kind: string, statesToken: boolean): string {
  const what = artifactKindWord(kind);
  const head = `An upload under the name of a ${what} the owner already holds is refused: it writes that ${what}'s next version, over the bytes it holds.`;
  if (!statesToken) {
    return `${head} The version standing under the name states no token, so an upload naming one is refused: to write the next version, read the ${what} again with dispatch doc-read, then send the upload again with force (--force) alone.`;
  }
  return `${head} To write the next version, send the upload again with force (--force) and the token of the version you read: --precondition-json '{"document":"<the Document token your own dispatch doc-read of the ${what} printed>"}'. A version written since that read refuses it 409 PRECONDITION_FAILED: read the ${what} again before you replace it.`;
}

/**
 * What `dispatch artifact` says of a name the owner holds as one kind of artifact when the upload
 * is of another, decided as Dispatch decides it: a declared Markdown upload is a document, bytes
 * that are a PNG, JPEG, GIF or WebP are a picture, and anything else is a file. Dispatch refuses
 * such an upload under that name whatever it carries, `--force` and a token included (400
 * ARTIFACT_KIND_MISMATCH, "give it another name"), so the rule names another name and nothing
 * else.
 */
export function artifactKindConflictRule(held: string, upload: string): string {
  return `This upload is a ${artifactKindWord(upload)}, and the name holds a ${artifactKindWord(held)}: Dispatch keeps one kind under one name, so this upload cannot be its next version, with force or without. Give the upload another name.`;
}

/**
 * What `dispatch artifact` says of inline text the store would change. The command renders the
 * text as Dispatch renders a document it stores and compares the two, so a text stored under
 * another spelling (a `*` list stored with `-`, an ordered list renumbered, a CR LF stored as a
 * line feed) is refused before the upload rather than stored changed. A typed block's attributes
 * in another order are the one spelling it stores rather than refuses, since an attribute's
 * position carries no meaning. The rule holds for both kinds of name: `--path` keeps the bytes of
 * a name that is not Markdown, and a Markdown name is parsed and rendered whichever flag sends
 * it, so its one way out is to send the stored text.
 */
export const ARTIFACT_STORED_TEXT_RULE = `content and content-file hold text Dispatch stores as Markdown, and a text the store would write under another spelling is refused. A typed block whose attributes differ from Dispatch's only in their order is not refused: the command uploads it with them in Dispatch's order and names each such block. A refusal names the first difference: a changed line by the attribute or the word that differs, a word of more than 200 characters by a window from its first character that differs, and a line the store adds or drops by a quote cut at 200 characters, each cut named. The refusal prints the text Dispatch stores: send that text. A name that does not end in .md or .markdown has a second way out, which keeps the text as it was written: send it as a file with ${PATH_KEEPS_BYTES}.`;

/** A name's extension, or undefined for a name with none: what follows its last dot when that
 *  holds no blank. Digits count, so "rows.jsonl.1" has one, and so does a title such as
 *  "plan v1.2", which takes content once it ends in .md. */
export const NAME_EXTENSION = /\.([^\s.]+)$/u;

export const MARKDOWN_EXTENSION = /^(md|markdown)$/i;

/**
 * Whether `--path` would store this name's text as the bytes it was written as: true for every
 * name that does not end in .md or .markdown, a name with no extension included. Dispatch reads
 * an upload's media type, which a Markdown path alone makes `text/markdown`, and parses and
 * renders only a document (`PATH_KEEPS_BYTES`). A refusal of inline text the store would change
 * names `--path` for such a name, and never for a Markdown one, which `--path` would change the
 * same way.
 */
export function pathKeepsBytesOf(name: string): boolean {
  const extension = NAME_EXTENSION.exec(name.trim())?.[1];
  return extension === undefined || !MARKDOWN_EXTENSION.test(extension);
}

/** The words of a step record, written once for Dispatch's store and the Legion daemon, a Go module
 *  of its own that reads them through `github.com/sjawhar/envoy/steprecord`. A word one side drifts
 *  on is a `400 STEP_INPUT` the daemon's outbox ends the row on, and the record is lost.
 *
 *  The kinds of a run: a step a plan names, at its place from 1, and the retro and the merge,
 *  acts of Legion that no plan names, at the place 0. Generated into Go as
 *  `contracts.StepKindStep`, `contracts.StepKindRetro` and `contracts.StepKindMerge`. */
export const STEP_KINDS = ["step", "retro", "merge"] as const;

/** The results of a run that ended: generated into Go as `contracts.StepResultPassed`,
 *  `contracts.StepResultFailed` and `contracts.StepResultStopped`. A run that still runs has none. */
export const STEP_RESULTS = ["passed", "failed", "stopped"] as const;

/** Where a step of an issue's plan stands in the steps read (`PlannedStep.state`), from the records
 *  of the plan's own run of the issue: not started before any run of it, running while its newest
 *  run has no end, then that run's result (`STEP_RESULTS`). Generated into Go as
 *  `contracts.PlannedStepNotStarted` and `contracts.PlannedStepRunning`, which Dispatch's steps read
 *  writes; the OpenAPI document names them as the state's enum, which the web client switches on. */
export const PLANNED_STEP_STATES = ["not started", "running", ...STEP_RESULTS] as const;

/** The codes of the step write's refusals that no retry changes: a value outside its form, a writer
 *  that is not the daemon's session, and an id whose record is a run of another issue. Generated
 *  into Go as `contracts.StepInputCode`, `contracts.StepWriterCode` and
 *  `contracts.StepOtherIssueCode`, which the route answers and the daemon ends a row on. */
export const STEP_INPUT_CODE = "STEP_INPUT";
export const STEP_WRITER_CODE = "STEP_WRITER";
export const STEP_OTHER_ISSUE_CODE = "STEP_OTHER_ISSUE";

/** The codes of the plan write's refusals that no retry changes: a value outside its form, and an
 *  id whose plan is the plan of another issue. A writer that is not the daemon's session is refused
 *  `STEP_WRITER`, as a step write is. Generated into Go as `contracts.PlanInputCode` and
 *  `contracts.PlanOtherIssueCode`, which the route answers and the daemon ends a row on. */
export const PLAN_INPUT_CODE = "PLAN_INPUT";
export const PLAN_OTHER_ISSUE_CODE = "PLAN_OTHER_ISSUE";

/** The form of the agent, the session and the model a step record names: printable ASCII with no
 *  space, from 1 to 128 characters. The session and the model come from the agent's own frames,
 *  and `dispatch steps` joins a record's fields with spaces, so a space or a control character in
 *  one would shift its columns or forge a line. Generated into Go as
 *  `contracts.StepAgentTextPattern`: Dispatch refuses a step write that names text outside it,
 *  and the Legion daemon leaves such text out of the record it writes. */
export const STEP_AGENT_TEXT_PATTERN = /^[!-~]{1,128}$/;

/** STEP_AGENT_TEXT_PATTERN's rule in words, which Dispatch's refusal gives: generated into Go as
 *  `contracts.StepAgentTextRule`. */
export const STEP_AGENT_TEXT_RULE_WORDS = "printable ASCII with no space, from 1 to 128 characters";

/** The most bytes the id of a task a step record names holds. A task id is ASCII, so its bytes and
 *  its characters are one count. Generated into Go as `contracts.StepTaskMax`. */
export const STEP_TASK_MAX = 64;

/** The form of the id of a task a step record names, which is also the id of a `plan_task` block
 *  of an owner's plan: lowercase letters, digits and hyphens, the first a letter or a digit, at
 *  most `STEP_TASK_MAX` bytes. Generated into Go as `contracts.StepTaskPattern`: Dispatch refuses
 *  a step write and a plan write that names a task outside it, and the Legion daemon, through
 *  `github.com/sjawhar/envoy/steprecord`, refuses a plan whose task block has such an id, whose
 *  steps Dispatch could record none of. */
export const STEP_TASK_PATTERN = new RegExp(`^[a-z0-9][a-z0-9-]{0,${STEP_TASK_MAX - 1}}$`);

/** STEP_TASK_PATTERN's rule in words, which the refusal of either side gives: generated into Go as
 *  `contracts.StepTaskRule`. */
export const STEP_TASK_RULE_WORDS = `lowercase letters, digits and hyphens, starting with a letter or a digit, at most ${STEP_TASK_MAX} bytes`;
