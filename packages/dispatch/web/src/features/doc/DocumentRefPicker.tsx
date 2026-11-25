import {
  type DispatchWritingBoxRow,
  dispatchWritingBox,
} from "@legion/contracts/dispatch-writing-boxes";
import type {
  EditorReferencePicker,
  ReferenceInsertion,
  ReferencePickerCaret,
} from "@legion/proof-editor";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { useTooltips } from "../../components/tooltips";
import { pageQuietText, textMutedOnCanvas } from "../../theme/classes";
import { RefPickerList } from "../refs/RefPickerList";
import { type RefPickerInsertion, readTrigger } from "../refs/ref-picker";
import { useRefPicker } from "../refs/useRefPicker";
import type { EditorHandle } from "./editor";

/** How far under the caret the list hangs, so it clears the line being typed. */
const LIST_GAP_PX = 4;

/** The writing list's row of a document's own body, which an editor names unless it hosts the
 *  picker for another box. */
const DOCUMENT_BODY = dispatchWritingBox("document-body", "document-text");

/** A pick as the document takes it: a party's mention is the words it shows with the address
 *  it names, which the editor writes as a link mark, and every other pick is the bare address
 *  as text. The Markdown a `<textarea>` host writes for a mention (`insertion.text`) would be
 *  stored here as the escaped characters of that Markdown, which every reader of the document
 *  would then read as `[Planner](` and an address rather than as a mention (R4). A `](` the
 *  reader typed is no link's address here (the picker is told the document's links are marks),
 *  so a party picked after one is still its mention. */
function documentInsertion(insertion: RefPickerInsertion): ReferenceInsertion {
  const { end, link, start, text, trigger } = insertion;
  return link === undefined
    ? { end, start, text, trigger }
    : { end, href: link.href, start, text: link.words, trigger };
}

/** Whether a door of the picker stands open at `caret`, as the picker reads the text before it
 *  (`useRefPicker`, a document's links being marks). */
function opensDoor(caret: ReferencePickerCaret | undefined): boolean {
  return caret !== undefined && readTrigger(caret.text, caret.text.length, "marks") !== undefined;
}

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
 * document's stored text, which is what a drawn title never does. Another text drawn in the same
 * editor - a priority's description (`PriorityDescription`) - hosts the same picker under its own
 * row of the writing list (`writing`).
 */
export function DocumentRefPicker({
  editor,
  enabled,
  issueKey,
  project,
  writing = DOCUMENT_BODY,
}: {
  editor: EditorHandle | undefined;
  /** False while the document takes no edits: a reader who cannot type has nothing to pick. */
  enabled: boolean;
  /** The issue this document belongs to, whose people the picker offers. */
  issueKey?: string;
  /** The project a document of a project belongs to, whose components it offers. */
  project?: string;
  /** The box's row of the writing list, whose words the line under the editor says and whose
   *  write says which parties a pick may name: a document's body where it is left out. */
  writing?: DispatchWritingBoxRow;
}): ReactNode {
  const tooltips = useTooltips();
  const [caret, setCaret] = useState<ReferencePickerCaret | undefined>(undefined);
  const picker = useRefPicker({
    caret: caret?.text.length ?? 0,
    delivers: writing.delivers,
    enabled: caret !== undefined,
    issueKey,
    links: "marks",
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
  // The caret the picker last drew. A report draws the picker again only where it may change
  // what the picker shows: a door open at the caret drawn, or at the one reported
  // (`readTrigger`). Between two carets with no door the picker draws nothing either way, so a
  // keystroke of plain words, or the focus coming to the editor, draws nothing again.
  const drawn = useRef<ReferencePickerCaret | undefined>(undefined);

  const bridge = useMemo<EditorReferencePicker>(
    () => ({
      onCaret: (reported) => {
        if (!opensDoor(drawn.current) && !opensDoor(reported)) return;
        drawn.current = reported;
        setCaret(reported);
      },
      pressKey: (key) => {
        const result = pickerNow.current.pressKey(key);
        if (result.action === "ignored") return false;
        if (result.action === "took") {
          editorNow.current?.insertReference(documentInsertion(result.insertion));
        }
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
      drawn.current = undefined;
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
            onTake={(insertion) => editor?.insertReference(documentInsertion(insertion))}
            picker={picker}
          />
        </div>
      )}
      {/* What a pick does here, in the writing list's own words, as every other box says it.
          Inside `NoTooltips` it is the new issue page's quiet words, as the document's other
          lines there are the page's. */}
      <p
        className={`mt-2 text-xs ${tooltips ? textMutedOnCanvas : pageQuietText}`}
        data-testid={`${writing.box}-hint`}
      >
        {writing.hint}
      </p>
    </>
  );
}
