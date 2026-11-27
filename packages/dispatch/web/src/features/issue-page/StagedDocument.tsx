import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type Ref, useLayoutEffect, useRef } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import type { EditorConstruction, EditorHandle } from "../doc/editor";
import { type DocumentToolbar, ProofDocument } from "../doc/ProofDocument";
import { EVENT_FRESH } from "../refs/reference-target";
import { useReportAdmission } from "./document-admission";
import { DocumentStage } from "./document-stage";
import { EDITOR_BOX_ATTRIBUTE } from "./node-caret";

/**
 * One document's editor, on that document's own stage (`document-stage.tsx`).
 *
 * The body draws it, through `IssuePresetPanels.tsx`, as the page's one editor of the document
 * a row of the outline is being edited in: one document has one room, so the row takes this
 * editor onto its own stage rather than opening a second connection to the room, and holds it
 * there in a box no taller than the window (`EditorBox`). It is not what opens a document at
 * its own address.
 *
 * It draws the document's `ProofDocument` itself, as `ArtifactDocument` does for an address that
 * names no comment and no ask - the reader's read, then the document - since no address lands
 * here, and so it can hand the row the live editor (`onEditorHandle`), which the row holds every
 * edit of to its node's block (`useNodeEditing.ts`).
 *
 * It reports what the document's room admitted the page to (`document-admission.tsx`), so no row
 * of the outline of a document its room admitted read-only draws the cue that it takes typing.
 * The room's answer reaches the page only as `ProofDocument`'s mark on its article
 * (`READ_ONLY_ATTRIBUTE`), which this reads in the box as the mark changes and as the editor is
 * made. On an open issue the mark says read-only only for the room's answer, so that is reported
 * as it comes. It says the document takes typing for the room's answer too, but also from the
 * moment the article is drawn, as `ProofDocument` starts every connection, before the room has
 * answered; so that is reported only once the editor is made, which `ProofDocument` does only
 * after the room has admitted its socket: as the editor is made, and at each later admission while
 * it stands - a reconnect's, a reopened issue's. On a closed issue the mark says read-only for the
 * close itself, as the server admits every socket of a closed issue read-only, so nothing is read
 * while the issue is closed: that admission, held, would keep the cue off the document's rows once
 * the issue is reopened. A report outlives the editor: the admission stands for the page until a
 * room of that document answers again.
 */
export function StagedDocument({
  artifact,
  isClosed,
  issueKey,
  onEditorConstructed,
  onEditorHandle,
  onNoEditor,
  onToolbarChange,
  onVersionChange,
  showDiff,
}: {
  artifact: Artifact;
  isClosed: boolean;
  issueKey: string;
  /** Takes the document's editor as it is constructed, before its binding draws a block, so
   *  the row's hold on its edits stands from the first one (`ProofDocument`'s
   *  `onEditorConstructed`). */
  onEditorConstructed?: (editor: Pick<EditorConstruction, "view">) => void;
  /** Takes the document's live editor as it is made, and `undefined` as it is torn down. */
  onEditorHandle?: (handle: EditorHandle | undefined) => void;
  /** Told where no editor is coming: the document could not make or build one. */
  onNoEditor?: () => void;
  onToolbarChange: (toolbar: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  showDiff: boolean;
}): ReactNode {
  const user = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const reportAdmission = useReportAdmission();
  const box = useRef<HTMLDivElement>(null);
  // Whether the editor is made, and so the room has answered: `ProofDocument` hands it over as it
  // is made and takes it back as it is torn down.
  const editorMade = useRef(false);
  // A layout effect, so the commit that closes the issue takes the observer off before it hears
  // the mark the close sets: `disconnect` drops what it had not yet heard. The observer watches
  // the whole box, so it hears the mark of an article drawn in it after it starts.
  useLayoutEffect(() => {
    const element = box.current;
    if (isClosed || element === null) return;
    const observer = new MutationObserver(() => {
      const readOnly = articleReadOnly(element);
      if (readOnly === true || (readOnly === false && editorMade.current)) {
        reportAdmission(artifact.id, readOnly);
      }
    });
    observer.observe(element, { attributeFilter: [READ_ONLY_ATTRIBUTE], subtree: true });
    return () => observer.disconnect();
  }, [artifact.id, isClosed, reportAdmission]);
  // `ProofDocument` reads the latest of these, so one made at each render reopens nothing.
  const takeEditor = (handle: EditorHandle | undefined) => {
    editorMade.current = handle !== undefined;
    // The room answered before the editor was made, so the mark already says what it answered,
    // which an admission to write may not have changed.
    const readOnly =
      handle === undefined || isClosed || box.current === null
        ? undefined
        : articleReadOnly(box.current);
    if (readOnly !== undefined) reportAdmission(artifact.id, readOnly);
    onEditorHandle?.(handle);
  };
  return (
    <DocumentStage artifactId={artifact.id}>
      <EditorBox ref={box}>
        {user.data === undefined ? (
          <p>Loading document…</p>
        ) : (
          <ProofDocument
            artifact={artifact}
            highlight={undefined}
            highlightTerm={undefined}
            isClosed={isClosed}
            key={artifact.id}
            onEditorConstructed={onEditorConstructed}
            onEditorHandle={takeEditor}
            onNoEditor={onNoEditor}
            onToolbarChange={onToolbarChange}
            onVersionChange={onVersionChange}
            owner={{ key: issueKey, kind: "issue" }}
            showDiff={showDiff}
            user={user.data}
            version={undefined}
          />
        )}
      </EditorBox>
    </DocumentStage>
  );
}

/**
 * The box a row's editor scrolls in: the editor holds the whole document, and the row holding it
 * is no taller than the window and scrolls it inside itself, so the page around the row stays
 * where the reader had it. It is never taller than the room between the band that
 * stays at the top (`--issue-identity-height`) and the box at the foot of the screen
 * (`--issue-composer-height`, `identity-block.ts`), and once the editor is in it the row holds it
 * to a height that fits the whole row in that room (`node-caret.ts`'s `holdEditorAtBlock`, which
 * says how a low row and a tall body are held). A scroll that reaches either end of the box stops
 * there, rather than going on to move the page.
 *
 * The window's scroll anchoring passes over the box and everything in it: the box stands in an
 * element that takes no part in it (`overflow-anchor: none`, as the sidebar's in `app.tsx`). The
 * editor draws the names of the issues a document cites after the caret is in, so the words above
 * the caret grow, and the window, anchored on a node inside the box, scrolled the page down by as
 * much: the row being edited stood above the window, at 1440 by 900 px, until the reader typed.
 * The property is that element's and not the box's, since on the box it would turn off the box's
 * own anchoring as well: the box anchors on its own words, so the words the caret is in stay
 * where they stand in it as the names above them draw. Once the drawing has settled, the row
 * still puts the node's block at the box's top (`holdEditorAtBlock`).
 *
 * The box never scrolls sideways: its words are held to its width, a code block wraps in the
 * editor (`white-space: pre-wrap`, `@legion/proof-editor`'s `lib.css`), and a wide table or
 * diagram scrolls sideways inside itself alone, by the document's own rules (a table's in
 * `styles.css`, a diagram's in `lib.css`), so nothing here holds one. A run with no space in it
 * breaks anywhere: the editor's own `break-word` gives it no place to break where its box is
 * sized to its words, as a typed block's header sizes its title, which held a path at its whole
 * width. That rule carries `!`, since the editor's stylesheets stand outside Tailwind's layers
 * and would win over a utility. A reference the editor draws as its issue's title is one line
 * cut at 24em or at its line's end, whichever comes first (`lib.css`). A collaborator's caret
 * label near a line's end hangs back toward the line's start rather than past the box's end
 * (`@legion/proof-editor`'s `collab-cursor-plugin.ts`).
 *
 * The box carries the page's cue that a block takes typing (`dispatch-edit-cue`, `styles.css`):
 * the text cursor and the page's faint pane under the pointer, and a 1 px rule in the page's ink
 * inside its edge while the editor in it holds the focus, which takes no room in the box. It
 * draws the cue only while the document in it takes typing: one the editor admits read-only - a
 * closed issue's, or one this build's schema is out of date for, its article marked so
 * (`ProofDocument.tsx`'s `data-read-only`) - draws none of it.
 *
 * `ref` takes the box itself, where `StagedDocument` reads the article's mark.
 */
export function EditorBox({
  children,
  ref,
}: {
  children: ReactNode;
  ref?: Ref<HTMLDivElement>;
}): ReactNode {
  return (
    <div className="min-w-0 [overflow-anchor:none]">
      <div
        className="dispatch-edit-cue max-h-[calc(100dvh-var(--issue-identity-height,0px)-var(--issue-composer-height,0px))] overflow-y-auto overscroll-contain [&_.ProseMirror]:[overflow-wrap:anywhere]!"
        ref={ref}
        {...{ [EDITOR_BOX_ATTRIBUTE]: "" }}
      >
        {children}
      </div>
    </div>
  );
}

/** `ProofDocument`'s mark on the article it draws the document in: whether the document takes
 *  typing, `"false"`, or is read-only, `"true"`. */
const READ_ONLY_ATTRIBUTE = "data-read-only";

/** Whether the live document's article in `box` - the first in it, before any version's - is
 *  marked read-only, or `undefined` where none is drawn. */
function articleReadOnly(box: HTMLElement): boolean | undefined {
  const mark = box.querySelector(".dispatch-doc")?.getAttribute(READ_ONLY_ATTRIBUTE);
  return mark === undefined || mark === null ? undefined : mark === "true";
}
