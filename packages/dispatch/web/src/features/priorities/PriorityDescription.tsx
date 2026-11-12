import type { ReactNode } from "react";
import { textMutedOnCanvas } from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { InPlaceField } from "./InPlaceField";

/**
 * A priority's description, in the one shape every reading of it takes: written where it is
 * read by a person who may write it, drawn as the markdown it is for one who may not, and said
 * in one line where none is written. The priority's own page and the open row of the priorities
 * table draw it alike, so what a description looks like does not depend on where it is read.
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
  /** What the field is called, which names the priority it belongs to: "Description of P3". */
  label: string;
  onSave: (next: string) => void;
  /** How tall the field is where it is written; the field's own height when unset. */
  rows?: number;
}): ReactNode {
  if (canWrite)
    return (
      <InPlaceField
        label={label}
        multiline
        onSave={onSave}
        placeholder="What this priority is, in markdown"
        rows={rows}
        value={description}
      />
    );
  if (description.trim() === "")
    return <p className={textMutedOnCanvas}>No description is written.</p>;
  return <MarkdownBody markdown={description} />;
}
