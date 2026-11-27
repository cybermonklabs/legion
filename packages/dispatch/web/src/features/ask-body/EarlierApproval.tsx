import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { AskApproval } from "../../api/types";
import { ChevronIcon, DisclosureToggle } from "../../components/DisclosureToggle";
import { useTooltips } from "../../components/tooltips";
import { dangerText, pageFailedText, pageQuietText, textMutedOnSurface } from "../../theme/classes";
import type { AgentNames } from "../conversation/CachedAuthor";
import { ChangeView } from "../doc/ChangeView";
import { cutDecisionBlocks } from "../doc/decision-blocks";
import { inboxActionMark } from "../inbox/inbox-action-mark";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { AskAgent } from "./ask-agent";
import { ASK_APPROVAL_WORDS } from "./ask-body-words";
import { type AskPalette, authored, ownPart, type TextMarks } from "./ask-text";

/** The colours of the earlier approval by the palette the ask body's host hands (`AskPalette`),
 *  as `issue-page/DecisionActions.tsx` keeps its own: `main`'s are the classes every surface of
 *  today's pages draws, and on the new issue page the line, Show diff and its chevron are the
 *  quiet grey, as the ask body's own line of who asked and when is, and a diff that could not
 *  be read the failed red. */
const TONES = {
  main: { failed: dangerText, quiet: textMutedOnSurface },
  page: { failed: pageFailedText, quiet: pageQuietText },
} as const;

/**
 * What changed between the version a human approved and the version the request names, with every
 * decision block cut out of both (`cutDecisionBlocks`), so it shows what changed outside them:
 * the answers to decision blocks between the two are what the human himself decided. Mounted
 * only once the reader asks for it, so nothing is fetched or rendered before. Both reads share
 * the document page's `["artifact", id, "version", n]` keys, and the loading and error copy is
 * its comparison's (`features/artifacts/ArtifactDetails.tsx`). A host that hands `marks` gets
 * either line in the mark of the body's own words, and the two versions' own words, which are
 * the document's, inside the mark of a value.
 */
function ApprovedChangeView({
  approvedVersion,
  artifactId,
  marks,
  palette,
  version,
}: {
  approvedVersion: number;
  artifactId: string;
  marks: TextMarks | undefined;
  palette: AskPalette;
  version: number;
}): ReactNode {
  const tones = TONES[palette];
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
    return (
      <p className={`text-sm ${tones.failed}`} {...marks?.pageChrome}>
        {ASK_APPROVAL_WORDS.diff_failed}
      </p>
    );
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
  return (
    <p className={`text-sm ${tones.quiet}`} {...marks?.pageChrome}>
      {ASK_APPROVAL_WORDS.diff_loading}
    </p>
  );
}

/** The approval an approval request follows, in one line: `Approved at version N on <date> by
 *  <who>`, with no full stop at its end. Inside `NoTooltips` its date carries no `title`, as no
 *  time there does. `marks` and `agentNames` are `EarlierApproval`'s. */
function ApprovedLine({
  agentNames,
  approved,
  marks,
}: {
  agentNames: AgentNames;
  approved: NonNullable<AskApproval["approved"]>;
  marks: TextMarks | undefined;
}): ReactNode {
  const tooltips = useTooltips();
  return (
    <p {...marks?.pageChrome}>
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
  );
}

/** The earlier approval's **Show diff** in `main`'s palette, which reveals what changed since it
 *  and reads **Hide diff** while it does, marked as the control the Inbox's Show the earlier
 *  approval's diff presses (`inboxActionMark`). The reading pane's head draws it in the place at
 *  the end of its controls, apart from what it reveals (`EarlierApprovalDetails`, under the
 *  title). */
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
      label={diffOpen ? ASK_APPROVAL_WORDS.hide_diff : ASK_APPROVAL_WORDS.show_diff}
      marks={inboxActionMark("inbox.earlier-approval-diff")}
      onToggle={onToggle}
      textClassName={TONES.main.quiet}
    />
  );
}

/** The reading pane's half of `EarlierApproval`, under the title: the line, then what changed
 *  since that approval while its toggle has it open, in `main`'s palette with the names every
 *  surface of today's pages draws. Nothing for a first request. */
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
    <div className={`text-xs ${TONES.main.quiet}`}>
      <ApprovedLine agentNames="live" approved={approved} marks={undefined} />
      {diffOpen ? (
        <ApprovedChangeView
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          marks={undefined}
          palette="main"
          version={approval.version}
        />
      ) : null}
    </div>
  );
}

/**
 * The approval an approval request follows, when the document has one, under the question of an
 * ask body: its line (`ApprovedLine`) and a **Show diff** control that reveals
 * `ApprovedChangeView`. Renders nothing for a first request, whose `approval` carries no
 * `approved`. `agentNames` is how the ask body names the approver (`ask-agent.tsx`). A host that
 * hands `marks` (`AskBodyProps.marks`) gets the line and Show diff each in the mark of the body's
 * own words, and the version, the date and the approver inside the line in the mark of a value,
 * and one that hands the page's palette its colours (`TONES`). `DisclosureToggle` takes no class
 * for its chevron, which it draws in `main`'s disclosure grey, so in the page's palette Show diff
 * is drawn as that toggle draws it - the same button, words and chevron - in the page's quiet
 * grey. The Inbox's reading pane head draws the toggle and the rest apart, beside its own
 * approval chip (`EarlierApprovalToggle`, `EarlierApprovalDetails`).
 */
export function EarlierApproval({
  agentNames = "live",
  approval,
  marks,
  palette = "main",
}: {
  agentNames?: AgentNames;
  approval: AskApproval;
  marks?: TextMarks;
  /** The palette it draws in (`TONES`): `main`'s, unless the ask body's host hands the page's. */
  palette?: AskPalette;
}): ReactNode {
  const [diffOpen, setDiffOpen] = useState(false);
  const tones = TONES[palette];
  const { approved } = approval;
  if (approved === undefined) {
    return null;
  }
  return (
    <div className={`mt-1 text-xs ${tones.quiet}`}>
      <ApprovedLine agentNames={agentNames} approved={approved} marks={marks} />
      {palette === "page" ? (
        <button
          aria-expanded={diffOpen}
          className={`flex min-h-11 items-center gap-1 text-sm font-medium ${tones.quiet}`}
          onClick={() => setDiffOpen((open) => !open)}
          type="button"
          {...marks?.pageChrome}
        >
          {diffOpen ? ASK_APPROVAL_WORDS.hide_diff : ASK_APPROVAL_WORDS.show_diff}
          <ChevronIcon expanded={diffOpen} />
        </button>
      ) : (
        ownPart(
          marks,
          <EarlierApprovalToggle
            diffOpen={diffOpen}
            onToggle={() => setDiffOpen((open) => !open)}
          />
        )
      )}
      {diffOpen ? (
        <ApprovedChangeView
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          marks={marks}
          palette={palette}
          version={approval.version}
        />
      ) : null}
    </div>
  );
}
