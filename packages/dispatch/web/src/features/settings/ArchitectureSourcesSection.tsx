import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage, isSourceNotFound, removedEvenIfGone } from "../../api/client";
import { architectureSourcesQuery, projectsQuery } from "../../api/queries";
import { heldField, heldPress } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import {
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../components/TableShell";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import {
  refusedNote,
  stoppedNote,
  useKeyedWrite,
  useRefusalOfSubject,
} from "../writes/useKeyedWrite";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
} from "./classes";
import { NoProjectPicked, ProjectSelect } from "./ProjectSelect";

/** A source's address: a project and one of its repositories. */
interface SourceKey {
  readonly project: string;
  readonly repo: string;
}

/** `source` as the section's sentences name it: `the architecture source acme/web for CORE`. */
function sourceName(source: SourceKey): string {
  return `the architecture source ${source.repo} for ${source.project}`;
}

/** A project's architecture sources, one row per repository: the branch its architecture
 * documents are imported from, the commit imported and how many components it supplies. Saving
 * runs the server's GitHub App access check, so a listed row has proven access — it shows
 * "Access verified" until a sync reports otherwise — and a failed check surfaces the server's
 * reason inline. Saving a repository the project already has changes that source's branch. */
export function ArchitectureSourcesSection(): ReactNode {
  const queryClient = useQueryClient();
  const [project, setProject] = useState("");
  // The form was submitted with no project picked, and says so until one is.
  const [projectMissing, setProjectMissing] = useState(false);
  const [repository, setRepository] = useState("");
  const [branch, setBranch] = useState("main");
  const submitGuard = useSubmitGuard();
  const sources = useQuery(architectureSourcesQuery());
  const projects = useQuery(projectsQuery());
  // The form shows a refused save beside the repository it was refused over.
  const saveSource = useKeyedWrite({
    inline: true,
    label: (input: { project: string; repo: string; branch: string }) =>
      `${input.project}'s architecture source ${input.repo}`,
    method: "PUT",
    write: (input, bounds) =>
      api.putArchitectureSource(input.project, input.repo, { branch: input.branch }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      // The settings list's key prefixes every project's own list, so both refresh.
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey });
      setProject("");
      setRepository("");
      setBranch("main");
    },
  });
  const deleteSource = useKeyedWrite({
    label: (source: SourceKey) => `${source.project}'s architecture source ${source.repo} removed`,
    // A removal made again after one that landed finds no source (`404 SOURCE_NOT_FOUND`), which
    // is the removal done (`removedEvenIfGone`).
    method: "DELETE",
    write: (source, bounds) =>
      removedEvenIfGone(
        api.deleteArchitectureSource(source.project, source.repo, bounds),
        isSourceNotFound
      ),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey }),
  });
  // A refresh settles into the row either way: a rejected model comes back 200
  // with the reason in last_error, and even a 409 (access revoked, branch gone)
  // recorded its reason on the row before failing, so both invalidate.
  const refreshSource = useKeyedWrite({
    label: (source: SourceKey) =>
      `Refresh of ${source.project}'s architecture source ${source.repo}`,
    method: "POST",
    write: (source, bounds) => api.syncArchitectureSource(source.project, source.repo, bounds),
    onSettled: () =>
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey }),
  });
  // Add source is held while a save or a removal is out, by `aria-disabled`, and a submit sends
  // nothing then. The repository and the branch are held while a save is out, `aria-disabled` and
  // read-only. So the control the reader submitted from keeps the focus.
  const formHeld = saveSource.isPending || deleteSource.isPending;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (formHeld) return;
    // The project list is a button, which no `required` holds: an empty one files nothing here.
    if (project === "") {
      setProjectMissing(true);
      return;
    }
    submitGuard.guard(() => saveSource.mutate({ branch, project, repo: repository }));
  };
  // A removal's and a refresh's refusals sit under the table, each one line for every row, so each
  // names the source it was about, and goes once that source leaves the list - removed in another
  // tab, or by another person - rather than stand under sources it was never about.
  const listed = (source: SourceKey) =>
    sources.data?.some((row) => row.project === source.project && row.repo === source.repo) ===
    true;
  const deleteRefused = useRefusalOfSubject(deleteSource, listed)
    ? deleteSource.variables
    : undefined;
  const refreshRefused = useRefusalOfSubject(refreshSource, listed)
    ? refreshSource.variables
    : undefined;

  return (
    <section aria-labelledby="architecture-sources-heading">
      <h2
        className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
        id="architecture-sources-heading"
      >
        Architecture sources
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        A project can import its architecture documents from several repositories, each on one
        branch. Its component model is every source's components together, so a component id must
        not repeat across a project's sources. Saving verifies the Dispatch GitHub App can read the
        repository and that the branch exists in it (a branch name, not a commit or tag); saving a
        repository the project already has changes its branch.
      </p>

      {sources.isPending || projects.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading architecture sources…</p>
      ) : null}
      {sources.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load architecture sources."
            onRetry={() => void sources.refetch()}
            retrying={sources.isFetching}
          />
        </div>
      ) : null}
      {sources.isSuccess && projects.isSuccess ? (
        <>
          <TableShell className="mt-6" label="Architecture sources">
            <TableHead>
              <TableHeadCell>Project</TableHeadCell>
              <TableHeadCell>Repository</TableHeadCell>
              <TableHeadCell>Branch</TableHeadCell>
              <TableHeadCell>Status</TableHeadCell>
              <TableHeadCell>
                <span className="sr-only">Actions</span>
              </TableHeadCell>
            </TableHead>
            <TableBody>
              {sources.data.length === 0 ? (
                <TableEmpty colSpan={5}>No architecture sources yet.</TableEmpty>
              ) : (
                sources.data.map((source) => (
                  <TableRow key={`${source.project} ${source.repo}`}>
                    <TableCell mono tone="primary">
                      {source.project}
                    </TableCell>
                    <TableCell mono tone="primary">
                      {source.repo}
                    </TableCell>
                    <TableCell mono tone="secondary">
                      {source.branch}
                    </TableCell>
                    <TableCell tone="secondary">
                      {source.last_error !== null ? (
                        <>
                          <span className={dangerText}>{source.last_error}</span>
                          {source.last_commit !== null ? (
                            <div className={textMutedOnSurface}>
                              Previous model stays up at{" "}
                              <span className="font-mono">{source.last_commit.slice(0, 12)}</span>
                              {` · ${componentCount(source.components)}`}
                            </div>
                          ) : null}
                        </>
                      ) : source.last_commit !== null ? (
                        <>
                          Synced{" "}
                          <span className="font-mono">{source.last_commit.slice(0, 12)}</span>
                          {` · ${componentCount(source.components)}`}
                          {source.last_sync_at !== null ? (
                            <>
                              {" "}
                              <Timestamp at={source.last_sync_at} className={textMutedOnSurface} />
                            </>
                          ) : null}
                        </>
                      ) : (
                        "Access verified"
                      )}
                    </TableCell>
                    {/* A row's Refresh and Delete are held while a write of the section is out
                        (`heldPress`), not disabled, so the one the reader pressed keeps the focus
                        and a press of either then sends nothing. */}
                    <TableCell align="right">
                      <button
                        aria-label={`Refresh architecture source ${source.repo} for ${source.project}`}
                        className={`mr-4 font-medium refused:cursor-not-allowed refused:opacity-50 ${linkText} ${linkHoverText}`}
                        {...heldPress(
                          refreshSource.isPending || deleteSource.isPending || saveSource.isPending,
                          () => refreshSource.mutate({ project: source.project, repo: source.repo })
                        )}
                        type="button"
                      >
                        {refreshSource.isPending &&
                        refreshSource.variables.project === source.project &&
                        refreshSource.variables.repo === source.repo
                          ? "Refreshing…"
                          : "Refresh"}
                      </button>
                      <button
                        aria-label={`Delete architecture source ${source.repo} for ${source.project}`}
                        className={`font-medium refused:cursor-not-allowed refused:opacity-50 ${dangerText} ${dangerHoverText}`}
                        {...heldPress(deleteSource.isPending || saveSource.isPending, () =>
                          deleteSource.mutate({ project: source.project, repo: source.repo })
                        )}
                        type="button"
                      >
                        Delete
                      </button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </TableShell>

          <form
            className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-2 sm:items-end ${borderDefault}`}
            onSubmit={submit}
          >
            <ProjectSelect
              disabled={saveSource.isPending}
              label="Source project"
              onChange={(picked) => {
                setProject(picked);
                setProjectMissing(false);
              }}
              projects={projects.data}
              value={project}
            />
            <label className={settingsFieldLabel} htmlFor="architecture-source-repository">
              Source repository
              <input
                {...heldField(saveSource.isPending)}
                className={settingsMonoInput}
                id="architecture-source-repository"
                onChange={(event) => setRepository(event.target.value)}
                pattern={ownerRepoPattern}
                placeholder="owner/repo"
                required
                value={repository}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="architecture-source-branch">
              Branch
              <input
                {...heldField(saveSource.isPending)}
                className={settingsMonoInput}
                id="architecture-source-branch"
                onChange={(event) => setBranch(event.target.value)}
                required
                value={branch}
              />
            </label>
            <button
              aria-disabled={formHeld || undefined}
              className={settingsSubmitButton}
              type="submit"
            >
              Add source
            </button>
          </form>
          {projectMissing ? <NoProjectPicked /> : null}
          {saveSource.isError ? (
            <div className="mt-4">
              <QueryError
                message={
                  stoppedNote(saveSource, "the architecture source", "saved") ??
                  apiErrorMessage(saveSource.error, "Couldn't save the architecture source.")
                }
                onRetry={() => submitGuard.retryLast(saveSource)}
                retrying={saveSource.isPending}
              />
            </div>
          ) : null}
          {deleteRefused === undefined ? null : (
            <div className="mt-4">
              <QueryError
                message={
                  stoppedNote(
                    deleteSource,
                    `the removal of ${sourceName(deleteRefused)}`,
                    "made"
                  ) ?? `Couldn't delete ${sourceName(deleteRefused)}.`
                }
                onRetry={deleteSource.retry}
                retrying={deleteSource.isPending}
              />
            </div>
          )}
          {refreshRefused === undefined ? null : (
            <div className="mt-4">
              <QueryError
                message={
                  stoppedNote(
                    refreshSource,
                    `the refresh of ${sourceName(refreshRefused)}`,
                    "made"
                  ) ?? refusedNote(`refresh ${sourceName(refreshRefused)}`, refreshSource.error)
                }
                onRetry={refreshSource.retry}
                retrying={refreshSource.isPending}
              />
            </div>
          )}
        </>
      ) : null}
    </section>
  );
}

/** `1 component`, `N components`: how much of the project's model one source supplies. */
function componentCount(count: number): string {
  return count === 1 ? "1 component" : `${count} components`;
}
