import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { linkText, textMutedOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { buildReferencePath, parseNodeReference } from "../refs/routes";

export function IssueLinks({
  issueKey,
  version,
}: {
  issueKey: string;
  version: number;
}): ReactNode {
  const query = useQuery({
    queryKey: ["issue-links", issueKey, version],
    queryFn: () => api.getIssueLinks(issueKey),
  });
  return (
    <section aria-label="Links" className="mt-2 space-y-1 text-sm">
      <h3 className={`font-semibold ${textSecondaryOnSurface}`}>Links</h3>
      {query.isPending ? <p className={textMutedOnSurface}>Loading links…</p> : null}
      {query.isError ? (
        <QueryError
          message="Could not read links."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data?.edges.length === 0 ? <p className={textMutedOnSurface}>No links.</p> : null}
      <ul className="flex flex-wrap gap-x-4 gap-y-1">
        {query.data?.edges.map((edge) => {
          const route = edge.node.ref === undefined ? undefined : parseNodeReference(edge.node.ref);
          return (
            <li key={`${edge.kind}:${edge.node.kind}:${edge.node.id}`}>
              <span className={textMutedOnSurface}>{edge.kind} </span>
              {route === undefined ? (
                edge.node.id
              ) : (
                <Link className={`underline ${linkText}`} to={buildReferencePath(route)}>
                  {edge.node.kind === "issue"
                    ? edge.node.id
                    : `${edge.node.kind} · ${edge.node.issue_key ?? edge.node.id}`}
                </Link>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
