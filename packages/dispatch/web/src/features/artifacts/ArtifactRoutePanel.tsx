import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { dangerText, linkHoverText, linkText } from "../../theme/classes";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath, type DispatchRoute } from "../refs/routes";
import { ArtifactDetails } from "./ArtifactDetails";
import { ArtifactBlobView, ArtifactHeader } from "./ArtifactHeader";
import { IssueReferenceClosure } from "./IssueReferenceClosure";

/**
 * One artifact of an issue, as its own address `/issues/<key>/artifacts/<slug>` opens it: drawn
 * in the issue page's main column in place of the outline (dispatch://INFRA-1196 R3), with its
 * header, its editor or its picture, its versions, their downloads and what references it, and
 * the issue's reference closure, which the Artifacts list it replaces drew at its head.
 *
 * A document's version control is the one beside the filters, which reads the document opened
 * here (`features/issue/VersionControl.tsx`), so its header draws none of its own; a file's
 * header keeps its version picker, since the control beside the filters reads documents alone.
 * The panel is named by what it holds - the artifact's name - and never by a tab that is gone.
 */
export function ArtifactRoutePanel({
  artifact,
  artifactRoute,
  children,
  isClosed,
  toolbar,
}: {
  artifact: Artifact | undefined;
  artifactRoute: Extract<DispatchRoute, { kind: "artifact" }>;
  /** The document's editor, drawn under its header; nothing for a file. */
  children: ReactNode;
  isClosed: boolean;
  toolbar: DocumentToolbar | undefined;
}): ReactNode {
  if (artifact === undefined) {
    return (
      <section aria-label={artifactRoute.slug} className="space-y-3">
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
  const isDocument = artifact.kind === "doc";
  return (
    <section aria-label={artifact.name} className="space-y-4" data-testid="opened-artifact">
      <ArtifactHeader
        artifact={artifact}
        highlight
        isClosed={isClosed}
        showVersionPicker={!isDocument}
        toolbar={isDocument ? toolbar : undefined}
        version={artifactRoute.version}
        versionControls={false}
      >
        {isDocument ? (
          children
        ) : (
          <ArtifactBlobView artifact={artifact} version={artifactRoute.version} />
        )}
      </ArtifactHeader>
      <ArtifactDetails artifact={artifact} />
      <IssueReferenceClosure issueKey={artifactRoute.key} />
    </section>
  );
}
