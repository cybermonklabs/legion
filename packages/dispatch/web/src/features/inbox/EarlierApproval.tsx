import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { AskApproval } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { dangerText, textMutedOnSurface } from "../../theme/classes";
import { ChangeView } from "../doc/ChangeView";
import { cutDecisionBlocks } from "../doc/decision-blocks";
import { AgentName } from "../refs/AgentName";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { inboxActionMark } from "./inbox-action-mark";

/**
 * What changed between the version a human approved and the version the request names, with every
 * decision block cut out of both (`cutDecisionBlocks`), so it shows what changed outside them:
 * the answers to decision blocks between the two are what the human himself decided. Mounted
 * only once the reader asks for it, so nothing is fetched or rendered before. Both reads share
 * the document page's `["artifact", id, "version", n]` keys, and the loading and error copy is
 * its comparison's (`features/artifacts/ArtifactDetails.tsx`).
 */
function ApprovedChangeView({
  approvedVersion,
  artifactId,
  version,
}: {
  approvedVersion: number;
  artifactId: string;
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
    return <p className={`text-sm ${dangerText}`}>Could not load versions to compare.</p>;
  }
  if (cut !== undefined) {
    return <ChangeView after={cut.after} before={cut.before} />;
  }
  return <p className={`text-sm ${textMutedOnSurface}`}>Loading versions to compare…</p>;
}

/** The approval an approval request follows, in one sentence: `Approved at version N on <date>
 *  by <who>.` */
function ApprovedLine({ approved }: { approved: NonNullable<AskApproval["approved"]> }): ReactNode {
  return (
    <p>
      Approved at version {approved.version} on{" "}
      <time dateTime={approved.at} title={approved.at}>
        {approved.at.slice(0, 10)}
      </time>{" "}
      by <AgentName actor={approved.by} inline />.
    </p>
  );
}

/** The earlier approval's **Show diff**, which reveals what changed since it and reads **Hide
 *  diff** while it does. The reading pane's head draws it in the place at the end of its
 *  controls, apart from what it reveals (`EarlierApprovalDetails`, under the title). */
export function EarlierApprovalToggle({
  diffOpen,
  onToggle,
  shortcutKeys,
}: {
  diffOpen: boolean;
  onToggle: () => void;
  /** The keys that press it, where the host draws them on it (the reading pane's head, with a
   *  key a person gave it). */
  shortcutKeys?: readonly string[];
}): ReactNode {
  return (
    <DisclosureToggle
      expanded={diffOpen}
      keyHint={<ControlKeyHint keys={shortcutKeys} />}
      keyshortcuts={keyshortcutsOf(shortcutKeys)}
      label={diffOpen ? "Hide diff" : "Show diff"}
      marks={inboxActionMark("inbox.earlier-approval-diff")}
      onToggle={onToggle}
      textClassName={textMutedOnSurface}
    />
  );
}

/** The reading pane's half of `EarlierApproval`, under the title: the sentence, then what changed
 *  since that approval while its toggle has it open. Nothing for a first request. */
export function EarlierApprovalDetails({
  approval,
  diffOpen,
}: {
  approval: AskApproval;
  diffOpen: boolean;
}): ReactNode {
  const { approved } = approval;
  if (approved === undefined) {
    return null;
  }
  return (
    <div className={`text-xs ${textMutedOnSurface}`}>
      <ApprovedLine approved={approved} />
      {diffOpen ? (
        <ApprovedChangeView
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          version={approval.version}
        />
      ) : null}
    </div>
  );
}

/**
 * The approval an approval request follows, when the document has one, under the question of an
 * ask card: `Approved at version N on <date> by <who>.` and a **Show diff** control that reveals
 * `ApprovedChangeView`. Renders nothing for a first request, whose `approval` carries no
 * `approved`. The reading pane's head draws the toggle and the rest apart
 * (`EarlierApprovalToggle`, `EarlierApprovalDetails`).
 */
export function EarlierApproval({ approval }: { approval: AskApproval }): ReactNode {
  const [diffOpen, setDiffOpen] = useState(false);
  const { approved } = approval;
  if (approved === undefined) {
    return null;
  }
  return (
    <div className={`mt-1 text-xs ${textMutedOnSurface}`}>
      <ApprovedLine approved={approved} />
      <EarlierApprovalToggle diffOpen={diffOpen} onToggle={() => setDiffOpen((open) => !open)} />
      {diffOpen ? (
        <ApprovedChangeView
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          version={approval.version}
        />
      ) : null}
    </div>
  );
}
