import { expect, type Locator, type Page, type Request } from "@playwright/test";

import { type Body, routeUrl } from "../web/src/api/paths";
import type { Actor, IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { APPROVAL_STATE_WORDS, LENS_WORDS } from "../web/src/features/issue-page/page-words";
import PART_R_WORDS from "../web/src/features/issue-page/page-words-r";
import { mergeTreePages } from "../web/src/features/issue-page/tree-read/tree-nodes";
import {
  answerAsk,
  baseUrl,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  e2eAgentToken,
  editArtifact,
  recordCheckResult,
  reviewArtifact,
  setGatedSteps,
  userHeaders,
} from "./api";
import { countDocumentSockets } from "./editor";
import { awaitBlockAsk } from "./issue-layout-seed-support";
import { specBlockRef } from "./issue-layout-seed-text";
import { gotoIssue, issuePage, issueRead } from "./issue-page";
import { DESKTOP } from "./issue-page-g";
import { approvalChip } from "./issue-page-p";
import { type Gate, OWED_BY } from "./issue-page-t-tree";
import { focusedRowReadId, requestedTreeRead } from "./issue-page-w";
import { sql } from "./psql";
import { settledState, settledValue } from "./settled";

/**
 * What the rows of `e2e/issue-page-r.e2e.ts` seed, gate on and read: Graph's node menu, its moves,
 * its delete, its new rows and Retype, and the link of a reference to a node a delete took
 * (INFRA-5705, part R of the new issue page).
 *
 * - **The words** are `page-words-r.ts`'s (`EDIT_LABELS`, `EDIT_LINES`), with part W's Lens act
 *   and the page's shared tables. Neither this file nor the rows import a module of
 *   `web/src/features/issue-page/graph-edit/`, so both load before that code exists.
 * - **The seed** `seedGraphEdit` writes the Verification preamble's seed through the server's own
 *   routes, in the order `api/graph_edit_web_fixtures_test.go` writes the fixture the unit cases
 *   read: a converted issue Legion runs (label `legion`, its architect role held by
 *   `ARCHITECT`), a person's question and a criterion under it, three statements under the
 *   criterion (the second the architect's), a check serving all three under the first with one
 *   recorded result, the architect's check under the second, a plan task, a person's comment on
 *   the third statement's text (which part T draws in the margin beside it, not as a row), a
 *   person's comment on the issue itself, tied to no text (which Graph draws as a row, and which
 *   the Go fixture does not write: no unit case reads it), a person's message and the architect's
 *   reply, the architect's decision, and a prose block of the issue's own fragment placed under
 *   the criterion after the statements; a child, a view of the spec, whose check serves the first
 *   statement; and a second issue the conversion has not reached, created while a live process
 *   that lists no issue room stands beside the server, as the Go tests' `createDigestIssue` makes
 *   one. `graphEditIds` names each node by the id the server's tree read gives it, never one this
 *   file builds.
 * - **The gates**, each a `Gate` read by `owedUnless` (`e2e/issue-page-t-tree.ts`), built as
 *   `convertedGate` is there: `deleteRouteGate` (INFRA-1253's `DELETE` of a node, in
 *   `GET /api/v1`), `roomWriteGate` (part Y's editor writing through the issue's one room),
 *   `proseRowGate` (part T's prose draft step drawing the seeded prose block as a row) and
 *   `criterionEditorGate` (part Y's criterion row and its block editor).
 * - **The readers**: a row by its tree read id (`data-node-read-id`), the landing on it by
 *   `?node=`, the menu's control (`button[data-row-menu]`, `RowMenu.tsx`'s `ROW_MENU_ATTRIBUTE`,
 *   named `EDIT_LABELS.edit_menu`), the menu (`role="menu"`, its entries `role="menuitem"`, a grey
 *   one `aria-disabled="true"` with its reason inside the menu) and its submenu, the confirm
 *   (`role="dialog"` named `EDIT_LINES.delete_confirm_title`), a refusal's line
 *   (`role="status"` inside the row), each request the page sends to the node routes with its
 *   `If-Match`, body and answer (`watchNodeRequests`), the caret's row, the tree read after a
 *   reload, and whether the menus fit the screen with no sideways scroll
 *   (`document.documentElement.scrollWidth <= window.innerWidth`).
 */

// ---------------------------------------------------------------------------------------------
// The words.
// ---------------------------------------------------------------------------------------------

/** Part R's controls: the menu's entries, Retype's kinds, the delete's count and its confirm. */
export const EDIT_LABELS = PART_R_WORDS.add.CONTROL_LABELS;

/** Part R's lines: each grey entry's reason, the confirm's words and each refusal's line. */
export const EDIT_LINES = PART_R_WORDS.add.ANSWER_LINES;

/** The menu's entries in R1's order, Lens, part W's act, last. */
export const MENU_ENTRIES = [
  EDIT_LABELS.edit_new_below,
  EDIT_LABELS.edit_new_under,
  EDIT_LABELS.edit_retype,
  EDIT_LABELS.edit_indent,
  EDIT_LABELS.edit_outdent,
  EDIT_LABELS.edit_move_up,
  EDIT_LABELS.edit_move_down,
  EDIT_LABELS.edit_delete,
  LENS_WORDS.act,
] as const;

/** The four moves, which a node with no place, a second row and a node of another issue grey. */
export const MOVE_ENTRIES = [
  EDIT_LABELS.edit_indent,
  EDIT_LABELS.edit_outdent,
  EDIT_LABELS.edit_move_up,
  EDIT_LABELS.edit_move_down,
] as const;

/** The count and its unit, "4 rows" or "1 row", as Delete and the confirm name them. */
function countWords(count: number): string {
  return `${count} ${count === 1 ? EDIT_LABELS.delete_row : EDIT_LABELS.delete_rows}`;
}

/** What Delete reads once its dry run answered `count`: "Delete 4 rows", "Delete 1 row". */
export function deleteWords(count: number): string {
  return `${EDIT_LABELS.edit_delete} ${countWords(count)}`;
}

/** What the confirm's body reads for a delete that takes `count` nodes. */
export function confirmWords(count: number): string {
  return `${EDIT_LINES.confirm_takes} ${countWords(count)}`;
}

// ---------------------------------------------------------------------------------------------
// The seed.
// ---------------------------------------------------------------------------------------------

/** The project of every seeded issue. */
const GRAPH_PROJECT = "GEDIT";

/** The agent session that holds the converted issue's architect role, which R28's writer check
 *  lets write a statement on an issue Legion runs (`refuseSpecWriterOfNode`). */
const ARCHITECT = "graph-edit-architect";

const architectActor: Actor = { id: ARCHITECT, kind: "session" };

/** A write as the architect: the shared agent token naming the architect's session. */
export const asArchitect = { actor: architectActor, as: "agent" as const };

/** The session the Legion daemon of the project writes as, the one actor the role-holders route
 *  takes a publication from. */
const daemonActor: Actor = { id: `legion-daemon:${GRAPH_PROJECT}`, kind: "session" };

/** The commit the shared check's one result is recorded at: the issue names no repository, so
 *  the server reads no branch for it. */
const RESULT_COMMIT = "1".repeat(40);

/** The words of the seed, each unique in its store, so a node is found by them in the tree read. */
export const SEED_WORDS = {
  agentProse: "The agent's prose block under the third statement.",
  answer: "Indent, outdent, the two moves and the delete.",
  checkAgent: "A move names the placement it read, or stores nothing.",
  checkShared: "Every edit of the graph is one write.",
  childCheck: "The child checks the first statement.",
  /** A person's comment on the child, which the c-retype row writes: a prose block of the
   *  child's own fragment typed under it stands under a comment, so Retype's criterion is grey
   *  there for the view alone. */
  childComment: "Which kind may the child's own paragraph take?",
  /** The paragraph the c-retype row types into the child's own fragment, under that comment. */
  childParagraph: "The child's design paragraph.",
  /** The child's spec: one paragraph of a role document, which the tree read leaves out
   *  (INFRA-1253 R2), so it is no node of the child's tree read. */
  childSpec: "The child views the spec of the issue above it.",
  comment: "Does a refused move store nothing either?",
  criterion: "The reader edits the graph by keyboard alone.",
  decision: "The graph keeps one write for each move.",
  /** The person's comment on the issue itself, tied to no text: a post, which Graph draws as a
   *  row (Verification preamble). */
  issueComment: "Which acts does a comment on the issue take?",
  memo: "The memo the graph keeps first.",
  message: "The menu ships with the moves first.",
  planTask: "Build the node menu of Graph.",
  prose: "The prose block the seed places under the criterion.",
  question: "Which edits does the graph take?",
  reading: "The agent reads each move as one write.",
  reply: "The moves are planned first.",
  statement1: "An indent is one write of the node's place.",
  statement2: "A move names the placement it read.",
  statement3: "A refused edit stores nothing at all.",
  statement3Quote: "stores nothing",
  unconvertedCriterion: "The unconverted issue's criterion.",
  unconvertedStatement: "The unconverted issue's statement.",
} as const;

/** What `seedGraphEdit` wrote, by the ids the server answered. */
export interface GraphEditSeed {
  /** The child of the converted issue, a view of its spec. */
  readonly child: string;
  /** The person's comment on the third statement's text. */
  readonly comment: string;
  readonly key: string;
  readonly spec: string;
  /** The issue's tree artifact: its own fragment, which a prose block is written in. */
  readonly tree: string;
  /** The issue the conversion has not reached. */
  readonly unconverted: string;
  readonly verification: string;
}

/** A member of a JSON answer, read with no shape assumed: undefined where `value` holds none. */
export function member(value: unknown, name: string): unknown {
  return value !== null && typeof value === "object" && name in value
    ? Reflect.get(value, name)
    : undefined;
}

/** A check block `id` holding `words` that serves each statement address of `serves`. */
function checkBlock(id: string, words: string, serves: readonly string[]): string {
  return (
    `:::check{#${id} serves="${serves.join(" ")}" surface="api" run="go test ./graph" ` +
    `negative_control="Drop the rule: the test fails."}\n${words}\n:::\n`
  );
}

/** One `insert` of `markdown` after `after` (`end`, or `block:<id>`) into the document `id`, as
 *  alice or as the architect. */
export function insertInto(
  id: string,
  after: string,
  markdown: string,
  options: Parameters<typeof editArtifact>[2] = {}
) {
  return editArtifact(id, { ops: [{ after, markdown, op: "insert" }] }, options);
}

/** Issue `key`'s tree read with no view, every page of it merged in order, read as alice, as
 *  `e2e/issue-page-w.ts`'s `requestedTreeRead` reads it through a page. */
export async function treeOf(key: string): Promise<IssueTreeRead> {
  const pageAt = async (cursor: string | null): Promise<IssueTreeRead> => {
    const path = routeUrl("/api/v1/issues/{key}/tree", { key }, { cursor });
    const response = await fetch(new URL(path, baseUrl), { headers: await userHeaders("alice") });
    if (!response.ok) {
      throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
    }
    return (await response.json()) as IssueTreeRead;
  };
  const first = await pageAt(null);
  const rest: IssueTreeRead[] = [];
  let cursor = first.cursor;
  while (cursor !== null) {
    const next = await pageAt(cursor);
    rest.push(next);
    cursor = next.cursor;
  }
  return mergeTreePages([first, ...rest]);
}

/** The first node of `answer` that `holds` is true of, failing with `what` where none is. */
export function nodeWhere(
  answer: IssueTreeRead,
  what: string,
  holds: (node: IssueTreeNode) => boolean
): IssueTreeNode {
  const found = (answer.nodes ?? []).find(holds);
  if (found === undefined) throw new Error(`the tree read of ${answer.issue} holds no ${what}`);
  return found;
}

/** The block `blockId` of supertag `supertag` that belongs to `answer`'s own issue. */
function ownBlock(answer: IssueTreeRead, blockId: string, supertag: string): IssueTreeNode {
  return nodeWhere(
    answer,
    `${supertag} block ${blockId}`,
    (node) =>
      node.block_id === blockId &&
      node.supertag === supertag &&
      (node.issue === undefined || node.issue === answer.issue) &&
      node.role !== "context"
  );
}

/** Whether `node` is a prose block (a block node that opens no typed block) holding `words`. */
function isProse(node: IssueTreeNode, words: string): boolean {
  return node.kind === "block" && node.supertag === "block" && node.text.includes(words);
}

/** How long the seed waits for the block index to give a block written into a room its rank. */
const RANK_WAIT_MS = 60_000;

/** Waits until issue `key`'s tree read holds a prose block holding `words` with a rank and a
 *  `placement_seq`, which a block written into a room holds once the block index has read it:
 *  the `PATCH` refuses a node with no rank (`400 INVALID_POINTER`). */
export async function rankedProse(key: string, words: string): Promise<IssueTreeNode> {
  const ranked = (node: IssueTreeNode) =>
    isProse(node, words) && node.rank !== null && typeof node.placement_seq === "number";
  await expect
    .poll(async () => (await treeOf(key)).nodes?.some(ranked) === true, {
      message: `${key}'s tree read holds no ranked prose block "${words}"`,
      timeout: RANK_WAIT_MS,
    })
    .toBe(true);
  return nodeWhere(await treeOf(key), `ranked prose block "${words}"`, ranked);
}

/** A caller of the node routes from outside the page: alice, or the architect's session. */
export type NodeCaller = "alice" | "architect";

/** The headers a write of `caller` sends: alice's sign-in, or the shared agent token. */
async function callerHeaders(caller: NodeCaller): Promise<Record<string, string>> {
  return caller === "alice"
    ? { ...(await userHeaders("alice")), "Content-Type": "application/json" }
    : { Authorization: `Bearer ${e2eAgentToken}`, "Content-Type": "application/json" };
}

/** What one request of a node route answered: its status and its body. */
export interface NodeAnswer {
  readonly body: unknown;
  readonly status: number;
}

/** `PATCH /api/v1/issues/{key}/nodes/{id}` as `caller`, with `If-Match` the quoted
 *  `placementSeq`: the move the server takes from a person and from an agent alike. */
export async function moveNode(
  key: string,
  id: string,
  placementSeq: number,
  move: { readonly after?: string | null; readonly parent?: string },
  caller: NodeCaller = "alice"
): Promise<NodeAnswer> {
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}", { id, key });
  const body: Body<"/api/v1/issues/{key}/nodes/{id}", "patch"> =
    caller === "alice" ? { ...move } : { ...move, actor: architectActor };
  const response = await fetch(new URL(path, baseUrl), {
    body: JSON.stringify(body),
    headers: { ...(await callerHeaders(caller)), "If-Match": `"${placementSeq}"` },
    method: "PATCH",
  });
  return { body: await response.json().catch(() => null), status: response.status };
}

/** `DELETE /api/v1/issues/{key}/nodes/{id}` as `caller`: its dry run, which sends no `If-Match`,
 *  where `confirm` is undefined, else the delete with `expected_count` and `If-Match`. */
export async function deleteNode(
  key: string,
  id: string,
  caller: NodeCaller,
  confirm?: { readonly expected: number; readonly placementSeq: number }
): Promise<NodeAnswer> {
  const query = confirm === undefined ? { dry_run: true } : { expected_count: confirm.expected };
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}", { id, key }, query);
  const headers = await callerHeaders(caller);
  const response = await fetch(new URL(path, baseUrl), {
    body: caller === "alice" ? undefined : JSON.stringify({ actor: architectActor }),
    headers:
      confirm === undefined ? headers : { ...headers, "If-Match": `"${confirm.placementSeq}"` },
    method: "DELETE",
  });
  return { body: await response.json().catch(() => null), status: response.status };
}

/** The count a dry run, or a `409 DELETE_COUNT_MOVED`, answered, failing with `what` where it
 *  answered none. */
export function countOf(answer: NodeAnswer | undefined, what: string): number {
  const count = member(answer?.body, "count");
  if (typeof count !== "number") {
    throw new Error(`${what} answered ${answer?.status} ${JSON.stringify(answer?.body)}, no count`);
  }
  return count;
}

/** Publishes the architect as the holder of issue `key`'s architect role, as the Legion daemon of
 *  the project does (`PUT /api/v1/issues/{key}/role-holders` on the deployment's shared token). */
async function publishArchitect(key: string): Promise<void> {
  const path = routeUrl("/api/v1/issues/{key}/role-holders", { key });
  const body: Body<"/api/v1/issues/{key}/role-holders", "put"> = {
    actor: daemonActor,
    holders: [{ role: "architect", session: ARCHITECT }],
  };
  const response = await fetch(new URL(path, baseUrl), {
    body: JSON.stringify(body),
    headers: { Authorization: `Bearer ${e2eAgentToken}`, "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`PUT ${path} failed: ${response.status} ${await response.text()}`);
  }
}

/** Creates an issue of `spec` while a live process that lists no issue room stands beside the
 *  server, so the issue is born unconverted (`fleetServesIssueRoom` in `api/issue_create.go`): a
 *  copy of the newest server's row of `server_processes` under an instance of its own, with no
 *  capabilities, taken back once the issue is written, as `holdIssuesUnconverted` does in the Go
 *  tests and `e2e/acceptance/tree-read-views.sh` does for its issue C. */
async function createUnconvertedIssue(title: string, spec: string): Promise<string> {
  // A SELECT over the insert, so psql prints the instance alone and no `INSERT 0 1` tag after it.
  const held = await sql(
    `with held as (
       insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at)
       select gen_random_uuid(), signing_keys, schema_version, schema_creatable, now() + interval '1 hour'
       from server_processes order by heartbeat_at desc limit 1
       returning instance::text
     )
     select instance from held`
  );
  expect(held, "no live process to stand a release before beside").toMatch(/^[0-9a-f-]{36}$/u);
  try {
    return (await createIssue({ force: true, project: GRAPH_PROJECT, spec, title })).key;
  } finally {
    await sql(`delete from server_processes where instance = '${held}'::uuid`);
  }
}

/** The Verification preamble's seed, written through the server's own routes (this file's head
 *  comment). */
export async function seedGraphEdit(): Promise<GraphEditSeed> {
  await createProject({ key: GRAPH_PROJECT, name: "Graph editing" });
  const issue = await createIssue({
    force: true,
    labels: ["legion"],
    project: GRAPH_PROJECT,
    spec:
      `:::callout{#memo kind="note" title="Memo"}\n${SEED_WORDS.memo}\n:::\n\n` +
      `:::ask{#q1}\n${SEED_WORDS.question}\n:::\n`,
    title: "Graph editing's seed",
  });
  const key = issue.key;
  const spec = issue.primary_artifact_id ?? "";
  const askId = await awaitBlockAsk(key, "q1");
  await answerAsk(askId, { expected_edited_at: null, selected: [], text: SEED_WORDS.answer });
  const statement = (block: string) => specBlockRef(key, block);
  const criterion = statement("k1");
  await insertInto(
    spec,
    "end",
    `:::criterion{#k1 parent="dispatch://${key}/ask/${askId}"}\n${SEED_WORDS.criterion}\n:::\n\n` +
      `:::requirement{#s1 serves="${criterion}"}\n${SEED_WORDS.statement1}\n:::\n\n` +
      `:::requirement{#s3 serves="${criterion}"}\n${SEED_WORDS.statement3}\n:::\n`
  );
  await publishArchitect(key);
  await insertInto(
    spec,
    "block:s1",
    `:::requirement{#s2 serves="${criterion}"}\n${SEED_WORDS.statement2}\n:::\n`,
    asArchitect
  );
  await insertInto(
    spec,
    "end",
    `::::decision{#d1}\n${SEED_WORDS.decision}\n\n:::reading{#g1}\n${SEED_WORDS.reading}\n:::\n::::\n`,
    asArchitect
  );
  const verification = await createIssueArtifact(key, {
    content: checkBlock("c1", SEED_WORDS.checkShared, ["s1", "s2", "s3"].map(statement)),
    name: "verification.md",
  });
  await insertInto(
    verification.artifact.id,
    "end",
    checkBlock("c2", SEED_WORDS.checkAgent, [statement("s2")]),
    asArchitect
  );
  await recordCheckResult(
    key,
    {
      check: "c1",
      commit: RESULT_COMMIT,
      negative_control: "Broke what the check proves and the run failed.",
      role: "implementer",
      verdict: "pass",
    },
    {}
  );
  await createIssueArtifact(key, {
    content: `:::plan_task{#t1 serves="${statement("s1")}"}\n${SEED_WORDS.planTask}\n:::\n`,
    name: "plan.md",
  });
  const comment = await createComment(key, {
    anchor: { artifact: "spec", quote: SEED_WORDS.statement3Quote },
    body: SEED_WORDS.comment,
  });
  await createComment(key, { body: SEED_WORDS.issueComment });
  const message = await createMessage(key, { body: SEED_WORDS.message });
  await createMessage(key, { body: SEED_WORDS.reply, in_reply_to: message.id }, asArchitect);

  // The prose block: a paragraph at the end of the issue's own fragment, which stands under the
  // issue, moved under the criterion by alice. Naming the criterion alone places it after the
  // statements, since the fragment's band stands after the spec's (INFRA-1253 R7).
  const tree = (await treeOf(key)).tree ?? "";
  expect(tree, `${key} is not born converted: its tree read names no tree artifact`).not.toBe("");
  await insertInto(tree, "end", `${SEED_WORDS.prose}\n`);
  const prose = await rankedProse(key, SEED_WORDS.prose);
  const placed = await moveNode(key, prose.id, prose.placement_seq ?? 0, {
    parent: ownBlock(await treeOf(key), "k1", "criterion").id,
  });
  expect(placed.status, `the seed's move of its prose block: ${JSON.stringify(placed.body)}`).toBe(
    200
  );

  const child = await createIssue({
    force: true,
    parent: key,
    project: GRAPH_PROJECT,
    spec: `${SEED_WORDS.childSpec}\n`,
    title: "The child, a view of the spec",
  });
  await createIssueArtifact(child.key, {
    content: checkBlock("cc1", SEED_WORDS.childCheck, [statement("s1")]),
    name: "verification.md",
  });
  const unconverted = await createUnconvertedIssue(
    "The unconverted issue",
    `:::criterion{#uk1}\n${SEED_WORDS.unconvertedCriterion}\n:::\n\n` +
      `:::requirement{#us1}\n${SEED_WORDS.unconvertedStatement}\n:::\n`
  );
  expect(
    (await treeOf(unconverted)).converted,
    `${unconverted} was born converted beside a process that lists no issue room`
  ).toBe(false);
  return {
    child: child.key,
    comment: comment.id,
    key,
    spec,
    tree,
    unconverted,
    verification: verification.artifact.id,
  };
}

/** The person approves the spec and the Verification document, which a row does first where it
 *  reads an approval as changed since (the Verification preamble). */
export async function approveDocuments(seed: GraphEditSeed): Promise<void> {
  await reviewArtifact(seed.spec, { state: "approved" });
  await reviewArtifact(seed.verification, { state: "approved" });
}

/** Gates the design steps of the seed's project, the Verification document's (`criteria`) and
 *  the spec's, as a deployment that gates them publishes them (`PUT .../gated-steps`): the seed's
 *  issue is a root of the project, so its header draws the approval of each of the two documents
 *  (`gated-steps.ts`' `gatedStepEntries`), the Verification document's among them, which the
 *  header of a project that gates nothing leaves out. Called before the page loads the issue, so
 *  its first read of the project holds the steps. */
export async function gateDesignSteps(): Promise<void> {
  const gated = await setGatedSteps(GRAPH_PROJECT, ["criteria", "spec"]);
  expect(gated.steps, "the seed's project gates no design step").toEqual(["criteria", "spec"]);
}

/** The tree read id of each node of the seed a row names. */
export interface GraphEditIds {
  readonly checkAgent: string;
  readonly checkShared: string;
  /** The child's check, which stands under the first statement as a node of another issue. */
  readonly childCheck: string;
  /** The child's own paragraph, a prose block of the child's own fragment, in the child's tree
   *  read; undefined until a reader types one there, since the child's spec is a role document
   *  whose prose the tree read leaves out (INFRA-1253 R2) and the seed writes none. */
  readonly childParagraph: string | undefined;
  /** The person's comment on the third statement's text, which part T draws in the margin beside
   *  that statement, not as a row. */
  readonly comment: string;
  readonly criterion: string;
  readonly decision: string;
  readonly issue: string;
  /** The person's comment on the issue itself, tied to no text, which Graph draws as a row. */
  readonly issueComment: string;
  readonly message: string;
  readonly planTask: string;
  readonly prose: string;
  /** The person's question, the ask node the criterion stands under, and the read id of the
   *  question's row: Graph draws the ask and its source block of the spec in one row, read as
   *  the ask, the first of the two the tree read's walk reaches. */
  readonly question: string;
  readonly reply: string;
  readonly result: string;
  /** The first, second and third statements, in the spec's order. */
  readonly statements: readonly [string, string, string];
  /** The unconverted issue's statement, in its own tree read. */
  readonly unconvertedStatement: string;
}

/** The read id of every node of the seed, from Graph's read of each issue (the tree with no view
 *  and no kind), read through the signed-in page's own request context. */
export async function graphEditIds(page: Page, seed: GraphEditSeed): Promise<GraphEditIds> {
  const [answer, child, unconverted] = await Promise.all([
    requestedTreeRead(page, seed.key),
    requestedTreeRead(page, seed.child),
    requestedTreeRead(page, seed.unconverted),
  ]);
  const criterion = ownBlock(answer, "k1", "criterion");
  const checkShared = ownBlock(answer, "c1", "check");
  const statement3 = ownBlock(answer, "s3", "requirement");
  const message = nodeWhere(
    answer,
    "person's message",
    (node) => node.kind === "message" && node.parent?.kind === "issue"
  );
  return {
    checkAgent: ownBlock(answer, "c2", "check").id,
    checkShared: checkShared.id,
    childCheck: nodeWhere(
      answer,
      "child's check",
      (node) => node.block_id === "cc1" && node.supertag === "check" && node.issue === seed.child
    ).id,
    childParagraph: (child.nodes ?? []).find((node) => isProse(node, SEED_WORDS.childParagraph))
      ?.id,
    comment: nodeWhere(
      answer,
      "comment on the third statement",
      (node) => node.kind === "comment" && node.parent?.id === statement3.id
    ).id,
    criterion: criterion.id,
    decision: ownBlock(answer, "d1", "decision").id,
    issue: nodeWhere(
      answer,
      "issue node",
      (node) => node.kind === "issue" && node.id === answer.issue
    ).id,
    issueComment: nodeWhere(
      answer,
      "person's comment on the issue",
      (node) => node.kind === "comment" && node.parent?.kind === "issue"
    ).id,
    message: message.id,
    planTask: ownBlock(answer, "t1", "plan_task").id,
    prose: nodeWhere(answer, "seeded prose block", (node) => isProse(node, SEED_WORDS.prose)).id,
    question: nodeWhere(
      answer,
      "person's question",
      (node) => node.kind === "ask" && node.id === criterion.parent?.id
    ).id,
    reply: nodeWhere(
      answer,
      "architect's reply",
      (node) => node.kind === "message" && node.parent?.id === message.id
    ).id,
    result: nodeWhere(
      answer,
      "shared check's result",
      (node) => node.kind === "result" && node.parent?.id === checkShared.id
    ).id,
    statements: [
      ownBlock(answer, "s1", "requirement").id,
      ownBlock(answer, "s2", "requirement").id,
      statement3.id,
    ],
    unconvertedStatement: ownBlock(unconverted, "us1", "requirement").id,
  };
}

/** The ids of the nodes standing directly under `parent` in `answer`, in the read's order: the
 *  parent's conversation nodes first, in the order they were written, then its ranked children,
 *  then its results and its unranked blocks. */
export function childIds(answer: IssueTreeRead, parent: string): string[] {
  return (answer.nodes ?? []).flatMap((node) =>
    node.parent?.id === parent && node.role !== "context" ? [node.id] : []
  );
}

/** The node `id` of `answer`, or undefined where the read holds none. */
export function nodeOf(answer: IssueTreeRead, id: string): IssueTreeNode | undefined {
  return (answer.nodes ?? []).find((node) => node.id === id);
}

/** Whether `answer` holds a tuple of kind `kind` from `from` to `to`. */
export function holdsTuple(
  answer: IssueTreeRead,
  kind: NonNullable<IssueTreeRead["tuples"]>[number]["kind"],
  from: string,
  to: string
): boolean {
  return (answer.tuples ?? []).some(
    (tuple) => tuple.kind === kind && tuple.from.id === from && tuple.to.id === to
  );
}

/** Whether `node` names alice, the person signed in, among its authors. */
export function writtenByAlice(node: IssueTreeNode | undefined): boolean {
  return (node?.author ?? []).some((author) => author.kind === "user" && author.id === "alice");
}

// ---------------------------------------------------------------------------------------------
// The gates.
// ---------------------------------------------------------------------------------------------

/** The route INFRA-1253's delete of a node serves, as `GET /api/v1` lists it. */
const NODE_ROUTE = "/api/v1/issues/{key}/nodes/{id}";

/** INFRA-1253's `DELETE /api/v1/issues/{key}/nodes/{id}` and its dry run: the server's own index,
 *  `GET /api/v1`, lists the route. Read through the signed-in page's own request context. */
export async function deleteRouteGate(page: Page): Promise<Gate> {
  const response = await page.request.get("/api/v1");
  expect(response.ok(), `GET /api/v1 answers ${response.status()}`).toBe(true);
  const routes = member(await response.json(), "routes");
  return {
    met:
      Array.isArray(routes) &&
      routes.some(
        (route: unknown) =>
          member(route, "method") === "DELETE" && member(route, "path") === NODE_ROUTE
      ),
    owed:
      "owed until INFRA-1253's DELETE /api/v1/issues/{key}/nodes/{id} is on this branch's " +
      `base (deleteRouteGate): GET /api/v1 lists no such route here; ${OWED_BY}`,
  };
}

/**
 * Part Y's editor, which writes through the issue's one room (Y's branch holding T's):
 * on Graph, Enter on the third statement's row puts the caret in an editor on that row whose
 * document socket is the statement's own fragment of the room - its document's
 * (`/ws/doc/<spec>`), or the tree artifact's for a block no document holds - read from the
 * statement's own `document` in the tree read, as `criterionEditorGate` reads the criterion's.
 * The probe uses a separate signed-in page, so the row's own page, its watches and its listeners
 * stay untouched. The gate is owed only where the base draws the converted statement without its
 * edit cue, read once from the row Graph committed: where the row takes a caret, an Enter that
 * puts none in it, or an editor that opens no socket on that fragment, fails the row - each
 * polled until it holds or its wait ends, never read once at a moment that may come too early.
 */
export async function roomWriteGate(page: Page, key: string): Promise<Gate> {
  const answer = await requestedTreeRead(page, key);
  const statement = ownBlock(answer, "s3", "requirement");
  const artifact = statement.document?.artifact_id ?? answer.tree;
  expect(artifact, "the seeded statement has a room fragment").toBeTruthy();
  const room = `/ws/doc/${artifact}`;
  const probe = await page.context().newPage();
  try {
    const roomSockets = countDocumentSockets(probe, artifact ?? "");
    await probe.setViewportSize(DESKTOP);
    await gotoIssue(probe, presetPath(key, "graph"));
    const row = readRow(probe, statement.id);
    await expect(row.first(), "Graph drew no third statement row").toBeVisible();
    // One read is enough: a statement of a document carries its edit cue from the commit that
    // first draws its row, and the room's facts keep it when they come (`nodeEditable`).
    const editable = await row.evaluateAll(([one]) => one?.hasAttribute("data-editable") ?? null);
    if (editable !== true) {
      return {
        met: false,
        owed:
          "owed until part Y's editor writes through the issue's one room on this branch's base " +
          "(roomWriteGate): Graph draws the converted statement without its edit cue here; " +
          OWED_BY,
      };
    }
    await focusOn(row.first(), "the third statement");
    await probe.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(probe), {
        message: "Enter does not put the caret in the third statement",
      })
      .toBe(statement.id);
    await expect
      .poll(roomSockets, {
        message: `the statement's editor opens no socket on ${room}, its fragment in the issue room`,
      })
      .toBeGreaterThan(0);
    return { met: true, owed: "" };
  } finally {
    await probe.close();
  }
}

/**
 * Part T's prose draft step, which draws a prose block of the issue's own fragment as a row:
 * Graph, opened at 1440 px where it draws every row at once, draws the seeded prose block once it
 * draws the third statement, whose row the same read brings. The row's own size is set back after.
 */
export async function proseRowGate(page: Page, key: string): Promise<Gate> {
  const answer = await requestedTreeRead(page, key);
  const prose = nodeWhere(answer, "seeded prose block", (node) => isProse(node, SEED_WORDS.prose));
  const statement = ownBlock(answer, "s3", "requirement");
  const size = page.viewportSize() ?? DESKTOP;
  await page.setViewportSize(DESKTOP);
  await gotoIssue(page, presetPath(key, "graph"));
  await expect(
    readRow(page, statement.id).first(),
    "Graph drew no third statement row"
  ).toBeVisible();
  // The third statement's row is visible, so Graph has already committed every row from this read.
  const drawn = (await readRow(page, prose.id).count()) > 0;
  await page.setViewportSize(size);
  return {
    met: drawn,
    owed:
      "owed until part T's prose draft step is on this branch's base (proseRowGate): Graph " +
      `draws no row for the seeded prose block here; ${OWED_BY}`,
  };
}

/**
 * Graph draws the seeded criterion and opens its block editor. The probe uses a separate
 * signed-in page so the test's menu and request watches stay untouched.
 * At desktop width the third statement and criterion come from the same committed tree read.
 * On a converted issue, a socket to the criterion's document reaches the issue's shared room
 * for that document's fragment. A criterion with no document uses the tree artifact instead.
 * If the row takes a caret, a failed editor open is a test failure, not an owed skip.
 */
export async function criterionEditorGate(page: Page, key: string): Promise<Gate> {
  const answer = await requestedTreeRead(page, key);
  const criterion = ownBlock(answer, "k1", "criterion");
  const statement = ownBlock(answer, "s3", "requirement");
  const artifact = criterion.document?.artifact_id ?? answer.tree;
  expect(artifact, "the seeded criterion has a room fragment").toBeTruthy();
  const room = `/ws/doc/${artifact}`;
  const owed = (gate: string, here: string): Gate => ({
    met: false,
    owed: `owed until ${gate} is on this branch's base (criterionEditorGate): ${here}; ${OWED_BY}`,
  });
  const probe = await page.context().newPage();
  try {
    const roomSockets = countDocumentSockets(probe, artifact ?? "");
    await probe.setViewportSize(DESKTOP);
    await gotoIssue(probe, presetPath(key, "graph"));
    await expect(
      readRow(probe, statement.id).first(),
      "Graph drew no third statement row"
    ).toBeVisible();
    const criterionRow = readRow(probe, criterion.id);
    // One read is enough: the visible third statement and the criterion come from the same
    // committed tree read, and a block of a document carries its edit cue from that commit.
    const editable = await criterionRow.evaluateAll(
      ([row]) => row?.hasAttribute("data-editable") ?? null
    );
    if (editable === null) {
      return owed("part Y's criterion row", "Graph draws no row for the seeded criterion here");
    }
    if (!editable) {
      return owed("part Y's block editor", "Graph draws the seeded criterion without its edit cue");
    }
    await focusOn(criterionRow.first(), "the seeded criterion");
    await probe.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(probe), {
        message: "Enter does not put the caret in the seeded criterion",
      })
      .toBe(criterion.id);
    await expect
      .poll(roomSockets, {
        message: `the criterion's editor opens no socket on ${room}, its fragment in the issue room`,
      })
      .toBeGreaterThan(0);
    return { met: true, owed: "" };
  } finally {
    await probe.close();
  }
}

// ---------------------------------------------------------------------------------------------
// The rows and the menu.
// ---------------------------------------------------------------------------------------------

/** Every row the page draws for the node the tree read names `readId` (`data-node-read-id`): a
 *  check under several statements is drawn once under each. */
export function readRow(page: Page, readId: string): Locator {
  expect(readId, `${readId} cannot be written into a selector`).not.toContain('"');
  return issuePage(page).locator(`[data-node-row][data-node-read-id="${readId}"]`);
}

/** The row that holds the caret: the innermost row around the focused element. */
export function caretRow(page: Page): Locator {
  return issuePage(page).locator("[data-node-row]:has(:focus)").last();
}

/** The read id of the innermost row around a focused editable element, or null where no caret is
 *  in a row, or the row it is in holds no read id yet, as a new row before its node is read. */
export function caretRowReadId(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const focused = document.activeElement;
    if (!(focused instanceof HTMLElement) || !focused.isContentEditable) return null;
    return focused.closest("[data-node-row]")?.getAttribute("data-node-read-id") ?? null;
  });
}

/** Whether a focused editable element stands inside a row: the caret is in a row's text. */
export function caretInARow(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const focused = document.activeElement;
    return (
      focused instanceof HTMLElement &&
      focused.isContentEditable &&
      focused.closest("[data-node-row]") !== null
    );
  });
}

/**
 * Opens issue `key` at the node `readId`'s own address (`?node=` on Graph's, or on `path`) and
 * waits until its landing has put the focus on that row, at any width: below 1024 px the outline
 * draws one level under the node a landing roots it at, so a row reached by its address is drawn
 * there too. The landing takes the focus back whenever nothing holds it until the reader's first
 * act (`IssueTree.tsx`), so this acts as a reader with a Shift, which no key of the page binds.
 * Answers the row the landing focused, its node's first row, which is the row of its own place.
 */
export async function landOn(
  page: Page,
  key: string,
  readId: string,
  what: string,
  path = presetPath(key, "graph")
): Promise<Locator> {
  await gotoIssue(page, `${path}?node=${encodeURIComponent(readId)}`);
  await expect
    .poll(() => focusedRowReadId(page), {
      message: `the address of ${what} does not land on its row`,
      timeout: 30_000,
    })
    .toBe(readId);
  await page.keyboard.press("Shift");
  return readRow(page, readId).first();
}

/** Puts the focus on the row `row` locates and waits until it holds it. */
export async function focusOn(row: Locator, what: string): Promise<Locator> {
  await row.focus();
  await expect(row, `${what}'s row takes no focus`).toBeFocused();
  return row;
}

/** The read id of the row the first row of `readId` stands under, by the tree's own
 *  `aria-level`; null at the top level and while the page draws no row of it. */
export function parentReadIdOf(page: Page, readId: string): Promise<string | null> {
  return issuePage(page).evaluate((root, id) => {
    const rows = [...root.querySelectorAll("[data-node-row]")];
    const at = rows.findIndex((row) => row.getAttribute("data-node-read-id") === id);
    if (at === -1) return null;
    const level = Number(rows[at]?.getAttribute("aria-level") ?? "");
    const parent = rows
      .slice(0, at)
      .reverse()
      .find((row) => Number(row.getAttribute("aria-level") ?? "") === level - 1);
    return parent?.getAttribute("data-node-read-id") ?? null;
  }, readId);
}

/** The index, among the rows the page draws for `child`, of the one drawn directly under a row of
 *  `parent` by the tree's own `aria-level`, or -1 where none is. */
function indexUnder(page: Page, parent: string, child: string): Promise<number> {
  return issuePage(page).evaluate(
    (root, ids) => {
      const rows = [...root.querySelectorAll("[data-node-row]")];
      return rows
        .filter((row) => row.getAttribute("data-node-read-id") === ids.child)
        .findIndex((row) => {
          const level = Number(row.getAttribute("aria-level") ?? "");
          const above = rows
            .slice(0, rows.indexOf(row))
            .reverse()
            .find((before) => Number(before.getAttribute("aria-level") ?? "") === level - 1);
          return above?.getAttribute("data-node-read-id") === ids.parent;
        });
    },
    { child, parent }
  );
}

/** The row of `child` drawn directly under a row of `parent`: a check's second row under another
 *  statement its `serves` names (part T's second row of one node). */
export async function rowUnder(
  page: Page,
  parent: string,
  child: string,
  what: string
): Promise<Locator> {
  const at = await settledValue(`${what}: its row under its parent`, () =>
    indexUnder(page, parent, child)
  );
  expect(at, `Graph draws no row of ${what} under its parent`).toBeGreaterThanOrEqual(0);
  return readRow(page, child).nth(at);
}

/** The focused row's control that opens its node menu (`RowMenu.tsx`'s `ROW_MENU_ATTRIBUTE`). */
export function menuControl(page: Page): Locator {
  return issuePage(page).locator("[data-node-row] button[data-row-menu]").first();
}

/** The open node menu: the first menu drawn inside a row. */
export function rowMenu(page: Page): Locator {
  return issuePage(page).locator('[data-node-row] [role="menu"]').first();
}

/** The open submenu, Retype's or Lens's: the second menu drawn inside a row. */
export function rowSubmenu(page: Page): Locator {
  return issuePage(page).locator('[data-node-row] [role="menu"]').nth(1);
}

/** Matches a text that starts with `word`, whatever space stands before it. */
function startingWith(word: string): RegExp {
  return new RegExp(`^\\s*${word.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&")}`, "u");
}

/** The entry of `menu` whose text starts with `word`. */
function entryIn(menu: Locator, word: string): Locator {
  return menu
    .getByRole("menuitem")
    .filter({ hasText: startingWith(word) })
    .first();
}

/** The entry of the open menu whose text starts with `word`. */
export function menuEntry(page: Page, word: string): Locator {
  return entryIn(rowMenu(page), word);
}

/** The entry of the open submenu whose text starts with `word`. */
export function submenuEntry(page: Page, word: string): Locator {
  return entryIn(rowSubmenu(page), word);
}

/** The words of each entry of the menu `which` (0 the menu, 1 its submenu) in its order, each
 *  entry of a menu inside it left out; empty while no such menu is open. */
function entryWords(page: Page, which: 0 | 1): Promise<string[]> {
  return issuePage(page).evaluate((root, at) => {
    const menu = root.querySelectorAll('[data-node-row] [role="menu"]')[at];
    return [...(menu?.querySelectorAll('[role="menuitem"]') ?? [])]
      .filter((entry) => entry.closest('[role="menu"]') === menu)
      .map((entry) => (entry.textContent ?? "").replace(/\s+/gu, " ").trim());
  }, which);
}

/** The words of each entry of the open menu, in its order. */
export function menuWords(page: Page): Promise<string[]> {
  return entryWords(page, 0);
}

/** The words of each entry of the open submenu, in its order. */
export function submenuWords(page: Page): Promise<string[]> {
  return entryWords(page, 1);
}

/**
 * Opens the node menu of the focused row, by a press on its control or by `m`, and waits until
 * the menu is drawn with the focus on its first entry. `message` names a failure of the opening:
 * `m opens the node menu` where a row reads that key.
 */
export async function openRowMenu(
  page: Page,
  how: "m" | "press",
  what: string,
  message = `the node menu of ${what} does not open`
): Promise<void> {
  if (how === "press") {
    await expect(menuControl(page), `${what}'s row draws no menu control`).toHaveText(
      EDIT_LABELS.edit_menu
    );
    await menuControl(page).click();
  } else {
    await page.keyboard.press("m");
  }
  await expect(rowMenu(page), message).toBeVisible();
  await expect(
    rowMenu(page).getByRole("menuitem").first(),
    `the node menu of ${what} puts the focus on no entry`
  ).toBeFocused();
}

/** Closes the open menu with Escape and waits until it is gone. */
export async function closeRowMenu(page: Page, what: string): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(rowMenu(page), `Escape leaves the menu of ${what} open`).toHaveCount(0);
}

/** Fails with `message` unless the open menu's entry `word` is grey, with `reason` in the menu. */
export async function expectGrey(
  page: Page,
  word: string,
  reason: string,
  message: string
): Promise<void> {
  await expect(menuEntry(page, word), message).toHaveAttribute("aria-disabled", "true");
  await expect(rowMenu(page), message).toContainText(reason);
}

/** Fails with `message` unless the open menu's entry `word` is drawn and not grey. */
export async function expectOffered(page: Page, word: string, message: string): Promise<void> {
  await expect(menuEntry(page, word), message).toBeVisible();
  await expect(menuEntry(page, word), message).not.toHaveAttribute("aria-disabled", "true");
}

/** Picks the open menu's entry `word` from the keyboard, as a grey entry is reached: the focus on
 *  it and Enter. A press is no way to a grey entry, which Playwright takes for a disabled one. */
export async function pickByKey(page: Page, word: string): Promise<void> {
  await menuEntry(page, word).focus();
  await page.keyboard.press("Enter");
}

/** The state Delete has settled on once its dry run answered: its count (`counted`), or grey with
 *  the refusal's or the failed read's words (`grey`); never the counting it reads meanwhile. */
export function deleteState(page: Page, what: string): Promise<"counted" | "grey"> {
  const entry = menuEntry(page, EDIT_LABELS.edit_delete);
  return settledState(`Delete on ${what}`, [
    [
      "grey",
      entry
        .and(page.locator('[aria-disabled="true"]'))
        .filter({ hasNotText: EDIT_LABELS.delete_counting }),
    ],
    ["counted", entry.and(page.locator(':not([aria-disabled="true"])'))],
  ]);
}

/** The confirm a pick of Delete opens (`features/project/ConfirmDialog.tsx`). */
export function confirmDialog(page: Page): Locator {
  return page.getByRole("dialog", { name: EDIT_LINES.delete_confirm_title });
}

/** The confirm's control that sends the delete. */
export function confirmDelete(page: Page): Locator {
  return confirmDialog(page).getByRole("button", { exact: true, name: EDIT_LABELS.confirm_delete });
}

/** The confirm's Cancel, which sends nothing. */
export function cancelDelete(page: Page): Locator {
  return confirmDialog(page).getByRole("button", { exact: true, name: "Cancel" });
}

/** The line a row draws in answer to a refused write (`role="status"` inside the row). */
export function refusalLine(row: Locator): Locator {
  return row.locator('[role="status"]').first();
}

/** How many open menus stand past either edge of the viewport, and whether the page scrolls
 *  sideways (`document.documentElement.scrollWidth > window.innerWidth`). */
function fitOf(
  page: Page
): Promise<{ readonly menusPastAnEdge: number; readonly sideways: boolean }> {
  return page.evaluate(() => {
    const width = window.innerWidth;
    const menus = [...document.querySelectorAll('[data-node-row] [role="menu"]')];
    return {
      menusPastAnEdge: menus.filter((menu) => {
        const box = menu.getBoundingClientRect();
        return box.left < 0 || box.right > width;
      }).length,
      sideways: document.documentElement.scrollWidth > width,
    };
  });
}

/** Fails with `message` unless every open menu fits the screen and the page has no sideways
 *  scroll, read until the page has laid the menus out. */
export async function expectMenusFit(page: Page, message: string): Promise<void> {
  await expect
    .poll(() => fitOf(page), { message })
    .toEqual({ menusPastAnEdge: 0, sideways: false });
}

// ---------------------------------------------------------------------------------------------
// What the page sent and what the server keeps.
// ---------------------------------------------------------------------------------------------

/** One request the page sent to a node route of the issue, with its answer. */
export interface NodeRequest {
  /** The answer, once it came; undefined where none came. */
  readonly answer: Promise<NodeAnswer | undefined>;
  readonly body: unknown;
  readonly dryRun: boolean;
  /** `expected_count` as sent, or null. */
  readonly expected: string | null;
  /** The node's address, the route's `{id}`, decoded. */
  readonly id: string;
  readonly ifMatch: string | null;
  readonly method: string;
  /** What the path names after the node's `{id}`: empty for the node route itself, and, say,
   *  `/parent` for a route that moves a node another way. */
  readonly rest: string;
}

/** The node requests that write: a move, and a delete that is no dry run. */
export function nodeWrites(requests: readonly NodeRequest[]): NodeRequest[] {
  return requests.filter((request) => request.method !== "GET" && !request.dryRun);
}

/** The dry runs among `requests`. */
export function dryRuns(requests: readonly NodeRequest[]): NodeRequest[] {
  return requests.filter((request) => request.method === "DELETE" && request.dryRun);
}

/** What `request`'s answer carries, or undefined where none came. */
async function answerOf(request: Request): Promise<NodeAnswer | undefined> {
  const response = await request.response();
  if (response === null) return undefined;
  return { body: await response.json().catch(() => null), status: response.status() };
}

/** Keeps each request the page sends from now on to a route under issue `key`'s
 *  `/api/v1/issues/{key}/nodes/`, in the order sent. */
export function watchNodeRequests(page: Page, key: string): NodeRequest[] {
  const prefix = `/api/v1/issues/${encodeURIComponent(key)}/nodes/`;
  const sent: NodeRequest[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (!url.pathname.startsWith(prefix)) return;
    const [segment = "", ...rest] = url.pathname.slice(prefix.length).split("/");
    sent.push({
      answer: answerOf(request),
      body: request.postDataJSON(),
      dryRun: url.searchParams.get("dry_run") === "true",
      expected: url.searchParams.get("expected_count"),
      id: decodeURIComponent(segment),
      ifMatch: request.headers()["if-match"] ?? null,
      method: request.method(),
      rest: rest.length === 0 ? "" : `/${rest.join("/")}`,
    });
  });
  return sent;
}

/** Waits until `requests` holds `count` writes, failing with `message`, and answers the
 *  `count`th. */
export async function nthWrite(
  requests: readonly NodeRequest[],
  count: number,
  message: string,
  timeout?: number
): Promise<NodeRequest> {
  await expect
    .poll(() => nodeWrites(requests).length, { message, timeout })
    .toBeGreaterThanOrEqual(count);
  const write = nodeWrites(requests)[count - 1];
  if (write === undefined) throw new Error(message);
  return write;
}

/** The `placement_seq` a taken move answered, as the quoted entity tag the next move sends. */
export function nextTag(answer: NodeAnswer | undefined): string {
  return `"${String(member(answer?.body, "placement_seq"))}"`;
}

/** Issue `key`'s tree read after a reload of the page: what the server keeps, read whole through
 *  the signed-in page's own request context once the reloaded page has read its issue. */
export async function treeAfterReload(page: Page, key: string): Promise<IssueTreeRead> {
  await page.reload();
  await issueRead(page);
  return requestedTreeRead(page, key);
}

/** The version of the document `id` the server holds now (`GET /api/v1/artifacts/{id}/text`). */
export async function documentVersion(page: Page, id: string): Promise<number> {
  const path = routeUrl("/api/v1/artifacts/{id}/text", { id });
  const response = await page.request.get(path);
  expect(response.ok(), `GET ${path} answers ${response.status()}`).toBe(true);
  const version = member(await response.json(), "version");
  if (typeof version !== "number") throw new Error(`${path} answers no version`);
  return version;
}

/** Marks the page's document, so a row can tell that what follows loaded no other. */
export async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    Reflect.set(window, "graphEditDocument", true);
  });
}

/** Whether the page still shows the document `markDocument` marked: no reload stood between. */
function sameDocument(page: Page): Promise<boolean> {
  return page.evaluate(() => Reflect.get(window, "graphEditDocument") === true);
}

/** Fails with `message` unless the identity block's approval chip of the document `id` reads it
 *  approved and changed since (`ApprovalChip.tsx`: `Approved v<N> · ...`), with no reload since
 *  `markDocument`. */
export async function expectApprovalChangedSince(
  page: Page,
  id: string,
  message: string
): Promise<void> {
  await expect(approvalChip(page, id), message).toHaveText(
    new RegExp(`^${APPROVAL_STATE_WORDS.approved} v\\d+ · `, "u"),
    { timeout: 30_000 }
  );
  expect(await sameDocument(page), `${message}, with no reload`).toBe(true);
}

/** Fails with `message` unless the identity block's approval chip of the document `id` reads it
 *  approved at the version it holds, nothing changed since (`ApprovalChip.tsx`: `Approved v<N>`). */
export async function expectApproved(page: Page, id: string, message: string): Promise<void> {
  await expect(approvalChip(page, id), message).toHaveText(
    new RegExp(`^${APPROVAL_STATE_WORDS.approved} v\\d+$`, "u"),
    { timeout: 30_000 }
  );
}

/** The words of the identity block's approval chip of the document `id`, once it is drawn reading
 *  the document approved (`Approved v<N>`, with what changed since where something did) and two
 *  reads a poll apart agree; fails with `message` where it is not drawn. */
export async function approvalWords(page: Page, id: string, message: string): Promise<string> {
  const chip = approvalChip(page, id);
  await expect(chip, message).toHaveText(
    new RegExp(`^${APPROVAL_STATE_WORDS.approved} v\\d+`, "u"),
    { timeout: 30_000 }
  );
  return settledValue(message, async () => (await chip.textContent()) ?? "");
}

/** Fails with `message` unless the identity block's approval chip of the document `id` comes to
 *  read other words than `before` (`approvalWords`), as the page draws a write of the document
 *  live, with no reload since `markDocument`. */
export async function expectApprovalWordsChanged(
  page: Page,
  id: string,
  before: string,
  message: string
): Promise<void> {
  const chip = approvalChip(page, id);
  await expect
    .poll(async () => (await chip.textContent()) ?? "", { message, timeout: 30_000 })
    .not.toBe(before);
  expect(await sameDocument(page), `${message}, with no reload`).toBe(true);
}
