import { type ReactNode, useState } from "react";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { InPlaceField } from "./InPlaceField";

const NONE_WRITTEN = "No description is written.";

/**
 * A priority's description, in the one shape every reading of it takes: the markdown it is,
 * drawn rendered, and said in one line where none is written. For a person who may write it, a
 * click on the rendered text opens the editor in its place, which saves as it is written and
 * gives the rendered block back when it is left. There is no edit mode beyond that and no Save
 * button. The priority's own page and the open row of the priorities table draw it alike, so
 * what a description does does not depend on where it is read.
 *
 * The rendered text is not itself a control. A control wrapping it would either be named for
 * something other than what it shows (WCAG 2.5.3, axe's `label-content-name-mismatch`) or be
 * named by the whole description, and either way a link in the description would be a control
 * inside a control (axe's `nested-interactive`), which a screen reader cannot reach. The
 * keyboard's and the screen reader's way in is a real button beside the text, named by the
 * words it shows ("Edit the description of P3") and drawn over the block's corner only while it
 * holds the focus, so the page a mouse reads is the rendered text alone.
 */
export function PriorityDescription({
  canWrite,
  description,
  label,
  onSave,
  rows,
}: {
  canWrite: boolean;
  description: string;
  /** What the description is called, which names the priority it belongs to: "Description of
   *  P3". The editor and the group around the rendered text carry it, and the button that
   *  opens the editor reads it after "Edit the": "Edit the description of P3". */
  label: string;
  onSave: (next: string) => void;
  /** How tall the editor is; the field's own height when unset. */
  rows?: number;
}): ReactNode {
  const [editing, setEditing] = useState(false);
  // What this reader last wrote, over which text of the server's: the rendered block shows it
  // until the server's own text changes, since the write is a round trip and the block must
  // not show the old text meanwhile.
  const [written, setWritten] = useState<{ readonly text: string; readonly over: string }>();
  const shown = written?.over === description ? written.text : description;

  if (canWrite && editing)
    return (
      <InPlaceField
        autoFocus
        label={label}
        multiline
        onDone={() => setEditing(false)}
        onSave={(next) => {
          setWritten({ text: next, over: description });
          onSave(next);
        }}
        placeholder="What this priority is, in markdown"
        rows={rows}
        value={shown}
      />
    );
  const body =
    shown.trim() === "" ? (
      <p className={textMutedOnCanvas}>
        {canWrite ? `${NONE_WRITTEN} Click here to write one.` : NONE_WRITTEN}
      </p>
    ) : (
      <MarkdownBody markdown={shown} />
    );
  if (!canWrite) return body;
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset groups form fields, and this holds rendered prose and the one button that edits it.
    <div aria-label={label} className="relative" role="group">
      <button
        className={`sr-only text-xs focus:not-sr-only focus:absolute focus:top-1 focus:right-1 focus:z-10 focus:rounded-md focus:border focus:px-2 focus:py-1 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${focusVisibleRing}`}
        onClick={() => setEditing(true)}
        type="button"
      >
        {`Edit the ${label.charAt(0).toLowerCase()}${label.slice(1)}`}
      </button>
      {/* biome-ignore lint/a11y/useKeyWithClickEvents lint/a11y/noStaticElementInteractions: the
          button above is the keyboard's way in; the text is no control, so a link in it stays
          one a screen reader reaches. */}
      <div
        className={`cursor-text rounded-md border px-2 py-1 ${borderDefault}`}
        onClick={(event) => {
          // A link in the description is followed, not edited.
          if (event.target instanceof Element && event.target.closest("a") !== null) return;
          setEditing(true);
        }}
      >
        {body}
      </div>
    </div>
  );
}
