import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { api, type WriteBounds, type WriteMethod } from "../../api/client";
import { isLiveQuery } from "../../api/query-refresh";
import type { DeliverySessionGroup } from "../../api/types";
import { heldPress } from "../../components/held-press";
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
import { useHoldUntilDrawn } from "./useHoldUntilDrawn";

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
 *  sessions to add, drawn as an item of the editor's list of groups, keyed by the group's id.
 *  While any group write is pending, every mutating button is held so a second click cannot race
 *  the one request that decides the membership. A write of this card's own that lands keeps them
 *  held until the live update carrying it brings the group's new `updated_at`, or takes the group
 *  away (`useHoldUntilDrawn`); a refusal lets them go at once. That update fills the name again
 *  from the stored group in place, so the card and its buttons stay the same elements. Rename
 *  with no new name and Add selected with no session to add are held as well. Every hold is by
 *  `aria-disabled`, not `disabled` (`heldPress`): a held button keeps its Tab stop and the focus
 *  the reader's press left on it, and a press on it does nothing. */
function GroupCard({
  group,
  storedTitle,
  selected,
  pending,
  write,
}: {
  group: DeliverySessionGroup;
  storedTitle: (session: string) => string | undefined;
  selected: readonly string[];
  pending: boolean;
  /** Sends `change`, and calls `landed` once Dispatch has made it. */
  write: (change: GroupChange, landed: () => void) => void;
}): ReactNode {
  const id = useId();
  const text = useAgentText();
  const [name, setName] = useState(group.name);
  // A new stored group - this card's write, or anyone's that a live update brings - fills the
  // name again. It is set during render (React's pattern for state derived from props) and not
  // by a key on the card, so the card and the button the reader pressed stay the same elements.
  const [filledFrom, setFilledFrom] = useState(group.updated_at);
  if (group.updated_at !== filledFrom) {
    setFilledFrom(group.updated_at);
    setName(group.name);
  }
  const [drawing, holdUntilDrawn] = useHoldUntilDrawn(group.updated_at);
  const held = pending || drawing;
  // The press's render hands its own `holdUntilDrawn`, so the hold is on the version pressed on.
  const send = (change: GroupChange) => write(change, holdUntilDrawn);
  const addable = selected.filter((session) => !(group.sessions ?? []).includes(session));
  const nothingToRename = name.trim() === "" || name.trim() === group.name;
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
          {...heldPress(held || nothingToRename, () =>
            send({
              group,
              method: "PATCH",
              run: (bounds) =>
                api.updateDeliverySessionGroup(group.id, { name: name.trim() }, bounds),
              what: `the rename of the group ${group.name}`,
            })
          )}
          type="button"
        >
          Rename
        </button>
      </div>
      <ul className="flex flex-wrap gap-1">
        {(group.sessions ?? []).map((session) => (
          <li
            className={`flex items-center gap-1 rounded border px-2 py-0.5 text-xs ${borderStrong}`}
            key={session}
          >
            <AgentName
              className="max-w-48 truncate"
              sessionId={session}
              storedTitle={storedTitle(session)}
            />
            <button
              aria-label={`Remove ${text.sessionText(session, storedTitle(session))}`}
              className={dismissButtonText}
              {...heldPress(held, () =>
                send({
                  group,
                  method: "PATCH",
                  run: (bounds) =>
                    api.updateDeliverySessionGroup(group.id, { remove: [session] }, bounds),
                  what: `the removal of ${text.sessionText(session, storedTitle(session))} from ${group.name}`,
                })
              )}
              type="button"
            >
              ✕
            </button>
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <button
          className={secondaryButtonCompact}
          {...heldPress(held || addable.length === 0, () =>
            send({
              group,
              method: "PATCH",
              run: (bounds) => api.updateDeliverySessionGroup(group.id, { add: addable }, bounds),
              what: `the sessions added to ${group.name}`,
            })
          )}
          type="button"
        >
          Add selected
        </button>
        <button
          className={`${secondaryButtonCompact} ${dangerText}`}
          {...heldPress(held, () =>
            send({
              group,
              method: "DELETE",
              run: (bounds) => api.deleteDeliverySessionGroup(group.id, bounds),
              what: `the deletion of the group ${group.name}`,
            })
          )}
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
 *  it reads by its registry title, else its short id. A session's count is drawn only while
 *  `counted` holds, the page's word that the answer on screen is the one for the window, filters
 *  and search its address names: not while a new read is out and the previous answer stays on
 *  screen, nor while a typed search waits to be asked for. Where the address names another
 *  window, the counts on screen are the previous window's. While `counted` does not hold, each
 *  session is listed with its group alone. A save reaches the page's live views as Dispatch's own
 *  delta, so it refreshes only the `["delivery"]` reads that are not live (an open run's
 *  drill-down), and a refusal shows as the server wrote it, after the change it refused. A dialog
 *  (`useDialog`), at the right edge as the drill-down is, open while it is mounted. */
export function SessionGroupsEditor({
  onClose,
  groups,
  titles,
  counted = true,
  counts,
}: {
  onClose: () => void;
  groups: readonly DeliverySessionGroup[];
  titles: Readonly<Record<string, string>>;
  /** Whether the answer `counts` comes from is the one for the window, filters and search the
   *  page's address names (the page's `counted`); an editor given none counts. */
  counted?: boolean;
  counts: Readonly<Record<string, number>>;
}): ReactNode {
  const id = useId();
  const queryClient = useQueryClient();
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open: true });
  const [selected, setSelected] = useState<string[]>([]);
  const [name, setName] = useState("");
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
            // No count while the answer on screen is not the address's (`counted`): the session's
            // group alone.
            const detail = [counted ? `${count} PR${count === 1 ? "" : "s"}` : undefined, inGroup]
              .filter((part) => part !== undefined)
              .join(" · ");
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
                {detail === "" ? null : (
                  <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>{detail}</span>
                )}
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
          {/* Held, as a card's Rename and Add selected are, while its write is out and while it
              has nothing to create: no name, or no session selected. A create that lands empties
              the name, and the button the reader pressed keeps the focus through both
              (`heldPress`). */}
          <button
            className={secondaryButtonCompact}
            {...heldPress(name.trim() === "" || selected.length === 0 || save.isPending, create)}
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
          <h3 className={sectionLabel} id={`${id}-groups`}>
            Groups
          </h3>
          <ul aria-labelledby={`${id}-groups`} className="grid gap-2">
            {groups.map((group) => (
              <li key={group.id}>
                <GroupCard
                  group={group}
                  pending={save.isPending}
                  selected={selected}
                  storedTitle={storedTitle}
                  write={(change, landed) => save.mutate(change, { onSuccess: landed })}
                />
              </li>
            ))}
          </ul>
        </section>
      )}
    </aside>
  );
}
