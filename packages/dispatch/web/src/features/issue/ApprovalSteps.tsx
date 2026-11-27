import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Artifact, Ask } from "../../api/types";
import { approvalPillClassName } from "../../components/Pill";
import { type PageWords, usePageWords } from "../../components/tooltips";
import {
  pageInkText,
  pageMutedText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip, approvalVersion } from "../doc/ApprovalChip";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import type { StepEntry } from "./gated-steps";

/** A value drawn inside `NoTooltips` - a document's name, a version, a reviewer's reason - keeps
 *  `max-w-full` and breaks anywhere, so it never runs past its line. */
const VALUE_WRAPS = "max-w-full [overflow-wrap:anywhere]";

/** What an entry needs to link its document at the version its approval speaks for: the
 *  issue's open asks, whose approval requests name that version, and the issue's key. */
export interface ApprovalVersions {
  readonly asks: readonly Ask[];
  readonly issueKey: string;
}

/**
 * The version of `document` its approval speaks for: while a request is open, the version that
 * request names - read from the request itself (`AskApproval.documents`, else its own `version`
 * where it was opened on this document), since the document's own approval carries no version
 * for a request on it alone - and otherwise the version a review approved or sent back
 * (`approvalVersion`). Undefined for a document nobody has asked to review.
 */
function approvalLinkVersion(document: Artifact, asks: readonly Ask[]): number | undefined {
  const approval = document.approval;
  if (approval === undefined || approval.state === "draft") return undefined;
  if (approval.state !== "awaiting") return approvalVersion(approval);
  // The first open request that names the document decides; one that names it with no version
  // leaves the document's own approval to say it, and no later request is read.
  let named: number | undefined;
  for (const ask of asks) {
    const request = ask.kind === "approval" && ask.state === "open" ? ask.approval : undefined;
    if (request === undefined) continue;
    const listed = request.documents?.find((entry) => entry.artifact_id === document.id);
    if (listed !== undefined) {
      named = listed.version;
      break;
    }
    if (request.artifact_id === document.id) {
      named = request.version;
      break;
    }
  }
  return (
    named ??
    approval.documents?.find((entry) => entry.artifact_id === document.id)?.version ??
    approval.latest_version
  );
}

/**
 * The link to the version of `document` an approval speaks for, opened in place of the outline
 * at that version (`?v=`), as the old panel's card of an approval request linked it: the
 * document's name and the version, `spec.md, version 1`.
 * It is drawn inside `NoTooltips` alone, so it is the new issue page's link, in the ink with
 * its underline, and never the application's blue.
 */
function ApprovalVersionLink({
  document,
  issueKey,
  version,
  words,
}: {
  document: Artifact;
  issueKey: string;
  version: number;
  words: PageWords;
}): ReactNode {
  return (
    <Link
      className={`dispatch-inline-link text-xs underline ${pageInkText}`}
      data-approval-version={document.id}
      to={buildIssuePath({ key: issueKey, kind: "artifact", slug: document.slug, version })}
    >
      <span className={VALUE_WRAPS} {...words.authorText}>{`${document.name},`}</span>{" "}
      <span {...words.pageChrome}>{words.CONTROL_LABELS.version.toLowerCase()}</span>{" "}
      <span className={VALUE_WRAPS} {...words.authorText}>
        {version}
      </span>
    </Link>
  );
}

/** The state of one step: its document's chip, or the dashed pill that says nobody has asked for
 *  this document to be reviewed - a step with no document, a document with no approval, and a
 *  draft all read the same to someone waiting on the gate, and none of the three has a review to
 *  open, so the pill is a span rather than a button. */
function StepState({
  actions,
  actionsOn,
  actionsTogether,
  entry,
  primaryArtifactId,
  size,
  versions,
}: {
  actions: boolean;
  actionsOn: readonly string[] | undefined;
  actionsTogether: boolean;
  entry: StepEntry;
  primaryArtifactId: string;
  size: "compact" | "default";
  versions: ApprovalVersions | undefined;
}): ReactNode {
  const page = usePageWords();
  const { document } = entry;
  if (document?.approval === undefined || document.approval.state === "draft") {
    return (
      <span className={approvalPillClassName("draft", size, page !== null)} {...page?.pageChrome}>
        {page?.CONTROL_LABELS.not_requested ?? "Not requested"}
      </span>
    );
  }
  // The link to the version an approval speaks for is drawn inside `NoTooltips` alone, in the
  // new issue page's words: Details' copy of the list, which keeps the old card's link, so that
  // everything the old page reached stays reachable.
  const version =
    page === null || versions === undefined
      ? undefined
      : approvalLinkVersion(document, versions.asks);
  // Where `actionsOn` names two or more documents the list holds more than one Approve and
  // Request changes, so inside `NoTooltips` each says the document it reviews, in the new issue
  // page's words (WCAG 2.5.3). Outside `NoTooltips` (`page === null`) each keeps `main`'s name
  // whatever `actionsOn` holds, as it does inside with one document or none given.
  const actionsDocument =
    page === null || actionsOn === undefined || actionsOn.length < 2
      ? undefined
      : page.CONTROL_LABELS[`approval_document_${entry.step}` as const];
  return (
    // The chip is reached only for a document with an approval that is not a draft - the only
    // approval `ApprovalChip` draws anything for - so the only thing left to decide here is whether
    // this is the issue's primary document, where the actions have always sat. A gate that
    // names no step for that document reaches none of this: the header's own chip carries it.
    // Given `actionsOn`, it is instead whether this document's approval request waits on the
    // reader, so each such entry carries its own two actions, and a press on them reviews this
    // entry's document, the one its chip is drawn for.
    <>
      <ApprovalChip
        actionsDocument={actionsDocument}
        artifact={document}
        keepActionsTogether={actionsTogether}
        layout="contents"
        showActions={
          actions &&
          (actionsOn === undefined
            ? document.id === primaryArtifactId
            : actionsOn.includes(document.id))
        }
        size={size}
        variant="header"
      />
      {page === null || versions === undefined || version === undefined ? null : (
        <ApprovalVersionLink
          document={document}
          issueKey={versions.issueKey}
          version={version}
          words={page}
        />
      )}
    </>
  );
}

/** Why a step's reviewer sent its document back, as one wrapping line: the chip says only
 *  `Changes requested`, and the reason is the whole of what the writer must act on. It is given
 *  no width, no clamp and no tooltip, so a phone reads all of it. The reason is the reviewer's
 *  own text on that document, drawn by the renderer from the reading the server sent with it
 *  (`reason_tree`), as the Reviews dialog draws it. Inside `NoTooltips` the line is the page's
 *  own words and the reason is the reviewer's, each with its mark; the reviewer's name carries
 *  its own (`AgentName`). */
function StepReason({ entry }: { entry: StepEntry }): ReactNode {
  const page = usePageWords();
  const approval = entry.document?.approval;
  if (approval === undefined || approval.state !== "changes_requested") {
    return null;
  }
  const version = approvalVersion(approval);
  const reason = approval.reason ?? null;
  const reasonBody =
    reason === null ? null : (
      <MarkdownBody
        markdown={reason}
        source={textSourceOf("artifact", entry.document?.id, entry.document?.issue_key)}
        surface="approval-review-reason"
        tree={approval.reason_tree}
        variant="inline"
      />
    );
  return (
    <p
      // Inside `NoTooltips` the line is the new issue page's secondary words.
      className={`min-w-0 text-xs [overflow-wrap:anywhere] ${page === null ? textSecondaryOnSurface : pageMutedText}`}
      data-approval-step-reason={entry.step}
      {...page?.pageChrome}
    >
      {`${entry.label}: `}
      {approval.by === undefined ? (
        `Changes requested on v${version}`
      ) : (
        <>
          <AgentName actor={approval.by} inline />
          {` requested changes on v${version}`}
        </>
      )}
      {reasonBody === null ? null : (
        <>
          {" · "}
          {/* Inside `NoTooltips` the reason is a value: it carries the author's mark and breaks
              anywhere, on a wrapper, since the renderer takes no class. */}
          {page === null ? (
            reasonBody
          ) : (
            <span className={VALUE_WRAPS} {...page.authorText}>
              {reasonBody}
            </span>
          )}
        </>
      )}
    </p>
  );
}

/**
 * The design gate's steps in a root issue's header: one line per gated step, in the order the
 * deployment publishes them, each naming the document that step reviews and where that document
 * stands. It takes the place of the header's one chip, which spoke for the primary document
 * alone and left a reader guessing which of three documents the gate was waiting on. The chip
 * is drawn beside this list when no step here reviews the primary document, so that document's
 * state and its two controls never leave the header, whatever the gate names.
 *
 * The new issue page draws the list twice: its header at the `compact` size with no reasons, and
 * Details with the reasons and, inside `NoTooltips`, each document's link at the version its
 * approval speaks for (`versions`). Each option's default is this header's shape.
 *
 * The caller decides which steps there are, from the issue read's approval gate
 * (`approvalGateSteps`); this draws them.
 */
export function ApprovalSteps({
  actions = true,
  actionsOn,
  entries,
  layout = "block",
  primaryArtifactId,
  reasons = true,
  size = "default",
  testId = "issue-approval-steps",
  versions,
}: {
  /** Whether any entry carries Approve and Request changes: the primary document's, or each
   *  entry of `actionsOn`. */
  actions?: boolean;
  /** The ids of the documents whose approval request waits on the reader. Given, each of those
   *  documents' entries carries its own Approve and Request changes, which review that entry's
   *  document, and every other entry carries none; not given, the primary document's entry
   *  alone carries them, as on `main`. The new issue page's header passes it, from the asks that
   *  wait on the reader. Naming two or more, inside `NoTooltips` each action's name says its
   *  document, "Approve the spec", "Request changes to the verification" (`ApprovalChip`'s
   *  `actionsDocument`). */
  actionsOn?: readonly string[];
  entries: StepEntry[];
  /** `block` draws the list and each entry as boxes of their own, each wrapping within itself,
   *  as on `main`. `contents` draws no box for the list or for an entry, so each entry's name,
   *  its chip, and its Approve and Request changes, kept together in one box that does not wrap
   *  (`ApprovalChip`'s `keepActionsTogether`), are items of the caller's line, which sets their
   *  gaps; the reasons, when drawn, follow them there. The new issue page's header uses it. */
  layout?: "block" | "contents";
  primaryArtifactId: string;
  /** Whether each step a reviewer sent back says why, under the list. */
  reasons?: boolean;
  /** `compact` draws each chip and its actions at the new issue page header's size
   *  (`ApprovalChip`), in a list that wraps with 4 px between its lines and takes no full
   *  width. */
  size?: "compact" | "default";
  /** The list's test id: the header's is the one every check reads the gate by, and a second
   *  copy on a page carries its own, so the two are never one ambiguous name. */
  testId?: string;
  /** Given, inside `NoTooltips`, every entry links its document at the version its approval
   *  speaks for. */
  versions?: ApprovalVersions;
}): ReactNode {
  const page = usePageWords();
  const contents = layout === "contents";
  return (
    // The list takes the header row's full width and wraps within itself: three steps, their
    // chips and the two actions never fit a phone's line, and a row that cannot wrap scrolls
    // the page sideways. With no reasons under it, the list is its host's own flex item; at
    // `layout="contents"` the host, the list and each entry draw no box at all.
    <div className={reasons && !contents ? "flex w-full min-w-0 flex-col gap-2" : "contents"}>
      <ul
        aria-label="Approvals"
        className={
          contents
            ? "contents"
            : size === "compact"
              ? "flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1"
              : "flex w-full min-w-0 flex-wrap items-center gap-x-4 gap-y-2"
        }
        data-testid={testId}
      >
        {entries.map((entry) => (
          <li
            className={contents ? "contents" : "flex min-w-0 flex-wrap items-center gap-2"}
            data-approval-step={entry.step}
            key={entry.step}
          >
            {/* Inside `NoTooltips` the step's name is the new issue page's secondary words, and
                each chip and pill its own colours. */}
            <span
              className={`text-xs font-medium ${page === null ? textMutedOnSurface : pageMutedText}`}
            >
              {entry.label}
            </span>
            <StepState
              actions={actions}
              actionsOn={actionsOn}
              actionsTogether={contents}
              entry={entry}
              primaryArtifactId={primaryArtifactId}
              size={size}
              versions={versions}
            />
          </li>
        ))}
      </ul>
      {reasons ? entries.map((entry) => <StepReason entry={entry} key={entry.step} />) : null}
    </div>
  );
}
