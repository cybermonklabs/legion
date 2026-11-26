import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { AskApproval } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { useTooltips } from "../../components/tooltips";
import { dangerText, textMutedOnSurface } from "../../theme/classes";
import type { AgentNames } from "../conversation/CachedAuthor";
import { ChangeView } from "../doc/ChangeView";
import { cutDecisionBlocks } from "../doc/decision-blocks";
import { AskAgent } from "./ask-agent";
import { ASK_APPROVAL_WORDS } from "./ask-body-words";
import { authored, type TextMarks } from "./ask-text";

/**
 * What changed between the version a human approved and the version the request names, with every
 * decision block cut out of both (`cutDecisionBlocks`), so it shows what changed outside them:
 * the answers to decision blocks between the two are what the human himself decided. Mounted
 * only once the reader asks for it, so nothing is fetched or rendered before. Both reads share
 * the document page's `["artifact", id, "version", n]` keys, and the loading and error copy is
 * its comparison's (`features/artifacts/ArtifactDetails.tsx`). The two versions' own words are
 * the document's, which a host that hands `marks` gets inside the mark of a value.
 */
function ApprovedChangeView({
  approvedVersion,
  artifactId,
  marks,
  version,
}: {
  approvedVersion: number;
  artifactId: string;
  marks: TextMarks | undefined;
  version: number;
}): ReactNode {
  const before = useQuery({
    queryKey: ["artifact", artifactId, "version", approvedVersion],
    queryFn: () => api.getArtifactVersion(artifactId, approvedVersion),
  });
  const after = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  // The cut runs once per fetched pair, not once per render: the card re-renders on every
  // keystroke in its reason field while the diff stays open.
  const beforeMarkdown = isText(before.data) ? before.data.markdown : undefined;
  const afterMarkdown = isText(after.data) ? after.data.markdown : undefined;
  const cut = useMemo(
    () =>
      beforeMarkdown === undefined || afterMarkdown === undefined
        ? undefined
        : { after: cutDecisionBlocks(afterMarkdown), before: cutDecisionBlocks(beforeMarkdown) },
    [afterMarkdown, beforeMarkdown]
  );
  if (before.isError || after.isError) {
    return <p className={`text-sm ${dangerText}`}>{ASK_APPROVAL_WORDS.diff_failed}</p>;
  }
  if (cut !== undefined) {
    // The exact characters a change swaps: the reading list's `change-view-diff`.
    const view = <ChangeView after={cut.after} before={cut.before} />;
    return marks === undefined ? (
      view
    ) : (
      <div className="contents" {...marks.authorText}>
        {view}
      </div>
    );
  }
  return <p className={`text-sm ${textMutedOnSurface}`}>{ASK_APPROVAL_WORDS.diff_loading}</p>;
}

/**
 * The approval an approval request follows, when the document has one: `Approved at version N
 * on <date> by <who>`, with no full stop at its end, and a **Show diff** control that reveals
 * `ApprovedChangeView`. Renders nothing for a first request, whose `approval` carries no
 * `approved`. Inside `NoTooltips` its date carries no `title`, as no time there does.
 * `agentNames` is how the ask body names the approver (`ask-agent.tsx`); the reading pane's
 * head, which draws it beside its own approval chip, names it as every surface of today's pages
 * does. A host that hands `marks` (`AskBodyProps.marks`) gets the version, the date and the
 * approver inside the mark of a value.
 */
export function EarlierApproval({
  agentNames = "live",
  approval,
  marks,
}: {
  agentNames?: AgentNames;
  approval: AskApproval;
  marks?: TextMarks;
}): ReactNode {
  const [diffOpen, setDiffOpen] = useState(false);
  const tooltips = useTooltips();
  const { approved } = approval;
  if (approved === undefined) {
    return null;
  }
  return (
    <div className={`mt-1 text-xs ${textMutedOnSurface}`}>
      <p>
        {`${ASK_APPROVAL_WORDS.approved.at_version} `}
        {authored(marks, approved.version)}
        {` ${ASK_APPROVAL_WORDS.approved.on}`}{" "}
        <time
          dateTime={approved.at}
          title={tooltips ? approved.at : undefined}
          {...marks?.authorText}
        >
          {approved.at.slice(0, 10)}
        </time>{" "}
        {`${ASK_APPROVAL_WORDS.approved.by} `}
        <AskAgent actor={approved.by} agentNames={agentNames} inline marks={marks} />
      </p>
      <DisclosureToggle
        expanded={diffOpen}
        label={diffOpen ? ASK_APPROVAL_WORDS.hide_diff : ASK_APPROVAL_WORDS.show_diff}
        onToggle={() => setDiffOpen((open) => !open)}
        textClassName={textMutedOnSurface}
      />
      {diffOpen ? (
        <ApprovedChangeView
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          marks={marks}
          version={approval.version}
        />
      ) : null}
    </div>
  );
}
