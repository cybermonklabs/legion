import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import type { Artifact, Version } from "../../api/types";
import {
  borderDefault,
  card,
  dangerText,
  highlightRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import {
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  documentRoute,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { useContentDrawn } from "../shell/content-drawn";
import {
  CLOSED_ISSUE_REMOVE_REFUSAL,
  CLOSED_ISSUE_RENAME_REFUSAL,
  CorrectTitle,
  correctionIsOffered,
  RemoveDocument,
  removalIsOffered,
} from "./DocumentWayBack";

export function artifactVersionUrl(artifactID: string, version: number): string {
  return `/api/v1/artifacts/${encodeURIComponent(artifactID)}/versions/${version}`;
}

export function formatArtifactBytes(bytes: number | undefined): string {
  if (bytes === undefined) {
    return "Size unavailable";
  }
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function versionLabel(version: Version): string {
  return `Version ${version.number}${version.summary === null ? "" : ` — ${version.summary}`}`;
}

/** A copy of `versions` ordered latest first, the order every version list and picker shows. */
export function versionsNewestFirst(versions: readonly Version[] | null): Version[] {
  return [...(versions ?? [])].sort((left, right) => right.number - left.number);
}

export function ArtifactHeader({
  artifact,
  children,
  highlight,
  isClosed = false,
  onShowDiffChange,
  showDiff = false,
  showVersionPicker,
  toolbar,
  version,
}: {
  artifact: Artifact;
  children: ReactNode;
  highlight: boolean;
  isClosed?: boolean;
  onShowDiffChange?(next: boolean): void;
  showDiff?: boolean;
  showVersionPicker: boolean;
  toolbar?: DocumentToolbar;
  version: number | undefined;
}): ReactNode {
  const [highlighted, setHighlighted] = useState(highlight);
  const drawn = useContentDrawn();
  const navigate = useNavigate();
  const versions = versionsNewestFirst(toolbar?.versions ?? artifact.versions);
  // An artifact of an agent's conversation (a picture sent in a direct message) belongs to no
  // issue or project; its page is under the agent's session.
  const agentRoute = documentRoute(artifact);
  const agentSession = agentRoute.kind === "agent-artifact" ? agentRoute.session : undefined;
  // A project's document is not removed, and an agent's conversation holds files rather than
  // documents: the removal is the issue document's alone. A `const` keeps the narrowing inside
  // the callback below.
  const issueKey = artifact.issue_key;

  // The ring shows the reader which artifact the address named, for two seconds. On a cold
  // landing the shell holds the margin until the document draws (`features/shell/
  // content-drawn.ts`), and the margin then comes in beside this header. If the two seconds
  // started when this header mounted, the ring could go before the page stops changing. So the
  // two seconds start when the landed page's content has drawn. Where nothing is held, which is
  // every page after the first, they start at once, as before.
  useEffect(() => {
    setHighlighted(highlight);
    if (!highlight || !drawn) {
      return;
    }
    const timeout = window.setTimeout(() => setHighlighted(false), 2000);
    return () => window.clearTimeout(timeout);
  }, [drawn, highlight]);

  // One button, placed by the branch below: it leads the row's non-wrapping box where there is
  // no version to diff, and sits before that box where there is one.
  const nameVersion =
    toolbar === undefined ? null : (
      <button
        className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        disabled={isClosed || toolbar.isNamingVersion}
        onClick={toolbar.requestNamedVersion}
        type="button"
      >
        Name version
      </button>
    );

  return (
    <section className="space-y-4">
      <header
        className={`flex flex-wrap items-center justify-between gap-3 rounded-lg p-3 ${card} ${
          highlighted ? highlightRing : ""
        }`}
        data-testid="artifact-header"
      >
        <div className="min-w-0">
          {agentSession !== undefined ? (
            // The agent whose conversation owns the artifact, by its current name, linked to that
            // conversation. A long title wraps rather than widening the header.
            <p
              className={`flex min-h-11 min-w-0 flex-wrap items-center gap-x-1 text-sm font-semibold ${textSecondaryOnSurface}`}
            >
              Agent
              <AgentName
                className="min-w-0 break-words"
                sessionId={agentSession}
                storedTitle={undefined}
              />
            </p>
          ) : artifact.issue_key === null ? (
            <Link
              to={buildProjectPath({ kind: "documents", project: artifact.project })}
              className={`inline-flex min-h-11 items-center text-sm font-semibold ${linkText} ${linkHoverText}`}
            >
              Project {artifact.project}
            </Link>
          ) : null}
          {/* `items-start` keeps the copy button beside the first line of a title that wraps. The
              button (44 px, 32 px from `md`) is taller than one `text-lg` line (28 px), so a
              negative margin of half the difference centres its icon on that first line. */}
          <div className="flex min-w-0 items-start gap-1">
            <h2 className={`min-w-0 break-words text-lg font-semibold ${textPrimaryOnSurface}`}>
              {artifact.name}
            </h2>
            <CopyRefButton className="-my-2 md:-my-0.5" route={documentRoute(artifact, version)} />
          </div>
          {artifact.approval === undefined ? null : (
            <div className="mt-1">
              <ApprovalChip artifact={artifact} variant="header" />
            </div>
          )}
        </div>
        {showVersionPicker || toolbar !== undefined ? (
          <div className="flex min-w-0 flex-wrap items-center gap-3">
            {showVersionPicker ? (
              <label
                className={`flex min-h-11 min-w-0 items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
              >
                Version
                <select
                  aria-label="Version"
                  className={`min-h-11 min-w-0 max-w-56 truncate rounded border px-2 py-2 font-normal ${inputClasses(true)}`}
                  onChange={(event) => {
                    // Both path builders read `version: undefined` as the unversioned route.
                    const picked =
                      event.target.value === "" ? undefined : Number(event.target.value);
                    navigate(
                      agentSession !== undefined
                        ? buildReferencePath(documentRoute(artifact, picked))
                        : artifact.issue_key === null
                          ? buildProjectPath({
                              kind: "document",
                              project: artifact.project,
                              slug: artifact.slug,
                              version: picked,
                            })
                          : buildIssuePath({
                              key: artifact.issue_key,
                              kind: "artifact",
                              slug: artifact.slug,
                              version: picked,
                            })
                    );
                  }}
                  value={version ?? ""}
                >
                  <option value="">Current</option>
                  {versions.map((item) => (
                    <option key={item.number} value={item.number}>
                      {versionLabel(item)}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}
            {toolbar === undefined ? null : (
              // Two boxes. The outer one wraps, because at 320 px the actions are wider than the
              // header card. The inner one does not: the dot says whether this page is live on
              // the document these actions write to, so it shares that box with the action
              // beside it — the diff button on a version view, Name version where there is none
              // — and travels with it instead of wrapping onto a line of its own.
              <div className="flex flex-wrap items-center gap-3">
                {!correctionIsOffered(artifact) ? null : (
                  <CorrectTitle
                    artifact={artifact}
                    refusal={isClosed ? CLOSED_ISSUE_RENAME_REFUSAL : null}
                  />
                )}
                {issueKey === null || !removalIsOffered(artifact) ? null : (
                  <RemoveDocument
                    artifact={artifact}
                    onRemoved={() =>
                      // The route stays on the document the issue no longer holds, so the
                      // artifacts panel's own branch says it is gone rather than failing to
                      // load. A version route is normalised away first: the version the person
                      // was reading went with the document.
                      navigate(
                        buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug })
                      )
                    }
                    refusal={isClosed ? CLOSED_ISSUE_REMOVE_REFUSAL : null}
                  />
                )}
                {version === undefined ? null : nameVersion}
                <div className="flex items-center gap-3">
                  {version === undefined ? (
                    nameVersion
                  ) : (
                    <button
                      className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                      onClick={() => onShowDiffChange?.(!showDiff)}
                      type="button"
                    >
                      {showDiff ? "Show version" : "Diff vs current"}
                    </button>
                  )}
                  <ConnectionDot connection={toolbar.connection} pending={toolbar.pending} />
                </div>
              </div>
            )}
          </div>
        ) : null}
      </header>
      {children}
    </section>
  );
}

export function ArtifactBlobView({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: number | undefined;
}): ReactNode {
  const latest = versionsNewestFirst(artifact.versions)[0];
  const selected =
    version === undefined
      ? latest
      : (artifact.versions ?? []).find((item) => item.number === version);

  if (selected === undefined) {
    return version === undefined ? (
      <p className={`text-sm ${textMutedOnCanvas}`}>No versions have been uploaded.</p>
    ) : (
      <p className={`text-sm ${dangerText}`}>
        Version {version} is not available for this artifact.
      </p>
    );
  }

  const download = artifactVersionUrl(artifact.id, selected.number);
  return (
    <section aria-label={`${artifact.name} version ${selected.number}`} className="space-y-3">
      {artifact.kind === "image" ? (
        <img
          alt={`${artifact.name} version ${selected.number}`}
          className={`h-auto w-full rounded-lg border ${borderDefault}`}
          src={download}
        />
      ) : null}
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <div>
          <dt className={textMutedOnCanvas}>Version</dt>
          <dd className={`font-medium ${textPrimaryOnCanvas}`}>{selected.number}</dd>
        </div>
        <div>
          <dt className={textMutedOnCanvas}>Size</dt>
          <dd className={`font-medium ${textPrimaryOnCanvas}`}>
            {formatArtifactBytes(selected.size)}
          </dd>
        </div>
      </dl>
      <p className={`text-sm ${textMutedOnCanvas}`}>
        Uploaded <Timestamp at={selected.created_at} />
      </p>
      <a
        className={`inline-flex min-h-11 items-center font-medium underline ${linkText} ${linkHoverText}`}
        download=""
        href={download}
      >
        Download version {selected.number}
      </a>
    </section>
  );
}
