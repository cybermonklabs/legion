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
 * This module is data and pure functions. It holds no React and touches no DOM, and the only
 * thing it imports is its sibling `page-structure.ts`, whose property and section names and
 * help lines `DECLARED_WORDS` is derived from as well; so a test, a browser spec and a
 * component all read the same list.
 */

import { MAX_ISSUE_BLOCKERS, type ResultCommitStands } from "@legion/contracts";

import { ASK_RESOLUTION_WORDS } from "../refs/actor";
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
  ask: "ask",
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
  requirement: "requirement",
  result: "result",
} as const);

/** One kind of node the page draws, by the model's own token for it. */
export type NodeType = keyof typeof NODE_TYPE_NAMES;

/**
 * The state words of each type whose state the model holds, and no entry at all for a type it
 * gives no state: a comment is not in a state, so the page has no word to draw for one, and an
 * entry holding an empty table would invite a row to draw a blank badge.
 *
 * The ask's three states are the `state` choices of the `ask` block (`blocks.json`). The
 * check's seven are `checkState`'s (`features/issue/tree-drafts.ts`), and the page
 * must tell all seven apart, so each keeps a word of its own.
 *
 * A decision's three are how the old Decisions tab grouped them, which is what a reader used
 * to find the ones waiting on them: `waiting_review` is what waits for the reader's review,
 * `yours` is a decision a person made or an agent's one the reader kept
 * (`kind === "person" || kept !== null`, and not replaced), and `replaced` is one a later
 * decision replaced (`decisions.go`, `DecisionsPanel.tsx` at
 * 7567aeb606f309f40d4145f3d6a49c19fd452558). A decision in none of the three - an agent's that
 * nobody has reviewed and nothing replaced - stood in no section of that tab either, and draws
 * no state here.
 */
export const NODE_STATE_WORDS = freeze({
  ask: {
    answered: "Answered",
    open: "Open",
    // How an ask was closed rather than answered, in the words the card and the margin draw
    // beside the tree (`ASK_RESOLUTION_WORDS`, the one table all three read): an ask whose
    // `state` is `resolved` stands in the state its resolution names, so a declined question
    // reads "Declined" on its row and "Declined" on its card. `resolved` is both the block
    // state the schema declares and the kind of a plain resolution, under the one word.
    ...ASK_RESOLUTION_WORDS,
  },
  check: {
    failed: "Failed",
    "no-result": "No result",
    /** The newest result that stands was recorded against work the check no longer compares
     *  its results with, or against an older wording of the check, so it no longer speaks for
     *  the work as it stands, whatever it found. A result says the same of itself in the same
     *  words (`RESULT_STANDS_WORDS`), and nothing else on the page draws them: a decision's
     *  chip for a block it changed after naming says so in words of its own
     *  (`CONTROL_LABELS.target_stale`). */
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
    replaced: "Replaced",
    waiting_review: "Waiting for your review",
    yours: "Yours",
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

/** The five presets in the order the row draws them (R3). The row is a query over one tree, so
 *  the order is the reader's reading order: the whole tree, then what was decided, then what is
 *  checked, then the spec and the plan the tree carries. */
export const PRESET_ORDER = freeze(["tree", "decisions", "checks", "spec", "plan"] as const);

/** One preset of the issue page, by its own token, which is also its address. */
export type Preset = (typeof PRESET_ORDER)[number];

/** The name of each preset. "Checks" rather than "Verification", because verification names an
 *  activity and the preset draws a thing (R15). */
export const PRESET_NAMES = freeze({
  checks: "Checks",
  decisions: "Decisions",
  plan: "Plan",
  spec: "Spec",
  tree: "Tree",
} as const satisfies Record<Preset, string>);

/** The four states the conversation panel can be read in (R12), in the order the panel offers
 *  them: everything the issue recorded, what the reader pinned, the questions alone, and what
 *  people and agents actually said. */
export const CONVERSATION_STATE_ORDER = freeze([
  "everything",
  "pinned",
  "questions",
  "words",
] as const);

/** One state of the conversation panel, by its own token, which is its `data-conversation-state`. */
export type ConversationState = (typeof CONVERSATION_STATE_ORDER)[number];

/** The name of each conversation state. */
export const CONVERSATION_STATE_NAMES = freeze({
  everything: "Everything",
  pinned: "Pinned",
  questions: "Questions",
  words: "What people said",
} as const satisfies Record<ConversationState, string>);

/**
 * How an issue stands, as the page says it: the word a child's row draws beside it (R10). The
 * keys are the server's own status tokens, in the lifecycle order `features/project/board-model.ts`
 * holds them in, and `page-words.test.ts` reads that module, so a status added there fails
 * until it has a word here.
 */
export const ISSUE_STATUS_WORDS = freeze({
  backlog: "Backlog",
  done: "Done",
  icebox: "Icebox",
  in_progress: "In progress",
  needs_review: "Needs review",
  retro: "Retro",
  testing: "Testing",
  todo: "Todo",
  triage: "Triage",
} as const);

/** The word for the issue status `token`. A token the module does not declare reads as itself,
 *  which is what the server sent; `page-words.test.ts` fails until a declared status has a word. */
export function issueStatusWord(token: string): string {
  const words: Readonly<Record<string, string>> = ISSUE_STATUS_WORDS;
  return words[token] ?? token;
}

/**
 * Every line the page draws where a place holds nothing, one per situation: a preset whose
 * query matches nothing, an issue with no children, a spec with no summary, a check with no
 * result, and a folded branch that turns out to hold nothing. Each is one sentence, in the
 * simple present, saying what is absent rather than what the reader should do about it.
 */
export const EMPTY_STATE_LINES = freeze({
  check_result: "No result recorded against this check.",
  checks: "This issue has no checks yet.",
  children: "This issue has no child issues.",
  decisions: "This issue has no decisions yet.",
  /** A document node whose own text has not arrived yet (R16). */
  document_reading: "This document has not arrived yet.",
  /** A document node whose own text could not be read. */
  document_unread: "This document could not be read.",
  folded_branch: "This branch holds nothing.",
  plan: "This issue has no plan tasks yet.",
  spec: "This issue has no requirements yet.",
  /** What the requirements panel says of a spec that marks none, which is not the same as an
   *  issue with no spec at all. */
  spec_marks_none: "This spec marks no requirements.",
  spec_summary: "This spec has no summary.",
  /** A summary whose stored excerpt stops inside its first sentence (`BlockExcerptRunes`, 480
   *  runes): the spec has a summary, and the head has no whole sentence of it to draw. */
  spec_summary_cut: "The summary is too long to show here.",
  /** A summary whose whole text ends in no sentence end, so it holds no sentence the head
   *  draws. */
  spec_summary_unended: "The summary has no complete sentence.",
  tree: "This issue holds nothing yet.",
} as const);

/**
 * What the page says of a gap in the reasoning, one line per kind of gap (R4). A gap is drawn on
 * the node it is about and never as an error: a node nothing explains is a node, and the line
 * says what is absent.
 *
 * `no_check` and `no_plan_task` are the two gaps the coverage read already carries
 * (`RequirementCoverage.gaps`), said in the page's own words rather than in the server's
 * `<block id> has no check`, which names a block by its id. `no_origin` is a node whose spine
 * parent the model does not hold; `shaped_disagreement` is a decision claiming a node the
 * node's own origin does not name, which the page draws rather than picking one of the two.
 *
 * Three of them are the chain's (R8), each read from what the page already holds
 * (`tree-drafts.ts`, `check-facts.ts`): `serves_not_requirement` is a citation that must name a
 * requirement and names a node of another kind, `nobody_asked` a plan task naming neither a
 * requirement it serves nor a declaration it attempts, and `no_negative_control` a check's
 * latest result recording no negative control. The last one says what the result left out and
 * no more: whether a control could have changed the verdict is not in the data, and the page
 * states no verdict of its own on it.
 *
 * The lines say each of these in the reader's words rather than the model's: a node is "this",
 * because the line is drawn in the node's own body; a node's origin is where it came from; a
 * declaration is what a person asked for; a read-back is a person confirming the sentences;
 * and a result with no negative control is said as what that leaves out, which is a showing
 * that the check can fail. The control keeps its own name where the check's attribute is drawn
 * (`BLOCK_ATTRIBUTE_LABELS.negative_control`).
 */
export const GAP_LINES = freeze({
  no_check: "No check serves this requirement.",
  no_negative_control: "This result does not show that the check can fail.",
  no_origin: "This does not say where it came from.",
  no_plan_task: "No plan task serves this requirement.",
  no_read_back: "Dispatch does not record who confirmed these sentences.",
  no_summary_author: "Dispatch does not record who wrote these sentences.",
  nobody_asked: "This task names no requirement it serves and nothing a person asked for.",
  serves_not_requirement: "What this says it serves is not a requirement.",
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
  /** What the result the check was last run for recorded under its negative control: nothing,
   *  so nothing in it shows that the check can fail. */
  no_negative_control: "No proof it can fail",
  /** The node names nothing it came from, so a reader cannot follow it back. It says the node
   *  gives no source, and never that it has none (R23). */
  no_origin: "No source given",
  no_plan_task: "No plan task",
  /** The head of every issue draws this one, because Dispatch holds no record of a person
   *  confirming a summary at all yet (dispatch://INFRA-1278). It names Dispatch rather than
   *  the issue, so a reader does not take it for something their own issue is missing, and it
   *  is short enough that the two head chips share one line at 390 px: measured there, the
   *  chips are 141 px and 166.6 px with an 8 px gap, 315.6 px of the head's 358 (the words
   *  before them took 310.3), and a second line there puts the first open question off the
   *  first screen (R6, `e2e/issue-layout.e2e.ts`, c-open-questions). */
  no_read_back: "Not confirmed in Dispatch",
  /** The same for who wrote the sentences, which Dispatch holds of no issue either. */
  no_summary_author: "No author in Dispatch",
  /** The task names no requirement it serves and nothing a person asked for, so no reason
   *  stands behind it. The chip says that and passes no judgement on the work. */
  nobody_asked: "No reason given",
  /** The citation names a node, and that node is a requirement no longer. */
  serves_not_requirement: "Not a requirement",
  /** A decision says it shaped the node, and the node says it came from somewhere else: the
   *  two name different sources, in the word `no_origin` uses for one. */
  shaped_disagreement: "Sources disagree",
} as const satisfies Record<GapKind, string>);

/**
 * Which gap one line of the coverage read names. The server writes `<block id> has no check`
 * and `<block id> has no plan task` (`RequirementCoverage.gaps`), which name a block by its id;
 * every surface of the page reads the line through this and draws `GAP_WORDS` or `GAP_LINES`
 * instead, so no identifier and no server phrasing is drawn where a name belongs (R21).
 */
export function coverageGapKind(line: string): GapKind {
  return line.endsWith("has no check") ? "no_check" : "no_plan_task";
}

/** The noun a progress count is written with: `1/1 task`, `3/7 tasks`, `0/1 child`,
 *  `2/5 children`. The count itself is the issue's own and carries `AUTHOR_TEXT_ATTRIBUTE`. */
export const PROGRESS_NOUNS = freeze({
  child: "child",
  children: "children",
  task: "task",
  tasks: "tasks",
} as const);

/** What a panel says while a read of its own is out, and when that read failed. Neither is an
 *  empty state - the place may hold plenty - so neither belongs in `EMPTY_STATE_LINES`. */
export const READING_LINES = freeze({
  coverage_failed: "Could not load the requirement coverage.",
  coverage_loading: "Loading requirements…",
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
} as const);

/** What a recorded result says about itself beside its verdict: where its commit stands against
 *  what its check compared its results with, by the server's answer for that result
 *  (`commit_stands`, `results_comparison.go`). A commit the check compared with is up to date
 *  with the work as it stands, any other commit is older work, and a commit Dispatch could not
 *  place draws no word at all. Keyed by the answers the API declares, so an answer renamed or
 *  added there is a compile error here rather than a result that silently draws no word.
 *
 *  A result whose check's words moved since it was recorded is older work too, whatever its
 *  commit (`check-facts.ts`). That word is the one its check's `older` state reads, which is the
 *  state of a check whose newest result that stands is such a result and whose pass stands
 *  nowhere (`tree-drafts.ts`'s `checkState`): one fact, one word, on the result and on its
 *  check. */
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

/**
 * One line per thing the page cannot show yet, because Dispatch does not serve it (R20). A
 * preset says what it cannot show rather than drawing a count with nothing behind it: the
 * coverage read counts the checks that serve a requirement and names none of them, and the
 * chain read that would name them (dispatch://INFRA-1098 item 14) has no route.
 */
export const WAITING_LINES = freeze({
  chain:
    "Each requirement shows how many checks serve it, and Dispatch cannot yet say which checks those are.",
} as const);

/**
 * What the blocked-by editor says when Dispatch refuses the list it saved
 * (`BlockedByControl.tsx`), one sentence per refusal, in place of the server's message, which
 * names the field by its wire token (`blocked_by issue CORE-9 not found`, R21). Each sentence is
 * cut where the issue it is about goes: that key is the server's detail and the reader's own
 * text, drawn between `before` and `after`. `BLOCKED_BY_INPUT` is three refusals, told apart by
 * the key it names: no issue, the issue itself, or none at all for a list over the limit;
 * `BLOCKED_BY_OUTSIDE_PROJECT` is one; and `DEPENDENCY_CYCLE` is two, the issue the list would
 * make wait on itself, or none for a chain longer than Dispatch follows
 * (`packages/envoy/internal/dispatch/api/issue_parent.go`).
 */
export const BLOCKED_BY_REFUSALS = freeze({
  cycle: {
    after:
      " would then wait on itself, through the issues it waits on, its parent or its children.",
    before: "",
  },
  itself: { after: " is this issue, and an issue cannot wait on itself.", before: "" },
  not_found: { after: ".", before: "There is no issue " },
  outside_project: {
    after: " is in another project, and an issue can wait only on issues of its own project.",
    before: "",
  },
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
  /** The one control that reaches a property with no value yet (R11, which bans "more"). */
  add: "Add",
  /** The other requirements a transcluded node is also drawn under (R23). */
  also_under: "Also under",
  /** What the blocked-by line says while the issue waits on no other issue
   *  (`BlockedByControl.tsx`). */
  blocked_by_none: "nothing",
  /** Leaves an editor with the value the issue had, in every editor the page opens. */
  cancel: "Cancel",
  /** Answers an agent's decision with a change, on the decision's own node (R7). It opens the
   *  composer below, whose comment is anchored to the decision's own words. */
  change_decision: "Change",
  /** The collapsible section of child issues at the top of every preset (R10). */
  children: "Children",
  /** The direct children done of total, labelled on the collapsed header (R10). */
  children_done: "Children done",
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
  /** The heading of the conversation inside the one right-hand panel (R12). */
  conversation: "Conversation",
  /** The word that control carries, beside the command it copies. */
  copy: "Copy",
  /** Copies a check's `run` command, which renders as code beside it (R15). */
  copy_command: "Copy the command",
  /** What it says once the command is on the clipboard, and when the browser refused it. */
  copied: "Copied",
  copy_failed: "Copy failed",
  /** Why a copy the browser refused can still be made: the toolbar says what to do instead. */
  copy_failed_hint: "Copy failed - select the text",
  /** Copies the address of the block the caret is in, in the open document's toolbar (R16). */
  copy_link: "Copy link",
  /** Draws a pinned version of the open document against the document as it stands (R16). */
  diff_vs_current: "Diff vs current",
  /** The one row a branch below the three drawn levels folds to, which opens it in place: the
   *  count before it is the branch's own and carries `AUTHOR_TEXT_ATTRIBUTE` (R1). It says
   *  what pressing it shows, because that row is the only way to those rows, and it agrees
   *  with its count: "1 more row below", "3 more rows below". A node is a row to the reader,
   *  the word the tree's own keys use (`useTreeKeymap.ts`). */
  deeper_row: "more row below, press to show it",
  deeper_rows: "more rows below, press to show them",
  /** Takes a version of an artifact down (R16). */
  download: "Download",
  /** Opens the editor of a property beside the value it already holds, where that value is
   *  drawn: the issues this one waits on (`BlockedByControl.tsx`). */
  edit: "Edit",
  /** Gives an issue that has a parent a different one (R11, row one). */
  edit_parent: "Edit parent issue",
  /** What an external link says of an address the browser will not open. */
  external_link_unsafe: "unsafe external link",
  /** The box that narrows the preset to the rows whose words hold what is typed (R3). */
  filter: "Filter",
  /** Closes a branch of the tree (R1). */
  fold: "Fold",
  /** What a folded branch says it holds, after the count of the rows under it (R1): they are
   *  not gone, they are out of sight until the branch is opened. It agrees with its count, as
   *  `deeper_row` does. */
  folded_row: "row hidden inside",
  folded_rows: "rows hidden inside",
  /** How many checks a pull request has, before the count GitHub gives. */
  github_checks: "checks",
  /** What an external link says when Dispatch holds no token for its repository: the line is
   *  drawn beside the link rather than kept in a tooltip (R21). */
  github_unavailable: "GitHub details are unavailable for this repository",
  /** The head's first row: where the plan says how the issue gets done (R5). */
  how_it_gets_done: "How it gets done",
  /** The one control row one keeps the actions behind: close, reopen and setting the parent.
   *  It names actions, never a property (R11 bans "more"). */
  issue_actions: "Issue actions",
  /** Agrees with an agent's decision as its node draws it, which takes it out of what waits
   *  for the reader's review (R7). */
  keep_decision: "Keep",
  /** Keeps a node's body open once the focus leaves it (R2); the focused node's body is open
   *  without it. */
  keep_open: "Keep open",
  /** Lets a node kept open close again (R2). */
  let_close: "Let it close",
  /** What the shell's indicator calls a write of where this issue's messages go: "Route of
   *  CORE-1 saved". It is the noun the editor's own Save route and the row's No route carry,
   *  so the line names what the reader just pressed. The box itself is named by the label it
   *  is drawn under, `PROPERTY_LABELS.route` ("Messages go to"), so a voice user says the
   *  words they read (WCAG 2.5.3). */
  route: "Route",
  /** The word the name-a-version control shows; `name_version` is its accessible name (R16). */
  name: "Name",
  /** Gives the open document's current text a version of its own (R16). */
  name_version: "Name version",
  /** Makes a document of the issue from a title typed, from the node menu (R16): the words the
   *  Artifacts panel's own control carries (`CreateDocument.tsx`), the one way into one form. */
  new_document: "New document",
  /** Moves to the next open question that waits on the reader (R6). */
  next_question: "Next open question",
  /** What the repository control says while neither the issue nor its project names one. */
  no_repository: "No repository",
  /** What the route control says while the issue has none to show. */
  no_route: "No route",
  /** What one side of a plan task's pointers says while it names nothing (R4). */
  none: "none",
  /** Marks a document or an artifact in the node menu that no block cites (R16). */
  nothing_cites_it: "Cited by nothing",
  /** What the claim says of a holder the agent registry no longer lists (R11, row four). */
  not_running: "not running",
  /** The node menu beside the presets, holding what no block cites and the upload (R16). */
  other_nodes: "Files and documents",
  /** The row of five queries over the one tree, as a reader of it hears it named (R3). */
  presets: "Issue presets",
  /** Hands a claimed issue back, so another agent can take it (R11, row four). */
  release_claim: "Release",
  /** What reopening does, as the control's own words rather than a tooltip (R11). */
  reopen_into_backlog: "Reopen into Backlog",
  /** Why the repository shown may not be the issue's own, said beside the control rather than
   *  in a tooltip (R11, R21). */
  repository_inherited: "inherited or the project's default",
  /** The heading of the requirements the spec marks, under the document itself (R14). */
  requirements: "Requirements",
  /** Each editor's save, naming the field it writes. */
  save_blocked_by: "Save blocked by",
  save_parent: "Save parent",
  save_route: "Save route",
  /** Sends the change a reader wrote against a decision, named apart from the conversation's
   *  own Send so a page drawing both offers two controls a reader can tell apart (R7). */
  send_change: "Send change",
  /** What a control says while its own write is out: Dispatch has not answered, so none of
   *  these says the thing is done. */
  saving_close: "Closing…",
  saving_field: "Saving…",
  saving_labels: "Saving labels…",
  saving_release: "Releasing…",
  saving_reopen: "Reopening…",
  /** How many checks serve a requirement, which the coverage read counts without naming them
   *  (R4, until the chain read lands). */
  serving_checks: "Checks serving it",
  /** How many plan tasks serve a requirement, counted the same way (R4). */
  serving_plan_tasks: "Plan tasks serving it",
  /** Gives an issue with no parent one (R11, row one). */
  set_parent: "Set parent issue",
  /** Ends that comparison and draws the pinned version itself (R16). */
  show_version: "Show version",
  /** The whole subtree done of total, labelled beside the direct count (R10). */
  subtree_done: "Whole tree done",
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
  /** Every version an artifact has (R16). */
  versions: "Versions",
  /** The head's second row: where the checks say what the reader will be shown (R5). */
  what_you_will_be_shown: "What you will be shown",
  /** Names the author of a node whose author the model holds (R7); the name itself is the
   *  author's own text. */
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
 * recorded result says of itself (`features/issue/check-facts.ts`). A check's own attributes
 * keep the labels their document's header draws them under, so a check reads the same in the
 * tree and in the document it lives in, and only what the schema has no name for is here.
 *
 * A label is lowercase, like the attribute labels it sits beside, and the row draws the colon.
 */
export const NODE_FACT_LABELS = freeze({
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
 * The `data-glyph` token of each shape the page draws: one per node type, one per state of the
 * three types that have states, and one for an author. No two types share a token and no two
 * states of one type's surface share one, so a reader who tells shapes apart tells the nodes
 * apart; a state's token carries its type, which keeps the check's seven states distinct from
 * the ask's own as well, including the `retracted` both of them hold.
 *
 * The four ways an ask is closed carry four tokens drawing the one closed shape
 * (`NodeGlyph.tsx`): to a reader a declined, a retracted, a withdrawn and a plainly resolved
 * question are all shut, and the word beside the shape is what tells them apart. The tokens
 * stay distinct because a shape carries its own accessible name, and a declined ask's shape
 * may not announce itself as a resolved one.
 *
 * `author` is one token for every author (R7), drawn beside the author's own id: a node carries
 * an author's id and nothing else, so a shape per kind of author would be the page asserting a
 * kind the model does not hold. It waits on whatever gives a node's author a kind.
 */
export const GLYPH_TOKENS = freeze({
  author: "author",
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
      replaced: "decision-replaced",
      waiting_review: "decision-waiting-review",
      yours: "decision-yours",
    },
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
  readonly author: string;
  readonly states: {
    readonly ask: Record<AskState, string>;
    readonly check: Record<CheckState, string>;
    readonly decision: Record<DecisionState, string>;
  };
  readonly types: Record<NodeType, string>;
});

/** Every type whose state the page draws, which is every key of `NODE_STATE_WORDS`. */
const STATE_TYPES = freeze(keysOf(NODE_STATE_WORDS));

function glyphNames(): Readonly<Record<string, string>> {
  const names: Record<string, string> = {};
  for (const type of NODE_TYPES) {
    names[GLYPH_TOKENS.types[type]] = NODE_TYPE_NAMES[type];
  }
  for (const type of STATE_TYPES) {
    const words: Readonly<Record<string, string>> = NODE_STATE_WORDS[type];
    const tokens: Readonly<Record<string, string>> = GLYPH_TOKENS.states[type];
    for (const state of Object.keys(words)) {
      const token = tokens[state];
      if (token !== undefined) names[token] = `${words[state]} ${NODE_TYPE_NAMES[type]}`;
    }
  }
  // The author's shape says what the id beside it is, in the words the page names an author
  // with everywhere else (`CONTROL_LABELS.written_by`), so a reader who cannot see the shape
  // hears "Written by" and then the id.
  names[GLYPH_TOKENS.author] = CONTROL_LABELS.written_by;
  return names;
}

/**
 * The accessible name of each glyph, by its token: a shape carries its own name, because a
 * reader who cannot see the shape still has to hear what it says. A type's shape is named by
 * the type; a state's shape by its state word and the type it stands on, as in "Passed check";
 * the author's shape by the page's own words for who wrote a node.
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
 * Everything the page's own chrome may say, as the declarations above say it. `DECLARED_WORDS`
 * is derived from this list and from nothing else, so a word added to a name, a label or a line
 * joins the set by itself, and a word added nowhere is not in it.
 */
const WORD_SOURCES: readonly unknown[] = [
  NODE_TYPE_NAMES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  CONVERSATION_STATE_NAMES,
  ISSUE_STATUS_WORDS,
  EMPTY_STATE_LINES,
  KEY_REPORTS,
  GAP_LINES,
  GAP_WORDS,
  PROGRESS_NOUNS,
  READING_LINES,
  CHECK_WHEN_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  WAITING_LINES,
  BLOCKED_BY_REFUSALS,
  APPROVAL_STATE_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  MARGIN_SECTION_NAMES,
  HELP_LINES,
  BLOCK_ATTRIBUTE_LABELS,
  NODE_FACT_LABELS,
];

/**
 * Every word the page's own chrome may show, lowercase and sorted, with no word that appears in
 * none of the declarations above. A reader of the page checks the text of each
 * `[data-page-chrome]` element against this list, so an internal word drawn where a name
 * belongs has nowhere to hide.
 */
export const DECLARED_WORDS: readonly string[] = freeze(
  [...new Set(WORD_SOURCES.flatMap(phrasesIn).flatMap(wordsIn))].sort()
);
