import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  dangerText,
  inputClasses,
  linkText,
  surfaceMutedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ChangeView } from "../doc/ChangeView";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import {
  artifactVersionUrl,
  formatArtifactBytes,
  versionLabel,
  versionsNewestFirst,
} from "./ArtifactHeader";

// A blob (image/file) has no markdown to diff, so its "compare" is size and checksum, not a
// text diff — this is what a document's From/To compare falls back to when either side lacks
// text content.
function BlobVersionComparison({ after, before }: { after: Version; before: Version }): ReactNode {
  const identical = before.size === after.size && before.sha256 === after.sha256;

  return (
    <section aria-label="Blob version comparison" className="space-y-2">
      <div className="grid grid-cols-2 gap-2">
        <div className={`rounded-lg p-3 ${borderDefault} ${surfaceMutedBg}`}>
          <p className={`text-xs font-medium ${textMutedOnSurfaceMuted}`}>
            From · Version {before.number}
          </p>
          <p className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            {formatArtifactBytes(before.size)}
          </p>
          <p className={`mt-1 break-all text-xs ${textMutedOnSurfaceMuted}`}>
            SHA-256 {before.sha256}
          </p>
        </div>
        <div className={`rounded-lg p-3 ${borderDefault} ${surfaceMutedBg}`}>
          <p className={`text-xs font-medium ${textMutedOnSurfaceMuted}`}>
            To · Version {after.number}
          </p>
          <p className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            {formatArtifactBytes(after.size)}
          </p>
          <p className={`mt-1 break-all text-xs ${textMutedOnSurfaceMuted}`}>
            SHA-256 {after.sha256}
          </p>
        </div>
      </div>
      {identical ? <p className={`text-sm ${textMutedOnSurface}`}>Identical blobs.</p> : null}
    </section>
  );
}

/** One side of the From/To compare: a list of every version, newest first, whose text box
 *  narrows them. The word over it is for the eye: the trigger is named for the whole side
 *  (`Compare <name> from`), which is what a screen reader hears. */
function CompareVersionSelect({
  ariaLabel,
  label,
  onChange,
  value,
  versions,
}: {
  ariaLabel: string;
  label: string;
  onChange: (version: number) => void;
  value: number | undefined;
  versions: readonly Version[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  const labels = new Map(
    versions.map((version) => [String(version.number), versionLabel(version)])
  );
  return (
    <div className={`text-xs font-medium ${textSecondaryOnSurface}`}>
      <span aria-hidden="true">{label}</span>
      <MultiSelect
        emptyMessage="No version matches that."
        label={ariaLabel}
        mode="single"
        onOpenChange={setOpen}
        onPick={(number) => onChange(Number(number))}
        open={open}
        optionLabel={(number) => labels.get(number) ?? `Version ${number}`}
        options={versions.map((version) => String(version.number))}
        searchLabel="Search versions"
        triggerClassName={`mt-1 block min-h-11 w-full rounded border px-2 py-2 text-left text-sm ${inputClasses(true)}`}
        value={value === undefined ? undefined : String(value)}
      />
    </div>
  );
}

// Rendered below the document/blob on the artifact page only: the version history, a From/To
// compare across any two versions, and the artifact's own inbound references. The page's
// `ArtifactHeader` already offers a version picker plus "Diff vs current" for documents (only
// current vs. one historical version); this From/To compare covers the arbitrary-pair case
// (e.g. version 3 vs version 7) that picker can't, for both docs and blobs.
export function ArtifactDetails({ artifact }: { artifact: Artifact }): ReactNode {
  const [showAllVersions, setShowAllVersions] = useState(false);
  // A diff is render-heavy and long; it appears only after the reader asks for it.
  const [compareOpen, setCompareOpen] = useState(false);
  const versions = versionsNewestFirst(artifact.versions);
  const namedVersions = versions.filter((version) => version.named);
  const displayedVersions = showAllVersions ? versions : namedVersions;
  const beforeDefault = versions[1]?.number;
  const afterDefault = versions[0]?.number;
  const [before, setBefore] = useState<number | undefined>();
  const [after, setAfter] = useState<number | undefined>();
  const beforeVersion = before ?? beforeDefault;
  const afterVersion = after ?? afterDefault;
  const beforeContent = useQuery({
    enabled: compareOpen && artifact.kind === "doc" && beforeVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", beforeVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, beforeVersion ?? 0),
  });
  const afterContent = useQuery({
    enabled: compareOpen && artifact.kind === "doc" && afterVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", afterVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, afterVersion ?? 0),
  });
  const beforeBlob = versions.find((version) => version.number === beforeVersion);
  const afterBlob = versions.find((version) => version.number === afterVersion);

  return (
    <div className="space-y-4">
      <section aria-label={`Versions for ${artifact.name}`} className="space-y-2">
        <div className="flex items-center justify-between gap-3">
          <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Versions</h3>
          {versions.length > namedVersions.length ? (
            <button
              className={`min-h-11 text-sm font-medium ${linkText}`}
              onClick={() => setShowAllVersions((current) => !current)}
              type="button"
            >
              {showAllVersions ? "Show named versions" : "Show all versions"}
            </button>
          ) : null}
        </div>
        {displayedVersions.length === 0 ? (
          <p className={`text-sm ${textMutedOnSurface}`}>No named versions yet.</p>
        ) : (
          <ul className="space-y-1">
            {displayedVersions.map((version) => (
              <li
                className="flex flex-wrap items-center justify-between gap-2 text-sm"
                key={version.number}
              >
                <span className={textSecondaryOnSurface}>{versionLabel(version)}</span>
                <a
                  className={`inline-flex min-h-11 items-center font-medium ${linkText}`}
                  download=""
                  href={artifactVersionUrl(artifact.id, version.number, { download: true })}
                >
                  Download version {version.number}
                </a>
                <span className={`w-full break-all text-xs ${textMutedOnSurface}`}>
                  <Timestamp at={version.created_at} />
                  {version.size === undefined ? null : ` · ${formatArtifactBytes(version.size)}`}
                  {artifact.kind === "doc" ? null : ` · SHA-256 ${version.sha256 ?? "unavailable"}`}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {versions.length >= 2 ? (
        <section aria-label={`Compare versions for ${artifact.name}`} className="space-y-2">
          <div className="flex items-center justify-between gap-3">
            <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Compare versions</h3>
            <button
              aria-expanded={compareOpen}
              className={`min-h-11 text-sm font-medium ${linkText}`}
              onClick={() => setCompareOpen((current) => !current)}
              type="button"
            >
              {compareOpen ? "Hide comparison" : "Show comparison"}
            </button>
          </div>
          {compareOpen ? (
            <>
              <div className="grid grid-cols-2 gap-2">
                <CompareVersionSelect
                  ariaLabel={`Compare ${artifact.name} from`}
                  label="From"
                  onChange={setBefore}
                  value={beforeVersion}
                  versions={versions}
                />
                <CompareVersionSelect
                  ariaLabel={`Compare ${artifact.name} to`}
                  label="To"
                  onChange={setAfter}
                  value={afterVersion}
                  versions={versions}
                />
              </div>
              {artifact.kind === "doc" ? (
                beforeContent.isError || afterContent.isError ? (
                  <p className={`text-sm ${dangerText}`}>Could not load versions to compare.</p>
                ) : isText(beforeContent.data) && isText(afterContent.data) ? (
                  <ChangeView
                    after={afterContent.data.markdown}
                    before={beforeContent.data.markdown}
                  />
                ) : (
                  <p className={`text-sm ${textMutedOnSurface}`}>Loading versions to compare…</p>
                )
              ) : beforeBlob !== undefined && afterBlob !== undefined ? (
                <BlobVersionComparison after={afterBlob} before={beforeBlob} />
              ) : (
                <p className={`text-sm ${textMutedOnSurface}`}>Select two versions to compare.</p>
              )}
            </>
          ) : null}
        </section>
      ) : null}

      <ReferencedBy reference={buildDispatchReference(documentRoute(artifact))} />
    </div>
  );
}
