import { dispatchWritingBox } from "@legion/contracts";
import type { EditorReferencePicker, ReferencePickerCaret } from "@legion/proof-editor";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { textMutedOnCanvas } from "../../theme/classes";
import { RefPickerList } from "../refs/RefPickerList";
import { useRefPicker } from "../refs/useRefPicker";
import type { EditorHandle } from "./editor";

/** How far under the caret the list hangs, so it clears the line being typed. */
const LIST_GAP_PX = 4;

/**
 * The reference picker in a document's body: the same picker every other box has
 * (`features/refs/useRefPicker.ts`), hosted by the editor rather than by the shared text field,
 * because the body is a ProseMirror document and not a textarea.
 *
 * The boundary is the one `DocumentReferences` already draws for the titles, the other way
 * round: the editor owns the document and this owns the picker. The editor reports the text of
 * the caret's own block up to the caret and offers it every key
 * (`@legion/proof-editor`'s `src/reference-picker.ts`); this reads the open door and the query
 * from those characters, draws the rows, and answers a pick as the words to write and the
 * stretch of that same text they replace, which the editor writes as one transaction. So the
 * pick reaches every other reader of the document through collaboration and stands in the
 * document's stored text, which is what a drawn title never does.
 */
export function DocumentRefPicker({
  editor,
  enabled,
  issueKey,
  project,
}: {
  editor: EditorHandle | undefined;
  /** False while the document takes no edits: a reader who cannot type has nothing to pick. */
  enabled: boolean;
  /** The issue this document belongs to, whose people and stopped roles the picker offers. */
  issueKey?: string;
  /** The project a document of a project belongs to, whose components it offers. */
  project?: string;
}): ReactNode {
  const writing = dispatchWritingBox("document-body", "document-text");
  const [caret, setCaret] = useState<ReferencePickerCaret | undefined>(undefined);
  const picker = useRefPicker({
    caret: caret?.text.length ?? 0,
    enabled: caret !== undefined,
    issueKey,
    project,
    text: caret?.text ?? "",
  });
  // The key handler below reads the picker and the editor as they stand rather than as the
  // render that built the bridge saw them, so the bridge is built once and the editor is told
  // about it once: a fresh value on every render would be a transaction on every render.
  const pickerNow = useRef(picker);
  pickerNow.current = picker;
  const editorNow = useRef(editor);
  editorNow.current = editor;

  const bridge = useMemo<EditorReferencePicker>(
    () => ({
      onCaret: setCaret,
      pressKey: (key) => {
        const result = pickerNow.current.pressKey(key);
        if (result.action === "ignored") return false;
        if (result.action === "took") editorNow.current?.insertReference(result.insertion);
        return true;
      },
    }),
    []
  );

  useEffect(() => {
    if (!enabled) return;
    editor?.setReferencePicker(bridge);
    return () => {
      editor?.setReferencePicker(null);
      setCaret(undefined);
    };
  }, [bridge, editor, enabled]);

  if (!enabled) return null;
  return (
    <>
      {caret === undefined || !picker.open ? null : (
        // Fixed, by the caret the editor reported: the list belongs to where the reader is
        // typing, which in a document of any length is nowhere near its foot.
        <div
          className="fixed z-50 w-80"
          style={{ left: `${caret.left}px`, top: `${caret.bottom + LIST_GAP_PX}px` }}
        >
          <RefPickerList
            label="References"
            onTake={(insertion) => editor?.insertReference(insertion)}
            picker={picker}
          />
        </div>
      )}
      {/* What a pick does here, in the writing list's own words, as every other box says it. */}
      <p className={`mt-2 text-xs ${textMutedOnCanvas}`} data-testid="document-body-hint">
        {writing.hint}
      </p>
    </>
  );
}
