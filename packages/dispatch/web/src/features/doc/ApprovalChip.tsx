import { dispatchWritingBox } from "@legion/contracts";
import { type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";

import {
  api,
  apiErrorCodeIn,
  type CreateArtifactReviewInput,
  isCapExceeded,
} from "../../api/client";
import { inboxQuery } from "../../api/queries";
import { type ListReadBound, useListReadBound } from "../../api/read-bound";
import type { Artifact, ArtifactApproval } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { approvalPillClassName } from "../../components/Pill";
import { usePageWords, useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop40,
  card,
  dangerText,
  focusBorder,
  focusVisibleRing,
  pageFailedText,
  pageHairBorder,
  pageHeaderControl,
  pageInkBorder,
  pageInkFill,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionField } from "../conversation/MentionField";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { focusOnDocument } from "../shell/roving";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { useDialog } from "../shell/useDialog";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite, usePendingKeyedPress } from "../writes/useKeyedWrite";

/**
 * Each colour the chip, its two actions and its two dialogs draw inside `NoTooltips`, the new
 * issue page's, by the role `main`'s colour has: a dialog is the page's paper inside its hairline
 * rule over the icon rail's dark, the one page colour dark in both schemes; its heading and a
 * review are the ink, the reviewer's reason and a label the muted grey, Close, a pending read and
 * the line under the reason box the quiet grey, a refusal the failed red; Approve and the
 * dialog's Request changes are filled with the ink, as the header's one filled control is, and
 * the other actions are the header's control. The reason box keeps `main`'s focus rule, the
 * application's one focus indicator.
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page.
 */
const ON_PAGE = {
  action: `${pageLineBorder} ${pagePaperBg} ${pageMutedText}`,
  backdrop: "bg-icon-rail/40",
  dialog: `${pageHairBorder} ${pagePaperBg}`,
  dismiss: pageQuietText,
  failure: pageFailedText,
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  heading: pageInkText,
  hint: pageQuietText,
  label: pageMutedText,
  pending: pageQuietText,
  reason: pageMutedText,
  reasonBox: `${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusBorder}`,
  review: pageInkText,
} as const;

/** The version an approval speaks for: `version` when the review named one, else the document's
 *  latest. */
export function approvalVersion(approval: ArtifactApproval): number {
  return approval.version ?? approval.latest_version;
}

/** One review the reader pressed: the document it is of, what it says of that document, and the
 *  version of the document the page read, which is the version it approves or asks changes of. A
 *  Retry sends that same version again. */
interface ReviewPress {
  readonly artifact: Artifact;
  readonly input: CreateArtifactReviewInput;
  readonly version: number;
}

/** Dispatch's refusal of a review whose version moved after the page read it (`409
 *  REVIEW_VERSION_MOVED`): its words name the version a review would now record. */
const VERSION_MOVED: ReadonlySet<string> = new Set(["REVIEW_VERSION_MOVED"]);

/** What the chip says beside its actions, before why the reads its landed review made stale have
 *  not settled once their hold ran out (`reviewReads`). */
const APPROVAL_NOT_READ = "The new approval is not read yet.";

/** The reads each page draws `reviewed`'s approval from: its own (the Inbox's reading pane), and
 *  its owner's - an issue document's issue and that issue's documents (both issue pages' header,
 *  the Artifacts tab), a project document's project documents and its read by reference. */
function approvalReads(reviewed: Artifact): readonly QueryKey[] {
  return [
    ["artifact", reviewed.id],
    ...(reviewed.issue_key === null
      ? [
          ["project", reviewed.project, "artifacts"],
          ["artifact-ref", `${reviewed.project}/${reviewed.slug}`],
        ]
      : [
          ["issue", reviewed.issue_key],
          ["artifacts", reviewed.issue_key],
        ]),
  ];
}

/** The chip's words. `latest` is the issue page's word for the latest version, given inside
 *  `NoTooltips` alone: there a stale chip draws the latest version, which its `title` says
 *  everywhere else. */
function approvalLabel(approval: ArtifactApproval, latest?: string): string {
  // A plan is approved by its plan review, never by a person, and its chip says so.
  const approved = approval.kind === "plan_review" ? "Approved by plan review" : "Approved";
  const version = approvalVersion(approval);
  switch (approval.state) {
    case "draft":
      return "Draft";
    case "awaiting":
      return `Awaiting approval${approvalScope(approval)}`;
    case "approved":
      return `${approved} v${version}`;
    case "stale":
      return latest === undefined
        ? `${approved} v${version} · changed since`
        : `${approved} v${version} · ${latest} v${approval.latest_version}`;
    case "changes_requested":
      return "Changes requested";
    default:
      // The route table carries the state as a plain string. Dispatch writes one of the five
      // above; one this build does not know reads as itself, as an unknown capability does
      // (`capabilityLabel`), rather than leaving the chip blank.
      return approval.state;
  }
}

/** The documents an open request covers, when it covers more than the one being read. */
function approvalScope(approval: ArtifactApproval): string {
  const documents = approval.documents ?? [];
  if (documents.length < 2) {
    return "";
  }
  const named = documents.map((document) => `${document.name} v${document.version}`);
  return ` of ${named.slice(0, -1).join(", ")} and ${named[named.length - 1]}`;
}

function ReviewsDialog({
  artifactId,
  issueKey,
  onClose,
}: {
  artifactId: string;
  /** The issue the document is on, which each review's reason is a text of. */
  issueKey: string | null;
  onClose: () => void;
}): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  const page = usePageWords();
  const reviews = useQuery({
    queryFn: () => api.listArtifactReviews(artifactId),
    queryKey: ["artifact-reviews", artifactId],
  });

  return (
    <>
      <div
        aria-hidden="true"
        className={`fixed inset-0 z-40 ${page === null ? backdrop40 : ON_PAGE.backdrop}`}
        onClick={onClose}
      />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Reviews"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-md space-y-3 rounded-lg border p-4 shadow-xl ${page === null ? card : ON_PAGE.dialog}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className="flex items-center justify-between gap-3">
            <h2
              className={`text-sm font-semibold ${page === null ? textSecondaryOnSurface : ON_PAGE.heading}`}
            >
              Reviews
            </h2>
            <button
              className={`text-sm font-medium ${page === null ? textMutedOnSurface : ON_PAGE.dismiss}`}
              onClick={onClose}
              type="button"
            >
              Close
            </button>
          </div>
          {reviews.isPending ? (
            <p className={`text-sm ${page === null ? textMutedOnSurface : ON_PAGE.pending}`}>
              Loading reviews…
            </p>
          ) : reviews.isError ? (
            <p className={`text-sm ${page === null ? dangerText : ON_PAGE.failure}`}>
              Could not load reviews.
            </p>
          ) : reviews.data.length === 0 ? (
            <p className={`text-sm ${page === null ? textMutedOnSurface : ON_PAGE.pending}`}>
              No reviews yet.
            </p>
          ) : (
            <ul className="space-y-3">
              {reviews.data.map((review) => (
                <li key={review.id}>
                  <p
                    className={`text-sm font-medium ${page === null ? textPrimaryOnSurface : ON_PAGE.review}`}
                  >
                    <AgentName actor={review.actor} inline />{" "}
                    {review.state === "approved" ? "approved" : "requested changes on"} v
                    {review.version} · <Timestamp at={review.created_at} />
                  </p>
                  {review.reason === null ? null : (
                    <div
                      className={`mt-1 text-sm ${page === null ? textSecondaryOnSurface : ON_PAGE.reason}`}
                    >
                      <MarkdownBody
                        markdown={review.reason}
                        source={textSourceOf("artifact", artifactId, issueKey)}
                        surface="approval-review-reason"
                        tree={review.reason_tree}
                      />
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </>
  );
}

function RequestChangesDialog({
  error,
  held,
  issueKey,
  onClose,
  onSubmit,
  project,
}: {
  /** What the server answered the last request with, or null while none was refused. */
  error: Error | null;
  /** The request is out: the reason and both buttons keep the focus and their Tab stops, and
   *  send or close nothing (`heldPress`) - but for Cancel inside `NoTooltips`, which reads Close
   *  then and closes the dialog. */
  held: boolean;
  /** The issue the document under review belongs to, where it has one: the picker offers that
   *  issue's people. */
  issueKey?: string;
  onClose: () => void;
  onSubmit: (reason: string) => void;
  /** The document's project, whose components the picker offers. */
  project: string;
}): ReactNode {
  const [reason, setReason] = useState("");
  // The dialog moves the focus into the box it opens over, which the field hands back as its
  // own element (`MentionFieldHandle`).
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: textareaRef, onClose, open: true });
  const page = usePageWords();
  const trimmed = reason.trim();

  return (
    <>
      <div
        aria-hidden="true"
        className={`fixed inset-0 z-40 ${page === null ? backdrop40 : ON_PAGE.backdrop}`}
        onClick={onClose}
      />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Request changes"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${page === null ? card : ON_PAGE.dialog}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2
            className={`text-sm font-semibold ${page === null ? textSecondaryOnSurface : ON_PAGE.heading}`}
          >
            Request changes
          </h2>
          {/* While the write is out the text box is held (`heldField`): read-only, never
              `disabled`, which drops the focus to the body in a browser, and Tab would then
              leave the dialog. */}
          <MentionField
            className="text-sm aria-disabled:opacity-50"
            body={reason}
            fieldClassName={page === null ? undefined : ON_PAGE.reasonBox}
            fieldRef={(handle) => {
              textareaRef.current = handle?.textarea() ?? null;
            }}
            held={held}
            hintClassName={page === null ? undefined : `text-xs ${ON_PAGE.hint}`}
            issueKey={issueKey}
            label="Reason"
            labelClassName={
              page === null ? undefined : `block text-sm font-medium ${ON_PAGE.label}`
            }
            onChange={(change) => setReason(change.body)}
            onKeyDown={(event) =>
              submitOnModifiedEnter(event, {
                onFormlessSubmit: () => {
                  if (trimmed !== "" && !held) {
                    onSubmit(trimmed);
                  }
                },
              })
            }
            project={project}
            writing={dispatchWritingBox("approval-reason", "approval-reason")}
          />
          {error === null ? null : (
            <p className={`text-sm ${page === null ? dangerText : ON_PAGE.failure}`} role="alert">
              {/* A reason Dispatch refused for its length says so in its own words, which name
                  the block and the bound, and so does a version that moved after the page read
                  it, naming the version a review would now record; a review the reader stopped
                  may have been made, and Request changes is a press of its own; any other failure
                  is worth another try. */}
              {isCapExceeded(error) || apiErrorCodeIn(error, VERSION_MOVED)
                ? error.message
                : error instanceof SendStoppedError
                  ? stoppedWriteNote("your review", "made", {
                      anew: "Request changes sends a new review.",
                    })
                  : "Could not request changes. Try again."}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              // Inside `NoTooltips`, while the write is out, this button reads Close and closes the
              // dialog as Escape and the backdrop do: `onClose` leaves the write going, and the
              // dialog gives the focus back to the button that opened it. Once the write has ended
              // it is Cancel again. Outside it is held while the write is out (`heldPress`): it
              // keeps the focus and closes nothing.
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${page === null ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}` : ON_PAGE.action}`}
              {...heldPress(page === null && held, onClose)}
              type="button"
            >
              {page !== null && held ? page.CONTROL_LABELS.close : "Cancel"}
            </button>
            <button
              // Held while the write is out (`heldPress`), so the focus the reader's press left
              // on it stays there; `disabled` only while there is no reason to send.
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${page === null ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}` : ON_PAGE.filled}`}
              disabled={trimmed === ""}
              {...heldPress(held, () => onSubmit(trimmed))}
              type="button"
            >
              {held ? "Submitting…" : "Request changes"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

/** The keys that press the chip's controls where its host binds them (the Inbox's `a` and
 *  Shift+R, or the person's own): Approve, Request changes, and the pill, which opens the
 *  reviews. */
export interface ApprovalShortcutKeys {
  readonly approve: readonly string[];
  readonly requestChanges: readonly string[];
  readonly showReviews: readonly string[];
}

/** The box each action of the reading pane's head stands in (`pane`, and the head's Dismiss): 44
 *  px high, the height of the tallest control the head's place holds (the earlier approval's Show
 *  diff), with the button centred in it. The place lines its controls up at their tops, so a pill
 *  whose label wraps past that height grows the place downward and moves no action. */
export const paneActionBox = "flex min-h-11 shrink-0 items-center";

/** The chip's actions: `together`, in one box that does not wrap; otherwise with no box around
 *  them, each its parent's own item, as on `main` (`keepActionsTogether`). */
function ActionsGroup({
  children,
  together,
}: {
  children: ReactNode;
  together: boolean;
}): ReactNode {
  return together ? (
    <span className="inline-flex flex-nowrap items-center gap-2 whitespace-nowrap">{children}</span>
  ) : (
    children
  );
}

export interface ApprovalChipProps {
  /** The words naming the document Approve and Request changes review ("the spec"), for a host
   *  whose line carries more than one document's actions: `ApprovalSteps` passes it where
   *  `actionsOn` names two or more documents. Inside `NoTooltips` each action's name then begins
   *  with the action's own words and says its document, "Approve the spec", "Approve v5 of the
   *  spec" for a stale approval, "Request changes to the spec", so no two actions of the line
   *  share a name and a voice user says what they read (WCAG 2.5.3). Not given, the default, and
   *  everywhere outside `NoTooltips`, where no caller passes it today and the words to join them
   *  are not loaded, each action is named by its words alone, as on `main`. */
  actionsDocument?: string;
  /** The document, once the page has read it. Only the reading pane's head (`pane`) draws the
   *  chip before that: Approve and Request changes, held until the read returns; for any other
   *  variant an unread document draws nothing. */
  artifact: Artifact | undefined;
  /** `header`: shown on the document header, with an approval-state chip.
   * `pane`: the header's chip and actions in the Inbox's reading-pane head, which draws the
   * place they stand in at a fixed width from its first draw (`ReadingPaneHead`, R9): each
   * action stands in its own box (`paneActionBox`), compact like the controls beside it, Approve
   * keeps the one label it is drawn with before the read (never `Approve v<latest>`; the pill and
   * its title name the versions), and the pill takes only the room the actions leave.
   * `list` (default): shown in a compact list row. */
  variant?: "header" | "list" | "pane";
  /** The issue header exposes review actions only after an approval request exists. */
  showActions?: boolean;
  /** `contents` lets a parent flex group place the state chip and actions independently. */
  layout?: "contents" | "inline";
  /** Whether Approve and Request changes sit in one box that does not wrap, so in a parent's line
   *  that wraps the two stay together as one item beside the chip. `ApprovalSteps` passes it at
   *  `layout="contents"`, which the new issue page's header uses. Off, the default, each action is
   *  its own item, as on `main`. */
  keepActionsTogether?: boolean;
  /** `compact` draws the chip and its two actions at the size of the new issue page header's
   *  other pills and controls (the prototype's `.pill` and `.hbtn`): 12 px type, and 24 px tall at
   *  every width for the chip and for each action, since each is a target and a 24 px target
   *  holds at every width. `default` is every other host's. */
  size?: "compact" | "default";
  /** Called beside the chip's own refreshes once the server has recorded the reader's review, so
   *  a host that selected the approval request learns it was reviewed here. */
  onReviewed?(): void;
  /** The line's one hold through the reads each chip's landed review makes stale, where the line
   *  draws more than one chip with actions, whose reviews read the same issue again
   *  (`approvalReads`): the new issue page's header (`IssuePropertiesRow`, and its steps through
   *  `ApprovalSteps`), by each chip's document (`useListReadBound`). Absent, the chip holds its
   *  own. */
  reviewReads?: ListReadBound;
  /** The keys that press the pill and the two actions, where the host draws them: named in each
   *  button's `aria-keyshortcuts`, and the first drawn inside it after its label
   *  (`ControlKeyHint`). */
  shortcutKeys?: ApprovalShortcutKeys;
  /** `data-*` attributes the host writes on the state button (the pill, which opens the
   *  reviews), for its own code to find it by: the Inbox's command bar presses it by its mark. */
  stateMarks?: Readonly<Record<`data-${string}`, string>>;
}

/**
 * The document approval indicator: a small badge summarizing `artifact.approval`, clickable to
 * open a popover of every human review (`ArtifactReview`). On the document header only, it also
 * carries the Approve / Request changes actions that write a new review, of the version the page
 * read: a version that moved after the read is refused, and the refusal is shown here.
 *
 * The two actions carry `data-approval-action` (`approve`, `request-changes`), which the keys
 * that press them find them by (the Inbox's `a` and Shift+R), and the state button carries the
 * marks its host gives it (`stateMarks`), which the Inbox's command bar presses it by.
 *
 * In the reading pane's head (`pane`) one chip stands for one approval request from the head's
 * first draw: the two actions it draws, held, before the document's read returns are the
 * buttons it acts with once the read has returned, so neither moves, and a reader who reached
 * one with Tab keeps the focus on it.
 *
 * It holds the review it made and the change-request dialog, with the reason typed into it, so a
 * host that draws it for one document after another keys it by the document (`ArtifactHeader`).
 * The review goes under the document's name, so a chip drawn again while it is out, before it
 * lands - the document page's header after another document and back, the reading pane's head
 * after the selection moves off the request and back - holds its actions from that press until
 * the review's deadline, as the chip it was made on did (`usePendingKeyedPress`).
 *
 * Each review that lands reads the reviewed document's issue again, which every chip of that
 * issue draws from, so a line that draws more than one chip with actions holds them all through
 * those reads with one hold (`reviewReads`): a review that lands forgets why an earlier one's
 * reads had not settled, whichever chip's it was, and only the chip whose review made the latest
 * reads says why those have not. Every other chip holds its own (`ChipWithOwnReads`).
 */
export function ApprovalChip({ reviewReads, ...chip }: ApprovalChipProps): ReactNode {
  return reviewReads === undefined ? (
    <ChipWithOwnReads {...chip} />
  ) : (
    <ChipOfReads {...chip} reviewReads={reviewReads} />
  );
}

/** The chip where its line holds no reads for it: the chip holds its own, through its document's
 *  read, which its review reads again first (`approvalReads`). */
function ChipWithOwnReads(chip: Omit<ApprovalChipProps, "reviewReads">): ReactNode {
  const reviewReads = useListReadBound(["artifact", chip.artifact?.id]);
  return <ChipOfReads {...chip} reviewReads={reviewReads} />;
}

/** The chip itself, drawn with the hold through its reviews' reads it is handed
 *  (`ApprovalChip`). */
function ChipOfReads({
  actionsDocument,
  artifact,
  keepActionsTogether = false,
  layout = "inline",
  onReviewed,
  reviewReads: [unsettledOf, holdThrough],
  shortcutKeys,
  stateMarks,
  size = "default",
  variant = "list",
  showActions = variant !== "list",
}: ApprovalChipProps & { reviewReads: ListReadBound }): ReactNode {
  const approval = artifact?.approval;
  const [reviewsOpen, setReviewsOpen] = useState(false);
  const [requestChangesOpen, setRequestChangesOpen] = useState(false);
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const tooltips = useTooltips();
  const page = usePageWords();
  // Why the reads the latest landed review of the line made stale have not settled, once the
  // hold over them ran out, where this chip's review made them.
  const unsettled = artifact === undefined ? undefined : unsettledOf(artifact.id);

  const mutationKey = ["artifact-review", artifact?.id];
  const reviewOut = usePendingKeyedPress(mutationKey);
  const review = useKeyedWrite({
    // The press carries the document it reviews: the indicator names it, the route takes its id,
    // and the reads made again are its own, from the press rather than from the render the write
    // runs under.
    label: ({ artifact: reviewed, input }: ReviewPress) =>
      `${input.state === "approved" ? "Approval" : "Change request"} on ${reviewed.name}`,
    onError: (error, { artifact: reviewed }) => {
      // The document moved after the page read it: every read a page draws its approval from is
      // read again - the issue's own among them, which both issue pages' header draws the chip
      // from - so the next press reviews the version a review would now record, which the reader
      // then has in front of them, and never one they did not read.
      if (apiErrorCodeIn(error, VERSION_MOVED)) {
        for (const queryKey of approvalReads(reviewed)) {
          void queryClient.invalidateQueries({ queryKey });
        }
      }
    },
    onSettled: () => submitGuard.release(),
    method: "POST",
    mutationKey,
    write: ({ artifact: reviewed, input, version }, bounds) =>
      api.createArtifactVersionReview(reviewed.id, version, input, bounds),
    onSuccess: (_result, { artifact: reviewed }) => {
      // `onSuccess` returns the reads the write makes stale, which TanStack Query awaits before
      // the write stops pending, on both sides of `NoTooltips`: Approve, Request changes and the
      // dialog's submit stay refused, and keep the focus, until the new approval is read. So no
      // second press - a second `a`, a held key's repeat, a second click - records a second
      // approval while the page still draws the request as waiting. A read that has not settled
      // by the request deadline - one the network paused while the browser is offline - ends the
      // wait there, so no read holds the actions for good, and the chip says why beside them
      // until the reads settle or the next review of its line lands (`reviewReads`). Outside
      // `NoTooltips` the dialog closes as the write lands, as on `main`. Inside, it closes once
      // the reads are in, and a dialog the reader has not closed (`onClose`) then gives the focus
      // back to the button that opened it. The reads are the reviewed document's, from the press.
      if (page === null) setRequestChangesOpen(false);
      onReviewed?.();
      const rereads = holdThrough(
        reviewed.id,
        Promise.all([
          queryClient.invalidateQueries({ queryKey: ["artifact-reviews", reviewed.id] }),
          queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey }),
          ...(reviewed.issue_key === null
            ? []
            : [queryClient.invalidateQueries({ queryKey: ["asks", reviewed.issue_key] })]),
          ...approvalReads(reviewed).map((queryKey) => queryClient.invalidateQueries({ queryKey })),
        ])
      );
      return page === null ? rereads : rereads.then(() => setRequestChangesOpen(false));
    },
  });

  // Inside `NoTooltips` the review sent, by a press of Approve or by the Request changes dialog's
  // submit, is remembered until its write stops pending, which is once the new approval is read
  // (`onSuccess` returns its reads, and closes a change request's dialog before the write stops
  // pending). Approve and Request changes are never `disabled` (they are held, `heldPress`), so
  // while they are drawn they keep the focus, and the dialog gives it back to Request changes as
  // it closes: once the read is drawn, or at once when the reader closes it with Escape, the
  // backdrop or its Close while the write is out, which goes on (`onClose`). Once the write stops
  // pending with the dialog closed and the read has taken the sent review's own button off the
  // page, Approve for an approval and Request changes for a change request, a focus that fell with
  // it to the document (`focusOnDocument`) goes to the chip; a focus anywhere else is left where
  // it is. A change request refused while its dialog is open keeps the dialog open with its error,
  // and the focus in it. One refused after the reader closed its dialog moves no focus: its
  // refusal is in the shell's write indicator, and the dialog opened next shows its error, as it
  // shows any refusal of a review left over, until a submit or a close clears it. The review is
  // forgotten whenever the write stops pending, landed or not. It is state, not a ref, so the
  // render the press makes reads the write pending, and a write that ends before its own pending
  // render is drawn still ends it.
  const [sentReview, setSentReview] = useState<CreateArtifactReviewInput["state"] | null>(null);
  const approveButton = useRef<HTMLButtonElement>(null);
  const requestChangesButton = useRef<HTMLButtonElement>(null);
  const chipButton = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (sentReview === null || review.isPending) return;
    setSentReview(null);
    if (requestChangesOpen) return;
    const sentButton = sentReview === "approved" ? approveButton : requestChangesButton;
    if (sentButton.current === null && focusOnDocument()) chipButton.current?.focus();
  }, [sentReview, review.isPending, requestChangesOpen]);

  const pane = variant === "pane";
  // An unread document draws nothing but in the reading pane's head, which holds its two actions
  // there until the read returns; a document with no approval, or a draft, draws nothing at all.
  if (artifact === undefined ? !pane : approval === undefined || approval.state === "draft") {
    return null;
  }

  /** Whether a review holds the actions and the dialog's submit: while it is out within its
   *  deadline, until the reads it made stale are read again (`onSuccess`), on a chip drawn again
   *  while it is out too (`reviewOut`). A review past its deadline (`late`) holds them no longer:
   *  it is still tried under its key, and the shell's indicator offers to stop it, but one that
   *  never answers - the browser offline - would otherwise hold them for good. */
  const reviewHolds = reviewOut.isPending && !reviewOut.late;
  /** Held while a review holds them, and in the pane's head until the read has given the
   *  approval. */
  const held = reviewHolds || approval === undefined;
  const approveLabel =
    !pane && approval?.state === "stale" ? `Approve v${approval.latest_version}` : "Approve";
  const requestChangesLabel = "Request changes";
  // Each name begins with the words its action draws (`actionsDocument` says why it is given).
  const actionNames =
    page === null || actionsDocument === undefined
      ? undefined
      : {
          approve:
            !pane && approval?.state === "stale"
              ? `${approveLabel} ${page.CONTROL_LABELS.approval_version_of} ${actionsDocument}`
              : `${approveLabel} ${actionsDocument}`,
          requestChanges: `${requestChangesLabel} ${page.CONTROL_LABELS.approval_request_changes_to} ${actionsDocument}`,
        };
  /** Sends the review of the version the page read: the latest, which Approve names when the
   *  approval it follows is stale, and which the reading pane draws. Inside `NoTooltips` the
   *  review is remembered (`sentReview`). Nothing is sent while a review of the document is out,
   *  past its deadline too: the press's own guard holds this chip's until it settles, and a chip
   *  drawn again since finds the press by the document's name for it. */
  const send = (input: CreateArtifactReviewInput) => {
    if (artifact === undefined || approval === undefined) return;
    if (queryClient.isMutating({ exact: true, mutationKey }) > 0) return;
    const press: ReviewPress = { artifact, input, version: approval.latest_version };
    submitGuard.guard(() => {
      if (page !== null) setSentReview(input.state);
      review.mutate(press);
    });
  };
  // An Approve the server refused because the version moved, in its words; the dialog shows its
  // own press's refusal itself.
  const movedRefusal =
    !requestChangesOpen && review.isError && apiErrorCodeIn(review.error, VERSION_MOVED)
      ? review.error.message
      : null;
  // The class of a line under the actions that says what failed: that refusal, or why the reads
  // a landed review made stale have not settled (`unsettled`).
  const failureLineClass = `basis-full text-sm ${page === null ? dangerText : ON_PAGE.failure}`;
  // The pane's head draws its actions as compact as the controls beside them, so the two and the
  // pill hold one line of the head's place.
  const actionSize = pane ? "px-2 py-1 text-xs" : "px-3 py-2 text-sm md:py-1";
  // One filled button per surface: approving is the action the header wants, and two outline
  // buttons made a reader choose before reading either. At the new issue page header's size it is
  // filled with that page's ink, not the application's blue. Both actions are held while the
  // write and the reads it makes again are out (`heldPress`), on both sides of `NoTooltips`, so
  // each keeps the focus: a `disabled` button drops it to the body in a browser.
  const approveAction = (
    <button
      aria-keyshortcuts={keyshortcutsOf(shortcutKeys?.approve)}
      aria-label={actionNames?.approve}
      className={
        size === "compact"
          ? `inline-flex min-h-6 shrink-0 items-center rounded-[7px] border px-2.5 text-xs leading-5 font-medium refused:cursor-not-allowed refused:opacity-50 md:min-h-0 md:py-px ${pageInkBorder} ${pageInkFill} ${focusVisibleRing}`
          : `min-h-11 rounded-lg font-medium md:min-h-8 ${actionSize} ${page === null ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}` : ON_PAGE.filled}`
      }
      data-approval-action="approve"
      {...heldPress(held, () => send({ state: "approved" }))}
      ref={approveButton}
      type="button"
    >
      {approveLabel}
      <ControlKeyHint keys={shortcutKeys?.approve} />
    </button>
  );
  // The dialog Request changes opens gives the focus back to it once a change request lands (and,
  // inside `NoTooltips`, once its new approval is read), which a `disabled` button could not take.
  const requestChangesAction = (
    <button
      aria-keyshortcuts={keyshortcutsOf(shortcutKeys?.requestChanges)}
      aria-label={actionNames?.requestChanges}
      className={
        size === "compact"
          ? pageHeaderControl
          : `min-h-11 rounded-lg border font-medium md:min-h-8 ${actionSize} ${page === null ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}` : ON_PAGE.action}`
      }
      data-approval-action="request-changes"
      {...heldPress(held, () => setRequestChangesOpen(true))}
      ref={requestChangesButton}
      type="button"
    >
      {requestChangesLabel}
      <ControlKeyHint keys={shortcutKeys?.requestChanges} />
    </button>
  );
  // In the pane's head each action stands in its own box.
  const boxed = (button: ReactNode): ReactNode =>
    pane ? <span className={paneActionBox}>{button}</span> : button;

  const pill =
    approval === undefined ? null : (
      <button
        aria-haspopup="dialog"
        aria-keyshortcuts={keyshortcutsOf(shortcutKeys?.showReviews)}
        className={approvalPillClassName(approval.state, size, page !== null)}
        // Inside `NoTooltips`, the document whose approval this is, which an approval request's
        // own address puts the focus on.
        data-approval-artifact={page === null ? undefined : artifact?.id}
        {...stateMarks}
        onClick={() => setReviewsOpen(true)}
        ref={chipButton}
        title={
          tooltips && approval.state === "stale"
            ? `Approved at version ${approval.version}; the latest version is ${approval.latest_version}`
            : undefined
        }
        type="button"
      >
        {approvalLabel(approval, page?.CONTROL_LABELS.approval_latest)}
        <ControlKeyHint keys={shortcutKeys?.showReviews} />
      </button>
    );

  return (
    <div
      className={layout === "contents" ? "contents" : "inline-flex flex-wrap items-center gap-2"}
    >
      {pane && pill !== null ? (
        // In the pane's head the pill takes the room the two actions leave on their line, from
        // none at all (`basis-0`), so a label of many documents wraps inside it and never takes
        // an action off that line.
        <span className="flex min-w-0 grow basis-0 justify-end self-center">{pill}</span>
      ) : (
        pill
      )}
      {variant !== "list" && showActions ? (
        <>
          <ActionsGroup together={keepActionsTogether}>
            {approval?.state === "approved" ? null : boxed(approveAction)}
            {boxed(requestChangesAction)}
          </ActionsGroup>
          {movedRefusal === null ? null : (
            <p className={failureLineClass} role="alert">
              {movedRefusal}
            </p>
          )}
          {unsettled === undefined ? null : (
            <p className={failureLineClass} role="alert">
              {`${APPROVAL_NOT_READ} ${unsettled}.`}
            </p>
          )}
        </>
      ) : null}
      {reviewsOpen && artifact !== undefined ? (
        <ReviewsDialog
          artifactId={artifact.id}
          issueKey={artifact.issue_key}
          onClose={() => setReviewsOpen(false)}
        />
      ) : null}
      {requestChangesOpen && artifact !== undefined ? (
        <RequestChangesDialog
          error={review.isError ? review.error : null}
          held={reviewHolds}
          issueKey={artifact.issue_key ?? undefined}
          onClose={() => {
            // Escape and the backdrop close the dialog at any time, and so does its Close inside
            // `NoTooltips`, which stands in Cancel's place while the write is out; outside, Cancel
            // is held then (`RequestChangesDialog`). Outside `NoTooltips` closing lets go of the
            // write (`reset`), as on `main`. Inside, a write that is out goes on, and Approve and
            // Request changes stay refused until it lands and its read is drawn, or until it ends
            // without landing: `reset` would let them be pressed again before it lands. A refusal
            // that comes later is shown by the shell's write indicator, as every write's is. A
            // write that keeps trying never holds the reader in the dialog, whose focus trap would
            // keep the indicator's Stop trying out of reach. With nothing out, closing clears the
            // refusal the dialog showed, on both sides.
            if (page === null || !review.isPending) review.reset();
            setRequestChangesOpen(false);
          }}
          onSubmit={(reason) => send({ reason, state: "changes_requested" })}
          project={artifact.project}
        />
      ) : null}
    </div>
  );
}
