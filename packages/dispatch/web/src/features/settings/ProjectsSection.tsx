import { PROJECT_NAME_MAX } from "@legion/contracts";
import { type FormEvent, type ReactNode, useState } from "react";
import { useCache, useMutations } from "../../api/cache/cache";
import { useShownQuery } from "../../api/cache/shown-query";
import { api, apiErrorMessage } from "../../api/client";
import {
  projectDefaultAssigneesQuery,
  projectsQuery,
  usersQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { CreateProjectInput, ProjectDefaultAssignee } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  inputClasses,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

/** Lists native projects, lets a human create one, and carries each project's default assignee -
 * the person an issue created with nobody named lands on, and the person a question on an issue
 * nobody holds reaches. Every project names one and no write clears it, so each row of the table
 * and the create form below it name a person. The Settings route's repository mapping form reads
 * the same `["projects"]` query, so a project created here is immediately selectable there
 * without any explicit wiring between the two sections. */
export function ProjectsSection(): ReactNode {
  const cache = useCache();
  const [projectKey, setProjectKey] = useState("");
  const [projectName, setProjectName] = useState("");
  // Who the new project defaults to until the reader picks somebody else: `null` is "nobody has
  // picked", not "nobody", and reads as the viewer below.
  const [projectAssignee, setProjectAssignee] = useState<string | null>(null);
  const projectSubmitGuard = useSubmitGuard();
  const projects = useShownQuery(projectsQuery());
  // The form shows a refused create beside the key it was refused over.
  const createProject = useKeyedWrite({
    inline: true,
    label: (input: CreateProjectInput) => `New project ${input.key}`,
    unlined: "a create names no row until Dispatch makes the project",
    write: (input, bounds) => api.createProject(input, bounds),
    onSettled: () => projectSubmitGuard.release(),
    // Made either way, so the form empties; with no answer to give, it says what was made
    // (`createProject.unanswered`).
    onSuccess: () => {
      void cache.invalidate({ queryKey: projectsQuery().queryKey });
      // The project list carries no default assignee: the defaults read is the one place the new
      // project's default comes from, and the read the page holds was answered before it existed.
      void cache.invalidate({ queryKey: projectDefaultAssigneesQuery().queryKey });
      setProjectKey("");
      setProjectName("");
      setProjectAssignee(null);
    },
  });
  const defaults = useShownQuery(projectDefaultAssigneesQuery());
  // The people list is read only once a reader reaches for a picker, as `AssigneeControl` does
  // it, so opening Settings makes no request for it.
  const [wantUsers, setWantUsers] = useState(false);
  const users = useShownQuery({ ...usersQuery(), enabled: wantUsers });
  const defaultAssignees = new Map((defaults.data ?? []).map((row) => [row.project, row.assignee]));
  const logins = (users.data ?? []).map((user) => user.login);
  // The server takes the person creating the project when the call names nobody, so that is what
  // the form starts on; the reader may pick anyone else who has signed in.
  const viewer = useShownQuery(whoAmIQuery()).data?.login;
  const newProjectAssignee = projectAssignee ?? viewer ?? "";
  const submitProject = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // An emptied form - a create that landed - sends nothing, whatever submits it, and so does
    // one whose default assignee is nobody, which is a page whose viewer has not resolved.
    if (projectKey === "" || projectName.trim() === "" || newProjectAssignee === "") return;
    projectSubmitGuard.guard(() =>
      createProject.mutate({
        default_assignee: newProjectAssignee,
        key: projectKey,
        name: projectName,
      })
    );
  };

  return (
    <section aria-labelledby="projects-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="projects-heading">
        Projects
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Native issues are grouped under a project key, such as CORE-1.
      </p>

      {projects.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading projects…</p> : null}
      {projects.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load projects."
            onRetry={() => void projects.refetch()}
            retrying={projects.isFetching}
          />
        </div>
      ) : null}
      {projects.isSuccess ? (
        <>
          <div className={settingsTableWrapper}>
            <table className="w-full text-left text-sm">
              <thead className={settingsTableHead}>
                <tr>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Key
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Name
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Default assignee
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Created
                  </th>
                </tr>
              </thead>
              <tbody>
                {projects.data.length === 0 ? (
                  <tr>
                    <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                      No projects yet.
                    </td>
                  </tr>
                ) : (
                  projects.data.map((candidate) => (
                    <tr className={settingsTableRow} key={candidate.key}>
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {candidate.key}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>{candidate.name}</td>
                      <td className="px-4 py-3">
                        <DefaultAssigneeCell
                          logins={logins}
                          onReach={() => setWantUsers(true)}
                          project={candidate.key}
                          readFailed={defaults.isError}
                          // A project the defaults read has not answered for - one created since
                          // that read - reads as not read yet rather than as nobody: every project
                          // names a person, and the read behind this cell is the one place it is.
                          stored={defaultAssignees.get(candidate.key)}
                        />
                      </td>
                      <td className={`px-4 py-3 ${textMutedOnSurface}`}>
                        <Timestamp at={candidate.created_at} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
            Every project names a default assignee, and it cannot be cleared: an issue created with
            nobody named, and with no person, token owner or assigned parent to take it, goes to
            that person, and so does a question on an issue nobody holds. An issue created from a
            repository with no project mapping lands in the default project, so that project's
            default assignee receives each such issue too.
          </p>
          {defaults.isError ? (
            <div className="mt-4">
              <QueryError
                message="Couldn't load the default assignees."
                onRetry={() => void defaults.refetch()}
                retrying={defaults.isFetching}
              />
            </div>
          ) : null}

          <form
            className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_2fr_1fr_auto] sm:items-end ${borderDefault}`}
            onSubmit={submitProject}
          >
            <label className={settingsFieldLabel} htmlFor="project-key">
              Key
              <input
                className={settingsMonoInput}
                disabled={createProject.isPending}
                id="project-key"
                onChange={(event) => {
                  createProject.forgetUnanswered();
                  setProjectKey(event.target.value.trim().toUpperCase());
                }}
                placeholder="CORE"
                required
                value={projectKey}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="project-name">
              Name
              <input
                className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={createProject.isPending}
                id="project-name"
                maxLength={PROJECT_NAME_MAX}
                onChange={(event) => setProjectName(event.target.value)}
                placeholder="Core"
                required
                value={projectName}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="project-default-assignee">
              Default assignee
              <select
                className={`w-full min-w-0 rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={createProject.isPending || newProjectAssignee === ""}
                id="project-default-assignee"
                onChange={(event) => setProjectAssignee(event.target.value)}
                onFocus={() => setWantUsers(true)}
                onPointerEnter={() => setWantUsers(true)}
                onTouchStart={() => setWantUsers(true)}
                required
                value={newProjectAssignee}
              >
                {/* The viewer is a person who has signed in, so they are in the list once it is
                    read; until it is, they are the only option. An empty value is nobody picked
                    yet, which is the page before the viewer resolves. */}
                {assigneeOptions(
                  newProjectAssignee === "" ? undefined : newProjectAssignee,
                  logins
                ).map((login) => (
                  <option key={login} value={login}>
                    {login}
                  </option>
                ))}
              </select>
            </label>
            <button
              className={settingsSubmitButton}
              disabled={createProject.isPending}
              type="submit"
            >
              New project
            </button>
          </form>
          {createProject.unanswered === undefined ? null : (
            <div className="mt-4">
              <LandedWithoutAnswer what={`Project ${createProject.unanswered.variables.key}`} />
            </div>
          )}
          {createProject.isError ? (
            <div className="mt-4">
              <QueryError
                message={apiErrorMessage(createProject.error, "Couldn't create the project.")}
                onRetry={() => projectSubmitGuard.retryLast(createProject)}
                retrying={createProject.isPending}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

/** The options a default-assignee picker offers: everyone who has signed in, with the selected
 *  login ahead of them when the list does not hold it, so a control never shows a value its
 *  options lack - a stored default whose person the people list has not read, or has not
 *  fetched yet, still holds its project. `undefined` is nobody selected, and the list stands as
 *  it is. Both pickers of this section read this one rule: the create form's and
 *  `DefaultAssigneeSelect`'s. */
function assigneeOptions(
  selected: string | undefined,
  logins: readonly string[]
): readonly string[] {
  return selected === undefined || logins.includes(selected) ? logins : [selected, ...logins];
}

/** One project's default-assignee cell: the picker and, under it, the error of this project's
 *  newest save when the server refused it. `stored` is the person the page has read, and
 *  `undefined` while the defaults read has not answered for this project: the control then takes
 *  no pick and names nobody, since who holds the project is not known yet. */
function DefaultAssigneeCell({
  logins,
  onReach,
  project,
  readFailed,
  stored,
}: {
  logins: readonly string[];
  onReach: () => void;
  project: string;
  readFailed: boolean;
  stored: string | undefined;
}): ReactNode {
  const write = useDefaultAssigneeWrite(project);
  const unread = stored === undefined ? (readFailed ? "Unavailable" : "Loading…") : null;
  return (
    <>
      <DefaultAssigneeSelect
        assignee={write.shown ?? stored}
        logins={logins}
        onPick={write.submit}
        onReach={onReach}
        project={project}
        unread={unread}
      />
      {write.error === null ? null : (
        <div className="mt-2">
          <QueryError
            message={apiErrorMessage(write.error, "Couldn't update the default assignee.")}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </div>
      )}
    </>
  );
}

/** One pick of a project's default assignee, which always names a person: no write clears a
 *  default. Each pick is its own object, so a save can tell whether it is still the newest pick
 *  by identity. */
type DefaultAssigneePick = { assignee: string };

/** What a pick's save resolves to when a newer pick was waiting behind it: nothing was sent. */
type DefaultAssigneeSuperseded = { superseded: true };

/** One project's default-assignee saves. The mutation scope runs a project's saves one at a time
 *  in pick order in the page's mutations, so a save still out or waiting when the row
 *  unmounts keeps its place, and a pick on a remounted row waits behind it. When a save's turn
 *  comes and a newer pick is waiting behind it, it is superseded and sends nothing: while a save
 *  is out, only the newest pick follows it, from whichever mount made it. The server ends at the
 *  reader's last pick.
 *
 *  The control shows the newest pick still pending, so a remounted row shows the pick in flight;
 *  once none is pending it shows the stored value. The row's error is read from the cache too:
 *  when the project's newest save was refused, the row says so with its reason, even on a row
 *  mounted after it failed, until a newer pick or Retry starts another save. Each answer lands in
 *  the defaults cache, so the control does not flash back to the old value, and the defaults are
 *  refetched after each save that reached the server settles. */
function useDefaultAssigneeWrite(project: string): {
  /** The project's newest save when the server refused it, until another save starts. */
  error: Error | null;
  /** Whether any save of this project is still out or waiting. */
  pending: boolean;
  /** Resends the refused value; a no-op unless the project's newest save failed. */
  retry: () => void;
  /** The newest pick still saving or queued; `undefined` shows the stored value. */
  shown: string | undefined;
  submit: (assignee: string) => void;
} {
  const cache = useCache();
  const defaultsKey = projectDefaultAssigneesQuery().queryKey;
  const mutationKey = ["project-default-assignee", project];
  const mutation = useKeyedWrite<
    ProjectDefaultAssignee | DefaultAssigneeSuperseded,
    DefaultAssigneePick
  >({
    mutationKey,
    scope: { id: `project-default-assignee:${project}` },
    // A refused save stays in the cache, with no row observing it, until a newer save of the
    // project settles and removes it (see onSettled), so a row mounted later can still say so.
    gcTime: Number.POSITIVE_INFINITY,
    // The row shows its newest save's refusal itself, beside the control it was refused over.
    inline: true,
    label: (pick) => `${project}'s default assignee ${pick.assignee}`,
    unlined: "one project's picks are ordered by the newest press, not by a line (`scope`)",
    write: async (pick, bounds): Promise<ProjectDefaultAssignee | DefaultAssigneeSuperseded> => {
      // The page lists mutations in the order they were made, so the last pending one is the
      // newest pick. Any other is waiting behind this save, and only it need reach the server.
      const waiting = cache.mutations<DefaultAssigneePick>({
        exact: true,
        mutationKey,
        status: "pending",
      });
      if (waiting.at(-1)?.variables !== pick) {
        return { superseded: true };
      }
      return api.putProjectDefaultAssignee(project, { assignee: pick.assignee }, bounds);
    },
    // A defaults read already out was asked before this save and could land over its answer.
    onMutate: () => cache.cancel({ queryKey: defaultsKey }),
    onSuccess: (saved) => {
      // Saved with no answer to give: the store reads every query again.
      if (saved === null || "superseded" in saved) {
        return;
      }
      cache.setDispatched<ProjectDefaultAssignee[]>(defaultsKey, (rows) => {
        if (rows === undefined) {
          return rows;
        }
        const others = rows.filter((row) => row.project !== saved.project);
        return [...others, saved];
      });
    },
    onSettled: (saved) => {
      // With gcTime Infinity nothing else clears this project's saves. A save settles in pick
      // order and is still pending while this runs, so every settled save left is older than it
      // and no longer the one a row reports: drop them, keeping at most the newest settled save
      // and the pending ones. cache.forgetSettledMutations({ exact: true, mutationKey });
      // A superseded save changed nothing on the server. Not awaited: the next pick in the scope
      // need not wait for this refetch.
      if (saved == null || !("superseded" in saved)) {
        void cache.invalidate({ queryKey: defaultsKey });
      }
    },
  });
  const saves = useMutations<DefaultAssigneePick>({ exact: true, mutationKey });
  const pendingPicks = saves.filter((save) => save.status === "pending");
  // The page lists mutations in the order they were made, so the last is the newest.
  const newest = saves.at(-1);
  const refused = newest?.status === "error" ? newest : undefined;
  return {
    error: refused?.error ?? null,
    pending: pendingPicks.length > 0,
    retry: () => {
      if (refused?.variables !== undefined) {
        mutation.mutate({ assignee: refused.variables.assignee });
      }
    },
    shown: pendingPicks.at(-1)?.variables?.assignee,
    submit: (assignee) => mutation.mutate({ assignee }),
  };
}

/** One project's default-assignee picker: everyone who has signed in, and nothing else - no
 *  write clears a default, so the control offers no way to name nobody. `onReach` fires the
 *  moment a reader points at, touches or focuses the control, which is when the people list is
 *  fetched. Labelled `Default assignee of <KEY>`. While the defaults are unread, `unread` is the
 *  control's only option and the control is disabled. */
function DefaultAssigneeSelect({
  assignee,
  logins,
  onPick,
  onReach,
  project,
  unread,
}: {
  /** The person the project names, or `undefined` while `unread` says it has not been read. */
  assignee: string | undefined;
  logins: readonly string[];
  onPick: (assignee: string) => void;
  onReach: () => void;
  project: string;
  unread: string | null;
}): ReactNode {
  const options = assigneeOptions(assignee, logins);
  return (
    <select
      aria-label={`Default assignee of ${project}`}
      className={`rounded-md px-3 py-2 disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(false)} ${textPrimaryOnSurface}`}
      disabled={unread !== null}
      onChange={(event) => onPick(event.target.value)}
      onFocus={onReach}
      onPointerEnter={onReach}
      onTouchStart={onReach}
      value={unread ?? assignee ?? ""}
    >
      {unread === null ? (
        options.map((login) => (
          <option key={login} value={login}>
            {login}
          </option>
        ))
      ) : (
        <option value={unread}>{unread}</option>
      )}
    </select>
  );
}
