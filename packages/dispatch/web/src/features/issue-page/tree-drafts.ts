import { referenceSpans } from "@legion/contracts";

import { artifactAtAddress, primarySpec, roleDocument } from "../../api/issue-cache";
import type {
  Actor,
  Artifact,
  Ask,
  CheckBlock,
  CheckResult,
  DisplayTree,
  IssueChild,
  IssueCoverage,
  IssueDetails,
  IssueTreeNode,
  ReferenceTarget,
  RequirementCoverage,
} from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { type AgentText, actorLabelText } from "../refs/AgentName";
import { deletedLineText, type PostDeletion, postDeletion } from "../refs/post-marks";
import type { ReferenceNameReading } from "../refs/reference-names";
import { buildDispatchReference, issueOrDocumentTitle, parseNodeReference } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { firstWordedBlock } from "../refs/tree-fragment";
import { blockIdOfNode } from "./decision-review";
import { artifactsBySlug } from "./issue-artifacts";
import type {
  IssueTreeReads,
  NodeAnswer,
  NodeChip,
  NodeCounts,
  NodeFact,
  NodeOption,
  RestatingDecision,
  TreeAsk,
} from "./issue-tree";
import type { LensFacts } from "./lenses/lens-drafts";
import {
  type AskState,
  type CheckState,
  coverageGapKind,
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
 * at (`treeContext`), how a check stands (`checkStanding`), and the gaps a statement draws
 * (`requirementGaps`).
 *
 * Every node is drafted from the tree read (`tree-read/tree-nodes.ts`'s `treeDraftsFrom`),
 * which places and orders each one. Every function here is pure and takes the reads as its
 * input, and no request is made per node. Where a read sends the server's reading of a node's
 * words beside them, the draft keeps it with the record the words are, which is what the row
 * draws (`TreeNode.tree`).
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
  /** The key of the issue under this one that this node is of, from the read's own `issue`
   *  (`TreeNode.issue`): on each check, plan task and result of another issue that
   *  `model-rows/model-drafts.ts` drafts, so a check of it is a check whatever its standing
   *  (`issue-tree.ts`' `isBorrowedCheck`). Absent on every node of the issue itself. */
  issue?: string;
  /** The child issue this node is, as the issue read carries it (`TreeNode.child`). */
  child?: IssueChild;
  /** The person whose Change this decision waits on (`TreeNode.changedBy`). */
  changedBy?: Actor;
  /** The person who kept this decision as it reads now (`TreeNode.keptBy`). */
  keptBy?: Actor;
  /** The lens facts of a lens, a finding, a risk, a constraint or a statement a constraint names
   *  (`TreeNode.lens`). */
  lens?: LensFacts;
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
  /** The decisions that restate this question's answer, each by its tree id and the tree read's
   *  own id (`TreeNode.restatedBy`); absent on every other draft. */
  restatedBy?: RestatingDecision[];
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
 * nor a block of any other document. It is the one rule both readers of the list read: the
 * outline's `waiting` on a decision (`tree-read/tree-nodes.ts`' `draftDecision`, which the
 * drafting context hands it) and the header's count (`IssuePropertiesRow.tsx`, which hands it to
 * `waitingOnReader`), so the count counts the very decisions the outline marks. It reads a
 * reference by `coverageStatementOf`, so the two never differ.
 */
export function coverageStatements(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">,
  coverage: IssueCoverage | undefined
): (reference: string) => boolean {
  const statementOf = coverageStatementOf(issue, coverage);
  return (reference: string): boolean => statementOf(reference) !== undefined;
}

/**
 * The coverage read's row of the statement a reference names (`RequirementCoverage`), by the one
 * rule `coverageStatements` reads: a requirement that read holds, compared as the tree addresses
 * it, so the spec's block written short and by the spec's own slug name the same row. Undefined
 * for every reference that names none, and for all while that read has not answered. The row
 * holds the statement's words (`text`), the server's reading of them where the read sent one
 * (`text_tree`) and its own address (`ref`): what a view draws a statement by where Conversation's
 * read, which is paged, holds no node of it (`decisions-filter/decisions-parts.ts`' `shapesOf`).
 */
export function coverageStatementOf(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">,
  coverage: IssueCoverage | undefined
): (reference: string) => RequirementCoverage | undefined {
  const specSlug = primarySpec(issue)?.slug ?? "";
  const statements = new Map<string, RequirementCoverage>();
  for (const requirement of coverage?.requirements ?? []) {
    const id = blockNodeId(specSlug, requirement.block_id);
    if (!statements.has(id)) statements.set(id, requirement);
  }
  const nodeIdOfReference = nodeIdOf(issue);
  return (reference: string): RequirementCoverage | undefined => {
    const id = nodeIdOfReference(reference);
    return id === undefined ? undefined : statements.get(id);
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
export function treeContext(reads: IssueTreeReads): TreeContext {
  const key = reads.issue.key;
  const artifacts = reads.issue.artifacts ?? [];
  const slugs = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const spec = primarySpec(reads.issue);
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
  const nodeIdOfReference = nodeIdOf(reads.issue);
  // The whole title of the issue and of each child the issue read lists, by its own address:
  // the names read cuts a title to its first 240 characters, and the issue read holds these
  // whole, as the rows draw them.
  const titles = new Map<string, string>([
    [buildDispatchReference({ key, kind: "issue" }), reads.issue.title],
    ...(reads.issue.children ?? []).map((child): [string, string] => [
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
        reads.issue,
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
  issue: IssueDetails,
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
function documentWord(issue: IssueDetails, document: Pick<Artifact, "id"> | undefined): string {
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
 * read are the draft's text: every node is a node of the tree read, drafted with its words as
 * that read answers them, a block's its stored source and a decision's its block's words
 * (`tree-read/tree-nodes.ts`' `draftOf`). The chip draws the page's word for the document, as
 * every chip naming a whole document does (`documentReading`): the filter's name for the spec
 * and for the plan and Verification documents, and "document" for any other, never its file
 * name (decision 39). It addresses the document as `artifact/<slug>` - the spec too, whose
 * `spec` reference is the Spec filter's address - and carries the name a link to that address
 * reads, the document's file name. A
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

/**
 * Who decided a decision node, as the page names an author: the far end of its `decided_by`
 * tuple, which the tree read answers with that person or session as context
 * (`tree-read/tree-nodes.ts`' `draftDecision`): a session for an agent (`session:<id>`) and a
 * person by login (`person:<login>`), which every other read names as a `user` actor. A session
 * carries the newest title it stamped, which the model's tree route answers on each session node
 * (`title`, INFRA-1253's spec v168), as its origin's `session_title`, where `nodeOf` - the
 * caller's lookup of the answer's nodes by their store ids - finds the node the end names and that
 * node's title is not empty; else it carries no origin. The page names a session by the
 * registry's title first, else that stamped title, else its word for an agent nothing names
 * (`AgentName.tsx`'s `drawnSessionName`). Undefined where the decision names nobody, or an end of
 * another kind.
 */
export function deciderOf(
  end: { readonly id: string; readonly kind: string } | null,
  nodeOf: (readId: string) => Pick<IssueTreeNode, "title"> | undefined
): Actor | undefined {
  if (end === null) return undefined;
  if (end.kind === "session") {
    const title = nodeOf(end.id)?.title;
    return title === undefined || title === ""
      ? { id: end.id, kind: "session" }
      : { id: end.id, kind: "session", origin: { session_title: title } };
  }
  return end.kind === "person" ? { id: end.id, kind: "user" } : undefined;
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
