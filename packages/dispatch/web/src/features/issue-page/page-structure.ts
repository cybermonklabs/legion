/**
 * The issue page's structural vocabulary: what it calls each property of an issue, what it calls
 * each section of its panel, the sentences it draws where a tooltip used to hold the meaning,
 * and the two marks it divides its own words from an author's with (dispatch://INFRA-1196 R11,
 * R21).
 *
 * It sits beside `page-words.ts`, which names the things the page draws - a node's type, its
 * state, a preset, a control - and derives `DECLARED_WORDS` from both modules, so a label added
 * here joins the page's declared vocabulary by itself. Nothing here imports that module, which
 * is why the derivation can run one way.
 *
 * This module is data. It holds no React, touches no DOM and imports nothing.
 */

/**
 * Freezes `value` and everything it holds. Each declaration in this module and in `page-words.ts`
 * is `as const`, which is the compiler's half of the promise; this is the runtime half, so no
 * caller can write a word into the page's vocabulary after the module loads.
 */
export function freeze<T>(value: T): T {
  for (const held of Object.values(value as Record<string, unknown>)) {
    if (typeof held === "object" && held !== null) {
      freeze(held);
    }
  }
  return Object.freeze(value);
}

/** The attribute every element carries whose visible text is the page's own vocabulary. */
export const PAGE_CHROME_ATTRIBUTE = "data-page-chrome";

/**
 * The attribute every element carries whose visible text is not the page's own vocabulary: what
 * a person or an agent wrote - a title, a name, a node's own text, a value - and what another
 * surface the page hosts draws in its own words, such as a document's rendered body on its
 * stage (`document-stage.tsx`) or the margin's own cards inside the panel (`IssueMargin.tsx`).
 * The page declares the words it says itself, and marks the rest as text it did not author.
 */
export const AUTHOR_TEXT_ATTRIBUTE = "data-author-text";

/** The mark an element wears whose text is the page's own vocabulary: `<span {...pageChrome}>`,
 *  so the attribute is spelled once. */
export const pageChrome: Readonly<Record<string, string>> = freeze({
  [PAGE_CHROME_ATTRIBUTE]: "",
});

/** The same, for text the page did not author (`AUTHOR_TEXT_ATTRIBUTE`). */
export const authorText: Readonly<Record<string, string>> = freeze({
  [AUTHOR_TEXT_ATTRIBUTE]: "",
});

/**
 * The name of every property the page draws beside its value, in the identity block and in the
 * panel's sections: the fewest plain words that read on their own, because the value beside it
 * is the author's and carries `AUTHOR_TEXT_ATTRIBUTE`.
 */
export const PROPERTY_LABELS = freeze({
  /** The priorities the issue advances, its own and those an issue above it states
   *  (dispatch://INFRA-2947 R4, R16), which the panel's Development section draws with the
   *  control that adds and removes one (dispatch://INFRA-1196 R13). */
  advances: "Advances",
  /** The documents of the issue a reviewer approves, each with how it stands and why a
   *  reviewer sent it back (R13, R31): Details' row of the approval steps and their reasons. */
  approval: "Approval",
  assignee: "Assignee",
  /** The issues this one waits on (`blocked_by`). */
  blocked_by: "Blocked by",
  /** The issues that wait on this one: the other direction of `blocked_by`, which Dispatch
   *  reads as the references of kind `blocked_by` that point at the issue. */
  blocks: "Blocks",
  /** Who is working the issue, which is not where its messages go. */
  claim: "Claimed by",
  /** Everyone who worked the issue, running or not, which the panel opens on request. */
  collaborators: "Collaborators",
  components: "Components",
  /** The issue's links out of Dispatch other than a pull request. */
  external_links: "Links",
  /** Every document and file of the issue, each opening its own address (R13). */
  files: "Files and documents",
  /** The heading of the panel's facts of the node the reader's focus is on (R12). A node is a
   *  row to the reader, the word the tree's own keys use (`useTreeKeymap.ts`). */
  focused_node: "The row you are on",
  labels: "Labels",
  opened_by: "Opened by",
  parent: "Parent",
  priority: "Priority",
  /** The issue's pull requests, each with its state (R13). */
  pull_request: "Pull request",
  referenced_by: "Referenced by",
  repository: "Repository",
  /** Where a message on this issue goes, said rather than named (R11). */
  route: "Messages go to",
  status: "Status",
  subscribers: "Subscribers",
  /** The checklist of the issue's spec, its Markdown task list: how many of its items are
   *  checked, which the header drew as a chip before Details held it (R13). */
  tasks: "Checklist",
  /** The issue's own title, which the identity block draws as itself; the name is what the
   *  shell's indicator calls a write of it. */
  title: "Title",
  /** The header's count of what waits on the reader, and the indicator of whose turn the issue
   *  is when nothing waits on them (R13). */
  waiting_on_agents: "Waiting on agents",
  waiting_on_you: "Waiting on you",
} as const);

/** The panel's three property sections (R12), in the order it draws them. */
export const MARGIN_SECTION_ORDER = freeze(["development", "people", "references"] as const);

/** One section of the panel's properties, by its own token, which is its `data-margin-section`. */
export type MarginSection = (typeof MARGIN_SECTION_ORDER)[number];

/** The name of each panel section: what the properties inside it are about. */
export const MARGIN_SECTION_NAMES = freeze({
  development: "Development",
  people: "People",
  references: "References",
} as const satisfies Record<MarginSection, string>);

/**
 * What the page says of a box once the reader opens its editor, and never on load: what the
 * box takes, as its description and as the refusal of a draft Dispatch would not take. One
 * sentence each. None explains what a control does or what Dispatch cannot do: the control's
 * own label says the one, and the other is no line the page draws (R11, R22). It is one of the
 * page's answers to the reader's own act (`ANSWER_SOURCES`, `page-words.ts`).
 */
export const HELP_LINES = freeze({
  /** What the blocked-by editor takes, as the box's description (`BlockedByControl.tsx`):
   *  issues with a space or a comma between them, which is what `issueKeysIn` splits on. */
  blocked_by:
    "Type the issues this one waits on, with a space or a comma between them, or save an empty box to remove them all.",
  /** What the route box takes, beside it (`IssueFieldForms.tsx`): drawn as a refusal while the
   *  box holds a route Dispatch would not take, and read as the box's description otherwise.
   *  It says in words what `useIssueDrafts.ts`'s `routePattern` accepts - `role:` and a name of
   *  lowercase letters, digits and hyphens, or `session:` and a session's id - and never draws
   *  the pattern itself (R21). */
  route_shape:
    "Type role: and a role name of lowercase letters, digits and hyphens, such as role:reviewer, or session: and an agent's session id, or leave the box empty to remove the route.",
} as const);

/**
 * What the page says in its own `role="status"` when a key has nowhere to go (R9, R21). A key
 * that reports is a key that did nothing: the line says where the key could not go, so a
 * reader on the keyboard is never left wondering whether the page took the press at all. It is
 * an answer to the reader's own press, and never drawn on load (`ANSWER_SOURCES`).
 */
export const KEY_REPORTS = freeze({
  /** The next-question key, with no open question after the node the reader is on (R6). */
  no_next_question: "No further open question.",
  /** The decision key, on a node no decision of this issue names. */
  no_decision: "No decision is recorded behind this row.",
  /** The edit key, on a node whose text no document of this issue holds. */
  no_text: "This row has no text to put the caret in.",
  nothing_under: "Nothing is under this row.",
  parent_zoomed: "The tree is already zoomed to this row's parent.",
  editor_closed: "This row's editor could not open.",
  /** The key that zooms back out, with the whole issue already drawn. */
  whole_tree: "The tree is already showing the whole issue.",
  /** What the row an address lands on says where the address names a turn of a thread only
   *  agents wrote in, which the outline draws no row for: the row is the turn's nearest drawn
   *  ancestor (`address-target.ts`'s `useAddressFallback`). An answer to the reader's own
   *  following of that address, never drawn on a plain load. */
  agents_thread: "This thread is the agents' alone.",
} as const);
