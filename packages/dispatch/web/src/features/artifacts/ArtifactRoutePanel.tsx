import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { dangerText, linkHoverText, linkText } from "../../theme/classes";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath, type DispatchRoute } from "../refs/routes";
import { ArtifactDetails } from "./ArtifactDetails";
import { ArtifactBlobView, ArtifactHeader } from "./ArtifactHeader";
import { ArtifactsTab } from "./ArtifactsTab";

/** What the Artifacts tab's panel draws once that tab has been opened: the list, or the artifact
 *  the address names. The panel element itself is the issue page's (`IssuePage.tsx`), as every
 *  other tab's is, so that it is in the page while this chunk is not. */
export function ArtifactRoutePanel({
  artifact,
  artifactRoute,
  children,
  isClosed,
  isPrimaryArtifactRoute,
  onShowDiffChange,
  route,
  showDiff,
  toolbar,
}: {
  artifact: Artifact | undefined;
  artifactRoute: Extract<DispatchRoute, { kind: "artifact" }> | undefined;
  children: ReactNode;
  isClosed: boolean;
  isPrimaryArtifactRoute: boolean;
  onShowDiffChange(next: boolean): void;
  route: DispatchRoute;
  showDiff: boolean;
  toolbar: DocumentToolbar | undefined;
}): ReactNode {
  if (route.kind === "artifacts") {
    return <ArtifactsTab isClosed={isClosed} />;
  }
  if (artifactRoute === undefined || isPrimaryArtifactRoute) {
    return null;
  }
  if (artifact === undefined) {
    return (
      <section className="space-y-3">
        <p className={dangerText}>
          No artifact {artifactRoute.slug} on {artifactRoute.key}.
        </p>
        <Link
          className={`font-medium underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: artifactRoute.key, kind: "artifacts" })}
        >
          View artifacts
        </Link>
      </section>
    );
  }
  return (
    <div className="space-y-4">
      <ArtifactHeader
        artifact={artifact}
        highlight
        isClosed={isClosed}
        onShowDiffChange={onShowDiffChange}
        showDiff={showDiff}
        showVersionPicker
        toolbar={artifact.kind === "doc" ? toolbar : undefined}
        version={artifactRoute.version}
      >
        {artifact.kind === "doc" ? (
          children
        ) : (
          <ArtifactBlobView artifact={artifact} version={artifactRoute.version} />
        )}
      </ArtifactHeader>
      <ArtifactDetails artifact={artifact} />
    </div>
  );
}
