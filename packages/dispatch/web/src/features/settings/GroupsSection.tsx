import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { accessGroupsQuery, whoAmIQuery } from "../../api/queries";
import type { AccessGroup } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
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
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

/** A group's name as the server takes it (`access.ValidGroupName`): lowercase letters, digits,
 *  dots, underscores and dashes, starting with a letter or digit, at most 63. The dash is escaped
 *  because browsers compile a `pattern` with the `v` flag, where an unescaped `-` at the end of a
 *  character class is a syntax error, and a `pattern` that does not compile checks nothing. */
const groupNamePattern = "[a-z0-9][a-z0-9._\\-]{0,62}";
const groupNameFormat =
  "Lowercase letters, digits, dots, underscores and dashes, starting with a letter or digit; at most 63.";

/** The members a field names: emails separated by commas, spaces or lines, each once, in the
 *  order written. The server checks each is an email and no restricted member. */
function membersOf(text: string): string[] {
  return [...new Set(text.split(/[\s,;]+/).filter((member) => member !== ""))];
}

/** The group list with `group` in place of the row of its name, or added, by name. */
function withGroup(
  groups: AccessGroup[] | undefined,
  group: AccessGroup
): AccessGroup[] | undefined {
  return groups === undefined
    ? groups
    : [...groups.filter((row) => row.name !== group.name), group].sort((a, b) =>
        a.name < b.name ? -1 : a.name > b.name ? 1 : 0
      );
}

const textAreaClass = `min-h-11 rounded-md px-3 py-2 text-sm disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(false)} ${textPrimaryOnSurface}`;

/**
 * Settings → Groups: every group a project's access list can name, each with its one owner (the
 * full member who made it or was given it) and its members, and a form that makes one. Only a
 * group's owner edits its members or deletes it, so those controls are disabled on any other
 * group, with the owner named as the reason in their title. A restricted member cannot make a
 * group, which the form says before anyone tries. A make is a PUT, which would write an existing
 * group of that name again, so the form refuses a name the list already holds and points at its
 * row.
 */
export function GroupsSection(): ReactNode {
  const queryClient = useQueryClient();
  const groups = useQuery(accessGroupsQuery());
  // The Access tab links here as `/settings#groups`, and a router navigation scrolls to no
  // fragment: the section brings itself into view once its list is drawn, and only then, since
  // the sections above it are still growing while their own reads answer.
  const sectionRef = useRef<HTMLElement>(null);
  const drawn = groups.isSuccess;
  useEffect(() => {
    if (drawn && window.location.hash === "#groups") {
      sectionRef.current?.scrollIntoView({ block: "start" });
    }
  }, [drawn]);
  const whoami = useQuery(whoAmIQuery()).data;
  const viewer = whoami?.login;
  // The form's two fields, kept together so a make that lands lets go of both at once.
  const [form, setForm] = useState({ members: "", name: "" });
  const [listed, setListed] = useState<string | null>(null);
  const createRefusal =
    whoami?.member_kind === "restricted" ? "A restricted member cannot make a group." : null;
  const groupsKey = accessGroupsQuery().queryKey;
  const createGroup = useKeyedWrite<AccessGroup, { name: string; members: string[] }>({
    // The form shows a refused make beside what it was refused over.
    inline: true,
    label: (input) => `New group ${input.name}`,
    write: (input, bounds) => api.putAccessGroup(input.name, { members: input.members }, bounds),
    method: "PUT",
    onSuccess: (group, input) => {
      if (group !== null) {
        queryClient.setQueryData<AccessGroup[]>(groupsKey, (rows) => withGroup(rows, group));
      }
      // A make lets go of the form it was made from, and of nothing typed since.
      setForm((draft) => (draft.name === input.name ? { members: "", name: "" } : draft));
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: groupsKey }),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const named = form.name.trim();
    if (createRefusal !== null || named === "") return;
    if ((groups.data ?? []).some((group) => group.name === named)) {
      setListed(named);
      return;
    }
    createGroup.mutate({ members: membersOf(form.members), name: named });
  };
  const edited = () => {
    setListed(null);
    createGroup.reset();
  };

  return (
    <section aria-labelledby="groups-heading" id="groups" ref={sectionRef}>
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="groups-heading">
        Groups
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        A group is a named list of full members that a project's Access tab can name. Its owner, the
        person who made it or was given it, is the only one who edits or deletes it.
      </p>

      {groups.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading groups…</p> : null}
      {groups.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load the groups."
            onRetry={() => void groups.refetch()}
            retrying={groups.isFetching}
          />
        </div>
      ) : null}
      {groups.isSuccess ? (
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={settingsTableHead}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Name
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Owner
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Members
                </th>
                <th className="px-4 py-3" scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {groups.data.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                    No groups yet.
                  </td>
                </tr>
              ) : (
                groups.data.map((group) => (
                  <GroupRow group={group} key={group.name} viewer={viewer} />
                ))
              )}
            </tbody>
          </table>
        </div>
      ) : null}

      {createRefusal === null ? null : (
        <p className={`mt-6 text-sm ${textSecondaryOnCanvas}`} id="groups-create-note" role="note">
          {createRefusal}
        </p>
      )}
      <form
        className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_2fr_auto] sm:items-end ${borderDefault}`}
        onSubmit={submit}
      >
        <label className={settingsFieldLabel} htmlFor="group-name">
          Group name
          <input
            autoComplete="off"
            className={`${settingsMonoInput} disabled:cursor-not-allowed disabled:opacity-50`}
            disabled={createRefusal !== null || createGroup.isPending}
            id="group-name"
            onChange={(event) => {
              edited();
              setForm({ ...form, name: event.target.value.toLowerCase() });
            }}
            pattern={groupNamePattern}
            placeholder="crew"
            required
            title={createRefusal ?? groupNameFormat}
            value={form.name}
          />
        </label>
        <label className={settingsFieldLabel} htmlFor="group-members">
          Members
          <textarea
            className={textAreaClass}
            disabled={createRefusal !== null || createGroup.isPending}
            id="group-members"
            onChange={(event) => {
              edited();
              setForm({ ...form, members: event.target.value });
            }}
            placeholder="ada@example.com, grace@example.com"
            rows={2}
            title={createRefusal ?? undefined}
            value={form.members}
          />
        </label>
        <RefusableButton
          busy={createGroup.isPending ? "Making…" : undefined}
          className="min-h-11 md:min-h-9"
          refusal={createRefusal}
          refusalShownBy={createRefusal === null ? undefined : "groups-create-note"}
          type="submit"
        >
          New group
        </RefusableButton>
      </form>
      <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
        Members are emails, separated by commas or lines. A restricted member cannot be in a group:
        name them on a project's access list directly.
      </p>
      {listed === null ? null : (
        <div className="mt-4">
          <QueryError
            message={`A group named ${listed} exists already. Its owner edits it in its row.`}
          />
        </div>
      )}
      {createGroup.isError && createGroup.variables !== undefined ? (
        <div className="mt-4">
          <QueryError
            message={
              // A make the reader stopped may have made the group, and Retry makes that very
              // press again (`retry`).
              stoppedNote(createGroup, `the new group ${createGroup.variables.name}`, "made") ??
              apiErrorMessage(
                createGroup.error,
                `Couldn't make group ${createGroup.variables.name}.`
              )
            }
            onRetry={createGroup.retry}
            retrying={createGroup.isPending}
          />
        </div>
      ) : null}
    </section>
  );
}

/** One write of a group its owner makes from its row: its members written again, or the group
 *  deleted. */
type GroupChange =
  | { readonly kind: "members"; readonly members: string[] }
  | { readonly kind: "delete" };

/** One group's row: its name, its owner, its members, and its owner's Edit and Delete. A group's
 *  writes are one mutation scope, so they go out one at a time in the order they were pressed. */
function GroupRow({
  group,
  viewer,
}: {
  group: AccessGroup;
  viewer: string | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const groupsKey = accessGroupsQuery().queryKey;
  const members = group.members ?? [];
  const notSignedIn = new Set(group.not_signed_in ?? []);
  const [draft, setDraft] = useState<string | null>(null);
  const refusal =
    viewer === undefined
      ? "Checking who you are…"
      : viewer === group.owner
        ? null
        : `Only ${group.owner}, the group's owner, changes it.`;
  const write = useKeyedWrite<AccessGroup | undefined, GroupChange>({
    scope: { id: `access-group:${group.name}` },
    // The row shows a refusal itself, beneath the group it was refused over.
    inline: true,
    label: (change) =>
      change.kind === "delete" ? `Delete group ${group.name}` : `Members of group ${group.name}`,
    write: async (change, bounds) => {
      if (change.kind === "delete") {
        await api.deleteAccessGroup(group.name, bounds);
        return undefined;
      }
      return api.putAccessGroup(group.name, { members: change.members }, bounds);
    },
    method: (change) => (change.kind === "delete" ? "DELETE" : "PUT"),
    onSuccess: (answer, change) => {
      if (change.kind === "delete") {
        queryClient.setQueryData<AccessGroup[]>(groupsKey, (rows) =>
          rows?.filter((row) => row.name !== group.name)
        );
        return;
      }
      if (answer !== null && answer !== undefined) {
        queryClient.setQueryData<AccessGroup[]>(groupsKey, (rows) => withGroup(rows, answer));
      }
      setDraft(null);
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: groupsKey }),
  });
  const remove = () => {
    if (
      window.confirm(
        `Delete group ${group.name}? Every project's access list that names it loses it.`
      )
    ) {
      write.mutate({ kind: "delete" });
    }
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (draft !== null) write.mutate({ kind: "members", members: membersOf(draft) });
  };
  const failed = write.isError && write.variables !== undefined ? write.variables : undefined;

  return (
    <>
      <tr className={settingsTableRow}>
        <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>{group.name}</td>
        <td className={`px-4 py-3 break-all ${textSecondaryOnSurface}`}>
          {`${group.owner}${group.owner === viewer ? " (you)" : ""}`}
        </td>
        <td className={`px-4 py-3 break-words ${textSecondaryOnSurface}`}>
          {members.length === 0 ? (
            <span className={textMutedOnSurface}>No members</span>
          ) : (
            members
              .map((member) => (notSignedIn.has(member) ? `${member} (not signed in yet)` : member))
              .join(", ")
          )}
        </td>
        <td className="px-4 py-3">
          <div className="flex flex-wrap justify-end gap-2">
            <RefusableButton
              look="secondary-compact"
              onPress={() => setDraft(draft === null ? members.join("\n") : null)}
              refusal={refusal}
            >
              {draft === null ? "Edit" : "Close"}
              <span className="sr-only"> {group.name}</span>
            </RefusableButton>
            <RefusableButton
              busy={write.isPending && write.variables?.kind === "delete" ? "Deleting…" : undefined}
              look="secondary-compact"
              onPress={remove}
              refusal={refusal}
            >
              Delete<span className="sr-only"> {group.name}</span>
            </RefusableButton>
          </div>
        </td>
      </tr>
      {draft === null ? null : (
        <tr className={settingsTableRow}>
          <td className="px-4 py-3" colSpan={4}>
            <form className="grid gap-2 sm:grid-cols-[1fr_auto] sm:items-end" onSubmit={save}>
              <label className={settingsFieldLabel} htmlFor={`group-members-${group.name}`}>
                Members of {group.name}
                <textarea
                  className={textAreaClass}
                  disabled={write.isPending}
                  id={`group-members-${group.name}`}
                  onChange={(event) => setDraft(event.target.value)}
                  rows={3}
                  value={draft}
                />
              </label>
              <div className="flex gap-2">
                <RefusableButton
                  busy={write.isPending ? "Saving…" : undefined}
                  className="min-h-11 md:min-h-9"
                  refusal={null}
                  type="submit"
                >
                  Save<span className="sr-only"> {group.name}</span>
                </RefusableButton>
                <RefusableButton
                  look="secondary-compact"
                  onPress={() => setDraft(null)}
                  refusal={null}
                >
                  Cancel
                </RefusableButton>
              </div>
            </form>
          </td>
        </tr>
      )}
      {failed === undefined ? null : (
        <tr className={settingsTableRow}>
          <td className="px-4 py-3" colSpan={4}>
            <QueryError
              message={
                // A change the reader stopped may have landed, and Retry makes that very press
                // again (`retry`).
                stoppedNote(
                  write,
                  failed.kind === "delete"
                    ? `the deletion of group ${group.name}`
                    : `the members of ${group.name}`,
                  failed.kind === "delete" ? "made" : "saved"
                ) ??
                apiErrorMessage(
                  write.error,
                  failed.kind === "delete"
                    ? `Couldn't delete group ${group.name}.`
                    : `Couldn't save the members of ${group.name}.`
                )
              }
              onRetry={write.retry}
              retrying={write.isPending}
            />
          </td>
        </tr>
      )}
    </>
  );
}
