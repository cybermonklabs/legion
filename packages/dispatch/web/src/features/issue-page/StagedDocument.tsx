import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import type { EditorHandle } from "../doc/editor";
import { type DocumentToolbar, ProofDocument } from "../doc/ProofDocument";
import { DocumentStage, STAGE_IN_PANEL, useDocumentStage } from "./document-stage";
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
 * `resting` says whether the document's stage rests here while no row of the tree holds it.
 * The editor for an edited row rests nowhere, because that row is holding the stage itself.
 *
 * It draws the document's `ProofDocument` itself, as `ArtifactDocument` does for an address that
 * names no comment and no ask - the reader's read, then the document - since no address lands
 * here, and so it can hand the row the live editor (`onEditorHandle`), which the row holds every
 * edit of to its node's block (`useNodeEditing.ts`).
 */
export function StagedDocument({
  artifact,
  isClosed,
  issueKey,
  onEditorHandle,
  onToolbarChange,
  onVersionChange,
  resting = true,
  showDiff,
}: {
  artifact: Artifact;
  isClosed: boolean;
  issueKey: string;
  /** Takes the document's live editor as it is made, and `undefined` as it is torn down. */
  onEditorHandle?: (handle: EditorHandle | undefined) => void;
  onToolbarChange: (toolbar: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  resting?: boolean;
  showDiff: boolean;
}): ReactNode {
  const stage = useDocumentStage(artifact.id, STAGE_IN_PANEL);
  const user = useQuery(whoAmIQuery());
  return (
    <>
      {resting ? <div className="flex min-w-0 flex-col" ref={stage} /> : null}
      <DocumentStage artifactId={artifact.id}>
        <EditorBox>
          {user.data === undefined ? (
            <p>Loading document…</p>
          ) : (
            <ProofDocument
              artifact={artifact}
              highlight={undefined}
              highlightTerm={undefined}
              isClosed={isClosed}
              key={artifact.id}
              onEditorHandle={onEditorHandle}
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
    </>
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
 * width. A reference the editor draws as its issue's title, one line cut at 24em (`lib.css`), is
 * cut at its line's end too where that comes first, as in a bullet on a phone. Both rules carry
 * `!`, since the editor's stylesheets stand outside Tailwind's layers and would win over a
 * utility. A collaborator's caret label near a line's end hangs back toward the line's start
 * rather than past the box's end (`@legion/proof-editor`'s `collab-cursor-plugin.ts`).
 */
export function EditorBox({ children }: { children: ReactNode }): ReactNode {
  return (
    <div className="min-w-0 [overflow-anchor:none]">
      <div
        className="max-h-[calc(100dvh-var(--issue-identity-height,0px)-var(--issue-composer-height,0px))] overflow-y-auto overscroll-contain [&_.ProseMirror]:[overflow-wrap:anywhere]! [&_.dispatch-reference]:max-w-[min(24em,100%)]!"
        {...{ [EDITOR_BOX_ATTRIBUTE]: "" }}
      >
        {children}
      </div>
    </div>
  );
}
