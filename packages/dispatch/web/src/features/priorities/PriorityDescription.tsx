import { type ReactNode, useState } from "react";
import { borderDefault, focusVisibleRing, textMutedOnCanvas } from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { InPlaceField } from "./InPlaceField";

const NONE_WRITTEN = "No description is written.";

/**
 * A priority's description, in the one shape every reading of it takes: the markdown it is,
 * drawn rendered, and said in one line where none is written. For a person who may write it,
 * the rendered block is itself the way in: a click on it, or Enter on it, opens the editor in
 * its place, which saves as it is written and gives the rendered block back when it is left.
 * There is no edit mode beyond that and no Save button. The priority's own page and the open
 * row of the priorities table draw it alike, so what a description does does not depend on
 * where it is read.
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
   *  P3". The rendered block and the editor both carry it. */
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
        {canWrite ? `${NONE_WRITTEN} Click here, or press Enter, to write one.` : NONE_WRITTEN}
      </p>
    ) : (
      <MarkdownBody markdown={shown} />
    );
  if (!canWrite) return body;
  return (
    // biome-ignore lint/a11y/useSemanticElements: a button holds no paragraphs or lists, and the rendered markdown is both
    <div
      aria-label={label}
      className={`cursor-text rounded-md border px-2 py-1 ${borderDefault} ${focusVisibleRing}`}
      onClick={(event) => {
        // A link in the description is followed, not edited.
        if (event.target instanceof Element && event.target.closest("a") !== null) return;
        setEditing(true);
      }}
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget) return;
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        setEditing(true);
      }}
      role="button"
      tabIndex={0}
    >
      {body}
    </div>
  );
}
