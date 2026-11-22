/**
 * Every word the issue page says of its own accord, and every shape it draws beside one
 * (INFRA-1196 R21, `dispatch://INFRA-1196/spec#b-r-plain-words`). Nothing the page draws names
 * itself in an internal word: a type name, a state word, a preset name, a control label, an
 * empty-state line and an attribute's own `when` value all come from here, so a word a reader
 * sees is a word somebody chose rather than a schema token, a slug or an enumeration case.
 *
 * The page divides its own words from the author's with two attributes:
 *
 * - every element whose visible text is the page's own vocabulary carries
 *   `PAGE_CHROME_ATTRIBUTE`;
 * - every element whose visible text came from a person or an agent carries
 *   `AUTHOR_TEXT_ATTRIBUTE`: a node's own text, an issue or child title, a check's `run`
 *   command, a label value, an author's name.
 *
 * A reader of the page's words therefore takes each `[data-page-chrome]` element's text with
 * every `[data-author-text]` descendant's text removed, and every word that survives is in
 * `DECLARED_WORDS`.
 *
 * A shape is an inline `<svg>` carrying its `GLYPH_TOKENS` token in `data-glyph` and its own
 * accessible name from `GLYPH_NAMES`, drawn beside the word rather than in place of it: a state
 * and a type are told apart by a word and a shape, never by colour alone.
 *
 * This module is data and pure functions. It holds no React and touches no DOM, and it imports
 * data and types, with four helpers:
 *
 * - from its sibling `page-structure.ts`, the property and section names and help lines
 *   `DECLARED_WORDS` is derived from as well, and `freeze`, which its tables are made with;
 * - from `@legion/contracts`, the most issues one issue may wait on, the most priorities it may
 *   advance, and the types of how a result's commit stands, of when a check runs and of how a
 *   delivery stands;
 * - from `../../api/types`, the type of an actor;
 * - from `../project/board-model`, the issue statuses and `statusLabel` and `statusText`, the
 *   board's word for a status, which `ISSUE_STATUS_WORDS` and `issueStatusWord` say;
 * - from `../refs/actor`, `ASK_RESOLUTION_WORDS`, the words a closed question is drawn in, and
 *   `isViewer`, whether an actor is the signed-in person.
 *
 * The last two are the module's links to files today's pages use.
 *
 * A test, a browser spec and a component therefore all read the same list.
 */

import {
  type CheckWhen,
  type DeliveryAttemptState,
  MAX_ISSUE_ADVANCES,
  MAX_ISSUE_BLOCKERS,
  type ResultCommitStands,
} from "@legion/contracts";

import type { Actor } from "../../api/types";
import { issueStatuses, statusLabel, statusText } from "../project/board-model";
import { ASK_RESOLUTION_WORDS, isViewer } from "../refs/actor";
import {
  freeze,
  HELP_LINES,
  KEY_REPORTS,
  MARGIN_SECTION_NAMES,
  PROPERTY_LABELS,
} from "./page-structure";

/** The keys of `value`, as its own key type rather than as plain strings. */
function keysOf<T extends object>(value: T): readonly (keyof T)[] {
  return Object.keys(value) as (keyof T)[];
}

/**
 * A plain name for every kind of node the page draws, which is every kind the model holds: the
 * issue, the documents and images it carries, the five typed blocks of those documents, the
 * decisions and asks, the conversation, the results recorded against a check, and the child
 * issues.
 *
 * A key is the model's own token for the kind, so the page needs no second table to read a
 * node: `callout`, `ask`, `requirement`, `plan_task` and `check` are the block type names in
 * `packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`, and `ask`, `comment` and
 * `message` are the item kinds of a `dispatch://` reference. A name is lowercase because the
 * page draws it inside its own sentences and rows, where a capital would read as a proper noun;
 * `sentenceCase` supplies the capital where a name opens a sentence or a heading.
 */
export const NODE_TYPE_NAMES = freeze({
  /** An `ask`: the word the approved pictures draw on its tag, "question" (R34). */
  ask: "question",
  /** A `callout` block: a note somebody set apart from the text around it. The page draws the
   *  word a reader uses, never the schema's own token (R21). */
  callout: "note",
  check: "check",
  child_issue: "child issue",
  comment: "comment",
  decision: "decision",
  document: "document",
  handoff: "handoff",
  image: "image",
  issue: "issue",
  message: "message",
  plan_task: "plan task",
  /** A `requirement` block, drawn by his word for it (R22, the Design's "The words"): a
   *  sentence that must be true of the software. */
  requirement: "statement",
  result: "result",
} as const);

/** One kind of node the page draws, by the model's own token for it. */
export type NodeType = keyof typeof NODE_TYPE_NAMES;

/**
 * The name a typed block draws in a document that does not play its type's part, in place of
 * its type's name, lowercase as every name above: a `requirement` block of any document but the
 * issue's spec is no statement (`isStatement`), so its row's tag says its type's own word and
 * never "statement" (decision 246), as a `check` block outside the Verification document draws
 * no state word (decision 152).
 */
export const BLOCK_TYPE_NAMES = freeze({
  requirement: "requirement",
} as const);

/**
 * The state words of each type whose state the model holds, and no entry at all for a type it
 * gives no state: a comment is not in a state, so the page has no word to draw for one, and an
 * entry holding an empty table would invite a row to draw a blank badge.
 *
 * The ask's three states are the `state` choices of the `ask` block (`blocks.json`). The
 * check's seven are `checkState`'s (`tree-drafts.ts`), and the page
 * must tell all seven apart, so each keeps a word of its own.
 *
 * A decision's states are spec R2's, "wherever a decision is drawn": `waiting_review` is an
 * agent's decision that waits for a person's review, `kept` one a person kept as it reads now -
 * the row draws the name of who kept it after the word - and `replaced` one a later decision
 * replaced (`decisions.go`'s `replaced`, `waiting_review` and `kept`, read by `tree-drafts.ts`'s
 * `decisionState`). `change_requested` is a decision the reader sent a Change on since it last
 * changed, which waits on its agent and no longer on the reader (`decision-review.ts`). A
 * decision in none of them - an agent's that nobody has reviewed and nothing replaced, or a
 * person's own section - draws no state.
 */
export const NODE_STATE_WORDS = freeze({
  ask: {
    answered: "Answered",
    open: "Open",
    // How an ask was closed rather than answered, in the words of `ASK_RESOLUTION_WORDS`: that
    // table repeats the words `askResolutionBadge` (`features/refs/actor.ts`) draws on the card,
    // and the test `each way a question is closed has one word on its card and in the list`
    // holds the two equal. An ask whose `state` is `resolved` stands in the state its resolution
    // names, so a declined question reads "Declined" on its row and "Declined" on its card.
    // `resolved` is both the block state the schema declares and the kind of a plain
    // resolution, under the one word.
    ...ASK_RESOLUTION_WORDS,
  },
  check: {
    failed: "Failed",
    "no-result": "No result",
    /** The newest result that stands does not stand at the commit the server names for the
     *  check: it was recorded against another commit, or against an older wording of the
     *  check, so it no longer speaks for the work as it stands, whatever it found. The check's
     *  row and its result's fact read that one standing, the server's for the check, so the
     *  result says the same of itself in the same words (`RESULT_STANDS_WORDS`); nothing else
     *  on the page draws them: a decision's chip for a block it changed after naming says so
     *  in words of its own (`CONTROL_LABELS.target_stale`). */
    older: "Out of date",
    passed: "Passed",
    /** Every result recorded against the check has been withdrawn (`CheckResultRetracted`),
     *  so nothing stands for it or against it. A check whose pass was taken back is not a
     *  check that failed, and the word says which of the two a reader is looking at. The ask
     *  that was retracted reads the same word for the same reason, and `STATE_WORDS` holds
     *  the two to it. */
    retracted: "Retracted",
    /** There is a result, and Dispatch cannot tell whether a pass of the check stands - it
     *  could not read the repository, or could not place the commit a pass was recorded at -
     *  so the page will not say that it passed. The word says Dispatch cannot tell, which "No
     *  result" does not: there is a result to tell about. */
    unmarked: "Cannot tell",
    /** The payload carries no answer over the check's results at all, which is a server that
     *  does not send `pass_stands` - an older build behind the same load balancer as this
     *  one. The page says what is true of it, which is that it could not read how this check
     *  stands: it is nothing the check did, so none of the six words above may be drawn for
     *  it, and a page that guessed would be stating a verdict nobody recorded. */
    unreadable: "Could not read",
  },
  decision: {
    change_requested: "Change requested",
    kept: "Kept by",
    replaced: "Replaced",
    waiting_review: "Waiting for review",
  },
} as const);

/** How an ask stands, by the `state` the server writes into its block. */
export type AskState = keyof typeof NODE_STATE_WORDS.ask;

/** How a check stands, over every result recorded against it (`tree-drafts.ts`'s `checkState`). */
export type CheckState = keyof typeof NODE_STATE_WORDS.check;

/** How a decision stands with the reader, from the decisions read. */
export type DecisionState = keyof typeof NODE_STATE_WORDS.decision;

/** Every node kind, in one list, for a caller that walks them all. */
export const NODE_TYPES: readonly NodeType[] = freeze(keysOf(NODE_TYPE_NAMES));

/**
 * The state words of `type`, or `undefined` when the model gives that type no state. The page
 * draws no badge for an answer of `undefined`: it is the difference between a node whose state
 * nobody recorded and a node that has no state to record.
 */
export function nodeStateWords(type: NodeType): Readonly<Record<string, string>> | undefined {
  const byType: Partial<Record<NodeType, Readonly<Record<string, string>>>> = NODE_STATE_WORDS;
  return byType[type];
}

/**
 * How a node of a type the model gives a state stands: one of the tokens `NODE_STATE_WORDS`
 * declares a word for. A node's own `state` is this and never a plain string
 * (`issue-tree.ts`'s `TreeNode`), so a state the page has no word for cannot reach a row and
 * no row draws a schema token where a word belongs (R21): a state the model adds is a compile
 * error here until it is named.
 */
export type NodeState = AskState | CheckState | DecisionState;

/** The word for every state, in one table. A token two types share draws the one word -
 *  `retracted` is a question its asker took back and a check whose results were all withdrawn,
 *  and both read "Retracted" - which `page-words.test.ts` holds this to, so one table answers
 *  for all three types. */
const STATE_WORDS: Readonly<Record<NodeState, string>> = freeze({
  ...NODE_STATE_WORDS.ask,
  ...NODE_STATE_WORDS.check,
  ...NODE_STATE_WORDS.decision,
});

/** The page's word for `state`. Total: every state a node can be in has one. */
export function nodeStateWord(state: NodeState): string {
  return STATE_WORDS[state];
}

/**
 * The name of the node kind `token`, which is `NODE_TYPE_NAMES`' word for a kind the module
 * declares. A kind it does not declare reads as its token with each underscore as a space:
 * a person can read that, and it is the fallback rather than the answer, because
 * `page-words.test.ts` reads the block schema and fails until the kind has a word here.
 */
export function nodeTypeName(token: string): string {
  const names: Readonly<Record<string, string>> = NODE_TYPE_NAMES;
  return names[token] ?? token.replaceAll("_", " ");
}

/** The name at the start of a sentence or a heading: its first letter capitalised, nothing else
 *  touched, so "child issue" opens a heading as "Child issue". */
export function sentenceCase(name: string): string {
  return name === "" ? name : `${name[0]?.toUpperCase() ?? ""}${name.slice(1)}`;
}

/** The five controls of the filter row in the order the row draws them (R2): Conversation,
 *  which is the page with no filter on, then the four filters in the order of his answer of
 *  2026-11-11 20:57Z, "the spec, the verification, the plan, the decisions"
 *  (dispatch://INFRA-1098/ask/b1fe0ff5-3b4a-4f6d-9b2a-e1f1e146779b). The page opens on
 *  Conversation. */
export const PRESET_ORDER = freeze([
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
] as const);

/** One control of the filter row, by its own token: `conversation`, which turns every filter
 *  off, or one of the four filters. Each has an address of its own (`issue-address.ts`). */
export type Preset = (typeof PRESET_ORDER)[number];

/** The name of each control of the filter row. Verification is his word for the filter that
 *  draws the checks (2026-11-11 21:57Z), and nothing is called Tree (R2). */
export const PRESET_NAMES = freeze({
  conversation: "Conversation",
  decisions: "Decisions",
  plan: "Plan",
  spec: "Spec",
  verification: "Verification",
} as const satisfies Record<Preset, string>);

/**
 * The words a control of the filter row draws after its count where its nodes are in a state
 * that asks something of the reader, in the prototype's style ("Verification 2 failing",
 * "Decisions 6 to review"), and the noun the Plan filter counts its tasks in ("Plan 13
 * steps", R26's "ordered steps"). The count itself is a number and no word.
 */
export const FILTER_SIGNAL_WORDS = freeze({
  /** After the number of checks whose state is failed at the commit the server names for each
   *  (`checkStanding`), the state their rows draw. */
  failing: "failing",
  /** After the number of plan tasks. */
  steps: "steps",
  /** After the number of decisions waiting for the reader's review (R35). */
  to_review: "to review",
} as const);

/**
 * What each mark on an entry of the Contents pane counts in the branch under it, the
 * prototype's `.cp` marks ("R2 ◆1 ◫1"): the statements, the decisions waiting for the reader's
 * review, the child issues, and the statements a check of theirs failed. Each is drawn as its
 * kind's shape and its count, and named by these words (R6, R22).
 */
export const CONTENTS_MARK_WORDS = freeze({
  children: "child issues",
  failing: "failing statements",
  for_review: "decisions to review",
  statements: "statements",
} as const);

/** One mark of a Contents entry, by its own token, which is its `data-contents-mark`. */
export type ContentsMark = keyof typeof CONTENTS_MARK_WORDS;

/**
 * The words the Spec filter's body draws of its own (dispatch://INFRA-1196 R5, R17, R22, R35,
 * `spec-filter/SpecFilterBody.tsx`). Nothing here says anything about the page or about
 * Dispatch. The mark at the right of a statement is how it stands at the commit the page
 * names, in the Verification filter's own words (`VERIFICATION_WORDS.states`: Met, Not met,
 * Unknown), so one statement reads the same in both views; a statement no check serves
 * carries `no_check` in their place. What the Decided by column says beside statements no
 * decision shaped is an empty line (`EMPTY_STATE_LINES.spec_no_decision_one`,
 * `spec_no_decision_many`).
 */
export const SPEC_WORDS = freeze({
  /** Opens the line under a statement that names the decision that shaped it. */
  because: "because:",
  /** On a decision's line, where the statement's own words changed after the decision was
   *  made (R5). */
  changed_since: "Changed since",
  /** Before the options a person chose, where the ask offered them. */
  chose: "chose",
  /** The left column's heading: what decided the statements beside it. */
  decided_by: "Decided by",
  /** Before the other decisions that shaped a statement, beyond the one beside it (R5). */
  informed_by: "informed by",
  /** The mark of a statement no check serves, which the legend names (R10, R17). */
  no_check: "no check",
  /** The heading over an agent's decision. */
  said_agent: "Because an agent decided",
  /** The heading over the words of a person who is not the reader. */
  said_person: "Because a person said",
  /** The heading over the reader's own words, as the picture draws it. */
  said_you: "Because you said",
  /** Opens the whole of a person's answer, where the column draws its first sentence. */
  whole_answer: "whole answer",
  /** Opens the whole decision. */
  whole_decision: "whole decision",
} as const);

/**
 * The words the Decisions filter's body draws of its own (dispatch://INFRA-1196 R2, R22, R35,
 * `decisions-filter/`). None is a sentence about the page or about Dispatch, and no sentence
 * explains the four parts: their headings are the whole of it. How a decision stands is
 * `NODE_STATE_WORDS.decision`, which the outline draws too.
 */
export const DECISIONS_WORDS = freeze({
  /** The fourth part's heading, which is the control that unfolds it, after the count of the
   *  decisions it holds; the words agree with the count (R2: "folded at the foot, the agents'
   *  decisions that no statement rests on"). */
  agents_many: "agent decisions that no statement rests on",
  agents_one: "agent decision that no statement rests on",
  /** Before the reader's words of a Change they sent on a decision, under that decision. */
  change: "change asked for",
  /** After the name of the person who declined a question, so the row reads as a question that
   *  person declined and never as an answer (R2). */
  declined: "declined to answer",
  /** The link from an entry to the node that holds it, in the conversation. */
  in_conversation: "in the conversation",
  /** Before the spec's statements that an entry's links tie it to, as the picture draws it. */
  informs: "informs",
  /** The headings of the headed parts, in the order the filter draws them (R2): the reader's
   *  own decisions are "his own decisions", and what another person answered, declined, kept or
   *  asked to change stands under a heading that says it is theirs. */
  parts: {
    open_questions: "Open questions",
    others: "Decided by others",
    review: "Waiting for your review",
    yours: "Your decisions",
  },
} as const);

/**
 * The words the Verification filter's body draws of its own (dispatch://INFRA-1196 R17, R22,
 * `verification-filter/`). `states` is how a statement stands at the commit the page names,
 * which is the page's own reading of the checks that serve it and no state the model holds: a
 * `requirement` block has no state, so these are not `NODE_STATE_WORDS.requirement` and never
 * become one. No count, legend or heading reads proven, or met for the coverage count.
 */
export const VERIFICATION_WORDS = freeze({
  /** The chip that narrows to nothing, which is the body as it opens. */
  all: "All",
  /** The heading over the checks that serve each statement. */
  checks: "Checks",
  /** Before the one commit the issue's results are read against. */
  commit: "Commit",
  /** The count of the statements that carry a check: "6 of 13 covered" (R17, R22). */
  covered: "covered",
  /** After the count of a check's other results, which the line opens in place (ruling 6). */
  earlier_many: "earlier results",
  earlier_one: "earlier result",
  /** After the count of the checks the coverage read counts for a statement beyond those the
   *  view draws with it, which serve it from an issue under this one. */
  elsewhere_many: "more checks elsewhere",
  elsewhere_one: "more check elsewhere",
  /** The heading over each check's latest result. */
  latest_result: "Latest result",
  /** Between the two numbers of the covered count. */
  of: "of",
  /** How a statement stands at the commit the page names (R17): every check of it passed
   *  there, one failed there, or neither is known. */
  states: {
    met: "Met",
    not_met: "Not met",
    unknown: "Unknown",
  },
  /** Beside the result a check's state stands on: the newest pass that stands for a check that
   *  passed, and the newest result that stands for every other. */
  stands: "Counts for this check",
} as const);

/** How a statement stands at the commit the page names (`verification-filter/statement-state.ts`). */
export type StatementState = keyof typeof VERIFICATION_WORDS.states;

/**
 * The words the Plan filter's body draws of its own (dispatch://INFRA-1196 R26, ruling 5 of
 * build-rulings-md, `plan-filter/`): the two ways it draws the plan, which its one control
 * switches between, the chart's two headings, the line for tasks that need each other, and the
 * mark of a bar whose task holds no size.
 */
export const PLAN_WORDS = freeze({
  /** The chart of time: one bar per task, after every task it needs (R26). */
  chart: "Chart",
  /** The line that lists tasks which need each other, so no order can put one after the rest. */
  cycle: "Steps that need each other",
  /** Before the tasks a step needs. */
  needs: "needs",
  /** A bar whose task holds no size, drawn one unit long (R26). */
  no_size: "No size",
  /** The heading over the statements each step serves. */
  serves: "Serves",
  /** The heading over each step's words, and the control that draws the plan as ordered
   *  steps. */
  steps: "Steps",
} as const);

/**
 * The words of the list of what references the row the reader is on, in Details
 * (dispatch://INFRA-1196 R14, R19, `node-references/NodeReferences.tsx`). The edge and kind
 * words are lowercase, as `NODE_TYPE_NAMES` are, because a row draws them beside the author's
 * own words. What the list says while its read is out and when it failed is the page's answer to
 * the reader opening it (`ANSWER_LINES.references_loading`, `references_unreadable`).
 */
export const NODE_REFERENCES_WORDS = freeze({
  /** The relation each kind of tuple puts the citing node in with the row the reader is on, in
   *  the words the old page's Referenced-by panel drew for the same edges
   *  (`features/refs/ReferencedBy.tsx`'s `STRUCTURAL_LABELS`). */
  edges: {
    anchored_to: "anchored item",
    answers: "answer",
    attached_to: "attached document",
    blocked_by: "blocked issue",
    child_of: "child issue",
    followed_by: "follower",
    needs: "needed by",
    owned_by: "owned item",
    replies_to: "reply",
    serves: "served by",
  },
  /** The page's word for each kind of node the graph answers with, which names a row whose
   *  edge is a mention. `artifact` and `block` are both a document to a reader, and a session is
   *  the conversation of one agent (R22: no schema token where a word belongs). */
  kinds: {
    artifact: "document",
    ask: "question",
    block: "document",
    comment: "comment",
    component: "component",
    issue: "issue",
    message: "message",
    session: "agent",
  },
  /** The control in Details that lists what references the row the reader is on (R14, R19).
   *  It names the row, because Details' References section holds the issue's own Referenced by
   *  beside it and two lists cannot share one name. */
  what_references: "What references this row",
} as const);

/**
 * How an issue stands, as the page says it: the word a child's row draws beside it (R10). The
 * keys are the server's own status tokens and the words the board's own labels, both read from
 * `features/project/board-model.ts` in the lifecycle order it holds them in, so a status added
 * there has its word here too.
 */
export const ISSUE_STATUS_WORDS: Readonly<Record<string, string>> = freeze(
  Object.fromEntries(issueStatuses.map((status) => [status, statusLabel(status)]))
);

/** The word for the issue status `token`, the board's own (`statusText`): a token the board does
 *  not know reads as itself, which is what the server sent. */
export function issueStatusWord(token: string): string {
  return statusText(token);
}

/**
 * Every line the page draws where a place holds nothing, one per situation: a preset whose
 * query matches nothing, and a spec with no summary or none the head can draw whole. Each is
 * one sentence, in the simple present, saying what is absent rather than what the reader should
 * do about it.
 */
export const EMPTY_STATE_LINES = freeze({
  /** The page with no filter on, on an issue whose tree holds nothing under it. */
  conversation: "This issue holds nothing yet.",
  decisions: "This issue has no decisions yet.",
  /** What the node menu says where its filter leaves none of the issue's files (R3). */
  files_filtered: "No file or document matches this filter.",
  /** What a view says where the filter by words (`?q=`) leaves nothing of what it draws: the
   *  issue holds such nodes, and none of them holds those words. */
  no_match: "Nothing here holds these words.",
  plan: "This issue has no plan tasks yet.",
  spec: "This issue has no statements yet.",
  /** What the Spec filter's Decided by column says beside a run of statements no decision
   *  shaped (R35): one statement, and several. */
  spec_no_decision_many: "No decision shaped these statements.",
  spec_no_decision_one: "No decision shaped this statement.",
  spec_summary: "This spec has no summary.",
  /** A summary whose stored excerpt stops inside its first sentence (`BlockExcerptRunes`, 480
   *  runes): the spec has a summary, and the head has no whole sentence of it to draw. */
  spec_summary_cut: "The summary is too long to show here.",
  /** A summary whose whole text ends in no sentence end, so it holds no sentence the head
   *  draws. */
  spec_summary_unended: "The summary has no complete sentence.",
  verification: "This issue has no checks yet.",
  /** What the Verification filter's table says where the state its chip narrows to holds no
   *  statement. */
  verification_none: "No statement stands in this state.",
} as const);

/**
 * What the page says of a gap in the reasoning, one line per kind of gap (R4, R10). A gap is
 * drawn on the node it is about and never as an error, and only where it tells that node apart
 * from its siblings and names work somebody owes: a requirement no check serves, and a check or
 * a plan task whose `serves` names no requirement. A gap every node of its kind has under
 * today's model tells the reader nothing about the node, so the page has no line for it - none
 * for a node that names no origin, since no requirement carries a `from` yet and no decision a
 * question it settles, and none for a summary nobody is recorded as writing or confirming,
 * which no issue records - and a requirement no plan task serves is no gap the page draws
 * either. Nor is a result that records no negative control: dispatch://INFRA-1253 R3 refuses
 * the results that gap would name, so it is no gap the page draws.
 *
 * `no_check` is the gap the coverage read already carries (`RequirementCoverage.gaps`), said in
 * the page's own words rather than in the server's `<block id> has no check`, which names a
 * block by its id. `shaped_disagreement` is a decision claiming a node whose own origin names
 * another, which the page draws rather than picking one of the two.
 *
 * Two of them are the chain's (R10), each read from what the page already holds
 * (`tree-drafts.ts`): `serves_not_requirement` is a citation that must name a requirement and
 * names a node of another kind, and `nobody_asked` a plan task naming neither a requirement it
 * serves nor a declaration it attempts.
 *
 * The lines say each of these in the reader's words rather than the model's: a node is "this",
 * because the line is drawn in the node's own body, and a declaration is what a person asked
 * for.
 */
export const GAP_LINES = freeze({
  no_check: "No check serves this statement.",
  nobody_asked: "This task names no statement it serves and nothing a person asked for.",
  serves_not_requirement: "What this says it serves is not a statement.",
  shaped_disagreement: "A decision says it shaped this, but this says it came from somewhere else.",
} as const);

/** One kind of gap the page names, which is a row's `data-gap-kind`. */
export type GapKind = keyof typeof GAP_LINES;

/**
 * The same gap in the fewest words, which is what a row of the tree draws beside the node (R1:
 * a row is one line). The sentence above is drawn in the node's body, where there is room for
 * it, so a reader sees that something is absent on the row and reads what it is by focusing it.
 */
export const GAP_WORDS = freeze({
  no_check: "No check",
  /** The task names no requirement it serves and nothing a person asked for, so no reason
   *  stands behind it. The chip says that and passes no judgement on the work. */
  nobody_asked: "No reason given",
  /** The citation names a node, and that node is a requirement no longer. */
  serves_not_requirement: "Not a statement",
  /** A decision says it shaped the node, and the node says it came from somewhere else: the
   *  two name different sources. */
  shaped_disagreement: "Sources disagree",
} as const satisfies Record<GapKind, string>);

/**
 * Which gap one line of the coverage read names, where it names one the page draws. The server
 * writes `<block id> has no check` and `<block id> has no plan task`
 * (`RequirementCoverage.gaps`), which name a block by its id; every surface of the page reads
 * the line through this and draws `GAP_WORDS` or `GAP_LINES` instead, so no identifier and no
 * server phrasing is drawn where a name belongs (R22). A requirement no plan task serves is no
 * gap the page draws (R10), so that line answers nothing.
 */
export function coverageGapKind(line: string): GapKind | undefined {
  return line.endsWith("has no check") ? "no_check" : undefined;
}

/** The noun a progress count is written with: `1/1 item`, `3/7 items` of the spec's checklist
 *  (`PROPERTY_LABELS.tasks`), `0/1 child`, `2/5 children`. The count itself is the issue's own
 *  and carries `AUTHOR_TEXT_ATTRIBUTE`. */
export const PROGRESS_NOUNS = freeze({
  child: "child",
  children: "children",
  task: "item",
  tasks: "items",
} as const);

/** How the issue stands, in a line of its own where no control's word carries it: a closed
 *  issue, whose writes the identity block refuses, and an issue that advances as many
 *  priorities as one may, whose priority row offers no control that adds another
 *  (`features/priorities/IssuePriorityLine.tsx`). One line each, saying what is so. */
export const STATE_LINES = freeze({
  advances_at_cap: `An issue advances at most ${MAX_ISSUE_ADVANCES} priorities.`,
  closed: "This issue is closed.",
  /** What Details says of a GitHub link whose issue or pull request Dispatch cannot read, since
   *  it holds no GitHub token for the link's repository, which today's pages say in the link's
   *  tooltip (`features/issue/GitHubLink.tsx`). It is that tooltip's sentence without
   *  "GitHub", whose capital inside the word reads as a code name to this list's rule on
   *  internal words (`INTERNAL_WORD` in `page-words.test.ts`); the link's own address, drawn
   *  before it, already says where the link points. */
  github_unreadable: "Details are unavailable for this repository.",
  /** How a subscribed agent stands whose subscription is wider than this issue, which Details
   *  draws where today's pages draw "via" and its topic with the reason as a tooltip
   *  (`features/issue/SubscribedAgents.tsx`). */
  subscription_wider:
    "Its subscription covers other issues and documents too, so it cannot be unsubscribed from this one alone.",
  /** What the outline says over its rows, on load as much as later, of each read they come from
   *  that stands failed, beside that read's Retry (`outline-reads.ts`): the names the rows cite,
   *  which checks serve each statement, the issue's decisions, the checks' results, the
   *  questions, the comments, the messages, and each document whose blocks it draws, named by
   *  the part the document plays. */
  names_unreadable: "Could not load the names this issue's rows cite.",
  coverage_unreadable: "Could not load which checks serve each statement.",
  issue_decisions_unreadable: "Could not load this issue's decisions.",
  results_unreadable: "Could not load the checks' results.",
  asks_unreadable: "Could not load the questions.",
  comments_unreadable: "Could not load the comments.",
  messages_unreadable: "Could not load the messages.",
  spec_unreadable: "Could not load the spec.",
  plan_unreadable: "Could not load the plan.",
  verification_unreadable: "Could not load the verification document.",
  decisions_document_unreadable: "Could not load the decisions document.",
} as const);

/**
 * What the page says in answer to the reader's own act, and never on load: a copy they pressed,
 * a list they opened still being read, and a write of theirs still out (dispatch://INFRA-1196
 * R22). The page's other answers are tables of their own -
 * `BLOCKED_BY_REFUSALS` here, `HELP_LINES` and `KEY_REPORTS` in `page-structure.ts` - and
 * `ANSWER_SOURCES` gathers all four, so a test can tell what the page says unasked from what it
 * says when asked. Each is at most one sentence and names no schema token.
 */
export const ANSWER_LINES = freeze({
  /** What a copy control says once the command is on the clipboard, and when the browser
   *  refused it; the toolbar's says what to do instead. */
  copied: "Copied",
  copy_failed: "Copy failed",
  copy_failed_hint: "Copy failed - select the text",
  /** What the priority picker says while the list it offers is still being read
   *  (`features/priorities/IssuePriorityLine.tsx`). */
  loading_priorities: "Loading priorities…",
  /** What the panel's priority row says when the priorities could not be read, and when a
   *  write of what the issue advances failed and the server gave no reason of its own. */
  priorities_unreadable: "Could not load the priorities.",
  priorities_not_saved: "Could not save what this issue advances.",
  /** What a handoff's line in the history says while its fields, which the reader opened, are
   *  read, and when they could not be (`HistoryHandoff.tsx`). */
  loading_handoff: "Loading handoff…",
  handoff_unreadable: "Could not load the handoff.",
  /** What the list of what references the focused row says while its read, which the reader
   *  opened, is out, and when it failed (`node-references/NodeReferences.tsx`). */
  references_loading: "Loading references…",
  references_unreadable: "Could not load the references.",
  /** What a decision's Change form says when Dispatch refused its send, whatever the server
   *  answered: the reader's words stay in the field, to send again (`DecisionActions.tsx`). */
  change_not_sent: "Could not send the change.",
  /** What a control says while its own write is out: Dispatch has not answered, so none of
   *  these says the thing is done. */
  saving_close: "Closing…",
  saving_components: "Saving components…",
  saving_field: "Saving…",
  saving_labels: "Saving labels…",
  saving_release: "Releasing…",
  saving_reopen: "Reopening…",
} as const);

/**
 * Each `when` value of a check block as words (R15). The values are the `when` choices of the
 * `check` block in `blocks.json`, which `CheckWhen` in `packages/contracts/src/dispatch-api.ts`
 * mirrors; `page-words.test.ts` reads that schema, so a new choice fails until it has words
 * here.
 */
export const CHECK_WHEN_WORDS = freeze({
  after_merge: "after the merge",
  before_merge: "before the merge",
  default_branch: "on the default branch",
} as const satisfies Record<CheckWhen, string>);

/** What a recorded result says about itself beside its verdict: where its commit stands against
 *  what its check compared its results with, by the server's answer for that result
 *  (`commit_stands`, `results_comparison.go`). A commit the check compared with is up to date
 *  with the work as it stands, any other commit is older work, and a commit Dispatch could not
 *  place draws no word at all. Keyed by the answers the API declares, so an answer renamed or
 *  added there is a compile error here rather than a result that silently draws no word.
 *
 *  A result whose check's words moved since it was recorded is older work too, whatever its
 *  commit (`check-facts.ts`). That word is the one its check's `older` state reads: a check's
 *  row and its result's fact read one standing, the server's for that check, so one fact has
 *  one word, on the result and on its check. */
export const RESULT_STANDS_WORDS = freeze({
  at_basis: "Up to date",
  other_commit: NODE_STATE_WORDS.check.older,
  unknown: undefined,
} as const satisfies Record<ResultCommitStands["answer"], string | undefined>);

/** What a recorded result says it saw, in the page's own words rather than the wire's `pass`
 *  and `fail`: the verdict is drawn as a fact of the result's own node (R15). */
export const RESULT_VERDICT_WORDS = freeze({
  fail: "Fail",
  pass: "Pass",
} as const);

/** How one delivery of a comment's mention or a message to an agent stands, in the focused
 *  row's facts, in words rather than the delivery's `failed`, `pending`, `queued` and `sent`
 *  (R22). Keyed by the states the API declares, so a state added there is a compile error here
 *  rather than a delivery that draws no word. */
export const DELIVERY_STATE_WORDS = freeze({
  failed: "Not delivered",
  pending: "Sending",
  queued: "Queued",
  sent: "Delivered",
} as const satisfies Record<DeliveryAttemptState, string>);

/**
 * What the blocked-by editor says when Dispatch refuses the list it saved
 * (`BlockedByControl.tsx`), one sentence per refusal, in place of the server's message, which
 * names the field by its wire token (`blocked_by issue CORE-9 not found`, R21). Each sentence is
 * cut where the issue it is about goes: that key is the server's detail and the reader's own
 * text, drawn between `before` and `after`. `BLOCKED_BY_INPUT` is three refusals, told apart by
 * the key it names: no issue, the issue itself, or none at all for a list over the limit; and
 * `DEPENDENCY_CYCLE` is two, the issue the list would make wait on itself, or none for a chain
 * longer than Dispatch follows (`packages/envoy/internal/dispatch/api/issue_parent.go`).
 */
export const BLOCKED_BY_REFUSALS = freeze({
  cycle: {
    after:
      " would then wait on itself, through the issues it waits on, its parent or its children.",
    before: "",
  },
  itself: { after: " is this issue, and an issue cannot wait on itself.", before: "" },
  not_found: { after: ".", before: "There is no issue " },
  too_deep: {
    after: "",
    before:
      "These issues wait on one another through more links than Dispatch follows, so it cannot check them for a loop.",
  },
  too_many: { after: "", before: `An issue can wait on at most ${MAX_ISSUE_BLOCKERS} issues.` },
} as const);

/** One refusal of a blocked-by list the page has a sentence for. */
export type BlockedByRefusalWords = keyof typeof BLOCKED_BY_REFUSALS;

/**
 * How a document of the issue stands with its reviewers, in words (R5): the head's "How it gets
 * done" row says this of the plan. The keys are `ArtifactApproval.state`; the version a review
 * covers is no part of the word, because the head is one line and the document's own toolbar
 * carries the version.
 */
export const APPROVAL_STATE_WORDS = freeze({
  approved: "Approved",
  awaiting: "Awaiting approval",
  changes_requested: "Changes requested",
  draft: "Draft",
  stale: "Approved, and changed since",
} as const);

/** The word for the approval state `token`, which the generated document carries as a plain
 *  string. A token the module does not declare reads as itself, which is what the server sent,
 *  as `issueStatusWord` does for a status. */
export function approvalStateWord(token: string): string {
  const words: Readonly<Record<string, string>> = APPROVAL_STATE_WORDS;
  return words[token] ?? token;
}

/**
 * Every label the page's own controls and headings carry. They live here rather than inside a
 * component so `DECLARED_WORDS` stays derived: a label a component writes inline would be a
 * word the page draws and the list does not hold.
 *
 * Each label names what the control does to this page, in a plain verb or a plain noun. The
 * values a control acts on - a title, a label, an author's name - are the author's words and
 * carry `AUTHOR_TEXT_ATTRIBUTE` instead.
 */
export const CONTROL_LABELS = freeze({
  /** Takes a suggestion's replacement into its document, in the panel's facts of the focused
   *  comment that carries it (the old conversation's thread card's control). */
  accept: "Accept",
  /** The one control that reaches a property with no value yet (R11, which bans "more"). */
  add: "Add",
  /** The labels control's word where the issue has no label yet, as `add_priority` is the
   *  priority's: the property's own name would draw "Labels" twice in its row of Details. */
  add_label: "Add a label",
  /** The control that places an issue under a priority, where the issue advances none: the
   *  words its name begins with, which `IssuePriorityLine.tsx` ends with the issue's key. */
  add_priority: "Add a priority",
  /** What the priority row says of a priority the issue advances because an issue above it
   *  does, which is that issue's to remove (dispatch://INFRA-2947 R4). */
  advances_inherited: "inherited",
  /** The other parents a transcluded node is also drawn under (R1, R4). */
  also_under: "Also under",
  /** Names a person's answer, drawn in the ask's own row and body, never as a node of its own
   *  (R1); the answer itself is the person's own words (`NodeRow.tsx`'s `AskAnswer`). It is also
   *  the focused open question's control that answers it, which sets the line at the foot of the
   *  outline to answer it (R16, the prototype's `.acts`), as `a` does. */
  answer: "Answer",
  /** What the line at the foot of the outline says of the open question it answers, before the
   *  question's own first line (the prototype's `.tgt.ans`). */
  answering: "Answering:",
  /** What it says of the options the reader picked, before them: "you chose 1024 px". */
  answer_chose: "you chose",
  /** What the line says while it is empty and answers a question. */
  answer_placeholder: "Answer in your own words…  (Enter sends · Shift+Enter new line)",
  /** The document a design-gate step reviews, by the step (`StepEntry.step`; `criteria` reviews
   *  the verification), as the names of that document's Approve and Request changes end with
   *  it where the gate's list carries the actions of more than one document (`ApprovalSteps`'
   *  `actionsOn`): each name begins with its control's own words and says its document,
   *  "Approve the spec", "Approve v5 of the spec", "Request changes to the verification"
   *  (WCAG 2.5.3). */
  approval_document_criteria: "the verification",
  approval_document_decisions: "the decisions",
  approval_document_spec: "the spec",
  /** What a stale approval chip says of the document's latest version inside `NoTooltips`,
   *  where no `title` carries it: "Approved v3 · latest v5" (`features/doc/ApprovalChip.tsx`). */
  approval_latest: "latest",
  /** The words between Request changes, or a stale approval's Approve, and the document it
   *  reviews, in those names: "Request changes to the spec", "Approve v5 of the spec". */
  approval_request_changes_to: "to",
  approval_version_of: "of",
  /** Leaves an editor with the value the issue had, in every editor the page opens. */
  cancel: "Cancel",
  /** Changes the reader's own answer to a question, on its focused row: the line at the foot of
   *  the outline answers it again over that answer (the old conversation's ask card's control). */
  change_answer: "Change answer",
  /** Answers an agent's decision with a change, on the decision's own node (R7). It opens the
   *  composer below, whose comment is anchored to the decision's own words. */
  change_decision: "Change",
  /** The direct children's count in the header's properties line, before the issue's own
   *  numbers: "Children 2 of 7 done" (R13). */
  children: "Children",
  /** The word between the words a shared control draws and the issue's key, in the name the
   *  control answers to inside `NoTooltips`: "Set components of CORE-1", "Saving components of
   *  CORE-1" (`componentsControlName`) and "Saving labels of CORE-1" (`labelsControlName`). */
  control_name_of: "of",
  /** The word between the labels control's `add_label` and the issue's key, in the name it
   *  answers to inside `NoTooltips` while the issue has no label: "Add a label to CORE-1"
   *  (`labelsControlName`). */
  control_name_to: "to",
  /** The two words that make a progress count read as a sentence in the header's properties
   *  line, around the issue's own numbers: "Children 2 of 7 done", "All issues under it 5 of 30
   *  done" (R13, the 1440 px picture). */
  count_of: "of",
  count_done: "done",
  /** The priority picker's first choice, which picks nothing. */
  choose_priority: "Choose a priority",
  /** Closes the Details panel and the Outline sheet, each in its own head (R14); Escape does
   *  the same. */
  close: "Close",
  /** Closes the document opened in place of the outline and draws the outline again (R3); the
   *  key that does the same is Escape. */
  close_document: "Back to the conversation",
  /** Closes the issue, from the identity block's one actions control (R11). */
  close_issue: "Close issue",
  /** What the components line says of an issue whose components come from an ancestor. */
  components_inherited_from: "inherited from",
  /** What it says of an issue the project's component tree reaches no component for, before
   *  the reason the server gives for that. */
  components_none: "None",
  /** What it says of an issue attached to no component at all. */
  components_not_attached: "Not attached",
  /** What it says of a component id the project's tree no longer holds, before that id. */
  components_retired: "retired",
  /** The control that attaches an issue with no components of its own to some. */
  components_set: "Set components",
  /** The Contents pane's list: each question and each of the reader's own threads, which go to
   *  their node in the outline (R14). */
  contents: "Contents",
  /** The kinds the line at the foot of the outline writes, as its chips (R16, the prototype's
   *  `.ck`): a comment, and a question, which is an ask of the issue. */
  composer_comment: "Comment",
  composer_question: "Question",
  /** The word that control carries, beside the command it copies. */
  copy: "Copy",
  /** Copies a check's `run` command, which renders as code beside it (R15). */
  copy_command: "Copy the command",
  /** Copies the address of the block the caret is in, in the open document's toolbar (R16). */
  copy_link: "Copy link",
  /** Copies the focused node's `dispatch://` address, among the focused row's acts, as today's
   *  page offers it on the same node (part G's R6, root R19). */
  copy_reference: "Copy reference",
  /** Draws a pinned version of the open document against the document as it stands (R16). */
  diff_vs_current: "Diff vs current",
  /** The one panel of the issue's properties, which opens over the right of the page, and the
   *  control at the right of the header's first row that opens it (R14). */
  details: "Details",
  /** The one row a branch below the three drawn levels folds to, which opens it in place: the
   *  count before it is the branch's own and carries `AUTHOR_TEXT_ATTRIBUTE` (R1), and the
   *  words agree with it: "1 more row below", "3 more rows below". A node is a row to the
   *  reader, the word the tree's own keys use (`useTreeKeymap.ts`). */
  deeper_row: "more row below",
  deeper_rows: "more rows below",
  /** The panel's list of the issue's history (R15): its status moves, claims, handoffs, the
   *  Legion daemon's messages, what agents wrote outside every thread a person wrote in, and
   *  every other event that is no row of the outline, newest first. `/activity` opens it. */
  history: "History",
  /** A file's latest version as a download, in the node menu, before that version's number:
   *  the link the Artifacts list drew on each row (R3, c-nothing-lost). */
  download_version: "Download version",
  /** Opens the editor of a property beside the value it already holds, where that value is
   *  drawn: the issues this one waits on (`BlockedByControl.tsx`). */
  edit: "Edit",
  /** What the line at the foot of the outline says of the reader's own comment it is editing,
   *  before that comment's first line: `edit` in the comment's facts opens it, and `save` sends
   *  it back (the old thread card's Edit and Save). */
  editing: "Editing:",
  /** Gives an issue that has a parent a different one (R11, row one). */
  edit_parent: "Edit parent issue",
  /** What an external link says of an address the browser will not open. */
  external_link_unsafe: "unsafe external link",
  /** The box that narrows the outline to the rows whose words hold what is typed, which is
   *  `?q=` (R20). */
  filter: "Filter",
  /** The box that narrows the node menu to the files whose name or kind holds what is typed:
   *  the Artifacts list's own filter, in the menu's words (R3). */
  filter_files: "Filter files and documents",
  /** Closes a branch of the tree (R1). */
  fold: "Fold",
  /** What a folded branch says it holds, after the count of the rows under it (R1): they are
   *  not gone, they are out of sight until the branch is opened. It agrees with its count, as
   *  `deeper_row` does. */
  folded_row: "row hidden inside",
  folded_rows: "rows hidden inside",
  /** Draws one branch of the outline at a time, the Contents pane's current one, by zooming the
   *  outline to it; pressed again, the whole outline (R14). */
  focus_branch: "Focus branch",
  /** How many checks a pull request has, before the count GitHub gives. */
  github_checks: "checks",
  /** The labels of the issue's three sentences, each before its sentence in the order the spec's
   *  summary holds them: what the issue is, why it matters, and what is true when it is done
   *  (R8, the architect's ruling 5, the approved pictures' "In short"). */
  in_short_what: "What it is:",
  in_short_why: "Why it matters:",
  in_short_done: "Done when:",
  /** The heading over the issue's three sentences in the Contents pane (R8). */
  in_short: "In short",
  /** The one control row one keeps the actions behind: close, reopen, the pin and setting the
   *  parent. It names actions, never a property (R11 bans "more"). */
  issue_actions: "Issue actions",
  /** Agrees with an agent's decision as its node draws it, which takes it out of what waits
   *  for the reader's review (R7). */
  keep_decision: "Keep",
  /** Keeps a node's body open once the focus leaves it (R2); the focused node's body is open
   *  without it. */
  keep_open: "Keep open",
  /** Opens the list of the page's keys, at the right of the filter row (the 1440 px picture):
   *  what `?` opens. */
  keys: "Keys",
  /** Lets a node kept open close again (R2). */
  let_close: "Let it close",
  /** Says what the reader may do with their pins, which the panel lists (R14). */
  pin: "Pin",
  /** The panel's list of what the reader pinned, each a link to the pinned row (R14). */
  pinned: "Pinned",
  /** The count that ends the header's properties line when the issue's labels do not all fit
   *  on it, after the number of labels left over: "+1 label", "+17 labels". It opens Details
   *  at its Labels row, which lists every label (R13, the ruling of
   *  dispatch://INFRA-1196/comment/63b73642-8fc5-4cc4-8f5d-a3d8989d3bf4). */
  labels_left_one: "label",
  labels_left_many: "labels",
  /** What the shell's indicator calls a write of where this issue's messages go: "Route of
   *  CORE-1 saved". It is the noun the editor's own Save route carries, so the line names what
   *  the reader just pressed. The box itself is named by the label it is drawn under,
   *  `PROPERTY_LABELS.route` ("Messages go to"), so a voice user says the words they read
   *  (WCAG 2.5.3). */
  route: "Route",
  /** Gives the open document's current text a version of its own (R16): the words the control
   *  draws, which are its name. */
  name_version: "Name version",
  /** Makes a document of the issue from a title typed, from the node menu (R16): the words the
   *  Artifacts panel's own control carries (`CreateDocument.tsx`), the one way into one form. */
  new_document: "New document",
  /** What a row of Details says of a property the issue has no value for (R13): the row is
   *  drawn, and the one add control lists the property. */
  none: "None",
  /** What the approval control says of a document nobody has asked anyone to approve: no
   *  approval, or a draft one (R31). The gated steps' entries say the same word. */
  not_requested: "Not requested",
  /** What the repository control says while neither the issue nor its project names one. */
  no_repository: "No repository",
  /** What the claim says of a holder the agent registry no longer lists (R11, row four). */
  not_running: "not running",
  /** The node menu beside the presets, holding every document and artifact of the issue and
   *  the upload (R3). */
  other_nodes: "Files and documents",
  /** Opens the Contents pane's list below 1024 px, where the pane is not drawn (R14). */
  outline: "Outline",
  /** The pin, as the issue's actions control holds it (R13, the Design's "Where each thing
   *  goes"): it reads what pressing it does, and carries `aria-pressed` for what is so. */
  pin_issue: "Pin issue",
  /** The row of five controls, Conversation and the four filters, as a reader hears it named
   *  (R2). */
  presets: "Filters",
  /** The priority picker, named by what the issue will do with the one picked. */
  priority_to_advance: "Priority to advance",
  /** Hands a claimed issue back, so another agent can take it (R11, row four). */
  release_claim: "Release",
  /** Turns a suggestion down, beside Accept. */
  reject: "Reject",
  /** Opens, in place, the rest of the words a row cut at six lines, and closes them again: an
   *  agent's words and an answered question are cut where more than 20 words would be hidden,
   *  and the control says how many, "more · 80 words" (the architect's ruling 1 on R1, the
   *  prototype's `CUT_LINES`). The count is the node's own and carries
   *  `AUTHOR_TEXT_ATTRIBUTE`. */
  read_more: "more",
  read_less: "less",
  read_more_words: "words",
  /** What reopening does, as the control's own words rather than a tooltip (R11). */
  reopen_into_backlog: "Reopen into Backlog",
  /** Reopens a resolved comment's thread, in the panel's facts of the focused row. */
  reopen_thread: "Reopen",
  /** Every row of the conversation that takes a reply carries it on its focus: a comment, a
   *  message and an ask whatever its state, an answered or a resolved one included (R16,
   *  dispatch://INFRA-3677); it sets the line at the foot of the outline to answer it, as `r`
   *  does. */
  reply: "Reply",
  /** What the line at the foot of the outline says of the row it replies to, before that row's
   *  author and first line (the prototype's `.tgt`). */
  replying_to: "Replying to:",
  /** Marks a comment's thread done, in the panel's facts of the focused row. */
  resolve: "Resolve",
  /** Sends a mention's delivery to its agent again, in the facts of the focused comment, while
   *  that is safe (the old conversation's delivery line). */
  retry: "Retry",
  /** Why the repository shown may not be the issue's own, said beside the control rather than
   *  in a tooltip (R11, R21). */
  repository_inherited: "inherited or the project's default",
  /** Writes the reader's own comment back from the line that edits it. */
  save: "Save",
  /** Each editor's save, naming what it writes. */
  save_blocked_by: "Save what it waits on",
  save_parent: "Save parent",
  save_route: "Save route",
  /** The names a focused row's scrollers carry while each keeps its tab stop, so a reader on the
   *  keyboard can reach it and scroll it sideways (decision 317): a code block, a drawn diagram,
   *  a table, and content of any other kind. */
  scrolls_code: "Code, scrolls sideways",
  scrolls_content: "Content, scrolls sideways",
  scrolls_diagram: "Diagram, scrolls sideways",
  scrolls_table: "Table, scrolls sideways",
  /** Sends the change a reader wrote against a decision, named apart from the conversation's
   *  own Send so a page drawing both offers two controls a reader can tell apart (R7). */
  send_change: "Send change",
  /** Sends what the line at the foot of the outline holds (R16), and the answer it holds to the
   *  question it answers (`send_answer`). */
  send: "Send",
  send_answer: "Send answer",
  /** How many checks serve a requirement, which the coverage read counts without naming them
   *  (R4, until the chain read lands). */
  serving_checks: "Checks serving it",
  /** How many plan tasks serve a requirement, counted the same way (R4). */
  serving_plan_tasks: "Plan tasks serving it",
  /** Gives an issue with no parent one (R11, row one). */
  set_parent: "Set parent issue",
  /** Ends that comparison and draws the pinned version itself (R16). */
  show_version: "Show version",
  /** Opens the rest of a list of unbounded length in Details, which shows its first ten
   *  entries and then this, with the whole count (R13): "Show all 14". */
  show_all: "Show all",
  /** Opens a handoff's fields and every proof entry under its line in the history, and closes
   *  them again (the old conversation's handoff card, R19). */
  show_fields: "Show fields",
  hide_fields: "Hide fields",
  /** What the claim says of the moment another agent may take an unresponsive holder's issue,
   *  which is its last heartbeat plus the grace the server refuses a take until (R11, row
   *  four). The moment itself follows this word, drawn as a date and a time and never as an
   *  age, so the chip reads "free for another agent from 11/11/26, 12:05" (`ClaimChip.tsx`). */
  takeable: "free for another agent from",
  /** The same once that moment has passed and the server would grant the take now. */
  takeable_now: "free for another agent to take now",
  /** What a chip says of a target nothing answers to now (`ReferenceTarget.gone`). */
  target_gone: "Deleted",
  /** What a chip on a decision says of a block the decision shaped and changed after: the
   *  decision changed since it named the block, and the block has not changed since
   *  (`DecisionLink.stale`). It says what happened in full, so it is no second meaning of the
   *  check state "Out of date". */
  target_stale: "Not updated since this decision changed",
  /** The body of the page, as a reader of the tree hears it named (R1). */
  tree: "Issue tree",
  /** What the assignee control says while nobody answers the issue's asks (R11). */
  unassigned: "Unassigned",
  /** What the claim says while the agent registry could not be read at all, so whether the
   *  holder is still running is unknown: a refusal the page draws rather than leaves to a
   *  tooltip it raises none of (R11, row four, and R21). It is the wording every other view of
   *  an unjudged running state already carries (`AgentName`'s "Running state unknown"). */
  unchecked: "running state unknown",
  /** Opens a branch of the tree (R1). */
  unfold: "Unfold",
  /** The pin's words while the issue is pinned. */
  unpin_issue: "Unpin issue",
  /** Takes a pin back, in the panel's facts of the focused row and in its list of pins. */
  unpin: "Unpin",
  /** The count of every issue under this one, at any depth, in the header's properties line:
   *  "All issues under it 5 of 30 done". */
  whole_tree: "All issues under it",
  /** The line at the foot of the outline, as a reader hears it named (R16, the prototype's
   *  `#composer`), and what it says while it is empty and writes a new comment or question on
   *  the issue. */
  write: "Write to the conversation",
  write_placeholder: "Write at the top of the conversation…  (Enter sends)",
  /** What the line says while it is empty and replies to a row: where the words go, as the
   *  answering line says of an answer. */
  reply_placeholder: "Your reply, under the row it answers…  (Enter sends)",
  /** What the line says while it is empty and edits the reader's own comment. */
  edit_placeholder: "Your comment, as it should read…  (Enter saves)",
  /** Sets the line back from the row it replies to, or the question it answers, to a new comment
   *  on the issue (the prototype's `.tgt .x`). */
  write_on_issue: "Write on the issue instead",
  /** What a decision the model marks unsure says of itself, in one word on its own row (R9):
   *  the decisions read's `unsure`, which the server sets for a decision holding a paragraph
   *  that opens `Unsure:`. */
  unsure: "Unsure",
  /** Who made a node, where the author is an agent nothing names: a session with no title in
   *  the registry and none stamped on its write. An author is drawn by a name and never by an
   *  identifier (R9, R22), so the page says what it is rather than which one
   *  (`AgentText.author`, `features/refs/AgentName.tsx`). */
  unnamed_author: "An agent",
  /** Who made a node the model holds no author for (R9): a requirement, a check, a plan task,
   *  a note and a decision, whose reads carry none. It is the page's own word, drawn with a
   *  shape of its own (`AuthorKind`), so no reader takes it for somebody's name. */
  unknown_author: "Author unknown",
  /** What the claim says of a holder that has missed its heartbeats without ending: it has not
   *  gone, it still holds the issue, and it may yet answer (R11, row four). */
  unresponsive: "unresponsive",
  /** What stands between that word and the heartbeat the holder was last seen at. */
  unresponsive_since: "since",
  /** Adds an artifact from the node menu (R16). */
  upload: "Upload",
  /** The file chooser that control opens, named apart from the Artifacts tab's own so a page
   *  drawing both offers two controls a reader can tell apart. */
  upload_file: "Upload a file",
  /** Which version of the open document the preset draws (R16). */
  version: "Version",
  /** Names the author of a node whose author the model holds (R7); the name itself is the
   *  author's own (`AgentText.author`). */
  written_by: "Written by",
  /** The box that composer writes in: what the reader would change about the decision. */
  your_change: "Your change",
} as const);

/**
 * A plain label for every attribute a typed block can draw in its header, keyed by the
 * attribute's own name in `blocks.json`. The server-owned ones are here too, because a block
 * whose type the schema no longer describes draws every attribute it carries
 * (`renderTypedBlock`, `features/doc/typed-block-drawing.ts`).
 *
 * A label is lowercase, like the block name it sits under, and the stylesheet adds the colon.
 */
export const BLOCK_ATTRIBUTE_LABELS = freeze({
  answer: "answer",
  answered_at: "answered at",
  answered_by: "answered by",
  /** The CI run whose conclusion at the pull request's head is a check's result, read in place
   *  of one a role records. "CI" keeps its capitals: it is an initialism, not a word. */
  ci_check: "CI check",
  copied_from: "copied from",
  copied_from_document: "copied from document",
  /** Why the block cannot be answered as written; "invalid" names a verdict, not the reason. */
  invalid: "problem",
  kind: "kind",
  /** Whether the reader may choose more than one option. */
  multiple: "more than one answer",
  needs: "needs",
  negative_control: "negative control",
  run: "run",
  selected: "selected",
  /** The requirements a check or a plan task is there for; the spec's own verb for the edge. */
  serves: "serves",
  state: "state",
  surface: "surface",
  title: "title",
  urgency: "urgency",
  when: "when",
} as const);

/**
 * The label of the block attribute `name`. An attribute the module does not declare reads as
 * its own name with each underscore as a space, which a person can read; it is the fallback
 * rather than the answer, because `page-words.test.ts` reads the block schema and fails until
 * the attribute has a label here.
 */
export function blockAttributeLabel(name: string): string {
  const labels: Readonly<Record<string, string>> = BLOCK_ATTRIBUTE_LABELS;
  return labels[name] ?? name.replaceAll("_", " ");
}

/**
 * A plain label for every fact a node's body draws that is no attribute of a block: what a
 * recorded result says of itself (`check-facts.ts`). A check's own attributes
 * keep the labels their document's header draws them under, so a check reads the same in the
 * tree and in the document it lives in, and only what the schema has no name for is here.
 *
 * A label is lowercase, like the attribute labels it sits beside, and the row draws the colon.
 */
export const NODE_FACT_LABELS = freeze({
  /** Why a check CI proves has no result: the server's own sentence for what its run showed at
   *  the head it read (`CheckBlock.ci.detail`), drawn where an ordinary check draws none. */
  ci_unproved: "why no result",
  commit: "commit",
  excluded: "excluded",
  failed: "failed",
  role: "role",
  screenshot: "screenshot",
  skipped: "skipped",
  /** Whether the result still speaks for the work as it stands (`RESULT_STANDS_WORDS`). */
  stands: "how it stands",
  verdict: "verdict",
} as const);

/** The label of the fact `name`: this module's own where it has one, else the label its block
 *  attribute carries (`blockAttributeLabel`). */
export function nodeFactLabel(name: string): string {
  const labels: Readonly<Record<string, string>> = NODE_FACT_LABELS;
  return labels[name] ?? blockAttributeLabel(name);
}

/**
 * The three kinds of author a node of the page can have, each drawn with a shape of its own so
 * two authors of different kinds never share one (R9): a person, writing as themselves; an
 * agent, writing through a session; and an author the model holds none of, which the row says
 * in the page's own words (`CONTROL_LABELS.unknown_author`). Dispatch's own writes, which no
 * person and no session made, are an agent's: something other than a person wrote them.
 */
export type AuthorKind = "agent" | "person" | "unknown";

/** The kind of author `actor` is, and `unknown` where the model holds none. */
export function authorKind(actor: Actor | undefined): AuthorKind {
  if (actor === undefined) return "unknown";
  return actor.kind === "user" ? "person" : "agent";
}

/**
 * Whether `actor` is the reader writing as themselves: a person signed in as `reader`, whose
 * login matches in any casing as the server matches it (`isViewer`), and not an agent acting for
 * them, whose words are the agent's (R9, R34). It is the one rule for whose words carry the ink
 * rule and which decisions are the reader's own, read by the outline's rows and by each filter's
 * body.
 */
export function isReader(actor: Actor | undefined, reader: string | undefined): boolean {
  return actor !== undefined && isViewer(actor, reader) && (actor.act?.session ?? "") === "";
}

/** What each kind of author is called, in the accessible name of its shape: "Written by a
 *  person", "Written by an agent", "Written by an author nobody recorded". */
export const AUTHOR_KIND_WORDS = freeze({
  agent: "an agent",
  person: "a person",
  unknown: "an author nobody recorded",
} as const satisfies Record<AuthorKind, string>);

/** What the node menu says beside each file's name (R3, R22): its kind, in a reader's word where
 *  the Artifacts list drew the kind's own token (`doc`), and the mark on the issue's spec, which
 *  that list drew as its Primary badge. A kind this build does not know is a file. */
export const FILE_WORDS = freeze({
  doc: "document",
  file: "file",
  image: "image",
  primary: "Primary",
} as const);

/**
 * The `data-glyph` token of each shape the page draws: one per node type, one per state of the
 * three types that have states, and one per kind of author. No two types share a token and no
 * two states of one type's surface share one, so a reader who tells shapes apart tells the
 * nodes apart; a state's token carries its type, which keeps the check's seven states distinct
 * from the ask's own as well, including the `retracted` both of them hold.
 *
 * The four ways an ask is closed carry four tokens drawing the one closed shape
 * (`NodeGlyph.tsx`): to a reader a declined, a retracted, a withdrawn and a plainly resolved
 * question are all shut, and the word beside the shape is what tells them apart. The tokens
 * stay distinct because a shape carries its own accessible name, and a declined ask's shape
 * may not announce itself as a resolved one.
 *
 * `authors` holds one token per kind of author (R9), drawn beside the author's name: the shape
 * says what kind of author made the node, and the name beside it says who.
 */
export const GLYPH_TOKENS = freeze({
  authors: {
    agent: "author-agent",
    person: "author-person",
    unknown: "author-unknown",
  },
  states: {
    ask: {
      answered: "ask-answered",
      declined: "ask-declined",
      open: "ask-open",
      resolved: "ask-resolved",
      retracted: "ask-retracted",
      withdrawn: "ask-withdrawn",
    },
    check: {
      failed: "check-failed",
      "no-result": "check-no-result",
      older: "check-older",
      passed: "check-passed",
      retracted: "check-retracted",
      unmarked: "check-unmarked",
      unreadable: "check-unreadable",
    },
    decision: {
      change_requested: "decision-change-requested",
      kept: "decision-kept",
      replaced: "decision-replaced",
      waiting_review: "decision-waiting-review",
    },
  },
  /** The shape of a statement no check serves, at the right of its row in the Spec filter in
   *  the place of how it stands (R10, R17). */
  no_check: "statement-no-check",
  /** How a statement stands at the commit the page names, in the Verification filter and at
   *  the right of its row in the Spec filter (R17): the page's reading and no state of the
   *  model's, so it is a table of its own. */
  statements: {
    met: "statement-met",
    not_met: "statement-not-met",
    unknown: "statement-unknown",
  },
  types: {
    ask: "ask",
    callout: "callout",
    check: "check",
    child_issue: "child-issue",
    comment: "comment",
    decision: "decision",
    document: "document",
    handoff: "handoff",
    image: "image",
    issue: "issue",
    message: "message",
    plan_task: "plan-task",
    requirement: "requirement",
    result: "result",
  },
} as const satisfies {
  readonly authors: Record<AuthorKind, string>;
  readonly no_check: string;
  readonly states: {
    readonly ask: Record<AskState, string>;
    readonly check: Record<CheckState, string>;
    readonly decision: Record<DecisionState, string>;
  };
  readonly statements: Record<StatementState, string>;
  readonly types: Record<NodeType, string>;
});

/** Every type whose state the page draws, which is every key of `NODE_STATE_WORDS`. */
const STATE_TYPES = freeze(keysOf(NODE_STATE_WORDS));

/** A state word as it reads after its type's name and a colon, "Passed" as "passed": its first
 *  letter in lower case where the rest of it already is, and every capital kept in a word that
 *  holds one past its first letter, as an initialism or a name does. */
function stateWordAfterColon(word: string): string {
  const rest = word.slice(1);
  return rest === rest.toLowerCase() ? `${word.slice(0, 1).toLowerCase()}${rest}` : word;
}

function glyphNames(): Readonly<Record<string, string>> {
  const names: Record<string, string> = {};
  for (const type of NODE_TYPES) {
    names[GLYPH_TOKENS.types[type]] = NODE_TYPE_NAMES[type];
  }
  for (const type of STATE_TYPES) {
    const words: Readonly<Record<string, string>> = NODE_STATE_WORDS[type];
    const tokens: Readonly<Record<string, string>> = GLYPH_TOKENS.states[type];
    for (const [state, word] of Object.entries(words)) {
      const token = tokens[state];
      // A state's shape is named by its type and then its state: "Check: passed". A state the
      // row follows with a name ("Kept by" and who kept it) names its shape without the word
      // that leads to the name: "Decision: kept".
      if (token !== undefined) {
        const stateWord = stateWordAfterColon(word.replace(/ by$/u, ""));
        names[token] = `${sentenceCase(NODE_TYPE_NAMES[type])}: ${stateWord}`;
      }
    }
  }
  // How a statement stands is named by the word the legend draws beside its shape, and a
  // statement no check serves by the gap's own short word.
  for (const state of keysOf(GLYPH_TOKENS.statements)) {
    names[GLYPH_TOKENS.statements[state]] = VERIFICATION_WORDS.states[state];
  }
  names[GLYPH_TOKENS.no_check] = GAP_WORDS.no_check;
  // An author's shape says what the name beside it is, in the words the page names an author
  // with everywhere else (`CONTROL_LABELS.written_by`) and the kind of author it is, so a reader
  // who cannot see the shape hears "Written by a person" and then the name.
  for (const kind of keysOf(GLYPH_TOKENS.authors)) {
    names[GLYPH_TOKENS.authors[kind]] = `${CONTROL_LABELS.written_by} ${AUTHOR_KIND_WORDS[kind]}`;
  }
  return names;
}

/**
 * The accessible name of each glyph, by its token: a shape carries its own name, because a
 * reader who cannot see the shape still has to hear what it says. A type's shape is named by
 * the type; a state's shape by the type it stands on and then its state word, as in
 * "Check: passed"; the author's shape by the page's own words for who wrote a node and what
 * kind of author it is.
 */
export const GLYPH_NAMES: Readonly<Record<string, string>> = freeze(glyphNames());

/**
 * The words of `text`, as the page's vocabulary counts them: lowercase, each run of letters and
 * digits a word, an apostrophe inside a word kept, and every other character a break. A reader
 * of the page splits drawn text the same way, so the two sides compare word for word.
 */
export function wordsIn(text: string): readonly string[] {
  return text.toLowerCase().match(/[\p{L}\p{N}]+(?:'[\p{L}\p{N}]+)*/gu) ?? [];
}

/** Every string `value` holds, however deeply, in the order it holds them. */
function phrasesIn(value: unknown): readonly string[] {
  if (typeof value === "string") {
    return [value];
  }
  if (typeof value === "object" && value !== null) {
    return Object.values(value).flatMap(phrasesIn);
  }
  return [];
}

/**
 * What the page says unasked: every name, state, label and empty line it may draw on load. None
 * is a sentence about the page or about Dispatch; the sentences among them are the empty lines,
 * the gap lines - what a node lacks - and the state lines.
 */
export const UNASKED_SOURCES: readonly unknown[] = freeze([
  NODE_TYPE_NAMES,
  BLOCK_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  FILTER_SIGNAL_WORDS,
  CONTENTS_MARK_WORDS,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  PROGRESS_NOUNS,
  CHECK_WHEN_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  DELIVERY_STATE_WORDS,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
  AUTHOR_KIND_WORDS,
  FILE_WORDS,
  SPEC_WORDS,
  DECISIONS_WORDS,
  VERIFICATION_WORDS,
  PLAN_WORDS,
  NODE_REFERENCES_WORDS,
]);

/** What the page says only in answer to the reader's own act, and never on load
 *  (`ANSWER_LINES`). */
export const ANSWER_SOURCES: readonly unknown[] = freeze([
  ANSWER_LINES,
  BLOCKED_BY_REFUSALS,
  HELP_LINES,
  KEY_REPORTS,
]);

/** Every string the answers above hold, in the order they hold them: a browser check reads a
 *  freshly loaded page for each of them. */
export const ANSWER_PHRASES: readonly string[] = freeze(ANSWER_SOURCES.flatMap(phrasesIn));

/**
 * Everything the page's own chrome may say, as the declarations above say it. `DECLARED_WORDS`
 * is derived from this list and from nothing else, so a word added to a name, a label or a line
 * joins the set by itself, and a word added nowhere is not in it.
 */
const WORD_SOURCES: readonly unknown[] = [...UNASKED_SOURCES, ...ANSWER_SOURCES];

/**
 * Every word the page's own chrome may show, lowercase and sorted, with no word that appears in
 * none of the declarations above. A reader of the page checks the text of each
 * `[data-page-chrome]` element against this list, so an internal word drawn where a name
 * belongs has nowhere to hide.
 */
export const DECLARED_WORDS: readonly string[] = freeze(
  [...new Set(WORD_SOURCES.flatMap(phrasesIn).flatMap(wordsIn))].sort()
);
