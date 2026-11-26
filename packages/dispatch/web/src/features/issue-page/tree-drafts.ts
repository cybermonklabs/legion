import { referenceSpans } from "@legion/contracts";

import { artifactAtAddress, primarySpec, roleDocument } from "../../api/issue-cache";
import type {
  Actor,
  Artifact,
  Ask,
  CheckBlock,
  CheckResult,
  DecisionEntry,
  DisplayTree,
  IssueChild,
  IssueCoverage,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { isDecisionEntry } from "../issue/decision-entries";
import { type AgentText, actorLabelText } from "../refs/AgentName";
import { deletedLineText, type PostDeletion, postDeletion } from "../refs/post-marks";
import type { ReferenceNameReading } from "../refs/reference-names";
import { buildDispatchReference, issueOrDocumentTitle, parseNodeReference } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { firstWordedBlock } from "../refs/tree-fragment";
import {
  blockIdOfNode,
  type DecisionContent,
  decisionChangeComments,
  decisionChanges,
  decisionContent,
  decisionDocumentId,
  decisionNodes,
  inReviewList,
} from "./decision-review";
import { artifactsBySlug } from "./issue-artifacts";
import type {
  IssueTreeReads,
  NodeAnswer,
  NodeChip,
  NodeCounts,
  NodeFact,
  NodeOption,
  TreeAsk,
} from "./issue-tree";
import {
  type AskState,
  type CheckState,
  coverageGapKind,
  type DecisionState,
  type GapKind,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeState,
  type NodeType,
  PRESET_NAMES,
} from "./page-words";
import { titleReading } from "./statement-rules";
import { storedReading } from "./stored-reading";

/**
 * The outline's rules for the drafts `buildIssueTree` places (root R4), which the tree read
 * keeps: how this issue addresses a node and names what a reference points
 * at (`treeContext`), how a check stands (`checkStanding`),
 * the gaps a statement draws (`requirementGaps`), and the drafts of the decisions read's agent
 * decisions that no node of the tree read holds (`addDecisions`): a section of `decisions.md` a
 * server of the release before answers.
 *
 * Every other node is drafted from the tree read (`tree-read/tree-nodes.ts`'s
 * `treeDraftsFrom`), which places and orders each one. Every function here is pure and takes the
 * reads as its input, and no request is made per node. Where a read sends the server's reading
 * of a node's words beside them, the draft keeps it with the record the words are, which is what
 * the row draws (`TreeNode.tree`).
 *
 * The tree is the issue's conversation and what it revealed: a document, an artifact and a
 * handoff are nodes of the issue and not of its conversation, so nothing here drafts one.
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  /** When it was made, where its read records one (`TreeNode.at`). */
  at?: string;
  author?: Actor;
  /** A person's answer to this ask, which the ask's own row and body draw (`TreeNode.answer`). */
  answer?: NodeAnswer;
  chips: NodeChip[];
  /** What the coverage read counts for a requirement it holds (`tree-read/tree-nodes.ts`);
   *  absent on every other draft, a statement of the spec drawn before that read answers among
   *  them. */
  counts?: NodeCounts;
  /** The coverage read's own lines a requirement it holds draws in its body
   *  (`TreeNode.coverageNotes`); absent where the read holds none, and on every other draft. */
  coverageNotes?: string[];
  /** When its author deleted this comment or message (`TreeNode.deletedAt`). */
  deletedAt?: string;
  /** Who deleted it, where the read names them (`TreeNode.deletedBy`). */
  deletedBy?: Actor;
  /** When its author last replaced this comment's or message's body (`TreeNode.editedAt`). */
  editedAt?: string;
  /** What the node's body draws under its text (`check-facts.ts`). */
  facts: NodeFact[];
  gaps: GapKind[];
  readonly id: string;
  /** The child issue this node is, as the issue read carries it (`TreeNode.child`). */
  child?: IssueChild;
  /** The person who kept this decision as it reads now (`TreeNode.keptBy`). */
  keptBy?: Actor;
  /** Whether this ask takes more than one of its options (`TreeNode.multiple`). */
  multiple?: true;
  /** The choices an ask offers (`TreeNode.options`). */
  options?: NodeOption[];
  order: number;
  /** The nodes this one sits under, in the order the model placed them: the first is where it is
   *  edited, every other a transclusion. `[]` is "under the issue". */
  parents: string[];
  question: boolean;
  /** The id the tree read (`tree-read/tree-nodes.ts`'s `treeDraftsFrom`) gives the node this
   *  draft was made from (`TreeNode.readId`); absent on a draft made from another read. */
  readId?: string;
  reference?: string;
  replaced: boolean;
  /** The record `text` is, where the draft's read names one (`TreeNode.source`). */
  source?: TextSource;
  /** How it stands, for a type the model gives a state (`TreeNode.state`); absent on a check
   *  block of any document but the issue's Verification document (`tree-read/tree-nodes.ts`'
   *  `draftServing`). */
  state?: NodeState;
  /** Whether this requirement is a statement (`TreeNode.statement`): a `requirement` block of the
   *  issue's spec or a requirement the coverage read holds (`tree-read/tree-nodes.ts`'
   *  `draftRequirement`). Absent on every other draft. */
  statement?: true;
  text: string;
  /** The server's reading of `text`, where the draft's read sends one (`TreeNode.tree`). */
  tree?: DisplayTree;
  type: NodeType;
  /** Whether the model marks this decision unsure: its `uncertainty` is `high`. */
  unsure?: true;
  /** Whether this node waits on the reader: a decision in their review list (`inReviewList`),
   *  or an open question whose turn is the reader's own (`waiting_on_person`). */
  waiting: boolean;
}

/** What every builder needs of the reads: how this issue addresses a node, and what a reference
 *  points at. Held as one value so the placement and the names are read the same way everywhere. */
export interface TreeContext {
  /** A chip naming what `reference` points at, by the one batched reference read and the
   *  issue's own documents (`treeContext`): one chip per reference, the same value on every
   *  call. */
  readonly chipOf: (reference: string) => NodeChip;
  readonly key: string;
  /** The node `reference` names, as this issue addresses it; undefined for anything else. */
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** The slug of the issue's spec (`primarySpec`), `""` for an issue with none. */
  readonly specSlug: string;
  /** The slug of one document of the issue, by its artifact id. */
  readonly slugOf: (artifactId: string) => string | undefined;
  /** The reader's login, which an open question waits on where its turn is theirs; undefined
   *  until the login resolves, and nothing waits on nobody. */
  readonly viewer: string | undefined;
}

/**
 * How `issue` addresses a node: the node a `dispatch://` reference names, as the tree's node ids
 * write it - the spec, a document, a block of either, an ask, a comment, a message, the issue
 * itself - and undefined for a reference to another issue or to anything else. It reads only the
 * issue's key and its spec's slug (`primarySpec`), so a caller that needs a node id and no chip
 * calls it without building a whole `treeContext`, and `treeContext` addresses nodes by it too,
 * so the two never differ.
 */
export function nodeIdOf(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">
): (reference: string) => string | undefined {
  const key = issue.key;
  const specSlug = primarySpec(issue)?.slug ?? "";
  return (reference: string): string | undefined => {
    const route = parseNodeReference(reference);
    if (route === undefined || !("key" in route) || route.key !== key) return undefined;
    const block = "block" in route ? route.block : undefined;
    if (route.kind === "spec") {
      return block === undefined ? specSlug : blockNodeId(specSlug, block.id);
    }
    if (route.kind === "artifact") {
      return block === undefined ? route.slug : blockNodeId(route.slug, block.id);
    }
    if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
      return route.id;
    }
    return route.kind === "issue" ? key : undefined;
  };
}

/**
 * Whether a reference names a statement of `issue` as the review list reads one (R35): a
 * requirement the coverage read `coverage` holds, each compared as the tree addresses it - the
 * requirement by the node `tree-read/tree-nodes.ts`' `draftRequirement` draws for it, the
 * reference by `nodeIdOf` - so the spec's block written short (`spec#b-…`) and by the spec's own
 * slug name the same requirement, and another issue's address names none. Nothing is one while
 * that read has not answered, and no other block is one: not a `requirement` block of the spec
 * that read does not hold, which its row draws as a statement all the same (`Draft.statement`),
 * nor a block of any other document. It is the one rule both readers of the list read, the
 * tree's `waiting` (`addDecisions`) and the header's count, whose caller hands it to
 * `waitingOnReader`, so the count counts the very decisions the tree marks.
 */
export function coverageStatements(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">,
  coverage: IssueCoverage | undefined
): (reference: string) => boolean {
  const specSlug = primarySpec(issue)?.slug ?? "";
  const statements = new Set(
    (coverage?.requirements ?? []).map((requirement) => blockNodeId(specSlug, requirement.block_id))
  );
  const nodeIdOfReference = nodeIdOf(issue);
  return (reference: string): boolean => {
    const id = nodeIdOfReference(reference);
    return id !== undefined && statements.has(id);
  };
}

/** How the model names the actor of a delete: as `AgentName` names one before the agent
 *  registry answers (`actorLabelText`), since the model is pure and reads no registry. */
const ACTOR: Pick<AgentText, "label"> = { label: actorLabelText };

/**
 * The line a deleted comment's or message's node holds in place of its body: the words
 * `DeletedBody` (`refs/PostBody.tsx`) prints where the body was, `Deleted by <who> · <when>`, or
 * `Deleted · <when>` for a delete whose read names nobody (`deletedLineText`). The model is pure
 * and reads no agent registry and no clock, so `<who>` is the actor as `AgentName` names one
 * before the registry answers (`actorLabelText`: a person's login, a session's stamped title
 * else its short id), and `<when>` is the time the read stores (`deleted_at`) as it stores it,
 * never an age; a view that draws the line itself names the actor from `deletedBy` and draws
 * the time from `deletedAt`. It is made from the delete alone, so it holds no part of the body
 * even where a read still carries one.
 */
export function deletedPostLine(deleted: PostDeletion): string {
  return `${deletedLineText(deleted, ACTOR)} · ${deleted.at}`;
}

/**
 * The reads as every builder reads them: the tree's (`buildIssueTree`) and the page's published
 * names (`targetNaming`) both, so a chip and a link name one target by one rule and cannot drift
 * apart.
 *
 * A chip carries how its target is named (`NodeChip.reading`), and never the target's words:
 * the page publishes each reading and the shared reader projects it (`refs/reference-names.tsx`),
 * as a title drawn over one record, so no word of a name is written by a reader of this page's
 * own, and markup is never a word of one (R21). The name is the one a link to the target reads
 * (R8), and a chip cuts it again where it draws it (`first-line.ts`'s `chipName`).
 *
 * - **A target nothing answers to now** by nothing, which the chip draws as gone. A short
 *   reference two or more targets carry the id of is such a target, which the read answers
 *   `ambiguous` as well, and its chip carries `ambiguous` so it can draw the reference as
 *   written rather than as a target deleted.
 * - **A block** - the reference names one, or the read answers that its target is one - by the
 *   server's reading of its whole text (`ReferenceTarget.text_tree`), as a link to it titles it
 *   (R8); a chip names it by the first block of that reading that holds any words instead, so a
 *   decision's chip is its own heading (`refs/reference-names.tsx`'s `chipLineReading`). Where
 *   the read answered and sent no reading, or one that holds no words, a block of this issue
 *   whose document the tree read is named by the page's word for its kind (`NODE_TYPE_NAMES`),
 *   never by its id (R22), and so is one whose words project to none (`fallback`); another
 *   issue's block, whose kind no read of this page gives, by nothing, and a surface reads it
 *   itself. A block the read has not answered for yet has no name either, which the chip draws
 *   as not yet named (`NodeChip.answered`). The read's `name` for a block is its stored excerpt,
 *   which is the block's source: a typed block's directive and the markdown its words are
 *   written in are markup and no word of it (R21).
 * - **A whole document** - a `spec` or an `artifact` reference naming no block - by the name a
 *   link to it reads (R8, `issueOrDocumentTitle`): an issue's spec by the issue's title, and any
 *   other document by its own name, which is its file name, each read as one text run
 *   (`titleReading`), as a title is. This issue's documents are titled by the issue read, and
 *   another issue's by the batched read: its spec by that issue's title, which the page asks the
 *   read for beside every whole spec it holds (`useIssueTree.ts`'s `treeReferences`), where the
 *   read answered for the issue, and its document by the read's name for it. A document nothing
 *   titles is named by the page's word for a document. The reading carries the page's word for the
 *   document as well (`ReferenceNameReading.chipWord`, `documentWord`), which every chip draws
 *   in place of its name, a tie's and one in place in a node's words: a chip never draws a
 *   document's file name (decision 39).
 * - **An issue** - the read answers that its target is one - by its title, which is no markdown,
 *   read as one text run (`titleReading`), so nothing in it is parsed. The issue and each child
 *   the issue read lists are named by their whole titles, which that read holds, so their chips
 *   draw the very title their rows draw. Any other issue is named by the read's name for it,
 *   which is the first 240 characters of its title (the server's `refs.excerptRunes`). An issue
 *   the read has not answered for yet has no name, as a block has none.
 * - **Anything else** by the read's `name` for it. An ask's, a comment's and a message's name is
 *   the question or the body as stored, which is markdown whose marks are no word of it (R21),
 *   and the read sends no reading of it, so it is named from the reading of its stored text in
 *   the shape of the server's (`storedReading`), whose words are the server's for every text that
 *   holds none of three things (`STORED_TEXT_PARSE`): an HTML comment an agent's comment ends
 *   with is no word of it. A comment or a message the read marks deleted
 *   (`ReferenceTarget.deleted_at`) is named by the deleted line's words (`deletedLineText`),
 *   never by the emptied body, which is no words at all, as a link to it is
 *   (`reference-target.ts`'s `useReferenceLabel`); the model reads no agent registry, so the
 *   line names the actor as `AgentName` does before the registry answers (`actorLabelText`),
 *   where a link names a listed session by its registry title. A component's name is a title,
 *   which is no markdown, and is drawn as the read gives it.
 *
 * A chip carries the marks the read answers for a post (`NodeChip.deletedAt`, `deletedBy`,
 * `editedAt`), and its reading is of the words alone: a place that draws the name of an edited
 * one, a comment, a message or a block of either, cuts the words to its own length and marks what
 * the cut left, so no cut takes the mark. The row's chip draws the mark as an element of its own
 * (`NodeRow.tsx`'s `Chip`, with `EditedMark`), and a place that names the post in one string
 * writes it with `editedName`.
 */
export function treeContext(reads: IssueTreeReads): TreeContext;
export function treeContext(
  reads: Omit<IssueTreeReads, "issue"> & { readonly issue?: IssueDetails }
): Omit<TreeContext, "key"> & { readonly key: string | undefined };
export function treeContext(
  reads: Omit<IssueTreeReads, "issue"> & { readonly issue?: IssueDetails }
): Omit<TreeContext, "key"> & { readonly key: string | undefined } {
  const { issue } = reads;
  const key = issue?.key;
  const artifacts = issue?.artifacts ?? [];
  const slugs = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const spec = primarySpec(issue);
  const specSlug = spec?.slug ?? "";
  const names = new Map((reads.targets ?? []).map((target) => [target.ref, target]));
  // The kind of each typed block of this issue the tree read answers, by its node id, which is
  // all a chip can say of a block the reference read sent no words for.
  const types: readonly string[] = NODE_TYPES;
  const kinds = new Map<string, NodeType>();
  for (const node of reads.tree?.nodes ?? []) {
    if (node.document !== undefined && node.issue === undefined && types.includes(node.supertag)) {
      kinds.set(blockNodeId(node.document.slug, blockIdOfNode(node)), node.supertag as NodeType);
    }
  }
  const nodeIdOfReference = issue === undefined ? () => undefined : nodeIdOf(issue);
  // The whole title of the issue and of each child the issue read lists, by its own address:
  // the names read cuts a title to its first 240 characters, and the issue read holds these
  // whole, as the rows draw them.
  const titles = new Map<string, string>([
    ...(issue === undefined
      ? []
      : [[buildDispatchReference({ key: issue.key, kind: "issue" }), issue.title] as const]),
    ...(issue?.children ?? []).map((child): [string, string] => [
      buildDispatchReference({ key: child.key, kind: "issue" }),
      child.title,
    ]),
  ]);
  // The issue `issueKey` as a link to it is titled: by its whole title where the issue read holds
  // it, and else by the batched read's name for it, where that read answered for the issue. The
  // page asks it for the issue of every whole spec the tree holds (`useIssueTree.ts`'s
  // `treeReferences`), so another issue's spec is titled as a link to it is.
  const titledIssue = (issueKey: string): { readonly title: string } | undefined => {
    const reference = buildDispatchReference({ key: issueKey, kind: "issue" });
    const whole = titles.get(reference);
    if (whole !== undefined) return { title: whole };
    const answered = names.get(reference);
    return answered?.kind === "issue" && !answered.gone ? { title: answered.name } : undefined;
  };
  const readingOf = (
    reference: string,
    target: ReferenceTarget | undefined,
    deleted: PostDeletion | null
  ): ReferenceNameReading | undefined => {
    const route = parseNodeReference(reference);
    const block = route !== undefined && "block" in route ? route.block : undefined;
    if (block !== undefined || target?.kind === "block") {
      if (target === undefined) return undefined;
      const kind = kinds.get(nodeIdOfReference(reference) ?? "");
      const word = kind === undefined ? undefined : NODE_TYPE_NAMES[kind];
      const whole = target.text_tree;
      if (whole === undefined || firstWordedBlock(whole) === undefined) {
        return word === undefined ? undefined : { words: word };
      }
      return word === undefined ? { tree: whole } : { fallback: word, tree: whole };
    }
    // A block of it returned above, so this is the document itself: one of this issue's by the
    // issue read, and another issue's by what the batched read answered for it.
    if (route?.kind === "spec" || route?.kind === "artifact") {
      const own =
        route.key !== key
          ? undefined
          : route.kind === "spec"
            ? spec
            : artifactAtAddress(artifacts, route.slug);
      const answered = target?.kind === "artifact" ? target : undefined;
      return documentReading(
        issue,
        own,
        issueOrDocumentTitle(route, titledIssue(route.key), own ?? answered)
      );
    }
    if (target === undefined) return undefined;
    if (target.kind === "ask" || target.kind === "comment" || target.kind === "message") {
      return deleted === null
        ? { tree: storedReading(target.name, target) }
        : { words: deletedLineText(deleted, ACTOR) };
    }
    if (target.kind === "issue") {
      return { tree: titleReading(titles.get(reference) ?? target.name) };
    }
    const words = blockWords(target.name);
    return words === "" ? undefined : { words };
  };
  // One chip per reference, made the first time it is asked for: a tree asks once for every
  // citation of a target, and no caller changes a chip - its fields are read-only (`NodeChip`),
  // and a caller that adds one, a `needs` label or a link's `stale`, spreads it into a chip of
  // its own - so every place that cites the target holds the same one.
  const chips = new Map<string, NodeChip>();
  return {
    chipOf: (reference: string): NodeChip => {
      const held = chips.get(reference);
      if (held !== undefined) return held;
      const target = names.get(reference);
      const gone = target?.gone ?? false;
      const deleted = postDeletion(target);
      // A deleted post's deleted line already says what became of its text, so it carries no
      // edit mark.
      const edited = deleted === null ? target?.edited_at : undefined;
      const reading = gone ? undefined : readingOf(reference, target, deleted);
      const chip: NodeChip = {
        ...(target?.ambiguous === true ? { ambiguous: true } : {}),
        answered: target !== undefined,
        ...(deleted === null ? {} : { deletedAt: deleted.at }),
        ...(deleted === null || deleted.by === null ? {} : { deletedBy: deleted.by }),
        ...(edited === undefined ? {} : { editedAt: edited }),
        gone,
        ...(reading === undefined ? {} : { reading }),
        ref: reference,
      };
      chips.set(reference, chip);
      return chip;
    },
    key,
    nodeIdOf: nodeIdOfReference,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
    viewer: reads.viewer,
  };
}

/**
 * How the page names a whole document: by `title`, the title a link to it reads
 * (`issueOrDocumentTitle`), read as one text run (`titleReading`), which the page publishes for
 * every plain line to read (R8), with the page's word for it (`documentWord`), which every chip
 * draws in that name's place (`chipWord`), since a chip never draws a document's file name
 * (decision 39). `own` is the document of `issue` the reference names, if any. A document
 * nothing titles is published by that word as well.
 */
function documentReading(
  issue: IssueDetails | undefined,
  own: Pick<Artifact, "id"> | undefined,
  title: string | undefined
): ReferenceNameReading {
  const chipWord = documentWord(issue, own);
  return title === undefined
    ? { chipWord, words: chipWord }
    : { chipWord, tree: titleReading(title) };
}

/**
 * The page's word for one document of the issue, which a chip naming the whole document draws
 * (`documentReading`): the name of the filter that draws what it holds (`PRESET_NAMES`) for the
 * spec and for the plan and Verification documents, and the page's word for a document
 * (`NODE_TYPE_NAMES.document`) for any other, another issue's included (`undefined`), since
 * another issue's document plays no part of this one. A link and every line read a document by
 * its own name, which is its file name, `spec.md` (R8); a chip never draws it (decision 39). A
 * chip the page has given no word yet draws this rule as far as an address and the row's issue
 * say it (`node-row-chip.ts`'s `documentWord`).
 *
 * Which document plays which part is the issue read's answer and nothing else: the id it names
 * for each (`primary_artifact_id`, and the artifact of its plan and of its verification query in
 * `queries`), read by `primarySpec` and `roleDocument`, the rule the tree read's blocks of each
 * part are read by (`tree-read/tree-nodes.ts`' `blocksOfRole`). Where the issue names no id for a
 * part - none (`""`), or no query of that name - no document plays it, and a document flagged
 * `primary` or carrying that part's `role` is named by the page's word for a document: a chip
 * naming "Spec" or "Plan" opens a document whose blocks the tree reads as that part. A decisions
 * document plays no part: a decision is a `decision` block of the spec or of another document,
 * and a decisions document an earlier release stored is named by the page's word for a document
 * too.
 */
function documentWord(
  issue: IssueDetails | undefined,
  document: Pick<Artifact, "id"> | undefined
): string {
  if (document === undefined) return NODE_TYPE_NAMES.document;
  if (document.id === primarySpec(issue)?.id) return PRESET_NAMES.spec;
  for (const role of ["plan", "verification"] as const) {
    if (document.id === roleDocument(issue, role)?.id) return PRESET_NAMES[role];
  }
  return NODE_TYPE_NAMES.document;
}

/** A directive's own opening line, `:::plan_task{#t1 serves="…"}`, and its closing fence. */
const DIRECTIVE_OPENER = /^:{3,}[a-z_]/;
const DIRECTIVE_FENCE = /^:{3,}\s*$/;

/**
 * The words of the name the batched reference read gives a target (`ReferenceTarget.name`).
 * The read names a document's block by its stored excerpt, which is the block's source: a typed
 * block is a directive, so its excerpt opens on the directive's line - its type and its
 * attributes - and, where the excerpt holds the whole block, ends on its closing fence. Both are
 * markup and no word of the block (R21), so they are left out. Any other name is left as the
 * read gives it.
 */
export function blockWords(name: string): string {
  const lines = name.trimEnd().split("\n");
  if (!DIRECTIVE_OPENER.test(lines[0] ?? "")) return name;
  lines.shift();
  if (DIRECTIVE_FENCE.test(lines.at(-1) ?? "")) lines.pop();
  return lines.join("\n").trim();
}

/**
 * A chip that opens each document of the issue a node's own words cite (R3): a document is no
 * node of the outline, so a block that names one carries a chip of its own for it, which opens
 * the document at its own address in place of the outline (`NodeRow.tsx`'s `Chip`). The words
 * read are the draft's text: a node of the tree read is drafted with its words as that read
 * answers them, a block's its stored source (`tree-read/tree-nodes.ts`' `draftOf`), and a
 * decision of the decisions read with its block's words (`addDecisions`). The chip draws the
 * page's word for the document, as every chip naming a whole document does
 * (`documentReading`): the filter's name for the spec and for the plan and Verification
 * documents, and "document" for any other, never its file name (decision 39). It addresses the
 * document as `artifact/<slug>` - the spec too, whose `spec` reference is the Spec filter's
 * address - and carries the name a link to that address reads, the document's file name. A
 * citation of one block of a document names that block, which the text's own link opens, and no
 * chip.
 */
export function addDocumentChips(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const bySlug = artifactsBySlug(reads.issue);
  if (bySlug.size === 0) return;
  for (const draft of drafts.values()) {
    for (const span of referenceSpans(draft.text, "dispatch://")) {
      const route = parseNodeReference(span.value);
      if (route === undefined || !("key" in route) || route.key !== context.key) continue;
      if ("block" in route && route.block !== undefined) continue;
      const slug =
        route.kind === "artifact" ? route.slug : route.kind === "spec" ? context.specSlug : "";
      const artifact = bySlug.get(slug);
      if (artifact === undefined) continue;
      const address = { key: context.key, kind: "artifact", slug } as const;
      const ref = buildDispatchReference(address);
      if (draft.chips.some((chip) => chip.ref === ref)) continue;
      draft.chips = [
        ...draft.chips,
        {
          answered: true,
          gone: false,
          reading: documentReading(
            reads.issue,
            artifact,
            issueOrDocumentTitle(address, reads.issue, artifact)
          ),
          ref,
        },
      ];
    }
  }
}

/** A draft with nothing on it but its id and its type, for a builder to fill in. */
export function blank(id: string, type: NodeType, order: number): Draft {
  return {
    chips: [],
    facts: [],
    gaps: [],
    id,
    order,
    parents: [],
    question: false,
    replaced: false,
    text: "",
    type,
    waiting: false,
  };
}

/** An ask's answer as the tree holds it: the person it is theirs, with the agent they answered
 *  through where they did, and their choices and words as stored, with the server's reading of
 *  each where the read sent one. */
export function answerOf(answer: NonNullable<TreeAsk["answer"]>): NodeAnswer {
  return {
    at: answer.at,
    by: { id: answer.user, kind: "user", ...(answer.act === undefined ? {} : { act: answer.act }) },
    selected: answer.selected ?? [],
    ...(answer.selected_trees === undefined ? {} : { selectedTrees: answer.selected_trees }),
    text: answer.text ?? "",
    ...(answer.text_tree === undefined ? {} : { textTree: answer.text_tree }),
  };
}

/**
 * How an ask stands, in one of the words the page declares for an ask (`NODE_STATE_WORDS.ask`):
 * the kind of its resolution for an ask that was closed rather than answered, so the tree says
 * "Declined" where the card and the margin beside it say "Declined", and the `state` the server
 * wrote for every other ask. The server types both as plain strings (`Ask.state`,
 * `AskResolution.kind`), and a token the page declares no word for is drawn as no state rather
 * than as the server's own token (R21).
 */
export function askState(ask: Ask): AskState | undefined {
  const words: Readonly<Record<string, string | undefined>> = NODE_STATE_WORDS.ask;
  const token =
    ask.state === "resolved" && ask.resolution !== undefined ? ask.resolution.kind : ask.state;
  return words[token] === undefined ? undefined : (token as AskState);
}

/** Each text source (`textSourceOf`) a row was handed, by its kind, since one object can stand
 *  for more than one - an ask block's reading is its ask's text and a block of its document - and
 *  then by the object the row draws: the reading of its words, else the node or the entry it was
 *  drafted from. A re-read whose answer is unchanged keeps each of these objects, and one whose
 *  node changed beside its words keeps the node's reading, so every build of the drafts hands a
 *  row the source object it handed it before, and the row's words are not drawn again for a
 *  source that only looks new; and a source goes when that object does. */
const recordSources: Readonly<Record<TextSource["kind"], WeakMap<object, TextSource>>> = {
  artifact: new WeakMap(),
  ask: new WeakMap(),
  comment: new WeakMap(),
  message: new WeakMap(),
};

/** The text source of record `id` of `kind` on issue `issue` (`textSourceOf`) for `drawn`, the
 *  object the row it is handed to draws - its reading, else the node or the entry it is drafted
 *  from: the one object `recordSources` holds for that object and kind, made again only where
 *  `id` or `issue` is not the one it was made with, since the reads an id is found through can
 *  change while the object stays; none where there is no id, as `textSourceOf` answers. */
export function stableSourceOf(
  drawn: object,
  kind: TextSource["kind"],
  id: string | undefined,
  issue: string
): TextSource | undefined {
  if (id === undefined) return undefined;
  const sources = recordSources[kind];
  const kept = sources.get(drawn);
  if (kept?.id === id && kept.issue === issue) return kept;
  const source = textSourceOf(kind, id, issue);
  if (source === undefined) return undefined;
  sources.set(drawn, source);
  return source;
}

/** Each decision's own content (`decisionContent`), by the entry it was read from: a re-read
 *  whose answer is unchanged keeps the entry's object, and so a row is handed the words it was
 *  handed before. */
const decisionContents = new WeakMap<DecisionEntry, DecisionContent>();

function stableDecisionContent(decision: DecisionEntry): DecisionContent {
  let own = decisionContents.get(decision);
  if (own === undefined) {
    own = decisionContent(decision);
    decisionContents.set(decision, own);
  }
  return own;
}

/**
 * Each agent's decision of the decisions read that no node of the tree read holds
 * (`decisionNodes`) - a section of the issue's `decisions.md` a server of the release before
 * answers - drafted as the outline drafted it, beside the drafts of the tree read, with its
 * Changes under it. The decisions are the read's entries that are decisions (`isDecisionEntry`):
 * an open or a declined ask decided nothing. A person's decision is the answered ask itself
 * (`DecisionEntry.kind`), which the tree read answers as the ask's own node with the ties it
 * shaped, so nothing is drafted for it here.
 *
 * It stands under the question it settles (R4): the asks its entry names for it
 * (`DecisionEntry.from_ask`), the first of them where it is edited and every other one a
 * transclusion, as a check stands under each requirement it serves. An ask the tree draws no
 * node for - another issue's, or an approval request - places nothing and stays the decision's
 * chip, so a decision that names no question of this issue stands under the issue, as any node
 * whose parent is absent does, and never inside its document, which is no node at all. Wherever
 * it stands it comes before its siblings, but for the issue's memo, the first callout of its
 * spec, which comes first: the outline drew these decisions above the statements and the
 * conversation. It names every node its `shaped` edge reaches as a chip
 * (`DecisionEntry.shaped`), since that edge places nothing, and each whole document its words
 * cite (`addDocumentChips`). It draws the decision's own content as the read answers it
 * (`decisionContent`), whose references resolve against the document its block stands in
 * (`decisionDocumentId`), and its author is the decider its entry names (`deciderOf`).
 *
 * Its Change is a comment of the issue whose quote its words hold, where they are the first
 * decision's words to hold it, with no block of its document read
 * (`decisionChangeComments`). The read hangs that comment under the issue, since no node holds
 * the decision, so it is drawn under the decision it
 * changes, which then waits on its agent. Whether a decision waits on the reader is the review
 * list's rule (`inReviewList`), by the statements the coverage read holds (`coverageStatements`),
 * as the header's count reads it (`waiting.ts`).
 *
 * `asksOf` answers the node each ask of the issue is drawn by, by the ask's id (`askNodeIds`),
 * asked only where a decision is drawn here.
 */
export function addDecisions(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  asksOf: () => ReadonlyMap<string, string>
): void {
  const all = (reads.decisions?.entries ?? []).filter(
    (decision) => isDecisionEntry(decision) && decision.kind !== "person"
  );
  const held = decisionNodes(all, reads.tree);
  const rows = all.filter((decision) => !held.has(decision.ref));
  if (rows.length === 0) return;
  const asks = asksOf();
  const placed = decisionChangeComments(
    reads.decisions,
    reads.comments,
    reads.issue,
    undefined,
    reads.tree
  );
  const changes = decisionChanges(reads.decisions, reads.comments, reads.issue, undefined, placed);
  const memo = [...drafts.values()]
    .filter(
      (draft) =>
        draft.type === "callout" &&
        draft.parents.length === 0 &&
        draft.id.startsWith(blockNodeId(context.specSlug, ""))
    )
    .sort((one, other) => one.order - other.order)[0];
  const first = memo?.order ?? -1;
  // A decision waits for the reader's review where it is on their review list (R35), which
  // reads a statement as a requirement the coverage read holds (`coverageStatements`), the rule
  // the header's count is handed too (`waitingOnReader`), and not as the rows read one
  // (`Draft.statement`): a spec's `requirement` block that read does not hold - each one, before
  // it answers - is a statement on its row, and a tie to it puts no decision on the list.
  const isCoverageStatement = coverageStatements(reads.issue, reads.coverage);
  const made = new Map<string, Draft>();
  rows.forEach((decision, index) => {
    const id = context.nodeIdOf(decision.ref);
    if (id === undefined) return;
    // Each a fraction of one past the memo's order, so they stand together before every
    // sibling the read orders after it.
    const draft = blank(id, "decision", first + (index + 1) / (rows.length + 1));
    const answers = decision.from_ask ?? [];
    // A target with no address of its own is named by nothing, so there is no chip to draw for
    // it (`GraphNode.ref` is optional). Each target is one chip, stale where any of its links is,
    // though a read names it more than once.
    const linked = new Map<string, NodeChip>();
    for (const link of decision.shaped ?? []) {
      const ref = link.to.ref;
      if (ref === undefined) continue;
      const stale = link.stale || linked.get(ref)?.stale === true;
      linked.set(ref, { ...context.chipOf(ref), stale });
    }
    draft.chips = [
      ...linked.values(),
      ...[...new Set(answers)]
        .filter((ask) => !asks.has(ask))
        .map((ask) =>
          context.chipOf(buildDispatchReference({ id: ask, key: context.key, kind: "ask" }))
        ),
    ];
    draft.parents = answers.flatMap((ask) => {
      const parent = asks.get(ask);
      return parent === undefined ? [] : [parent];
    });
    if (decision.changed_at !== null) draft.at = decision.changed_at;
    const decider = deciderOf(decision.decided_by);
    if (decider !== undefined) draft.author = decider;
    draft.reference = decision.ref;
    draft.replaced = decision.replaced;
    const own = stableDecisionContent(decision);
    draft.source = stableSourceOf(
      decision,
      "artifact",
      decisionDocumentId(reads.issue, decision),
      context.key
    );
    draft.text = own.text;
    draft.tree = own.tree;
    // How the decision stands with the reader, drawn as this node's own state word and shape
    // the way every other state is (R21).
    const changed = changes.has(decision.ref);
    draft.state = decisionState(decision, changed);
    markKeptAndUnsure(draft, {
      kept: decision.kept,
      unsure: decision.uncertainty === "high",
      waiting_review: decision.waiting_review,
    });
    // The tie to a statement decides whether it waits on the reader, and a decision the reader
    // asked to change waits on its agent, not on the reader (`decision-review.ts`). Nothing
    // waits on a reader whose login has not resolved, as `waiting.ts` counts it.
    draft.waiting =
      context.viewer !== undefined && inReviewList(decision, isCoverageStatement, changed);
    made.set(id, draft);
  });
  addDocumentChips(made, reads, context);
  for (const [id, draft] of made) drafts.set(id, draft);
  // Each Change of these decisions stands under the decision it changes, where the read hangs it
  // under the issue: a comment the read places under another node is a reply, in its thread.
  for (const [comment, ref] of placed) {
    const draft = drafts.get(comment);
    const decision = context.nodeIdOf(ref);
    if (
      draft?.type === "comment" &&
      draft.parents.length === 0 &&
      decision !== undefined &&
      made.has(decision)
    ) {
      draft.parents = [decision];
    }
  }
}

/**
 * Who decided, as the page names an author: the far end of an entry's `decided_by` tuple, a
 * session for an agent (`session:<id>`) and a person by login (`person:<login>`), which every
 * other read names as a `user` actor. A session carries the title it stamped on what it wrote
 * (`decided_by.title`) as its origin's `session_title`, so a session the registry no longer lists
 * is still named by it (`AgentName.tsx`'s `drawnSessionName`); one that stamped none carries no
 * origin. Undefined where the entry names nobody, or a node of another kind. A decision node of
 * the tree read draws its decider by it too (`tree-read/tree-nodes.ts`' `draftDecision`), from
 * its entry in the decisions read, since the node's `decision` names no decider.
 */
export function deciderOf(node: DecisionEntry["decided_by"]): Actor | undefined {
  if (node === null) return undefined;
  if (node.kind === "session") {
    return node.title === undefined
      ? { id: node.id, kind: "session" }
      : { id: node.id, kind: "session", origin: { session_title: node.title } };
  }
  return node.kind === "person" ? { id: node.id, kind: "user" } : undefined;
}

/**
 * How an agent's decision stands with the reader, in the states the page draws
 * (`NODE_STATE_WORDS.decision`, R2): replaced by a later one first, since nothing waits on it
 * any longer; then a change the reader asked for, which waits on its agent and not on the
 * reader (`decision-review.ts`); then waiting for review; then kept, by the person whose name the
 * row draws after it (`TreeNode.keptBy`). A person's own decision is the answered ask, which
 * draws the ask's own state (`addDecisions`). A decision of the decisions read and a decision
 * node of the tree read (`tree-read/tree-nodes.ts`' `draftDecision`) both stand by it, the node
 * by the same members, each optional there.
 */
export function decisionState(
  decision: Partial<Pick<DecisionEntry, "kept" | "replaced" | "waiting_review">>,
  changed: boolean
): DecisionState | undefined {
  if (decision.replaced) return "replaced";
  if (changed) return "change_requested";
  if (decision.waiting_review) return "waiting_review";
  return decision.kept == null ? undefined : "kept";
}

/** Who kept a decision as it reads now, whose name its row draws after its state word, unless it
 *  waits for review again; and whether its agent marked it unsure. One rule for a decision of the
 *  decisions read and a decision node, as `decisionState` is. */
export function markKeptAndUnsure(
  draft: Draft,
  decision: Partial<Pick<DecisionEntry, "kept" | "waiting_review">> & { readonly unsure?: boolean }
): void {
  if (decision.kept != null && decision.waiting_review !== true) {
    draft.keptBy = { id: decision.kept.kept_by, kind: "user" };
  }
  if (decision.unsure === true) draft.unsure = true;
}

/**
 * The gaps on one statement that the coverage read carries and the page draws
 * (`coverageGapKind`). `tree-read/tree-nodes.ts`' `draftRequirement` adds the ones it reads from
 * the tree read and the Plan view itself.
 */
export function requirementGaps(coverage: readonly string[]): GapKind[] {
  return coverage.flatMap((line): GapKind[] => {
    const kind = coverageGapKind(line);
    return kind === undefined ? [] : [kind];
  });
}

/**
 * How a check stands where no result of it stands at its basis, in the states
 * `NODE_STATE_WORDS.check` names: `checkStanding`'s answer for such a check, and for a check with
 * no result or no `pass_stands` at all, and read by nothing else, so no place of the page reads
 * a check other than `checkStanding` does. Whether it passed is the server's own answer over
 * every result of the check (`pass_stands`, main's e98fc56ef8): a pass recorded before a later
 * fail still stands, while a retracted pass, a pass recorded against an older wording of the
 * check and a pass at a commit the check no longer compares with stand for nothing. The other
 * states read what no pass standing is:
 *
 * - **`no-result`,** no result recorded at all, which the results say by themselves.
 * - **`unreadable`,** a payload carrying no `pass_stands` at all, which is a server that does
 *   not send the field - an older build behind the same load balancer as the one this page
 *   came from. It is read before everything the answer decides, because without it the page
 *   knows nothing of how the check stands; it is the page's own blindness and no verdict of
 *   the check's.
 * - **`retracted`,** every result of the check withdrawn (`CheckResultRetracted`, which is on
 *   the result and never on the check): nothing stands for it and nothing stands against it,
 *   so neither "Passed" nor "Failed" is true of it, and the withdrawal is what the reader is
 *   owed.
 * - **`older`,** a newest result that stands and no longer speaks for the work as it stands,
 *   whatever it found, because its commit is not one the check compared with or the check's
 *   own words have moved under it.
 * - **`failed`,** a newest result that stands and failed at a commit Dispatch could not place:
 *   a fail at the work the check compares with is `checkStanding`'s own answer, before this.
 * - **`unmarked`,** where nothing could be judged: a repository Dispatch cannot read, or a
 *   commit it cannot place.
 *
 * A retracted result is no evidence, as the server counts it (`results_comparison.go`'s
 * `passStandsOf` skips it), so `older` and `failed` are read from the newest result that still
 * stands and never from a withdrawn one: a pass taken back at the compared commit over an older
 * pass at another commit is `older`, and a pass taken back over a fail that stands is `failed`,
 * whatever commit the withdrawn pass ran at. The newest is read by the time each result was
 * recorded (`newestFirst`) and never by the order the read sent them in, as `checkStanding` and
 * the Verification filter's body read it, so the three name one result whatever that order is.
 *
 * It is how a check stands against the basis the check itself carries, which is the server's
 * own reading (`pass_stands`, `commit_stands`), where no result over the check's words now is at
 * that basis, so one check reads one state wherever it is drawn, each told apart by a word and a
 * shape (R15, R21).
 */
function checkState(check: CheckStateReads): CheckState {
  if ((check.results ?? []).length === 0) return "no-result";
  if (check.pass_stands === undefined) return "unreadable";
  if (check.pass_stands.answer === "yes") return "passed";
  if (check.pass_stands.answer === "unknown") return "unmarked";
  const standing = newestFirst(standingResults(check))[0];
  if (standing === undefined) return "retracted";
  if (standing.check_words_moved === true) return "older";
  return standing.commit_stands?.answer === "other_commit" ? "older" : "failed";
}

/** What a check's state is read from: the server's answer over its results and the results
 *  themselves. `pass_stands` is declared required by the API types and is missing from what
 *  the deployed server sends, so it is optional here and the page reads a payload without it
 *  rather than throwing over the whole issue page. */
export type CheckStateReads = Pick<CheckBlock, "results"> & {
  readonly pass_stands?: CheckBlock["pass_stands"];
};

/** How one check stands, as the server reads the commit each of its results ran at. */
export interface CheckStanding {
  /** How it stands, in the states `NODE_STATE_WORDS.check` names. */
  readonly state: CheckState;
  /** The result that state stands on: the newest result over the check's words now at its
   *  basis, a pass or a fail, where one is there; else the newest result that stands at all,
   *  and none for a check the server says passed with no pass at its basis in the read. Absent
   *  where none stands. */
  readonly stands?: CheckResult;
}

/**
 * How a check stands: the page's one reading of a check's state (R10, R17), and the only one -
 * `checkState` is its own fallback and nothing else reads it. The tree's check node holds it
 * (`tree-read/tree-nodes.ts`' `draftServing`), so the outline's row and the Verification
 * filter's failing count (`presets.ts`'s `filterSignals`)
 * read it, and so does the Verification filter's body (`statement-state.ts`'s `checkReading`):
 * one check never reads Passed on its row and Failed in the filter.
 *
 * Each result is read against the commit the server names for its own check - the basis the
 * check compares with, which the result's `commit_stands` answers (`at_basis`, `other_commit`,
 * `unknown`) - as the server's `standingCommit` and `passStandsOf` read it
 * (`results_comparison.go`), and never against one commit the page names for every check: two
 * checks of one issue compare with different commits, a `before_merge` check with the pull
 * request's head and an `after_merge` one with the branch it merged into, so a pass at its own
 * check's basis reads Passed whatever newer result another check holds at another commit, as its
 * own fact reads "Up to date" (`check-facts.ts`'s `resultFacts`). A result stands for something
 * at the basis only where nobody withdrew it and its check's words have not moved since it was
 * recorded.
 *
 * **The latest evidence at the basis decides.** A check with no result reads No result, and one
 * whose payload carries no `pass_stands` reads Could not read, as `checkState` reads them. Else,
 * where a result stands for something at the basis, the newest of them is the check's state: a
 * fail there is Failed, whatever older pass the server still counts as standing (`pass_stands`
 * says yes for any pass at a basis, later fails or not), and a pass there is Passed. A newer
 * result at another commit, or at one Dispatch could not place, says nothing about the basis,
 * so it neither hides a fail there nor turns a pass there over; and a result recorded against
 * words the check no longer has stands for nothing, since it is evidence about a check that no
 * longer reads that way. The results read sends no verdict but `pass` and `fail`
 * (`api/results.go` refuses any other). Where nothing stands at the basis, it is `checkState`'s
 * answer: Out of date for a newest result at another commit or over moved words, Retracted,
 * Cannot tell, or Failed for a newest fail at a commit Dispatch could not place.
 */
export function checkStanding(check: CheckStateReads): CheckStanding {
  const standing = newestFirst(standingResults(check));
  // The newest result that stands for something at the commit the server names for the check:
  // over the check's words now and at its basis (`standingCommit`). A check with no result has
  // none, and one with no `pass_stands` is read before it.
  const here = check.pass_stands === undefined ? undefined : standing.find(standsAtBasis);
  if (here !== undefined) {
    return { stands: here, state: here.verdict === "pass" ? "passed" : "failed" };
  }
  const state = checkState(check);
  // No pass is at the basis in the read, so a check the server says passed stands on none of
  // its results here.
  const stands = state === "passed" ? undefined : standing[0];
  return { state, ...(stands === undefined ? {} : { stands }) };
}

/** Whether `result` stands at its check's basis: recorded against the check's words now
 *  (`check_words_moved`) and at the commit the server names for the check (`commit_stands` is
 *  `at_basis`), as the server's `standingCommit` reads it. It is the one reading of "this result
 *  stands at its check's basis" that a check's state (`checkStanding`) and its body's mark
 *  (`statement-state.ts`'s `atBasis`) share, so the two never disagree. Whether anybody withdrew
 *  the result is no part of it: both read it only of a result that stands (`standingResults`). */
export function standsAtBasis(result: CheckResult): boolean {
  return result.check_words_moved !== true && result.commit_stands?.answer === "at_basis";
}

/** Every result of `check` nobody withdrew, which is every result that stands as evidence: the
 *  server's `passStandsOf` skips a withdrawn one, and so does every reading of the page. */
export function standingResults(check: CheckStateReads): readonly CheckResult[] {
  return (check.results ?? []).filter((result) => result.retracted === undefined);
}

/** `results` newest first, by the time each was recorded. The server already answers newest
 *  first; this reads the times themselves, so the order is the model's own and no caller
 *  depends on the order a read happened to arrive in. */
export function newestFirst(results: readonly CheckResult[]): readonly CheckResult[] {
  return [...results].sort(newerFirst);
}

/** Orders two results newest first by the time each was recorded, to the microsecond the server
 *  writes (`compareTimestamps`), which `Date.parse` would cut to the millisecond and so tie. A
 *  time that cannot be read (`isTimestamp`) is the oldest possible, so a result with no readable
 *  time never outranks one that has one, two such tie, and no time throws. */
function newerFirst(first: CheckResult, second: CheckResult): number {
  const firstRead = isTimestamp(first.created_at);
  const secondRead = isTimestamp(second.created_at);
  if (firstRead && secondRead) return compareTimestamps(second.created_at, first.created_at);
  return Number(secondRead) - Number(firstRead);
}
