import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, IssueReferences } from "../../api/types";
import { ControlRow } from "../../components/ControlRow";
import { SearchBox } from "../../components/SearchBox";
import { TruncatedText } from "../../components/TruncatedText";
import { useCappedAddressParam, usePageAddress } from "../../lib/page-address";
import { compareTimestamps } from "../../lib/timestamps";
import {
  badgePrimary,
  borderDefault,
  dangerText,
  highlightRing,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import {
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  documentRoute,
  isAgentArtifact,
  parseIssuePath,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { artifactVersionUrl, formatArtifactBytes, versionsNewestFirst } from "./ArtifactHeader";
import { ArtifactDropZone, ArtifactUploadRow, useArtifactUpload } from "./ArtifactUpload";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL, CreateDocument } from "./CreateDocument";
import { CLOSED_ISSUE_REMOVE_REFUSAL, RemoveDocument, removalIsOffered } from "./DocumentWayBack";

function kindIcon(kind: Artifact["kind"]): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-5 shrink-0",
    fill: "none",
    viewBox: "0 0 24 24",
  };
  if (kind === "doc") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Document artifact</title>
        <path
          d="M7 3h7l3 3v15H7z"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
        <path d="M14 3v4h4M10 12h4M10 16h4" strokeLinecap="round" strokeWidth="1.75" />
      </svg>
    );
  }
  if (kind === "image") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Image artifact</title>
        <rect height="16" rx="2" width="18" x="3" y="4" strokeWidth="1.75" />
        <circle cx="9" cy="10" r="1.25" strokeWidth="1.75" />
        <path
          d="m5 18 5-5 3 3 2-2 4 4"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
      </svg>
    );
  }
  return (
    <svg {...common} stroke="currentColor">
      <title>File artifact</title>
      <path d="M7 3h7l3 3v15H7z" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
      <path d="M14 3v4h4" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
    </svg>
  );
}

function artifactPath(artifact: Artifact): string {
  // A conversation's picture has its own page, under its agent's session.
  if (isAgentArtifact(artifact)) {
    return buildReferencePath(documentRoute(artifact));
  }
  return artifact.issue_key === null
    ? buildProjectPath({
        kind: "document",
        project: artifact.project,
        slug: artifact.slug,
      })
    : buildIssuePath({
        key: artifact.issue_key,
        kind: "artifact",
        slug: artifact.slug,
      });
}

/** Who owns an artifact, as a References row names it: its issue, its project, or the agent
 *  whose conversation holds it. */
function ArtifactOwner({ artifact }: { artifact: Artifact }): ReactNode {
  if (artifact.issue_key !== null) return artifact.issue_key;
  if (!isAgentArtifact(artifact)) return artifact.project;
  return (
    <>
      agent <AgentName inline sessionId={artifact.session_id} storedTitle={undefined} />
    </>
  );
}

// The issue's reference closure (every artifact reachable by following references, up to 8
// hops) — a different thing from a single artifact's own "Referenced by" list above. Collapsed
// by default: it is background context for the issue, not something worth scrolling past on
// every visit to the tab.
function References({ references }: { references: IssueReferences }): ReactNode {
  if ((references.members ?? []).length === 0) {
    return null;
  }

  return (
    <details className={`rounded-lg border p-3 ${borderDefault}`}>
      <summary
        className={`min-h-11 cursor-pointer text-sm font-semibold ${textSecondaryOnSurface}`}
      >
        References ({(references.members ?? []).length})
      </summary>
      <section aria-label="References" className="mt-3 space-y-2">
        <ul className="space-y-2">
          {(references.members ?? []).map(({ artifact, depth, via }) => (
            <li className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={artifact.id}>
              <Link
                className={`font-medium underline ${linkText} ${linkHoverText}`}
                to={artifactPath(artifact)}
              >
                {artifact.name}
              </Link>
              <p className={textMutedOnSurface}>
                <ArtifactOwner artifact={artifact} /> · via {via.kind} · depth {depth}
              </p>
            </li>
          ))}
        </ul>
        {references.truncated ? (
          <p className={`text-sm ${textMutedOnSurface}`}>more references beyond 8 hops</p>
        ) : null}
      </section>
    </details>
  );
}

function ArtifactRow({
  artifact,
  highlighted,
  isClosed,
  issueKey,
}: {
  artifact: Artifact;
  highlighted: boolean;
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const versions = versionsNewestFirst(artifact.versions);
  const latestVersion = versions[0];

  // The name keeps at least 8 rem; on a phone the trailing controls wrap under it rather than
  // squeezing the name to nothing.
  return (
    <li
      className={`flex flex-wrap items-center gap-3 border-b py-3 last:border-b-0 ${borderDefault} ${
        highlighted ? `rounded-xl px-3 ${highlightRing}` : ""
      }`}
      data-testid={`artifact-${artifact.slug}`}
    >
      <span className={`shrink-0 ${textSecondaryOnSurface}`}>{kindIcon(artifact.kind)}</span>
      {artifact.kind === "image" && latestVersion !== undefined ? (
        <img
          alt={`${artifact.name} version ${latestVersion.number}`}
          className={`h-10 w-10 shrink-0 rounded border object-cover ${borderDefault}`}
          loading="lazy"
          src={artifactVersionUrl(artifact.id, latestVersion.number)}
        />
      ) : null}
      <div className="min-w-32 flex-1">
        {/* `max-w-full`: below 1280 px the link is inline-flex rather than `block`, and an
            inline-flex box is as wide as its whole name unless something caps it. */}
        <Link
          className={`block max-w-full truncate font-medium underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug })}
        >
          <TruncatedText title={artifact.name}>{artifact.name}</TruncatedText>
        </Link>
        {/* The name truncates; its details wrap. Truncating them hid the update time on a row
            with room to spare. The size is the latest version's as stored (a document's
            markdown), so a document near the size cap shows here without being opened. */}
        <p className={`text-xs ${textMutedOnSurface}`}>
          {artifact.kind} · {versions.length} {versions.length === 1 ? "version" : "versions"} ·
          {latestVersion?.size === undefined
            ? null
            : ` ${formatArtifactBytes(latestVersion.size)} ·`}{" "}
          Updated <Timestamp at={latestVersion?.created_at ?? artifact.created_at} />
        </p>
      </div>
      {artifact.primary ? (
        <span
          className={`shrink-0 rounded-full px-2 py-1 text-xs font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
        >
          Primary
        </span>
      ) : null}
      <CopyRefButton route={documentRoute(artifact)} />
      <ApprovalChip artifact={artifact} />
      {latestVersion === undefined ? null : (
        <a
          className={`shrink-0 text-sm font-medium underline ${linkText} ${linkHoverText}`}
          download=""
          href={artifactVersionUrl(artifact.id, latestVersion.number)}
        >
          Download version {latestVersion.number}
        </a>
      )}
      {/* A removal is offered only where the server can take one: a document of this issue that
          is neither its spec nor a document that plays a role, since those are refused whatever
          state they are in (`removalIsOffered`). Every other refusal is the server's, shown on
          the row. */}
      {removalIsOffered(artifact) ? (
        <RemoveDocument
          artifact={artifact}
          refusal={isClosed ? CLOSED_ISSUE_REMOVE_REFUSAL : null}
        />
      ) : null}
    </li>
  );
}

/** The issue's artifacts: a control that makes a document, an upload that a closed issue refuses
 *  before a file is picked, and every artifact the issue holds. */
export function ArtifactsTab({ isClosed }: { readonly isClosed: boolean }): ReactNode {
  const { pathname } = usePageAddress();
  const route = parseIssuePath(pathname);
  const issueKey = route?.key;
  const highlightedSlug = route?.kind === "artifact" ? route.slug : undefined;
  // The words that narrow the list live in the address (`?q=`), so a reader who reloads, shares
  // the tab or comes back to it finds the list narrowed the way they left it.
  const [filter, setFilter] = useCappedAddressParam("q");
  // One sentence covers every control of the strip: a closed issue takes neither a new document
  // nor an upload, and both controls are described by this line.
  const closedRefusalId = useId();
  const closedRefusal = isClosed ? CLOSED_ISSUE_DOCUMENT_REFUSAL : null;
  const upload = useArtifactUpload({ issue: issueKey ?? "" }, { refusal: closedRefusal });
  const artifacts = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["artifacts", issueKey],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Artifact list requires an issue route.");
      }
      return api.listArtifacts(issueKey);
    },
  });
  const references = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey, "references"],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Issue references require an issue route.");
      }
      return api.getIssueReferences(issueKey);
    },
  });
  const orderedArtifacts = useMemo(
    () =>
      [...(artifacts.data ?? [])].sort((left, right) => {
        if (left.primary !== right.primary) {
          return left.primary ? -1 : 1;
        }
        const leftUpdated = (left.versions ?? []).at(-1)?.created_at ?? left.created_at;
        const rightUpdated = (right.versions ?? []).at(-1)?.created_at ?? right.created_at;
        return compareTimestamps(rightUpdated, leftUpdated) || left.id.localeCompare(right.id);
      }),
    [artifacts.data]
  );
  const filterQuery = filter.trim().toLocaleLowerCase();
  const visibleArtifacts =
    filterQuery === ""
      ? orderedArtifacts
      : orderedArtifacts.filter(
          (artifact) =>
            artifact.name.toLocaleLowerCase().includes(filterQuery) ||
            artifact.kind.toLocaleLowerCase().includes(filterQuery)
        );

  if (issueKey === undefined) {
    return (
      <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Open an issue to view its artifacts.</p>
    );
  }
  if (artifacts.isPending) {
    return <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading artifacts…</p>;
  }
  if (artifacts.isError) {
    return <p className={`pt-3 text-sm ${dangerText}`}>Could not load artifacts.</p>;
  }

  return (
    <div className="space-y-3 pt-3">
      {/* One control strip: what narrows the list at its head, what adds to it after. */}
      <ControlRow
        search={
          <SearchBox
            className="w-full"
            label="Filter artifacts"
            onChange={setFilter}
            placeholder="Filter artifacts"
            value={filter}
          />
        }
      >
        <CreateDocument
          documents={orderedArtifacts}
          owner={{ issue: issueKey }}
          refusal={closedRefusal}
          refusalShownBy={closedRefusalId}
        />
        <ArtifactUploadRow refusalShownBy={closedRefusalId} upload={upload} />
        {closedRefusal === null ? null : (
          <span className={`text-xs ${textMutedOnSurface}`} id={closedRefusalId}>
            {closedRefusal}
          </span>
        )}
      </ControlRow>
      {references.data === undefined ? null : <References references={references.data} />}
      <ArtifactDropZone upload={upload}>
        {visibleArtifacts.length === 0 ? (
          <p className={`text-sm ${textMutedOnSurface}`}>
            {orderedArtifacts.length === 0
              ? "No artifacts yet."
              : "No artifacts match this filter."}
          </p>
        ) : (
          <ul className={`rounded-lg border px-3 ${borderDefault}`}>
            {visibleArtifacts.map((artifact) => (
              <ArtifactRow
                artifact={artifact}
                highlighted={artifact.slug === highlightedSlug}
                isClosed={isClosed}
                issueKey={issueKey}
                key={artifact.id}
              />
            ))}
          </ul>
        )}
      </ArtifactDropZone>
    </div>
  );
}
