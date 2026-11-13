import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { documentItemPath, openDocumentPath } from "../refs/routes";

/**
 * The document an approval ask asks about, as a link naming both the document and the version it
 * asked (`approval.version`, kept current while the request is open — see `AskApproval`). It
 * opens the document's current route, never a historical `@vN` snapshot. An issue's document is
 * opened itself, at its own address (`openDocumentPath`): `?ask=` there would open the issue's
 * outline instead (dispatch://INFRA-1196 R20). A project document's page has no outline, so its
 * link names the ask beside it (`documentItemPath`), which selects it in the margin. A reader who
 * follows it after the document has moved further still reaches the live document, with the
 * approve controls in view, and the text beside it keeps saying which version the request named.
 */
export function AskApprovalLink({ ask }: { ask: Ask }): ReactNode {
  const { approval, approval_artifact: artifact } = ask;
  if (approval === undefined || artifact === undefined) {
    return null;
  }
  const document = {
    issue_key: ask.issue_key,
    kind: "doc" as const,
    primary: artifact.primary,
    project: artifact.project,
    session_id: null,
    slug: artifact.slug,
  };
  const route =
    ask.issue_key === null
      ? documentItemPath(document, { id: ask.id, kind: "ask" })
      : openDocumentPath(document);
  return (
    <Link className="underline" to={route}>
      {approval.name}, version {approval.version}
    </Link>
  );
}
