import type { ReactNode } from "react";

import type { Artifact, UserState } from "../../api/types";
import { ActivityTab } from "../activity/ActivityTab";
import type { DocumentToolbar } from "../doc/ProofDocument";
import type { IssueTab } from "../refs/routes";
import type { ConversationState } from "./page-words";
import { StagedDocument } from "./StagedDocument";

/**
 * What the page draws under the outline (dispatch://INFRA-1196 R3, R15): the one editor a row of
 * the outline is being edited in, and the history at `/activity`. No preset, filter or address
 * draws a document here: a document is opened from the node menu, at its own address, in place
 * of the outline (`IssueBody`), and `/artifacts` opens the node menu, which is the issue's list
 * of files (`NodeMenu.tsx`).
 *
 * These are plain panels, not tabpanels: the presets are queries over one tree and the page has
 * no tablist of panels any more. A panel the reader has never opened stays unmounted, and one
 * they have opened keeps rendering while hidden (`activatedTabs`).
 */
export function IssuePresetPanels({
  activatedTabs,
  activeTab,
  conversationState,
  editedDocument,
  isClosed,
  issueKey,
  onToolbarSink,
  onVersionChange,
  state,
}: {
  activatedTabs: Record<IssueTab, boolean>;
  activeTab: IssueTab;
  /** Which state the conversation is in, which is the reader's pick wherever they made one
   *  (`IssuePage`'s `pickState`). The Activity list below is the Everything state's own, so
   *  it is what says whether that list is drawn. */
  conversationState: ConversationState;
  /** The document a row of the outline is being edited in. */
  editedDocument: Artifact | undefined;
  isClosed: boolean;
  issueKey: string;
  onToolbarSink: (artifactId: string) => (next: DocumentToolbar | undefined) => void;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  state: UserState | undefined;
}): ReactNode {
  // The issue's whole history, which is the Everything state's own reading of the
  // conversation and what `/activity` opens in the page's column. A reader there who picks
  // Questions or Pinned keeps their address (`IssuePage`'s `pickState`), so the history goes
  // with the state that drew it rather than standing under a panel showing the questions
  // alone; it stays mounted, and Everything brings it back.
  const drawsActivity = activeTab === "activity" && conversationState === "everything";
  return (
    <>
      {/* The document a row of the outline is being edited in: one editor per document, so the
          row takes this one onto its own stage. It rests nowhere while no row holds it. */}
      {editedDocument === undefined ? null : (
        <StagedDocument
          artifact={editedDocument}
          isClosed={isClosed}
          issueKey={issueKey}
          onToolbarChange={onToolbarSink(editedDocument.id)}
          onVersionChange={(version) => onVersionChange(editedDocument, version)}
          resting={false}
          showDiff={false}
        />
      )}
      <div aria-hidden={!drawsActivity} hidden={!drawsActivity} id="issue-activity-panel">
        {activatedTabs.activity ? <ActivityTab issueKey={issueKey} state={state} /> : null}
      </div>
    </>
  );
}
