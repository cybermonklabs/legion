import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useState } from "react";

import { isNoRoute } from "../../api/client";
import { changeRequestQuery, issueQuery } from "../../api/queries";
import type { AskApprovalDocument, ChangeRequestNode } from "../../api/types";
import { usePageWords } from "../../components/tooltips";
import {
  backdrop40,
  card,
  dangerText,
  pageFailedText,
  pageHairBorder,
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
import { PHONE_VIEWPORT_QUERY, useDialog } from "../shell/useDialog";
import { blockParts } from "./change-request";

/**
 * Each colour the sheet draws inside `NoTooltips`, the new issue page's, by the role it has there,
 * as `ApprovalChip`'s dialogs draw theirs: the page's paper inside its hairline rule over the icon
 * rail's dark, its heading and the texts in the ink, the labels in the muted grey, a pending read
 * in the quiet grey, a failed read in the failed red, Approve filled with the ink and Cancel the
 * header's control. They stand in this table, not beside `main`'s classes, for the reason
 * `ApprovalChip`'s `ON_PAGE` gives (`theme/text-on-background.test.ts`).
 */
const ON_PAGE = {
  action: `${pageLineBorder} ${pagePaperBg} ${pageMutedText}`,
  backdrop: "bg-icon-rail/40",
  dialog: `${pageHairBorder} ${pagePaperBg}`,
  failure: pageFailedText,
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  heading: pageInkText,
  label: pageMutedText,
  pending: pageQuietText,
  text: pageInkText,
} as const;

/** What an approval covers, as the sheet is asked for it: the issue the approval is of (null for
 *  a project document's) and each document the approval names, the one it was asked on first. */
export interface ApprovalCovered {
  readonly issueKey: string | null;
  readonly documents: readonly Pick<AskApprovalDocument, "artifact_id" | "version">[];
}

/** How a node stands to the approved spec, as a row's heading says it. */
const WHAT_WORDS: Readonly<Record<string, string>> = {
  added: "Adds",
  changed: "Changes",
  removed: "Removes",
  stale: "Was written against an older version of",
};

/** The kind of a node, as a row's heading names it. */
const KIND_WORDS: Readonly<Record<string, string>> = {
  check: "the check",
  component: "the component",
  interface: "the interface",
  statement: "the statement",
};

/** One node the approval covers: what it does to the node, its attributes, the text the approval
 *  writes and, for a changed node, the approved text it replaces. A removed node shows the
 *  approved text the approval removes, never the issue's own words for the removal. */
function SheetNode({ node, page }: { node: ChangeRequestNode; page: boolean }): ReactNode {
  const removed = node.what === "removed";
  const shown = removed
    ? blockParts(node.approved_text, "approved")
    : blockParts(node.text, "issue");
  const replaced =
    removed || node.approved_text === "" ? null : blockParts(node.approved_text, "approved");
  const label = page ? ON_PAGE.label : textMutedOnSurface;
  const text = page ? ON_PAGE.text : textPrimaryOnSurface;
  return (
    <li className="space-y-1" data-node-id={node.id} data-what={node.what}>
      <p className={`text-sm font-semibold ${page ? ON_PAGE.heading : textSecondaryOnSurface}`}>
        {WHAT_WORDS[node.what] ?? node.what} {KIND_WORDS[node.kind] ?? node.kind}{" "}
        <code className="[overflow-wrap:anywhere]">{node.id}</code>
      </p>
      {shown.attributes.map(([name, value]) => (
        <p className={`text-xs ${label}`} key={name}>{`${name} ${value}`}</p>
      ))}
      <p
        className={`text-sm whitespace-pre-wrap [overflow-wrap:anywhere] ${text}`}
        data-testid="approval-sheet-text"
      >
        {shown.body}
      </p>
      {replaced === null ? null : (
        <div>
          <p className={`text-xs font-medium ${label}`}>It replaces the approved text:</p>
          <p
            className={`text-sm whitespace-pre-wrap [overflow-wrap:anywhere] ${label}`}
            data-testid="approval-sheet-approved"
          >
            {replaced.body}
          </p>
        </div>
      )}
    </li>
  );
}

export interface ApprovalSheetProps {
  readonly issueKey: string;
  /** The version of the issue's spec the approval request names, or null while the sheet cannot
   *  tell which document of the request is the spec (the issue's read failed): the sheet then
   *  says so and offers `onRetry`. */
  readonly version: number | null;
  /** Whether the approval names the issue's Verification document beside its spec, so the sheet
   *  shows the digest it pins on that document too. */
  readonly coversVerification: boolean;
  /** The version of the Verification document the approval request names. The sheet restates the
   *  checks and the digest of that version only: a Verification document that moved past it is
   *  said so, and Approve sends nothing, since Dispatch refuses such an approval. */
  readonly verificationVersion?: number;
  readonly onApprove: () => void;
  readonly onClose: () => void;
  /** Called by Retry while `version` is null. */
  readonly onRetry?: () => void;
}

/**
 * The sheet a phone shows before a person approves a change request (R22: "Before the person
 * approves on a phone, Dispatch shows a sheet that restates what the approval covers"). It reads
 * the change request at the spec version the approval request names, never at another version,
 * since an approval counts only for the texts its request names. It lists each node the request
 * adds, changes and removes, and each check it changes, with its text, then the digest the
 * approval pins on the spec and, when the request names it, on the Verification document at the
 * version the request names. Approve sends the approval; it is refused until the read is in, and
 * while the Verification document stands at another version than the one the request names.
 * Cancel, Escape and the backdrop close the sheet and send nothing.
 */
export function ApprovalSheet({
  coversVerification,
  issueKey,
  onApprove,
  onClose,
  onRetry,
  verificationVersion,
  version,
}: ApprovalSheetProps): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  const page = usePageWords() !== null;
  const read = useQuery({
    ...changeRequestQuery(issueKey, version ?? 0),
    enabled: version !== null,
  });
  const request = read.data;
  const nodes =
    request === undefined ? [] : [...(request.proposals ?? []), ...(request.changed_checks ?? [])];
  // The Verification document moved past the version the request names: the checks and the digest
  // the read restates are another version's.
  const verificationMoved =
    request !== undefined &&
    coversVerification &&
    verificationVersion !== undefined &&
    request.verification_version !== verificationVersion;
  const failed = version === null || read.isError;
  const label = page ? ON_PAGE.label : textMutedOnSurface;
  const digest = `font-mono text-xs break-all ${page ? ON_PAGE.text : textPrimaryOnSurface}`;
  const failure = page ? ON_PAGE.failure : dangerText;
  return (
    <>
      <div
        aria-hidden="true"
        className={`fixed inset-0 z-40 ${page ? ON_PAGE.backdrop : backdrop40}`}
        onClick={onClose}
      />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-12">
        <div
          aria-label="What this approval covers"
          aria-modal="true"
          className={`pointer-events-auto flex max-h-[85vh] w-full max-w-md flex-col gap-3 rounded-lg border p-4 shadow-xl ${page ? ON_PAGE.dialog : card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2
            className={`text-sm font-semibold ${page ? ON_PAGE.heading : textSecondaryOnSurface}`}
          >
            What this approval covers
          </h2>
          <div className="min-h-0 space-y-3 overflow-y-auto">
            {request === undefined ? (
              failed ? (
                <div
                  className={`flex flex-wrap items-center gap-3 text-sm ${failure}`}
                  role="alert"
                >
                  <p>Could not read what this approval covers.</p>
                  <button
                    className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
                    disabled={read.isFetching}
                    onClick={() => (version === null ? onRetry?.() : void read.refetch())}
                    type="button"
                  >
                    Retry
                  </button>
                </div>
              ) : (
                <p className={`text-sm ${page ? ON_PAGE.pending : textMutedOnSurface}`}>
                  Reading what this approval covers…
                </p>
              )
            ) : (
              <>
                {nodes.length === 0 ? (
                  <p className={`text-sm ${label}`}>
                    This approval adds, changes and removes no statement, component, interface or
                    check.
                  </p>
                ) : (
                  <ul aria-label="What the approval writes" className="space-y-3">
                    {nodes.map((node) => (
                      <SheetNode key={`${node.kind} ${node.id}`} node={node} page={page} />
                    ))}
                  </ul>
                )}
                <div className="space-y-1">
                  <p className={`text-xs ${label}`}>
                    The approval pins this digest of version {request.version} of the spec:
                  </p>
                  <p className={digest} data-testid="approval-sheet-digest">
                    {request.digest}
                  </p>
                  {verificationMoved ? (
                    <p className={`text-sm ${failure}`} role="alert">
                      This approval request names version {verificationVersion} of the Verification
                      document, and the document is now at version {request.verification_version}.
                      Dispatch refuses this approval. Ask for a new approval request.
                    </p>
                  ) : coversVerification && request.verification_version > 0 ? (
                    <>
                      <p className={`text-xs ${label}`}>
                        And this digest of version {request.verification_version} of the
                        Verification document:
                      </p>
                      <p className={digest} data-testid="approval-sheet-verification-digest">
                        {request.verification_digest}
                      </p>
                    </>
                  ) : null}
                </div>
              </>
            )}
          </div>
          <div className="flex items-center justify-end gap-2">
            <button
              className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium ${page ? ON_PAGE.action : `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}`}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`min-h-11 rounded-lg px-3 py-1.5 text-sm font-medium ${page ? ON_PAGE.filled : `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}`}
              disabled={request === undefined || verificationMoved}
              onClick={onApprove}
              type="button"
            >
              Approve
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

/** An approval the sheet holds until the person approves or cancels it: the spec version and the
 *  Verification version its request names, or no version while the issue's read failed. */
interface HeldApproval {
  readonly covered: ApprovalCovered;
  readonly issueKey: string;
  readonly version: number | null;
  readonly verificationVersion?: number;
  readonly approve: () => void;
}

/**
 * The sheet before an approval on a phone (R22), for a surface that sends an approval: the Inbox
 * card's Answer with Approve picked, and the document header's Approve. `confirm(covered, approve)`
 * stands in for the send. On a wider screen, and for a document of no issue, it sends at once, as
 * every approval was sent before. Otherwise it reads the issue, to tell which document of the
 * request is the spec: an approval that names no spec sends at once. It then reads the change
 * request at the spec version the request names: an issue whose repository is no spec source
 * sends at once, and so does a Dispatch older than the change request (its read has no route).
 * Every other issue opens the sheet, which sends only from its Approve, with the Verification
 * version the request names when it names one. A read that failed opens the sheet too, which
 * says so with its Retry and sends nothing: the person approves only what the sheet restated, and
 * the sheet never reads a version the request does not name. `sheet` is the element the surface
 * draws.
 */
export function useApprovalSheet(): {
  readonly confirm: (covered: ApprovalCovered, approve: () => void) => void;
  readonly sheet: ReactNode;
} {
  const queryClient = useQueryClient();
  const [held, setHeld] = useState<HeldApproval | null>(null);
  const confirm = useCallback(
    (covered: ApprovalCovered, approve: () => void) => {
      const { documents, issueKey } = covered;
      if (
        !window.matchMedia(PHONE_VIEWPORT_QUERY).matches ||
        issueKey === null ||
        documents.length === 0
      ) {
        approve();
        return;
      }
      void (async () => {
        let primary: string;
        try {
          primary = (await queryClient.ensureQueryData(issueQuery(issueKey))).primary_artifact_id;
        } catch {
          setHeld({ approve, covered, issueKey, version: null });
          return;
        }
        const spec = documents.find((document) => document.artifact_id === primary);
        if (spec === undefined) {
          approve();
          return;
        }
        // A request that names the Verification document names it beside the spec.
        const verification = documents.find((document) => document.artifact_id !== primary);
        try {
          const request = await queryClient.ensureQueryData(
            changeRequestQuery(issueKey, spec.version)
          );
          if (!request.spec_source) {
            approve();
            return;
          }
        } catch (error) {
          if (isNoRoute(error)) {
            approve();
            return;
          }
        }
        setHeld({
          approve,
          covered,
          issueKey,
          verificationVersion: verification?.version,
          version: spec.version,
        });
      })();
    },
    [queryClient]
  );
  const sheet =
    held === null ? null : (
      <ApprovalSheet
        coversVerification={held.verificationVersion !== undefined}
        issueKey={held.issueKey}
        onApprove={() => {
          setHeld(null);
          held.approve();
        }}
        onClose={() => setHeld(null)}
        onRetry={() => {
          setHeld(null);
          confirm(held.covered, held.approve);
        }}
        verificationVersion={held.verificationVersion}
        version={held.version}
      />
    );
  return { confirm, sheet };
}
