import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import type { DocumentToolbar } from "../doc/ProofDocument";
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
 */
export function StagedDocument({
  artifact,
  askId,
  commentId,
  highlightTerm,
  isClosed,
  issueKey,
  onToolbarChange,
  onVersionChange,
  resting = true,
  showDiff,
  version,
}: {
  artifact: Artifact;
  /** The ask an address names for this document's editor to land on (`issue-address.ts`'s
   *  `documentItem`). */
  askId?: string;
  /** The comment an address names the same way. */
  commentId?: string;
  /** What a `?q=` search term marks in the document's text. */
  highlightTerm?: string;
  isClosed: boolean;
  issueKey: string;
  onToolbarChange: (toolbar: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  resting?: boolean;
  showDiff: boolean;
  /** The version the address pins, or undefined for the document as it stands. */
  version?: number;
}): ReactNode {
  const stage = useDocumentStage(artifact.id, STAGE_IN_PANEL);
  return (
    <>
      {resting ? <div className="flex min-w-0 flex-col" ref={stage} /> : null}
      <DocumentStage artifactId={artifact.id}>
        <EditorBox>
          <ArtifactDocument
            artifact={artifact}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={onToolbarChange}
            onVersionChange={onVersionChange}
            owner={{ key: issueKey, kind: "issue" }}
            showDiff={showDiff}
            version={version}
          />
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
