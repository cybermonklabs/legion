import { PROJECT_NAME_MAX } from "@legion/contracts";
import type { Mutation, QueryKey } from "@tanstack/react-query";
import { useMutationState, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage, type WriteBounds } from "../../api/client";
import {
  projectDefaultAssigneesQuery,
  projectsQuery,
  usersQuery,
  whoAmIQuery,
} from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type {
  CreateProjectInput,
  Project,
  ProjectDefaultAssignee,
  ProjectDefaultRepository,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useRestrictedMember } from "../../hooks/useRestrictedMember";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { dropSettledWrites } from "../writes/settled-writes";
import { type KeyedPress, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import {
  NOT_OWNER_NAME,
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
  wholeValue,
} from "./classes";

/** Lists native projects, lets a human create one, and carries two settings of each project:
 *
 *  - The default assignee: the person an issue created with nobody named lands on, and the
 *    person a question on an issue nobody holds reaches. Every project names one and no write
 *    clears it, so each row of the table and the create form below it name a person. The
 *    defaults read (`projectDefaultAssigneesQuery`) holds it.
 *  - The default repository: the `owner/name` an issue of the project resolves to when neither
 *    the issue nor an issue above it names a repository. A project has one or none, and a person
 *    sets or clears it in the project's row. The project list (`projectsQuery`) holds it, as
 *    `default_repo`, which a project with none does not carry.
 *
 * A new project and a project's default repository belong to the deployment, whose writes
 * Dispatch refuses a restricted member (403 RESTRICTED_MEMBER), so their page lists the projects
 * that name them, shows each one's default repository as text, and offers no create form and no
 * control that sets or clears a default repository. */
export function ProjectsSection(): ReactNode {
  const queryClient = useQueryClient();
  const [projectKey, setProjectKey] = useState("");
  const [projectName, setProjectName] = useState("");
  // Who the new project defaults to until the reader picks somebody else: `null` is "nobody has
  // picked", not "nobody", and reads as the viewer below.
  const [projectAssignee, setProjectAssignee] = useState<string | null>(null);
  const projectSubmitGuard = useSubmitGuard();
  const projects = useQuery(projectsQuery());
  const restricted = useRestrictedMember();
  // The form shows a refused create beside the key it was refused over.
  const createProject = useKeyedWrite({
    inline: true,
    label: (input: CreateProjectInput) => `New project ${input.key}`,
    method: "POST",
    write: (input, bounds) => api.createProject(input, bounds),
    onSettled: () => projectSubmitGuard.release(),
    // Made either way, so the form empties; with no answer to give, it says what was made
    // (`createProject.unanswered`).
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      // The project list carries no default assignee: the defaults read is the one place the new
      // project's default comes from, and the read the page holds was answered before it existed.
      void queryClient.invalidateQueries({ queryKey: projectDefaultAssigneesQuery().queryKey });
      setProjectKey("");
      setProjectName("");
      setProjectAssignee(null);
    },
  });
  const defaults = useQuery(projectDefaultAssigneesQuery());
  // The people list is read only once a reader reaches for a picker, as `AssigneeControl` does
  // it, so opening Settings makes no request for it.
  const [wantUsers, setWantUsers] = useState(false);
  const users = useQuery({ ...usersQuery(), enabled: wantUsers });
  const defaultAssignees = new Map((defaults.data ?? []).map((row) => [row.project, row.assignee]));
  const logins = (users.data ?? []).map((user) => user.login);
  // The server takes the person creating the project when the call names nobody, so that is what
  // the form starts on; the reader may pick anyone else who has signed in.
  const viewer = useQuery(whoAmIQuery()).data?.login;
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
                    Default repository
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Created
                  </th>
                </tr>
              </thead>
              <tbody>
                {projects.data.length === 0 ? (
                  <tr>
                    <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={5}>
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
                      <td className="px-4 py-3">
                        {/* The default belongs to the deployment, whose writes Dispatch refuses a
                            restricted member: their row shows it and offers nothing to change. */}
                        {restricted ? (
                          <span className={`font-mono ${textSecondaryOnSurface}`}>
                            {candidate.default_repo ?? "None"}
                          </span>
                        ) : (
                          <DefaultRepositoryCell
                            project={candidate.key}
                            // A project with no default repository carries no `default_repo`.
                            stored={candidate.default_repo ?? null}
                          />
                        )}
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
            that person, and so does a question on an issue nobody holds. A project's default
            repository is optional. An issue of the project takes it when the issue and its parent
            issues name no repository.
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

          {restricted ? null : (
            <>
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
                    {/* The viewer is a person who has signed in, so they are in the list once it
                        is read; until it is, they are the only option. An empty value is nobody
                        picked yet, which is the page before the viewer resolves. */}
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
                    message={
                      stoppedNote(createProject, "the new project", "made") ??
                      apiErrorMessage(createProject.error, "Couldn't create the project.")
                    }
                    onRetry={() => projectSubmitGuard.retryLast(createProject)}
                    retrying={createProject.isPending}
                  />
                </div>
              ) : null}
            </>
          )}
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
        assignee={write.shown?.value ?? stored}
        logins={logins}
        onPick={write.submit}
        onReach={onReach}
        project={project}
        unread={unread}
      />
      {write.refusal === null ? null : (
        <div className="mt-2">
          <QueryError message={write.refusal} onRetry={write.retry} retrying={write.pending} />
        </div>
      )}
    </>
  );
}

/** One pick of a project setting: a person for the default assignee, and an `owner/name` or
 *  `null` (none) for the default repository. Each pick is its own object, so a save can tell
 *  whether it is still the newest pick by identity. */
type SettingPick<TValue> = { value: TValue };

/** What a pick's save resolves to when a newer pick was waiting behind it: nothing was sent. */
type SettingSuperseded = { superseded: true };

/** One save as the mutation cache holds it: the press the keyed write made of a pick
 *  (`KeyedPress`), the pick inside it. */
type SettingSave<TAnswer, TValue> = Mutation<
  TAnswer | SettingSuperseded | null,
  Error,
  KeyedPress<SettingPick<TValue>>
>;

/** What a row reads from one project's saves of one setting. */
interface ProjectSettingWrite<TValue> {
  /** Whether any save of this project is still out or waiting. */
  pending: boolean;
  /** What the row says of the project's newest save when it did not land - the server refused
   *  it, or the reader stopped it - until another save starts; `null` otherwise. */
  refusal: string | null;
  /** Makes the project's newest save again: the stopped press itself, or a new press of the
   *  refused value. A no-op unless that save did not land. */
  retry: () => void;
  /** The newest pick still saving or queued; `undefined` shows the stored value. */
  shown: SettingPick<TValue> | undefined;
  submit: (value: TValue) => void;
}

/** One project's saves of one setting, its default assignee or its default repository. The
 *  mutation scope runs a project's saves of the setting one at a time in pick order in the
 *  QueryClient's mutation cache, so a save still out or waiting when the row unmounts keeps its
 *  place, and a pick on a remounted row waits behind it. When a save's turn comes and a newer
 *  pick is waiting behind it, it is superseded and sends nothing: while a save is out, only the
 *  newest pick follows it, from whichever mount made it. The server ends at the reader's last
 *  pick.
 *
 *  The control shows the newest pick still pending, so a remounted row shows the pick in flight;
 *  once none is pending it shows the stored value. The row's error is read from the cache too:
 *  when the project's newest save was refused, the row says so with its reason, even on a row
 *  mounted after it failed, until a newer pick or Retry starts another save. Each answer lands in
 *  `read`, the read that holds the setting, so the control does not flash back to the old value,
 *  and `read` is refetched after each save that reached the server settles. */
function useProjectSettingWrite<TAnswer extends object, TValue, TRows>({
  label,
  project,
  read,
  save,
  setting,
  store,
  what,
}: {
  /** What the shell's pending-writes indicator names a pick. */
  label: (value: TValue) => string;
  project: string;
  read: QueryKey;
  save: (value: TValue, bounds: WriteBounds) => Promise<TAnswer>;
  /** The setting's name in the mutation key and scope, such as `default-assignee`. */
  setting: string;
  /** The rows of `read` with a save's answer in them. */
  store: (rows: TRows | undefined, saved: TAnswer) => TRows | undefined;
  /** The setting in the row's refusal, such as `the default assignee`. */
  what: string;
}): ProjectSettingWrite<TValue> {
  // Every project setting is saved with a `PUT`. The row reads its refusal from the mutation
  // cache, not from a `KeyedWriteResult`, so no write is in hand there to read the method from:
  // the sentence of a stopped save names this one.
  const method = "PUT";
  const queryClient = useQueryClient();
  const mutationKey = [`project-${setting}`, project];
  const mutation = useKeyedWrite<TAnswer | SettingSuperseded, SettingPick<TValue>>({
    mutationKey,
    scope: { id: `project-${setting}:${project}` },
    // A refused save stays in the cache, with no row observing it, until a newer save of the
    // project settles and removes it (see onSettled), so a row mounted later can still say so.
    gcTime: Number.POSITIVE_INFINITY,
    // The row shows its newest save's refusal itself, beside the control it was refused over.
    inline: true,
    label: (pick) => label(pick.value),
    method,
    write: async (pick, bounds): Promise<TAnswer | SettingSuperseded> => {
      // The cache lists mutations in the order they were made, so the last pending one is the
      // newest pick. Any other is waiting behind this save, and only it need reach the server.
      const waiting = queryClient
        .getMutationCache()
        .findAll({ exact: true, mutationKey, status: "pending" });
      const last = waiting.at(-1) as SettingSave<TAnswer, TValue> | undefined;
      if (last?.state.variables?.variables !== pick) {
        return { superseded: true };
      }
      return save(pick.value, bounds);
    },
    // A read already out was asked before this save and could land over its answer. One asked
    // after the press can be out still when the answer lands - the save may wait its turn behind
    // another in the scope - so the answer is put over the reads out then as well
    // (`replaceQueryData`).
    onMutate: () => queryClient.cancelQueries({ queryKey: read }),
    onSuccess: (saved) => {
      // Saved with no answer to give: the store reads every query again.
      if (saved === null || "superseded" in saved) {
        return;
      }
      replaceQueryData<TRows>(queryClient, read, (rows) => store(rows, saved));
    },
    onSettled: (saved) => {
      // With gcTime Infinity nothing else clears this project's saves: keep at most the newest
      // settled save and the pending ones.
      dropSettledWrites(queryClient, mutationKey);
      // A superseded save changed nothing on the server. Not awaited: the next pick in the scope
      // need not wait for this refetch.
      if (saved == null || !("superseded" in saved)) {
        void queryClient.invalidateQueries({ queryKey: read });
      }
    },
  });
  const saves = useMutationState<
    {
      error: Error | null;
      pick: SettingPick<TValue> | undefined;
      status: SettingSave<TAnswer, TValue>["state"]["status"];
    },
    SettingSave<TAnswer, TValue>
  >({
    filters: { exact: true, mutationKey },
    select: (save) => ({
      error: save.state.error,
      pick: save.state.variables?.variables,
      status: save.state.status,
    }),
  });
  const pendingPicks = saves.filter((save) => save.status === "pending");
  // The cache lists mutations in the order they were made, so the last is the newest.
  const newest = saves.at(-1);
  const refused = newest?.status === "error" ? newest : undefined;
  const error = refused?.error ?? null;
  return {
    pending: pendingPicks.length > 0,
    refusal:
      error === null
        ? null
        : error instanceof SendStoppedError
          ? stoppedWriteNote(what, "saved", { method, stopped: error })
          : apiErrorMessage(error, `Couldn't update ${what}.`),
    retry: () => {
      // The pick is read from the mutation cache rather than from `saves`, which
      // `useMutationState` gives: that hook shares its `select` result structurally, so it can
      // hand back an older save's pick where the two are deep-equal. While the Retry is on screen
      // the cache holds one settled save - each save's `onSettled` removes the project's settled
      // saves - so the newest save is that one.
      const newestSave = queryClient
        .getMutationCache()
        .findAll({ exact: true, mutationKey })
        .at(-1) as SettingSave<TAnswer, TValue> | undefined;
      const press = newestSave?.state.status === "error" ? newestSave.state.variables : undefined;
      if (press === undefined) {
        return;
      }
      // The Retry of the press the refusal is about, which is the very press the cache holds: one
      // this row made and the reader stopped is made again as that press (`again`). A project
      // setting is a `PUT`, which sends no key, so that is the same change sent again. A refusal
      // Dispatch decided, or a press of a row mounted before this one, is retried as a new pick.
      if (!mutation.again(press)) {
        mutation.mutate({ value: press.variables.value });
      }
    },
    shown: pendingPicks.at(-1)?.pick,
    submit: (value) => mutation.mutate({ value }),
  };
}

/** One project's default-assignee saves. A pick always names a person: no write clears a
 *  default. Each answer lands in the defaults read. */
function useDefaultAssigneeWrite(project: string): ProjectSettingWrite<string> {
  return useProjectSettingWrite<ProjectDefaultAssignee, string, ProjectDefaultAssignee[]>({
    label: (assignee) => `${project}'s default assignee ${assignee}`,
    project,
    read: projectDefaultAssigneesQuery().queryKey,
    save: (assignee, bounds) => api.putProjectDefaultAssignee(project, { assignee }, bounds),
    setting: "default-assignee",
    store: (rows, saved) => {
      if (rows === undefined) {
        return rows;
      }
      const others = rows.filter((row) => row.project !== saved.project);
      return [...others, saved];
    },
    what: "the default assignee",
  });
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

/** The check the default-repository cell makes before it sends. The server holds the name to
 *  GitHub's own grammar, which is stricter than `ownerRepoPattern`, and its refusal shows under
 *  the cell. */
const ownerRepo = wholeValue(ownerRepoPattern);

/** One project's default-repository cell. The text box holds the project's default repository,
 *  labelled `Default repository of <KEY>`, and an empty box reads **None**. Save sends what was
 *  typed: an `owner/name`, or `null` for an emptied box. A value that is not `owner/name` is
 *  refused at the box, with what was typed still in it, and nothing is sent. Clear, shown while
 *  the project has a default, sends `null`. Under the box is the error of this project's newest
 *  save when the server refused it. `stored` is the default the project list holds, `null` for
 *  none. */
function DefaultRepositoryCell({
  project,
  stored,
}: {
  project: string;
  stored: string | null;
}): ReactNode {
  const write = useDefaultRepositoryWrite(project);
  const repo = write.shown === undefined ? stored : write.shown.value;
  /** What the reader typed since the box last showed the project's default; null while it shows
   *  it. */
  const [draft, setDraft] = useState<string | null>(null);
  /** Whether the draft was refused here, for not being `owner/name`. */
  const [refused, setRefused] = useState(false);
  const submit = (value: string | null) => {
    setDraft(null);
    setRefused(false);
    if (value !== repo) {
      write.submit(value);
    }
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (draft === null) {
      return;
    }
    const typed = draft.trim();
    if (typed !== "" && !ownerRepo.test(typed)) {
      setRefused(true);
      return;
    }
    submit(typed === "" ? null : typed);
  };
  return (
    <>
      <form className="flex flex-wrap items-center gap-x-3 gap-y-2" onSubmit={save}>
        <input
          aria-invalid={refused || undefined}
          aria-label={`Default repository of ${project}`}
          autoComplete="off"
          className={`w-48 min-w-0 ${settingsMonoInput}`}
          onChange={(event) => {
            setDraft(event.target.value);
            setRefused(false);
          }}
          placeholder="None"
          spellCheck={false}
          value={draft ?? repo ?? ""}
        />
        <button
          aria-label={`Save the default repository of ${project}`}
          className={`font-medium ${linkText} ${linkHoverText}`}
          type="submit"
        >
          Save
        </button>
        {repo === null ? null : (
          <button
            aria-label={`Clear the default repository of ${project}`}
            className={`font-medium ${dangerText} ${dangerHoverText}`}
            onClick={() => submit(null)}
            type="button"
          >
            Clear
          </button>
        )}
      </form>
      {refused ? (
        <p className={`mt-2 text-sm ${dangerText}`} role="alert">
          {NOT_OWNER_NAME}
        </p>
      ) : null}
      {write.refusal === null ? null : (
        <div className="mt-2">
          <QueryError message={write.refusal} onRetry={write.retry} retrying={write.pending} />
        </div>
      )}
    </>
  );
}

/** One project's default-repository saves. A pick is an `owner/name`, or `null` to clear the
 *  default. Each answer lands in the project list, which holds the default as `default_repo`. */
function useDefaultRepositoryWrite(project: string): ProjectSettingWrite<string | null> {
  return useProjectSettingWrite<ProjectDefaultRepository, string | null, Project[]>({
    label: (repo) =>
      repo === null
        ? `${project}'s default repository cleared`
        : `${project}'s default repository ${repo}`,
    project,
    read: projectsQuery().queryKey,
    save: (repo, bounds) => api.putProjectDefaultRepository(project, { repo }, bounds),
    setting: "default-repository",
    store: (rows, saved) =>
      rows?.map((row) => (row.key === saved.project ? withDefaultRepo(row, saved.repo) : row)),
    what: "the default repository",
  });
}

/** `project` with `repo` as its default repository. A project with none carries no
 *  `default_repo`, as the project list answers it. */
function withDefaultRepo(project: Project, repo: string | null): Project {
  const { default_repo: _previous, ...rest } = project;
  return repo === null ? rest : { ...rest, default_repo: repo };
}
