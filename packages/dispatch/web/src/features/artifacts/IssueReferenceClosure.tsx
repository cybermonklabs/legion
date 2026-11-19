import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import {
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  documentRoute,
  isAgentArtifact,
} from "../refs/routes";

function artifactPath(artifact: Artifact): string {
  // A conversation's picture has its own page, under its agent's session.
  if (isAgentArtifact(artifact)) {
    return buildReferencePath(documentRoute(artifact));
  }
  return artifact.issue_key === null
    ? buildProjectPath({
        kind: "document",
        project: artifact.project,
        slug: artifact.slug,
      })
    : buildIssuePath({
        key: artifact.issue_key,
        kind: "artifact",
        slug: artifact.slug,
      });
}

/** Who owns an artifact, as a References row names it: its issue, its project, or the agent
 *  whose conversation holds it. */
function ArtifactOwner({ artifact }: { artifact: Artifact }): ReactNode {
  if (artifact.issue_key !== null) return artifact.issue_key;
  if (!isAgentArtifact(artifact)) return artifact.project;
  return (
    <>
      agent <AgentName inline sessionId={artifact.session_id} storedTitle={undefined} />
    </>
  );
}

/**
 * The issue's reference closure: every artifact reachable by following references from the
 * issue's own, up to 8 hops - a different thing from one artifact's own "Referenced by" list.
 * It was drawn at the head of the issue's Artifacts list; the spec gives it no home of its own
 * (dispatch://INFRA-1196, the Design's "Where each thing goes" moves an artifact's references to
 * the artifact's own page), so it stays with the issue's artifacts on each one's own page, read
 * only once one is opened. Collapsed by default: it is background context for the issue, not
 * something worth scrolling past.
 */
export function IssueReferenceClosure({ issueKey }: { issueKey: string }): ReactNode {
  const references = useQuery({
    queryKey: ["issue", issueKey, "references"],
    queryFn: () => api.getIssueReferences(issueKey),
  });
  const members = references.data?.members ?? [];
  if (members.length === 0) {
    return null;
  }

  return (
    <details className={`rounded-lg border p-3 ${borderDefault}`}>
      <summary
        className={`min-h-11 cursor-pointer text-sm font-semibold ${textSecondaryOnSurface}`}
      >
        References ({members.length})
      </summary>
      <section aria-label="References" className="mt-3 space-y-2">
        <ul className="space-y-2">
          {members.map(({ artifact, depth, via }) => (
            <li className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={artifact.id}>
              <Link
                className={`font-medium underline ${linkText} ${linkHoverText}`}
                to={artifactPath(artifact)}
              >
                {artifact.name}
              </Link>
              <p className={textMutedOnSurface}>
                <ArtifactOwner artifact={artifact} /> · via {via.kind} · depth {depth}
              </p>
            </li>
          ))}
        </ul>
        {references.data?.truncated === true ? (
          <p className={`text-sm ${textMutedOnSurface}`}>more references beyond 8 hops</p>
        ) : null}
      </section>
    </details>
  );
}
