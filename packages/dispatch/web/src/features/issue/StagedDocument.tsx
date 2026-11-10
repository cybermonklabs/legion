import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { DocumentStage, STAGE_IN_PANEL, useDocumentStage } from "./document-stage";

/**
 * One document's editor, on that document's own stage (dispatch://INFRA-1196 R16,
 * `document-stage.tsx`).
 *
 * Every place the issue page opens a document draws it through this: the Spec panel, the two
 * role-document panels, the panel an `/artifacts/<slug>` address opens, and the page's own
 * editor for a row of the tree being edited in a document no panel has open. They were four
 * copies of the same block, and a guard changed in one of them left a document with two
 * editors - two sockets on one document - or a row with none.
 *
 * `resting` says whether this place is where the document's stage rests while no row of the
 * tree holds it: a panel is, and the page's editor for an edited row is not, because that row
 * is holding the stage itself.
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
  /** The ask an address names for this document's editor to open (`?ask=`). */
  askId?: string;
  /** The comment an address names the same way (`?comment=`). */
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
      </DocumentStage>
    </>
  );
}
