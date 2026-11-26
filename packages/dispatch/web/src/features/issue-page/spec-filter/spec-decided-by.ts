import type { Actor, CheckBlock, DecisionLink, DisplayTree } from "../../../api/types";
import { compareTimestamps, isTimestamp } from "../../../lib/timestamps";
import type { TextSource } from "../../refs/text-references";
import { type DecisionNode, decidedByPerson } from "../decision-nodes/decision-nodes";
import { askIdOf } from "../decisions-filter/decisions-parts";
import { drawnLine, firstLineOf, firstSentenceLength } from "../first-line";
import { firstBlockWordsRaw, isStatement, lineWordsRaw, type TreeNode } from "../issue-tree";
import { chipWords } from "../node-row-chip";
import { type LineNames, referenceStandIns } from "../one-line";
import { isReader, type NodeState, type NodeType, type StatementState } from "../page-words";
import { withCriterionLines } from "../presets";
import { storedReading } from "../stored-reading";
import { nodesInOrder } from "../tree-keys";
import { statementStandings } from "../verification-filter/statement-state";

/**
 * What decided each statement of the Spec filter, and the mark each statement carries (R5, R17,
 * R22, R35).
 *
 * The Spec filter draws two columns: the statements, and beside each run of statements one
 * decision shaped, that decision in the words it came from (R5). This module is that reading of
 * the tree, and nothing else: it is pure, holds no React and makes no request, so the body
 * (`SpecFilterBody.tsx`) and the unit tests read one model.
 *
 * What a statement's decisions are, in the order they are drawn:
 *
 * 1. **The answered question it hangs under.** A statement that hangs under an answered ask
 *    carries that ask's line with no tuple, "since that decision is its parent" (R5), and "the
 *    line is drawn whether or not" other decisions apply, so it heads the statement's run.
 * 2. **Each `shaped` tie.** A decision whose link names the statement shaped it
 *    (`Decision.links[].to`): a decision node, or a question a person
 *    answered that carries `shaped` ties, which is that person's decision (`decisionNodes`). The
 *    first decision of the statement heads its run, a person's before an agent's; every other
 *    one is named at the end of the statement's own line, which draws "the decisions that
 *    informed it", and a statement may rest on several. A replaced decision draws no line at
 *    all. A decision node whose `decided_by` names a person is that person's decision too
 *    (`decision-nodes.ts`' `decidedByPerson`, INFRA-1196's decision 516).
 * 3. **Nothing.** A statement no decision shaped is drawn as such, which is what makes a
 *    missing tie visible (R35).
 *
 * What a person's decision quotes is the person's own answer, never a restatement of it: the
 * first sentence (`firstSentenceOf`) of the words the answer opens with (`firstBlockWordsRaw`),
 * with each reference in it named in plain words once the sentence is cut
 * (`referenceStandIns`), or, where they wrote none, the options they chose; the whole answer is
 * carried beside it as stored, for the focus on the line to draw character for character (R5).
 * The question it answers is given by its first sentence too, whole, cut by the same rule from
 * the words the question opens with and named after the cut, with no limit of characters: the
 * cell draws it, and the line under each statement, also where the group's heading is that
 * question, whose line is cut by R1's one-line rule (R5).
 * A person's decision whose answered question Conversation's read has not reached - one past
 * the pages of it the page holds - is drawn as that person's answer all the same, read from the
 * Spec view read's own tree (`viewTree`), which holds each answered question whose `shaped` ties
 * name a statement the view holds, as context (INFRA-1253 R8). A decision node the server marks a
 * person's whose answered question neither tree holds is the agent's record of that decision, so
 * it is drawn as an agent's words and never quoted as the person's (R9).
 *
 * An agent's decision carries its own text whole, which the cell draws as a row draws a node's
 * words (`RowText`), and is quoted by its first line, by R1's one-line rule (`lineWordsRaw`,
 * `firstLineOf`), with each reference named once the line is cut: the line under a statement
 * quotes it as one link, and a text drawn with its own links cannot stand inside one. With it
 * goes the one word the page holds for how sure it was (`DecisionNode.unsure`): unsure where the
 * read says so or its block's `uncertainty` is `high`, as a row reads it
 * (`tree-read/tree-nodes.ts`' `draftDecision`). A decision node a person decided is drawn the
 * same way, by its own words, which are that person's: the cell says that person said it, the
 * reader's own words where they are the reader, and names them as who decided it.
 *
 * Every line and quote here is read with each bare address as its author wrote it
 * (`lineWordsRaw` and `firstBlockWordsRaw`), so that `referenceStandIns` names it as a row of
 * the outline names it, from the names the page published (`lineNames`): an issue by its key
 * and its title, and a reference the page has no name for as its author wrote it.
 *
 * A criterion of the view - what a person wants to be able to do, written under the question,
 * comment or message that asked for it - is a row of the statements' list too, in the view's
 * order (R1), drawn as a statement is by its kind, its words and its author, and with no mark,
 * since no check serves it. A statement written under a criterion hangs under it: the criterion
 * is that statement's parent line, and heads the group it stands in.
 *
 * Whether a statement's words changed after the tie that names the decision heading its run is
 * read from the decision's links (`Decision.links`), as the Spec view read answers them on its
 * node, as R5 says until the model holds a tie's own word for it: the tie's far end's
 * `changed_at` is later than the tie's `created_at`, each read as a time (`changedSinceTie`),
 * and a time that cannot be read is no change. A decision that names the statement in more than
 * one of its blocks holds as many ties to it, and the statement changed since that decision only
 * where it changed after each of them. Only the decision the run is headed by decides it, since
 * it is the one the line under the statement names. The answered question a statement hangs
 * under has no tie, and no read of today holds when its statement changed, so a run that
 * question heads never says it (`SpecStatement.changedSince`); nor does a link a person's
 * decision is read with from the answer's `shaped` tuples alone, which carries no time
 * (`decision-nodes.ts`' `shapedLinks`).
 *
 * One part of R5 the body does not draw, which it draws nothing in the place of: an agent's
 * level of uncertainty and its reason, beyond the one word above.
 */

/** Whose words a run of statements came from, which the column's heading says: the reader's own,
 *  another person's, or an agent's. */
export type SpecSaid = "agent" | "person" | "you";

/** The mark at the right of a statement's row: how its checks stand, each at the commit the
 *  server names for it, in the Verification filter's own reading, or that no check a count
 *  reads serves it, as for any statement no count reads, checks or none (`carriesCheck`, R10,
 *  R17). */
export type SpecMark = StatementState | "no_check";

/** The decision that shaped a run of statements, as the left column draws it (R5). */
export interface SpecDecision {
  /** When it was decided: the person's answer's time, or the decision's last change. */
  readonly at?: string;
  /** Who decided it, where the model holds an author. */
  readonly by?: Actor;
  /** The person whose Change it waits on, where it stands asked to change
   *  (`TreeNode.changedBy`), whom its line names after "Changed by". */
  readonly changedBy?: Actor;
  /** How a line names the decision among others, after "informed by": its quote cut to a chip's
   *  length as a chip names its target (`chipWords`), cut with each reference it cites unnamed
   *  and each named after the cut (`referenceStandIns`), so the cut never falls inside a name. */
  readonly chip: string;
  /** The options a person chose, in the ask's own words with each reference they cite named
   *  (`referenceStandIns`); empty where they chose none. */
  readonly chose: readonly string[];
  /** The node that holds the whole of it, which the link to it names: the answered ask whose
   *  answer this is, or the decision. */
  readonly id: string;
  /** The person who kept it, where it stands kept (`TreeNode.keptBy`), whom its line names after
   *  "Kept by". */
  readonly keptBy?: Actor;
  /** The decision node the tie came from, where the tree holds it: the node the cell is drawn
   *  as, which Details draws while the cell holds the focus. A person's decision is drawn by
   *  their answer (`id`), and this is the question itself (`decisionNodes`). */
  readonly node?: string;
  /** The question the words answer, its first sentence whole, read from the words the question
   *  opens with and ending at its first sentence end as the answer's quote does
   *  (`firstSentenceOf`), with each reference named after the cut and no limit of characters:
   *  what the cell and the line under each statement draw, also where the group's heading is
   *  that question (R5). Absent where the words answer no question. */
  readonly question?: string;
  /** The one line the decision is quoted by under a statement, in plain words with each
   *  reference named after its cut (`referenceStandIns`): the first sentence of the words a
   *  person's answer opens with (`firstBlockWordsRaw`), or the options they chose where they
   *  wrote nothing, or the first line of a decision node's own words (`lineWordsRaw`), an
   *  agent's or a person's who decided it. */
  readonly quote: string;
  /** The node's own `dispatch://` address, which the link to the whole of it follows. */
  readonly reference?: string;
  readonly said: SpecSaid;
  /** How the decision stands with the reader, where the model gives it a state. */
  readonly state?: NodeState;
  /** What the node is: the ask whose answer this is, or the decision. */
  readonly type: NodeType;
  /** Whether the page marks this decision unsure (`DecisionNode.unsure`). */
  readonly unsure: boolean;
  /** A person's whole answer as the store holds it, where it holds anything other than what
   *  `quote` draws - more than its first sentence, a mark or a link's address `quote` reads as
   *  plain words, or a reference `quote` names in words: what the focus on the decision's line
   *  draws, character for character (R5). */
  readonly whole?: string;
  /** A decision node's own text whole, an agent's or a person's who decided it
   *  (`decidedByPerson`), as the cell draws it (`RowText`): as stored, the server's reading of it
   *  where one was sent, and the record it is. Absent for a person's answer. */
  readonly words?: {
    readonly markdown: string;
    readonly source?: TextSource;
    readonly tree?: DisplayTree;
  };
}

/** One statement of the Spec filter, as its row draws it. */
export interface SpecStatement {
  /** Every other decision that shaped it, beyond the one its run is headed by (R5). */
  readonly alsoShapedBy: readonly SpecDecision[];
  /** Whether its words changed after the tie that names the decision its run is headed by,
   *  which its line under the words says (R5, `changedSinceTie`): false where that decision is
   *  the answered question it hangs under, which has no tie, and where no decision is drawn. */
  readonly changedSince: boolean;
  /** How its checks stand (R17); the row of a criterion draws none, since no check serves what a
   *  person wants to be able to do (`SpecFilterBody.tsx`'s `Statement`). */
  readonly mark: SpecMark;
  /** The node as the filter's view left it, whose words the row draws. */
  readonly node: TreeNode;
}

/** One run of statements that one decision shaped, which the bracket in the left column ties
 *  together (R5: "the same decision stands in a column beside the statements it shaped"). */
export interface SpecRun {
  /** The decision that shaped every statement of the run; absent where none did (R35), and
   *  where the one that did is a person's whose answer the asks read has not brought yet. Such a
   *  run is that decision's all the same, and its cell says nothing, as every cell with no
   *  decision does until what decided each statement is held (`SpecFilterBody.tsx`'s
   *  `decidedHeld`). */
  readonly decision?: SpecDecision;
  /** The key the runs are split by: the head tie's `key`; absent where no decision heads it. */
  readonly key?: string;
  readonly statements: readonly SpecStatement[];
}

/** One of a statement's ties as the runs are built: the decision as the column draws it, the
 *  key the runs are split by, and whether the statement changed after the decision named it
 *  (`changedSinceTie`). A person's decision whose answer the asks read has not brought yet keeps
 *  its place by its reference, with no decision drawn for it until the read is in. The answered
 *  question a statement hangs under is a tie here with no `changed`, since it has no tie in the
 *  decision's links to read it from. */
interface Tie {
  readonly changed?: boolean;
  readonly decision?: SpecDecision;
  readonly key: string;
}

/** The node a group of statements hangs under, which the filter draws as no row of its own
 *  (R2): the question that was answered, or the comment they were written under. */
export interface SpecParent {
  readonly at?: string;
  readonly by?: Actor;
  readonly id: string;
  /** Its own words' first line, as R1 draws one (`lineWordsRaw`, `drawnLine`), with each
   *  reference named after the cut; empty where it has no words, which the view names by its
   *  kind's word. */
  readonly line: string;
  readonly type: NodeType;
}

/** The statements under one parent, in the order the filter draws them. */
export interface SpecGroup {
  /** The node they hang under; absent for the statements that hang under the issue itself. */
  readonly parent?: SpecParent;
  readonly runs: readonly SpecRun[];
}

/** What the Spec filter's body draws. */
export interface SpecReading {
  readonly groups: readonly SpecGroup[];
  /** Every row the filter kept that is no statement: a node an address named, which outranks
   *  the filter (`presets.ts`), drawn by its kind and its words with no mark and no decision
   *  beside it (R19, R20). */
  readonly others: readonly TreeNode[];
}

/**
 * The Spec filter's body, read from the tree, the decisions and the results.
 *
 * `view` is the Spec filter's own view of the tree (`presetView`), whose rows are the
 * statements in the order the page draws them; `root` is the whole tree, which holds the asks,
 * the decisions and each statement's checks, none of which the Spec filter draws as rows (R2).
 * `viewTree` is the tree the Spec view read builds (`TreeViews.trees.spec`), whose nodes `root`
 * holds none of are read beside `root`'s: Conversation's read, which `root` is built from, is
 * paged, and that tree holds each answered question whose ties name a statement the view holds.
 * `decisions` are the decisions of the Spec view read (`?view=spec`) as every reader of a
 * decision reads them (`decisionNodes`), in that answer's order. `nodeIdOf` is how this issue
 * addresses a node, which is the one rule the tree is built with (`tree-drafts.ts`'s
 * `treeContext`), so a tie resolves to the node it names however its address is written.
 * `lineNames` is what a reference a plain line cites is named by (`one-line.ts`'s
 * `useIssueNames`). `checks` is the issue's results read, which each statement's mark is read
 * from as the Verification filter reads it (`statement-state.ts`).
 */
export function specDecidedBy({
  asksHeld,
  checks,
  decisions,
  lineNames,
  nodeIdOf,
  reader,
  root,
  view,
  viewTree,
}: {
  /** Whether the asks read holds an answer. A person's decision is drawn by the answer they
   *  gave, whose question the tree holds once that read is in; until it is, the decision is
   *  drawn as nothing, rather than as an agent's record of it, which is how a person's decision
   *  whose question the tree does not hold is drawn. */
  readonly asksHeld: boolean;
  readonly checks: readonly CheckBlock[];
  readonly decisions: readonly DecisionNode[];
  readonly lineNames: LineNames;
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** Who is reading, so a decision of theirs reads as their own words (R34). */
  readonly reader: string | undefined;
  /** The whole tree. */
  readonly root: TreeNode;
  /** The tree as the Spec filter leaves it. */
  readonly view: TreeNode;
  /** The tree the Spec view read builds, where the page holds one. */
  readonly viewTree?: TreeNode;
}): SpecReading {
  // A row the view draws under a criterion carries that criterion as its parent's line, as a row
  // whose parent the view draws as no row carries its own (`withCriterionLines`), so a statement
  // written under a criterion stands in the group that criterion heads.
  view = withCriterionLines(view);
  const nodes = nodesById(root, viewTree);
  const asks = asksById(nodes);
  const shaped = shapedBy({ asks, asksHeld, decisions, lineNames, nodeIdOf, nodes, reader });
  const standings = statementStandings(root, checks);
  const groups: {
    parent?: SpecParent;
    runs: { decision?: SpecDecision; key?: string; statements: SpecStatement[] }[];
  }[] = [];
  // A criterion of the view is a row of the statements' list as well, in the view's order: what
  // a person wants to be able to do, drawn as a statement is (`SpecFilterBody.tsx`'s
  // `Statement`). So here a statement is every row `isListed` names.
  const isStatement = isListed;
  // Every row the Spec filter kept, in the order it draws them: the view's nodes but its root.
  // A statement is a requirement of the issue's own spec (`isStatement`).
  // Here `isStatement` is `isListed`, so a criterion is one of these rows too.
  const rows = nodesInOrder(view).slice(1);
  for (const node of rows.filter(isStatement)) {
    const parent = parentOf(node, nodes, lineNames);
    const under = answerUnder(parent, nodes, reader, lineNames);
    // The answered question the statement hangs under first, then each tie, each decision once.
    // The first heads the statement's run and every other is named after "informed by". A
    // person's decision that waits on the asks read keeps its place: where it is first it heads
    // the run, which draws nothing for it until the read is in, and no later decision moves up
    // in its place. The others that informed the statement are named after "informed by" all the
    // same, since what they say does not wait on that read; a waiting one among them is named
    // by nothing until it is in.
    const ties: readonly Tie[] = [
      ...(under === undefined ? [] : [tieOf(under)]),
      ...(shaped.get(node.id) ?? []).filter((tie) => tie.key !== under?.id),
    ];
    const head = ties.at(0);
    const standing = standings.get(node.id);
    const statement: SpecStatement = {
      alsoShapedBy: ties
        .slice(1)
        .flatMap((tie) => (tie.decision === undefined ? [] : [tie.decision])),
      // Only the tie of the decision the run is headed by, which the line under the statement
      // names; the answered question it hangs under carries no `changed`, having no tie.
      changedSince: head?.decision !== undefined && head.changed === true,
      mark:
        node.gaps.includes("no_check") || standing === undefined || !standing.covered
          ? "no_check"
          : standing.state,
      node,
    };
    const fresh = {
      ...(head?.decision === undefined ? {} : { decision: head.decision }),
      ...(head === undefined ? {} : { key: head.key }),
      statements: [statement],
    };
    const group = groups.at(-1);
    const run = group?.runs.at(-1);
    if (group === undefined || group.parent?.id !== parent?.id) {
      groups.push({ ...(parent === undefined ? {} : { parent }), runs: [fresh] });
    } else if (run === undefined || run.key !== head?.key) {
      group.runs.push(fresh);
    } else {
      run.statements.push(statement);
    }
  }
  return { groups, others: rows.filter((row) => !isStatement(row)) };
}

/**
 * The first sentence of `text`, word for word: everything up to where `firstSentenceLength` says
 * its first sentence ends, and the whole text where it holds no end, with no limit of characters
 * (R5). Line breaks inside it are spaces, because the line it is drawn on is one line.
 *
 * It is not `firstLineOf`, which cuts a row's first line at 100 characters (R1): a person's
 * answer is quoted word for word, and a sentence of theirs is as long as they wrote it.
 */
export function firstSentenceOf(text: string): string {
  const whole = text.replace(/\s+/gu, " ").trim();
  const length = firstSentenceLength(whole);
  return length === undefined ? whole : whole.slice(0, length);
}

/** Whether `node` is a row of the Spec filter's statements' list: a statement of the spec
 *  (`isStatement`), or a criterion, what a person wants to be able to do, which the list draws
 *  as it draws a statement (R1). */
function isListed(node: TreeNode): boolean {
  return isStatement(node) || node.type === "criterion";
}

/** Every node of the tree by its id, at its first placement, which is where a node drawn under
 *  more than one parent is edited (`TreeNode.transcluded`), and after them each node of
 *  `viewTree` the tree holds none of. It is not `decisions-filter/decisions-parts.ts`'s
 *  `placements`, which reads each node's parent line as well (`issue-tree.ts`'s
 *  `parentLineOf`): the Spec view reads a parent's line by its own rule (`parentOf`), so this
 *  walks the nodes alone. */
function nodesById(root: TreeNode, viewTree?: TreeNode): ReadonlyMap<string, TreeNode> {
  const nodes = new Map<string, TreeNode>();
  const walk = (node: TreeNode): void => {
    if (!nodes.has(node.id)) nodes.set(node.id, node);
    for (const child of node.children) walk(child);
  };
  walk(root);
  if (viewTree !== undefined) walk(viewTree);
  return nodes;
}

/** Each ask of the tree by the id a decision names it with (`Decision.asks`), which is the ask's
 *  own id and not the id of the row it is drawn as: an ask the server indexed from a block of a
 *  document is drawn as that block (`tree-read/tree-nodes.ts`' `draftAsk`). */
function asksById(nodes: ReadonlyMap<string, TreeNode>): ReadonlyMap<string, TreeNode> {
  const asks = new Map<string, TreeNode>();
  for (const node of nodes.values()) {
    const id = askIdOf(node);
    if (id !== undefined) asks.set(id, node);
  }
  return asks;
}

/** The decisions that shaped each statement, by the statement's node id: a person's first, then
 *  an agent's, each in the order the Spec view read answers them. So a person's decision heads
 *  every statement it shaped, and an agent's that also shaped one is named after it, whichever
 *  the read answers first: the read places an answered ask as context after the decision blocks
 *  of the spec. Each is drawn, but a person's while the asks read holds no answer
 *  (`specDecidedBy`'s `asksHeld`), which keeps its place as a tie with no decision drawn. Each
 *  carries whether the statement changed after the decision named it (`changedSinceTie`). */
function shapedBy({
  asks,
  asksHeld,
  decisions,
  lineNames,
  nodeIdOf,
  nodes,
  reader,
}: {
  asks: ReadonlyMap<string, TreeNode>;
  asksHeld: boolean;
  decisions: readonly DecisionNode[];
  lineNames: LineNames;
  nodeIdOf: (reference: string) => string | undefined;
  nodes: ReadonlyMap<string, TreeNode>;
  reader: string | undefined;
}): ReadonlyMap<string, readonly Tie[]> {
  const shaped = new Map<string, Tie[]>();
  // A decision node a person decided is that person's decision (`decidedByPerson`), so it heads
  // the run as a person's answer does.
  const people: DecisionNode[] = [];
  const agents: DecisionNode[] = [];
  for (const decision of decisions) {
    (decision.kind === "person" || decidedByPerson(decision) ? people : agents).push(decision);
  }
  for (const decision of [...people, ...agents]) {
    // A replaced decision draws no line (R5): it is drawn where it sits in the outline, and a
    // statement it shaped is no longer standing on it.
    if (decision.replaced) continue;
    const links = decision.links ?? [];
    if (links.length === 0) continue;
    const made: Tie =
      decision.kind === "person" && !asksHeld
        ? { key: decision.ref }
        : tieOf(decisionOf(decision, { asks, lineNames, nodeIdOf, nodes, reader }));
    for (const link of links) {
      const id = link.to.ref === undefined ? undefined : nodeIdOf(link.to.ref);
      if (id === undefined) continue;
      const held = shaped.get(id) ?? [];
      const changed = changedSinceTie(link);
      const at = held.findIndex((one) => one.key === made.key);
      const same = held[at];
      // A decision that names one statement in two of its blocks is one decision of it, which
      // the statement changed since only where it changed after each of those ties.
      if (same === undefined) held.push({ ...made, changed });
      else held[at] = { ...same, changed: same.changed === true && changed };
      shaped.set(id, held);
    }
  }
  return shaped;
}

/** Whether the statement `link` names changed after the link was made (R5): its far end's
 *  `changed_at` is later than the link's own `created_at`, both as the decision's links answer
 *  them (`Decision.links`) and compared as the instants they name (`compareTimestamps`), never
 *  as text. A time that cannot be read, or none, is no change, since an order built on it would
 *  be a guess. */
function changedSinceTie(link: DecisionLink): boolean {
  const changed = link.to.changed_at;
  return (
    changed !== null &&
    isTimestamp(changed) &&
    isTimestamp(link.created_at) &&
    compareTimestamps(changed, link.created_at) > 0
  );
}

/** `decision` as a tie, keyed by the node that holds the whole of it. */
function tieOf(decision: SpecDecision): Tie {
  return { decision, key: decision.id };
}

/** One decision as the column draws it: a person's by the answer they gave, an agent's by its
 *  own words. */
function decisionOf(
  decision: DecisionNode,
  {
    asks,
    lineNames,
    nodeIdOf,
    nodes,
    reader,
  }: {
    asks: ReadonlyMap<string, TreeNode>;
    lineNames: LineNames;
    nodeIdOf: (reference: string) => string | undefined;
    nodes: ReadonlyMap<string, TreeNode>;
    reader: string | undefined;
  }
): SpecDecision {
  const node = nodes.get(nodeIdOf(decision.ref) ?? "");
  // A person's decision is their answer to a question, so the words the column quotes are the
  // answer's own and never the decision's restatement of them (R4): a person's decision that
  // names an answered ask of this issue is drawn by the first of its asks the tree holds an
  // answer for.
  if (decision.kind === "person") {
    const ask = (decision.asks ?? [])
      .map((id) => asks.get(id))
      .find((one) => one?.answer !== undefined);
    if (ask?.answer !== undefined) {
      return {
        ...answerDecision(ask, ask.answer, reader, lineNames),
        ...(node === undefined ? {} : { node: node.id }),
      };
    }
  }
  // Every other decision is drawn by its own words: a decision node's, an agent's or a person's
  // who decided it (`decidedByPerson`), or an agent's record of a person's decision whose
  // answered question this tree does not hold - one the tree places elsewhere, or one whose read
  // failed. That record is never put in the person's mouth (R9: "no agent's words are drawn
  // under a person's name"), and a decision node a person decided is in that person's own words,
  // who decided it: the author the tree's node names, and that person where the tree holds no
  // node of it yet.
  const person = decision.kind !== "person" && decidedByPerson(decision);
  const said: SpecSaid = !person
    ? "agent"
    : isReader(decision.decidedBy, reader)
      ? "you"
      : "person";
  const by = node?.author ?? (person ? decision.decidedBy : undefined);
  const at = node?.at ?? decision.changed_at ?? undefined;
  const tree = decision.text_tree ?? node?.tree;
  // The one line is cut from the words `lineWordsRaw` reads, as every one-line place cuts a
  // node's, with none of the symbols the cell draws as formatting. A decision the tree holds no
  // node for is read from its own text and its reading in the same way.
  const words = lineWordsRaw(node ?? { text: decision.text, tree: decision.text_tree });
  // The quote and the name a line gives it among others are cut from the one stood-in line, each
  // before its references are named, so no cut falls inside a word of a name.
  const references = referenceStandIns(lineNames);
  const line = drawnLine(firstLineOf(references.standIn(words)));
  return {
    ...(at === undefined ? {} : { at }),
    ...(by === undefined ? {} : { by }),
    ...(node?.changedBy === undefined ? {} : { changedBy: node.changedBy }),
    chip: references.named(chipWords(line)),
    chose: [],
    id: node?.id ?? decision.ref,
    ...(node?.keptBy === undefined ? {} : { keptBy: node.keptBy }),
    ...(node === undefined ? {} : { node: node.id }),
    quote: references.named(line),
    reference: decision.ref,
    said,
    ...(node?.state === undefined ? {} : { state: node.state }),
    type: "decision",
    unsure: decision.unsure,
    words: {
      markdown: decision.text,
      ...(node?.source === undefined ? {} : { source: node.source }),
      ...(tree === undefined ? {} : { tree }),
    },
  };
}

/** A person's answer to `ask` as the column draws it: the first sentence of the words it opens
 *  with (`firstBlockWordsRaw`) with each reference named, or the options they chose where they
 *  wrote none (R5), with the whole answer as stored wherever it is not what the quote draws, and
 *  the question's own first sentence whole, cut by the same rule (`namedFirstSentence`). */
function answerDecision(
  ask: TreeNode,
  answer: NonNullable<TreeNode["answer"]>,
  reader: string | undefined,
  lineNames: LineNames
): SpecDecision {
  // The quote reads the words the answer opens with (`firstBlockWordsRaw`), as the Contents entry
  // reads a comment's. A link the person wrote reads as its words and a mark as nothing, so the
  // quote is their words and none of the markdown they were stored in. Each reference is named
  // after the sentence is cut, so a name that opens with a label ("R2.") ends none of it. The
  // answer holds its stored text's reading, so a redraw reads it again from memory rather than
  // from a second parse.
  const references = referenceStandIns(lineNames);
  const words = firstBlockWordsRaw(storedReading(answer.text, answer));
  const written = firstSentenceOf(references.standIn(words));
  // An option's label is a plain line too, so each reference it cites is named as the quote
  // names one, wherever the options are drawn: the quote, the cell's line of what was chosen,
  // each line naming the decision. The quote and the name a line gives it among others are cut
  // from the one stood-in line, each before its references are named.
  const labels = answer.selected.map((label) => references.standIn(label));
  const line = written === "" ? labels.join(", ") : written;
  const quote = references.named(line);
  const stored = answer.text.trim();
  return {
    at: answer.at,
    by: answer.by,
    chip: references.named(chipWords(line)),
    chose: labels.map((label) => references.named(label)),
    id: ask.id,
    question: namedFirstSentence(lineWordsRaw(ask), lineNames),
    quote,
    ...(ask.reference === undefined ? {} : { reference: ask.reference }),
    said: isReader(answer.by, reader) ? "you" : "person",
    type: "ask",
    unsure: false,
    // A quote read to plain words, or naming a reference in words, is not the characters the
    // person wrote, so the focus draws those too, though the answer is one sentence.
    ...(stored === "" || stored === quote ? {} : { whole: answer.text }),
  };
}

/** The question a statement hangs under, where that question was answered: its answer is the
 *  decision that shaped the statement, with no tuple (R5). */
function answerUnder(
  parent: SpecParent | undefined,
  nodes: ReadonlyMap<string, TreeNode>,
  reader: string | undefined,
  lineNames: LineNames
): SpecDecision | undefined {
  if (parent === undefined) return undefined;
  const node = nodes.get(parent.id);
  if (node === undefined || node.type !== "ask" || node.answer === undefined) return undefined;
  return answerDecision(node, node.answer, reader, lineNames);
}

/** The node a statement hangs under, where the filter draws it as no row (R2), with who made it
 *  and when, and its own words' first line, which its heading draws by R1's rule. */
function parentOf(
  node: TreeNode,
  nodes: ReadonlyMap<string, TreeNode>,
  lineNames: LineNames
): SpecParent | undefined {
  const line = node.parentLine;
  if (line === undefined) return undefined;
  const parent = nodes.get(line.id);
  // Read from the node and not from `line`, which the row projects as every one line of a node
  // is drawn (`issue-tree.ts`'s `lineReading`): the heading names each reference as a row of the
  // outline names it, and counts it against the line's limit as the name it draws.
  const words = parent === undefined ? "" : namedFirstLine(lineWordsRaw(parent), lineNames);
  return {
    ...(parent?.at === undefined ? {} : { at: parent.at }),
    ...(parent?.author === undefined ? {} : { by: parent.author }),
    id: line.id,
    line: words,
    type: line.type,
  };
}

/** `words`' first line as a one-line place draws it (R1: `firstLineOf`, `drawnLine`), cut with
 *  each reference it cites unnamed and each named after the cut (`referenceStandIns`). */
function namedFirstLine(words: string, lineNames: LineNames): string {
  const references = referenceStandIns(lineNames);
  return references.named(drawnLine(firstLineOf(references.standIn(words))));
}

/** `words`' first sentence whole, as a person's quote is cut (`firstSentenceOf`), with no limit
 *  of characters, cut with each reference it cites unnamed and each named after the cut
 *  (`referenceStandIns`), so a name that opens with a label ("R2.") ends none of it: how the cell
 *  and the line under each statement give the question an answer answers (R5). */
function namedFirstSentence(words: string, lineNames: LineNames): string {
  const references = referenceStandIns(lineNames);
  return references.named(firstSentenceOf(references.standIn(words)));
}
