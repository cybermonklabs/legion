import { type ReactNode, useCallback, useState } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact, IssueDetails } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath } from "../refs/routes";
import { StagedDocument } from "./StagedDocument";
import { useToolbarSink } from "./useDocumentToolbars";
import type { OnEditing, RowEditorHandlers } from "./useNodeEditing";

/**
 * How a body that draws the outline's rows lets one be edited in its document's one editor:
 * `onEditing`, handed to the rows, names the document a row is being edited in, and how to hand
 * that row the editor, and `panels` is that document's editor (`IssuePresetPanels`), drawn by the
 * body for the row to take onto its own stage, where a version picked opens that document at it.
 * The toolbar sink is read alone, which never changes: the toolbars it fills are no part of the
 * body, so each one an editor publishes while a row is edited draws the body no more.
 *
 * The outline's body imports it (`slots/body.tsx`), and it is exported for the Plan view's chart
 * too, which is to edit a step as a row of the outline is edited.
 */
export function useOutlineEditing(issue: IssueDetails): {
  readonly onEditing: OnEditing;
  readonly panels: ReactNode;
} {
  const navigate = useNavigate();
  const toolbarSink = useToolbarSink();
  const [editing, setEditing] = useState<
    { readonly artifactId: string; readonly handlers?: RowEditorHandlers } | undefined
  >(undefined);
  // Stable: the rows call it from an effect that depends on it.
  const onEditing = useCallback<OnEditing>(
    (artifactId, handlers) =>
      setEditing(artifactId === undefined ? undefined : { artifactId, handlers }),
    []
  );
  const edited =
    editing === undefined
      ? undefined
      : issue.artifacts?.find((artifact) => artifact.id === editing.artifactId);
  return {
    onEditing,
    panels: (
      <IssuePresetPanels
        editedDocument={edited}
        handlers={editing?.handlers}
        isClosed={issue.closed_at !== null}
        issueKey={issue.key}
        onToolbarSink={toolbarSink}
        // A version picked in the edited document's own editor opens that document at it.
        onVersionChange={(artifact, version) =>
          navigate(
            buildIssuePath({
              key: issue.key,
              kind: "artifact",
              slug: artifact.slug,
              ...(version === null ? {} : { version }),
            })
          )
        }
      />
    ),
  };
}

/**
 * What the body draws under the outline (`slots/body.tsx`): the one editor a row of the outline
 * is being edited in, which the row takes onto its own stage. No view or address draws a
 * document here: a document is opened from the node menu, at its own address, in place of the
 * outline. The history is no list under the outline either: it is Details'.
 */
export function IssuePresetPanels({
  editedDocument,
  handlers,
  isClosed,
  issueKey,
  onToolbarSink,
  onVersionChange,
}: {
  /** The document a row of the outline is being edited in. */
  editedDocument: Artifact | undefined;
  /** Take that document's editor for the row: as it is constructed, so the row's hold stands
   *  before its build draws a block, again as it is made and torn down, and the word that none
   *  is coming where the document says so. */
  handlers?: RowEditorHandlers;
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
      onEditorConstructed={handlers?.constructed}
      onEditorHandle={handlers?.handle}
      onNoEditor={handlers?.none}
      onToolbarChange={onToolbarSink(editedDocument.id)}
      onVersionChange={(version) => onVersionChange(editedDocument, version)}
      showDiff={false}
    />
  );
}
