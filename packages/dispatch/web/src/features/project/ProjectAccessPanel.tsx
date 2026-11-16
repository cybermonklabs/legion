import { type Mutation, useMutationState, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage, type WriteBounds } from "../../api/client";
import { accessGroupsQuery, projectAccessQuery, whoAmIQuery } from "../../api/queries";
import type { ProjectAccess } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import {
  borderDefault,
  card,
  inputClasses,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** One change of a project's access, as the owner presses it: the project given to another
 *  owner, one person or group named on its access list or taken off it, or the Slack channels
 *  its credentials may post to. */
type AccessChange =
  | { readonly kind: "owner"; readonly owner: string }
  | { readonly kind: "add-person" | "remove-person"; readonly email: string }
  | { readonly kind: "add-group" | "remove-group"; readonly name: string }
  | { readonly kind: "slack-channels"; readonly channels: readonly string[] };

type AccessSave = Mutation<ProjectAccess | null, Error, AccessChange>;

/** Which part of the panel a change belongs to, and so where its refusal is shown. */
type AccessPart = "owner" | "people" | "groups" | "slack";

function partOf(change: AccessChange): AccessPart {
  switch (change.kind) {
    case "owner":
      return "owner";
    case "add-person":
    case "remove-person":
      return "people";
    case "add-group":
    case "remove-group":
      return "groups";
    case "slack-channels":
      return "slack";
  }
}

function writeChange(
  project: string,
  change: AccessChange,
  bounds: WriteBounds
): Promise<ProjectAccess> {
  switch (change.kind) {
    case "owner":
      return api.patchProjectAccess(project, { owner: change.owner }, bounds);
    case "add-person":
      return api.putProjectAccessPerson(project, change.email, bounds);
    case "remove-person":
      return api.deleteProjectAccessPerson(project, change.email, bounds);
    case "add-group":
      return api.putProjectAccessGroup(project, change.name, bounds);
    case "remove-group":
      return api.deleteProjectAccessGroup(project, change.name, bounds);
    case "slack-channels":
      return api.patchProjectAccess(project, { slack_channels: [...change.channels] }, bounds);
  }
}

/** What the shell's pending-writes indicator names a change, in the reader's words. */
function changeLabel(project: string, change: AccessChange): string {
  switch (change.kind) {
    case "owner":
      return `Give ${project} to ${change.owner}`;
    case "add-person":
      return `Add ${change.email} to ${project}`;
    case "remove-person":
      return `Remove ${change.email} from ${project}`;
    case "add-group":
      return `Add group ${change.name} to ${project}`;
    case "remove-group":
      return `Remove group ${change.name} from ${project}`;
    case "slack-channels":
      return `Name ${project}'s Slack channels`;
  }
}

/** What a refusal says when Dispatch gave no reason of its own (the request never reached it). */
function refusalFallback(project: string, change: AccessChange): string {
  switch (change.kind) {
    case "owner":
      return `Couldn't give ${project} to ${change.owner}.`;
    case "add-person":
      return `Couldn't add ${change.email}.`;
    case "remove-person":
      return `Couldn't remove ${change.email}.`;
    case "add-group":
      return `Couldn't add group ${change.name}.`;
    case "remove-group":
      return `Couldn't remove group ${change.name}.`;
    case "slack-channels":
      return `Couldn't name ${project}'s Slack channels.`;
  }
}

/**
 * One project's access saves, as `ProjectsSection`'s default-assignee saves are made: one mutation
 * scope per project, so the project's saves go out one at a time in the order they were pressed,
 * from whichever mount pressed them, and a save still out when the tab unmounts keeps its place.
 * Each answer is the project's access as that write left it, which replaces the panel's read; the
 * read is asked again once each save settles, since another person's write may have landed
 * between. The newest save's refusal is read from the mutation cache, so the panel says it beside
 * its control, even on a mount made after it failed, until a newer save starts; `onLanded` hears of
 * each change that landed, so the panel can let go of the draft it was made from.
 */
function useAccessWrite(
  project: string,
  onLanded: (change: AccessChange) => void
): {
  /** Every change of this project still out or waiting its turn, oldest first. */
  pending: readonly AccessChange[];
  /** The project's newest save when Dispatch refused it, until another save starts. */
  refused: { change: AccessChange; error: Error } | undefined;
  save: (change: AccessChange) => void;
} {
  const queryClient = useQueryClient();
  const accessKey = projectAccessQuery(project).queryKey;
  const mutationKey = ["project-access", project];
  const mutation = useKeyedWrite<ProjectAccess, AccessChange>({
    mutationKey,
    scope: { id: `project-access:${project}` },
    // A refused save stays in the cache with no panel observing it, until a newer save of the
    // project settles and removes it (onSettled), so a panel mounted later can still say so.
    gcTime: Number.POSITIVE_INFINITY,
    // The panel shows a refusal itself, beside the control it was refused over.
    inline: true,
    label: (change) => changeLabel(project, change),
    write: (change, bounds) => writeChange(project, change, bounds),
    // A read already out was asked before this save and could land over its answer.
    onMutate: () => queryClient.cancelQueries({ queryKey: accessKey }),
    onSuccess: (answer, change) => {
      // Landed with no answer to give: the read asked again in onSettled says what it left.
      if (answer !== null) {
        queryClient.setQueryData(accessKey, answer);
      }
      onLanded(change);
    },
    onSettled: () => {
      // A save settles in press order and is still pending while this runs, so every settled
      // save left is older than it and no longer the one the panel reports.
      const cache = queryClient.getMutationCache();
      for (const older of cache.findAll({ exact: true, mutationKey })) {
        if (older.state.status !== "pending") {
          cache.remove(older);
        }
      }
      void queryClient.invalidateQueries({ queryKey: accessKey });
    },
  });
  const saves = useMutationState<
    {
      change: AccessChange | undefined;
      error: Error | null;
      status: AccessSave["state"]["status"];
    },
    AccessSave
  >({
    filters: { exact: true, mutationKey },
    select: (save) => ({
      change: save.state.variables,
      error: save.state.error,
      status: save.state.status,
    }),
  });
  // The cache lists mutations in the order they were made, so the last is the newest.
  const newest = saves.at(-1);
  return {
    pending: saves.flatMap((save) =>
      save.status === "pending" && save.change !== undefined ? [save.change] : []
    ),
    refused:
      newest?.status === "error" && newest.change !== undefined && newest.error !== null
        ? { change: newest.change, error: newest.error }
        : undefined,
    save: (change) => mutation.mutate(change),
  };
}

const fieldClass = `min-h-11 w-full min-w-0 rounded-md px-3 py-2 text-sm disabled:cursor-not-allowed disabled:opacity-50 md:min-h-9 ${inputClasses(false)} ${textPrimaryOnSurface}`;
const fieldLabelClass = `grid min-w-0 flex-1 basis-56 gap-1 text-sm font-medium ${textSecondaryOnCanvas}`;
const headingClass = `text-base font-semibold ${textPrimaryOnCanvas}`;
const listClass = `mt-3 rounded-xl border ${card}`;
const rowClass = `flex min-h-11 items-center justify-between gap-3 border-b px-4 py-2 last:border-0 ${borderDefault}`;

/**
 * A project's Access tab: its one owner, its visibility, and the people and groups its access
 * list names, with the owner's controls to name and take off each and to give the project to
 * another person. Only the owner changes any of it, so every control is disabled for anyone else,
 * with the reason as its title and on the page above them; whether the reader is the owner is
 * `/auth/whoami`'s `login` against the answer's `owner`, read from the identity the shell already
 * holds. Visibility is shown and not offered: a project cannot be made private in this release.
 */
export function ProjectAccessPanel({ project }: { project: string }): ReactNode {
  const ids = useId();
  const noteId = `${ids}-note`;
  const [ownerDraft, setOwnerDraft] = useState("");
  const [personDraft, setPersonDraft] = useState("");
  const [groupDraft, setGroupDraft] = useState("");
  const [channelsDraft, setChannelsDraft] = useState<string | null>(null);
  const access = useQuery(projectAccessQuery(project));
  const groups = useQuery(accessGroupsQuery());
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // A landed change lets go of the draft it was made from, and of no newer one.
  const write = useAccessWrite(project, (change) => {
    if (change.kind === "owner") {
      setOwnerDraft((draft) => (draft.trim() === change.owner ? "" : draft));
    } else if (change.kind === "add-person") {
      setPersonDraft((draft) => (draft.trim() === change.email ? "" : draft));
    } else if (change.kind === "add-group") {
      setGroupDraft((draft) => (draft === change.name ? "" : draft));
    } else if (change.kind === "slack-channels") {
      // The field follows the stored list again once this write's own channels landed; a draft
      // typed since stays.
      setChannelsDraft((draft) =>
        draft !== null &&
        draft
          .split(/[\s,]+/)
          .filter((id) => id !== "")
          .join(" ") === change.channels.join(" ")
          ? null
          : draft
      );
    }
  });

  if (access.isPending) {
    return <p className={`mt-4 ${textMutedOnCanvas}`}>Loading who may see this project…</p>;
  }
  if (access.isError) {
    return (
      <div className="mt-4">
        <QueryError
          error={access.error}
          message={apiErrorMessage(access.error, "Couldn't load who may see this project.")}
          onRetry={() => void access.refetch()}
          retrying={access.isFetching}
        />
      </div>
    );
  }

  const { owner } = access.data;
  const people = access.data.people ?? [];
  const listedGroups = access.data.groups ?? [];
  const channelsText = (access.data.slack_channels ?? []).join(" ");
  const refusal =
    viewer === undefined
      ? "Checking who you are…"
      : viewer === owner
        ? null
        : `Only ${owner}, the project's owner, changes who may see it.`;
  const available = (groups.data ?? [])
    .map((group) => group.name)
    .filter((name) => !listedGroups.includes(name));
  const groupRefusal =
    refusal ??
    (groups.isPending
      ? "Loading the groups…"
      : groups.isError
        ? "Couldn't load the groups."
        : groups.data.length === 0
          ? "No group exists yet. Make one in Settings → Groups."
          : available.length === 0
            ? "Every group is on this list already."
            : null);
  const pendingOf = (match: (change: AccessChange) => boolean) => write.pending.some(match);
  const refusalIn = (part: AccessPart): ReactNode => {
    const refused = write.refused;
    if (refused === undefined || partOf(refused.change) !== part) {
      return null;
    }
    return (
      <div className="mt-3">
        <QueryError
          message={apiErrorMessage(refused.error, refusalFallback(project, refused.change))}
          onRetry={() => write.save(refused.change)}
          retrying={pendingOf((change) => partOf(change) === part)}
        />
      </div>
    );
  };

  const give = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const next = ownerDraft.trim();
    if (refusal !== null || next === "") return;
    if (
      !window.confirm(
        `Give ${project} to ${next}? Only its new owner can change who may see it after this.`
      )
    ) {
      return;
    }
    write.save({ kind: "owner", owner: next });
  };
  const addPerson = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const email = personDraft.trim();
    if (refusal !== null || email === "") return;
    write.save({ email, kind: "add-person" });
  };
  const addGroup = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (groupRefusal !== null || groupDraft === "") return;
    write.save({ kind: "add-group", name: groupDraft });
  };
  const nameChannels = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (refusal !== null) return;
    const channels = (channelsDraft ?? channelsText).split(/[\s,]+/).filter((id) => id !== "");
    write.save({ channels, kind: "slack-channels" });
  };

  return (
    <div className="mt-4 max-w-3xl space-y-8">
      {refusal === null ? null : (
        <p className={`text-sm ${textSecondaryOnCanvas}`} id={noteId} role="note">
          {refusal}
        </p>
      )}

      <section aria-labelledby={`${ids}-owner`}>
        <h2 className={headingClass} id={`${ids}-owner`}>
          Owner
        </h2>
        <p className={`mt-1 break-all text-sm ${textPrimaryOnCanvas}`} data-testid="access-owner">
          {owner}
          {viewer === owner ? <span className={textMutedOnCanvas}> (you)</span> : null}
        </p>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          The owner names who may see the project and can give it to another full member.
        </p>
        <form className="mt-3 flex flex-wrap items-end gap-2" onSubmit={give}>
          <label className={fieldLabelClass} htmlFor={`${ids}-new-owner`}>
            Give this project to
            <input
              aria-describedby={refusal === null ? undefined : noteId}
              autoComplete="off"
              className={fieldClass}
              disabled={refusal !== null}
              id={`${ids}-new-owner`}
              onChange={(event) => setOwnerDraft(event.target.value)}
              placeholder="email of a full member"
              required
              title={refusal ?? undefined}
              value={ownerDraft}
            />
          </label>
          <RefusableButton
            busy={pendingOf((change) => change.kind === "owner") ? "Giving…" : undefined}
            className="min-h-11 md:min-h-9"
            refusal={refusal}
            refusalShownBy={refusal === null ? undefined : noteId}
            type="submit"
          >
            Give project
          </RefusableButton>
        </form>
        {refusalIn("owner")}
      </section>

      <section aria-labelledby={`${ids}-visibility`}>
        <h2 className={headingClass} id={`${ids}-visibility`}>
          Visibility
        </h2>
        <p
          className={`mt-1 text-sm font-medium ${textPrimaryOnCanvas}`}
          data-testid="access-visibility"
        >
          {access.data.visibility === "private" ? "Private" : "Public"}
        </p>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          Every full member can see a public project. A restricted member, who signs in from outside
          the company, sees it only when the people below name them directly.
        </p>
      </section>

      <section aria-labelledby={`${ids}-slack`}>
        <h2 className={headingClass} id={`${ids}-slack`}>
          Slack channels
        </h2>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          The channels this project's agents may post to, by channel id (the `C…` or `G…` Slack
          shows under About → Channel ID), at most 16, separated by spaces or commas. With none
          named, the secrets broker releases the Slack bot token for no channel of this project.
        </p>
        <form className="mt-3 flex flex-wrap items-end gap-2" onSubmit={nameChannels}>
          <label className={fieldLabelClass} htmlFor={`${ids}-slack-channels`}>
            Channel ids
            <input
              aria-describedby={refusal === null ? undefined : noteId}
              autoComplete="off"
              className={fieldClass}
              data-testid="access-slack-channels"
              disabled={refusal !== null}
              id={`${ids}-slack-channels`}
              onChange={(event) => setChannelsDraft(event.target.value)}
              placeholder="C0123456789 C0987654321"
              title={refusal ?? undefined}
              value={channelsDraft ?? channelsText}
            />
          </label>
          <RefusableButton
            busy={pendingOf((change) => change.kind === "slack-channels") ? "Saving…" : undefined}
            className="min-h-11 md:min-h-9"
            refusal={refusal}
            refusalShownBy={refusal === null ? undefined : noteId}
            type="submit"
          >
            Save channels
          </RefusableButton>
        </form>
        {refusalIn("slack")}
      </section>

      <section aria-labelledby={`${ids}-people`}>
        <h2 className={headingClass} id={`${ids}-people`}>
          People
        </h2>
        {people.length === 0 ? (
          <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>No one is named on this list.</p>
        ) : (
          <ul aria-label="People" className={listClass}>
            {people.map((email) => (
              <li className={rowClass} key={email}>
                <span className={`min-w-0 break-all text-sm ${textPrimaryOnSurface}`}>{email}</span>
                <RefusableButton
                  busy={
                    pendingOf((change) => change.kind === "remove-person" && change.email === email)
                      ? "Removing…"
                      : undefined
                  }
                  look="secondary-compact"
                  onPress={() => write.save({ email, kind: "remove-person" })}
                  refusal={refusal}
                  refusalShownBy={refusal === null ? undefined : noteId}
                >
                  Remove<span className="sr-only"> {email}</span>
                </RefusableButton>
              </li>
            ))}
          </ul>
        )}
        <form className="mt-3 flex flex-wrap items-end gap-2" onSubmit={addPerson}>
          <label className={fieldLabelClass} htmlFor={`${ids}-person`}>
            Add a person
            <input
              aria-describedby={refusal === null ? undefined : noteId}
              autoComplete="off"
              className={fieldClass}
              disabled={refusal !== null}
              id={`${ids}-person`}
              onChange={(event) => setPersonDraft(event.target.value)}
              placeholder="name@example.com"
              required
              title={refusal ?? undefined}
              value={personDraft}
            />
          </label>
          <RefusableButton
            busy={pendingOf((change) => change.kind === "add-person") ? "Adding…" : undefined}
            className="min-h-11 md:min-h-9"
            refusal={refusal}
            refusalShownBy={refusal === null ? undefined : noteId}
            type="submit"
          >
            Add person
          </RefusableButton>
        </form>
        {refusalIn("people")}
      </section>

      <section aria-labelledby={`${ids}-groups`}>
        <h2 className={headingClass} id={`${ids}-groups`}>
          Groups
        </h2>
        {listedGroups.length === 0 ? (
          <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>No group is on this list.</p>
        ) : (
          <ul aria-label="Groups" className={listClass}>
            {listedGroups.map((name) => (
              <li className={rowClass} key={name}>
                <span className={`min-w-0 break-all font-mono text-sm ${textPrimaryOnSurface}`}>
                  {name}
                </span>
                <RefusableButton
                  busy={
                    pendingOf((change) => change.kind === "remove-group" && change.name === name)
                      ? "Removing…"
                      : undefined
                  }
                  look="secondary-compact"
                  onPress={() => write.save({ kind: "remove-group", name })}
                  refusal={refusal}
                  refusalShownBy={refusal === null ? undefined : noteId}
                >
                  Remove<span className="sr-only"> {name}</span>
                </RefusableButton>
              </li>
            ))}
          </ul>
        )}
        <form className="mt-3 flex flex-wrap items-end gap-2" onSubmit={addGroup}>
          <label className={fieldLabelClass} htmlFor={`${ids}-group`}>
            Add a group
            <select
              aria-describedby={refusal === null ? undefined : noteId}
              className={fieldClass}
              disabled={groupRefusal !== null}
              id={`${ids}-group`}
              onChange={(event) => setGroupDraft(event.target.value)}
              required
              title={groupRefusal ?? undefined}
              value={groupDraft}
            >
              <option value="">Choose a group</option>
              {available.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </label>
          <RefusableButton
            busy={pendingOf((change) => change.kind === "add-group") ? "Adding…" : undefined}
            className="min-h-11 md:min-h-9"
            refusal={groupRefusal}
            refusalShownBy={refusal === null ? undefined : noteId}
            type="submit"
          >
            Add group
          </RefusableButton>
        </form>
        <p className={`mt-2 text-sm ${textMutedOnSurface}`}>
          A group admits only its full members. Groups are made and edited in{" "}
          <Link className={`font-medium ${linkText} ${linkHoverText}`} to="/settings#groups">
            Settings → Groups
          </Link>
          .
        </p>
        {groups.isError ? (
          <div className="mt-3">
            <QueryError
              message="Couldn't load the groups."
              onRetry={() => void groups.refetch()}
              retrying={groups.isFetching}
            />
          </div>
        ) : null}
        {refusalIn("groups")}
      </section>
    </div>
  );
}
