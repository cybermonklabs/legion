/**
 * The draft matrix's coverage list, the new issue page's box: each composer census item that
 * `draft-matrix.issue-page-outline.test.tsx` answers for, held to one of its tests or to one of
 * the numbered reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The write line and the edit line as the matrix file's titles name them. */
const HOST = "box";
const EDITING = "box editing a comment";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. Elsewhere needs a held refusal; the typed version is the Reply-on-another-row row, where a reply typed for one row is kept for it and its line names it.",
  2: "2. The box draws no Close (it is no anchored composer), no issue picker (a control its host counts as the reader's own move, `ownMoves`, is an Agents row's) and no phone thread view with a Back (it is the same one line at every width), so it makes none of those moves; its own controls are the target line's control and Escape, the own-move rows.",
  3: "3. `ProvidersSlot` draws the page's writer (`OutlineWriterProvider`) above every slot the page draws, the foot's among them, so the box always has a writer, and this return ends no mount.",
  4: "4. `IssueOutline` draws an error or a loading line in place of the slots while the issue's read fails or waits, which ends the box's mount as leaving the issue does; the box drawn again once the read lands is the one the issue-left rows draw again on return.",
  5: "5. An address under `/issues/` that parses as no issue draws the not-found page in place of the issue's (`route === undefined`), so no box is drawn there, and going there from an issue is leaving the issue, the issue-left rows.",
  6: "6. `IssuePage` draws today's page in place of the outline page when the reader's stored page choice says so (`useIssuePageChoice`), which ends the box's mount as leaving the issue does; choosing the outline again draws the box afresh, as the issue-left rows' return does.",
  7: "7. At 760 px and below the box's lines say their shorter placeholder words (`place.phone`), and that is all a width changes: the composer stays mounted at its address, with its draft and its send, so no width moves a draft.",
  8: "8. The box is drawn whenever the writer edits a comment (`useComposerShown`), so this section never hides a mounted edit line: it hides the box only once the edit has ended, which takes the edit line off the page, the edit rows.",
  9: "9. `place` tells the edit line where to put the reader once the edit ends, and its placeholder's words; neither moves the edit's address, which its comment names (`edit`), nor its draft.",
  10: "10. A refused or stopped send holds the box on its own target (the writer's `redirectedReplyTarget` and `keptOnTarget`), so the box keeps nothing away for that target and has no kept line whose way back can go; only a typed draft is kept for a target the box moved away from, the typed cells of the gone rows.",
  11: "11. While the box writes a criterion, this `div` hides the write line and unmounts nothing (`hidden`), so the write line's composer stays mounted at its address with its draft and its send, and shows again as it was once the criterion's line ends (`clear`). No row offers Write a criterion before the page reads the issue's tree (`takesCriterion`), so no row of this file can aim the box at one.",
} as const;

const WRITE_MOUNT = "WriteLine <MentionComposer mutationKey={writer.sendKey}>";
const EDIT_MOUNT = "EditLine <MentionComposer mutationKey={mutationKey}>";

/** The two rows whose typed cell takes away the target a draft is kept for. */
const GONE_ROWS = [
  ...cells(`${HOST} · a reply kept for a row, then a read drops that row`, ["typed"]),
  ...cells(`${HOST} · a statement's comment kept, then a new version rewrites it`, ["typed"]),
];

export const coverage: readonly CoverageEntry[] = [
  // The write line's composer.
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | address prop | kind={writesKinds ? writer.kind : "comment"}`,
    cells(`${HOST} · Question, then Comment again`),
    cells(`${HOST} · the issue closed, then open again`)
  ),
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | address prop | mutationKey={writer.sendKey}`,
    cells(`${HOST} · issue left, then back`),
    { reason: REASONS[2] }
  ),
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | address prop | outline={look}`,
    cells(`${HOST} · a comment on a statement, Reply on another row, then back`),
    cells(`${HOST} · Write on the issue instead`),
    cells(`${HOST} · Escape over a reply`),
    cells(`${HOST} · Write on the issue instead, the issue keeping a comment of its own`),
    GONE_ROWS
  ),
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | address prop | owner={owner}`,
    cells(`${HOST} · issue left, then back`)
  ),
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | address prop | replyTo={reply}`,
    cells(`${HOST} · Reply on another row, then back`),
    cells(`${HOST} · host moves it over a refusal (elsewhere), then back`, ["refused", "stopped"]),
    { reason: REASONS[1] },
    cells(`${HOST} · Write on the issue instead`),
    cells(`${HOST} · Escape over a reply`),
    cells(`${HOST} · Write on the issue instead, the issue keeping a comment of its own`)
  ),
  ...answers(`WriteLine | ${WRITE_MOUNT} | form | outline={look}`, { reason: REASONS[7] }),
  ...answers(
    `WriteLine | ${WRITE_MOUNT} | lifetime | <MentionComposer mutationKey={writer.sendKey}>`,
    cells(`${HOST} · the page reloaded, then back`)
  ),

  // The edit line's composer.
  ...answers(
    `EditLine | ${EDIT_MOUNT} | address prop | edit={editing}`,
    cells(`${EDITING} · a Reply on another row, then Edit again`),
    cells(`${EDITING} · Edit on another comment, then back`)
  ),
  ...answers(
    `EditLine | ${EDIT_MOUNT} | address prop | mutationKey={mutationKey}`,
    cells(`${EDITING} · Edit on another comment, then back`),
    cells(`${EDITING} · issue left, then Edit again`),
    { reason: REASONS[2] }
  ),
  ...answers(
    `EditLine | ${EDIT_MOUNT} | address prop | outline={look}`,
    cells(`${EDITING} · Escape, then Edit again`)
  ),
  ...answers(
    `EditLine | ${EDIT_MOUNT} | address prop | owner={owner}`,
    cells(`${EDITING} · issue left, then Edit again`)
  ),
  ...answers(`EditLine | ${EDIT_MOUNT} | form | outline={look}`, { reason: REASONS[7] }),
  ...answers(
    `EditLine | ${EDIT_MOUNT} | lifetime | <MentionComposer mutationKey={mutationKey}>`,
    cells(`${EDITING} · the page reloaded, then Edit again`)
  ),

  // The box, above the write line.
  ...answers(
    "IssueComposer | WriteLine | conditional | <div hidden={!writing}>",
    cells(`${HOST} · an edit opened, then ended`)
  ),
  ...answers(
    "IssueComposer | WriteLine | conditional | <div hidden={writer.criterion !== undefined}>",
    {
      reason: REASONS[11],
    }
  ),
  ...answers(
    "IssueComposer | WriteLine | conditional | <section hidden={!shown}>",
    cells(`${HOST} · a view, then back`),
    cells(`${HOST} · a document opened, then back`)
  ),
  ...answers("IssueComposer | WriteLine | early return | if (writer === undefined) return …", {
    reason: REASONS[3],
  }),
  ...answers(
    "IssueComposer | WriteLine | address prop | isClosed={isClosed}",
    cells(`${HOST} · the issue closed, then open again`)
  ),
  ...answers(
    "IssueComposer | WriteLine | address prop | issueKey={issueKey}",
    cells(`${HOST} · issue left, then back`)
  ),
  ...answers(
    "IssueComposer | WriteLine | address prop | place={place}",
    cells(`${HOST} · a view, then back`),
    cells(`${HOST} · a document opened, then back`)
  ),
  ...answers(
    "IssueComposer | WriteLine | address prop | writer={writer}",
    cells(`${HOST} · Reply on another row, then back`),
    cells(`${HOST} · an edit opened, then ended`)
  ),
  ...answers("IssueComposer | WriteLine | address prop | tree={tree}", GONE_ROWS, {
    reason: REASONS[10],
  }),
  ...answers("IssueComposer | WriteLine | form | place={place}", { reason: REASONS[7] }),

  // The box, above the edit line.
  ...answers(
    "IssueComposer | EditLine | key | <EditLine key={writer.editing.id}>",
    cells(`${EDITING} · Edit on another comment, then back`)
  ),
  ...answers(
    "IssueComposer | EditLine | conditional | !(writer.editing === undefined) ? …",
    cells(`${EDITING} · a Reply on another row, then Edit again`),
    cells(`${EDITING} · Escape, then Edit again`)
  ),
  ...answers("IssueComposer | EditLine | conditional | <section hidden={!shown}>", {
    reason: REASONS[8],
  }),
  ...answers("IssueComposer | EditLine | early return | if (writer === undefined) return …", {
    reason: REASONS[3],
  }),
  ...answers(
    "IssueComposer | EditLine | address prop | editing={writer.editing}",
    cells(`${EDITING} · a Reply on another row, then Edit again`),
    cells(`${EDITING} · Edit on another comment, then back`)
  ),
  ...answers(
    "IssueComposer | EditLine | address prop | issueKey={issueKey}",
    cells(`${EDITING} · issue left, then Edit again`)
  ),
  ...answers("IssueComposer | EditLine | address prop | place={place}", { reason: REASONS[9] }),
  ...answers(
    "IssueComposer | EditLine | address prop | writer={writer}",
    cells(`${EDITING} · a Reply on another row, then Edit again`)
  ),
  ...answers("IssueComposer | EditLine | form | place={place}", { reason: REASONS[7] }),

  // The foot slot and the page above the box.
  ...answers(
    "FootSlot | IssueComposer | address prop | documentOpen={place.documentOpen}",
    cells(`${HOST} · a document opened, then back`)
  ),
  ...answers(
    "FootSlot | IssueComposer | address prop | isClosed={issue.closed_at !== null}",
    cells(`${HOST} · the issue closed, then open again`)
  ),
  ...answers(
    "FootSlot | IssueComposer | address prop | issueKey={issue.key}",
    cells(`${HOST} · issue left, then back`),
    cells(`${EDITING} · issue left, then Edit again`)
  ),
  ...answers(
    "FootSlot | IssueComposer | address prop | viewOn={place.viewOn}",
    cells(`${HOST} · a view, then back`)
  ),
  ...answers(
    "FootSlot | IssueComposer | address prop | tree={readsHeld && namesHeld ? root : undefined}",
    GONE_ROWS,
    { reason: REASONS[10] }
  ),
  ...answers(
    "IssueOutline | FootSlot | address prop | <FootSlot {...props}>",
    cells(`${HOST} · issue left, then back`),
    cells(`${HOST} · a view, then back`),
    cells(`${HOST} · a document opened, then back`)
  ),
  ...answers(
    "ProvidersSlot | FootSlot | key | <FocusedNodeProvider key={issue.key}>",
    cells(`${HOST} · issue left, then back`),
    cells(`${EDITING} · issue left, then Edit again`)
  ),
  ...answers("IssueOutline | FootSlot | early return | if (issue.isError) return …", {
    reason: REASONS[4],
  }),
  ...answers("IssueOutline | FootSlot | early return | if (issue.data === undefined) return …", {
    reason: REASONS[4],
  }),
  ...answers(
    "IssueOutlinePage | IssueOutline | key | <IssueOutline key={route.key}>",
    cells(`${HOST} · issue left, then back`),
    cells(`${EDITING} · issue left, then Edit again`)
  ),
  ...answers("IssueOutlinePage | IssueOutline | conditional | !(route === undefined) ? …", {
    reason: REASONS[5],
  }),
  ...answers(
    "IssueOutlinePage | IssueOutline | address prop | route={route}",
    cells(`${HOST} · issue left, then back`),
    cells(`${HOST} · a view, then back`),
    cells(`${HOST} · a document opened, then back`)
  ),
  ...answers(
    'IssuePage | IssueOutlinePage | conditional | (useIssuePageChoice()[0] === "outline") ? …',
    { reason: REASONS[6] }
  ),
];
