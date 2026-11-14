import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useShownQuery } from "../../api/cache/shown-query";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  borderStrong,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { formatArtifactBytes, versionsNewestFirst } from "../artifacts/ArtifactHeader";
import {
  ArtifactDropZone,
  ArtifactUploadStatus,
  useArtifactUpload,
} from "../artifacts/ArtifactUpload";
import { CreateDocument } from "../artifacts/CreateDocument";
import { ApprovalChip } from "../doc/ApprovalChip";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, buildProjectPath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

function documentUpdatedAt(document: Artifact): string {
  return document.versions?.at(-1)?.created_at ?? document.created_at;
}

function DocumentRow({ document }: { document: Artifact }): ReactNode {
  // The latest version's stored size (its markdown's bytes), so a document near the size cap
  // shows in the list without being opened.
  const size = versionsNewestFirst(document.versions)[0]?.size;
  return (
    <li
      aria-label={document.name}
      // From `sm` a four-column grid; on a phone the name takes its own line and the kind, the
      // issue and the time share the line under it; a two-column grid would stack the kind
      // over a blank cell and put the time a line below both.
      className={`flex flex-wrap items-center gap-x-4 gap-y-2 border-t py-3 sm:grid sm:grid-cols-[minmax(0,1fr)_auto_auto_auto] ${borderDefault}`}
    >
      <div className="min-w-0 basis-full">
        <div className="flex min-w-0 items-center gap-1">
          {document.issue_key === null ? (
            <Link
              className={`min-w-0 truncate font-medium ${linkText} ${linkHoverText}`}
              to={buildProjectPath({
                kind: "document",
                project: document.project,
                slug: document.slug,
              })}
            >
              <TruncatedText title={document.name}>{document.name}</TruncatedText>
            </Link>
          ) : (
            <Link
              className={`min-w-0 truncate font-medium ${linkText} ${linkHoverText}`}
              to={buildIssuePath({
                key: document.issue_key,
                kind: "artifact",
                slug: document.slug,
              })}
            >
              <TruncatedText title={document.name}>{document.name}</TruncatedText>
            </Link>
          )}
          <CopyRefButton route={documentRoute(document)} />
        </div>
        <ApprovalChip artifact={document} />
      </div>
      <span className={`text-xs ${textSecondaryOnCanvas}`}>
        {document.kind}
        {size === undefined ? null : ` · ${formatArtifactBytes(size)}`}
      </span>
      <span className="text-xs max-sm:empty:hidden">
        {document.issue_key === null ? null : (
          <Link
            className={`rounded-full border px-2 py-1 font-medium ${borderDefault} ${surfaceMutedBg} ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: document.issue_key, kind: "issue" })}
          >
            {document.issue_key}
          </Link>
        )}
      </span>
      <Timestamp at={documentUpdatedAt(document)} className={`text-xs ${textMutedOnCanvas}`} />
    </li>
  );
}

export function DocumentList({ project }: { project: string }): ReactNode {
  // Documents upload the moment a file is picked or dropped; there is no staging step here.
  const upload = useArtifactUpload({ project }, { immediate: true });
  const documents = useShownQuery({
    queryKey: ["project", project, "artifacts"],
    queryFn: () => api.listProjectArtifacts(project, true),
  });

  if (documents.isPending) {
    return <LoadingSkeleton label="Loading project documents" />;
  }
  if (documents.isError) {
    return <p className={dangerText}>Could not load documents.</p>;
  }

  return (
    <section aria-label="Project documents" className="space-y-4">
      <CreateDocument documents={documents.data ?? []} owner={{ project }} />
      <section className={`space-y-3 border-b py-3 ${borderDefault}`}>
        <label
          className={`block text-sm font-medium ${textSecondaryOnSurface}`}
          htmlFor="artifact-summary"
        >
          Summary (optional)
          <input
            className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
            id="artifact-summary"
            onChange={(event) => upload.setSummary(event.target.value)}
            value={upload.summary}
          />
        </label>
        <ArtifactDropZone upload={upload}>
          <button
            className={`w-full rounded-xl border border-dashed p-4 text-center ${borderStrong} ${surfaceMutedBg}`}
            onClick={upload.openPicker}
            type="button"
          >
            <span className={`block text-sm ${textSecondaryOnSurface}`}>
              Drop a file here, or choose one to upload.
            </span>
            <span
              className={`mt-3 inline-block rounded-lg border px-3 py-2 text-sm font-medium ${borderStrong} ${surfaceBg} ${textSecondaryOnSurface}`}
            >
              Choose file
            </span>
          </button>
        </ArtifactDropZone>
        <ArtifactUploadStatus upload={upload} />
      </section>
      {(documents.data ?? []).length === 0 ? (
        <EmptyState label="Project documents empty state" message="No documents yet." />
      ) : (
        <ul>
          {(documents.data ?? []).map((document) => (
            <DocumentRow document={document} key={document.id} />
          ))}
        </ul>
      )}
    </section>
  );
}
