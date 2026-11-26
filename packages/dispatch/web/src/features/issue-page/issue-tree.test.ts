import { beforeAll, expect, test } from "bun:test";
import { parseBlockPointerValue } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactElement, type ReactNode } from "react";

import { countEngineParses } from "../../__tests__/display-trees";
import type {
  Artifact,
  ArtifactBlock,
  Ask,
  AskResolution,
  BlockAttributeValue,
  CheckBlock,
  CheckResult,
  Comment,
  CommentWithTrees,
  Decision,
  DecisionLink,
  DisplayTree,
  IssueCheckResults,
  IssueChild,
  IssueCoverage,
  IssueDetails,
  Message,
  MessageRead,
  ReferenceTarget,
  RequirementCoverage,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { askResolutionBadge } from "../refs/actor";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { usePlainHeadline } from "../refs/plain-text";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { parseNodeReference } from "../refs/routes";
import { wordsOf } from "../refs/tree-fragment";
import { agentsOnlyIds } from "./agents-only";
import { agentsOnlyItem } from "./issue-address";
import {
  buildIssueTree,
  chipName,
  drawnLine,
  FIRST_LINE_LIMIT,
  firstLineOf,
  type IssueTreeReads,
  lineWordsRaw,
  type NodeChip,
  nodeWords,
  type TreeAsk,
  type TreeNode,
  targetNaming,
} from "./issue-tree";
import { useNodeLine } from "./node-line";
import { NODE_TYPE_NAMES, NODE_TYPES, nodeStateWord, PRESET_NAMES } from "./page-words";
import { filterSignals, presetView } from "./presets";
import {
  nameOf,
  namesOf,
  nodeLineOf,
  Publisher,
  projectedNames,
  publishedNames,
  publishingNames,
} from "./projection-fixture";
import { knownIssueKeys, rowReferences, rowReferenceWords } from "./row-references";
import { SENTENCE_TABLE } from "./sentence-table";
import { storedReading } from "./stored-reading";
import { blockNodeId, blockWords, checkStanding, nodeIdOf, treeContext } from "./tree-drafts";
import { treeRows } from "./tree-fold";
import { decisionBehind, nodesInOrder } from "./tree-keys";
import {
  treeNode as answerNode,
  treeAnswer,
  treeParent,
  treeTuple,
} from "./tree-read/tree-fixtures";
import type { TreeAnswerNode, TreeTuple } from "./tree-read/tree-read";
import {
  statementCounts,
  statementStandings,
  verificationReading,
} from "./verification-filter/statement-state";

/**
 * The tree model, read as the page reads it: one call with the page's reads in and the issue's
 * root node out.
 *
 * The outline is drawn from the tree read, so each case writes its issue as the reads the
 * outline was drawn from before it - the documents' blocks, the asks, the comments, the
 * messages, the children - and `answered` writes the answer the tree read gives for that issue,
 * which is what the model is handed.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** A chip as a row reads it: its fields, with the name it draws (`nameOf`) in place of the
 *  reading. */
function drawnChip(chip: NodeChip): Omit<NodeChip, "reading"> & { readonly name: string } {
  const { reading: _reading, ...fields } = chip;
  return { ...fields, name: nameOf(chip) };
}

const KEY = "CORE-1";
const SPEC = "spec";
const VERIFICATION = "verification-md";

function artifact(id: string, slug: string, name: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

function issueDetails(artifacts: readonly Artifact[]): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [...artifacts],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    design_gates: [],
    external_links: [],
    key: KEY,
    labels: [],
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    decisions_artifact_id: "",
    primary_artifact_id: artifacts.find((one) => one.primary)?.id ?? "",
    verification_artifact_id: "",
    priority: null,
    project: "CORE",
    progress: { children: null, tasks: null },
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    status: "todo",
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
  };
}

/** One block of a document, with whatever `block[]` pointers the case needs. */
function block(
  id: string,
  type: string,
  pointers: Readonly<Record<string, string>> = {}
): ArtifactBlock {
  const attributes: BlockAttributeValue[] = Object.entries(pointers).map(([name, value]) => ({
    kind: "block[]",
    name,
    value,
  }));
  return {
    attributes,
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

function requirement(id: string, text: string, gaps: readonly string[] = []): RequirementCoverage {
  return {
    block_id: id,
    checks: 0,
    gaps: [...gaps],
    plan_tasks: 0,
    ref: `dispatch://${KEY}/spec#b-${id}`,
    text,
  };
}

function coverage(requirements: readonly RequirementCoverage[]): IssueCoverage {
  return {
    gaps: requirements.flatMap((one) => one.gaps ?? []),
    has_plan: false,
    issue: KEY,
    requirements: [...requirements],
    spec_artifact_id: "artifact-spec",
  };
}

/** The commit the fixture's results ran at, which is also the head their check compares them
 *  with, so each recorded pass stands at what the check compared with. */
const RESULT_COMMIT = "c".repeat(40);

/** One result recorded against a check, with the negative control it recorded; `""` is a result
 *  that recorded none, which the page draws no gap for, since the server is to refuse such a
 *  result. `extra` is whatever the case changes about it: the verdict it reached, the
 *  retraction that withdrew it, or the words of its check having moved under it. */
function recorded(
  id: string,
  negativeControl: string,
  extra: Partial<CheckResult> = {}
): CheckResult {
  return {
    actor: { id: "tester", kind: "session" },
    check_artifact_id: "artifact-verification",
    check_block_id: "c1",
    check_issue_key: KEY,
    check_sha256: "sum",
    check_version: 1,
    check_words_moved: false,
    commit_sha: RESULT_COMMIT,
    commit_stands: { answer: "at_basis" },
    created_at: "2026-11-05T00:00:00Z",
    excluded: 0,
    failed: 0,
    id,
    issue_key: KEY,
    negative_control: negativeControl,
    observed: `What result ${id} saw.`,
    role: "",
    run: "",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "",
    verdict: "pass",
    ...extra,
  };
}

/** One check of the Verification document with the results recorded against it, newest first,
 *  as `GET /api/v1/issues/{key}/results` answers them. `payload` is whatever the case changes
 *  about the check the server sends, including a field it sends no value for at all. */
function checkedResults(
  blocks: readonly {
    blockId: string;
    payload?: Partial<CheckBlock>;
    results: readonly CheckResult[];
  }[]
): IssueCheckResults {
  const checks: CheckBlock[] = blocks.map((one) => ({
    artifact_id: "artifact-verification",
    block_id: one.blockId,
    compared_with: [{ commit: RESULT_COMMIT, kind: "pull_request", pull_request: 7 }],
    issue_key: KEY,
    negative_control: "",
    pass_stands: { answer: "yes", roles: ["tester"] },
    ref: `dispatch://${KEY}/artifact/${VERIFICATION}#b-${one.blockId}`,
    results: [...one.results],
    run: "bun test",
    sha256: "sum",
    surface: "cli",
    text: `Check ${one.blockId} of the redesign.`,
    version: 1,
    when: "before_merge",
    ...one.payload,
  }));
  return { checks, issue: KEY, verification_artifact_id: "artifact-verification" };
}

/** One agent's decision, whose words are `text` and whose `shaped` edge reaches the spec blocks
 *  `shaped` - a link from the decision's block to each (`specLink`) - as the model's decision a
 *  decision node carries (`decided`). */
function decision(
  blockId: string,
  text: string,
  shaped: readonly string[]
): Decision & { readonly links: NonNullable<Decision["links"]> } {
  return {
    asks: [],
    block_id: blockId,
    changed_at: null,
    changed_version: 1,
    heading: text,
    kept: null,
    kind: "agent",
    links: shaped.map((one) => specLink(blockId, one)),
    ref: `dispatch://${KEY}/artifact/decisions-md#b-${blockId}`,
    replaced: false,
    text,
    text_sha256: "sum",
    unsure: false,
    waiting_review: false,
  };
}

/** A decision's link from its block `fromBlock` - the decision's, or the ask's, `""` for an ask of
 *  the conversation - to the spec block `blockId`, as the decisions read answers one. */
function specLink(fromBlock: string, blockId: string): DecisionLink {
  return {
    changed_since_tie: false,
    created_at: "2026-11-05T00:00:00Z",
    created_version: 1,
    from_block: fromBlock,
    stale: false,
    to: {
      block_id: blockId,
      changed_at: null,
      changed_by: null,
      id: "artifact-spec",
      kind: "block",
      made_at: null,
      made_by: null,
      parent: null,
      parent_version: null,
      ref: `dispatch://${KEY}/spec#b-${blockId}`,
    },
  };
}

/**
 * The decisions `made` as the tree read answers them: each one a
 * decision block of `document`, the decisions document unless the case names another, in the
 * order given, under the issue unless `parents` gives it another parent by its block id, carrying
 * the model's decision (`decision`) and its words; and a `shaped` tuple from it to the address of
 * each node its links name, which is what its chips are drawn from.
 */
function decided(
  made: readonly Decision[],
  parents: Readonly<Record<string, string>> = {},
  document: Pick<Artifact, "id" | "slug"> = { id: "artifact-decisions", slug: "decisions-md" }
): { readonly nodes: TreeAnswerNode[]; readonly tuples: TreeTuple[] } {
  return {
    nodes: made.map((row, position) =>
      answerNode(row.block_id, "decision", parents[row.block_id] ?? KEY, {
        decision: row,
        document: { artifact_id: document.id, position, slug: document.slug },
        text: row.text,
        ...(row.text_tree === undefined ? {} : { text_tree: row.text_tree }),
      })
    ),
    tuples: made.flatMap((row) =>
      (row.links ?? []).flatMap((link) =>
        link.to.ref === undefined ? [] : [treeTuple("shaped", row.block_id, link.to.ref)]
      )
    ),
  };
}

/** The issue's decisions document, whose blocks `decided`'s decision nodes are unless a case
 *  names another document. */
const made = artifact("artifact-decisions", "decisions-md", "decisions.md");

/** A link of decision `d1`'s to `ref`, which is a chip of the decision's and places nothing. */
function linkTo(ref: string, kind: DecisionLink["to"]["kind"]): DecisionLink {
  return {
    changed_since_tie: false,
    created_at: "2026-11-05T00:00:00Z",
    created_version: 1,
    from_block: "d1",
    stale: false,
    to: {
      changed_at: null,
      changed_by: null,
      id: ref,
      kind,
      made_at: null,
      made_by: null,
      parent: null,
      parent_version: null,
      ref,
    },
  };
}

/** The node `id` under `root`, wherever it sits; the first placement of a node drawn twice. */
function nodeOf(root: TreeNode, id: string): TreeNode | undefined {
  if (root.id === id) return root;
  for (const child of root.children) {
    const found = nodeOf(child, id);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Every placement of `id`, with the node each one sits under. */
function placementsOf(root: TreeNode, id: string): { parent: string; node: TreeNode }[] {
  const found: { parent: string; node: TreeNode }[] = [];
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (child.id === id) found.push({ node: child, parent: node.id });
      walk(child);
    }
  };
  walk(root);
  return found;
}

/** How many levels the deepest branch of `node` reaches, the node itself being one. */
function depthOf(node: TreeNode): number {
  return 1 + Math.max(0, ...node.children.map(depthOf));
}

/** One question asked of the issue, under no document unless `extra` names its block. */
function question(id: string, extra: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: `Question ${id}?`,
    state: "open",
    urgency: "med",
    ...extra,
  };
}

function comment(id: string, createdAt: string): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Comment ${id}.`,
    created_at: createdAt,
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: KEY,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

function thread(id: string, createdAt: string): MessageRead {
  const message: Message = {
    author: { id: "bob", kind: "user" },
    body: `Message ${id}.`,
    broadcast_id: null,
    created_at: createdAt,
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id,
    in_reply_to: null,
    issue_key: KEY,
    reply_block: null,
    target: null,
  };
  return { message, replies: [] };
}

/** A case's issue as the earlier reads hold it, with what the tree read answers beside them. */
interface Fixture extends IssueTreeReads {
  /** The issue's comments, each of which the tree read answers as a node, and which the reads
   *  carry as the comments read, which says what each one's author did to it. */
  readonly comments?: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[];
  /** The blocks of each document of the issue, whose typed blocks the tree read answers. */
  readonly documents?: readonly {
    readonly artifactId: string;
    readonly blocks: readonly ArtifactBlock[];
  }[];
  /** The issue's messages with their replies, each of which the tree read answers as a node. */
  readonly messages?: readonly MessageRead[];
  /** Nodes the case adds to the answer as they stand, a decision node among them. */
  readonly nodes?: readonly TreeAnswerNode[];
  /** The parent the read answers for a typed block, by its id, where the case gives it one. */
  readonly parents?: Readonly<Record<string, string>>;
  /** Tuples the case adds to the answer as they stand, each naming its ends by name
   *  (`treeTuple`): what an ask's node shaped among them, which no block's pointer writes. */
  readonly tuples?: readonly TreeTuple[];
}

/** A node of the answer being written, with its place among its siblings: the place the outline
 *  drew it in, for a node the read ranks, and none for an ask, a comment or a message, which the
 *  read orders after its ranked siblings by when it was made and then by id. */
interface Written {
  readonly node: TreeAnswerNode;
  readonly place?: number;
}

/** A node's name in the case: its block id, else its own id (`tree-fixtures.ts`). */
function nodeNameOf(node: TreeAnswerNode): string {
  return node.block_id ?? node.id;
}

/** The read's order of two siblings: ranked ones by their place, then by name; a ranked one
 *  before one with no rank; and two with none by when each was made, then by name. */
function siblingOrder(one: Written, other: Written): number {
  if (one.place !== undefined && other.place !== undefined) {
    return one.place - other.place || nodeNameOf(one.node).localeCompare(nodeNameOf(other.node));
  }
  if (one.place !== undefined) return -1;
  if (other.place !== undefined) return 1;
  return (
    compareTimestamps(one.node.made_at ?? "", other.node.made_at ?? "") ||
    nodeNameOf(one.node).localeCompare(nodeNameOf(other.node))
  );
}

/**
 * The case's reads with the answer the tree read gives for its issue: each typed block of its
 * documents once, with its `document` and the attributes its block carries, under the parent the
 * read answers it - a check or a plan task under the first
 * requirement its `serves` names, every other block under the issue unless the case gives it
 * another (`Fixture.parents`) - and a requirement the coverage read holds and a check the results
 * read holds with their words, each result under its check, the children, the asks, an ask a
 * person answered with that person's decision (`tree-fixtures.ts`), linked to each spec block a
 * `shaped` tuple the case adds from it reaches, a reply under its head, an ask's thread under the
 * ask, a comment anchored inside a typed block under that block, and every
 * thread a person wrote in, a thread only agents wrote in being no node (`agents-only.ts`). An
 * agent's decision is a node the case adds (`Fixture.nodes`, `decided`), and a decision block of
 * a document's blocks is none. Each block pointer is a tuple, to the node it names where the
 * answer holds it, and to its address where not or where it pins a version of that node, which
 * is no node of the answer; each tuple the case adds (`Fixture.tuples`) is one too. Ranks are the
 * read's, which a case does not state, so ranked siblings stand in the order the outline drew
 * them - each document's blocks in turn, the spec's first, then the statements the coverage read
 * holds, then the children - and the rest after them by time, then id, as the read orders them.
 */
function answered(fixture: Fixture): IssueTreeReads {
  const {
    comments,
    documents,
    messages,
    nodes: added,
    parents: given,
    tuples: tied,
    ...reads
  } = fixture;
  const issue = reads.issue;
  const key = issue.key;
  const { nodeIdOf, slugOf, specSlug } = treeContext({ issue });
  const spec = (issue.artifacts ?? []).find((one) => one.slug === specSlug);
  const ranks = new Map(
    (issue.artifacts ?? []).map((one, index) => [one.id, one.id === spec?.id ? 0 : index + 1])
  );
  const targets = new Map((reads.targets ?? []).map((target) => [target.ref, target]));
  const statements = reads.coverage?.requirements ?? [];
  const results = reads.results?.checks ?? [];
  const asks = new Map<string, TreeAsk>();
  for (const ask of [...(issue.open_asks ?? []), ...(reads.asks ?? [])]) asks.set(ask.id, ask);
  const askSlug = (ask: TreeAsk): string | undefined =>
    ask.block_artifact?.slug ?? slugOf(ask.artifact_id ?? "");
  const written: Written[] = [];
  /** The read id of each node a pointer can name, by the outline's id for it. */
  const byId = new Map<string, string>();
  const pointers: { from: string; name: string; refs: readonly string[] }[] = [];
  const types: readonly string[] = NODE_TYPES;
  const asBlock = new Map<string, string>();
  for (const document of documents ?? []) {
    const slug = slugOf(document.artifactId);
    if (slug === undefined) continue;
    const rank = ranks.get(document.artifactId) ?? 0;
    document.blocks.forEach((block, position) => {
      if (!types.includes(block.type) || block.type === "decision") return;
      const target = targets.get(`dispatch://${key}/artifact/${slug}#b-${block.id}`);
      const stored = target === undefined || target.gone ? "" : blockWords(target.name);
      const at = statements.findIndex((one) => slug === specSlug && one.block_id === block.id);
      const statement = block.type === "requirement" ? statements[at] : undefined;
      const check = results.find(
        (one) => one.artifact_id === document.artifactId && one.block_id === block.id
      );
      const ask = [...asks.values()].find(
        (one) => block.type === "ask" && one.block_id === block.id && askSlug(one) === slug
      );
      if (ask !== undefined) asBlock.set(ask.id, block.id);
      const words =
        ask !== undefined
          ? { text: ask.question, tree: ask.question_tree }
          : {
              text: statement?.text ?? (block.type === "check" ? check?.text : undefined) ?? stored,
              tree:
                statement?.text_tree ??
                (block.type === "check" ? check?.text_tree : undefined) ??
                target?.text_tree,
            };
      const attributes: Record<string, string> = {};
      for (const attribute of block.attributes ?? []) {
        attributes[attribute.name] = attribute.value;
        if (attribute.kind === "block[]") {
          pointers.push({
            from: block.id,
            name: attribute.name,
            refs: parseBlockPointerValue(attribute.value) ?? [],
          });
        }
      }
      written.push({
        node: answerNode(block.id, block.type, given?.[block.id] ?? key, {
          attributes,
          document: { artifact_id: document.artifactId, position, slug },
          text: words.text,
          ...(words.tree === undefined ? {} : { text_tree: words.tree }),
          ...(ask === undefined ? {} : { author: [ask.author], made_at: ask.created_at }),
        }),
        place: statement === undefined ? rank + position / document.blocks.length : 2000 + at,
      });
      byId.set(blockNodeId(slug, block.id), block.id);
    });
  }
  // A statement or a check whose block the case gave no document for is a node all the same.
  statements.forEach((statement, at) => {
    if (spec === undefined || byId.has(blockNodeId(specSlug, statement.block_id))) return;
    written.push({
      node: answerNode(statement.block_id, "requirement", key, {
        document: { artifact_id: spec.id, position: at, slug: specSlug },
        text: statement.text,
        ...(statement.text_tree === undefined ? {} : { text_tree: statement.text_tree }),
      }),
      place: 2000 + at,
    });
    byId.set(blockNodeId(specSlug, statement.block_id), statement.block_id);
  });
  for (const check of results) {
    const slug = slugOf(check.artifact_id);
    if (slug === undefined || byId.has(blockNodeId(slug, check.block_id))) continue;
    written.push({
      node: answerNode(check.block_id, "check", key, {
        document: { artifact_id: check.artifact_id, position: 0, slug },
        text: check.text,
        ...(check.text_tree === undefined ? {} : { text_tree: check.text_tree }),
      }),
      place: 0,
    });
    byId.set(blockNodeId(slug, check.block_id), check.block_id);
  }
  for (const check of results) {
    (check.results ?? []).forEach((result, at) => {
      written.push({
        // The read answers a result's reading of its observed text as the node's `text_tree`
        // (the tree route's `TestTheTreeReadAnswersAResultsObservedTextAsItsReading`).
        node: answerNode(result.id, "result", check.block_id, {
          author: [result.actor],
          made_at: result.created_at,
          text: result.observed,
          ...(result.observed_tree === undefined ? {} : { text_tree: result.observed_tree }),
        }),
        place: at,
      });
    });
  }
  const children = issue.children ?? [];
  children.forEach((child, at) => {
    written.push({
      node: answerNode(child.key, "issue", key, { text: child.title }),
      place: 3000 + at / children.length,
    });
  });
  // The read answers each ask a person answered with that person's decision
  // (`issue_tree_read.go`'s `readDecisions`), whose links are the spec blocks the ask's own
  // `shaped` tuples reach (`decisionread`'s `readLinks`).
  for (const ask of asks.values()) {
    if (asBlock.has(ask.id)) continue;
    const links = (tied ?? []).flatMap((tuple) =>
      tuple.kind === "shaped" &&
      tuple.from.id === ask.id &&
      byId.has(blockNodeId(specSlug, tuple.to.id))
        ? [specLink("", tuple.to.id)]
        : []
    );
    written.push({
      node: answerNode(ask.id, "ask", key, {
        author: [ask.author],
        made_at: ask.created_at,
        text: ask.question,
        ...(ask.question_tree === undefined ? {} : { text_tree: ask.question_tree }),
        ...(ask.state !== "answered" ? {} : { decision: { links } }),
      }),
    });
  }
  for (const node of added ?? []) written.push({ node });
  const history = agentsOnlyIds(comments, messages, [...asks.values()]);
  const blocks = new Map(
    written.flatMap(({ node }) => (node.document === undefined ? [] : [[nodeNameOf(node), node]]))
  );
  for (const comment of comments ?? []) {
    if (history.has(comment.id)) continue;
    const anchored = comment.anchor?.block_id ? blocks.get(comment.anchor.block_id) : undefined;
    const parent =
      comment.answers ??
      comment.reply_to ??
      (comment.ask_id === null ? undefined : (asBlock.get(comment.ask_id) ?? comment.ask_id)) ??
      (anchored?.document?.artifact_id === comment.anchor?.artifact_id
        ? anchored?.block_id
        : undefined);
    written.push({
      node: answerNode(comment.id, "comment", parent ?? key, {
        author: [comment.author],
        made_at: comment.created_at,
        text: comment.body,
        ...(comment.body_tree === undefined ? {} : { text_tree: comment.body_tree }),
      }),
    });
  }
  for (const message of (messages ?? []).flatMap((one) => [one.message, ...(one.replies ?? [])])) {
    if (history.has(message.id)) continue;
    written.push({
      node: answerNode(message.id, "message", message.in_reply_to ?? key, {
        author: [message.author],
        made_at: message.created_at,
        text: message.body,
        ...(message.body_tree === undefined ? {} : { text_tree: message.body_tree }),
      }),
    });
  }
  // The read answers each node once, by its own id, so a case whose names meet is no answer.
  const held = new Map(written.map((one) => [nodeNameOf(one.node), one]));
  if (held.size !== written.length) throw new Error("the answer holds two nodes of one id");
  // A check or a plan task stands under the first requirement its `serves` names.
  const placed = written.map((one): Written => {
    const node = one.node;
    if (node.supertag !== "check" && node.supertag !== "plan_task") return one;
    if (given?.[nodeNameOf(node)] !== undefined) return one;
    const served = pointers
      .filter((pointer) => pointer.from === nodeNameOf(node) && pointer.name === "serves")
      .flatMap((pointer) => pointer.refs)
      .map((ref) => byId.get(nodeIdOf(ref) ?? ""))
      .find((id) => {
        const target = id === undefined ? undefined : held.get(id)?.node;
        return target?.supertag === "requirement" && target.document?.slug === specSlug;
      });
    return served === undefined ? one : { ...one, node: { ...node, parent: treeParent(served) } };
  });
  // A parent the answer does not hold is no parent: the read answers the issue at worst.
  const ids = new Set([key, ...placed.map((one) => nodeNameOf(one.node))]);
  const below = new Map<string, Written[]>();
  for (const one of placed) {
    const named = one.node.parent?.id;
    const parent = named !== undefined && ids.has(named) ? named : key;
    below.set(parent, [
      ...(below.get(parent) ?? []),
      { ...one, node: { ...one.node, parent: treeParent(parent) } },
    ]);
  }
  const walk: TreeAnswerNode[] = [answerNode(key, "issue", null, { text: issue.title })];
  const visit = (id: string): void => {
    const siblings = [...(below.get(id) ?? [])].sort(siblingOrder);
    siblings.forEach((one, at) => {
      walk.push({
        ...one.node,
        rank: one.place === undefined ? null : String(at).padStart(4, "0"),
      });
      visit(nodeNameOf(one.node));
    });
  };
  visit(key);
  const pinned = (ref: string): boolean => {
    const route = parseNodeReference(ref);
    return (
      route !== undefined &&
      (("block" in route && route.block?.pin !== undefined) ||
        ("version" in route && route.version !== undefined))
    );
  };
  const tuples = pointers.flatMap((pointer) =>
    pointer.refs.map((ref) =>
      treeTuple(
        pointer.name,
        pointer.from,
        (pinned(ref) ? undefined : byId.get(nodeIdOf(ref) ?? "")) ?? ref
      )
    )
  );
  return {
    ...reads,
    ...(comments === undefined ? {} : { comments }),
    ...(messages === undefined ? {} : { messages }),
    tree: treeAnswer(walk, [...tuples, ...(tied ?? [])], { issue: key }),
  };
}

/** The issue's tree as `buildIssueTree` draws it from the case's reads and the tree read's
 *  answer for them (`answered`). */
function treeOf(fixture: Fixture): TreeNode {
  return buildIssueTree(answered(fixture));
}

/** `treeOf`, for a case whose own names hide it. */
const treeFrom = treeOf;

// R4: a check serves every requirement its `serves` names, edited under the first and
// transcluded under the rest, and each placement names the other requirement it sits under.
test("a check that serves two requirements is drawn under both", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first."), requirement("r2", "The second.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement"), block("r2", "requirement")] },
      {
        artifactId: checks.id,
        blocks: [
          block("c1", "check", {
            serves: `dispatch://${KEY}/spec#b-r1 dispatch://${KEY}/spec#b-r2`,
          }),
        ],
      },
    ],
    issue: issueDetails([spec, checks]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: ":::check{#c1}\nThe tree draws both requirements.\n:::",
        ref: `dispatch://${KEY}/artifact/${VERIFICATION}#b-c1`,
      },
    ],
  });

  const drawn = placementsOf(root, blockNodeId(VERIFICATION, "c1"));
  expect(drawn.map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
    blockNodeId(SPEC, "r2"),
  ]);
  expect(drawn.map((one) => one.node.transcluded)).toEqual([false, true]);
  expect(drawn[0]?.node.alsoUnder.map((chip) => chip.ref)).toEqual([`dispatch://${KEY}/spec#b-r2`]);
  expect(drawn[1]?.node.alsoUnder.map((chip) => chip.ref)).toEqual([`dispatch://${KEY}/spec#b-r1`]);
  // One node, two placements: both open the same block, so both carry its words.
  expect(drawn.map((one) => one.node.text)).toEqual([
    "The tree draws both requirements.",
    "The tree draws both requirements.",
  ]);
});

// R4, R8: a node whose spine parent is absent sits under the issue, as a node like any other and
// never as an error. No requirement carries a `from` and no decision a question it settles under
// today's model, so the absence tells no node apart from its siblings and no gap says it: the
// requirement keeps the one gap that does, that no check serves it.
test("a requirement no decision shaped sits under the issue as a node nothing explains", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = treeOf({
    coverage: coverage([requirement("r1", "Nothing explains this.", ["r1 has no check"])]),
    ...decided([decision("d1", "Shapes nothing.", [])]),
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec, made]),
  });

  expect(placementsOf(root, blockNodeId(SPEC, "r1")).map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.gaps).toEqual(["no_check"]);
  const settled = blockNodeId("decisions-md", "d1");
  expect(placementsOf(root, settled).map((one) => one.parent)).toEqual([KEY]);
});

// While a document of the issue is still being indexed the coverage read names no requirement as
// lacking a check, and writes that document's line among its own gaps in their place: each
// statement carries no gap and that line, in its own words, to draw where its gaps are.
test("a statement whose coverage waits on a document still being indexed carries its line and no gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const owed = "spec.md version 12 is still being indexed; its coverage shows once that ends.";
  const root = treeOf({
    coverage: {
      ...coverage([requirement("r1", "The page waits for the index.")]),
      gaps: [owed],
      owed_documents: [{ artifact_id: spec.id, version: 12 }],
    },
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec]),
  });

  const statement = nodeOf(root, blockNodeId(SPEC, "r1"));
  expect(statement?.gaps).toEqual([]);
  expect(statement?.coverageNotes).toEqual([owed]);
});

// R9: a decision the model marks unsure carries the mark its row says in one word, and one the
// model does not mark carries none. The model marks it unsure where the read says so, and where
// the conversion wrote its agent's uncertainty as high (`decision-nodes.ts`).
test("a decision the model marks unsure is marked so, and one it does not is not", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const { nodes, tuples } = decided([
    { ...decision("d1", "Fold to the frontier.", []), unsure: true },
    decision("d2", "Name every state.", []),
    decision("d3", "Hold the deploy.", []),
  ]);
  const root = treeOf({
    issue: issueDetails([spec, made]),
    nodes: nodes.map((one) =>
      one.block_id === "d3" ? { ...one, attributes: { uncertainty: "high" } } : one
    ),
    tuples,
  });

  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.unsure).toBe(true);
  expect(nodeOf(root, blockNodeId("decisions-md", "d2"))?.unsure).toBeUndefined();
  expect(nodeOf(root, blockNodeId("decisions-md", "d3"))?.unsure).toBe(true);
});

// R4: every edge that is not the spine is a chip naming its target; a target nothing answers to
// now is drawn as gone, with no name, rather than dropped.
test("a pointer whose target is gone is drawn as a chip that says so", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const gone = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [block("t1", "plan_task", { needs: gone, serves: `dispatch://${KEY}/spec#b-r1` })],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      { gone: true, kind: "block", name: "", ref: gone },
      {
        gone: false,
        kind: "block",
        name: "Task 1 of the redesign is one step of the build.",
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
      },
    ],
  });

  const task = nodeOf(root, blockNodeId("plan-md", "t1"));
  expect(task?.text).toBe("Task 1 of the redesign is one step of the build.");
  // The chip of a `needs` edge carries the page's word for it (`BLOCK_ATTRIBUTE_LABELS.needs`).
  expect(task?.chips.map(drawnChip)).toEqual([
    { answered: true, gone: true, label: "needs", name: "", ref: gone },
  ]);
});

// The batched read answers a short reference whose id two or more targets carry as
// gone and ambiguous, and the chip says it is ambiguous, so it can draw the reference as written
// rather than as a target deleted. A target that is gone and no more carries no `ambiguous`.
test("a chip of an ambiguous short reference says so, and a gone target's chip does not", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const ambiguous = `dispatch://${KEY}/artifact/plan-md#b-abcd1234`;
  const gone = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = treeOf({
    documents: [
      {
        artifactId: plan.id,
        blocks: [block("t1", "plan_task", { needs: `${ambiguous} ${gone}` })],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      { ambiguous: true, gone: true, kind: "block", name: "", ref: ambiguous },
      { gone: true, kind: "block", name: "", ref: gone },
    ],
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.chips.map(drawnChip)).toStrictEqual([
    { ambiguous: true, answered: true, gone: true, label: "needs", name: "", ref: ambiguous },
    { answered: true, gone: true, label: "needs", name: "", ref: gone },
  ]);
});

// An ambiguous short reference has no name, as a gone target has none, so the page publishes
// a reading for neither (`targetNaming`'s `readings`); it publishes the ambiguous one among the
// references it read as ambiguous (its `ambiguous`) and the gone one among those it read as gone
// (its `gone`), by the chip's own rule, so a surface draws each as written, links nothing for
// the first, and reads no target of its own for either (issue page decision 350).
test("the page publishes an ambiguous short reference as ambiguous, and a gone target as gone", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const ambiguous = `dispatch://${KEY}/artifact/plan-md#b-abcd1234`;
  const gone = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const named = "dispatch://CORE-7";
  const title = "Read the names once";
  const reads = {
    documents: [
      {
        artifactId: plan.id,
        blocks: [block("t1", "plan_task", { needs: `${ambiguous} ${gone}` })],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      { ambiguous: true, gone: true, kind: "block", name: "", ref: ambiguous },
      { gone: true, kind: "block", name: "", ref: gone },
      { gone: false, kind: "issue", name: title, ref: named },
    ] satisfies ReferenceTarget[],
  };

  const naming = targetNaming(answered(reads));
  expect([...naming.ambiguous]).toEqual([ambiguous]);
  expect([...naming.gone]).toEqual([gone]);
  expect([...namesOf(reads)]).toEqual([[named, title]]);
});

// R21: a typed block is a directive, and the reference read names a block by its stored excerpt,
// which is its source: `:::plan_task{#t1 serves="…" needs="…"}`, its words, and the closing
// fence. The opening line and the fence are markup, so a block's row carries its words alone,
// and a chip naming it the words of the server's reading of it.
test("a typed block's row and a chip naming it carry its words, not its directive's source", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const verification = artifact("artifact-verification", VERIFICATION, "verification.md");
  const serves = `dispatch://${KEY}/spec#b-r1`;
  const needs = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [block("t0", "plan_task", { serves }), block("t1", "plan_task", { needs, serves })],
      },
      { artifactId: verification.id, blocks: [block("c1", "check", { serves })] },
    ],
    issue: issueDetails([spec, plan, verification]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: `:::plan_task{#t0 serves="${serves}"}\nAdd the routes.\n:::`,
        ref: needs,
        text_tree: readingOf("Add the routes."),
      },
      {
        gone: false,
        kind: "block",
        name: `:::plan_task{#t1 serves="${serves}" needs="${needs}"}\nDraw the panel.\n:::`,
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
      },
      {
        gone: false,
        kind: "block",
        name: `:::check{#c1 serves="${serves}"}\nThe page loads in one second.\n\nOn a phone too.\n:::`,
        ref: `dispatch://${KEY}/artifact/${VERIFICATION}#b-c1`,
      },
    ],
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t0"))?.text).toBe("Add the routes.");
  const task = nodeOf(root, blockNodeId("plan-md", "t1"));
  expect(task?.text).toBe("Draw the panel.");
  expect(task?.chips.map(nameOf)).toEqual(["Add the routes."]);
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.text).toBe(
    "The page loads in one second.\n\nOn a phone too."
  );
});

// R1, R21: a chip names a block by the first line a row draws for it, read from the server's
// reading of the block's words and never from its stored source, whose directive and markdown
// are markup: where the read sent no reading, the chip draws no words of it, only the page's
// word for its kind (R22). A chip naming a whole document names it by the page's word for it,
// never by its file name.
test("a chip of a block draws the first line of its reading, and a document's chip its role word", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const first = `dispatch://${KEY}/spec#b-r1`;
  const second = `dispatch://${KEY}/spec#b-r2`;
  const whole = `dispatch://${KEY}/spec`;
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { marks: [{ type: "strong" }], text: "R2.", type: "text" },
          { text: " The second statement. It goes on.", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  // The second statement as the batched read answers it: its stored source as its name, and
  // the server's reading of its words where the read sends one.
  const statement = (tree?: DisplayTree): ReferenceTarget => ({
    gone: false,
    kind: "block",
    name: ":::requirement{#r2}\n**R2.** The second statement. It goes on.\n:::",
    ref: second,
    ...(tree === undefined ? {} : { text_tree: tree }),
  });
  // A decision tied to the spec as a whole, which the read names by its file name.
  const tied: Decision = {
    ...decision("d1", "Keep the spec as one document.", []),
    links: [
      {
        changed_since_tie: false,
        created_at: "2026-11-05T00:00:00Z",
        created_version: 1,
        from_block: "d1",
        stale: false,
        to: {
          changed_at: null,
          changed_by: null,
          id: spec.id,
          kind: "artifact",
          made_at: null,
          made_by: null,
          parent: null,
          parent_version: null,
          ref: whole,
        },
      },
    ],
  };
  const build = (member: ReferenceTarget) =>
    treeOf({
      coverage: coverage([
        requirement("r1", "The first statement."),
        requirement("r2", "The second statement."),
      ]),
      ...decided([tied]),
      documents: [
        { artifactId: spec.id, blocks: [block("r1", "requirement"), block("r2", "requirement")] },
        {
          artifactId: plan.id,
          blocks: [block("t1", "plan_task", { serves: `${first} ${second}` })],
        },
      ],
      issue: issueDetails([spec, plan, made]),
      targets: [member, { gone: false, kind: "artifact", name: "spec.md", ref: whole }],
    });
  // The chip the task's placement under the first statement draws for its other placement.
  const otherPlacement = (root: TreeNode) => {
    const chips = placementsOf(root, blockNodeId("plan-md", "t1"))[0]?.node.alsoUnder ?? [];
    expect(chips.map((chip) => chip.ref)).toEqual([second]);
    return chips[0] === undefined ? undefined : nameOf(chips[0]);
  };

  const root = build(statement(reading));
  const line = otherPlacement(root) ?? "";
  expect(line).not.toContain("**");
  expect(line).not.toContain(":::");
  expect(line).toBe("R2. The second statement.");
  expect(otherPlacement(build(statement()))).toBe(NODE_TYPE_NAMES.requirement);
  expect(
    nodeOf(root, blockNodeId("decisions-md", "d1"))?.chips.map((chip) => ({
      name: nameOf(chip),
      ref: chip.ref,
    }))
  ).toEqual([{ name: PRESET_NAMES.spec, ref: whole }]);
});

// R22: an id is no name a reader reads, and a chip that draws nothing names nothing. A block of
// this issue the batched read answered for with no reading, or with one that holds no words, is
// named by the page's word for its kind, which the blocks the tree read give. Another issue's
// block is of no kind this page read, and a block the read has not answered for yet has no name
// until it does (`answered: false`).
test("a needs chip of a block the read sent no words for is named by its kind's word", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const bare = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const ruled = `dispatch://${KEY}/artifact/plan-md#b-t2`;
  const unread = `dispatch://${KEY}/artifact/plan-md#b-t3`;
  const foreign = "dispatch://CORE-2/artifact/plan-md#b-t9";
  const root = treeOf({
    documents: [
      {
        artifactId: plan.id,
        blocks: [
          block("t0", "plan_task"),
          block("t1", "plan_task", { needs: `${bare} ${ruled} ${foreign} ${unread}` }),
          block("t2", "plan_task"),
          block("t3", "plan_task"),
        ],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      { gone: false, kind: "block", name: ":::plan_task{#t0}\nAdd the routes.\n:::", ref: bare },
      {
        gone: false,
        kind: "block",
        name: ":::plan_task{#t1}\nDraw the panel.\n:::",
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
        text_tree: readingOf("Draw the panel."),
      },
      {
        gone: false,
        kind: "block",
        name: ":::plan_task{#t2}\n---\n:::",
        ref: ruled,
        text_tree: { content: [{ type: "horizontal_rule" }], type: "doc" },
      },
      { gone: false, kind: "block", name: ":::plan_task{#t9}\nShip it.\n:::", ref: foreign },
    ],
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.chips.map(drawnChip)).toEqual([
    { answered: true, gone: false, label: "needs", name: NODE_TYPE_NAMES.plan_task, ref: bare },
    { answered: true, gone: false, label: "needs", name: NODE_TYPE_NAMES.plan_task, ref: ruled },
    { answered: true, gone: false, label: "needs", name: "", ref: foreign },
    { answered: false, gone: false, label: "needs", name: "", ref: unread },
  ]);
});

// One needs chip per task needed. The server accepts a task needed by a pinned and
// an unpinned pointer at once, and both need the one task, so it is one chip: its unpinned
// pointer's, which names the block's current words. A task only pinned pointers name is one chip,
// the first one's, and two tasks stay two chips, in the order their pointers first name them.
test("a task needed by a pinned and an unpinned pointer is one needs chip", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const task = (id: string): string => `dispatch://${KEY}/artifact/plan-md#b-${id}`;
  const root = treeOf({
    documents: [
      {
        artifactId: plan.id,
        blocks: [
          block("a1", "plan_task"),
          block("a2", "plan_task"),
          block("t1", "plan_task", { needs: `${task("a1")}@v1 ${task("a1")}` }),
          block("t2", "plan_task", { needs: `${task("a1")}@v1 ${task("a1")}@v2` }),
          block("t3", "plan_task", {
            needs: `${task("a2")} ${task("a1")}@v1 ${task("a2")}@v3 ${task("a1")}`,
          }),
        ],
      },
    ],
    issue: issueDetails([spec, plan]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: ":::plan_task{#a1}\nAdd the routes.\n:::",
        ref: task("a1"),
        text_tree: readingOf("Add the routes."),
      },
      {
        gone: false,
        kind: "block",
        name: ":::plan_task{#a1}\nAdd the old routes.\n:::",
        ref: `${task("a1")}@v1`,
        text_tree: readingOf("Add the old routes."),
      },
    ],
  });
  const needs = (id: string) =>
    nodeOf(root, blockNodeId("plan-md", id))?.chips.map((chip) => ({
      label: chip.label,
      name: nameOf(chip),
      ref: chip.ref,
    }));

  expect(needs("t1")).toEqual([{ label: "needs", name: "Add the routes.", ref: task("a1") }]);
  expect(needs("t2")).toEqual([
    { label: "needs", name: "Add the old routes.", ref: `${task("a1")}@v1` },
  ]);
  expect(needs("t3")).toEqual([
    { label: "needs", name: "", ref: task("a2") },
    { label: "needs", name: "Add the routes.", ref: task("a1") },
  ]);
});

// R21: an ask's question and a comment's or a message's body are markdown, and the batched read
// names each by that stored text, so a chip naming one - and the name the page publishes for it,
// which is the chip's by one rule (`targetNaming`) - is that text's plain-text projection, its
// words with no `**`, backtick or link mark. The page publishes the name as a link to the
// target reads it, as every surface reads the target by it (R8), and the chip cuts it as a
// chip's words are cut where it draws it.
// An issue's name is its title, which is no markdown and is drawn as written.
test("a chip naming an ask, a comment or a message draws its plain words, an issue its title", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const marked = "**Hold** the `deploy` until [the plan](https://example.com/plan) lands";
  const issueRef = "dispatch://CORE-7";
  const title = "Load __init__.py first";
  for (const kind of ["ask", "comment", "message"] as const) {
    const ref = `dispatch://${KEY}/${kind}/${kind}-9`;
    const reads = {
      ...decided([
        {
          ...decision("d1", "Keep it.", []),
          links: [linkTo(ref, kind), linkTo(issueRef, "issue")],
        },
      ]),
      issue: issueDetails([spec, made]),
      targets: [
        { gone: false, kind, name: marked, ref },
        { gone: false, kind: "issue", name: title, ref: issueRef },
      ],
    };
    const chips = nodeOf(treeOf(reads), blockNodeId("decisions-md", "d1"))?.chips;
    const whole = "Hold the deploy until the plan lands";
    expect(
      chips?.map((chip) => ({ name: nameOf(chip), ref: chip.ref })),
      kind
    ).toEqual([
      { name: "Hold the deploy until the plan…", ref },
      { name: title, ref: issueRef },
    ]);
    expect([...namesOf(answered(reads))], kind).toEqual([
      [ref, whole],
      [issueRef, title],
    ]);
  }
});

// R4, R20: the model takes any depth: a chain of parents the read answers is drawn however long
// it is.
test("a tree of one level and a tree of six are both drawn", () => {
  const alone = buildIssueTree({ issue: issueDetails([]) });
  expect(depthOf(alone)).toBe(1);
  expect(alone.children).toEqual([]);
  expect(alone.held).toBe(0);

  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const deep = [1, 2, 3, 4, 5];
  const root = treeOf({
    coverage: coverage(deep.map((at) => requirement(`r${at}`, `Requirement ${at}.`))),
    documents: [{ artifactId: spec.id, blocks: deep.map((at) => block(`r${at}`, "requirement")) }],
    issue: issueDetails([spec]),
    parents: Object.fromEntries(deep.slice(1).map((at) => [`r${at}`, `r${at - 1}`])),
  });

  expect(depthOf(root)).toBe(6);
  // The five requirements and nothing else: the document they are blocks of is no node (R3).
  expect(root.held).toBe(5);
  expect(nodeOf(root, blockNodeId(SPEC, "r5"))?.children).toEqual([]);
  expect(placementsOf(root, blockNodeId(SPEC, "r5")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r4"),
  ]);
});

// R1: a row's first line is the node's own text to the first sentence end or 100 characters,
// whichever comes first, cut at a word where the limit falls near one and at the limit itself
// where it does not.
test("a first line ends at a sentence, at a word, or at the limit where there is no word", () => {
  expect(firstLineOf("One sentence. And another.")).toEqual({
    cut: true,
    line: "One sentence.",
    sentence: true,
  });
  const long =
    "Every property of the issue that carries a value is visible at every width the page draws, " +
    "with no control opened and no sideways scroll. The right-hand panel holds the rest.";
  const first = firstLineOf(long);
  expect(first.cut).toBe(true);
  expect(first.line.length).toBeLessThanOrEqual(100);
  expect(long.startsWith(first.line)).toBe(true);
  expect(long.charAt(first.line.length)).toBe(" ");
  expect(firstLineOf("Short enough.")).toEqual({
    cut: false,
    line: "Short enough.",
    sentence: true,
  });
  expect(firstLineOf("")).toEqual({ cut: false, line: "", sentence: false });

  // An unbroken token past the limit holds no word boundary to cut at, so the line is the
  // limit's own 100 characters rather than the whole token.
  const unbroken = "a".repeat(FIRST_LINE_LIMIT + 20);
  expect(firstLineOf(unbroken)).toEqual({
    cut: true,
    line: "a".repeat(FIRST_LINE_LIMIT),
    sentence: false,
  });
  // A space at the limit's own edge is a word boundary, and the line ends before it.
  const atTheEdge = `${"a".repeat(FIRST_LINE_LIMIT)} tail`;
  expect(firstLineOf(atTheEdge)).toEqual({
    cut: true,
    line: "a".repeat(FIRST_LINE_LIMIT),
    sentence: false,
  });
});

// R1: a row's first line ends where the page's one sentence end falls (`firstSentenceLength`),
// as the table of sentence ends (`SENTENCE_TABLE`) states it for each text. Every row's text is
// under the line's limit, so the line is its first sentence and no cut of the limit's. A row
// whose first sentence depends on the blocks its text is stored as gives the function no answer
// to meet, and only its text's length is read here.
test("a first line ends where every row of the sentence table ends its first sentence", () => {
  for (const row of SENTENCE_TABLE) {
    expect(row.text.length, `${row.id} is under the line's limit`).toBeLessThanOrEqual(
      FIRST_LINE_LIMIT
    );
    if (row.first === undefined) continue;
    expect(firstLineOf(row.text).line, row.id).toBe(row.first);
  }
});

// R1: a node that opens with a list draws its one line from the list's first item, never from the
// whole list run together (`firstBlockWordsRaw`). The sentence table's rows k3 and k4 pin the
// same rule for an item that holds a nested list or a later paragraph; this case pins items with
// no end mark.
test("a node that opens with a list draws its first item's words, never the whole list's", () => {
  expect(lineWordsRaw({ text: "- Keep the old page\n- Then drop it" })).toBe("Keep the old page");
  expect(lineWordsRaw({ text: "1. Keep the old page\n2. Then drop it" })).toBe("Keep the old page");
});

// R1: a cut at the limit falls between two characters, never inside one. A character outside the
// Basic Multilingual Plane is two UTF-16 units, and a line that ends on the first of them holds a
// lone surrogate, which `encodeURIComponent` refuses and a row draws as a broken glyph.
test("a first line cut at the limit never ends inside a surrogate pair", () => {
  const face = "\u{1F600}";
  // The run starts one unit in, so the limit falls between the two units of its 50th face.
  const line = firstLineOf(`a${face.repeat(75)}`);
  expect(line).toEqual({ cut: true, line: `a${face.repeat(49)}`, sentence: false });
  const last = line.line.charCodeAt(line.line.length - 1);
  expect(last >= 0xd800 && last <= 0xdbff).toBe(false);
  expect(decodeURIComponent(encodeURIComponent(line.line))).toBe(line.line);
  // A run whose pairs end at the limit keeps every one of them.
  expect(firstLineOf(face.repeat(75))).toEqual({
    cut: true,
    line: face.repeat(50),
    sentence: false,
  });
});

// R1: a statement opens on its label - `R1.`, `12.`, `1.2.` - and the label is
// part of its first line, not a sentence end. A first word that ends in `.` and is longer than
// five characters or holds no digit is no label, and ends the line as any other sentence does.
test("a leading label is no sentence end", () => {
  expect(firstLineOf("R1. Draw the tree from one read. Then the rest.")).toEqual({
    cut: true,
    line: "R1. Draw the tree from one read.",
    sentence: true,
  });
  expect(firstLineOf("12. Twelve.")).toEqual({ cut: false, line: "12. Twelve.", sentence: true });
  expect(firstLineOf("t3. Cut the patch.").line).toBe("t3. Cut the patch.");
  expect(firstLineOf("1.2. Nested.").line).toBe("1.2. Nested.");
  expect(firstLineOf("Mr. Smith went home.").line).toBe("Mr.");
  expect(firstLineOf("R1234. Six.").line).toBe("R1234.");
  expect(firstLineOf("1.2.3. Six.").line).toBe("1.2.3.");

  // A labelled sentence past the limit is cut at a word inside it, and its label stays.
  const long = firstLineOf(`R1. ${"word ".repeat(30)}`);
  expect(long.line.length).toBeLessThanOrEqual(100);
  expect(long.line.startsWith("R1. ")).toBe(true);
  expect(long.cut).toBe(true);
  expect(drawnLine(long).endsWith("…")).toBe(true);
  // A label followed by one unbroken run is cut at the limit, never at the label's own space.
  expect(firstLineOf(`R1. ${"a".repeat(200)}`)).toEqual({
    cut: true,
    line: `R1. ${"a".repeat(96)}`,
    sentence: false,
  });
});

// A line that is a whole sentence ends on its own mark, so a cut after it draws no ellipsis -
// "Thread 1 opens here.…" reads as four dots - and a cut that ended inside a sentence draws one.
test("a first line draws its ellipsis only where the cut ended inside a sentence", () => {
  expect(drawnLine({ cut: true, line: "Thread 1 opens here.", sentence: true })).toBe(
    "Thread 1 opens here."
  );
  expect(drawnLine(firstLineOf("Thread 1 opens here. Thread 2 follows."))).toBe(
    "Thread 1 opens here."
  );
  const inside = firstLineOf(`${"word ".repeat(30)}and the end.`);
  expect(inside.cut).toBe(true);
  expect(drawnLine(inside)).toBe(`${inside.line}…`);
  expect(drawnLine({ cut: false, line: "No mark at its end", sentence: false })).toBe(
    "No mark at its end"
  );
  // A token cut hard at the limit whose last kept character is a dot ends no sentence, so its
  // line draws the ellipsis all the same.
  expect(drawnLine(firstLineOf(`${"x".repeat(99)}.yyyy more words here`)).endsWith("…")).toBe(true);
});

// A line cut just after an ellipsis of the author's own draws no second one, since
// `wait……` reads as six dots. The ellipsis is no sentence end, since it often stands inside one,
// so a cut that ended inside a sentence on any other character still draws one.
test("a first line cut after the author's own ellipsis draws no second one", () => {
  const words = `${"word ".repeat(19)}wait…`;
  const trailing = firstLineOf(`${words} and then more words follow here.`);
  expect(trailing).toEqual({ cut: true, line: words, sentence: false });
  expect(drawnLine(trailing)).toBe(words);
  expect(drawnLine({ cut: true, line: "Hold on, wait…", sentence: false })).toBe("Hold on, wait…");
  const plain = firstLineOf(`${"word ".repeat(30)}and the end.`);
  expect(plain.sentence).toBe(false);
  expect(drawnLine(plain)).toBe(`${plain.line}…`);
});

// A decision's own content opens with its own `##` heading, and the words of the whole reading
// join that heading to the paragraph after it, so a line cut from them read "Keep it Keep the
// model pure.", and where a pod sent no reading, "## Keep it Keep the model pure.". A node's one
// line is cut from its first block alone; its words stay whole, since they are what its body
// draws and what its citations are read from. The read answers a decision's text and reading as
// its one `decision` block, and the node holds that block's content, as its own: the lines that
// open and close the block are no words of it, and its reading's first block is the heading.
test("a node's one line is cut from its first block, so a heading is never run into its body", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const text = "## Keep it\n\nKeep the model pure.";
  const reading: DisplayTree = {
    content: [
      { attrs: { level: 2 }, content: [{ text: "Keep it", type: "text" }], type: "heading" },
      { content: [{ text: "Keep the model pure.", type: "text" }], type: "paragraph" },
    ],
    type: "doc",
  };
  const needs = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = treeOf({
    ...decided([{ ...decision("d1", text, []), text_tree: reading }, decision("d2", text, [])]),
    documents: [{ artifactId: plan.id, blocks: [block("t1", "plan_task", { needs })] }],
    issue: issueDetails([spec, made, plan]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: `:::plan_task{#t0}\n${text}\n:::`,
        ref: needs,
        text_tree: reading,
      },
    ],
  });

  const read = nodeOf(root, blockNodeId("decisions-md", "d1"));
  expect(read?.tree).toEqual(reading);
  expect(read === undefined ? "" : nodeWords(read)).toBe("Keep it Keep the model pure.");
  const line = read === undefined ? "" : nodeLineOf(read);
  expect(line).toBe("Keep it");
  // The heading is the whole of its block, so nothing of the line is cut and, by `drawnLine`'s
  // rule, it draws no ellipsis.
  expect(firstLineOf(line)).toEqual({ cut: false, line: "Keep it", sentence: false });
  expect(drawnLine(firstLineOf(line))).toBe("Keep it");
  // Where the read sent no reading the line is the stored text's first block, and the heading's
  // marker is markup and no word of it.
  const stored = nodeOf(root, blockNodeId("decisions-md", "d2"));
  expect(stored?.tree).toBeUndefined();
  expect(stored?.text).toBe(text);
  expect(stored === undefined ? "" : nodeLineOf(stored)).toBe("Keep it");
  // A chip naming a block is cut from the first block of the block's reading the same way.
  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.chips.map(nameOf)).toEqual(["Keep it"]);
});

// A stored heading's text is everything after its one marker, so a heading whose text opens on
// `#` keeps it: `## # 1 rule` is the heading `# 1 rule`.
test("a node's line from a stored heading leaves out its marker once", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const root = treeOf({
    ...decided([decision("d1", "## # 1 rule\n\nbody", [])]),
    issue: issueDetails([spec, made]),
  });
  const stored = nodeOf(root, blockNodeId("decisions-md", "d1"));
  expect(stored === undefined ? "" : nodeLineOf(stored)).toBe("# 1 rule");
});

// A rendered body draws an address in code as its author wrote it (`MarkdownBody.tsx`), so a
// node's line keeps it there too, the projection naming none in code: in a reading, a text run
// marked as inline code and a code block's text keep their address, and in a stored text a code
// span keeps its address. A bare address outside code reads as the words its author wrote until
// it is named, and as its target's name once it is (issue page decision 350, which replaces
// decision 95's short form). The reading the read sent is left as it was, and the node's words
// keep every address.
test("an address in code stays as written in a node's line, from a reading and from stored text", async () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const stored = "Run `dispatch read dispatch://CORE-2` before dispatch://CORE-3 lands.";
  const inline: DisplayTree = {
    content: [
      {
        content: [
          { text: "Run ", type: "text" },
          {
            marks: [{ type: "inlineCode" }],
            text: "dispatch read dispatch://CORE-2",
            type: "text",
          },
          { text: " before dispatch://CORE-3 lands.", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const fenced: DisplayTree = {
    content: [
      {
        attrs: { language: "sh" },
        content: [{ text: "dispatch read dispatch://CORE-2", type: "text" }],
        type: "code_block",
      },
      readingOf("Then dispatch://CORE-3."),
    ],
    type: "doc",
  };
  const sent = structuredClone([inline, fenced]);
  const root = treeOf({
    ...decided([
      { ...decision("d1", stored, []), text_tree: inline },
      {
        ...decision(
          "d2",
          "```sh\ndispatch read dispatch://CORE-2\n```\n\nThen dispatch://CORE-3.",
          []
        ),
        text_tree: fenced,
      },
      decision("d3", stored, []),
    ]),
    issue: issueDetails([spec, made]),
  });
  const node = (id: string): TreeNode | undefined => nodeOf(root, blockNodeId("decisions-md", id));
  const line = (id: string): string => {
    const found = node(id);
    return found === undefined ? "" : nodeLineOf(found);
  };

  const written = "Run dispatch read dispatch://CORE-2 before dispatch://CORE-3 lands.";
  expect(line("d1")).toBe(written);
  expect(line("d2")).toBe("dispatch read dispatch://CORE-2");
  expect(line("d3")).toBe(written);
  // Named, the bare address reads as its target's name, and the one in code stays as written,
  // by the projection a row's line is drawn with, from the names the page published.
  const projected = renderHook(() => usePlainHeadline(inline, undefined), {
    wrapper: publishingNames(
      new Map([
        ["dispatch://CORE-2", "Warm the cache"],
        ["dispatch://CORE-3", "Drain the queue"],
      ])
    ),
  });
  try {
    await waitFor(() =>
      expect(projected.result.current.text?.text).toBe(
        "Run dispatch read dispatch://CORE-2 before CORE-3 Drain the queue lands."
      )
    );
  } finally {
    projected.unmount();
  }
  expect([inline, fenced]).toEqual(sent);
  const read = node("d1");
  expect(read === undefined ? "" : nodeWords(read)).toBe(written);
});

// The reviewer's Minor at a5714d85: a reading may open with a block that holds no words - a
// horizontal rule, a paragraph that is only an image - and a line cut from that block is empty,
// so a parent with words drew an empty line and a chip an empty name. A node's one line is cut
// from its first block that holds words, and so is the line of a stored text with no reading.
test("a node's one line is cut from its first block that holds words, past a rule or an image", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const paragraph = (words: string): DisplayTree => ({
    content: [{ text: words, type: "text" }],
    type: "paragraph",
  });
  const ruled: DisplayTree = {
    content: [{ type: "horizontal_rule" }, paragraph("Keep the model pure.")],
    type: "doc",
  };
  const pictured: DisplayTree = {
    content: [
      {
        content: [{ attrs: { alt: "diagram", src: "shot.png" }, type: "image" }],
        type: "paragraph",
      },
      paragraph("Draw it once."),
    ],
    type: "doc",
  };
  const needs = `dispatch://${KEY}/artifact/plan-md#b-t0`;
  const root = treeOf({
    ...decided([
      { ...decision("d1", "---\n\nKeep the model pure.", []), text_tree: ruled },
      { ...decision("d2", "![diagram](shot.png)\n\nDraw it once.", []), text_tree: pictured },
      decision("d3", "---\n\nKeep the model pure.", []),
      decision("d4", "![diagram](shot.png)\n\nDraw it once.", []),
    ]),
    documents: [{ artifactId: plan.id, blocks: [block("t1", "plan_task", { needs })] }],
    issue: issueDetails([spec, made, plan]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: ":::plan_task{#t0}\n---\n\nKeep the model pure.\n:::",
        ref: needs,
        text_tree: ruled,
      },
    ],
  });
  const line = (id: string): string => {
    const node = nodeOf(root, blockNodeId("decisions-md", id));
    return node === undefined ? "" : nodeLineOf(node);
  };

  expect(line("d1")).toBe("Keep the model pure.");
  expect(line("d2")).toBe("Draw it once.");
  // Where the read sent no reading, a stored rule or image is passed over the same way.
  expect(line("d3")).toBe("Keep the model pure.");
  expect(line("d4")).toBe("Draw it once.");
  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.chips.map(nameOf)).toEqual([
    "Keep the model pure.",
  ]);
});

// A plan task opens on its label, which its stored text writes escaped, `1\.`,
// inside the bold of its first sentence. Its line holds that label and runs to the first
// sentence end after it, read from the server's reading of the task and, where the read sent
// none, from the stored text read to plain words, whose `**` after `today.` hid that sentence
// end. The task's own text stays as stored: its body and its citations read it.
test("a plan task written `**1\\. …**` keeps its label in its line, from its reading and from its stored text", () => {
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const ref = `dispatch://${KEY}/artifact/plan-md#b-t1`;
  const words =
    "**1\\. Stand the harness up and record what the page is today.** Run the steps the " +
    "Verification document names: `bun install`, and a database of your own.";
  const name = `:::plan_task{#t1 serves="dispatch://${KEY}/spec#b-r1"}\n${words}\n:::`;
  const reading: DisplayTree = {
    content: [
      {
        content: [
          {
            marks: [{ type: "strong" }],
            text: "1. Stand the harness up and record what the page is today.",
            type: "text",
          },
          { text: " Run the steps the Verification document names: ", type: "text" },
          { marks: [{ type: "inlineCode" }], text: "bun install", type: "text" },
          { text: ", and a database of your own.", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  for (const [sent, member] of [
    ["a reading", { gone: false, kind: "block", name, ref, text_tree: reading }],
    ["no reading", { gone: false, kind: "block", name, ref }],
  ] as const) {
    const root = treeOf({
      documents: [{ artifactId: plan.id, blocks: [block("t1", "plan_task")] }],
      issue: issueDetails([plan]),
      targets: [member],
    });
    const task = nodeOf(root, blockNodeId("plan-md", "t1"));
    expect(task?.text, sent).toBe(words);
    expect(task === undefined ? "" : nodeLineOf(task), sent).toBe(
      "1. Stand the harness up and record what the page is today."
    );
  }
});

/** Titles as an issue stores them, each with the words its line is projected to, every address
 *  in them as written until it is named (issue page decision 350), and, where they differ from
 *  those, the words it is named by elsewhere, which read each address as its short form, since a
 *  name is a title over one record and reads no other name (`ReferenceNaming.readsNone`). */
const TITLES: readonly (readonly [title: string, line: string, name?: string])[] = [
  ["1. Migrate the table", "1. Migrate the table"],
  ["Add `x` flag", "Add `x` flag"],
  ["Fix <script> handling", "Fix <script> handling"],
  ["Follow dispatch://CORE-3 up", "Follow dispatch://CORE-3 up", "Follow CORE-3 up"],
  [
    "1. Fix `x` for dispatch://CORE-3/spec#b-r1",
    "1. Fix `x` for dispatch://CORE-3/spec#b-r1",
    "1. Fix `x` for CORE-3 spec block",
  ],
  ["Migrate the table\n- then drop it", "Migrate the table - then drop it"],
  ["  Migrate   the\n\ntable  ", "Migrate the table"],
];

/** A child issue `key` of the issue, titled `title`, as the issue read lists it. */
function childIssue(key: string, title: string): IssueChild {
  return {
    active_at: "2026-11-05T00:00:00Z",
    external_links: [],
    key,
    status: "todo",
    subtree_done: 0,
    subtree_total: 0,
    title,
  };
}

// A title is no markdown: the header draws the issue's as written, and the list of an issue's
// children each child's (`slots/header.tsx`, `ChildrenTab.tsx`). The line of the issue and of a
// child issue is projected from its text read as one text run (`titleReading`), so it is its
// text as written, each run of white space one space, and a bare address in it reads as the
// words its author wrote until it is named (issue page decision 350, which replaces decisions 95
// and 327). Read as markdown, `1. Migrate the table` would be a list item with no label,
// ``Add `x` flag`` would lose its backticks and `Fix <script> handling` its tag, and a title with
// a line feed or a blank line would lose what follows it.
test("the line of the issue and of a child issue is its title as written", () => {
  for (const [title, line] of TITLES) {
    const root = treeOf({
      issue: { ...issueDetails([]), children: [childIssue("CORE-2", title)], title },
    });
    expect(nodeLineOf(root), title).toBe(`${KEY} ${line}`);
    const child = nodeOf(root, "CORE-2");
    expect(child?.type, title).toBe("child_issue");
    expect(child === undefined ? "" : nodeLineOf(child), title).toBe(line);
  }
});

// R1 on the issue's own line: the issue's node holds its key and then its title, and the key is
// the page's word for the issue, so R1's rule reads a leading label after it, as it reads one at
// the start of any node's own words: `CORE-1 1. Migrate the table` keeps its `1.`, and a sentence
// end after the label still ends the line. The key is no word of the title, so a name the page
// published for the issue's own address takes no place in its line either.
test("the issue's own line reads a label after its key, and names nothing in its key", () => {
  const root = buildIssueTree({
    issue: { ...issueDetails([]), title: "1. Ship it now. Then the rest of it." },
  });
  expect(nodeLineOf(root)).toBe(`${KEY} 1. Ship it now.`);
  const named = renderHook(() => useNodeLine(root, { surface: "node-row-first-line" }), {
    wrapper: publishingNames(new Map([[`dispatch://${KEY}`, "Ship it now"]])),
  });
  try {
    expect(named.result.current.line).toBe(`${KEY} 1. Ship it now.`);
  } finally {
    named.unmount();
  }
});

// R22: the batched read names an issue by its title cut to its first 240 characters, and a chip
// naming the issue or a child of it, the name the page publishes for it and the link a row draws
// for it by that name read the whole title the issue read holds, as a title over one record: its
// white space collapses and a bare address in it reads as its target's short form, since a name
// reads no other name (issue page decision 350), so a comment citing `dispatch://CORE-2`, titled
// `Follow dispatch://CORE-3 up`, links `CORE-2` and `Follow CORE-3 up`. A title longer than that
// cut is named whole too, so an address the cut would end inside, and one past it, read short.
test("a chip, a published name and a row's link name a child issue by its whole title", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const own = `dispatch://${KEY}`;
  const cited = "dispatch://CORE-2";
  const citing = { ...comment("c1", "2026-11-05T01:00:00Z"), body: `See ${cited} first.` };
  const long = `${"a".repeat(225)} dispatch://CORE-3/spec#b-r1 then dispatch://CORE-4 up`;
  const longName = `${"a".repeat(225)} CORE-3 spec block then CORE-4 up`;
  // The row's own line holds R1's 100 characters of the long title and says it carries on.
  const longLine = `${"a".repeat(100)}…`;
  for (const [title, line, name = line] of [...TITLES, [long, longLine, longName] as const]) {
    // The read's name for an issue, as the server cuts it: the title's first 240 characters.
    const head = [...title].slice(0, 240).join("");
    const reads = {
      comments: [citing],
      ...decided([
        {
          ...decision("d1", "Keep it.", []),
          links: [linkTo(own, "issue"), linkTo(cited, "issue")],
        },
      ]),
      issue: { ...issueDetails([spec, made]), children: [childIssue("CORE-2", title)], title },
      targets: [
        { gone: false, kind: "issue", name: head, ref: own },
        { gone: false, kind: "issue", name: head, ref: cited },
      ] satisfies ReferenceTarget[],
    };
    const root = treeOf(reads);
    const child = nodeOf(root, "CORE-2");
    expect(child === undefined ? "" : nodeLineOf(child), title).toBe(line);
    const chips = nodeOf(root, blockNodeId("decisions-md", "d1"))?.chips;
    // The chip cuts the whole name where it draws it; the page publishes it whole.
    expect(
      chips?.map((chip) => ({ name: nameOf(chip), ref: chip.ref })),
      title
    ).toEqual([
      { name: chipName(parseNodeReference(own), name), ref: own },
      { name: chipName(parseNodeReference(cited), name), ref: cited },
    ]);
    const names = namesOf(answered(reads));
    expect([...names], title).toEqual([
      [own, name],
      [cited, name],
    ]);
    const links = rowReferences(nodeOf(root, citing.id)?.text ?? "", knownIssueKeys(names));
    expect(
      links.map((link) => rowReferenceWords(link, names, reads.issue.key)),
      title
    ).toEqual([{ key: "CORE-2", words: name }]);
  }
});

// R1, R3, R4, R12: each node stands under the parent the read answers: a typed block under the
// issue unless the read gives it another - here the notes' callout under the spec's question -
// a plan task under the first requirement its `serves` names, and a child issue under the
// issue. A document is a node of the issue and not of its conversation, so the tree draws none,
// and the issue's own children stand in the read's order.
test("each node stands under the parent the read answers", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const image: Artifact = { ...artifact("artifact-image", "shot-png", "shot.png"), kind: "image" };
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: notes.id, blocks: [block("n1", "callout")] },
      {
        artifactId: spec.id,
        blocks: [block("summary", "callout"), block("r1", "requirement"), block("q1", "ask")],
      },
      {
        artifactId: plan.id,
        blocks: [
          block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` }),
          block("t2", "plan_task"),
        ],
      },
    ],
    // The issue carries its spec second, so a reading that ordered the blocks by their
    // document's place in the list alone would draw the notes' block first.
    issue: {
      ...issueDetails([notes, spec, plan, image]),
      children: [
        {
          active_at: "2026-11-05T00:00:00Z",
          external_links: [],
          key: "CORE-2",
          status: "todo",
          subtree_done: 0,
          subtree_total: 0,
          title: "A child.",
        },
      ],
    },
    parents: { n1: "q1" },
  });

  const kinds = new Set<string>();
  const walk = (node: TreeNode): void => {
    kinds.add(node.type);
    for (const child of node.children) walk(child);
  };
  walk(root);
  for (const kind of ["document", "image", "handoff"]) {
    expect(kinds.has(kind), kind).toBe(false);
  }
  for (const id of [SPEC, "notes-md", "plan-md", "shot-png"]) {
    expect(nodeOf(root, id), id).toBeUndefined();
  }
  expect(root.children.map((child) => child.id)).toEqual([
    blockNodeId(SPEC, "summary"),
    blockNodeId(SPEC, "q1"),
    blockNodeId("plan-md", "t2"),
    blockNodeId(SPEC, "r1"),
    "CORE-2",
  ]);
  expect(placementsOf(root, blockNodeId("notes-md", "n1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "q1"),
  ]);
  // Each child is a node of its own, carrying the child the issue read answered, whose row
  // draws its status, what blocks it, who holds it and its key.
  expect(nodeOf(root, "CORE-2")?.type).toBe("child_issue");
  expect(nodeOf(root, "CORE-2")?.text).toBe("A child.");
  expect(nodeOf(root, "CORE-2")?.child?.status).toBe("todo");
  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// R3, R4: the spec is the document the issue read names by its id (`primary_artifact_id`), the
// one the page reads the blocks of (`useIssueTree.ts`), whichever document carries the primary
// flag: a reference to the spec resolves to a block of the document the id names, and that
// document's blocks are drawn first.
test("the spec is the document the issue names by its id, not the one flagged primary", () => {
  const flagged = artifact("artifact-old-spec", "old-spec-md", "old-spec.md", true);
  const named = artifact("artifact-spec", SPEC, "spec.md");
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: flagged.id, blocks: [block("o1", "callout")] },
      { artifactId: named.id, blocks: [block("summary", "callout"), block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [
          block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` }),
          block("t2", "plan_task"),
        ],
      },
    ],
    issue: { ...issueDetails([flagged, named, plan]), primary_artifact_id: named.id },
  });

  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.type).toBe("requirement");
  expect(nodeOf(root, blockNodeId("old-spec-md", "r1"))).toBeUndefined();
  expect(root.children.map((child) => child.id)).toEqual([
    blockNodeId(SPEC, "summary"),
    blockNodeId("old-spec-md", "o1"),
    blockNodeId("plan-md", "t2"),
    blockNodeId(SPEC, "r1"),
  ]);
});

// R3: a document is no node, so a block whose words cite one carries a chip that opens it, at
// its own address and named by the page's word for it - the spec's included, which is named as
// the Spec filter is and whose `spec` reference is that filter's address. A citation of one
// block of a document is that block's, and draws no document chip.
test("a block whose text cites a document carries a chip that opens it", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const root = treeOf({
    coverage: coverage([
      requirement("r1", `The measurements are in dispatch://${KEY}/artifact/notes-md.`),
      requirement("r2", `Read dispatch://${KEY}/spec and dispatch://${KEY}/artifact/notes-md#b-x.`),
      requirement("r3", "Cites nothing."),
    ]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("r1", "requirement"),
          block("r2", "requirement"),
          block("r3", "requirement"),
        ],
      },
    ],
    issue: issueDetails([notes, spec]),
  });

  const chipsOf = (id: string) =>
    (nodeOf(root, blockNodeId(SPEC, id))?.chips ?? []).map((chip) => ({
      name: nameOf(chip),
      ref: chip.ref,
    }));
  expect(chipsOf("r1")).toEqual([
    { name: NODE_TYPE_NAMES.document, ref: `dispatch://${KEY}/artifact/notes-md` },
  ]);
  expect(chipsOf("r2")).toEqual([
    { name: PRESET_NAMES.spec, ref: `dispatch://${KEY}/artifact/${SPEC}` },
  ]);
  expect(chipsOf("r3")).toEqual([]);
});

// Which document plays which part is the id the issue read names for it, or
// nothing, as the tree reads each part's blocks (`primarySpec`, `roleDocument`). An issue read
// that names no spec (`""`) leaves a document's own `primary` flag no say, and a decisions
// document plays no part at all: a decision is a `decision` block of the spec, and the tree reads
// no decisions document's blocks, so a decisions document an earlier release stored - one the
// issue read still names (`decisions_artifact_id`) and whose `role` says so - has no part's word
// either. A chip naming such a document draws the page's word for any document, never
// "Decisions" or "Spec" for a document whose blocks the tree never reads. The document the issue
// does name for a part is named by that part's word.
test("a document chip names a part only where the issue read names that document for it", () => {
  const flagged = artifact("artifact-old-spec", "old-spec-md", "old-spec.md", true);
  const made = {
    ...artifact("artifact-decisions", "decisions-md", "decisions.md"),
    role: "decisions",
  };
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const cited = ["old-spec-md", "decisions-md", "plan-md"].map(
    (slug) => `dispatch://${KEY}/artifact/${slug}`
  );
  const root = treeFrom({
    comments: [
      { ...comment("c1", "2026-11-04T09:00:00Z"), body: `Read ${cited.join(", ")} first.` },
    ],
    issue: {
      ...issueDetails([flagged, made, plan]),
      decisions_artifact_id: made.id,
      plan_artifact_id: plan.id,
      primary_artifact_id: "",
    },
  });

  expect(nodeOf(root, "c1")?.chips.map((chip) => ({ name: nameOf(chip), ref: chip.ref }))).toEqual([
    { name: NODE_TYPE_NAMES.document, ref: cited[0] },
    { name: NODE_TYPE_NAMES.document, ref: cited[1] },
    { name: PRESET_NAMES.plan, ref: cited[2] },
  ]);
});

// A decision sits under the ask it answers, where the tree read places it: under the issue's
// own question, under the question a document's block is, by that block's node, and under the
// issue where it answers none. The ask that places a decision is its spine, so no chip names it.
// Each question here is still open: a decision whose `from_ask` names a question a person
// answered restates their answer and is drawn nowhere (INFRA-1196's decision 513,
// `decision-nodes.test.ts` and `IssueTree.test.tsx`).
test("a decision sits under the ask it answers, and one that answers none under the issue", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const own = question("ask-own");
  const inSpec = question("ask-block", {
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
  });
  const root = treeOf({
    asks: [own, inSpec],
    ...decided(
      [
        { ...decision("d1", "Answers the issue's own question.", []), asks: ["ask-own"] },
        { ...decision("d2", "Answers the spec's question.", []), asks: ["ask-block"] },
        decision("d4", "Answers nothing.", []),
      ],
      { d1: "ask-own", d2: "q1" }
    ),
    documents: [{ artifactId: spec.id, blocks: [block("q1", "ask")] }],
    issue: issueDetails([spec, made]),
  });

  const parents = (id: string) =>
    placementsOf(root, blockNodeId("decisions-md", id)).map((one) => one.parent);
  expect(parents("d1")).toEqual(["ask-own"]);
  expect(parents("d2")).toEqual([blockNodeId(SPEC, "q1")]);
  expect(parents("d4")).toEqual([KEY]);
  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.chips).toEqual([]);
});

// The server builds a decision's links paragraph by paragraph and keeps every repeat
// (`api/decisions.go`), and the tree read answers them on the node as that read built them, so a
// decision whose two paragraphs link one statement names it twice. It is one placement under the
// question the read places it under, counted once in what the question holds, and one chip for
// the statement, stale where either of its links is.
test("a decision naming one question and one statement twice is drawn once, with one chip", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const twice = decision("d1", "Answers one question in two paragraphs.", ["r1", "r1"]);
  const root = treeOf({
    asks: [question("ask-1")],
    ...decided(
      [
        {
          ...twice,
          asks: ["ask-1", "ask-1"],
          links: (twice.links ?? []).map((link, index) => ({ ...link, stale: index === 1 })),
        },
      ],
      { d1: "ask-1" }
    ),
    issue: issueDetails([spec, made]),
  });

  const id = blockNodeId("decisions-md", "d1");
  expect(placementsOf(root, id).map((one) => one.parent)).toEqual(["ask-1"]);
  expect(nodeOf(root, "ask-1")?.held).toBe(1);
  expect(nodeOf(root, id)?.chips.map(({ ref, stale }) => ({ ref, stale }))).toEqual([
    { ref: `dispatch://${KEY}/spec#b-r1`, stale: true },
  ]);
});

// A person's answer is part of the ask and no node of its own. The ask's
// node carries it as the store holds it, who answered, what they chose and what they wrote, and
// the ask holds no child for it.
test("an ask's answer is part of the ask's node and no node of its own", () => {
  const answered = question("ask-1", {
    answer: {
      at: "2026-11-06T00:00:00Z",
      selected: ["1280 px"],
      text: "The preset row keeps its labels.\n\nAbove that width.",
      user: "alice",
    },
    state: "answered",
  });
  const root = treeOf({ asks: [answered], issue: issueDetails([]) });

  const node = nodeOf(root, "ask-1");
  expect(node?.answer).toEqual({
    at: "2026-11-06T00:00:00Z",
    by: { id: "alice", kind: "user" },
    selected: ["1280 px"],
    text: "The preset row keeps its labels.\n\nAbove that width.",
  });
  expect(node?.children).toEqual([]);
  // Nor is it a node under the issue: the ask is the issue's one child.
  expect(root.children.map((child) => child.id)).toEqual(["ask-1"]);
});

// A person's decision is the answered ask itself (`decisionread`): the tree read answers it on
// the ask's own node, its words are the question, and it holds the `shaped` edge of what the
// decision shaped. It is no decision node of its own - one would restate the question under
// the ask, or, for a question asked of the issue itself, stand in the ask's own place - so each
// ask stays one node, with the person's answer on it, and carries a chip for each node its
// decision shaped. That edge is read from the ask's node as an agent's decision's is from its
// own: a statement another node made that the decision says it shaped draws the disagreement,
// one the ask's own block made draws none, and the decision key reaches the ask from what it
// shaped. An agent's decision that settles the answered question is a decision of its own under
// it, and the Decisions filter counts each once.
test("a person's decision is the answered ask's own node, which carries what it shaped", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const answer = { at: "2026-11-06T00:00:00Z", selected: [], text: "One read.", user: "alice" };
  const own = question("ask-own", { answer, state: "answered" });
  const inSpec = question("ask-block", {
    answer,
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
    state: "answered",
  });
  const settling = { ...decision("d1", "Read it once.", []), asks: [inSpec.id] };
  // The tree read answers the person's decision as the answered ask's own node, with what it
  // shaped as that node's `shaped` tuples: the question asked of the issue itself by a tuple of
  // its own, the question in the spec by its block's pointer. It places the agent's decision that
  // settles the question in the spec, a decision block of the spec, under that question.
  const root = treeOf({
    asks: [own, inSpec],
    coverage: coverage([
      requirement("r-origin", "The requirement the other one came from."),
      requirement("r-shaped", "The requirement the person's decision claims."),
      requirement("r1", "The requirement the question in the spec raised."),
    ]),
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("q1", "ask", { shaped: `dispatch://${KEY}/spec#b-r1` }),
          block("r-origin", "requirement"),
          block("r-shaped", "requirement", { from: `dispatch://${KEY}/spec#b-r-origin` }),
          block("r1", "requirement", { from: `dispatch://${KEY}/spec#b-q1` }),
        ],
      },
    ],
    issue: issueDetails([spec]),
    nodes: [
      answerNode("d1", "decision", "q1", {
        decision: settling,
        document: { artifact_id: spec.id, position: 4, slug: SPEC },
        text: settling.text,
      }),
    ],
    tuples: [treeTuple("shaped", own.id, "r-shaped")],
  });

  // Each ask is one node, in its own place, and no decision node stands for either.
  expect(placementsOf(root, "ask-own").map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, blockNodeId(SPEC, "q1")).map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, "ask-block")).toBeUndefined();
  expect(nodeOf(root, "ask-own")).toMatchObject({ state: "answered", type: "ask" });
  expect(
    nodesInOrder(root)
      .filter((node) => node.type === "decision")
      .map((node) => node.id)
  ).toEqual([blockNodeId(SPEC, "d1")]);
  // Each ask carries what its decision shaped.
  expect(nodeOf(root, "ask-own")?.chips.map((chip) => chip.ref)).toEqual([
    `dispatch://${KEY}/spec#b-r-shaped`,
  ]);
  expect(nodeOf(root, blockNodeId(SPEC, "q1"))?.chips.map((chip) => chip.ref)).toEqual([
    `dispatch://${KEY}/spec#b-r1`,
  ]);
  // The edge is read from the ask's node.
  expect(nodeOf(root, blockNodeId(SPEC, "r-shaped"))?.gaps).toEqual(["shaped_disagreement"]);
  expect(nodeOf(root, blockNodeId(SPEC, "r1"))?.gaps).toEqual([]);
  expect(decisionBehind(root, blockNodeId(SPEC, "r-shaped"))?.id).toBe("ask-own");
  // The agent's decision that settles the question in the spec sits under it.
  expect(placementsOf(root, blockNodeId(SPEC, "d1")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "q1"),
  ]);
  expect(filterSignals(root, "").decisions.count).toBe(3);
});

// R4: a reading is an agent's reading of the ask or the decision it reads, never of the issue:
// the store's `part_of` reaches that ask or decision, and the tree read gives it that node as its
// parent. So a reading inside a decision sits under the decision, and one inside an answered ask
// under the ask. A reading in a copy of an answered ask reads the source's ask, and the read
// places it there. A reading no ask or decision holds sits under the issue. Placement is the
// server's: a reading stands under the parent the read gives, a read that gives the copy's
// reading the copy itself placing it there.
test("a reading sits under the ask or the decision it reads", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const answer = { at: "2026-11-06T00:00:00Z", selected: [], text: "One read.", user: "alice" };
  const inSpec = question("ask-block", {
    answer,
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
    state: "answered",
  });
  // The spec's blocks: the answered question, a copy of it with no ask row of its own, the
  // decision, a reading in each and one in none. The tree read answers the decision as a node of
  // its own and places each reading where the store's `part_of` reaches: under the ask or the
  // decision it reads, the reading in the copy under the source's ask, and the one in none under
  // the issue.
  const documents = [
    {
      artifactId: spec.id,
      blocks: [
        block("q1", "ask"),
        block("q1-reading", "reading"),
        block("q2", "ask"),
        block("q2-reading", "reading"),
        block("d1-reading", "reading"),
        block("loose", "reading"),
      ],
    },
  ];
  const parents = { "d1-reading": "d1", "q1-reading": "q1", "q2-reading": "q1" };
  const decided = decision("d1", "Read it once.", []);
  const decisionNode = answerNode("d1", "decision", KEY, {
    decision: decided,
    document: { artifact_id: spec.id, position: 4, slug: SPEC },
    text: decided.text,
  });
  const parentsOf = (root: TreeNode, id: string): string[] =>
    placementsOf(root, blockNodeId(SPEC, id)).map((one) => one.parent);

  const root = treeOf({
    asks: [inSpec],
    documents,
    issue: issueDetails([spec]),
    nodes: [decisionNode],
    parents,
  });
  expect(parentsOf(root, "d1-reading")).toEqual([blockNodeId(SPEC, "d1")]);
  expect(parentsOf(root, "q1-reading")).toEqual([blockNodeId(SPEC, "q1")]);
  expect(nodeOf(root, blockNodeId(SPEC, "q1"))).toMatchObject({ state: "answered", type: "ask" });
  expect(parentsOf(root, "q2-reading")).toEqual([blockNodeId(SPEC, "q1")]);
  expect(parentsOf(root, "loose")).toEqual([KEY]);
  expect(nodeOf(root, blockNodeId(SPEC, "d1-reading"))?.type).toBe("reading");

  // A read that gives the copy's reading the copy itself places it there.
  const inCopy = treeOf({
    asks: [inSpec],
    documents,
    issue: issueDetails([spec]),
    nodes: [decisionNode],
    parents: { ...parents, "q2-reading": "q2" },
  });
  expect(parentsOf(inCopy, "q2-reading")).toEqual([blockNodeId(SPEC, "q2")]);
});

// R9: the issue's own node carries the author the issue read names, so its row draws who
// opened it rather than an author nobody recorded.
test("the issue's own node carries who opened it", () => {
  expect(buildIssueTree({ issue: issueDetails([]) }).author).toEqual({ id: "alice", kind: "user" });
});

// R1: the conversation is one order. The comments and the messages are two reads, each
// counting from zero, so a message sent today drawn among yesterday's comments would be the
// reader reading the conversation out of order.
test("the conversation's turns are drawn by the time each was made", () => {
  const root = treeOf({
    comments: [
      comment("c0", "2026-11-04T09:00:00Z"),
      comment("c1", "2026-11-04T10:00:00Z"),
      comment("c2", "2026-11-04T11:00:00Z"),
    ],
    issue: issueDetails([]),
    // An id that sorts before every comment's, so a tie broken by id would draw it first.
    messages: [thread("a-message", "2026-11-05T09:00:00Z")],
  });

  expect(root.children.map((child) => child.id)).toEqual(["c0", "c1", "c2", "a-message"]);
});

// A question asked of the issue and a comment are one conversation, so both are drawn by the
// time each was made, a question among the comments it was asked between, and the newest write
// of either kind is the outline's last row. Two questions were drawn in the order of their ids
// before, and every question before every comment.
test("a question of the issue is drawn among the turns, by the time it was asked", () => {
  const root = treeOf({
    asks: [
      // Ids that sort against the time each was asked.
      question("z-first", { created_at: "2026-11-04T09:30:00Z" }),
      question("a-last", { created_at: "2026-11-04T12:00:00Z" }),
    ],
    comments: [comment("c0", "2026-11-04T09:00:00Z"), comment("c1", "2026-11-04T10:00:00Z")],
    issue: {
      ...issueDetails([]),
      children: [
        {
          active_at: "2026-11-05T00:00:00Z",
          external_links: [],
          key: "CORE-2",
          status: "todo",
          subtree_done: 0,
          subtree_total: 0,
          title: "A child.",
        },
      ],
    },
  });

  expect(root.children.map((child) => child.id)).toEqual([
    "CORE-2",
    "c0",
    "z-first",
    "c1",
    "a-last",
  ]);
});

/** A delete of a post, as the read of a deleted comment or message carries it: when, and by whom. */
const DELETION: Pick<Comment, "deleted_at" | "deleted_by"> = {
  deleted_at: "2026-11-05T12:00:00Z",
  deleted_by: { id: "alice", kind: "user" },
};

/** The words a deleted post's author wrote, which the fixtures below leave on the read, though the
 *  server sends an empty body, so a node that read them would show it. */
const DELETED_WORDS = "Hold the deploy until Friday.";

/** What a node of a turn holds that a view draws: its marks, its text and reading, and the line
 *  and the words every place that reads a node reads from it (`nodeLineOf`, `nodeWords`). */
function turnMarks(node: TreeNode | undefined) {
  return {
    deletedAt: node?.deletedAt,
    deletedBy: node?.deletedBy,
    editedAt: node?.editedAt,
    line: node === undefined ? undefined : nodeLineOf(node),
    text: node?.text,
    tree: node?.tree,
    words: node === undefined ? undefined : nodeWords(node),
  };
}

// R3 of dispatch://INFRA-1523: a comment its author deleted keeps its place among the turns, and
// its node holds the deleted line in place of its body and no reading of it, so no line, body or
// filter reads a word its author wrote, even where the read still carries one. It carries no
// edit mark: the deleted line already says what became of the text.
test("a deleted comment keeps its place and holds the deleted line, and no word of its body", () => {
  const root = treeOf({
    comments: [
      comment("c0", "2026-11-04T09:00:00Z"),
      {
        ...comment("c1", "2026-11-04T10:00:00Z"),
        ...DELETION,
        body: DELETED_WORDS,
        body_tree: readingOf(DELETED_WORDS),
        edited_at: "2026-11-05T11:00:00Z",
      },
      comment("c2", "2026-11-04T11:00:00Z"),
    ],
    issue: issueDetails([]),
  });

  expect(root.children.map((child) => child.id)).toEqual(["c0", "c1", "c2"]);
  const line = "Deleted by alice · 2026-11-05T12:00:00Z";
  const deleted = turnMarks(nodeOf(root, "c1"));
  expect(deleted).toEqual({
    deletedAt: "2026-11-05T12:00:00Z",
    deletedBy: { id: "alice", kind: "user" },
    editedAt: undefined,
    line,
    text: line,
    tree: undefined,
    words: line,
  });
  expect(nodeOf(root, "c0")?.text).toBe("Comment c0.");
});

// R3 of dispatch://INFRA-1523: a message its author deleted is the same: its place, the deleted
// line, no reading. A session's delete names the session as `AgentName` does before the agent
// registry answers, by its short id, never by its raw id; a delete whose read names nobody says
// so with no name.
test("a deleted message keeps its place and holds the deleted line, and no word of its body", () => {
  const sent = thread("m1", "2026-11-04T10:00:00Z");
  const unnamed = thread("m2", "2026-11-04T11:00:00Z");
  const root = treeOf({
    issue: issueDetails([]),
    messages: [
      thread("m0", "2026-11-04T09:00:00Z"),
      {
        ...sent,
        message: {
          ...sent.message,
          body: DELETED_WORDS,
          body_tree: readingOf(DELETED_WORDS),
          deleted_at: "2026-11-05T12:00:00Z",
          deleted_by: { id: "planner", kind: "session" },
        },
      },
      {
        ...unnamed,
        message: { ...unnamed.message, deleted_at: "2026-11-05T13:00:00Z", deleted_by: null },
      },
    ],
  });

  expect(root.children.map((child) => child.id)).toEqual(["m0", "m1", "m2"]);
  const line = "Deleted by session:planner… · 2026-11-05T12:00:00Z";
  const deleted = turnMarks(nodeOf(root, "m1"));
  expect(deleted).toEqual({
    deletedAt: "2026-11-05T12:00:00Z",
    deletedBy: { id: "planner", kind: "session" },
    editedAt: undefined,
    line,
    text: line,
    tree: undefined,
    words: line,
  });
  const nobody = nodeOf(root, "m2");
  expect(nobody?.text).toBe("Deleted · 2026-11-05T13:00:00Z");
  expect(nobody !== undefined && "deletedBy" in nobody).toBe(false);
  expect(nodeOf(root, "m0")?.text).toBe("Message m0.");
});

// R2 of dispatch://INFRA-1523: a comment its author edited keeps its words and their reading,
// and says when it was last edited; one nobody edited carries no mark.
test("an edited comment keeps its words and its reading, and carries when it was edited", () => {
  const root = treeOf({
    comments: [
      {
        ...comment("c1", "2026-11-04T10:00:00Z"),
        body_tree: readingOf("Comment c1."),
        edited_at: "2026-11-05T11:00:00Z",
      },
      comment("c2", "2026-11-04T11:00:00Z"),
    ],
    issue: issueDetails([]),
  });

  expect(turnMarks(nodeOf(root, "c1"))).toEqual({
    deletedAt: undefined,
    deletedBy: undefined,
    editedAt: "2026-11-05T11:00:00Z",
    line: "Comment c1.",
    text: "Comment c1.",
    tree: readingOf("Comment c1."),
    words: "Comment c1.",
  });
  const unedited = nodeOf(root, "c2");
  expect(unedited !== undefined && "editedAt" in unedited).toBe(false);
});

// R2 of dispatch://INFRA-1523: a message its author edited is the same.
test("an edited message keeps its words and its reading, and carries when it was edited", () => {
  const sent = thread("m1", "2026-11-04T10:00:00Z");
  const root = treeOf({
    issue: issueDetails([]),
    messages: [
      {
        ...sent,
        message: {
          ...sent.message,
          body_tree: readingOf("Message m1."),
          edited_at: "2026-11-05T11:00:00Z",
        },
      },
      thread("m2", "2026-11-04T11:00:00Z"),
    ],
  });

  expect(turnMarks(nodeOf(root, "m1"))).toEqual({
    deletedAt: undefined,
    deletedBy: undefined,
    editedAt: "2026-11-05T11:00:00Z",
    line: "Message m1.",
    text: "Message m1.",
    tree: readingOf("Message m1."),
    words: "Message m1.",
  });
  const unedited = nodeOf(root, "m2");
  expect(unedited !== undefined && "editedAt" in unedited).toBe(false);
});

// R3 of dispatch://INFRA-1523: a chip naming a comment or a message its author deleted draws
// the deleted line, as a link to it does (`useReferenceLabel`), and carries the delete's marks,
// and the page publishes that line as its name, never the words of its body, even where the
// read still carries them. It says nothing of an edit: the deleted line says what became of the
// text.
test("a chip naming a deleted comment or message draws the deleted line and carries its marks", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  for (const kind of ["comment", "message"] as const) {
    const ref = `dispatch://${KEY}/${kind}/${kind}-9`;
    const reads = {
      ...decided([{ ...decision("d1", "Keep it.", []), links: [linkTo(ref, kind)] }]),
      issue: issueDetails([spec, made]),
      targets: [
        {
          deleted_at: "2026-11-05T12:00:00Z",
          deleted_by: { id: "alice", kind: "user" },
          edited_at: "2026-11-05T11:00:00Z",
          gone: false,
          kind,
          name: DELETED_WORDS,
          ref,
        },
      ],
    };

    const chips = nodeOf(treeOf(reads), blockNodeId("decisions-md", "d1"))?.chips;
    expect(chips?.map(drawnChip), kind).toEqual([
      {
        answered: true,
        deletedAt: "2026-11-05T12:00:00Z",
        deletedBy: { id: "alice", kind: "user" },
        gone: false,
        name: "Deleted by alice",
        ref,
        stale: false,
      },
    ]);
    expect([...namesOf(answered(reads))], kind).toEqual([[ref, "Deleted by alice"]]);
  }
});

// R3 of dispatch://INFRA-1523, and INFRA-1196's decision 342: beside the deleted line it publishes
// as a deleted post's name, the page publishes the delete itself (`targetNaming`'s `deleted`), so
// a row's words draw that line with its actor named as the page names an author, where the name
// holds the actor as no registry knows it. A post that stands is not among them, and neither is
// a block of a deleted post, which is gone and named by nothing.
test("the page publishes a deleted comment or message by its delete, and no post that stands", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const deletion = { at: "2026-11-05T12:00:00Z", by: { id: "alice", kind: "user" } };
  for (const kind of ["comment", "message"] as const) {
    const ref = `dispatch://${KEY}/${kind}/${kind}-9`;
    const block = `${ref}#b-p1`;
    const standing = `dispatch://${KEY}/${kind}/${kind}-8`;
    const reads = {
      ...decided([
        {
          ...decision("d1", "Keep it.", []),
          links: [linkTo(ref, kind), linkTo(block, "block"), linkTo(standing, kind)],
        },
      ]),
      issue: issueDetails([spec, made]),
      targets: [
        { deleted_at: deletion.at, deleted_by: deletion.by, gone: false, kind, name: "", ref },
        {
          deleted_at: deletion.at,
          deleted_by: deletion.by,
          gone: true,
          kind: "block",
          name: "",
          ref: block,
        },
        { gone: false, kind, name: "Ship it.", ref: standing },
      ],
    };

    const naming = targetNaming(answered(reads));
    expect([...namesOf(answered(reads))], kind).toEqual([
      [ref, "Deleted by alice"],
      [standing, "Ship it."],
    ]);
    expect([...naming.deleted], kind).toEqual([[ref, deletion]]);
  }
});

// R2 of dispatch://INFRA-1523: a chip naming a comment or a message its author edited, or a
// block of one, is named by its words alone and carries when it was edited; the page publishes
// those words as its name and the reference among the edited ones, so a link cuts the words to
// its own length and marks what the cut left ` · edited`, as a link to it does
// (`useReferenceLabel`). A post nobody edited carries no mark.
test("a chip naming an edited comment, message or block of one carries its words and when it was edited", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const block = `dispatch://${KEY}/comment/comment-9#b-p1`;
  const unedited = `dispatch://${KEY}/comment/comment-8`;
  for (const kind of ["comment", "message"] as const) {
    const ref = `dispatch://${KEY}/${kind}/${kind}-9`;
    const reads = {
      ...decided([
        {
          ...decision("d1", "Keep it.", []),
          links: [linkTo(ref, kind), linkTo(block, "block"), linkTo(unedited, "comment")],
        },
      ]),
      issue: issueDetails([spec, made]),
      targets: [
        { edited_at: "2026-11-05T11:00:00Z", gone: false, kind, name: "**Hold** it.", ref },
        {
          edited_at: "2026-11-05T11:00:00Z",
          gone: false,
          kind: "block",
          name: "Hold the deploy.",
          ref: block,
          text_tree: readingOf("Hold the deploy."),
        },
        { gone: false, kind: "comment", name: "Ship it.", ref: unedited },
      ],
    };

    const chips = nodeOf(treeOf(reads), blockNodeId("decisions-md", "d1"))?.chips;
    expect(
      chips?.map((chip) => ({ editedAt: chip.editedAt, name: nameOf(chip), ref: chip.ref })),
      kind
    ).toEqual([
      { editedAt: "2026-11-05T11:00:00Z", name: "Hold it.", ref },
      { editedAt: "2026-11-05T11:00:00Z", name: "Hold the deploy.", ref: block },
      { editedAt: undefined, name: "Ship it.", ref: unedited },
    ]);
    expect(
      chips?.some((chip) => chip.ref === unedited && "editedAt" in chip),
      kind
    ).toBe(false);
    const naming = targetNaming(answered(reads));
    expect([...namesOf(answered(reads))], kind).toEqual([
      [ref, "Hold it."],
      [block, "Hold the deploy."],
      [unedited, "Ship it."],
    ]);
    expect([...naming.edited], kind).toEqual([ref, block]);
    // A row's link draws the published words, then the mark (`rowReferenceWords`).
    const [link] = rowReferences(`see ${ref}`, [], naming.ambiguous);
    if (link === undefined) throw new Error("the text holds one reference");
    expect(rowReferenceWords(link, namesOf(reads), undefined, naming.edited).words, kind).toBe(
      "Hold it. · edited"
    );
  }
});

// R1, R12: the group of children sits before the conversation however many children there are.
// A child's place in the issue's order is a fraction of one past the group's, as a block's is in
// its document, so a thousandth child never reaches the conversation's orders, which are ordered
// by a time a child does not have.
test("an issue with more than a thousand children draws them in order, the conversation after", () => {
  const children = Array.from(
    { length: 1001 },
    (_, index): IssueChild => ({
      active_at: "2026-11-05T00:00:00Z",
      external_links: [],
      key: `CORE-${index + 2}`,
      status: "todo",
      subtree_done: 0,
      subtree_total: 0,
      title: `Child ${index + 1}.`,
    })
  );
  const root = treeOf({
    asks: [question("a-question", { created_at: "2026-11-04T09:30:00Z" })],
    comments: [comment("c0", "2026-11-04T09:00:00Z")],
    issue: { ...issueDetails([]), children },
  });

  expect(root.children.map((child) => child.id)).toEqual([
    ...children.map((child) => child.key),
    "c0",
    "a-question",
  ]);
});

// An open question waits on the reader only where its turn is the reader's own
// (`waiting_on_person`), as every other count of what waits on them reads it; a question that
// waits on somebody else is open all the same, and the fold still opens the path to it.
test("an open question waits on the reader only where its turn is theirs", () => {
  const asks = [
    question("mine", { waiting_on: "human", waiting_on_person: "alice" }),
    question("theirs", { waiting_on: "human", waiting_on_person: "bob" }),
    question("agents", { waiting_on: "agent", waiting_on_person: "alice" }),
  ];
  const waiting = (viewer: string | undefined): string[] => {
    const root = treeOf({
      asks,
      issue: issueDetails([]),
      ...(viewer === undefined ? {} : { viewer }),
    });
    expect(root.children.map((child) => child.question)).toEqual([true, true, true]);
    return root.children.filter((child) => child.waiting).map((child) => child.id);
  };
  expect(waiting("alice")).toEqual(["mine"]);
  expect(waiting("bob")).toEqual(["theirs"]);
  // Nobody is the reader until the login resolves.
  expect(waiting(undefined)).toEqual([]);
});

// A Change is a comment a person anchored to a decision's words. The fixture is
// the shape the Change control (`DecisionActions.tsx`) stored on a running build of the
// checkpoint (preview of 29111db16c41), read with
// `Dispatch-Trees: 1`: the Change is anchored with the decisions document's id
// (`api/anchors.go`'s `resolveAnchor`) inside the decision's paragraph, so the tree read hangs it
// under the decision's node, which the read answers with the members
// the decisions read stored for it. A Change newer than the decision's words makes the decision
// read Changed by and wait on its agent, not on the reader; one older than its words still
// stands under it and is no Change, and the decision waits for the reader's review.
test("a Change is drawn under its decision, which then waits on its agent", () => {
  // As the reads send them: the spec as the issue read lists it, the decision as the decisions
  // read answers it, the requirements its tie names and the decision's block as the blocks read
  // and the coverage read hold them, and the Change as the comments read sends it.
  const spec: Artifact = {
    approval: { latest_version: 2, state: "draft" },
    created_at: "2026-11-16T11:11:06.995403Z",
    created_by: { id: "alice", kind: "user" },
    id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
    issue_key: "LAY-2",
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "LAY",
    ref_key: "LAY-2/spec",
    session_id: null,
    slug: "spec",
    versions: [
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-11-16T11:11:06.995403Z",
        named: false,
        number: 1,
        size: 1594,
        summary: null,
      },
      {
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-11-16T11:11:07.35439Z",
        named: false,
        number: 2,
        size: 1713,
        summary: null,
      },
    ],
  };
  const reviewed: Decision = {
    asks: [],
    block_id: "4f4f7706-5e2f-4f98-ba08-ba9afa843985",
    changed_at: "2026-11-16T11:11:07.225118Z",
    changed_version: 1,
    heading: "Draw the tree from one read",
    kept: null,
    kind: "agent",
    links: [
      {
        changed_since_tie: false,
        created_at: "2026-11-16T11:11:07.225118Z",
        created_version: 1,
        from_block: "4f4f7706-5e2f-4f98-ba08-ba9afa843985",
        stale: false,
        to: {
          block_id: "r1",
          changed_at: "2026-11-16T11:11:06.995403Z",
          changed_by: [{ id: "alice", kind: "user" }],
          changed_version: 1,
          document: "dispatch://LAY-2/spec",
          id: "LAY-2/spec#r1",
          issue_key: "LAY-2",
          kind: "block",
          made_at: "2026-11-16T11:11:06.995403Z",
          made_by: [{ id: "alice", kind: "user" }],
          parent: {
            id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
            kind: "artifact",
            ref: "dispatch://LAY-2/spec",
          },
          parent_version: 1,
          project: "LAY",
          ref: "dispatch://LAY-2/spec#b-r1",
        },
      },
      {
        changed_since_tie: false,
        created_at: "2026-11-16T11:11:07.225118Z",
        created_version: 1,
        from_block: "4f4f7706-5e2f-4f98-ba08-ba9afa843985",
        stale: false,
        to: {
          block_id: "r2",
          changed_at: "2026-11-16T11:11:06.995403Z",
          changed_by: [{ id: "alice", kind: "user" }],
          changed_version: 1,
          document: "dispatch://LAY-2/spec",
          id: "LAY-2/spec#r2",
          issue_key: "LAY-2",
          kind: "block",
          made_at: "2026-11-16T11:11:06.995403Z",
          made_by: [{ id: "alice", kind: "user" }],
          parent: {
            id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
            kind: "artifact",
            ref: "dispatch://LAY-2/spec",
          },
          parent_version: 1,
          project: "LAY",
          ref: "dispatch://LAY-2/spec#b-r2",
        },
      },
    ],
    ref: "dispatch://LAY-2/spec#b-4f4f7706-5e2f-4f98-ba08-ba9afa843985",
    replaced: false,
    text: ":::decision{#4f4f7706-5e2f-4f98-ba08-ba9afa843985}\n## Draw the tree from one read\n\nChosen so the page makes no request per node. Shapes dispatch://LAY-2/spec#b-r1 and dispatch://LAY-2/spec#b-r2.\n:::\n",
    text_sha256: "86b47522df4be5cf892944e49934a341fcd5be73ebfa3c9677bca941946b6aba",
    text_tree: {
      content: [
        {
          attrs: { blockId: "4f4f7706-5e2f-4f98-ba08-ba9afa843985" },
          content: [
            {
              attrs: { level: 2 },
              content: [{ text: "Draw the tree from one read", type: "text" }],
              type: "heading",
            },
            {
              content: [
                {
                  text: "Chosen so the page makes no request per node. Shapes dispatch://LAY-2/spec#b-r1 and dispatch://LAY-2/spec#b-r2.",
                  type: "text",
                },
              ],
              type: "paragraph",
            },
          ],
          type: "decision",
        },
      ],
      type: "doc",
    },
    unsure: false,
    waiting_review: true,
  };
  const blocks: ArtifactBlock[] = [
    {
      embedding: "pending",
      from: 251,
      id: "r1",
      references: { asks: 0, comments: 0 },
      to: 449,
      token: "sha256:3f5b5138044f406f940b91f745999c621fdf44a8d4ec8b9a3f0c8108ac003797",
      type: "requirement",
    },
    {
      embedding: "pending",
      from: 570,
      id: "r2",
      references: { asks: 0, comments: 0 },
      to: 651,
      token: "sha256:498e9451a36332ad527069faa33c552c1d5c66c47390fd7de2eff3d24d5b342a",
      type: "requirement",
    },
    // The decision's block, and the paragraph inside it the Change was written in, whose markdown
    // range lies inside the decision's.
    {
      from: 653,
      id: "4f4f7706-5e2f-4f98-ba08-ba9afa843985",
      references: { asks: 0, comments: 1 },
      to: 864,
      token: "4f4f7706-5e2f-4f98-ba08-ba9afa843985",
      type: "decision",
    },
    {
      from: 732,
      id: "679d4ee9-addc-4934-9fb6-ef4cdf5f8d72",
      references: { asks: 0, comments: 1 },
      to: 859,
      token: "679d4ee9-addc-4934-9fb6-ef4cdf5f8d72",
      type: "paragraph",
    },
  ];
  const requirements: RequirementCoverage[] = [
    {
      block_id: "r1",
      checks: 1,
      gaps: [],
      plan_tasks: 1,
      ref: "dispatch://LAY-2/spec#b-r1",
      text: "Every property of the issue that carries a value is visible at every width the page draws, with no control opened and no sideways scroll. The right-hand panel holds the rest.",
      text_tree: {
        content: [
          {
            content: [
              {
                text: "Every property of the issue that carries a value is visible at every width the page draws, with no control opened and no sideways scroll. The right-hand panel holds the rest.",
                type: "text",
              },
            ],
            type: "paragraph",
          },
        ],
        type: "doc",
      },
    },
    {
      block_id: "r2",
      checks: 1,
      gaps: [],
      plan_tasks: 1,
      ref: "dispatch://LAY-2/spec#b-r2",
      text: "Requirement 2 of the redesign stands on a row of its own.",
      text_tree: {
        content: [
          {
            content: [
              { text: "Requirement 2 of the redesign stands on a row of its own.", type: "text" },
            ],
            type: "paragraph",
          },
        ],
        type: "doc",
      },
    },
  ];
  const change: CommentWithTrees = {
    anchor: {
      artifact_id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
      block_id: "679d4ee9-addc-4934-9fb6-ef4cdf5f8d72",
      mark_id: "66fd1e9f-1adb-4c92-b7db-a731dcf223c1",
      orphaned: false,
      quote:
        "Chosen so the page makes no request per node. Shapes dispatch://LAY-2/spec#b-r1 and dispatch://LAY-2/spec#b-r2.",
      version: 1,
    },
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "implementer", kind: "user" },
    body: "Please read the tree from the reads the page already makes, and say which read sends the names.",
    body_tree: {
      content: [
        {
          content: [
            {
              text: "Please read the tree from the reads the page already makes, and say which read sends the names.",
              type: "text",
            },
          ],
          type: "paragraph",
        },
      ],
      type: "doc",
    },
    created_at: "2026-11-16T11:12:42.406314Z",
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id: "66fd1e9f-1adb-4c92-b7db-a731dcf223c1",
    issue_key: "LAY-2",
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
  const issue: IssueDetails = {
    ...issueDetails([spec]),
    key: "LAY-2",
    primary_artifact_id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
    project: "LAY",
  };
  const treeWith = (createdAt: string): TreeNode => {
    const { nodes, tuples } = decided([reviewed], {}, made);
    return treeOf({
      coverage: {
        gaps: [],
        has_plan: true,
        issue: "LAY-2",
        requirements,
        spec_artifact_id: "98141a74-a34b-4cc1-b58c-57b6de4ed1f4",
      },
      documents: [{ artifactId: spec.id, blocks }],
      issue,
      nodes: [
        ...nodes,
        answerNode(change.id, "comment", reviewed.block_id, {
          author: [change.author],
          made_at: createdAt,
          text: change.body,
          ...(change.body_tree === undefined ? {} : { text_tree: change.body_tree }),
        }),
      ],
      tuples,
      viewer: "implementer",
    });
  };
  const id = blockNodeId(made.slug, reviewed.block_id);

  const changed = treeWith(change.created_at);
  expect(placementsOf(changed, change.id).map((one) => one.parent)).toEqual([id]);
  expect(nodeOf(changed, id)).toMatchObject({ state: "change_requested", waiting: false });

  // The same Change, made before the decision's words last changed (`changed_at`).
  const older = treeWith("2026-11-16T11:11:07.1Z");
  expect(placementsOf(older, change.id).map((one) => one.parent)).toEqual([id]);
  expect(nodeOf(older, id)).toMatchObject({ state: "waiting_review", waiting: true });
});

// A person may ask two changes of one decision before its agent edits it. The read hangs
// each comment anchored in the decision's own block under the decision, and the decision reads
// changed; a comment older than the decision's words stands under it too and is no change. On the
// decision's words, none of them is a row (R9): the margin beside the decision draws them.
test("every Change newer than a decision's words changes it, and none is a row", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const reviewed: Decision = {
    ...decision("d1", "## Keep it\n\nKeep the model pure.", ["r1"]),
    changed_at: "2026-11-05T00:00:00Z",
    waiting_review: true,
  };
  const change = (id: string, createdAt: string): Comment => ({
    ...comment(id, createdAt),
    anchor: {
      artifact_id: made.id,
      block_id: "d1",
      mark_id: id,
      orphaned: false,
      quote: "Keep the model pure.",
      version: 1,
    },
  });
  const root = treeOf({
    comments: [
      change("c-older", "2026-11-04T00:00:00Z"),
      change("c-first", "2026-11-06T00:00:00Z"),
      change("c-second", "2026-11-07T00:00:00Z"),
    ],
    coverage: coverage([requirement("r1", "The first.")]),
    ...decided([reviewed]),
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec, made]),
    viewer: "alice",
  });
  const id = blockNodeId("decisions-md", "d1");

  for (const one of ["c-first", "c-second", "c-older"]) {
    expect(placementsOf(root, one), `${one}, a comment on the decision's words`).toEqual([]);
  }
  expect(nodeOf(root, id)).toMatchObject({ state: "change_requested", waiting: false });
});

// R3: a decision node's Change is the comment the read hangs under it - a comment anchored inside
// a decision's words hangs under that decision's node - here the second decision's, though the
// first decision holds the same words too. So the Change lands on
// the decision the person changed, which reads Changed by and leaves the review list,
// and not on the first that holds its words, which still waits on them. On the decision's words,
// the Change is no row (R9): the margin draws it.
test("a Change anchored in one decision changes it, though another decision holds its words", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const waiting = (id: string, text: string): Decision => {
    const one = decision(id, text, ["r1"]);
    return {
      ...one,
      changed_at: "2026-11-05T00:00:00Z",
      // The tie comes from the decision's paragraph, as the server stores it.
      links: one.links.map((link) => ({ ...link, from_block: `${id}-words` })),
      waiting_review: true,
    };
  };
  const first = waiting("d1", "## Keep the model pure\n\nKeep the model pure. Draw it once.");
  const second = waiting("d2", "## Draw it once\n\nDraw it once.");
  const change: Comment = {
    ...comment("c-change", "2026-11-06T00:00:00Z"),
    anchor: {
      artifact_id: made.id,
      block_id: "d2",
      mark_id: "c-change",
      orphaned: false,
      quote: "Draw it once.",
      version: 1,
    },
  };
  const root = treeOf({
    comments: [change],
    coverage: coverage([requirement("r1", "The first.")]),
    ...decided([first, second]),
    documents: [{ artifactId: spec.id, blocks: [block("r1", "requirement")] }],
    issue: issueDetails([spec, made]),
    viewer: "alice",
  });
  const changed = blockNodeId("decisions-md", "d2");
  const other = blockNodeId("decisions-md", "d1");

  expect(placementsOf(root, "c-change"), "a comment on the decision's words").toEqual([]);
  expect(nodeOf(root, changed)).toMatchObject({ state: "change_requested", waiting: false });
  expect(nodeOf(root, other)).toMatchObject({ state: "waiting_review", waiting: true });
});

// R14: a reply is drawn under the entry it answers. The write climbs `reply_to` to the head of
// the thread before it stores, so a tree parented by `reply_to` draws every reply of a thread
// under its head; `answers` is the comment the write named, which is what nests a reply under
// the reply it answers.
test("a reply is drawn under the entry it answers", () => {
  const head = comment("c0", "2026-11-04T09:00:00Z");
  const first = { ...comment("c1", "2026-11-04T10:00:00Z"), answers: "c0", reply_to: "c0" };
  const second = { ...comment("c2", "2026-11-04T11:00:00Z"), answers: "c1", reply_to: "c0" };
  const root = treeOf({ comments: [head, first, second], issue: issueDetails([]) });

  expect(placementsOf(root, "c2").map((one) => one.parent)).toEqual(["c1"]);
  // The rest of a turn's placement is unchanged: the head sits under the issue and the first
  // reply under the head, so the reply still belongs to its thread.
  expect(placementsOf(root, "c1").map((one) => one.parent)).toEqual(["c0"]);
  expect(placementsOf(root, "c0").map((one) => one.parent)).toEqual([KEY]);
});

// R14: a comment stored before `comments.answers` existed carries no answered entry, and its
// thread's head is then the one entry it names.
test("a reply carrying no answered entry is drawn under its thread's head", () => {
  const head = comment("c0", "2026-11-04T09:00:00Z");
  const older = { ...comment("c1", "2026-11-04T10:00:00Z"), reply_to: "c0" };
  const root = treeOf({ comments: [head, older], issue: issueDetails([]) });

  expect(placementsOf(root, "c1").map((one) => one.parent)).toEqual(["c0"]);
  expect(placementsOf(root, "c0").map((one) => one.parent)).toEqual([KEY]);
});

// R16: a reply to an ask names the ask alone (`ask_id`), and is drawn
// under the ask's node whatever state the ask is in - an open, an answered and a resolved ask
// alike, and an ask indexed from a block by that block's node.
test("a reply to an ask is drawn under the ask, whatever state it is in", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const asks = [
    question("ask-open"),
    question("ask-answered", { state: "answered" }),
    question("ask-resolved", {
      resolution: {
        actor: { id: "planner", kind: "session" },
        at: "2026-11-06T00:00:00Z",
        kind: "resolved",
        reason: "Settled.",
      },
      state: "resolved",
    }),
    question("ask-block", {
      block_artifact: { id: spec.id, primary: true, slug: SPEC },
      block_id: "q1",
    }),
  ];
  const replies = asks.map((ask, index) => ({
    ...comment(`under-${ask.id}`, `2026-11-07T0${index}:00:00Z`),
    ask_id: ask.id,
  }));
  const root = treeOf({
    asks,
    comments: replies,
    documents: [{ artifactId: spec.id, blocks: [block("q1", "ask")] }],
    issue: issueDetails([spec]),
  });

  for (const id of ["ask-open", "ask-answered", "ask-resolved"]) {
    expect(
      placementsOf(root, `under-${id}`).map((one) => one.parent),
      id
    ).toEqual([id]);
  }
  expect(placementsOf(root, "under-ask-block").map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "q1"),
  ]);
});

// A comment thread tied to no text that no person wrote in, and a message thread the Legion
// daemon began, are no node of the outline: the history in the panel draws them. A comment
// thread a person wrote in is the conversation's, the agents' turns in it included, and a reply
// to an ask is in the ask's thread.
test("a thread no person wrote in is no node, and one a person replied in is drawn whole", () => {
  const agent = { id: "planner", kind: "session" as const };
  const status = { ...comment("status", "2026-11-04T09:00:00Z"), author: agent };
  const among = { ...comment("among", "2026-11-04T10:00:00Z"), author: agent };
  const answer = {
    ...comment("answer", "2026-11-04T11:00:00Z"),
    answers: "among",
    author: { id: "reviewer", kind: "session" as const },
    reply_to: "among",
  };
  const joined = { ...comment("joined", "2026-11-04T12:00:00Z"), author: agent };
  const person = {
    ...comment("person", "2026-11-04T13:00:00Z"),
    answers: "joined",
    reply_to: "joined",
  };
  const underAsk = {
    ...comment("under-ask", "2026-11-04T14:00:00Z"),
    ask_id: "ask-1",
    author: agent,
  };
  const daemon = thread("daemon", "2026-11-04T15:00:00Z");
  const root = treeOf({
    asks: [question("ask-1")],
    comments: [status, among, answer, joined, person, underAsk],
    issue: issueDetails([]),
    messages: [
      {
        ...daemon,
        message: { ...daemon.message, author: { id: "legion-daemon:CORE", kind: "session" } },
      },
    ],
  });

  for (const id of ["status", "among", "answer", "daemon"]) {
    expect(nodeOf(root, id), `${id} is drawn in the outline`).toBeUndefined();
  }
  expect(placementsOf(root, "joined").map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, "person").map((one) => one.parent)).toEqual(["joined"]);
  expect(placementsOf(root, "under-ask").map((one) => one.parent)).toEqual(["ask-1"]);
});

// The tree draws no approval request, so the replies under one are in no node's thread. Those
// only agents wrote, under a request no person answered, are no node of the outline, and the
// address of one lands in the history, as any thread only agents wrote in does. Once a person
// writes under the request the thread is the conversation's, the agents' turns in it included,
// drawn under the issue, since the request itself is no node.
test("an agent's reply under an approval request is no node until a person replies there", () => {
  const agent = { id: "planner", kind: "session" as const };
  const asks = [
    question("approval-quiet", { kind: "approval" }),
    question("approval-joined", { kind: "approval" }),
  ];
  const comments = [
    { ...comment("quiet", "2026-11-04T09:00:00Z"), ask_id: "approval-quiet", author: agent },
    { ...comment("asked", "2026-11-04T10:00:00Z"), ask_id: "approval-joined", author: agent },
    { ...comment("answered", "2026-11-04T11:00:00Z"), ask_id: "approval-joined" },
  ];
  const root = treeOf({ asks, comments, issue: issueDetails([]) });

  expect(nodeOf(root, "approval-quiet")).toBeUndefined();
  expect(nodeOf(root, "quiet"), "the agents' reply is drawn in the outline").toBeUndefined();
  expect(placementsOf(root, "asked").map((one) => one.parent)).toEqual([KEY]);
  expect(placementsOf(root, "answered").map((one) => one.parent)).toEqual([KEY]);
  const landing = (id: string) => agentsOnlyItem({ id, kind: "item" }, comments, undefined, asks);
  expect(landing("quiet")).toBe("quiet");
  expect(landing("asked")).toBeUndefined();
});

// R21: an ask the server writes `resolved` stands in the state its resolution names, so the
// row says what the card and the margin beside it say. A declined question drawn "Resolved"
// would have the page contradicting itself about the same question.
test("a declined ask is drawn Declined, as the card beside it is", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const declining: AskResolution = {
    actor: { id: "alice", kind: "user" },
    at: "2026-11-06T00:00:00Z",
    kind: "declined",
    reason: "The release owner decides this one.",
  };
  const declined: Ask = {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_artifact: { id: spec.id, primary: true, slug: SPEC },
    block_id: "q1",
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which width does the tree read at?",
    resolution: declining,
    state: "resolved",
    urgency: "med",
  };
  const root = treeOf({
    asks: [declined],
    documents: [{ artifactId: spec.id, blocks: [block("q1", "ask")] }],
    issue: issueDetails([spec]),
  });

  const node = nodeOf(root, blockNodeId(SPEC, "q1"));
  expect(node?.state).toBe("declined");
  expect(nodeStateWord(node?.state ?? "open")).toBe(askResolutionBadge(declining));
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// nobody asked for. A check is held to neither - a check is there to prove a requirement and
// attempts no declaration at all - so the gap is the plan task's alone.
test("a plan task that names no requirement and no declaration draws the gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      {
        artifactId: plan.id,
        blocks: [
          block("t1", "plan_task", { serves: `dispatch://${KEY}/spec#b-r1` }),
          block("t2", "plan_task"),
          // The planned declarations read lands as this pointer, read from
          // the same `block[]` attributes every other pointer is read from.
          block("t3", "plan_task", {
            attempts: `dispatch://${KEY}/artifact/declarations-md#b-d1`,
          }),
        ],
      },
      { artifactId: checks.id, blocks: [block("c1", "check")] },
    ],
    issue: issueDetails([spec, plan, checks]),
  });

  expect(nodeOf(root, blockNodeId("plan-md", "t1"))?.gaps).toEqual([]);
  expect(nodeOf(root, blockNodeId("plan-md", "t2"))?.gaps).toEqual(["nobody_asked"]);
  expect(nodeOf(root, blockNodeId("plan-md", "t3"))?.gaps).toEqual([]);
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.gaps).toEqual([]);
});

// R10: a `serves` naming a node that is no longer a requirement draws the gap on the node
// holding the citation, and places nothing by it: the check keeps its place under the issue.
test("a check whose citation names no requirement draws the gap, and keeps its place", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    coverage: coverage([requirement("r1", "The first.")]),
    documents: [
      {
        artifactId: spec.id,
        // `r-was` is the retyped target: the block the citation names is a callout now.
        blocks: [block("r1", "requirement"), block("r-was", "callout")],
      },
      {
        artifactId: checks.id,
        blocks: [
          block("c1", "check", { serves: `dispatch://${KEY}/spec#b-r-was` }),
          block("c2", "check", { serves: `dispatch://${KEY}/spec#b-r1` }),
        ],
      },
    ],
    issue: issueDetails([spec, checks]),
  });

  expect(nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.gaps).toEqual(["serves_not_requirement"]);
  expect(placementsOf(root, blockNodeId(VERIFICATION, "c1")).map((one) => one.parent)).toEqual([
    KEY,
  ]);
  // The check whose citation names a requirement draws no gap and sits under it.
  expect(nodeOf(root, blockNodeId(VERIFICATION, "c2"))?.gaps).toEqual([]);
  expect(placementsOf(root, blockNodeId(VERIFICATION, "c2")).map((one) => one.parent)).toEqual([
    blockNodeId(SPEC, "r1"),
  ]);
});

// R10: a check is held to no gap about its results. A result that records no negative control
// is one the server is to refuse, so the lack names nothing the page could hold.
test("a check whose latest result records no negative control draws no gap", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    documents: [{ artifactId: checks.id, blocks: [block("c1", "check")] }],
    issue: issueDetails([spec, checks]),
    results: checkedResults([{ blockId: "c1", results: [recorded("result-latest", "")] }]),
  });

  // The result is drawn under its check, and no node of the tree - the check, the result or any
  // other - draws a gap for the control it lacks.
  expect(placementsOf(root, "result-latest").map((one) => one.parent)).toEqual([
    blockNodeId(VERIFICATION, "c1"),
  ]);
  expect(nodesInOrder(root).flatMap((node) => node.gaps)).toEqual([]);
});

// A statement is a `requirement` block of the issue's own spec. One of the plan
// keeps its type and is a block and nothing more: no row of the Spec filter and none of its
// count, no statement of the Verification body, and nothing placed under it - a check whose
// `serves` names it keeps its place under the issue and draws the gap that says it serves no
// statement. The spec's own requirement blocks are statements before the coverage read answers
// as after it.
test("a requirement block outside the spec is no statement, and the spec's are, before coverage and after", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const statement = blockNodeId(SPEC, "r1");
  const planned = blockNodeId("plan-md", "r9");
  const treeOf = (covered: boolean): TreeNode =>
    treeFrom({
      ...(covered ? { coverage: coverage([requirement("r1", "The spec's own.")]) } : {}),
      documents: [
        { artifactId: spec.id, blocks: [block("r1", "requirement")] },
        { artifactId: plan.id, blocks: [block("r9", "requirement")] },
        {
          artifactId: checks.id,
          blocks: [
            block("c1", "check", { serves: `dispatch://${KEY}/spec#b-r1` }),
            block("c9", "check", { serves: `dispatch://${KEY}/artifact/plan-md#b-r9` }),
          ],
        },
      ],
      issue: {
        ...issueDetails([spec, plan, checks]),
        plan_artifact_id: plan.id,
        verification_artifact_id: checks.id,
      },
    });

  for (const covered of [false, true]) {
    const read = covered ? "after the coverage read" : "before the coverage read";
    const root = treeOf(covered);
    expect(nodeOf(root, statement)?.statement, read).toBe(true);
    expect(nodeOf(root, planned)?.type, read).toBe("requirement");
    expect(nodeOf(root, planned)?.statement, read).toBeUndefined();
    // The Spec filter draws and counts the spec's statement alone.
    expect(
      presetView(root, "spec", "").root.children.map((row) => row.id),
      read
    ).toEqual([statement]);
    expect(filterSignals(root, "").spec, read).toEqual({ count: 1 });
    // The Verification body reads the spec's statement alone.
    const body = verificationReading({
      checks: [],
      view: presetView(root, "verification", "").root,
    });
    expect(
      body.statements.map((held) => held.node.id),
      read
    ).toEqual([statement]);
    expect(statementCounts(body.statements), read).toEqual({ met: 0, not_met: 0, unknown: 1 });
    // A check serving the spec's statement sits under it; one serving the plan's block does not.
    expect(
      placementsOf(root, blockNodeId(VERIFICATION, "c1")).map((one) => one.parent),
      read
    ).toEqual([statement]);
    expect(
      placementsOf(root, blockNodeId(VERIFICATION, "c9")).map((one) => one.parent),
      read
    ).toEqual([KEY]);
    expect(nodeOf(root, planned)?.children, read).toEqual([]);
    expect(nodeOf(root, blockNodeId(VERIFICATION, "c9"))?.gaps, read).toEqual([
      "serves_not_requirement",
    ]);
  }
});

// A `check` block of a document other than the issue's Verification document
// is a block and nothing more. The results read never answers for it, so it draws no state word
// - "No result" would be drawn on it for ever - and it is no check of the Verification filter's
// rows or count, or of the statement it serves. The Verification
// document's own check reads No result until the results read answers, and then as it stands.
test("a check block outside the Verification document draws no state and is counted nowhere", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const statement = blockNodeId(SPEC, "r1");
  const verified = blockNodeId(VERIFICATION, "c1");
  const planned = blockNodeId("plan-md", "c9");
  const serves = { serves: `dispatch://${KEY}/spec#b-r1` };
  const results = checkedResults([
    { blockId: "c1", results: [recorded("result-pass", "Break it and it fails.")] },
  ]);
  const treeOf = (answered: boolean): TreeNode =>
    treeFrom({
      coverage: coverage([{ ...requirement("r1", "The spec's own."), checks: 1 }]),
      documents: [
        { artifactId: spec.id, blocks: [block("r1", "requirement")] },
        { artifactId: plan.id, blocks: [block("c9", "check", serves)] },
        { artifactId: checks.id, blocks: [block("c1", "check", serves)] },
      ],
      issue: {
        ...issueDetails([spec, plan, checks]),
        plan_artifact_id: plan.id,
        verification_artifact_id: checks.id,
      },
      ...(answered ? { results } : {}),
    });

  const unread = treeOf(false);
  expect(nodeOf(unread, verified)?.state).toBe("no-result");
  expect(nodeOf(unread, planned)?.type).toBe("check");
  expect(nodeOf(unread, planned)?.state).toBeUndefined();

  const root = treeOf(true);
  expect(nodeOf(root, verified)?.state).toBe("passed");
  expect(nodeOf(root, planned)?.state).toBeUndefined();
  // The Verification filter draws and counts the Verification document's check alone.
  const view = presetView(root, "verification", "");
  expect(view.root.children.map((row) => row.children.map((child) => child.id))).toEqual([
    [verified],
  ]);
  expect(filterSignals(root, "").verification).toEqual({ count: 1 });
  // The statement is read from its one check, which passed.
  const answered = results.checks ?? [];
  expect(
    verificationReading({ checks: answered, view: view.root }).statements.map((held) =>
      held.checks.map((one) => one.node.id)
    )
  ).toEqual([[verified]]);
  expect(statementStandings(root, answered).get(statement)).toEqual({
    covered: true,
    state: "met",
  });
  // No node opens folded (R11): the statement is drawn unfolded, and every node under it, the
  // check outside the Verification document among them, is a row.
  const rows = treeRows(root);
  expect(rows.find((row) => row.kind === "node" && row.node.id === statement)).toMatchObject({
    expanded: true,
  });
  for (const id of [verified, planned]) {
    expect(
      rows.some((row) => row.kind === "node" && row.node.id === id),
      id
    ).toBe(true);
  }
});

/** The state the page reads for the one check of a Verification document: what `payload`
 *  changes about the check the server sends, over the results recorded against it. */
function stateOfOneCheck(
  payload: Partial<CheckBlock>,
  results: readonly CheckResult[]
): string | undefined {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    documents: [{ artifactId: checks.id, blocks: [block("c1", "check")] }],
    issue: issueDetails([spec, checks]),
    results: checkedResults([{ blockId: "c1", payload, results }]),
  });
  return nodeOf(root, blockNodeId(VERIFICATION, "c1"))?.state;
}

/** The mark a retraction leaves on the result it withdrew (`CheckResultRetracted`). */
const WITHDRAWN: NonNullable<CheckResult["retracted"]> = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-11-06T00:00:00Z",
  reason: "The run was against the wrong branch.",
};

// R15: a check whose one pass was withdrawn is not a check that failed, and the page drew both
// as "Failed" - the server answers `no` to a pass standing either way. A reader who cannot tell
// the two apart is told a wrong word, which is worse than no word at all.
test("a check whose only pass was retracted is drawn as retracted, not as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
    ])
  ).toBe("retracted");
  expect(nodeStateWord("retracted")).toBe("Retracted");
});

test("a check whose result stands and failed is still drawn as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
  // A withdrawn pass under a fail that stands says nothing about how the check stands: what
  // the reader is owed is the fail.
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
    ])
  ).toBe("failed");
});

// The server counts a retracted result as no evidence at all (`results_comparison.go`'s
// `passStandsOf` skips it), so the page reads how a check stands from the results that still
// stand. Read from the withdrawn newest result instead, a pass taken back at the compared commit
// over an older pass at another commit was drawn "Failed", although nothing failed. The result
// that stands is a pass at work the check no longer compares with, which is what "Out of date"
// says; "Retracted" is for a check that has nothing standing at all.
test("a retracted pass over an older pass at another commit is out of date, not failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
      recorded("result-elsewhere", "Keep the pass and it stands.", {
        commit_sha: "d".repeat(40),
        commit_stands: { answer: "other_commit" },
      }),
    ])
  ).toBe("older");
  expect(nodeStateWord("older")).toBe("Out of date");
});

// The same rule, from the other side: a pass withdrawn after a fail leaves the fail as the
// newest result that stands, and the server's `no` is that fail. Whatever commit the withdrawn
// result ran at, it does not move the check out of date.
test("a retracted pass over an older fail that stands is drawn as failed", () => {
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", { retracted: WITHDRAWN }),
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
  expect(
    stateOfOneCheck({ pass_stands: { answer: "no" } }, [
      recorded("result-withdrawn", "Keep the pass and it stands.", {
        commit_sha: "d".repeat(40),
        commit_stands: { answer: "other_commit" },
        retracted: WITHDRAWN,
      }),
      recorded("result-failed", "Fix the fault and it passes.", { failed: 1, verdict: "fail" }),
    ])
  ).toBe("failed");
});

// The results read answers newest first, and `checkState` took the first result a retraction
// left standing as the newest, while `checkStanding` and the Verification filter's body sort them
// by the time each was recorded. Sent oldest first, a fail at the compared commit under a newer
// pass at another commit read Failed, where read newest first it read Out of date. Every reading
// sorts by time now, so the order the read sends changes no state. The state it reads is the
// latest evidence at the check's basis: the fail there, which the newer pass at another commit
// says nothing about, so it reads Failed in both orders, and the order still changes nothing.
test("a check reads one state whatever order the read sends its results in", () => {
  const failed = recorded("result-failed", "Fix the fault and it passes.", {
    created_at: "2026-11-05T00:00:00Z",
    failed: 1,
    verdict: "fail",
  });
  const elsewhere = recorded("result-elsewhere", "Keep the pass and it stands.", {
    commit_sha: "d".repeat(40),
    commit_stands: { answer: "other_commit" },
    created_at: "2026-11-06T00:00:00Z",
  });
  for (const [order, results] of [
    ["newest first", [elsewhere, failed]],
    ["oldest first", [failed, elsewhere]],
  ] as const) {
    expect(
      checkStanding({ pass_stands: { answer: "no" }, results: [...results] }),
      order
    ).toMatchObject({ stands: { id: "result-failed" }, state: "failed" });
    expect(stateOfOneCheck({ pass_stands: { answer: "no" } }, results), order).toBe("failed");
  }
});

// The latest evidence at the check's basis decides, and a newer result at another
// commit says nothing about the basis. A check passed at its basis and then failed there is
// Failed, though the server still counts the older pass as standing (`pass_stands` is yes) and
// a newer fail at another commit is the newest result of all.
test("a pass and then a fail at the basis read Failed under a newer fail elsewhere", () => {
  const results = [
    recorded("result-elsewhere", "Fix the fault and it passes.", {
      commit_sha: "d".repeat(40),
      commit_stands: { answer: "other_commit" },
      created_at: "2026-11-07T00:00:00Z",
      failed: 1,
      verdict: "fail",
    }),
    recorded("result-failed", "Fix the fault and it passes.", {
      created_at: "2026-11-06T00:00:00Z",
      failed: 1,
      verdict: "fail",
    }),
    recorded("result-passed", "Break it and it fails.", { created_at: "2026-11-05T00:00:00Z" }),
  ];

  expect(checkStanding({ pass_stands: { answer: "yes" }, results })).toMatchObject({
    stands: { id: "result-failed" },
    state: "failed",
  });
  expect(stateOfOneCheck({ pass_stands: { answer: "yes" } }, results)).toBe("failed");
});

// The deployed server sends no `pass_stands` at all, and Dispatch rolls out with two builds
// behind one load balancer, so a page meets a payload without it. Reading it unguarded threw
// inside `buildIssueTree`, which is every preset of the issue page and not the checks alone.
test("a check the server sends no pass_stands for says the page could not read its state", () => {
  expect(stateOfOneCheck({ pass_stands: undefined }, [recorded("result-only", "A control.")])).toBe(
    "unreadable"
  );
  expect(nodeStateWord("unreadable")).toBe("Could not read");
});

/** The server's reading of a text holding `words`, as one paragraph: the shape the reads send
 *  beside a stored text (`DisplayTree`). */
function readingOf(words: string): DisplayTree {
  return {
    content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
    type: "doc",
  };
}

// R20: a read that sends the server's reading of a node's text hands that reading to the node,
// which draws it (`NodeRow.tsx`); the record the text is goes with it, so its references link
// as they do on every other surface. A read that sends none - a statement's, a check's and a
// result's from a server that predates `text_tree` and `observed_tree` - leaves its node holding
// none: the page parses no stored markdown to make one up.
test("a node holds the server's reading of its text where its read sends one", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const question: Ask = {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which **width**?",
    state: "open",
    urgency: "med",
  };
  const message = thread("m1", "2026-11-05T10:00:00Z");
  const root = treeOf({
    asks: [{ ...question, question_tree: readingOf("ask") }],
    // The tree read answers every node by its own id, so the comment's is not the check's.
    comments: [
      { ...comment("comment-1", "2026-11-05T09:00:00Z"), body_tree: readingOf("comment") },
    ],
    coverage: coverage([requirement("r1", "The **first**.")]),
    ...decided([{ ...decision("d1", "## Keep it", []), text_tree: readingOf("decision") }]),
    issue: issueDetails([spec, checks, made]),
    messages: [{ ...message, message: { ...message.message, body_tree: readingOf("message") } }],
    results: checkedResults([{ blockId: "c1", results: [recorded("res-1", "A control.")] }]),
  });

  const held = (id: string) => {
    const node = nodeOf(root, id);
    return { source: node?.source, tree: node?.tree };
  };
  expect(held("decisions-md#b-d1")).toEqual({
    source: { id: "artifact-decisions", issue: KEY, kind: "artifact" },
    tree: readingOf("decision"),
  });
  expect(held("ask-1")).toEqual({
    source: { id: "ask-1", issue: KEY, kind: "ask" },
    tree: readingOf("ask"),
  });
  expect(held("comment-1")).toEqual({
    source: { id: "comment-1", issue: KEY, kind: "comment" },
    tree: readingOf("comment"),
  });
  expect(held("m1")).toEqual({
    source: { id: "m1", issue: KEY, kind: "message" },
    tree: readingOf("message"),
  });
  for (const id of [blockNodeId(SPEC, "r1"), blockNodeId(VERIFICATION, "c1"), "res-1"]) {
    expect({ id, ...held(id) }).toEqual({ id, source: undefined, tree: undefined });
  }
});

// The coverage read's and the results read's plain words leave out every
// `dispatch://` word of a statement and a check, and a block's stored excerpt stops at 480 runes.
// Each read also sends the server's reading of the whole text (`text_tree`), which keeps
// every reference, and a node's words are that reading's (`nodeWords`).
test("the words of a statement, a check and a block keep the references of their reading", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const cited = "dispatch://CORE-7";
  const reading = readingOf(`Read ${cited} first.`);
  const root = treeOf({
    coverage: coverage([{ ...requirement("r1", "Read first."), text_tree: reading }]),
    documents: [
      { artifactId: spec.id, blocks: [block("r1", "requirement")] },
      { artifactId: checks.id, blocks: [block("c1", "check")] },
      { artifactId: plan.id, blocks: [block("t1", "plan_task")] },
    ],
    issue: issueDetails([spec, checks, plan]),
    results: checkedResults([
      { blockId: "c1", payload: { text: "Read first.", text_tree: reading }, results: [] },
    ]),
    targets: [
      {
        gone: false,
        kind: "block",
        name: "Read first.",
        ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
        text_tree: reading,
      },
    ],
  });

  for (const id of [
    blockNodeId(SPEC, "r1"),
    blockNodeId(VERIFICATION, "c1"),
    blockNodeId("plan-md", "t1"),
  ]) {
    const node = nodeOf(root, id);
    expect(node?.text, id).toBe("Read first.");
    expect(node === undefined ? "" : nodeWords(node), id).toBe(`Read ${cited} first.`);
  }
});

/** The tree of an issue whose plan holds a task `t1`, `t2`, … for each of `words`, in order,
 *  which the batched reference read names by its stored excerpt and, where `read` holds, sends
 *  the server's reading of beside it: one paragraph whose one text run holds a bare address as
 *  plain text, which is the shape the server writes (`readingOf`). Task `t0` needs `t1`, so a
 *  chip names `t1`. */
function planTasks(words: readonly string[], read: boolean): TreeNode {
  const plan = artifact("artifact-plan", "plan-md", "plan.md");
  const refOf = (id: string): string => `dispatch://${KEY}/artifact/plan-md#b-${id}`;
  return treeOf({
    documents: [
      {
        artifactId: plan.id,
        blocks: [
          block("t0", "plan_task", { needs: refOf("t1") }),
          ...words.map((_, at) => block(`t${at + 1}`, "plan_task")),
        ],
      },
    ],
    issue: issueDetails([plan]),
    targets: words.map(
      (text, at): ReferenceTarget => ({
        gone: false,
        kind: "block",
        name: `:::plan_task{#t${at + 1}}\n${text}\n:::`,
        ref: refOf(`t${at + 1}`),
        ...(read ? { text_tree: readingOf(text) } : {}),
      })
    ),
  });
}

// R8 and issue page decision 350, which replaces decision 95's short form: the server's reading
// keeps a bare `dispatch://` address in a node's words as a plain text run, and a node's one
// line reads it as the projection reads one: as the words its author wrote until it is named, and
// as its target's name once it is, from the reading and, where the read sent none, from the
// stored text; the punctuation that trails it stays where it stood. A chip naming the block is a
// name, a title over one record, inside which an address reads as its short form, since a name
// reads no other name; where the read sent no reading it draws the kind's word, as before.
test("a bare address in a node's line reads as written until it is named, from a reading and from stored text", async () => {
  const lines = [
    ["Do it as dispatch://CORE-2 says. Then more.", "Do it as dispatch://CORE-2 says."],
    [
      "As dispatch://ACME-1196/spec#b-r1, read it first. Then more.",
      "As dispatch://ACME-1196/spec#b-r1, read it first.",
    ],
    ["It is in dispatch://CORE-2/comment/c9. Then more.", "It is in dispatch://CORE-2/comment/c9."],
  ] as const;
  for (const [sent, read] of [
    ["a reading", true],
    ["no reading", false],
  ] as const) {
    const root = planTasks(
      lines.map(([words]) => words),
      read
    );
    for (const [at, [words, line]] of lines.entries()) {
      const task = nodeOf(root, blockNodeId("plan-md", `t${at + 1}`));
      expect(task?.text, sent).toBe(words);
      expect(task === undefined ? "" : nodeLineOf(task), sent).toBe(line);
    }
    expect(nodeOf(root, blockNodeId("plan-md", "t0"))?.chips.map(nameOf), sent).toEqual([
      read ? "Do it as CORE-2 says." : NODE_TYPE_NAMES.plan_task,
    ]);
  }
  // Named, the address reads as its target's name, by the projection a row's line is drawn
  // with, from the name the page published.
  const task = nodeOf(planTasks([lines[0][0]], true), blockNodeId("plan-md", "t1"));
  const projected = renderHook(() => usePlainHeadline(task?.tree, undefined), {
    wrapper: publishingNames(new Map([["dispatch://CORE-2", "Warm the cache"]])),
  });
  try {
    await waitFor(() =>
      expect(projected.result.current.text?.text).toBe(
        "Do it as CORE-2 Warm the cache says. Then more."
      )
    );
  } finally {
    projected.unmount();
  }
});

// R22 and issue page decision 350: a comment's body is stored markdown, which a chip naming the
// comment draws as the plain-text projection of its reading, a name and so a title over one
// record: a bare address in the body reads as its target's short form in the chip's name and in
// the name the page publishes for it (`targetNaming`), since a name reads no other name. A link
// whose target is an address reads as its words, as it did. The page publishes the name as a
// link to the comment reads it, and the chip cuts it as a chip's words are cut.
test("a chip naming a comment whose body holds a bare address names it by the short form", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const ref = `dispatch://${KEY}/comment/comment-9`;
  const body =
    "**Hold** until dispatch://CORE-2 lands, as [the spec](dispatch://CORE-2/spec) says.";
  const name = "Hold until CORE-2 lands, as the spec says.";
  const reads = {
    ...decided([{ ...decision("d1", "Keep it.", []), links: [linkTo(ref, "comment")] }]),
    issue: issueDetails([spec, made]),
    targets: [{ gone: false, kind: "comment", name: body, ref }],
  };

  const chips = nodeOf(treeOf(reads), blockNodeId("decisions-md", "d1"))?.chips;
  expect(chips?.map((chip) => ({ name: nameOf(chip), ref: chip.ref }))).toEqual([
    { name: "Hold until CORE-2 lands, as the…", ref },
  ]);
  expect([...namesOf(answered(reads))]).toEqual([[ref, name]]);
});

// R1 and R21: a chip naming an ask, a comment or a message, and the name the page publishes for
// it, are the projection of the reading of its stored text in the shape of the server's
// (`storedReading`), the read sending none, so they hold the words the server's reading of the
// same text holds. Each pair below is a stored body and the words of the reading a running build
// of this head sent for it (`wordsOf` of `body_tree`). An HTML comment, as every agent's comment
// and every message of the daemon's outbox ends with, is no word of the text; nor is an inline
// tag, a table's pipes and rule, a thematic break's marks or a single-tilde strike; and a link
// whose URL holds parentheses reads as its words alone. A node with no reading takes its line
// from its stored text, where a list that follows a paragraph with no blank line ends it.
test("a stored text read to plain words holds the words of the server's reading of it", () => {
  for (const [stored, reading] of [
    ["Hold the deploy.\n\n<!-- legion-outbox:11715 -->", "Hold the deploy."],
    [
      'Fixed in abc1234.\n\n<!-- legion: {"session":"s-1","phase":"implementing"} -->',
      "Fixed in abc1234.",
    ],
    [
      "Done. <sub>Posted by the Legion implementer.</sub>",
      "Done. Posted by the Legion implementer.",
    ],
    ["See [the doc](https://example.com/a_(b)) first.", "See the doc first."],
    ["| Reading | At 1440 px |\n|---|---|\n| one | two |", "Reading At 1440 px one two"],
    ["~~gone~~ and ~also~ gone.", "gone and also gone."],
    ["* * *\n\nAfter the rule.", "After the rule."],
  ] as const) {
    const names = projectedNames(new Map([["stored", { tree: storedReading(stored) }]]));
    expect(names.get("stored"), stored).toBe(reading);
  }
  expect(
    nodeLineOf({ id: "c1", text: "Intro line:\n- item one\n- item two", type: "comment" })
  ).toBe("Intro line:");

  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const ref = `dispatch://${KEY}/message/message-9`;
  const reads = {
    ...decided([{ ...decision("d1", "Keep it.", []), links: [linkTo(ref, "message")] }]),
    issue: issueDetails([spec, made]),
    targets: [
      {
        gone: false,
        kind: "message",
        name: "Hold the deploy.\n\n<!-- legion-outbox:11715 -->",
        ref,
      },
    ],
  };
  const chips = nodeOf(treeOf(reads), blockNodeId("decisions-md", "d1"))?.chips;
  expect(chips?.map(nameOf)).toEqual(["Hold the deploy."]);
  expect([...namesOf(answered(reads))]).toEqual([[ref, "Hold the deploy."]]);
});

// The body draws a node's whole words, the words filter matches them and the batched reference
// read is asked to name each address they cite (`useIssueTree.ts`), so a node's words
// (`nodeWords`) keep every address as written, from a reading and from stored text alike, as its
// line beside them does until the address is named (issue page decision 350).
test("a node's words keep the bare address its line reads", () => {
  const words = "Do it as dispatch://CORE-2 says. Then more.";
  for (const [sent, read] of [
    ["a reading", true],
    ["no reading", false],
  ] as const) {
    const task = nodeOf(planTasks([words], read), blockNodeId("plan-md", "t1"));
    expect(task === undefined ? "" : nodeWords(task), sent).toBe(words);
    expect(task === undefined ? "" : nodeLineOf(task), sent).toBe(
      "Do it as dispatch://CORE-2 says."
    );
  }
});

// A caller that names each reference itself reads words in which no address is written short,
// or it finds no address left to name: the line of a reading keeps a bare address as written,
// and the words of a stored text's reading keep each bare address and each autolink's URL as
// written, its marks gone. The name the store projects from that reading writes each address as
// its target's short form, as a name reads no other name.
test("the raw words of a reading and of a stored text keep each address as written", () => {
  const reading = "Do it as dispatch://CORE-2 says.";
  const task = nodeOf(planTasks([reading], true), blockNodeId("plan-md", "t1"));
  expect(task?.tree).toBeDefined();
  expect(task === undefined ? "" : nodeLineOf(task)).toBe(reading);
  const tree = storedReading("Do it as **dispatch://CORE-2** says, see <dispatch://CORE-3>.");
  expect(wordsOf(tree)).toBe("Do it as dispatch://CORE-2 says, see dispatch://CORE-3.");
  expect(projectedNames(new Map([["stored", { tree }]])).get("stored")).toBe(
    "Do it as CORE-2 says, see CORE-3."
  );
});

// A span that only looks like an address - a path no route reads, a key in lower case, or the
// scheme alone - names nothing, so a node's line keeps it as its author wrote it, as it keeps an
// address that names a target it has not named yet.
test("a string that only looks like an address stays as written in a node's line", () => {
  const words =
    "Not dispatch://CORE-2/nowhere, dispatch://core-2 or dispatch:// but dispatch://CORE-2.";
  for (const [sent, read] of [
    ["a reading", true],
    ["no reading", false],
  ] as const) {
    const task = nodeOf(planTasks([words], read), blockNodeId("plan-md", "t1"));
    expect(task === undefined ? "" : nodeLineOf(task), sent).toBe(words);
  }
});

// A node whose read sent no reading takes its line from its stored text, which the page parses:
// the first line of it that holds words, a heading to its own line, its marks no word of it and
// each bare address as written until it is named. A plan task is read both ways, the reading the
// read sent and its stored text, and a decision with none from its stored text.
test("a node with no reading takes its line from its stored text's first line of words", () => {
  const words = "Do it as dispatch://CORE-2 says.";
  for (const [sent, read] of [
    ["a reading", true],
    ["no reading", false],
  ] as const) {
    const task = nodeOf(planTasks([words], read), blockNodeId("plan-md", "t1"));
    expect(task?.tree === undefined, sent).toBe(!read);
    expect(task === undefined ? "" : nodeLineOf(task), sent).toBe(words);
  }
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const ruled = decision("d1", "---\n\nDo it as **dispatch://CORE-2** says.", []);
  const headed = decision("d2", "## Title\n\nDo it as **dispatch://CORE-2** says.", []);
  const root = treeOf({
    ...decided([ruled, headed]),
    issue: issueDetails([spec, made]),
  });
  for (const [row, line] of [
    [ruled, "Do it as dispatch://CORE-2 says."],
    [headed, "Title"],
  ] as const) {
    const node = nodeOf(root, blockNodeId("decisions-md", row.block_id));
    expect(node?.tree, row.block_id).toBeUndefined();
    expect(node === undefined ? "" : nodeLineOf(node), row.block_id).toBe(line);
  }
});

/** One node's line as a row draws it (`useNodeLine`), and the reads of its references. */
function DrawnLine({ node }: { node: Parameters<typeof useNodeLine>[0] }): ReactNode {
  const { line, reads } = useNodeLine(node, { surface: "node-row-first-line" });
  return createElement("p", { "data-line": node.id }, line, reads);
}

// A page draws its rows again on every read that lands and every event, and a row's line is read
// from its stored text where the node has no reading, as a result's is from a server that sends
// none. The texts a draw parsed are not parsed again on the next: 900 distinct stored texts, each
// line drawn once, are drawn again - the page's batched read lands and publishes its names anew,
// the tree built again from the same records - and no text is parsed again. Each line holds its
// own parse, so no count of texts drops one parse for another.
test("the lines of more stored texts than the memo holds are drawn again without parsing them again", () => {
  const texts = Array.from(
    { length: 900 },
    (_, at) =>
      `The run \`bun run test:web web/src/case-${at}.test.ts\` ends with **0 fail**, and names each test below as passed.\n` +
      `- the first named test of case ${at}: pass\n- the second named test of case ${at}: pass\n` +
      `${"It held at the head, read with the junit report. ".repeat(12)}`
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = (names: ReadonlyMap<string, string>): ReactElement =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        ReferenceNamesProvider,
        null,
        createElement(Publisher, { published: publishedNames(names) }),
        ...texts.map((text, at) =>
          createElement(DrawnLine, {
            key: at,
            node: { id: `result-${at}`, text, type: "result" },
          })
        )
      )
    );
  const view = render(page(new Map()));
  try {
    expect(view.container.querySelector('[data-line="result-899"]')?.textContent).toBe(
      "The run bun run test:web web/src/case-899.test.ts ends with 0 fail, and names each test below as…"
    );
    const parses = countEngineParses();
    try {
      view.rerender(page(new Map([["dispatch://CORE-9", "Warm the cache"]])));
      expect(parses.count()).toBe(0);
    } finally {
      parses.restore();
    }
  } finally {
    view.unmount();
  }
});

// A caller that names each reference itself cuts a node's line from words in which no address is
// written short (`lineWordsRaw`), or it finds no address left to name. It walks a node as its
// row's line is cut - the first block of its reading, and where it has none, the first block of
// its stored text that holds words, a heading to its own line - and keeps each bare address as
// written on both paths. A decision passes as `{ text, tree: text_tree }`, with no node built for
// it.
test("a node's raw line keeps each bare address as written, from a reading and from stored text", () => {
  const words = "Do it as dispatch://CORE-2 says.";
  for (const [sent, read] of [
    ["a reading", true],
    ["no reading", false],
  ] as const) {
    const task = nodeOf(planTasks([words], read), blockNodeId("plan-md", "t1"));
    expect(task?.tree === undefined, sent).toBe(!read);
    expect(task === undefined ? "" : lineWordsRaw(task), sent).toBe(words);
  }
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const made = artifact("artifact-decisions", "decisions-md", "decisions.md");
  const ruled = decision("d1", "---\n\nDo it as **dispatch://CORE-2** says.", []);
  const headed = decision("d2", "## Title\n\nDo it as **dispatch://CORE-2** says.", []);
  const root = treeOf({
    ...decided([ruled, headed]),
    issue: issueDetails([spec, made]),
  });
  for (const [row, raw] of [
    [ruled, words],
    [headed, "Title"],
  ] as const) {
    const node = nodeOf(root, blockNodeId("decisions-md", row.block_id));
    expect(node?.tree, row.block_id).toBeUndefined();
    expect(node === undefined ? "" : lineWordsRaw(node), row.block_id).toBe(raw);
    expect(lineWordsRaw({ text: row.text, tree: row.text_tree }), row.block_id).toBe(raw);
  }
});

// A caller that needs a node id and no chip reads it from the issue alone, and the
// tree addresses nodes by the same rule, so the two never differ: the spec and its blocks by the
// spec's slug, another document and its blocks by the document's, an ask, a comment and a message
// by their ids, the issue by its key, and a reference to another issue by nothing.
test("an issue addresses a node as its tree does, from the issue alone", () => {
  const issue = issueDetails([artifact("a-spec", SPEC, "spec.md", true)]);
  const references = [
    `dispatch://${KEY}`,
    `dispatch://${KEY}/spec`,
    `dispatch://${KEY}/spec#b-r1`,
    `dispatch://${KEY}/artifact/plan-md`,
    `dispatch://${KEY}/artifact/plan-md#b-t1`,
    `dispatch://${KEY}/ask/a1`,
    `dispatch://${KEY}/comment/c1`,
    `dispatch://${KEY}/message/m1`,
    "dispatch://CORE-2/spec#b-r1",
    "not a reference",
  ];
  const alone = nodeIdOf(issue);
  expect(references.map(alone)).toEqual([
    KEY,
    SPEC,
    blockNodeId(SPEC, "r1"),
    "plan-md",
    blockNodeId("plan-md", "t1"),
    "a1",
    "c1",
    "m1",
    undefined,
    undefined,
  ]);
  const context = treeContext({ issue });
  expect(references.map(context.nodeIdOf)).toEqual(references.map(alone));
});

// A results read that carries each result's reading of its observed text (`observed_tree`) hands
// that reading to the result's node, so its line is the reading's first block and the page parses
// no result's stored text; the node names no source, since a result is no record the reference
// graph indexes. A result whose read sends none - a server of the build before - keeps its stored
// text's line. Under the tree read the reading reaches the node as its `text_tree`, as the
// model's tree route sends it (`TestTheTreeReadAnswersAResultsObservedTextAsItsReading`).
test("a result node holds the server's reading of its observed text where the read sends one", () => {
  const spec = artifact("artifact-spec", SPEC, "spec.md", true);
  const checks = artifact("artifact-verification", VERIFICATION, "verification.md");
  const root = treeOf({
    issue: issueDetails([spec, checks]),
    results: checkedResults([
      {
        blockId: "c1",
        results: [
          recorded("res-read", "A control.", {
            observed: "stored words",
            observed_tree: readingOf("Server words."),
          }),
          recorded("res-stored", "A control.", { observed: "Stored **words** only." }),
        ],
      },
    ]),
  });
  const read = nodeOf(root, "res-read");
  expect(read?.tree).toEqual(readingOf("Server words."));
  expect(read?.source).toBeUndefined();
  expect(read === undefined ? "" : nodeLineOf(read)).toBe("Server words.");
  const stored = nodeOf(root, "res-stored");
  expect(stored?.tree).toBeUndefined();
  expect(stored === undefined ? "" : nodeLineOf(stored)).toBe("Stored words only.");
});
