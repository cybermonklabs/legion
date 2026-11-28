import { type ReactNode, useCallback, useState, useSyncExternalStore } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact, IssueDetails } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath } from "../refs/routes";
import { createHeld, type Held } from "./held-value";
import { StagedDocument } from "./StagedDocument";
import { useToolbarSink } from "./useDocumentToolbars";
import type { OnEditing, OnEditor } from "./useNodeEditing";

/** Which document a row is being edited in, and how to hand that row the editor. */
interface OutlineEdited {
  readonly artifactId: string;
  readonly onEditor?: OnEditor;
}

/**
 * How a body that draws the outline's rows lets one be edited in its document's one editor:
 * `onEditing`, handed to the rows, names the document a row is being edited in, and how to hand
 * that row the editor, and `panels` is that document's editor (`OutlineEditedPanels`), drawn by
 * the body for the row to take onto its own stage, where a version picked opens that document at
 * it. The edited document is held outside the body's state (`Held`) and read inside `panels`
 * alone, so starting, moving or ending an edit draws the editor and never the body or its
 * outline. The toolbar sink is read inside `panels` too: the toolbars it fills are no part of
 * the body.
 *
 * The outline's body imports it (`slots/body.tsx`), and it is exported for the Plan view's chart
 * too, which is to edit a step as a row of the outline is edited.
 */
export function useOutlineEditing(issue: IssueDetails): {
  readonly onEditing: OnEditing;
  readonly panels: ReactNode;
} {
  const [editing] = useState(() => createHeld<OutlineEdited | undefined>(undefined));
  // Stable: the rows call it from an effect that depends on it.
  const onEditing = useCallback<OnEditing>(
    (artifactId, onEditor) =>
      editing.set(artifactId === undefined ? undefined : { artifactId, onEditor }),
    [editing]
  );
  return {
    onEditing,
    panels: <OutlineEditedPanels editing={editing} issue={issue} />,
  };
}

/** Reads `editing` below the body, so naming a document to edit draws only the editor. */
function OutlineEditedPanels({
  editing,
  issue,
}: {
  editing: Held<OutlineEdited | undefined>;
  issue: IssueDetails;
}): ReactNode {
  const navigate = useNavigate();
  const toolbarSink = useToolbarSink();
  const current = useSyncExternalStore(editing.subscribe, editing.get);
  const edited =
    current === undefined
      ? undefined
      : issue.artifacts?.find((artifact) => artifact.id === current.artifactId);
  return (
    <IssuePresetPanels
      editedDocument={edited}
      isClosed={issue.closed_at !== null}
      issueKey={issue.key}
      onEditor={current?.onEditor}
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
  );
}

/**
 * What the body draws under the outline (`slots/body.tsx`): the one editor a row of the outline
 * is being edited in, which the row takes onto its own stage. No view or address draws a
 * document here: a document is opened from the node menu, at its own address, in place of the
 * outline. The history is no list under the outline either: it is Details'.
 */
export function IssuePresetPanels({
  editedDocument,
  isClosed,
  issueKey,
  onEditor,
  onToolbarSink,
  onVersionChange,
}: {
  /** The document a row of the outline is being edited in. */
  editedDocument: Artifact | undefined;
  isClosed: boolean;
  issueKey: string;
  /** Takes that document's live editor for the row, as the editor is made and torn down. */
  onEditor?: OnEditor;
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
      onEditorHandle={onEditor}
      onToolbarChange={onToolbarSink(editedDocument.id)}
      onVersionChange={(version) => onVersionChange(editedDocument, version)}
      showDiff={false}
    />
  );
}
