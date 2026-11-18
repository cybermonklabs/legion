import { type ReactNode, useMemo } from "react";

import {
  dangerText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import type { ComposerKind } from "./composer-model";
import { SendMovedError, type SentDraft } from "./send-request";
import type { OfferedDraft } from "./useKeptDrafts";

/** Each kind as an offered draft names the one it was written as, beside a compose of another. */
const KIND_WORDS: Record<ComposerKind, string> = {
  ask: "an ask",
  comment: "a comment",
  suggestion: "a suggestion",
};

/**
 * What a composer offers beside its field that the field does not hold (`useKeptDrafts`): an
 * edit written over a body the comment no longer holds (`changed`), and a draft a margin compose
 * kept for other words of its document (`offered`). Each waits with the words that say why, and
 * with the reader's presses for it: one takes it into the field, where the composer can send it,
 * and the other drops it. Nothing is saved or sent meanwhile.
 */
export function KeptDraftOffers({
  changed,
  kind,
  offered,
  onDiscardChanged,
  onDropOffered,
  onTakeChanged,
  onTakeOffered,
}: {
  readonly changed: SentDraft | undefined;
  /** The kind the composer writes now, which an offered draft names its own beside. */
  readonly kind: ComposerKind;
  readonly offered: OfferedDraft | undefined;
  /** Discard my edit: the composer's Discard, which drops the draft the reader wrote there. */
  readonly onDiscardChanged: () => void;
  readonly onDropOffered: () => void;
  readonly onTakeChanged: () => void;
  /** Use my draft, which a compose that could not send the draft - a kind its closed issue
   *  refuses - leaves out, so the offer waits until the reader drops it. */
  readonly onTakeOffered: (() => void) | undefined;
}): ReactNode {
  // What a stopped send says once its draft is away from the address it went to
  // (`SendMovedError`): taken into this compose, it goes as a new message under a key of its own.
  const offeredStopped = useMemo(
    () => (offered?.stopped === true ? new SendMovedError(offered).message : undefined),
    [offered]
  );
  return (
    <>
      {/* An edit written over a body the comment no longer holds: the field shows the comment as
          it reads now, and the edit waits beside it with the words that say why, until the reader
          takes it back into the field or drops it. Nothing is saved meanwhile. */}
      {changed === undefined ? null : (
        <div className="space-y-2">
          <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
            <p>
              This comment changed since you wrote your edit. The field shows the comment as it
              reads now. Your edit is kept here:
            </p>
            <blockquote
              className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
            >
              {changed.body}
            </blockquote>
          </div>
          <div className="flex flex-wrap gap-3">
            <button
              className={`text-sm font-medium ${linkText} ${linkHoverText}`}
              onClick={onTakeChanged}
              type="button"
            >
              Use my edit
            </button>
            <button
              className={`text-sm font-medium underline ${dangerText}`}
              onClick={onDiscardChanged}
              type="button"
            >
              Discard my edit
            </button>
          </div>
        </div>
      )}
      {/* A draft the margin's compose kept for other words of the document: a newer compose there
          carried nothing, so it waits beside this one, naming the words it was written for, and
          the kind it was written as where this compose is another, since what it brings back -
          an ask's options, a suggestion's replacement - shows only under that kind. Only the
          reader's own press puts it in this field, and nothing is sent meanwhile. A draft whose
          send the reader stopped may have been delivered there, and taken here it goes as a new
          message, so the offer says so before that press, as the stopped send's note did. */}
      {offered === undefined ? null : (
        <div className="space-y-2">
          <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
            <p>
              {`You wrote this draft${offered.kind === undefined || offered.kind === kind ? "" : ` as ${KIND_WORDS[offered.kind]},`} for other words: “${offered.anchor.quote}”. It is kept here:`}
            </p>
            <blockquote
              className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
            >
              {offered.draft.body}
            </blockquote>
            {offered.draft.replacement === "" ? null : (
              <>
                <p>Replace with:</p>
                <blockquote
                  className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
                >
                  {offered.draft.replacement}
                </blockquote>
              </>
            )}
            {offeredStopped === undefined ? null : <p className={dangerText}>{offeredStopped}</p>}
          </div>
          <div className="flex flex-wrap gap-3">
            {onTakeOffered === undefined ? null : (
              <button
                className={`text-sm font-medium ${linkText} ${linkHoverText}`}
                onClick={onTakeOffered}
                type="button"
              >
                Use my draft
              </button>
            )}
            <button
              className={`text-sm font-medium underline ${dangerText}`}
              onClick={onDropOffered}
              type="button"
            >
              Discard my draft
            </button>
          </div>
        </div>
      )}
    </>
  );
}
