import { type ReactNode, useState } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact, IssueDetails } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath } from "../refs/routes";
import { StagedDocument } from "./StagedDocument";
import { useToolbarSink } from "./useDocumentToolbars";

/**
 * How a body that draws the outline's rows lets one be edited in its document's one editor:
 * `onEditing`, handed to the rows, names the document a row is being edited in, and `panels` is
 * that document's editor (`IssuePresetPanels`), drawn by the body for the row to take onto its
 * own stage, where a version picked opens that document at it. The toolbar sink is read alone,
 * which never changes: the toolbars it fills are no part of the body, so each one an editor
 * publishes while a row is edited draws the body no more.
 *
 * The outline's body imports it (`slots/body.tsx`), and it is exported for the Plan view's chart
 * too, which is to edit a step as a row of the outline is edited.
 */
export function useOutlineEditing(issue: IssueDetails): {
  readonly onEditing: (artifactId: string | undefined) => void;
  readonly panels: ReactNode;
} {
  const navigate = useNavigate();
  const toolbarSink = useToolbarSink();
  const [editing, setEditing] = useState<string | undefined>(undefined);
  const edited =
    editing === undefined
      ? undefined
      : issue.artifacts?.find((artifact) => artifact.id === editing);
  return {
    onEditing: setEditing,
    panels: (
      <IssuePresetPanels
        editedDocument={edited}
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
