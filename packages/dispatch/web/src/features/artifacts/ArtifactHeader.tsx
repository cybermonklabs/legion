import type { ReactNode } from "react";
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { routePath } from "../../api/paths";
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
import {
  CLOSED_ISSUE_REMOVE_REFUSAL,
  CLOSED_ISSUE_RENAME_REFUSAL,
  CorrectTitle,
  correctionIsOffered,
  RemoveDocument,
  removalIsOffered,
} from "./DocumentWayBack";

export function artifactVersionUrl(
  artifactID: string,
  version: number,
  // Links only: the server answers no-cache there, and a document as its Markdown file;
  // pictures keep the year-long cache.
  options: { download?: boolean } = {}
): string {
  return routePath(
    "get",
    "/api/v1/artifacts/{id}/versions/{number}",
    { id: artifactID, number: String(version) },
    options.download === true ? { download: true } : undefined
  );
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
  // A version's summary, as its writer gave it: the reading list's `artifact-version-summary`.
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
  // The document the route names, or `undefined` once the header is gone: the issue's page draws
  // this header for the document its route names, and none while the route names no document. A
  // removal lands when Dispatch answers it, by when the reader may have moved on, so its way back
  // reads this rather than the document the header drew when Remove was pressed (`onRemoved`).
  const drawn = useRef<string | undefined>(undefined);

  useEffect(() => {
    drawn.current = artifact.id;
    return () => {
      drawn.current = undefined;
    };
  }, [artifact.id]);

  useEffect(() => {
    if (!highlight) {
      setHighlighted(false);
      return;
    }
    setHighlighted(true);
    const timeout = window.setTimeout(() => setHighlighted(false), 2000);
    return () => window.clearTimeout(timeout);
  }, [highlight]);

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
        data-highlighted={highlighted ? "" : undefined}
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
              {/* Keyed by the document, as the correction and the removal below are: the chip
                  holds the review it made and the change-request dialog, with the reason typed
                  into it and what became of its last request, all of it this document's alone. */}
              <ApprovalChip artifact={artifact} key={artifact.id} variant="header" />
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
                  // Keyed by the document, as Remove is below: the form holds the title typed
                  // into it and what became of its last Save, both that document's alone. The
                  // key names the control too: Remove beside it is keyed by the same document,
                  // and siblings are told apart by their keys.
                  <CorrectTitle
                    artifact={artifact}
                    key={`correction:${artifact.id}`}
                    refusal={isClosed ? CLOSED_ISSUE_RENAME_REFUSAL : null}
                  />
                )}
                {issueKey === null || !removalIsOffered(artifact) ? null : (
                  // Keyed by the document: the issue's page draws this header for one document
                  // after another as its route moves, and the control holds the removal it
                  // made - `Removing…`, a refusal and its Retry - so each document is drawn
                  // with a Remove of its own, never under another document's refusal with that
                  // document's removal as its Retry.
                  <RemoveDocument
                    artifact={artifact}
                    key={artifact.id}
                    onRemoved={(removed) => {
                      // The route stays on the document the issue no longer holds, so the
                      // artifacts panel's own branch says it is gone rather than failing to
                      // load. A version route is normalised away first: the version the person
                      // was reading went with the document. That is the way back only while the
                      // route is still the removed document's (`drawn`): a removal that lands
                      // after the reader moved on leaves them on the page they moved to.
                      if (drawn.current === removed.id) {
                        navigate(
                          buildIssuePath({ key: issueKey, kind: "artifact", slug: removed.slug })
                        );
                      }
                    }}
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

  return (
    <section aria-label={`${artifact.name} version ${selected.number}`} className="space-y-3">
      {artifact.kind === "image" ? (
        <img
          alt={`${artifact.name} version ${selected.number}`}
          className={`h-auto w-full rounded-lg border ${borderDefault}`}
          src={artifactVersionUrl(artifact.id, selected.number)}
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
        href={artifactVersionUrl(artifact.id, selected.number, { download: true })}
      >
        Download version {selected.number}
      </a>
    </section>
  );
}
