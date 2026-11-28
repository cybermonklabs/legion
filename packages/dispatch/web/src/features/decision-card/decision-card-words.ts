/**
 * The decision card's words (dispatch://INFRA-5702/spec#b-r-two-lists): every word the decision
 * card draws, the same on every surface. The card reads its words from here, so a test, a browser
 * spec and the card read one list, and the issue page's list can take this one in.
 *
 * Each table is frozen, and so is each pair in it. A pair is the words on either side of a value
 * the card draws between them (`before`, `after`). The card adds the spaces between its words and
 * its values where it draws them.
 *
 * This file, as every file under `features/decision-card/`, imports no file under
 * `features/issue-page/` (R22).
 */

/** Who made the decision: the kind words at the right end of the card's first line. */
export const DECISION_KIND_WORDS = Object.freeze({
  agent: "Agent's decision",
  person: "A person's decision",
} as const);

/** The words of the card's author line and its level of uncertainty (R20). */
export const DECISION_LINE_WORDS = Object.freeze({
  /** The author of a decision whose read holds none, where the caller sets the author line. */
  author_unknown: "Author unknown",
  /** The level word of a decision its maker was unsure of. */
  unsure: "Unsure",
} as const);

/** How the decision stands now: the state word, and the words before the name it holds. */
export const DECISION_STATE_WORDS = Object.freeze({
  /** Before the name of the person who asked for the change the caller hands the card. */
  changed_by: "Changed by",
  /** Before the name of the person who kept the decision. */
  kept_by: "Kept by",
  replaced: "Replaced",
  waiting_review: "Waiting for review",
} as const);

/** The links the card draws: to each statement the decision shaped, and to the ask a person's
 *  decision answers. */
export const DECISION_LINK_WORDS = Object.freeze({
  /** After a statement that changed after the decision was tied to it. */
  changed_since: "Changed since",
  /** After a statement the decision changed after it was tied to it. */
  out_of_date: "out of date",
  /** A statement whose words the caller holds none of - its batched read of the statements'
   *  names has not answered, failed, or names it gone - in place of its first line. */
  statement: "A statement it shaped",
  the_ask: "The ask it answers",
} as const);

/** The card's two controls on a decision that waits for review, and the form Change opens. */
export const DECISION_ACT_WORDS = Object.freeze({
  cancel: "Cancel",
  change: "Change",
  /** Before the decision's heading in the name of a Change write (the shell's pending-writes
   *  indicator): "Change to <heading>", the words today's Decisions tab named it by on `main`. */
  change_to: "Change to",
  keep: "Keep",
  send_change: "Send change",
  /** What the card says of a write the reader stopped (`stoppedWriteNote`): the write, what it
   *  may have done, and what the control does now. These are the words today's Decisions tab
   *  said on `main`. */
  stopped: Object.freeze({
    change: "your change",
    change_anew: "Send posts it again as a new comment.",
    change_done: "posted",
    /** Around the decision's heading: the Keep of “<heading>”. */
    keep: Object.freeze({ after: "”", before: "the Keep of “" } as const),
    keep_anew: "Pressing Keep again sends a new Keep.",
    keep_done: "made",
  } as const),
  your_change: "Your change",
} as const);
