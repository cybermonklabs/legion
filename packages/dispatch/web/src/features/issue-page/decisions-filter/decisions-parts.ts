import type {
  Actor,
  CommentWithTrees,
  Decision,
  DisplayTree,
  IssueDecisions,
} from "../../../api/types";
import { compareTimestamps, isTimestamp } from "../../../lib/timestamps";
import { blockFragment } from "../../refs/block-reference";
import { buildIssuePath, parseNodeReference } from "../../refs/routes";
import type { TextSource } from "../../refs/text-references";
import { type ChangeComment, inReviewList } from "../decision-review";
import { presetPath } from "../issue-address";
import { type ParentLine, parentLineOf, type TreeAsk, type TreeNode } from "../issue-tree";
import { type DecisionState, isReader } from "../page-words";

/**
 * The Decisions filter's parts, read from the tree, the decisions read, the issue's asks and its
 * comments (R2, R9, R35).
 *
 * The filter is one list in parts, each under its own heading and in this order (R2):
 *
 * 1. **The open questions.** Every ask of the issue that is still open, those whose turn is the
 *    reader's first, each group in the order the outline draws them, which is the order the
 *    next-question key walks.
 * 2. **The decisions that wait for the reader's review** (R35): each agent decision that waits
 *    and that a `shaped` tie names a statement of this issue for, newest first. The list is
 *    read from the ties alone (`decision-review.ts`'s `inReviewList`): a decision that waits and
 *    is tied to nothing, or to a node that is no statement, is no entry of it, and neither is
 *    one the reader sent a Change on, which waits on its agent.
 * 3. **The reader's own decisions**, newest first: each ask the reader answered or declined,
 *    and each agent decision a statement rests on that the reader kept or asked to change, by
 *    the time they did. An answered ask is the person's decision itself, and a declined one
 *    reads as a question the person declined,
 *    never as an answer.
 * 4. **What others decided**, newest first: the same entries where the person who answered,
 *    declined, kept or changed is not the reader, a question its asker took back, and a section
 *    of `decisions.md` the server marks a person's with nobody it can be laid to. R2 names the
 *    third part "his own decisions", and Dispatch has several readers: another person's answer
 *    is no decision of the reader's, and it is still an ask the filter draws (R2: "Decisions the
 *    asks and the decisions"), so it stands under a heading that says whose it is.
 * 5. **The agents' decisions that no statement rests on**, folded at the foot: an agent
 *    decision no `shaped` tie names a statement for, and a replaced one, which no statement
 *    stands on any longer (R5: "A replaced decision draws no line").
 *
 * Before the five parts stands the node the address names where it is neither an ask nor a
 * decision - a comment, a statement, a check: the address outranks the filter (`presets.ts`),
 * so the view holds it, and the filter draws it as a row of its own kind, which the page focuses.
 *
 * What the filter draws none of is an agent's reading of a person's answer (R2, R9: "No
 * restatement of a person's answer appears anywhere in the view"). Today's nearest thing to a
 * `reading` node is a section of `decisions.md` the server marks a person's because its blocks
 * name an ask (`Decision.kind`, `api/decisions.go`): where that ask is an answered ask of this
 * issue, the section restates the answer, the ask's own row is the decision, and the section is
 * drawn under the ask in the outline and nowhere in this filter (`isReading`). That section is
 * where today's read holds the decision's `shaped` ties, so the ask's entry carries the
 * statements they name: the person's decision reaches every statement it shaped.
 *
 * This module is pure. It holds no React and makes no request, so the body
 * (`DecisionsFilterBody.tsx`) and the unit tests read one model.
 *
 * ## What today's reads hold, and what they do not
 *
 * - **The `shaped` tie** is a link from a block of a decision to a block of the spec
 *   (`Decision.links`), which is the edge as today's read carries it; the tuple is the decision
 *   model's. A tie counts where its far end is a statement of this issue, read
 *   from the tree.
 * - **When a decision was made** is the time its words last changed (`Decision.changed_at`);
 *   the decisions read holds no other time for one.
 * - **A Change** is a comment a person anchored to the decision's words since it last changed
 *   (`decision-review.ts`'s `decisionChanges`); the decisions read holds no state for it.
 * - **An agent decision's author, its level of uncertainty, its reason and its readings** are
 *   the decision model's: the read holds no author and one yes or no, `unsure`, which the
 *   row draws as the one word it has.
 */

/** The parts, in the order the filter draws them (R2). */
export const DECISIONS_PART_ORDER = [
  "named",
  "open_questions",
  "review",
  "yours",
  "others",
  "agents",
] as const;

/** One part of the filter, by its own token, which is its `data-decisions-part`. */
export type DecisionsPart = (typeof DECISIONS_PART_ORDER)[number];

/** One statement a decision's `shaped` tie names, as the decision's row draws it. */
export interface ShapedStatement {
  /** The statement's node id. */
  readonly id: string;
  /** The statement, whose one line a row projects from its words, as R1 cuts a row's
   *  (`issue-tree.ts`'s `lineReading`): the page draws one line for it here. */
  readonly node: TreeNode;
  /** Its address in the conversation, where it has one. */
  readonly path?: string;
  /** Whether the decision changed after it first named the statement and the statement has not
   *  changed since (`DecisionLink.stale`). */
  readonly stale: boolean;
}

/** What every entry holds. */
interface EntryBase {
  /** The moment the entry is ordered by, where the model holds one: when an ask closed, or was
   *  asked while it is open, when a decision was kept or asked to change, and otherwise when it
   *  was made. */
  readonly at?: string;
  /** The node, as the whole tree holds it at the place it is edited. */
  readonly node: TreeNode;
  /** The node's parent in the conversation as one line on its row (R2), since no row of this
   *  filter stands under another, with its address and a comment's or a message's marks, as a
   *  filter's parent line holds them (`presets.ts`); absent for a node that hangs under the
   *  issue itself. */
  readonly parent?: ParentLine;
  /** The address of the node in the conversation, which the entry links to. */
  readonly path?: string;
  /** The statements of this issue the entry's `shaped` ties name, each once, in the ties' own
   *  order: a decision's own, and for an answered ask those of the section of `decisions.md`
   *  that restates its answer. */
  readonly shapes: readonly ShapedStatement[];
}

/** That a person declined an ask, with the note they wrote where they wrote one. */
export interface AskDeclined {
  readonly by: Actor;
  /** What the person wrote as they declined, as stored; absent where they wrote nothing. */
  readonly note?: string;
  /** The record `note` is: the ask, on this issue. */
  readonly source: TextSource;
  /** The server's reading of `note`, where the asks read sent one. */
  readonly tree?: DisplayTree;
}

/** One ask of the filter: an open question, or a closed one. */
export interface AskEntry extends EntryBase {
  /** The person who declined the question, with the note they wrote where they wrote one;
   *  absent for every ask that was not declined. */
  readonly declined?: AskDeclined;
  readonly kind: "ask";
}

/** The change a person asked of a decision, as its row draws it under the decision's words. */
export interface DecisionChange {
  readonly at: string;
  /** What the person wrote, as stored. */
  readonly body: string;
  readonly by: Actor;
  /** When the person last edited what they wrote (`Comment.edited_at`), which the row says
   *  ` · edited` after the words for (`EditedMark`, INFRA-1523, R2); null for a change nobody
   *  edited. */
  readonly editedAt: string | null;
  /** The comment's own id, which is its node's in the conversation. */
  readonly id: string;
  /** The comment's address in the conversation. */
  readonly path: string;
  /** The record `body` is: the comment, on this issue. */
  readonly source: TextSource;
  /** The server's reading of `body`, where the comments read sent one. */
  readonly tree?: DisplayTree;
}

/** One decision of the filter. */
export interface DecisionEntry extends EntryBase {
  /** The change a person asked of it since it last changed, where one did (the tester's F6). */
  readonly change?: DecisionChange;
  /** The person who kept it (`DecisionKeep.kept_by`), where it stands kept. */
  readonly keptBy?: Actor;
  readonly kind: "decision";
  /** How it stands, where the model gives it a state (`NODE_STATE_WORDS.decision`). */
  readonly standing?: DecisionState;
}

/** A node the address names that is neither an ask nor a decision, drawn as a row of its own
 *  kind by its words. */
export interface NamedEntry extends EntryBase {
  readonly kind: "named";
}

/** One entry of the filter. */
export type DecisionsEntry = AskEntry | DecisionEntry;

/** The parts, each in the order it is drawn. */
export interface DecisionsParts {
  readonly agents: readonly DecisionEntry[];
  /** The node the address names, where it is neither an ask nor a decision. */
  readonly named: readonly NamedEntry[];
  readonly open_questions: readonly AskEntry[];
  readonly others: readonly DecisionsEntry[];
  readonly review: readonly DecisionEntry[];
  readonly yours: readonly DecisionsEntry[];
}

/** What the model reads of an ask beside its node: how it was closed. */
export type DecisionsAsk = Pick<TreeAsk, "id" | "resolution">;

/**
 * The address of `node` in the conversation: an ask's own, and for a block of a document
 * Conversation's own address (`presetPath`, `/conversation`) with the block as its fragment,
 * which opens the outline on that block's node (`issue-address.ts`,
 * dispatch://INFRA-5702/spec#b-r-new-page). A decision's own address opens `decisions.md` at
 * its heading, which is the document and not the conversation.
 */
export function conversationPath(node: Pick<TreeNode, "reference">): string | undefined {
  const route = node.reference === undefined ? undefined : parseNodeReference(node.reference);
  if (route === undefined || !("key" in route)) return undefined;
  if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
    return buildIssuePath({ id: route.id, key: route.key, kind: route.kind });
  }
  const block = "block" in route ? route.block : undefined;
  return block === undefined
    ? undefined
    : `${presetPath(route.key, "conversation")}${blockFragment(block.id)}`;
}

/**
 * The filter's parts.
 *
 * `root` is the whole tree, which holds each ask and each decision with its words and each
 * statement a tie may name; `view` is the tree as the Decisions filter leaves it
 * (`presetView`), whose nodes are the ones the filter by words kept and the one the address
 * names. `nodeIdOf` is how this issue addresses a node (`tree-drafts.ts`'s `treeContext`), so
 * a tie resolves to the node it names however its address is written. `changes` is the change
 * a person asked of each decision, by the decision's address (`decisionChanges`), with the
 * server's reading of its body where the comments read sent one, and `reader` who is reading,
 * which is what tells their own decisions from other people's.
 */
export function decisionsParts({
  asks,
  changes,
  decisions,
  issueKey,
  nodeIdOf,
  reader,
  root,
  view,
}: {
  /** The issue's asks, for how each closed one was closed. */
  readonly asks: readonly DecisionsAsk[] | undefined;
  readonly changes?: ReadonlyMap<string, ChangeComment & Pick<CommentWithTrees, "body_tree">>;
  readonly decisions: IssueDecisions | undefined;
  /** The issue's key, which a change's address in the conversation is built with. */
  readonly issueKey: string;
  readonly nodeIdOf: (reference: string) => string | undefined;
  readonly reader: string | undefined;
  /** The whole tree. */
  readonly root: TreeNode;
  /** The tree as the Decisions filter leaves it; the whole tree where none is given. */
  readonly view?: TreeNode;
}): DecisionsParts {
  const placed = placements(root);
  const kept = view === undefined ? undefined : idsOf(view);
  const rows = new Map((decisions?.decisions ?? []).map((row) => [row.ref, row]));
  const closings = new Map((asks ?? []).map((ask) => [ask.id, ask.resolution]));
  const isStatement = (reference: string): boolean =>
    placed.get(nodeIdOf(reference) ?? "")?.node.type === "requirement";
  // Each answered ask by its own id, which is how a decision names the question it restates
  // (`Decision.asks`): an ask the server indexed from a block of a document is drawn as that
  // block, so its node id is not its id.
  const answered = new Set<string>();
  for (const { node } of placed.values()) {
    const id = askIdOf(node);
    if (id !== undefined && node.answer !== undefined) answered.add(id);
  }
  // What each answered ask shaped: the ties of every section that restates its answer, which is
  // where today's read holds a person's decision's `shaped` ties.
  const shapedByAsk = new Map<string, Map<string, ShapedStatement>>();
  for (const row of rows.values()) {
    if (!isReading(row, answered)) continue;
    for (const ask of (row.asks ?? []).filter((one) => answered.has(one))) {
      const held = shapedByAsk.get(ask) ?? new Map<string, ShapedStatement>();
      for (const shape of shapesOf(row, nodeIdOf, placed)) {
        held.set(shape.id, { ...shape, stale: shape.stale || held.get(shape.id)?.stale === true });
      }
      shapedByAsk.set(ask, held);
    }
  }

  const open: AskEntry[] = [];
  const review: DecisionEntry[] = [];
  const yours: DecisionsEntry[] = [];
  const others: DecisionsEntry[] = [];
  const agents: DecisionEntry[] = [];
  const named: NamedEntry[] = [];
  // A decision of a person stands with the reader's own where that person is the reader, and
  // with what others decided otherwise, a decision nobody is recorded for included.
  const decidedBy = (by: Actor | undefined): DecisionsEntry[] =>
    isReader(by, reader) ? yours : others;
  for (const { node, parent } of placed.values()) {
    if (kept !== undefined && !kept.has(node.id)) continue;
    const base = {
      node,
      ...(parent === undefined ? {} : { parent }),
      ...pathOf(node),
    };
    if (node.type === "ask") {
      const askId = askIdOf(node) ?? "";
      const shapes = [...(shapedByAsk.get(askId)?.values() ?? [])];
      const closing = closings.get(askId);
      // `question` is the ask's own `state === "open"` (`tree-drafts.ts`'s `addAsks`).
      if (node.question) {
        open.push({ ...base, ...timeOf(node.at), kind: "ask", shapes });
        continue;
      }
      const note = closing?.reason ?? "";
      const declined: AskDeclined | undefined =
        node.state === "declined" && closing !== undefined
          ? {
              by: closing.actor,
              source: { id: askId, issue: issueKey, kind: "ask" },
              ...(note === "" ? {} : { note }),
              ...(note === "" || closing.reason_tree === undefined
                ? {}
                : { tree: closing.reason_tree }),
            }
          : undefined;
      decidedBy(node.answer?.by ?? closing?.actor).push({
        ...base,
        ...timeOf(node.answer?.at ?? closing?.at ?? node.at),
        ...(declined === undefined ? {} : { declined }),
        kind: "ask",
        shapes,
      });
      continue;
    }
    if (node.type !== "decision") {
      // A node of another kind is in the view only where the address named it; with no view
      // there is no address, and the issue itself is no row of a filter.
      if (kept !== undefined && node !== root) {
        named.push({ ...base, ...timeOf(node.at), kind: "named", shapes: [] });
      }
      continue;
    }
    const row = rows.get(node.reference ?? "");
    if (row === undefined) {
      // A decision the decisions read holds no row for, which is a tree one read ahead of it.
      agents.push({ ...base, ...timeOf(node.at), kind: "decision", shapes: [] });
      continue;
    }
    if (isReading(row, answered)) continue;
    const shapes = shapesOf(row, nodeIdOf, placed);
    const asked = row.replaced ? undefined : changes?.get(row.ref);
    const standing = standingOf(row, asked !== undefined);
    const keptBy: Actor | undefined =
      standing !== "kept" || row.kept === null ? undefined : { id: row.kept.kept_by, kind: "user" };
    const entry: DecisionEntry = {
      ...base,
      ...timeOf(row.changed_at ?? node.at),
      ...(asked === undefined
        ? {}
        : {
            change: {
              at: asked.created_at,
              body: asked.body,
              by: asked.author,
              editedAt: asked.edited_at,
              id: asked.id,
              path: buildIssuePath({ id: asked.id, key: issueKey, kind: "comment" }),
              source: { id: asked.id, issue: issueKey, kind: "comment" },
              ...(asked.body_tree === undefined ? {} : { tree: asked.body_tree }),
            },
          }),
      ...(keptBy === undefined ? {} : { keptBy }),
      kind: "decision",
      shapes,
      ...(standing === undefined ? {} : { standing }),
    };
    if (row.kind !== "agent") {
      // A section the server marks a person's that restates no answer of this issue: it names
      // an ask the tree does not hold, or one nobody answered. The server's word for whose it
      // is stands, and no read says which person (the decision model's `decided_by`).
      others.push(entry);
    } else if (row.replaced) {
      agents.push(entry);
    } else if (asked !== undefined) {
      // The reader asked for a change: the decision waits on its agent, and stands with the
      // decisions of whoever asked, by the time they did.
      decidedBy(asked.author).push({ ...entry, ...timeOf(asked.created_at) });
    } else if (inReviewList(row, isStatement)) {
      review.push(entry);
    } else if (shapes.length === 0) {
      agents.push(entry);
    } else {
      // An agent's decision a statement rests on that does not wait: a person kept it as it
      // reads now, and it is ordered by the time they did.
      decidedBy(keptBy).push({
        ...entry,
        ...timeOf(row.kept?.kept_at ?? row.changed_at ?? node.at),
      });
    }
  }
  return {
    agents: newestFirst(agents),
    named,
    // The questions whose turn is the reader's stand first, and each group keeps the order the
    // outline draws it in.
    open_questions: [
      ...open.filter((entry) => entry.node.waiting),
      ...open.filter((entry) => !entry.node.waiting),
    ],
    others: newestFirst(others),
    review: newestFirst(review),
    yours: newestFirst(yours),
  };
}

/** A node at the place it is edited, with its parent as the one line a row draws for it. */
interface Placement {
  readonly node: TreeNode;
  readonly parent?: ParentLine;
}

/** Every node of the tree by its id, in reading order, at its first placement, which is where it
 *  is edited (`TreeNode.transcluded`), with the parent it hangs under there: its one line, its
 *  address and its author's corrections of it, as the outline's filters give a row's parent
 *  (`presets.ts`). The issue itself is no parent a row names: the page is the issue. */
export function placements(root: TreeNode): ReadonlyMap<string, Placement> {
  const placed = new Map<string, Placement>();
  const walk = (node: TreeNode, above: TreeNode | undefined): void => {
    if (!placed.has(node.id)) {
      placed.set(node.id, {
        node,
        ...(above === undefined || above === root ? {} : { parent: parentLineOf(above) }),
      });
    }
    for (const child of node.children) walk(child, node);
  };
  walk(root, undefined);
  return placed;
}

/** The id of every node `view` holds. */
function idsOf(view: TreeNode): ReadonlySet<string> {
  const ids = new Set<string>();
  const walk = (node: TreeNode): void => {
    ids.add(node.id);
    for (const child of node.children) walk(child);
  };
  walk(view);
  return ids;
}

/** The ask a node is, by the ask's own id; undefined for every other node. */
export function askIdOf(node: TreeNode): string | undefined {
  if (node.type !== "ask" || node.reference === undefined) return undefined;
  const route = parseNodeReference(node.reference);
  return route?.kind === "ask" ? route.id : undefined;
}

/** `at` as an entry's time, where it is a time the page can order by. */
function timeOf(at: string | null | undefined): { readonly at?: string } {
  return at == null || !isTimestamp(at) ? {} : { at };
}

/** The entry's address in the conversation, where its node has one. */
function pathOf(node: TreeNode): { readonly path?: string } {
  const path = conversationPath(node);
  return path === undefined ? {} : { path };
}

/**
 * Whether `row` is an agent's reading of a person's answer, which the filter draws none of
 * (R2, R9): a section the server marks a person's - its blocks name an ask - where one of those
 * asks is an answered ask of this issue. The ask's own row is that decision, in the person's
 * words.
 */
export function isReading(row: Decision, answered: ReadonlySet<string>): boolean {
  return row.kind === "person" && (row.asks ?? []).some((ask) => answered.has(ask));
}

/** How a decision stands (`NODE_STATE_WORDS.decision`), read as the tree reads it
 *  (`tree-drafts.ts`'s `decisionState`): replaced first, then a change a person asked for, then
 *  waiting for review, then kept as it reads now. A decision in none of them draws no state. */
function standingOf(row: Decision, changed: boolean): DecisionState | undefined {
  if (row.replaced) return "replaced";
  if (changed) return "change_requested";
  if (row.waiting_review) return "waiting_review";
  return row.kept === null ? undefined : "kept";
}

/** The statements of this issue `row`'s ties name, each once, in the ties' own order. */
function shapesOf(
  row: Decision,
  nodeIdOf: (reference: string) => string | undefined,
  placed: ReadonlyMap<string, Placement>
): readonly ShapedStatement[] {
  const shapes = new Map<string, ShapedStatement>();
  for (const link of row.links ?? []) {
    const statement =
      link.to.ref === undefined ? undefined : placed.get(nodeIdOf(link.to.ref) ?? "");
    if (statement === undefined || statement.node.type !== "requirement") continue;
    const node = statement.node;
    const held = shapes.get(node.id);
    shapes.set(node.id, {
      id: node.id,
      node,
      ...pathOf(node),
      stale: link.stale || held?.stale === true,
    });
  }
  return [...shapes.values()];
}

/** `entries` with the newest first. An entry the model holds no time for stands last, and two
 *  entries of one instant keep the order the outline draws them in, so the same reads always
 *  draw the same list. */
function newestFirst<Entry extends EntryBase>(entries: readonly Entry[]): readonly Entry[] {
  return entries
    .map((entry, index) => ({ entry, index }))
    .sort((one, other) => {
      const left = one.entry.at;
      const right = other.entry.at;
      if (left === undefined || right === undefined) {
        return left === right ? one.index - other.index : left === undefined ? 1 : -1;
      }
      return compareTimestamps(right, left) || one.index - other.index;
    })
    .map(({ entry }) => entry);
}
