import {
  dangerText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnCanvas,
} from "../../theme/classes";

/** What the rebuild this page offers asks before it runs: the rebuild closes the document's room,
 *  and every browser connected to it drops the edits it has not sent. */
const NEEDS_REBUILD_CONFIRMATION =
  "Rebuild this document from its current text? Every browser connected to it, this one included, drops the edits it has not sent yet.";

export interface RefusedEditNoticeProps {
  /** The sentence the person reads: why their edit or change was refused, or why the document
   *  takes changes no more. */
  readonly notice: string;
  /** The server's own words for why, where it gave some: how near the decode cap the document's
   *  read found it (`near_decode_cap_reason`). */
  readonly detail?: string;
  /** Whether the notice carries the offer to rebuild the document: only on the document's own
   *  page, and only where the rebuild route takes the document (`ProofDocument` says when). */
  readonly offersRebuild: boolean;
  /** Whether the rebuild this panel offered is out. */
  readonly rebuilding: boolean;
  /** How the rebuild this panel offered was refused, if it was. */
  readonly rebuildError: unknown;
  /** Takes the notice away: the person has read it. */
  readonly onDismiss: () => void;
  /** Rebuilds the document. Called only once the person has confirmed it. */
  readonly onRebuild: () => void;
}

/**
 * The panel the editor shows for a refusal the person must read: an edit the server refused with
 * `DOCUMENT_TOO_LARGE_CLOSE_CODE` (the sentence for its reason, `refusedEditNotice`), a write to
 * the document refused because only a rebuild lets it take that write, or a document whose own
 * read says it stands so near the decode cap that it refuses writes (`near_decode_cap`). Each has
 * a Dismiss, and the ones a rebuild mends carry the offer to rebuild it - the one button and the
 * one request (`POST /api/v1/artifacts/{id}/rebuild`, from the document's own text) whichever led
 * to it.
 *
 * The rebuild route is human-only (`requireHuman`), and every person the dashboard signs in is a
 * user, so the offer goes to everyone the refusal reaches on the document's page. A refusal of the
 * rebuild itself shows beside its button.
 */
export function RefusedEditNotice({
  detail,
  notice,
  offersRebuild,
  onDismiss,
  onRebuild,
  rebuildError,
  rebuilding,
}: RefusedEditNoticeProps) {
  return (
    <div className="space-y-3" role="alert">
      <div className="flex flex-wrap items-center gap-3">
        <p className={dangerText}>{notice}</p>
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={onDismiss}
          type="button"
        >
          Dismiss
        </button>
      </div>
      {detail === undefined ? null : <p className={`text-sm ${textSecondaryOnCanvas}`}>{detail}</p>}
      {offersRebuild ? (
        <div className="flex flex-wrap items-center gap-3">
          <p>
            Rebuilding replaces the document's edit history with its current text, so it can take
            edits again.
          </p>
          <button
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={rebuilding}
            onClick={() => {
              if (window.confirm(NEEDS_REBUILD_CONFIRMATION)) {
                onRebuild();
              }
            }}
            type="button"
          >
            {rebuilding ? "Rebuilding…" : "Rebuild this document"}
          </button>
          {rebuildError === undefined ? null : (
            <p className={dangerText}>
              {rebuildError instanceof Error
                ? rebuildError.message
                : "Could not rebuild this document."}
            </p>
          )}
        </div>
      ) : null}
    </div>
  );
}
