import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { api, type WriteBounds, type WriteMethod } from "../../api/client";
import { isLiveQuery } from "../../api/query-refresh";
import type { DeliverySessionGroup, DeliveryTimelineWithMeasures } from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { pressOfWrite } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { compareTimestamps } from "../../lib/timestamps";
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
import { applyDelta } from "./lib/applyDelta";
import { prCount } from "./lib/facets";
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

/** A card's rename draft, as the reader typed it, and its newest Rename while the draft is still
 *  as that Rename sent it: the press, so only that Rename's own landing clears the draft, and,
 *  once that Rename landed with no group of its own to clear the draft by, the version of the
 *  group in effect at that landing (`over`), to tell a later version apart. */
interface RenameDraft {
  readonly draft: string | null;
  readonly sent: { readonly over?: DeliverySessionGroup; readonly press: object } | null;
}

const NO_RENAME: RenameDraft = { draft: null, sent: null };

/** The Delivery page's reads that hold the groups: its timeline, an entry per view
 *  (`deliveryTimelineQuery`). */
const GROUP_READS = { queryKey: ["delivery", "timeline"] };

/** The later of two versions of a group, by when Dispatch wrote each (`updated_at`): `held`, as
 *  the editor was handed it or a read holds it, and `other`, which may be newer - a Rename's
 *  answer, or the group as another read holds it - and wins a tie.
 *
 *  The order is exact: Dispatch's `UpdateSessionGroup`
 *  (`packages/envoy/internal/dispatch/delivery/groups.go`) writes `updated_at` under the groups'
 *  lock, as the later of the stored value and the statement's own clock time, one microsecond
 *  past the stored value, so a group's versions carry their commit times in order and no two of
 *  them tie (cybermonklabs/legion#5868, merged b5f564a9ab6a). A server from before that change
 *  writes the transaction's start instead, and against one of those a version that committed
 *  last can carry the older time. */
function laterGroup(
  held: DeliverySessionGroup,
  other: DeliverySessionGroup | undefined
): DeliverySessionGroup {
  return other !== undefined && compareTimestamps(other.updated_at, held.updated_at) >= 0
    ? other
    : held;
}

/** Writes `group`, a Rename's answer, into each groups read that holds an older version of it, as
 *  Dispatch's delta for the rename will (`applyDelta`): in that one's place, in Dispatch's order. */
function writeGroup(queryClient: QueryClient, group: DeliverySessionGroup): void {
  queryClient.setQueriesData<DeliveryTimelineWithMeasures>(GROUP_READS, (body) => {
    const held = body?.session_groups?.find((each) => each.id === group.id);
    if (body === undefined || held === undefined || laterGroup(held, group) === held) return body;
    // The delta as `applyDelta` reads it: the list's change alone. Its place in the change log
    // (`sub`, `seq`, `revs`) is the socket's own, and Dispatch's delta for the rename brings it.
    return applyDelta(body, {
      changed_at: null,
      counts: {},
      items: { session_groups: { remove: [], upsert: [group] } },
      maps: {},
      revs: {},
      seq: "",
      set: {},
      sub: "",
      type: "delta",
    });
  });
}

/** Whether `group` is a version other than `over`, the one in effect when a card's Rename landed
 *  with no answer: the card holds its draft until such a version arrives. */
function arrivedSince(over: DeliverySessionGroup, group: DeliverySessionGroup): boolean {
  return group.updated_at !== over.updated_at || group.name !== over.name;
}

const sectionLabel = `text-xs tracking-wide uppercase ${textMutedOnSurface}`;
const fieldClass = `h-8 min-w-0 flex-1 rounded border px-2 text-sm ${inputClasses(true)} ${textPrimaryOnSurface}`;

/** What a group's card says before why the live update carrying its write is not drawn, once the
 *  hold for it ran out (`useHoldUntilDrawn`): the editor reads its groups from that update. */
const SESSION_GROUPS_NOT_LOADED = "Couldn't load the session groups.";

/** One group: its name, renamable, its members, each removable, and the window's selected
 *  sessions to add, drawn as an item of the editor's list of groups, keyed by the group's id.
 *  While any group write is pending, every mutating button is held so a second click cannot race
 *  the one request that decides the membership. A write of this card's own that lands keeps them
 *  held until the update carrying it brings the group's new `updated_at` - a Rename's own answer,
 *  which the card writes into the page's groups reads (`writeGroup`), or Dispatch's live update -
 *  or takes the group away (`useHoldUntilDrawn`); a refusal lets them go at once. An update that
 *  never comes lets them go at the write deadline, and the card says why until it comes. The card
 *  and its buttons stay the same elements through that update. Rename with no new name and Add
 *  selected with no session to add are held as well. Every hold is by `aria-disabled`, not
 *  `disabled` (`heldPress`): a held button keeps its Tab stop and the focus the reader's press
 *  left on it, and a press on it does nothing.
 *
 *  Its groups, in this order, each on a line of its own: Name (the name's field and Rename),
 *  Sessions (its members as chips, each with its ✕: the first ten, then "+N" for the rest) and
 *  Actions (Add selected, Delete group), then the line that says why an update is not drawn,
 *  while there is one. A chip shrinks to the list's width and its name wraps inside it
 *  (`AgentName`'s own `[overflow-wrap:anywhere]`): below the wide layout a link is `inline-flex`
 *  (the 44 px touch rule in styles.css), and a cut line there would end in no ellipsis. */
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
  /** Sends `change`, and calls `press`'s `onError` if Dispatch refuses it, or its `onSuccess`
   *  with Dispatch's answer (`null` where it had none to give) once Dispatch has made it. */
  write: (
    change: GroupChange,
    press?: { onError?: () => void; onSuccess?: (answer: unknown) => void }
  ) => void;
}): ReactNode {
  const id = useId();
  const text = useAgentText();
  const queryClient = useQueryClient();
  // The reader's rename draft, kept as they typed it while the group changes under it (a member
  // added, a rename from another tab): null while the reader has typed nothing, so the field
  // shows the group's name as it now stands, and back to null once the group's name is the draft.
  // `sent` is this card's newest Rename while the draft is still as that Rename sent it: typing
  // drops it, and so does Dispatch refusing that Rename. That Rename's own landing clears the
  // draft, and its answer (`landed`) is the group from then on, until the editor is handed a newer
  // one. Taken in the render the draft clears in, it shows while TanStack Query has not yet told
  // the editor's host of the write - in a browser, a timer later. Where Dispatch had no answer to
  // give, the draft stays until a version of the group other than the one in effect at that
  // landing arrives: the read the landing starts, never a rename from another tab that arrived
  // while the Rename was out. A rename that arrives while the Rename is out is no answer to it,
  // and keeps the draft as typed.
  const [rename, setRename] = useState<RenameDraft>(NO_RENAME);
  const [landed, setLanded] = useState<DeliverySessionGroup | undefined>(undefined);
  const current = laterGroup(group, landed);
  // The version this render draws, which a landing with no answer reads as the one in effect then.
  const currentRef = useRef(current);
  currentRef.current = current;
  if (
    rename.draft !== null &&
    (rename.draft === current.name ||
      (rename.sent?.over !== undefined && arrivedSince(rename.sent.over, current)))
  ) {
    setRename(NO_RENAME);
  }
  const name = rename.draft ?? current.name;
  /** Rename `press` landed with `renamed` as its answer: that group stands until the editor is
   *  handed a newer one, and the draft goes where it is still as that Rename sent it. */
  const land = (press: object, renamed: DeliverySessionGroup) => {
    setLanded((before) => (before === undefined ? renamed : laterGroup(before, renamed)));
    setRename((now) => (now.sent?.press === press ? NO_RENAME : now));
  };
  const [drawing, holdUntilDrawn, undrawn] = useHoldUntilDrawn(group.updated_at);
  const held = pending || drawing;
  // The press's render hands its own `holdUntilDrawn`, so the hold is on the version pressed on,
  // and on the press the write began under (`pressOfWrite`), read here at that press. The hold is
  // put on as the write lands, before what the landing does with Dispatch's answer.
  const send = (change: GroupChange, outcome?: Parameters<typeof write>[1]) => {
    const press = pressOfWrite();
    write(change, {
      onError: outcome?.onError,
      onSuccess: (answer) => {
        holdUntilDrawn(press);
        outcome?.onSuccess?.(answer);
      },
    });
  };
  const addable = selected.filter((session) => !(group.sessions ?? []).includes(session));
  const nothingToRename = name.trim() === "" || name.trim() === current.name;
  return (
    <fieldset aria-label={group.name} className={`grid gap-2 rounded border p-3 ${borderDefault}`}>
      <div className="flex items-center gap-2">
        <label className="sr-only" htmlFor={`${id}-name`}>
          Name
        </label>
        <input
          className={fieldClass}
          id={`${id}-name`}
          onChange={(event) => setRename({ draft: event.target.value, sent: null })}
          value={name}
        />
        <button
          className={secondaryButtonCompact}
          {...heldPress(held || nothingToRename, () => {
            // This Rename, by identity: its own landing, and no rename that arrives meanwhile,
            // clears the draft, and only while the draft is still as this Rename sent it.
            const press = {};
            setRename({ draft: name, sent: { press } });
            send(
              {
                group,
                method: "PATCH",
                run: (bounds) =>
                  api.updateDeliverySessionGroup(group.id, { name: name.trim() }, bounds),
                what: `the rename of the group ${group.name}`,
              },
              {
                onError: () =>
                  setRename((now) => (now.sent?.press === press ? { ...now, sent: null } : now)),
                onSuccess: (answer) => {
                  if (answer === null) {
                    // Dispatch took the Rename and had no answer to give for it. The group stands
                    // as the reads this landing starts answer it - the editor's own `onSuccess`
                    // asks the delivery reads again, the write store reads every query again for
                    // a landing with no answer, and Dispatch sends its delta - so the draft stays
                    // as typed until a version other than the one in effect now arrives, and
                    // never takes that one back. A rename from another tab that arrived while
                    // this one was out is that version, and no answer to this Rename.
                    const over = currentRef.current;
                    setRename((now) =>
                      now.sent?.press === press ? { ...now, sent: { over, press } } : now
                    );
                    return;
                  }
                  // A rename's answer is the group as renamed (`updateDeliverySessionGroup`),
                  // written into the page's groups reads as Dispatch's delta for it will be.
                  const renamed = answer as DeliverySessionGroup;
                  writeGroup(queryClient, renamed);
                  land(press, renamed);
                },
              }
            );
          })}
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
          </span>
        ))}
      </CappedGroup>
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
      {undrawn === undefined ? null : (
        <QueryError message={`${SESSION_GROUPS_NOT_LOADED} ${undrawn}.`} />
      )}
    </fieldset>
  );
}

/** The Delivery page's session-groups editor, opened from **Edit groups** at the foot of the
 *  page's Filter panel: the window's sessions with their titles, pull request counts and groups,
 *  to select and name as a new group, and every group, to rename, add the selected sessions to,
 *  remove members from, or delete. Every session is an `AgentName`; a member no pull request of
 *  the window names has no stored title, so it reads by its registry title, else its short id. A
 *  session's count is drawn only while `counted` holds, the page's word that the answer on screen
 *  is the one for the window, filters and search its address names: not while a new read is out
 *  and the previous answer stays on screen, nor while a typed search waits to be asked for. Where
 *  the address names another window, the counts on screen are the previous window's. While
 *  `counted` does not hold, each session is listed with its group alone. A save reaches the
 *  page's live views as Dispatch's own delta, so it refreshes only the `["delivery"]` reads that
 *  are not live (an open run's drill-down), and a refusal shows as the server wrote it, after the
 *  change it refused. A dialog (`useDialog`), at the right edge as the drill-down is, open while
 *  it is mounted.
 *
 *  Its groups, in this order, each on a line of its own: Heading (the title, Close at the right),
 *  the note on what a group is, Sessions (the window's sessions, one row each), New group (the
 *  name field and Create group), the refusal, and Groups (one `GroupCard` each). A session's row
 *  has two groups: Session (its checkbox and name, with its live link) and Membership (its pull
 *  request count and its group's name); a row too narrow for both moves Membership to a line of
 *  its own, whose text wraps inside it, so a long group name never pushes the session's name
 *  aside. A card is keyed by its group's id, so a change to the group redraws it in place and
 *  keeps the reader's rename draft, and the sessions it opened past their "+N" (`CappedGroup`'s
 *  own state). */
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
            const detail = [counted ? prCount(count) : undefined, inGroup]
              .filter((part) => part !== undefined)
              .join(" · ");
            return (
              <li className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-sm" key={session}>
                {/* The checkbox's label keeps the name; the agent's live link sits beside it. */}
                <AgentName
                  className="flex min-w-0 flex-1 basis-40 items-center"
                  control={(agentName) => (
                    <label
                      className="flex min-w-0 flex-1 items-center gap-2"
                      title={`${text.sessionShown(session, storedTitle(session))} (${session})`}
                    >
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
                  <span
                    className={`min-w-0 max-w-full text-xs [overflow-wrap:anywhere] ${textMutedOnSurface}`}
                  >
                    {detail}
                  </span>
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
                  write={(change, press) => save.mutate(change, press)}
                />
              </li>
            ))}
          </ul>
        </section>
      )}
    </aside>
  );
}
