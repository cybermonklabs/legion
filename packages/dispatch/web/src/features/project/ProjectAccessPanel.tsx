import {
  type Mutation,
  type UseQueryResult,
  useMutationState,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import {
  ApiError,
  api,
  apiErrorMessage,
  type WriteBounds,
  type WriteMethod,
} from "../../api/client";
import { accessGroupsQuery, projectAccessQuery, whoAmIQuery } from "../../api/queries";
import type { AccessGroup, ProjectAccess } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { Switch } from "../../components/Switch";
import { useRestrictedMember } from "../../hooks/useRestrictedMember";
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
import { Timestamp } from "../refs/Timestamp";
import { retriesUnderItsKey, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { dropSettledWrites } from "../writes/settled-writes";
import { type KeyedPress, useKeyedWrite } from "../writes/useKeyedWrite";

/** One change of a project's access, as the owner presses it: the project given to another
 *  owner, made public or private, one person or group named on its access list or taken off it,
 *  or the Slack channels its credentials may post to. */
type AccessChange =
  | { readonly kind: "owner"; readonly owner: string }
  | { readonly kind: "visibility"; readonly visibility: ProjectAccess["visibility"] }
  | { readonly kind: "add-person" | "remove-person"; readonly email: string }
  | { readonly kind: "add-group" | "remove-group"; readonly name: string }
  | { readonly kind: "slack-channels"; readonly channels: readonly string[] };

/** One save as the mutation cache holds it: the press the keyed write made of a change
 *  (`KeyedPress`), the change inside it. */
type AccessSave = Mutation<ProjectAccess | null, Error, KeyedPress<AccessChange>>;

/** Which part of the panel a change belongs to, and so where its refusal is shown. */
type AccessPart = "owner" | "visibility" | "people" | "groups" | "slack";

/** What one change of a project's access is: the request that makes it and that request's
 *  method, what the shell's pending-writes indicator names it, the part of the panel its refusal
 *  shows in, what that refusal says when Dispatch gave no reason of its own (the request never
 *  reached it), and, for a save the reader stopped, what Dispatch has not confirmed and what that
 *  save may have been (`stoppedWriteNote`'s `what` and `made`). */
interface AccessOperation {
  readonly write: (bounds: WriteBounds) => Promise<ProjectAccess>;
  readonly method: WriteMethod;
  readonly label: string;
  readonly part: AccessPart;
  readonly refusal: string;
  readonly unconfirmed: string;
  readonly made: string;
}

/** The operation of one change. Every kind returns all seven, so a kind added to `AccessChange`
 *  does not compile until it says each of them. */
function accessOperation(project: string, change: AccessChange): AccessOperation {
  switch (change.kind) {
    case "owner":
      return {
        write: (bounds) => api.patchProjectAccess(project, { owner: change.owner }, bounds),
        method: "PATCH",
        label: `Give ${project} to ${change.owner}`,
        part: "owner",
        refusal: `Couldn't give ${project} to ${change.owner}.`,
        unconfirmed: `${project}'s new owner, ${change.owner}`,
        made: "saved",
      };
    case "visibility":
      return {
        write: (bounds) =>
          api.patchProjectAccess(project, { visibility: change.visibility }, bounds),
        method: "PATCH",
        label: `Make ${project} ${change.visibility}`,
        part: "visibility",
        refusal: `Couldn't make ${project} ${change.visibility}.`,
        unconfirmed: `${project}'s visibility, ${change.visibility}`,
        made: "saved",
      };
    case "add-person":
      return {
        write: (bounds) => api.putProjectAccessPerson(project, change.email, bounds),
        method: "PUT",
        label: `Add ${change.email} to ${project}`,
        part: "people",
        refusal: `Couldn't add ${change.email}.`,
        unconfirmed: `the addition of ${change.email}`,
        made: "made",
      };
    case "remove-person":
      return {
        write: (bounds) => api.deleteProjectAccessPerson(project, change.email, bounds),
        method: "DELETE",
        label: `Remove ${change.email} from ${project}`,
        part: "people",
        refusal: `Couldn't remove ${change.email}.`,
        unconfirmed: `the removal of ${change.email}`,
        made: "made",
      };
    case "add-group":
      return {
        write: (bounds) => api.putProjectAccessGroup(project, change.name, bounds),
        method: "PUT",
        label: `Add group ${change.name} to ${project}`,
        part: "groups",
        refusal: `Couldn't add group ${change.name}.`,
        unconfirmed: `the addition of group ${change.name}`,
        made: "made",
      };
    case "remove-group":
      return {
        write: (bounds) => api.deleteProjectAccessGroup(project, change.name, bounds),
        method: "DELETE",
        label: `Remove group ${change.name} from ${project}`,
        part: "groups",
        refusal: `Couldn't remove group ${change.name}.`,
        unconfirmed: `the removal of group ${change.name}`,
        made: "made",
      };
    case "slack-channels":
      return {
        write: (bounds) =>
          api.patchProjectAccess(project, { slack_channels: [...change.channels] }, bounds),
        method: "PATCH",
        label: `Name ${project}'s Slack channels`,
        part: "slack",
        refusal: `Couldn't name ${project}'s Slack channels.`,
        unconfirmed: `${project}'s Slack channels`,
        made: "saved",
      };
  }
}

/** What the owner who pressed the Private switch reads when Dispatch refuses it, by the refusal's
 *  code. Dispatch's own words for these refusals are written for the deployment's operator (a
 *  table, an envoy-dispatch command, a setting, the hub's bucket) and stay in the server's log and
 *  record, so none of them is drawn here: each code the route answers (`api/project_access.go`)
 *  has a sentence that says what the owner can do, and a code this dashboard does not know has one
 *  that names nothing of Dispatch's. None offers Retry: each answer stays the same until someone
 *  changes the deployment, the store or the project, and the switch itself can still be pressed.
 *  PRIVATE_PROJECTS_UNAVAILABLE is one of them: this release of Dispatch has no writer of the
 *  hub's private-projects record, so it refuses with that code every switch to private that passes
 *  the other checks (`cmd/dispatch/README.md`). */
function switchRefusal(
  project: string,
  visibility: ProjectAccess["visibility"],
  code: string | undefined
): string {
  switch (code) {
    case "PRIVATE_PROJECTS_OFF":
      return "This deployment cannot make a project private yet: ask its operator to set up private projects.";
    case "PRIVATE_PROJECT_NOT_PROVISIONED":
      return `This deployment has not set up ${project} to be private: ask its operator to set it up.`;
    case "PRIVATE_PROJECTS_INCOMPLETE":
      return `This deployment is not ready to keep ${project} private yet: ask its operator to finish setting up private projects.`;
    case "PRIVATE_PROJECTS_UNAVAILABLE":
      return `This deployment cannot keep ${project}'s messages from other people yet, so ${project} stays public: ask its operator to set up private projects.`;
    case "PROJECT_OWNER_REQUIRED":
      return `Only ${project}'s owner can change who may see it.`;
    case "PROJECT_NOT_FOUND":
      return `You can no longer see ${project}, so you cannot change who may see it.`;
    case "PROJECT_ACCESS_INPUT":
      return "Dispatch did not take this change: reload the page and press the switch again.";
    default:
      return `Dispatch did not make ${project} ${visibility}: if this keeps happening, ask the deployment's operator.`;
  }
}

/** Why no group can be named on the list yet, or null when one can: the groups are still
 *  loading or could not be read, none exists, or the list names every one already. */
function groupsRefusal(
  groups: UseQueryResult<readonly AccessGroup[]>,
  available: readonly string[]
): string | null {
  if (groups.isPending) return "Loading the groups…";
  if (groups.isError) return "Couldn't load the groups.";
  if (groups.data.length === 0) return "No group exists yet. Make one in Settings → Groups.";
  if (available.length === 0) return "Every group is on this list already.";
  return null;
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
  /** The project's newest save when Dispatch refused it, until another save starts, and whether
   *  this mount holds that press, which the reader stopped, to make again under its key
   *  (`madeHere`): a panel opened after the save holds none. */
  refused: { change: AccessChange; error: Error; madeHere: boolean } | undefined;
  /** The Retry of the newest save's refusal: the very press where this mount made it and the
   *  reader stopped it (`again`), else a new save of the same change. */
  retry: () => void;
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
    label: (change) => accessOperation(project, change).label,
    write: (change, bounds) => accessOperation(project, change).write(bounds),
    method: (change) => accessOperation(project, change).method,
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
      dropSettledWrites(queryClient, mutationKey);
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
      change: save.state.variables?.variables,
      error: save.state.error,
      status: save.state.status,
    }),
  });
  // The cache lists mutations in the order they were made, so the last is the newest.
  const newest = saves.at(-1);
  // The press is read from the mutation cache rather than from `saves`, whose `select` result
  // `useMutationState` shares structurally, as `useDefaultAssigneeWrite` reads it; the hook finds
  // a press it holds by that very object.
  const newestSave = queryClient.getMutationCache().findAll({ exact: true, mutationKey }).at(-1) as
    | AccessSave
    | undefined;
  const press = newestSave?.state.status === "error" ? newestSave.state.variables : undefined;
  const madeHere =
    press !== undefined &&
    mutation.stopped((variables) => variables === press.variables) !== undefined;
  return {
    pending: saves.flatMap((save) =>
      save.status === "pending" && save.change !== undefined ? [save.change] : []
    ),
    refused:
      newest?.status === "error" && newest.change !== undefined && newest.error !== null
        ? { change: newest.change, error: newest.error, madeHere }
        : undefined,
    retry: () => {
      if (press === undefined) {
        return;
      }
      // A press this mount made and the reader stopped is made again as that press, under its key
      // where its method sends one; a refusal Dispatch decided, or a press of a mount made before
      // this one, is retried as a new save.
      if (!mutation.again(press)) {
        mutation.mutate(press.variables);
      }
    },
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
 * list names, with the owner's controls to name and take off each, to make the project public or
 * private, and to give it to another person. Only the owner changes any of it, so every control
 * is disabled for anyone else, with the reason as its title and on the page above them; whether
 * the reader is the owner is `/auth/whoami`'s `login` against the answer's `owner`, read from the
 * identity the shell already holds. Dispatch refuses the switch with a code (PRIVATE_PROJECTS_OFF,
 * PRIVATE_PROJECT_NOT_PROVISIONED, PRIVATE_PROJECTS_INCOMPLETE, PRIVATE_PROJECTS_UNAVAILABLE and
 * others), and the switch shows beside it the owner's sentence for that code (`switchRefusal`),
 * never Dispatch's own words, which are the operator's. A switch to public, which shows the
 * project and everything in it to every full member, is sent only once the owner confirms it, as
 * giving the project away is; a switch to private asks nothing, since it shows nobody more.
 */
export function ProjectAccessPanel({ project }: { project: string }): ReactNode {
  const ids = useId();
  const noteId = `${ids}-note`;
  const [ownerDraft, setOwnerDraft] = useState("");
  const [personDraft, setPersonDraft] = useState("");
  const [groupDraft, setGroupDraft] = useState("");
  const [channelsDraft, setChannelsDraft] = useState<string | null>(null);
  const access = useQuery(projectAccessQuery(project));
  // The groups are only the owner's to add, and the list is the deployment's, which Dispatch
  // refuses a restricted member (`useRestrictedMember`); an owner is a full member, so such a
  // reader is never offered a group and never asked for the list.
  const restricted = useRestrictedMember();
  const groups = useQuery({ ...accessGroupsQuery(), enabled: !restricted });
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
  const notSignedIn = new Set(access.data.not_signed_in ?? []);
  const listedGroups = access.data.groups ?? [];
  const isPrivate = access.data.visibility === "private";
  const channelsText = (access.data.slack_channels ?? []).join(" ");
  const channelsChangedAt = access.data.slack_channels_changed_at ?? null;
  const refusal =
    viewer === undefined
      ? "Checking who you are…"
      : viewer === owner
        ? null
        : `Only ${owner}, the project's owner, changes who may see it.`;
  const available = (groups.data ?? [])
    .map((group) => group.name)
    .filter((name) => !listedGroups.includes(name));
  const groupRefusal = refusal ?? groupsRefusal(groups, available);
  const pendingOf = (match: (change: AccessChange) => boolean) => write.pending.some(match);
  const visibilityPending = pendingOf((change) => change.kind === "visibility");
  const refusalIn = (part: AccessPart): ReactNode => {
    const refused = write.refused;
    if (refused === undefined) {
      return null;
    }
    const operation = accessOperation(project, refused.change);
    if (operation.part !== part) {
      return null;
    }
    // Dispatch's answer to the switch is said in the owner's words, with no Retry; a request
    // Dispatch did not answer, a save the reader stopped, and every other part's refusal keep the
    // operation's line and its Retry.
    const shown =
      refused.change.kind === "visibility" && refused.error instanceof ApiError
        ? {
            message: switchRefusal(project, refused.change.visibility, refused.error.code),
            retry: false,
          }
        : {
            message:
              refused.error instanceof SendStoppedError
                ? // The refusal is read from the mutation cache, not from a `KeyedWriteResult`,
                  // so the method is the change's own (`accessOperation`). A panel opened after
                  // the save holds no press to make again, so its Retry is a new save under a new
                  // key. A save whose key ran out leaves no press to make again in any panel, the
                  // one that made it included, and its note says what that Retry is
                  // (`expiredRetryNote`).
                  stoppedWriteNote(
                    operation.unconfirmed,
                    operation.made,
                    refused.madeHere || !retriesUnderItsKey(refused.error)
                      ? { method: operation.method, stopped: refused.error }
                      : {
                          anew: "This panel was opened after that save, so Retry sends it as a new save.",
                        }
                  )
                : apiErrorMessage(refused.error, operation.refusal),
            retry: true,
          };
    return (
      <div className="mt-3">
        <QueryError
          message={shown.message}
          onRetry={shown.retry ? write.retry : undefined}
          retrying={pendingOf((change) => accessOperation(project, change).part === part)}
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
        <div className="mt-1 flex items-center gap-3">
          {/* The switch follows Dispatch's answer, not the press: it reads the stored visibility,
              and a refusal from Dispatch leaves it where it was. While a change of it is out, or
              while the reader is refused it, it cannot be pressed, and says so with
              `aria-disabled`, never `disabled`: a browser takes the focus off a control that
              becomes disabled under it, which would drop a keyboard reader who pressed the switch
              to the page's body. */}
          <Switch
            aria-describedby={refusal === null ? undefined : noteId}
            aria-disabled={refusal === null && !visibilityPending ? undefined : true}
            aria-label="Private"
            checked={isPrivate}
            className="aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
            onClick={() => {
              if (refusal !== null || visibilityPending) return;
              if (
                isPrivate &&
                !window.confirm(
                  `Make ${project} public? Every full member can then see it and everything in it.`
                )
              ) {
                return;
              }
              write.save({ kind: "visibility", visibility: isPrivate ? "public" : "private" });
            }}
            title={refusal ?? undefined}
          />
          <p
            className={`text-sm font-medium ${textPrimaryOnCanvas}`}
            data-testid="access-visibility"
          >
            {isPrivate ? "Private" : "Public"}
          </p>
        </div>
        {isPrivate && access.data.private_before_full_switch === true ? (
          <p
            className={`mt-1 text-sm font-medium ${textPrimaryOnCanvas}`}
            data-testid="access-before-full-switch"
          >
            This project is private before its full switch: Dispatch hides it as any private
            project, while its own namespace, stores and message gate come with its full switch.
            Until then, its notifications reach any agent session that subscribes to them.
          </p>
        ) : null}
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          Every full member can see a public project. A private project is seen only by its owner
          and the people and groups below. A restricted member, who signs in from outside the
          company, sees either only when the people below name them directly.
        </p>
        {refusalIn("visibility")}
      </section>

      <section aria-labelledby={`${ids}-slack`}>
        <h2 className={headingClass} id={`${ids}-slack`}>
          Slack channels
        </h2>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          The channels this project's agents may post to, by channel id (the `C…` or `G…` Slack
          shows under About → Channel ID), at most 16, separated by spaces or commas. The secrets
          broker is to release the Slack bot token to a private project's agents for these channels
          alone; it does not read this list yet.
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
        {channelsChangedAt === null ? null : (
          <p
            className={`mt-2 text-sm ${textMutedOnCanvas}`}
            data-testid="access-slack-channels-changed"
          >
            Changed <Timestamp at={channelsChangedAt} />
          </p>
        )}
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
                <span className={`min-w-0 break-all text-sm ${textPrimaryOnSurface}`}>
                  {email}
                  {notSignedIn.has(email) ? (
                    <span className={`ml-2 ${textSecondaryOnCanvas}`}>not signed in yet</span>
                  ) : null}
                </span>
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
          A group admits only its full members.
          {/* A restricted member's Settings draws no Groups, so they are sent to none. */}
          {restricted ? null : (
            <>
              {" "}
              Groups are made and edited in{" "}
              <Link className={`font-medium ${linkText} ${linkHoverText}`} to="/settings#groups">
                Settings → Groups
              </Link>
              .
            </>
          )}
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
