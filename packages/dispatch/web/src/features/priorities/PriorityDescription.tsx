import { dispatchWritingBox } from "@legion/contracts";
import { type ReactNode, useEffect, useRef, useState } from "react";
import type { DisplayTree } from "../../api/types";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import { MarkdownBody } from "../refs/MarkdownBody";
import { type InPlaceDraft, useInPlaceDraft } from "./useInPlaceDraft";

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
  descriptionTree,
  label,
  onSave,
  rows,
}: {
  canWrite: boolean;
  description: string;
  /** The reading Go made of `description` (`description_tree`), which the rendered block draws
   *  while it shows what Dispatch holds. */
  descriptionTree: DisplayTree | undefined;
  /** What the description is called, which names the priority it belongs to: "Description of
   *  P3". The editor and the group around the rendered text carry it, and the button that
   *  opens the editor reads it after "Edit the": "Edit the description of P3". */
  label: string;
  /** Writes `next`, answering with the write's promise (`useInPlaceDraft`). */
  onSave: (next: string) => Promise<unknown>;
  /** How tall the editor is; the field's own height when unset. */
  rows?: number;
}): ReactNode {
  const [editing, setEditing] = useState(false);
  // The draft is held here rather than in the editor, so it outlasts the editor: a text whose
  // write is still out when the reader leaves, and a refused text they open the editor on again,
  // are still theirs (`useInPlaceDraft`).
  const edit = useInPlaceDraft(description, onSave, () => setEditing(false));
  // The rendered block reads what Dispatch is about to hold: the reader's text until the write's
  // answer is read, and Dispatch's own after that, or once the write is refused. The reader's
  // text is a write the server has not stored yet, so it has no tree and is parsed; Dispatch's
  // own is drawn from the tree the server read of it.
  const shown = edit.sending ?? description;
  const shownTree = edit.sending === undefined ? descriptionTree : undefined;

  if (canWrite && editing) return <DescriptionEditor edit={edit} label={label} rows={rows} />;
  const body =
    shown.trim() === "" ? (
      <p className={textMutedOnCanvas}>
        {canWrite ? `${NONE_WRITTEN} Click here to write one.` : NONE_WRITTEN}
      </p>
    ) : (
      // No `source`: why a priority's description is no record is the reading list's
      // `priority-description` (`DISPATCH_READING_SURFACES`).
      <MarkdownBody markdown={shown} surface="priority-description" tree={shownTree} />
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

/** The writing list's row of a priority's description, which makes its editor the shared text
 *  field: the line under it is the row's hint. */
const DESCRIPTION_BOX = dispatchWritingBox("priority-description", "priority-description");

/** The editor a click on the description opens: the shared text field with the reference picker
 *  in it (`DISPATCH_WRITING_BOXES`, `priority-description`), over the draft its host holds, saved
 *  as it is written (`useInPlaceDraft`) and holding the focus from the moment it opens, with the
 *  caret at the end of the text. */
function DescriptionEditor({
  edit: { draft, hold, leave, type },
  label,
  rows = 8,
}: {
  edit: InPlaceDraft;
  label: string;
  rows?: number;
}): ReactNode {
  const field = useRef<MentionFieldHandle>(null);
  useEffect(() => {
    const node = field.current?.textarea() ?? null;
    if (node === null) return;
    node.focus();
    node.setSelectionRange(node.value.length, node.value.length);
  }, []);
  return (
    <MentionField
      body={draft}
      className="font-mono text-sm"
      fieldRef={field}
      label={label}
      labelHidden
      onBlur={leave}
      onChange={(change) => type(change.body)}
      onFocus={hold}
      placeholder="What this priority is, in markdown"
      rows={rows}
      writing={DESCRIPTION_BOX}
    />
  );
}
