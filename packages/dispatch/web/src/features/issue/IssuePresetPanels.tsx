import type { ReactNode } from "react";

import type { Artifact, UserState } from "../../api/types";
import { ActivityTab } from "../activity/ActivityTab";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { PlanTab } from "../plan/PlanTab";
import type { IssueRoute, IssueTab } from "../refs/routes";
import type { DrawnDocuments } from "./drawn-documents";
import { RoleDocumentPanel } from "./RoleDocumentPanel";
import { SpecRequirements } from "./SpecRequirements";
import { StagedDocument } from "./StagedDocument";

/**
 * What each preset draws beside the tree (dispatch://INFRA-1196 R16): the spec, the
 * Verification and Plan documents, whatever an `/artifacts/<slug>` address opens, the one
 * editor a row of the tree is being edited in, and the Activity list.
 *
 * These are plain panels, not tabpanels: the presets are queries over one tree and the page has
 * no tablist of panels any more. A panel the reader has never opened stays unmounted, and one
 * they have opened keeps rendering while hidden, which is what keeps the spec's editor
 * connected across a preset change (`activatedTabs`).
 *
 * It is a component of its own because the page's column is the identity block, the body and
 * this: the first two decide the page's shape, and this decides nothing - which documents are
 * drawn is `drawn-documents.ts`'s one answer, which the page reads too, so no panel here can
 * come to disagree with the page's own editor for an edited row.
 */
export function IssuePresetPanels({
  activatedTabs,
  activeTab,
  artifactRoute,
  askId,
  commentId,
  diffs,
  documents,
  editedDocument,
  highlightTerm,
  isClosed,
  isPrimaryArtifactRoute,
  issueKey,
  onShowDiff,
  onToolbarSink,
  onVersionChange,
  planArtifact,
  route,
  selectedArtifact,
  specOpensItem,
  specVersion,
  state,
  toolbars,
  verificationArtifact,
}: {
  activatedTabs: Record<IssueTab, boolean>;
  activeTab: IssueTab;
  artifactRoute: Extract<IssueRoute, { kind: "artifact" }> | undefined;
  askId: string | undefined;
  commentId: string | undefined;
  /** Which documents are drawn against their previous version. */
  diffs: ReadonlySet<string>;
  /** Which document each panel below draws (`drawn-documents.ts`). */
  documents: DrawnDocuments;
  /** The document a row of the tree is being edited in, where no panel here draws it. */
  editedDocument: Artifact | undefined;
  highlightTerm: string | undefined;
  isClosed: boolean;
  isPrimaryArtifactRoute: boolean;
  issueKey: string;
  onShowDiff: (artifactId: string, next: boolean) => void;
  onToolbarSink: (artifactId: string) => (next: DocumentToolbar | undefined) => void;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  /** The plan the Plan preset frames, drawn whether or not the issue has one. */
  planArtifact: Artifact | undefined;
  route: IssueRoute;
  /** The artifact an `/artifacts/<slug>` address names, document or blob, which that panel
   *  draws the header and the details of. */
  selectedArtifact: Artifact | undefined;
  /** Whether the `?ask=`/`?comment=` an address carries is the spec's to open: only where the
   *  Spec preset is what the address opens, since on every other preset the tree is what the
   *  address names and an editor focusing the item would take the focus off its row (R19). */
  specOpensItem: boolean;
  specVersion: number | undefined;
  state: UserState | undefined;
  toolbars: ReadonlyMap<string, DocumentToolbar>;
  verificationArtifact: Artifact | undefined;
}): ReactNode {
  const { plan, selected, spec, verification } = documents;
  return (
    <>
      <div aria-hidden={activeTab !== "spec"} hidden={activeTab !== "spec"} id="issue-spec-panel">
        {spec === undefined ? null : (
          <div className="space-y-4">
            <StagedDocument
              artifact={spec}
              askId={specOpensItem ? askId : undefined}
              commentId={specOpensItem ? commentId : undefined}
              highlightTerm={highlightTerm}
              isClosed={isClosed}
              issueKey={issueKey}
              onToolbarChange={onToolbarSink(spec.id)}
              onVersionChange={(version) => {
                onShowDiff(spec.id, false);
                onVersionChange(spec, version);
              }}
              showDiff={diffs.has(spec.id)}
              version={specVersion}
            />
            <SpecRequirements issueKey={issueKey} />
          </div>
        )}
      </div>
      <RoleDocumentPanel
        activated={activatedTabs.verification}
        active={activeTab === "verification"}
        artifact={verificationArtifact}
        issueKey={issueKey}
        tab="verification"
      >
        {verification === undefined ? null : (
          <StagedDocument
            artifact={verification}
            isClosed={isClosed}
            issueKey={issueKey}
            onToolbarChange={onToolbarSink(verification.id)}
            onVersionChange={(version) => onVersionChange(verification, version)}
            showDiff={diffs.has(verification.id)}
          />
        )}
      </RoleDocumentPanel>
      <RoleDocumentPanel
        Tab={PlanTab}
        activated={activatedTabs.plan}
        active={activeTab === "plan"}
        artifact={planArtifact}
        issueKey={issueKey}
        tab="plan"
      >
        {plan === undefined ? null : (
          <StagedDocument
            artifact={plan}
            isClosed={isClosed}
            issueKey={issueKey}
            onToolbarChange={onToolbarSink(plan.id)}
            onVersionChange={(version) => onVersionChange(plan, version)}
            showDiff={diffs.has(plan.id)}
          />
        )}
      </RoleDocumentPanel>
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={(next) => onShowDiff(selectedArtifact?.id ?? "", next)}
        route={route}
        showDiff={diffs.has(selectedArtifact?.id ?? "")}
        toolbar={toolbars.get(selectedArtifact?.id ?? "")}
      >
        {selected === undefined ? null : (
          <StagedDocument
            artifact={selected}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            issueKey={issueKey}
            onToolbarChange={onToolbarSink(selected.id)}
            onVersionChange={(version) => {
              onShowDiff(selected.id, false);
              onVersionChange(selected, version);
            }}
            showDiff={diffs.has(selected.id)}
            version={artifactRoute?.version}
          />
        )}
      </ArtifactRoutePanel>
      {/* The document a row of the tree is being edited in, where no panel above draws it:
          one editor per document, so the row takes this one onto its own stage. It rests
          nowhere while no row holds it, because no panel of this page is showing it. */}
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
      <div
        aria-hidden={activeTab !== "activity"}
        hidden={activeTab !== "activity"}
        id="issue-activity-panel"
      >
        {activatedTabs.activity ? <ActivityTab issueKey={issueKey} state={state} /> : null}
      </div>
    </>
  );
}
