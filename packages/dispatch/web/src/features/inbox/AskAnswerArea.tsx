import { type ComponentProps, type ReactNode, useId } from "react";

import type { ArtifactOwner } from "../../api/client";
import { useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { pageQuietText, textMutedOnSurface } from "../../theme/classes";
import { type DraftUpload, DraftUploadStatus } from "../artifacts/ArtifactUpload";
import type { MentionField } from "../conversation/MentionField";

/** What the buttons under the area are given: the id of the line already showing each refusal the
 *  host named, by the name the host knows it by. Generic over those names, so a host that reads
 *  `reasonId.askBack` where it named `askback` is a type error rather than a button that
 *  silently loses its description. Whether a picture is still out is the host's own, since the
 *  upload is (`upload`): a host draws sends this area never sees. */
export interface AskAnswerActions<Name extends string = string> {
  readonly reasonId: Readonly<Record<Name, string>>;
}

/** What the area hands the field its host mounts (`field`): the draft and each edit of it, and the
 *  paste, the drop and the keys, which are the area's own. */
type AskAnswerFieldProps = Required<
  Pick<ComponentProps<typeof MentionField>, "body" | "onChange" | "onKeyDown" | "readOnly" | "rows">
> &
  Pick<ComponentProps<typeof MentionField>, "onDrop" | "onPaste">;

/** The line that says why a send refuses, inside `NoTooltips` (`components/tooltips.tsx`): the
 *  new issue page's quiet words, which qualify the field above them. It stands in this table, as
 *  `AskCard`'s colours do, so `theme/text-on-background.test.ts` pairs none of `main`'s words
 *  with the page's ground. */
const ON_PAGE = {
  reason: pageQuietText,
} as const;

/**
 * The one answer area of an ask: the box every writing box mounts, with its reference picker
 * (`features/conversation/MentionField.tsx`), the draft behind it, the line that says why a send
 * refuses, and the host's uploads, which a file pasted or dropped into the box starts. Its two
 * hosts differ only in the box's row of the writing list and in what they send it with, which
 * they pass as `field` and `actions`: the open card's Answer and Ask back (`AskCard.tsx`, which
 * answers through `useAskAnswerForm`), and the reply box under an answered ask (`AskThread.tsx`).
 * The host mounts the field itself, since the field's row is the host's (`writing`), and the
 * area hands it everything else it holds (`AskAnswerFieldProps`). The draft is the host's - the
 * ask's answer draft or its reply draft (`answer-drafts.ts`, `reply-drafts.ts`) - and so is the
 * upload behind it, because the host is what sends them: a compact card's lone Answer stands
 * where this area is not mounted, and an upload the area held would have left that send ungated.
 *
 * A picked session is a reference written in the draft's text, which the comment Ask back and
 * Reply post carries to the server, and the server delivers the comment to that session. An
 * answer delivers to nobody it names, so a host whose send is an answer refuses the draft while
 * it names a session (`ANSWER_MENTION_REFUSAL`) rather than sending it to nobody.
 */
export function AskAnswerArea<Name extends string, UploadDraft>({
  actions,
  draft,
  field,
  onDraftChange,
  readOnly = false,
  refusals,
  upload,
  uploadDraft,
  uploadOwner,
}: {
  readonly actions: (state: AskAnswerActions<Name>) => ReactNode;
  readonly draft: string;
  /** The box, which the host mounts with its own row of the writing list and its own label,
   *  handed what the area holds of it. */
  readonly field: (props: AskAnswerFieldProps) => ReactNode;
  readonly onDraftChange: (text: string) => void;
  /** Inside `NoTooltips`, the host refuses the draft while its own write is out: the field keeps
   *  the focus, `readOnly` in place of `disabled`, and takes no paste, drop or Ctrl/Cmd+Enter. */
  readonly readOnly?: boolean;
  /** Why each of the host's buttons refuses the draft as it stands, by the name the host knows it
   *  by: each reason is shown on one line under the field, and `actions` is handed that line's id
   *  for the button's `refusalShownBy`. */
  readonly refusals: Readonly<Record<Name, string | null>>;
  /** The uploads behind this draft, which the host holds (`useDraftUpload`): a file pasted or
   *  dropped into the field starts one, the line under the field says one is still out, and the
   *  host holds its own sends while `upload.pending > 0` - including the ones it draws where
   *  this area is not mounted. */
  readonly upload: DraftUpload<UploadDraft>;
  /** The draft a file pasted or dropped here joins, which its press carries (`useDraftUpload`):
   *  a host may draw this area for another ask before the file lands, and the file's text then
   *  goes to the ask it was pasted under, never to the one the area shows. */
  readonly uploadDraft: UploadDraft;
  /** Where a file pasted or dropped into the box goes: the ask's issue, or its document's
   *  project. Without one the field takes a paste as text. */
  readonly uploadOwner?: ArtifactOwner;
}): ReactNode {
  const tooltips = useTooltips();
  const uploading = upload.pending > 0;
  // One area can be on the page twice (a decision block and the margin sheet, a card and its
  // drawer), so the line each button names is this instance's.
  const reasonBase = useId();
  const reasonId = {} as Record<Name, string>;
  for (const name of Object.keys(refusals) as Name[]) reasonId[name] = `${reasonBase}-${name}`;

  return (
    <div className="space-y-2">
      {field({
        body: draft,
        onChange: (change) => onDraftChange(change.body),
        onDrop:
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!readOnly) upload.drop(event, uploadOwner, uploadDraft);
              },
        // A draft sent before its upload lands would go without the picture; the buttons say the
        // upload is what holds them (`busy`), and this key says nothing at all.
        onKeyDown: (event) => submitOnModifiedEnter(event, { disabled: uploading || readOnly }),
        onPaste:
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!readOnly) upload.paste(event, uploadOwner, uploadDraft);
              },
        readOnly,
        // Two lines, as `main`'s answer and reply boxes drew, so a phone draws the placeholder
        // whole: one line is the touch floor's 44 px.
        rows: 2,
      })}
      <DraftUploadStatus upload={upload} />
      {/* The reason a send refuses, on screen beside the draft it was refused over, and the
          description its button carries (`RefusableButton`'s `refusalShownBy`). */}
      {(Object.entries(refusals) as [Name, string | null][]).map(([name, reason]) =>
        reason === null ? null : (
          <p
            className={`text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.reason}`}
            id={reasonId[name]}
            key={name}
          >
            {reason}
          </p>
        )
      )}
      {actions({ reasonId })}
    </div>
  );
}
