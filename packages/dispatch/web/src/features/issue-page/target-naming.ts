import { artifactAtAddress, primarySpec, roleDocument } from "../../api/issue-cache";
import type { Artifact, DisplayTree, IssueDetails, ReferenceTarget } from "../../api/types";
import { blockWords } from "../decision-card/adapters";
import { storedReading } from "../decision-card/stored-reading";
import { type AgentText, actorLabelText } from "../refs/AgentName";
import { deletedLineText, type PostDeletion, postDeletion } from "../refs/post-marks";
import type { PublishedReferences, ReferenceNameReading } from "../refs/reference-names";
import { buildDispatchReference, issueOrDocumentTitle, parseNodeReference } from "../refs/routes";
import { firstWordedBlock } from "../refs/tree-fragment";
import type { IssueTreeReads, NodeChip } from "./issue-tree";
import { NODE_TYPE_NAMES, NODE_TYPES, type NodeType, PRESET_NAMES } from "./page-words";
import { blockNodeId, nodeIdOf, titleReading } from "./statement-rules";

/**
 * The page's one rule for how a target of the batched reference read is named: by the chip
 * naming it (`treeContext`'s `chipOf`), which the tree's chips read and the page publishes for
 * every surface beside the tree (`targetNaming`, `chipNaming`), so a chip and a link naming one
 * target never disagree.
 *
 * It holds no React, makes no request, and imports no code of the tree's model - `tree-drafts.ts`,
 * `issue-tree.ts`, `useIssueTree.ts` - only its types, so Dashboard publishes the names it reads
 * by the rule the tree names them by, without carrying the tree in its chunk (R14).
 */

/** How the model names the actor of a delete: as `AgentName` names one before the agent
 *  registry answers (`actorLabelText`), since the model is pure and reads no registry. */
export const ACTOR: Pick<AgentText, "label"> = { label: actorLabelText };

/** What every builder needs of the reads: how this issue addresses a node, and what a reference
 *  points at. Held as one value so the placement and the names are read the same way everywhere. */
export interface TreeContext {
  /** A chip naming what `reference` points at, by the one batched reference read and the
   *  issue's own documents (`treeContext`): one chip per reference, the same value on every
   *  call. */
  readonly chipOf: (reference: string) => NodeChip;
  /** The words of the stored excerpt the batched reference read names `reference`'s target by
   *  (`ReferenceTarget.name`), with a typed block's directive left out (`blockWords`); `""` for
   *  a target the read has not answered for and for one nothing answers to now. It is a block's
   *  source, cut at 480 runes and unread, so no chip draws it: it is where a node's citations
   *  are read from beside its words, and a block's words where the read sent no reading. */
  readonly excerptOf: (reference: string) => string;
  readonly key: string;
  /** The node `reference` names, as this issue addresses it; undefined for anything else. */
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** The slug of the issue's spec (`primarySpec`), `""` for an issue with none. */
  readonly specSlug: string;
  /** The slug of one document of the issue, by its artifact id. */
  readonly slugOf: (artifactId: string) => string | undefined;
  /** The server's reading of what `reference` points at, where the batched reference read sent
   *  one: a typed block's own inner content, whole, references kept (`ReferenceTarget.text_tree`). */
  readonly treeOf: (reference: string) => DisplayTree | undefined;
  /** The reader's login, which an open question waits on where its turn is theirs; undefined
   *  until the login resolves, and nothing waits on nobody. */
  readonly viewer: string | undefined;
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
 * (R8), and a chip cuts it again where it draws it (`issue-tree.ts`'s `chipName`).
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
  // The kind of each typed block of the documents the tree read, by its node id, which is all a
  // chip can say of a block of this issue the read sent no words for.
  const types: readonly string[] = NODE_TYPES;
  const kinds = new Map<string, NodeType>();
  for (const document of reads.documents ?? []) {
    const slug = slugs.get(document.artifactId);
    if (slug === undefined) continue;
    for (const block of document.blocks) {
      if (types.includes(block.type)) {
        kinds.set(blockNodeId(slug, block.id), block.type as NodeType);
      }
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
    excerptOf: (reference: string): string => {
      const target = names.get(reference);
      return target === undefined || target.gone ? "" : blockWords(target.name);
    },
    key,
    nodeIdOf: nodeIdOfReference,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
    treeOf: (reference: string) => names.get(reference)?.text_tree,
    viewer: reads.viewer,
  };
}

/**
 * What the page publishes from the targets of the batched reference read (`reads.targets`) for
 * every surface beside the tree to name them by (`refs/reference-names.tsx`), each read by the
 * rule a chip naming the target is named by (`treeContext`), so a link and a chip naming one
 * target never disagree, save that a chip draws the page's word for a whole document in place
 * of its name (`ReferenceNameReading.chipWord`): the chip of each answered target, named by
 * `chipNaming`.
 */
export function targetNaming(reads: IssueTreeReads): Omit<PublishedReferences, "asked"> {
  const context = treeContext(reads);
  return chipNaming((reads.targets ?? []).map((target) => context.chipOf(target.ref)));
}

/** What the page publishes of the batched name read until it answers: no target read, and the
 *  same empty value on every render, so no surface beside the tree draws again for a read that
 *  names nothing. The tree publishes it (`useIssueTree.ts`), and so does Dashboard
 *  (`useDashboardNames`). */
export const NO_TARGET_NAMING: Omit<PublishedReferences, "asked"> = {
  ambiguous: new Set(),
  deleted: new Map(),
  edited: new Set(),
  gone: new Set(),
  readings: new Map(),
  unnamed: new Set(),
};

/**
 * How the page names each target the batched reference read answered, by the chip naming it
 * (`targetNaming`), under the chip's own reference (`NodeChip.ref`). Each reference is in one of
 * the four - `readings`, `ambiguous`, `gone`, `unnamed` - read from its chip in one walk, so the
 * four cannot disagree, and the marks of a named post are read in the same walk.
 *
 * - **`readings`**: how each target that something answers to now is named, by its address: the
 *   reading of a chip naming it (`NodeChip.reading`), which the store projects.
 * - **`ambiguous`**: the references the read answered ambiguous: each writes short an id two or
 *   more targets carry, so it names none of them, and a chip naming it says so
 *   (`NodeChip.ambiguous`). A surface draws one as its author wrote it and links nothing.
 * - **`gone`**: the references the read answered that nothing answers to now, an ambiguous one
 *   apart, which a surface draws as the words its author wrote (R8) and reads no target for.
 * - **`unnamed`**: the references the read answered with nothing to name them by - another
 *   issue's block it sent no words for - which a surface reads for itself, as a link to the
 *   target does (R8).
 * - **`edited`**: the references among `readings` whose post its author edited, a comment, a
 *   message or a block of either (`NodeChip.editedAt`). A link that draws a published name cuts
 *   it to its own length and then marks it ` · edited`, so no cut takes the mark: a mark
 *   published inside the name would be cut off a long one. A row's link names the post in one
 *   string, marked with `editedName` (`row-references.ts`); the row's chip draws the mark as an
 *   element of its own with `EditedMark`.
 * - **`deleted`**: the references among `readings` whose comment or message its author deleted,
 *   by its delete (`NodeChip.deletedAt`, `deletedBy`), each named in `readings` by its deleted
 *   line. A row's words draw that line in the deleted line's italic and name its actor as the
 *   page names an author, where the published name holds the actor as no registry knows it
 *   (`RowText.tsx`, INFRA-1196's decision 342); every other link draws the published name.
 *
 * Every reference in `edited` and in `deleted` is in `readings`: a target with no name takes no
 * mark, and a block of a deleted post is gone. None is in both `edited` and `deleted`: a deleted
 * post's chip carries no edit.
 */
export function chipNaming(chips: Iterable<NodeChip>): Omit<PublishedReferences, "asked"> {
  const ambiguous = new Set<string>();
  const deleted = new Map<string, PostDeletion>();
  const edited = new Set<string>();
  const gone = new Set<string>();
  const unnamed = new Set<string>();
  const readings = new Map<string, ReferenceNameReading>();
  for (const chip of chips) {
    if (chip.ambiguous === true) ambiguous.add(chip.ref);
    else if (chip.gone) gone.add(chip.ref);
    else if (chip.reading === undefined) unnamed.add(chip.ref);
    else {
      readings.set(chip.ref, chip.reading);
      if (chip.editedAt !== undefined) edited.add(chip.ref);
      const deletion = postDeletion({ deleted_at: chip.deletedAt, deleted_by: chip.deletedBy });
      if (deletion !== null) deleted.set(chip.ref, deletion);
    }
  }
  return { ambiguous, deleted, edited, gone, readings, unnamed };
}

/**
 * How the page names a whole document: by `title`, the title a link to it reads
 * (`issueOrDocumentTitle`), read as one text run (`titleReading`), which the page publishes for
 * every plain line to read (R8), with the page's word for it (`documentWord`), which every chip
 * draws in that name's place (`chipWord`), since a chip never draws a document's file name
 * (decision 39). `own` is the document of `issue` the reference names, if any. A document
 * nothing titles is published by that word as well.
 */
export function documentReading(
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
 * spec and for the plan, Verification and decisions documents, and the page's word for a
 * document (`NODE_TYPE_NAMES.document`) for any other, another issue's included (`undefined`),
 * since another issue's document plays no part of this one. A link and every line read a
 * document by its own name, which is its file name, `spec.md` (R8); a chip never draws it
 * (decision 39). A chip the page has given no word yet draws this rule as far as an address and
 * the row's issue say it (`node-row-chip.ts`'s `documentWord`).
 *
 * Which document plays which part is the issue read's answer and nothing else: the id it names
 * for each (`primary_artifact_id`, `plan_artifact_id`, `verification_artifact_id`,
 * `decisions_artifact_id`), read by `primarySpec` and `roleDocument`, the rule the tree reads the
 * documents' blocks by (`useIssueTree.ts`). Where the issue names no id for a part - none
 * (`""`), or a Dispatch older than that part (absent) - no document plays it, and a document
 * flagged `primary` or carrying that part's `role` is named by the page's word for a document:
 * a chip naming "Spec" or "Decisions" opens a document whose blocks the tree reads as that part.
 */
function documentWord(issue: IssueDetails, document: Pick<Artifact, "id"> | undefined): string {
  if (document === undefined) return NODE_TYPE_NAMES.document;
  if (document.id === primarySpec(issue)?.id) return PRESET_NAMES.spec;
  for (const role of ["plan", "verification", "decisions"] as const) {
    if (document.id === roleDocument(issue, role)?.id) return PRESET_NAMES[role];
  }
  return NODE_TYPE_NAMES.document;
}
