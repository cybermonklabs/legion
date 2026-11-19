import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import { api, type CreateArtifactReviewInput, isCapExceeded } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { Artifact, ArtifactApproval } from "../../api/types";
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
  inputClasses,
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
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { focusOnDocument } from "../shell/roving";
import { useDialog } from "../shell/useDialog";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/**
 * Each colour the chip, its two actions and its two dialogs draw inside `NoTooltips`, the new
 * issue page's, by the role `main`'s colour has: a dialog is the page's paper inside its hairline
 * rule over the icon rail's dark, the one page colour dark in both schemes; its heading and a
 * review are the ink, the reviewer's reason and a label the muted grey, Close and a pending read
 * the quiet grey, a refusal the failed red; Approve and the dialog's Request changes are filled
 * with the ink, as the header's one filled control is, and the other actions are the header's
 * control. The reason box keeps `main`'s focus rule, the application's one focus indicator.
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
  onClose,
}: {
  artifactId: string;
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
                        source={textSourceOf("artifact", artifactId)}
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
  onClose,
  onSubmit,
  saving,
}: {
  /** What the server answered the last request with, or null while none was refused. */
  error: Error | null;
  onClose: () => void;
  onSubmit: (reason: string) => void;
  saving: boolean;
}): ReactNode {
  const [reason, setReason] = useState("");
  const reasonId = useId();
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
          <label
            className={`block text-sm font-medium ${page === null ? textSecondaryOnSurface : ON_PAGE.label}`}
            htmlFor={reasonId}
          >
            Reason
            {/* Inside `NoTooltips`, while the write is out, the text box is read-only and the
                submit `aria-disabled`, a press of it doing nothing: `disabled` drops the focus to
                the body in a browser, and Tab would then leave the dialog. Outside both are
                `disabled` while the write is out, as on `main`. */}
            <textarea
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${page === null ? inputClasses(false) : ON_PAGE.reasonBox}`}
              disabled={page === null && saving}
              id={reasonId}
              onChange={(event) => setReason(event.target.value)}
              onKeyDown={(event) =>
                submitOnModifiedEnter(event, {
                  onFormlessSubmit: () => {
                    if (trimmed !== "" && !saving) {
                      onSubmit(trimmed);
                    }
                  },
                })
              }
              readOnly={page !== null && saving}
              ref={textareaRef}
              value={reason}
            />
          </label>
          {error === null ? null : (
            <p className={`text-sm ${page === null ? dangerText : ON_PAGE.failure}`} role="alert">
              {/* A reason Dispatch refused for its length says so in its own words, which name
                  the block and the bound; any other failure is worth another try. */}
              {isCapExceeded(error) ? error.message : "Could not request changes. Try again."}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              // Inside `NoTooltips`, while the write is out, this button reads Close and closes the
              // dialog as Escape and the backdrop do: `onClose` leaves the write going, and the
              // dialog gives the focus back to the button that opened it. Once the write has ended
              // it is Cancel again. Outside it is `disabled` while the write is out, as on `main`.
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${page === null ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}` : ON_PAGE.action}`}
              disabled={page === null && saving}
              onClick={onClose}
              type="button"
            >
              {page !== null && saving ? page.CONTROL_LABELS.close : "Cancel"}
            </button>
            <button
              aria-disabled={(page !== null && saving) || undefined}
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${page === null ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}` : ON_PAGE.filled}`}
              disabled={trimmed === "" || (page === null && saving)}
              onClick={() => {
                if (saving) {
                  return;
                }
                onSubmit(trimmed);
              }}
              type="button"
            >
              {saving ? "Submitting…" : "Request changes"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

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
  artifact: Artifact;
  /** `header`: shown on the document header, with an approval-state chip.
   * `list` (default): shown in a compact list row. */
  variant?: "header" | "list";
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
}

/**
 * The document approval indicator: a small badge summarizing `artifact.approval`, clickable to
 * open a popover of every human review (`ArtifactReview`). On the document header only, it also
 * carries the Approve / Request changes actions that write a new review.
 */
export function ApprovalChip({
  actionsDocument,
  artifact,
  keepActionsTogether = false,
  layout = "inline",
  onReviewed,
  size = "default",
  variant = "list",
  showActions = variant === "header",
}: ApprovalChipProps): ReactNode {
  const { approval } = artifact;
  const [reviewsOpen, setReviewsOpen] = useState(false);
  const [requestChangesOpen, setRequestChangesOpen] = useState(false);
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const tooltips = useTooltips();
  const page = usePageWords();

  const review = useKeyedWrite({
    label: (input: CreateArtifactReviewInput) =>
      `${input.state === "approved" ? "Approval" : "Change request"} on ${artifact.name}`,
    onSettled: () => submitGuard.release(),
    write: (input, bounds) => api.createArtifactReview(artifact.id, input, bounds),
    onSuccess: () => {
      // Outside `NoTooltips` the dialog closes as the write lands and the reads the write makes
      // stale go on after it, as on `main`. Inside, `onSuccess` returns those reads, which
      // TanStack Query awaits before the write stops pending: Approve, Request changes and the
      // dialog's submit stay refused, and keep the focus, until the new approval is read, and
      // only then does a dialog the reader has not closed (`onClose`) close and give the focus
      // back to the button that opened it.
      if (page === null) setRequestChangesOpen(false);
      onReviewed?.();
      const rereads = [
        queryClient.invalidateQueries({ queryKey: ["artifact-reviews", artifact.id] }),
        queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] }),
        queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey }),
        ...(artifact.issue_key === null
          ? [
              queryClient.invalidateQueries({
                queryKey: ["project", artifact.project, "artifacts"],
              }),
              queryClient.invalidateQueries({
                queryKey: ["artifact-ref", `${artifact.project}/${artifact.slug}`],
              }),
            ]
          : [
              queryClient.invalidateQueries({ queryKey: ["issue", artifact.issue_key] }),
              queryClient.invalidateQueries({ queryKey: ["artifacts", artifact.issue_key] }),
              queryClient.invalidateQueries({ queryKey: ["asks", artifact.issue_key] }),
            ]),
      ];
      if (page === null) return;
      return Promise.all(rereads).then(() => setRequestChangesOpen(false));
    },
  });

  // Inside `NoTooltips` the review sent, by a press of Approve or by the Request changes dialog's
  // submit, is remembered until its write stops pending, which is once the new approval is read
  // (`onSuccess` returns its reads, and closes a change request's dialog before the write stops
  // pending). Approve and Request changes are never `disabled` there, so while they are drawn they
  // keep the focus, and the dialog gives it back to Request changes as it closes: once the read is
  // drawn, or at once when the reader closes it with Escape, the backdrop or its Close while the
  // write is out, which goes on (`onClose`). Once the write stops pending with the dialog closed
  // and the read has taken the sent review's own button off the page, Approve for an approval and
  // Request changes for a change request, a focus that fell with it to the document
  // (`focusOnDocument`) goes to the chip; a focus anywhere else is left where it is. A change
  // request refused while its dialog is open keeps the dialog open with its error, and the focus
  // in it. One refused after the reader closed its dialog moves no focus: its refusal is in the
  // shell's write indicator, and the dialog opened next shows its error, as it shows any refusal
  // of a review left over, until a submit or a close clears it. The review is forgotten whenever
  // the write stops pending, landed or not. It is state, not a ref, so the render the press makes
  // reads the write pending, and a write that ends before its own pending render is drawn still
  // ends it.
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

  if (approval === undefined || approval.state === "draft") {
    return null;
  }

  const approveLabel =
    approval.state === "stale" ? `Approve v${approval.latest_version}` : "Approve";
  const requestChangesLabel = "Request changes";
  // Each name begins with the words its action draws (`actionsDocument` says why it is given).
  const actionNames =
    page === null || actionsDocument === undefined
      ? undefined
      : {
          approve:
            approval.state === "stale"
              ? `${approveLabel} ${page.CONTROL_LABELS.approval_version_of} ${actionsDocument}`
              : `${approveLabel} ${actionsDocument}`,
          requestChanges: `${requestChangesLabel} ${page.CONTROL_LABELS.approval_request_changes_to} ${actionsDocument}`,
        };

  return (
    <div
      className={layout === "contents" ? "contents" : "inline-flex flex-wrap items-center gap-2"}
    >
      <button
        aria-haspopup="dialog"
        className={approvalPillClassName(approval.state, size, page !== null)}
        // Inside `NoTooltips`, the document whose approval this is, which an approval request's
        // own address puts the focus on.
        data-approval-artifact={page === null ? undefined : artifact.id}
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
      </button>
      {variant === "header" && showActions ? (
        <ActionsGroup together={keepActionsTogether}>
          {approval.state === "approved" ? null : (
            // One filled button per surface: approving is the action the header wants, and
            // two outline buttons made a reader choose before reading either. At the new issue
            // page header's size it is filled with that page's ink, not the application's blue.
            <button
              // Inside `NoTooltips`, while the write is out, Approve is `aria-disabled` and a press
              // of it does nothing, so it keeps the focus: a `disabled` button drops it to the
              // body in a browser. Outside it is `disabled` while the write is out, as on `main`.
              aria-disabled={(page !== null && review.isPending) || undefined}
              aria-label={actionNames?.approve}
              className={
                size === "compact"
                  ? `inline-flex min-h-6 shrink-0 items-center rounded-[7px] border px-2.5 text-xs leading-5 font-medium refused:cursor-not-allowed refused:opacity-50 md:min-h-0 md:py-px ${pageInkBorder} ${pageInkFill} ${focusVisibleRing}`
                  : `min-h-11 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${page === null ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}` : ON_PAGE.filled}`
              }
              disabled={page === null && review.isPending}
              onClick={() => {
                if (review.isPending) {
                  return;
                }
                submitGuard.guard(() => {
                  if (page !== null) setSentReview("approved");
                  review.mutate({ state: "approved" });
                });
              }}
              ref={approveButton}
              type="button"
            >
              {approveLabel}
            </button>
          )}
          <button
            // Inside `NoTooltips`, while the write is out, this button is `aria-disabled` and a
            // press of it does nothing: the dialog it opens gives the focus back to it once a
            // change request lands and its new approval is read, and a `disabled` button takes no
            // focus, which then falls to the body. Outside it is `disabled` while the write is
            // out, as on `main`.
            aria-disabled={(page !== null && review.isPending) || undefined}
            aria-label={actionNames?.requestChanges}
            className={
              size === "compact"
                ? pageHeaderControl
                : `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${page === null ? `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}` : ON_PAGE.action}`
            }
            disabled={page === null && review.isPending}
            onClick={() => {
              if (review.isPending) {
                return;
              }
              setRequestChangesOpen(true);
            }}
            ref={requestChangesButton}
            type="button"
          >
            {requestChangesLabel}
          </button>
        </ActionsGroup>
      ) : null}
      {reviewsOpen ? (
        <ReviewsDialog artifactId={artifact.id} onClose={() => setReviewsOpen(false)} />
      ) : null}
      {requestChangesOpen ? (
        <RequestChangesDialog
          error={review.isError ? review.error : null}
          onClose={() => {
            // Escape and the backdrop close the dialog at any time, and so does its Close inside
            // `NoTooltips`, which stands in Cancel's place while the write is out; outside, Cancel
            // is `disabled` then (`RequestChangesDialog`). Outside `NoTooltips` closing lets go of
            // the write (`reset`), as on `main`. Inside, a write that is out goes on, and Approve
            // and Request changes stay refused until it lands and its read is drawn, or until it
            // ends without landing: `reset` would let them be pressed again before it lands. A
            // refusal that comes later is shown by the shell's write indicator, as every write's
            // is. A write that keeps trying never holds the reader in the dialog, whose focus trap
            // would keep the indicator's Stop trying out of reach. With nothing out, closing
            // clears the refusal the dialog showed, on both sides.
            if (page === null || !review.isPending) review.reset();
            setRequestChangesOpen(false);
          }}
          onSubmit={(reason) =>
            submitGuard.guard(() => {
              if (page !== null) setSentReview("changes_requested");
              review.mutate({ reason, state: "changes_requested" });
            })
          }
          saving={review.isPending}
        />
      ) : null}
    </div>
  );
}
