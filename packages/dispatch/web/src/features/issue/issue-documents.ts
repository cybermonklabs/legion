import { artifactAtAddress, primarySpec, roleDocument } from "../../api/issue-cache";
import type { Artifact, IssueDetails } from "../../api/types";
import {
  DECISIONS_DOCUMENT_SLUG,
  type IssueRoute,
  type IssueTab,
  isPrimaryDocumentArtifactRoute,
  issueTabForRoute,
} from "../refs/routes";

/** The tabs of the issue page that draw one document of the issue. */
export type DocumentTab = Extract<
  IssueTab,
  "spec" | "verification" | "plan" | "decisions" | "artifacts"
>;

/** What the issue page draws at a route. */
export interface IssueDocuments {
  /** The tab the route opens. */
  activeTab: IssueTab;
  /** Whether the route names the spec by its slug (`artifacts/<slug>`), which the Spec tab draws. */
  isPrimaryArtifactRoute: boolean;
  /** The document each tab draws: the Spec tab the spec, the Verification and Plan tabs the
   *  issue's document of that role, the Decisions tab its `decisions.md`, and the Artifacts tab the
   *  document the route names by slug, which is none on the list of the artifacts and on the
   *  spec's own slug (the Spec tab draws it there). */
  drawn: Record<DocumentTab, Artifact | undefined>;
}

/** The one choice of which document the issue page draws on each tab: `IssuePage.tsx` draws each
 *  tab's panel from it, and the dashboard's pages file (`dashboard-pages-file.ts`) reads from it
 *  which pages draw a document, so the pages file cannot name a page that draws another one. */
export function issueDocuments(
  route: IssueRoute,
  issue:
    | Pick<
        IssueDetails,
        | "artifacts"
        | "primary_artifact_id"
        | "verification_artifact_id"
        | "plan_artifact_id"
        | "decisions_artifact_id"
      >
    | undefined
): IssueDocuments {
  const primaryArtifact = primarySpec(issue);
  // A page opened at a document's current name, which a reference may write where its slug kept
  // an earlier name, shows that document as the link to it drew it (`artifactAtAddress`).
  const selectedArtifact =
    route.kind === "artifact"
      ? artifactAtAddress(issue?.artifacts, route.slug)
      : primaryArtifact;
  const isPrimaryArtifactRoute = isPrimaryDocumentArtifactRoute(route, selectedArtifact);
  return {
    activeTab: issueTabForRoute(route, selectedArtifact),
    isPrimaryArtifactRoute,
    drawn: {
      spec: primaryArtifact,
      verification: roleDocument(issue, "verification"),
      plan: roleDocument(issue, "plan"),
      decisions: issue?.artifacts?.find(
        (artifact) => artifact.slug === DECISIONS_DOCUMENT_SLUG && artifact.kind === "doc"
      ),
      artifacts:
        route.kind === "artifact" && !isPrimaryArtifactRoute ? selectedArtifact : undefined,
    },
  };
}
