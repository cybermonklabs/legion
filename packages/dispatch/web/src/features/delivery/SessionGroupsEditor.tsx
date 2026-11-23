import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { api, type WriteBounds, type WriteMethod } from "../../api/client";
import { isLiveQuery } from "../../api/query-refresh";
import type { DeliverySessionGroup } from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  borderStrong,
  dangerText,
  dismissButtonText,
  inputClasses,
  secondaryButtonCompact,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { useDialog } from "../shell/useDialog";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { refusedNote, useKeyedWrite, useRefusalOfSubject } from "../writes/useKeyedWrite";
import { storedSessionTitle } from "./lib/sessionTitles";

/** One change to the groups: what it is, as the editor's sentences name it (`the rename of the
 *  group Crew`), the group it changes, the method of its request and one attempt of it. A delete
 *  is idempotent and goes without the key. A create changes no group yet and names what it makes
 *  (`made`), which the editor says was made when Dispatch lost its answer. */
interface GroupChange {
  readonly group?: DeliverySessionGroup;
  readonly made?: string;
  readonly method: WriteMethod;
  readonly run: (bounds: WriteBounds) => Promise<unknown>;
  readonly what: string;
}

const sectionLabel = `text-xs tracking-wide uppercase ${textMutedOnSurface}`;
const fieldClass = `h-8 min-w-0 flex-1 rounded border px-2 text-sm ${inputClasses(true)} ${textPrimaryOnSurface}`;

/** One group: its name, renamable, its members, each removable, and the window's selected
 *  sessions to add. While any group write is pending, every mutating button is disabled so a
 *  second click cannot race the one request that decides the membership.
 *
 *  Its groups, in this order, each on a line of its own: Name (the name's field and Rename),
 *  Sessions (its members as chips, each with its ✕: the first ten, then "+N" for the rest) and
 *  Actions (Add selected, Delete group). A chip shrinks to the list's width and its name wraps
 *  inside it (`AgentName`'s own `[overflow-wrap:anywhere]`): below the wide layout a link is
 *  `inline-flex` (the 44 px touch rule in styles.css), and a cut line there would end in no
 *  ellipsis. */
function GroupCard({
  group,
  storedTitle,
  selected,
  pending,
  write,
  sessionsOpen,
  onSessionsOpen,
}: {
  group: DeliverySessionGroup;
  storedTitle: (session: string) => string | undefined;
  selected: readonly string[];
  pending: boolean;
  write: (change: GroupChange) => void;
  /** Whether the reader opened this group's sessions past their "+N". The editor holds it, by
   *  group id: a write moves the group's `updated_at`, which mounts this card again. */
  sessionsOpen: boolean;
  onSessionsOpen: (open: boolean) => void;
}): ReactNode {
  const id = useId();
  const text = useAgentText();
  const [name, setName] = useState(group.name);
  const addable = selected.filter((session) => !(group.sessions ?? []).includes(session));
  return (
    <fieldset aria-label={group.name} className={`grid gap-2 rounded border p-3 ${borderDefault}`}>
      <div className="flex items-center gap-2">
        <label className="sr-only" htmlFor={`${id}-name`}>
          Name
        </label>
        <input
          className={fieldClass}
          id={`${id}-name`}
          onChange={(event) => setName(event.target.value)}
          value={name}
        />
        <button
          className={secondaryButtonCompact}
          disabled={pending || name.trim() === "" || name.trim() === group.name}
          onClick={() =>
            write({
              group,
              method: "PATCH",
              run: (bounds) =>
                api.updateDeliverySessionGroup(group.id, { name: name.trim() }, bounds),
              what: `the rename of the group ${group.name}`,
            })
          }
          type="button"
        >
          Rename
        </button>
      </div>
      <CappedGroup
        className="flex flex-wrap gap-1 [&>li]:min-w-0 [&>li]:max-w-full"
        label="sessions"
        list={`Sessions in ${group.name}`}
        name="group-sessions"
        onOpenChange={onSessionsOpen}
        open={sessionsOpen}
      >
        {(group.sessions ?? []).map((session) => (
          <span
            className={`flex min-w-0 max-w-full items-center gap-1 rounded border px-2 py-0.5 text-xs ${borderStrong}`}
            key={session}
          >
            <AgentName sessionId={session} storedTitle={storedTitle(session)} />
            <button
              aria-label={`Remove ${text.sessionText(session, storedTitle(session))}`}
              className={dismissButtonText}
              disabled={pending}
              onClick={() =>
                write({
                  group,
                  method: "PATCH",
                  run: (bounds) =>
                    api.updateDeliverySessionGroup(group.id, { remove: [session] }, bounds),
                  what: `the removal of ${text.sessionText(session, storedTitle(session))} from ${group.name}`,
                })
              }
              type="button"
            >
              ✕
            </button>
          </span>
        ))}
      </CappedGroup>
      <div className="flex gap-2">
        <button
          className={secondaryButtonCompact}
          disabled={pending || addable.length === 0}
          onClick={() =>
            write({
              group,
              method: "PATCH",
              run: (bounds) => api.updateDeliverySessionGroup(group.id, { add: addable }, bounds),
              what: `the sessions added to ${group.name}`,
            })
          }
          type="button"
        >
          Add selected
        </button>
        <button
          className={`${secondaryButtonCompact} ${dangerText}`}
          disabled={pending}
          onClick={() =>
            write({
              group,
              method: "DELETE",
              run: (bounds) => api.deleteDeliverySessionGroup(group.id, bounds),
              what: `the deletion of the group ${group.name}`,
            })
          }
          type="button"
        >
          Delete group
        </button>
      </div>
    </fieldset>
  );
}

/** The Delivery page's session-groups editor, opened from the Parent agent facet: the window's
 *  sessions with their titles, pull request counts and groups, to select and name as a new group,
 *  and every group, to rename, add the selected sessions to, remove members from, or delete. Every
 *  session is an `AgentName`; a member no pull request of the window names has no stored title, so
 *  it reads by its registry title, else its short id. A save reaches the page's live views as
 *  Dispatch's own delta, so it refreshes only the `["delivery"]` reads that are not live (an open
 *  run's drill-down), and a refusal shows as the server wrote it, after the change it refused. A
 *  dialog (`useDialog`), at the right edge as the drill-down is, open while it is mounted. */
export function SessionGroupsEditor({
  onClose,
  groups,
  titles,
  counts,
}: {
  onClose: () => void;
  groups: readonly DeliverySessionGroup[];
  titles: Readonly<Record<string, string>>;
  counts: Readonly<Record<string, number>>;
}): ReactNode {
  const id = useId();
  const queryClient = useQueryClient();
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open: true });
  const [selected, setSelected] = useState<string[]>([]);
  const [name, setName] = useState("");
  const [openGroups, setOpenGroups] = useState<ReadonlySet<string>>(() => new Set());
  const openGroup = (groupId: string, open: boolean) => {
    setOpenGroups((current) => {
      const next = new Set(current);
      if (open) next.add(groupId);
      else next.delete(groupId);
      return next;
    });
  };
  // The editor shows a refusal itself, as the server wrote it.
  const save = useKeyedWrite({
    inline: true,
    label: (change: GroupChange) => `Session groups: ${change.what}`,
    onSuccess: () => {
      setSelected([]);
      void queryClient.invalidateQueries({
        queryKey: ["delivery"],
        predicate: (query) => !isLiveQuery(query),
      });
    },
    method: (change) => change.method,
    write: (change, bounds) => change.run(bounds),
  });
  // One refusal speaks for every group, so it names the change it refused, the group's name with
  // it, and goes once that group is renamed or removed - from another tab, or by another person -
  // rather than stand beside groups it was never about. A create's has no group to lose.
  const refused = useRefusalOfSubject(
    save,
    ({ group: changed }) =>
      changed === undefined ||
      groups.some((group) => group.id === changed.id && group.name === changed.name)
  )
    ? save.variables
    : undefined;

  const text = useAgentText();
  const storedTitle = (session: string): string | undefined => storedSessionTitle(titles, session);
  const groupOf = new Map<string, string>();
  for (const group of groups) {
    for (const session of group.sessions ?? []) groupOf.set(session, group.name);
  }
  const sessions = Object.keys(counts).sort(
    (a, b) =>
      (counts[b] ?? 0) - (counts[a] ?? 0) ||
      text.sessionText(a, storedTitle(a)).localeCompare(text.sessionText(b, storedTitle(b)))
  );
  const toggle = (session: string) =>
    setSelected((current) =>
      current.includes(session) ? current.filter((s) => s !== session) : [...current, session]
    );
  const create = () =>
    save.mutate(
      {
        made: `The group “${name.trim()}”`,
        method: "POST",
        run: (bounds) =>
          api.createDeliverySessionGroup({ name: name.trim(), sessions: selected }, bounds),
        what: `the new group ${name.trim()}`,
      },
      // Made either way - with no answer to give too - so the name goes and a second press
      // makes nothing.
      { onSuccess: () => setName("") }
    );
  const madeUnanswered = save.unanswered?.variables.made;

  return (
    <aside
      aria-labelledby={`${id}-heading`}
      aria-modal="true"
      className={`fixed top-0 right-0 z-30 flex h-full w-[28rem] max-w-full flex-col gap-4 overflow-y-auto border-l p-4 shadow-xl ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`}
      ref={dialog.containerRef}
      role="dialog"
    >
      <div className="flex items-start justify-between gap-2">
        <h2 className="text-lg font-semibold" id={`${id}-heading`}>
          Session groups
        </h2>
        <button
          aria-label="Close session groups"
          className={dismissButtonText}
          onClick={onClose}
          ref={closeButtonRef}
          type="button"
        >
          ✕
        </button>
      </div>
      <p className={`text-sm ${textMutedOnSurface}`}>
        A group is one Parent agent: every pull request its sessions wrote first counts under the
        group's name. A session can be in one group.
      </p>

      <section className="grid gap-2">
        <h3 className={sectionLabel} id={`${id}-sessions`}>
          Sessions in this window
        </h3>
        <ul aria-labelledby={`${id}-sessions`} className="grid gap-1">
          {sessions.map((session) => {
            const count = counts[session] ?? 0;
            const inGroup = groupOf.get(session);
            return (
              <li className="flex items-center gap-2 text-sm" key={session}>
                {/* The checkbox's label keeps the name; the agent's live link sits beside it. */}
                <AgentName
                  className="flex min-w-0 flex-1 items-center"
                  control={(agentName) => (
                    <label className="flex min-w-0 flex-1 items-center gap-2" title={session}>
                      <input
                        checked={selected.includes(session)}
                        onChange={() => toggle(session)}
                        type="checkbox"
                      />
                      <span className="min-w-0 truncate">{agentName}</span>
                    </label>
                  )}
                  kind="control"
                  sessionId={session}
                  storedTitle={storedTitle(session)}
                />
                <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>
                  {count} PR{count === 1 ? "" : "s"}
                  {inGroup === undefined ? "" : ` · ${inGroup}`}
                </span>
              </li>
            );
          })}
        </ul>
        <div className="flex items-center gap-2">
          <label className="sr-only" htmlFor={`${id}-new-name`}>
            New group name
          </label>
          <input
            className={fieldClass}
            id={`${id}-new-name`}
            onChange={(event) => {
              save.forgetUnanswered();
              setName(event.target.value);
            }}
            placeholder="Group name"
            value={name}
          />
          <button
            className={secondaryButtonCompact}
            disabled={name.trim() === "" || selected.length === 0 || save.isPending}
            onClick={create}
            type="button"
          >
            Create group
          </button>
        </div>
        {madeUnanswered === undefined ? null : <LandedWithoutAnswer what={madeUnanswered} />}
      </section>

      {/* A change the reader stopped may have been saved, and every button here is a press of
          its own, under a key of its own: none makes the stopped change again. */}
      {refused === undefined ? null : (
        <QueryError
          message={
            save.error instanceof SendStoppedError
              ? stoppedWriteNote(refused.what, "saved", {
                  anew: "Making it again sends it as a new write.",
                })
              : refusedNote(`save ${refused.what}`, save.error)
          }
        />
      )}

      {groups.length === 0 ? null : (
        <section className="grid gap-2">
          <h3 className={sectionLabel}>Groups</h3>
          {groups.map((group) => (
            <GroupCard
              group={group}
              key={`${group.id}:${group.updated_at}`}
              onSessionsOpen={(open) => openGroup(group.id, open)}
              pending={save.isPending}
              selected={selected}
              sessionsOpen={openGroups.has(group.id)}
              storedTitle={storedTitle}
              write={(change) => save.mutate(change)}
            />
          ))}
        </section>
      )}
    </aside>
  );
}
