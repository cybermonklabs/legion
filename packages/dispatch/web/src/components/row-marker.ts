/**
 * What a row of a table or list is, for the page's focus rescue (`ROW_SELECTOR`): an item of a
 * list or a table by its markup - an `<li>`, a `<tr>`, an `<article>`, an element of role `row`
 * or `listitem` - or an element that carries the shared marker below. HTML defines an
 * `<article>` as a self-contained composition - a post, a comment, a card - so a list of cards
 * drawn as `<article>`s is a list of rows with nothing added, as a list drawn as `<li>`s and a
 * table drawn as `<tr>`s are: every `<tr>` the shared tables draw (`DataTable`, `TableShell`, and
 * the rows their callers draw), header, spacer, group and empty lines included; every `<ul>`,
 * `<ol>` and table a page draws; and every card drawn as an `<article>` - the Agents page's rows
 * and its ended agents, the Architecture page's rows, the Board's cards, the Broadcast page's
 * recipients, the margin's threads, decisions and pinned cards, and every ask card, open or
 * answered.
 *
 * The marker is for a row whose list markup would change how a screen reader reads the page. One
 * row carries it: a send the reader made that Dispatch has not yet made (`PendingSendTurn`), a
 * turn of the session's transcript in its live view, whose stored turns are no list, and the same
 * turn above the composer in its Agents row. A page's own row attribute (`data-agent-row`,
 * `data-board-card`, ...) stays the page's, for its own keys and tests; the marker says only that
 * the element is a row.
 *
 * Rows nest - a reply's `<li>` inside a thread's `<article>` inside a Conversation turn's `<li>`,
 * an ask card inside an Inbox row - and each row around a control, itself included, is that
 * control's row: the nearest one, and every row around that one. A table in a document's or a
 * comment's prose is a table too, so its `<tr>`s are rows, and the `<article>` a document is drawn
 * in (`ProofDocument`, `VersionView`) is a row around every control of the document.
 *
 * The rescue reads rows (`focus-place.ts`) to tell whether the reader caused a loss - a press of
 * theirs inside the row of the control that lost the focus makes the loss theirs, and a press on
 * the one checkbox a row draws is a press on its mark - and to land a loss they caused: the same
 * control in the row that moves up into the place of the row a press took away takes the focus. A
 * loss the reader did not cause leaves the focus on the body, or on an open modal dialog's own
 * stop, whatever rows stand near it (`leaveUncaused`), so no row decides where it lands.
 */
export const ROW_MARKER_ATTRIBUTE = "data-row-marker";

/** The marker as props for a row's element to spread where the row is drawn, on an element whose
 *  list markup would change how a screen reader reads the page: `<div {...ROW_MARKER}>`. */
export const ROW_MARKER = { [ROW_MARKER_ATTRIBUTE]: "" } as const;

/** Every element that is a row: an item of a list or a table by its markup, or one that carries
 *  the marker. */
export const ROW_SELECTOR = [
  "li",
  "tr",
  "article",
  '[role="row"]',
  '[role="listitem"]',
  `[${ROW_MARKER_ATTRIBUTE}]`,
].join(", ");
