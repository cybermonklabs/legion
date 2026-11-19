import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { StagedDocument } from "./StagedDocument";

/**
 * What the page draws under the outline (dispatch://INFRA-1196 R3): the one editor a row of the
 * outline is being edited in, which the row takes onto its own stage. No preset, filter or
 * address draws a document here: a document is opened from the node menu, at its own address, in
 * place of the outline (`IssueBody`). The history is no list under the outline either: it is the
 * panel's (R15, `IssueMargin.tsx`).
 */
export function IssuePresetPanels({
  editedDocument,
  isClosed,
  issueKey,
  onToolbarSink,
  onVersionChange,
}: {
  /** The document a row of the outline is being edited in. */
  editedDocument: Artifact | undefined;
  isClosed: boolean;
  issueKey: string;
  onToolbarSink: (artifactId: string) => (next: DocumentToolbar | undefined) => void;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
}): ReactNode {
  // One editor per document, so the row takes this one onto its own stage. It rests nowhere
  // while no row holds it.
  return editedDocument === undefined ? null : (
    <StagedDocument
      artifact={editedDocument}
      isClosed={isClosed}
      issueKey={issueKey}
      onToolbarChange={onToolbarSink(editedDocument.id)}
      onVersionChange={(version) => onVersionChange(editedDocument, version)}
      resting={false}
      showDiff={false}
    />
  );
}
