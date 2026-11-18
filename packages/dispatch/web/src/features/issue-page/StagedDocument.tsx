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
 * (`--issue-composer-height`, `identity-block.ts`), and once the editor is in it the row narrows
 * it to the room left under where it stands (`node-caret.ts`'s `holdEditorAtBlock`). A row so low
 * on the screen that less than half that room is left under it gets half the room instead, and
 * the page scrolls just as far as brings the box on screen. A scroll that reaches either end of
 * the box stops there, rather than going on to move the page.
 */
export function EditorBox({ children }: { children: ReactNode }): ReactNode {
  return (
    <div
      className="max-h-[calc(100dvh-var(--issue-identity-height,0px)-var(--issue-composer-height,0px))] min-w-0 overflow-y-auto overscroll-contain"
      {...{ [EDITOR_BOX_ATTRIBUTE]: "" }}
    >
      {children}
    </div>
  );
}
