import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { TruncatedText } from "../../components/TruncatedText";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  borderStrong,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonText,
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
import { ApprovalChip } from "../doc/ApprovalChip";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, buildProjectPath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";

function documentUpdatedAt(document: Artifact): string {
  return document.versions?.at(-1)?.created_at ?? document.created_at;
}

/** What the form says of a title the project already holds a document under. */
function nameTaken(name: string): string {
  return `A document named ${name} already exists`;
}

/** What the form says of a refusal that says nothing itself: it calls what it could not make a
 *  document, the thing this form makes, rather than an artifact. */
const CREATE_REFUSAL = "Could not create the document.";

/** What the form says when Dispatch refused the document it posted under `name`. Dispatch
 *  refuses an upload under the name of a document that already exists unless it names the
 *  version it replaces (`400 PRECONDITION_REQUIRED`), and a new document replaces nothing, so
 *  it names none: the name is held by a document this list did not show - one created since the
 *  list was read, or one whose slug this title matches - which is what the duplicate check says
 *  above in the page's own words. The server's own sentence for that one is written for a caller
 *  holding the `dispatch` command, which a person in a browser has no way to run, so the form
 *  says the name is taken instead. Every other refusal is shown in its own words: the create is
 *  `inline`, so the shell lets go of a write the reader stopped and this form is the only place
 *  they read what became of it, and what a stopped write says is that a Retry asks again under
 *  the same key. */
function createRefusal(error: Error | null, name: string | undefined): string {
  if (name !== undefined && error instanceof ApiError && error.code === "PRECONDITION_REQUIRED") {
    return nameTaken(name);
  }
  return error === null || error.message === "" ? CREATE_REFUSAL : error.message;
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
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [duplicateName, setDuplicateName] = useState<string | undefined>();
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const navigate = useNavigate();
  // Documents upload the moment a file is picked or dropped; there is no staging step here.
  const upload = useArtifactUpload({ project }, { immediate: true });
  const documents = useQuery({
    queryKey: ["project", project, "artifacts"],
    queryFn: () => api.listProjectArtifacts(project, true),
  });
  // The form shows its own refusal under the name it was refused over.
  const create = useKeyedWrite({
    inline: true,
    label: (name: string) => `New document ${name}`,
    write: (name: string, bounds) =>
      api.uploadArtifact({ project }, { content: `# ${name}\n`, name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (made) => {
      setCreating(false);
      setTitle("");
      void queryClient.invalidateQueries({ queryKey: ["project", project, "artifacts"] });
      // Made, with no answer to open: the form closes, so a second press makes nothing, and the
      // list says what was made (`create.unanswered`).
      if (made === null) return;
      navigate(buildProjectPath({ kind: "document", project, slug: made.artifact.slug }));
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = title.trim();
    if (name === "") {
      return;
    }
    if ((documents.data ?? []).some((document) => document.name === name)) {
      setDuplicateName(name);
      return;
    }
    setDuplicateName(undefined);
    submitGuard.guard(() => create.mutate(name));
  };

  if (documents.isPending) {
    return <LoadingSkeleton label="Loading project documents" />;
  }
  if (documents.isError) {
    return <p className={dangerText}>Could not load documents.</p>;
  }

  return (
    <section aria-label="Project documents" className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${secondaryButtonBorder} ${secondaryButtonText}`}
          onClick={() => {
            setCreating(true);
            setDuplicateName(undefined);
            create.forgetUnanswered();
          }}
          type="button"
        >
          New document
        </button>
      </div>
      {create.unanswered === undefined ? null : (
        <LandedWithoutAnswer what={`Document ${create.unanswered.variables}`} />
      )}
      {creating ? (
        <form aria-label="New document" className="space-y-3" onSubmit={submit}>
          <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
            Title
            <input
              className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
              onChange={(event) => {
                setTitle(event.target.value);
                setDuplicateName(undefined);
              }}
              required
              value={title}
            />
          </label>
          {duplicateName === undefined ? null : (
            <p className={dangerText} role="alert">
              {nameTaken(duplicateName)}
            </p>
          )}
          {create.isError ? (
            <p className={dangerText} role="alert">
              {createRefusal(create.error, create.variables)}
            </p>
          ) : null}
          <button
            className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
            disabled={create.isPending}
            type="submit"
          >
            {create.isPending ? "Creating…" : "Create"}
          </button>
        </form>
      ) : null}
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
