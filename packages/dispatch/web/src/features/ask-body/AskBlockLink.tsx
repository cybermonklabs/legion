import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { isDecisionAsk } from "../inbox/ask-decision";
import type { OwnedAsk } from "../inbox/ask-name";
import { blockFragment } from "../refs/block-reference";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { ASK_LINK_WORDS } from "./ask-body-words";

export function AskBlockLink({ ask }: { ask: OwnedAsk }): ReactNode {
  if (!isDecisionAsk(ask)) {
    return null;
  }
  const fragment = blockFragment(ask.block_id);
  if (ask.issue_key !== null) {
    const route = ask.block_artifact.primary
      ? buildIssuePath({ key: ask.issue_key, kind: "spec" })
      : buildIssuePath({ key: ask.issue_key, kind: "artifact", slug: ask.block_artifact.slug });
    return <Link to={`${route}${fragment}`}>{ASK_LINK_WORDS.open_in_document}</Link>;
  }
  if (ask.document !== undefined) {
    return (
      <Link
        to={`${buildProjectPath({ kind: "document", project: ask.document.project, slug: ask.document.slug })}${fragment}`}
      >
        {ASK_LINK_WORDS.open_in_document}
      </Link>
    );
  }
  return null;
}
