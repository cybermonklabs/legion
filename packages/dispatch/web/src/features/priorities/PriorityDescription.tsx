import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useRef, useState } from "react";
import type { Doc } from "yjs";
import { whoAmIQuery } from "../../api/queries";
import {
  borderDefault,
  dangerText,
  hasFocusVisibleRing,
  textMutedOnCanvas,
} from "../../theme/classes";
import { colorForLogin } from "../doc/connection";
import { DocumentReferences, useReferenceClick } from "../doc/document-references";
import { createDoc, type EditorHandle } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { compiledBlockSchema } from "../doc/schema";
import { watchPicturePath } from "../refs/reference-target";
import { arrival, NOTHING_KEPT } from "./field-arrival";
import { ChangedMeanwhile, SAVE_AFTER_MS } from "./InPlaceField";

const NONE_WRITTEN = "No description is written.";

/** The editor a description is drawn in, and where it stands against the server's text
 *  (`FieldStand`). */
interface Held {
  readonly handle: EditorHandle;
  /** The markdown the editor held when it last matched the server's text: after it took that
   *  text in, and after each write it sent. Only a text that differs from it is written, since
   *  the editor reads a text back in its own form (`* ` for `- `, a soft break as a space, a
   *  closing newline), and that is no change the reader made. */
  base: string;
  /** The texts written that the server has not answered with yet, oldest first: its answer
   *  with one of them is the reader's own write coming back, not another person's text. */
  sent: readonly string[];
  /** The write waiting for the typing to rest. */
  timer: number | undefined;
}

/** Stops the write waiting for the typing to rest, if one waits. */
function stop(held: Held): void {
  window.clearTimeout(held.timer);
  held.timer = undefined;
}

/** Writes what the editor holds, at once, where it differs from the server's text. */
function save(held: Held, onSave: (next: string) => void): void {
  stop(held);
  const markdown = held.handle.getMarkdown();
  if (markdown === held.base) return;
  held.base = markdown;
  held.sent = [...held.sent, markdown];
  onSave(markdown);
}

/** Puts the server's text into the editor as it opens, unless the editor holds it already: an
 *  empty description is the empty editor. Throws, as `setMarkdown` does, for a text holding what
 *  this editor may not create. */
function take(held: Held, markdown: string): void {
  if (held.handle.getMarkdown() !== markdown) held.handle.setMarkdown(markdown);
  held.base = held.handle.getMarkdown();
}

/**
 * A priority's description, drawn and written in the editor Dispatch's documents are written in,
 * where it is read: the markdown it is, drawn as headings, lists and links, with no edit mode and
 * no Save button. It is a field of the priority and not a document: the editor holds a Yjs
 * document of its own, with no connection and no cursors, and writes no Proof mark of its own
 * (`proofMarks: false`) - no comment, suggestion or ask on a selection, and no authorship span
 * in the markdown it writes - and what it holds is written through the priority's own write as
 * markdown, once the typing rests, at once when the editor is left, and as the page goes, as
 * every field of a priority is (`InPlaceField`). The priority's own page and the open row of the
 * priorities table draw it alike, so what a description does does not depend on where it is read.
 *
 * A change of the server's text is read as a one-line field reads one (`arrival`): one that
 * answers one of the reader's own writes changes nothing they see. Another person's text is taken
 * in at once - with the reader in the editor or not - while the reader holds no unwritten change
 * and no write of theirs is out, so leaving the editor never writes the old text back over it.
 * Else it is taken in all the same, the waiting write is stopped, and the reader's text is kept
 * beside the editor (`ChangedMeanwhile`) until they use it again or let it go. Where the page
 * holds those texts (`kept`, `onKeptChange`) - in the write store, as the priority's page and
 * the priorities table do (`usePriorityWrites`) - they outlive the editor, the page and a
 * reload: a row of the table scrolled away, or the page left, and an editor drawn again shows
 * them again, and the page draws the editor while it holds any, a closed disclosure included.
 * Else the editor holds them itself, and they go with it.
 * A reader who may not write reads it in the same editor, read-only; with none written, they
 * read that none is, and a writer reads it above the empty editor.
 *
 * The editor is the one control here, a textbox named by `label`, which the keyboard reaches
 * as it reaches any field; the box around it shows the focus ring while the editor holds it.
 * Where a page holds the description in a card of its own (`boxed={false}`), the card is that
 * box: the editor draws no border, only the focus ring, and its words start where the card's do.
 */
export function PriorityDescription({
  boxed = true,
  canWrite,
  description,
  kept: keptByPage,
  label,
  onKeptChange,
  onSave,
}: {
  /** Whether the editor draws a box of its own; a page that draws it in a card draws none. */
  boxed?: boolean;
  canWrite: boolean;
  description: string;
  /** The reader's texts kept beside the editor, where the page holds them rather than the
   *  editor - in the write store (`usePriorityWrites`): `onKeptChange` hears each change of
   *  them. */
  kept?: readonly string[];
  /** What the description is called, which names the priority it belongs to: "Description of
   *  #3". The editor carries it as its accessible name. */
  label: string;
  onKeptChange?: (kept: readonly string[]) => void;
  onSave: (next: string) => void;
}): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const held = useRef<Held | undefined>(undefined);
  // The server's text the editor last took in, or saw arrive.
  const seen = useRef(description);
  // The reader's own texts, kept beside the editor while it shows another person's, where the
  // page does not hold them.
  const [ownKept, setOwnKept] = useState(NOTHING_KEPT);
  const kept = keptByPage ?? ownKept;
  const setKept = onKeptChange ?? setOwnKept;
  const latest = useRef({ canWrite, description, kept, onSave, setKept });
  latest.current = { canWrite, description, kept, onSave, setKept };
  // The handle once the editor holds the description: its name, its access and the references
  // drawn over its text follow it.
  const [editor, setEditor] = useState<EditorHandle | undefined>(undefined);
  // A text the editor could not take in, with the reason, until the server's text changes.
  const [failure, setFailure] = useState<{ readonly reason: string; readonly text: string }>();
  const { createEditor } = useContext(DocumentRuntime);
  const login = useQuery(whoAmIQuery()).data?.login;
  const queryClient = useQueryClient();
  const followReference = useReferenceClick();

  const none = description.trim() === "";
  const failed = failure?.text === description ? failure.reason : undefined;
  // A reader who may not write it has nothing to read in an empty editor.
  const drawn = failed === undefined && (canWrite || !none);

  useEffect(() => {
    const parent = root.current;
    if (!drawn || parent === null || login === undefined) return;
    let mounted = true;
    let ydoc: Doc | undefined;
    let handle: EditorHandle | undefined;
    // Every edit reaches the editor's Yjs document, the reader's and a take of the server's
    // text alike; `save` writes only what differs from that text.
    const onUpdate = () => {
      const current = held.current;
      if (current === undefined) return;
      stop(current);
      current.timer = window.setTimeout(() => save(current, latest.current.onSave), SAVE_AFTER_MS);
    };
    const mount = async () => {
      const doc = await createDoc();
      if (!mounted) {
        doc.destroy();
        return;
      }
      ydoc = doc;
      const created = await createEditor(parent, {
        access: latest.current.canWrite ? "edit" : "read",
        awareness: null,
        blockSchema: compiledBlockSchema,
        heatMapMode: "hidden",
        // A description is no document: it holds no decision block a card is drawn for.
        onAskBlockHosts: () => {},
        proofMarks: false,
        user: { color: colorForLogin(login), name: login },
        watchPicture: (route, onPicture) => watchPicturePath(queryClient, route, onPicture),
        ydoc: doc,
      });
      handle = created;
      if (!mounted) {
        void created.destroy();
        return;
      }
      const current: Held = { base: "", handle: created, sent: [], timer: undefined };
      take(current, latest.current.description);
      seen.current = latest.current.description;
      held.current = current;
      doc.on("update", onUpdate);
      setEditor(created);
    };
    mount().catch((error: unknown) => {
      void handle?.destroy();
      handle = undefined;
      if (mounted) {
        setFailure({
          reason: error instanceof Error ? error.message : String(error),
          text: latest.current.description,
        });
      }
    });

    return () => {
      mounted = false;
      // Leaving the page, or closing the disclosure, leaves the editor: what the reader wrote
      // is written now rather than lost with the timer.
      const current = held.current;
      held.current = undefined;
      if (current !== undefined) save(current, latest.current.onSave);
      ydoc?.off("update", onUpdate);
      void handle?.destroy();
      ydoc?.destroy();
      setEditor(undefined);
    };
  }, [createEditor, drawn, login, queryClient]);

  // The server's text, as it changes under the editor (`arrival`, and the component's own doc).
  useEffect(() => {
    const current = held.current;
    if (current === undefined || description === seen.current) return;
    seen.current = description;
    const markdown = current.handle.getMarkdown();
    const { kept: mine, setKept: keep } = latest.current;
    const next = arrival(description, markdown, {
      base: current.base,
      kept: mine,
      sent: current.sent,
    });
    current.sent = next.sent;
    current.base = next.base;
    if (next.kept !== mine) keep(next.kept);
    if (!next.take) return;
    // A text the editor cannot take in leaves the reader's in it: it is not written over the
    // text it could not draw as the editor closes. One it takes in is held in its own reading.
    current.base = markdown;
    try {
      current.handle.setMarkdown(description);
      current.base = current.handle.getMarkdown();
    } catch (error) {
      setFailure({
        reason: error instanceof Error ? error.message : String(error),
        text: description,
      });
    }
    // Stops the reader's write waiting for the typing to rest, which would write over the other
    // person's text, and the save the take's own Yjs update scheduled.
    stop(current);
  }, [description]);

  // The page going is the editor left, as a blur is: a write made now is let finish as the page
  // goes (`keepalive`, `api/client.ts`'s `pageGoing`), so a tab that closes writes it, and the tab
  // keeps it too, for a reload to make (`features/writes/write-storage.ts`).
  useEffect(() => {
    const leave = () => {
      if (held.current !== undefined) save(held.current, latest.current.onSave);
    };
    window.addEventListener("pagehide", leave);
    return () => window.removeEventListener("pagehide", leave);
  }, []);

  // One of the reader's kept texts, put back in the editor and written at once. With no editor
  // drawn - another person's text it could not draw - it is written as it is.
  const putBack = (text: string) => {
    setKept(NOTHING_KEPT);
    const current = held.current;
    if (current === undefined) {
      onSave(text);
      return;
    }
    current.handle.setMarkdown(text);
    save(current, onSave);
  };

  // The documents' editor is named "Document editor"; this one is named for the description it
  // holds, and says it is read-only to a reader who may not write it.
  useEffect(() => {
    if (editor === undefined) return;
    editor.setAccess(canWrite ? "edit" : "read");
    editor.view.dom.setAttribute("aria-label", label);
    if (canWrite) editor.view.dom.removeAttribute("aria-readonly");
    else editor.view.dom.setAttribute("aria-readonly", "true");
  }, [canWrite, editor, label]);

  return (
    <>
      {none ? (
        <p className={textMutedOnCanvas}>
          {canWrite ? `${NONE_WRITTEN} Write one below.` : NONE_WRITTEN}
        </p>
      ) : null}
      {failed === undefined ? null : (
        <p className={dangerText} role="alert">
          This description could not be drawn: {failed}
        </p>
      )}
      {drawn ? (
        // biome-ignore lint/a11y/useKeyWithClickEvents lint/a11y/noStaticElementInteractions: dispatch:// link clicks bubble here, as they do on a document; the editor handles the keyboard's activation of its own links.
        <div
          className={`dispatch-doc dispatch-description rounded-md px-2 py-1 has-focus-visible:ring-2 ${boxed ? `border ${borderDefault}` : "-mx-2"} ${hasFocusVisibleRing}`}
          data-read-only={!canWrite}
          onBlur={(event) => {
            if (
              event.relatedTarget instanceof Node &&
              event.currentTarget.contains(event.relatedTarget)
            )
              return;
            if (held.current !== undefined) save(held.current, latest.current.onSave);
          }}
          onClick={followReference}
        >
          <div ref={root} />
          <DocumentReferences editor={editor} source={undefined} />
        </div>
      ) : null}
      {kept.length === 0 ? null : (
        <ChangedMeanwhile
          label={label}
          mine={kept}
          onKeepTheirs={() => setKept(NOTHING_KEPT)}
          onUse={putBack}
        />
      )}
    </>
  );
}
