import {
  createContext,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import {
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { type IssueAssigneeWrite, useIssueAssignee } from "../issue/useIssueAssignee";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { useInboxWrites } from "./ask-writes";
import { inboxActionMark } from "./inbox-action-mark";
import { RowAsideContext } from "./pane-selection";

/** What one ask's Assign to me write is doing, as every control drawn for the ask shows it: the
 *  write's own `error`, `failed`, `held` and `pending`, and whether the refused save may be made
 *  again (`IssueFieldWrite.retry`). */
type AssignShown = Readonly<Pick<IssueAssigneeWrite, "error" | "failed" | "held" | "pending">> & {
  readonly retryable: boolean;
};

/** An ask whose Assign to me has made no save. */
const NOTHING_SAVED: AssignShown = {
  error: undefined,
  failed: false,
  held: false,
  pending: false,
  retryable: false,
};

/** An ask whose Assign to me has a save out, as its press leaves it (`useIssueFieldWrite`'s
 *  `submit`, and its `retry`, which make a save out at once and end any refusal). */
const SAVING: AssignShown = {
  error: undefined,
  failed: false,
  held: true,
  pending: true,
  retryable: false,
};

/** One ask's Assign to me in an Inbox: what its write is doing, and its two presses. */
export interface AskAssignWrite extends AssignShown {
  /** Makes the refused save again; undefined while no save may be made again. */
  readonly retry: (() => void) | undefined;
  /** Assigns the ask's issue to `viewer`. */
  readonly submit: (viewer: string) => void;
}

/** Every Assign to me of one Inbox, by ask (`AskAssignWritesProvider`). */
export interface AskAssignWrites {
  /** Notes a control drawn for `askId`, of `issueKey`, until the returned function is called.
   *  The Inbox makes the ask's write while a control is drawn for it, and while its write is out
   *  or refused. */
  readonly draw: (askId: string, issueKey: string) => () => void;
  /** The asks whose save is out or refused, which the Inbox marks as the reader's own writes
   *  (`AskWrites.live`). */
  readonly live: ReadonlySet<string>;
  /** The Assign to me of `askId`. Every control drawn for that ask shows this one write. */
  readonly of: (askId: string) => AskAssignWrite;
  /** The asks whose save is out. The Inbox keeps each one's row where the reader saw it until
   *  the save lands (`heldRow`). A refused or stopped save is not out, so the next read of the
   *  Inbox places its row. */
  readonly out: ReadonlySet<string>;
}

/** The Assign to me writes of the Inbox around a control. A control is drawn only inside an
 *  Inbox, so a control with no provider is a mistake in the code, and `useInboxWrites` throws for
 *  it. */
export const AskAssignWritesContext = createContext<AskAssignWrites | undefined>(undefined);

/** The Assign to me of `askId` in the Inbox around the caller. */
export function useAskAssign(askId: string): AskAssignWrite {
  return useInboxWrites(AskAssignWritesContext).of(askId);
}

/** One ask the Inbox makes an Assign to me write for: its issue, and what its write is doing. */
interface AssignEntry {
  readonly issueKey: string;
  readonly shown: AssignShown;
}

/**
 * Every Assign to me of one Inbox, made and held by ask in the Inbox's own state rather than by
 * the control that pressed. A control can go while its save is out: the reading column's head
 * goes whenever the reader moves the selection. So each ask's save is made by the Inbox, one
 * `useIssueAssignee` of the ask's issue drawn here for each ask (`AssignWrite`), which lives
 * while a control is drawn for the ask and while its save is out or refused. Every control drawn
 * for the ask - the row's own, the reading column's head, and the short row beside the column -
 * shows that one write, its `Assigning…`, its refusal and its Retry, and presses through it.
 *
 * A press makes its save out in the Inbox's state in the commit of the press itself, so the
 * control is held, and the Inbox holds the ask's row, from that commit on, before the press's
 * optimistic assignee reaches the Inbox's read (`useIssueFieldWrite`). Each write tells the Inbox
 * what it is doing from then on in the layout effects of each commit that changes that: its
 * landing, its refusal and its Retry. The writes' own steps re-render this component alone: what
 * it draws re-renders only when the writes it provides change. How many controls are drawn for
 * each ask is kept outside the state, so a control drawn or gone changes the writes only where it
 * changes which asks the Inbox makes a write for. Each Inbox provides its own: the page's and the
 * drawer's are separate.
 */
export function AskAssignWritesProvider({ children }: { children: ReactNode }): ReactNode {
  // The asks the Inbox makes a write for: an ask while a control is drawn for it, or while its
  // save is out or refused.
  const [entries, setEntries] = useState<ReadonlyMap<string, AssignEntry>>(() => new Map());
  /** How many controls are drawn for each ask that has one. */
  const [drawn] = useState(() => new Map<string, number>());
  /** Each ask's write as it was last drawn, which a press and a Retry go through. */
  const [made] = useState(() => new Map<string, RefObject<IssueAssigneeWrite>>());
  /** Changes the entry of `askId`, where it has one, and lets go of it once the Inbox no longer
   *  makes its write: no control is drawn for the ask, and its save is neither out nor refused. */
  const update = useCallback(
    (askId: string, change: (entry: AssignEntry) => AssignEntry) => {
      const controlDrawn = drawn.has(askId);
      setEntries((current) => {
        const entry = current.get(askId);
        if (entry === undefined) return current;
        const next = change(entry);
        const kept = controlDrawn || next.shown.pending || next.shown.failed;
        if (kept && next === entry) return current;
        const changed = new Map(current);
        if (kept) changed.set(askId, next);
        else changed.delete(askId);
        return changed;
      });
    },
    [drawn]
  );
  const draw = useCallback(
    (askId: string, issueKey: string) => {
      const controls = drawn.get(askId) ?? 0;
      drawn.set(askId, controls + 1);
      // An ask that already had a control drawn for it has its entry already.
      if (controls === 0) {
        setEntries((current) =>
          current.has(askId)
            ? current
            : new Map(current).set(askId, { issueKey, shown: NOTHING_SAVED })
        );
      }
      return () => {
        const left = (drawn.get(askId) ?? 0) - 1;
        if (left > 0) {
          drawn.set(askId, left);
          return;
        }
        drawn.delete(askId);
        update(askId, (entry) => entry);
      };
    },
    [drawn, update]
  );
  const report = useCallback(
    (askId: string, shown: AssignShown) =>
      update(askId, (entry) =>
        entry.shown.error === shown.error &&
        entry.shown.failed === shown.failed &&
        entry.shown.held === shown.held &&
        entry.shown.pending === shown.pending &&
        entry.shown.retryable === shown.retryable
          ? entry
          : { ...entry, shown }
      ),
    [update]
  );
  const writes = useMemo<AskAssignWrites>(() => {
    const out = new Set<string>();
    const live = new Set<string>();
    for (const [askId, { shown }] of entries) {
      if (shown.pending) out.add(askId);
      if (shown.pending || shown.failed) live.add(askId);
    }
    const writeOf = (askId: string): IssueAssigneeWrite => {
      const write = made.get(askId)?.current;
      if (write === undefined) {
        throw new Error(`Assign to me of ${askId} was pressed before the Inbox drew its write`);
      }
      return write;
    };
    // A press is out from the commit of the press itself, which the write tells the Inbox only in
    // that commit's layout effects: so the press says so here too, and the control is held, and
    // the row kept, from that commit on.
    return {
      draw,
      live,
      of: (askId) => {
        const shown = entries.get(askId)?.shown ?? NOTHING_SAVED;
        return {
          ...shown,
          retry: shown.retryable
            ? () => {
                const retry = writeOf(askId).retry;
                if (retry === undefined) return;
                retry();
                update(askId, (entry) => ({ ...entry, shown: SAVING }));
              }
            : undefined,
          submit: (viewer) => {
            writeOf(askId).submit(viewer);
            update(askId, (entry) => ({ ...entry, shown: SAVING }));
          },
        };
      },
      out,
    };
  }, [draw, entries, made, update]);
  return (
    <>
      {[...entries].map(([askId, { issueKey }]) => (
        <AssignWrite askId={askId} issueKey={issueKey} key={askId} made={made} report={report} />
      ))}
      <AskAssignWritesContext value={writes}>{children}</AskAssignWritesContext>
    </>
  );
}

/** One ask's Assign to me write, which the Inbox draws for the ask (`AskAssignWritesProvider`):
 *  it draws nothing itself, hands the write to the Inbox (`made`), and tells it what the write is
 *  doing (`report`) in the layout effects of each commit that changes that. */
function AssignWrite({
  askId,
  issueKey,
  made,
  report,
}: {
  askId: string;
  issueKey: string;
  made: Map<string, RefObject<IssueAssigneeWrite>>;
  report: (askId: string, shown: AssignShown) => void;
}): ReactNode {
  // Offered on an issue nobody holds, it stands on no assignee, and draws none: its words say
  // whether its write is out, not who holds the issue.
  const write = useIssueAssignee(issueKey, null);
  const latest = useRef(write);
  useLayoutEffect(() => {
    latest.current = write;
  });
  useLayoutEffect(() => {
    made.set(askId, latest);
    return () => {
      made.delete(askId);
    };
  }, [askId, made]);
  const { error, failed, held, pending } = write;
  const retryable = write.retry !== undefined;
  useLayoutEffect(() => {
    report(askId, { error, failed, held, pending, retryable });
  }, [askId, error, failed, held, pending, report, retryable]);
  return null;
}

/** Takes an issue nobody holds for the viewer: one PATCH through the shared assignee write, so
 *  the row is theirs at once and rolls back if the server refuses. The write is not this
 *  control's: the Inbox makes it, by ask (`AskAssignWritesProvider`), and this control shows it
 *  and presses through it. Under the Unassigned filter the optimistic assignee takes the row out
 *  of the list at the press, and the reading column's head that draws this control goes whenever
 *  the reader moves the selection. So the Inbox keeps the row where the reader saw it while the
 *  save is out (`heldRow`), whatever the selection does, and every control drawn for the ask - the
 *  row's own, the head, and the short row beside the column while the column shows another
 *  question - shows `Assigning…`, a refusal and its Retry. A refusal ends the hold at once: the
 *  rollback puts the issue back with nobody holding it, and the next read of the Inbox places the
 *  row. The button is held (`heldPress`) from the press until the commit after its write's
 *  answer (`useHeldThroughAnswer`), or until the write is late (`IssueFieldWrite.held`): a save is
 *  tried under its key until Dispatch decides it, so offline it holds the button no longer than
 *  its deadline, and a press then saves again, behind it on the issue's line, while the button
 *  still says `Assigning…`. The Inbox lets go of the row in the answer's own commit, so a row the
 *  landing takes out of the list takes this button away while it is still held. On a short row
 *  beside the column that is not the item in hand, a press - the button or its Retry - first
 *  takes a focus it left in the row to the item being read (`RowAsideContext`).
 *
 *  Its own module rather than `Inbox.tsx`'s, because the Inbox's reading pane draws it too
 *  (`ReadingPaneHead`): the Inbox mounts the pane, so a control the pane's head imports from the
 *  Inbox would close a cycle between the three files. */
export function AssignToMe({
  askId,
  issueKey,
  shortcutKeys,
  viewer,
}: {
  /** The row this control sits in; the write is the ask's, never the issue's, so a sibling row
   *  of the same issue has a write of its own. */
  askId: string;
  issueKey: string;
  /** The keys that press it, where the host draws them on the button (the reading pane's head,
   *  with a key a person gave it): named in its `aria-keyshortcuts`, the first drawn after its
   *  label (`ControlKeyHint`). */
  shortcutKeys?: readonly string[];
  viewer: string;
}): ReactNode {
  const writes = useInboxWrites(AskAssignWritesContext);
  const { draw } = writes;
  useLayoutEffect(() => draw(askId, issueKey), [askId, draw, issueKey]);
  const write = writes.of(askId);
  const held = useHeldThroughAnswer(write.held);
  const aside = useContext(RowAsideContext);
  const retry = write.retry;
  return (
    <>
      <button
        aria-keyshortcuts={keyshortcutsOf(shortcutKeys)}
        aria-label={`Assign ${issueKey} to me`}
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        {...inboxActionMark("inbox.assign-to-me")}
        {...heldPress(held, () => {
          aside?.();
          write.submit(viewer);
        })}
        type="button"
      >
        {write.pending ? "Assigning…" : "Assign to me"}
        <ControlKeyHint keys={shortcutKeys} />
      </button>
      {write.failed ? (
        <div className="basis-full">
          <QueryError
            message={write.error ?? `Could not assign ${issueKey} to you.`}
            onRetry={
              retry === undefined
                ? undefined
                : () => {
                    aside?.();
                    retry();
                  }
            }
            retrying={write.pending}
          />
        </div>
      ) : null}
    </>
  );
}
