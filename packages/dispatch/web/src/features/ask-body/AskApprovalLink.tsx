import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { documentItemPath } from "../refs/routes";
import { ASK_APPROVAL_WORDS } from "./ask-body-words";
import { authored, type TextMarks } from "./ask-text";

/**
 * The document an approval ask asks about, as a link naming both the document and the version it
 * asked (`approval.version`, kept current while the request is open — see `AskApproval`). Built
 * through `documentItemPath`, the same shape every in-app thread link uses: it opens the
 * document's current route, never a historical `@vN` snapshot, with this ask selected in the
 * margin. A reader who follows it after the document has moved further still reaches the live
 * document, with the approve controls in view, and the text beside it keeps saying which version
 * the request named. A host that hands `marks` gets the link inside the mark of the body's own
 * words, and the document's name and its number inside it in the mark of a value
 * (`AskBodyProps.marks`).
 */
export function AskApprovalLink({ ask, marks }: { ask: Ask; marks?: TextMarks }): ReactNode {
  const { approval, approval_artifact: artifact } = ask;
  if (approval === undefined || artifact === undefined) {
    return null;
  }
  const route = documentItemPath(
    {
      issue_key: ask.issue_key,
      kind: "doc",
      primary: artifact.primary,
      project: artifact.project,
      session_id: null,
      slug: artifact.slug,
    },
    { id: ask.id, kind: "ask" }
  );
  return (
    <Link className="underline" to={route} {...marks?.pageChrome}>
      {authored(marks, approval.name)}
      {`, ${ASK_APPROVAL_WORDS.version} `}
      {authored(marks, approval.version)}
    </Link>
  );
}
