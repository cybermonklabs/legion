import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectIssuesQuery, userStateQuery, whoAmIQuery } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, pillClassName, StatusPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  cardHoverBorder,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  selectedCardBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { useKeymap } from "../shell/keymap";
import { closestMatching, reachableRows, roveFocus } from "../shell/roving";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

/** What each row is marked with, for the keys that rove them. */
const ROW_SELECTOR = "[data-issue-row]";

/** How the List groups its rows. `status` is the List as it was; the others are the PROTOTYPE of
 *  dispatch://INFRA-1508: one issues view that groups by epic, shows the queue, or groups by
 *  assignee, instead of a separate view for each. */
export type ListGrouping = "status" | "epic" | "queue" | "assignee";
export const listGroupings: readonly { id: ListGrouping; label: string }[] = [
  { id: "status", label: "Status" },
  { id: "epic", label: "Epic" },
  { id: "queue", label: "Queue" },
  { id: "assignee", label: "Assignee" },
];

/** Legion's waiting line as Dispatch holds it: issues in `todo` handed to Legion, in board order,
 *  which is the order Legion starts them. An epic is an open root issue labelled `epic`. */
const isQueued = (issue: IssueSummary) =>
  issue.status === "todo" && (issue.labels ?? []).includes("legion");
const isEpic = (issue: IssueSummary) =>
  issue.parent === null && (issue.labels ?? []).includes("epic") && issue.status !== "done";
const RUNNING: ReadonlySet<string> = new Set(["in_progress", "testing", "needs_review", "retro"]);

interface RowExtras {
  /** The issue's place in the waiting line, when it is in it. */
  readonly position?: number;
  /** Show the status pill: a grouping other than by status says nothing about it. */
  readonly showStatus?: boolean;
  readonly selected?: boolean;
  readonly onToggle?: () => void;
  readonly onStartNext?: () => void;
  /** Just moved by the person: drawn highlighted so they see where it landed. */
  readonly moved?: boolean;
}

/** A row is a keyboard target for the `j`/`k`/`o`/`Enter` this file registers: `tabIndex={-1}`
 *  so those keys reach it and Tab does not, and `data-issue-row` names it for them, as the board's
 *  cards do. The focus ring is the row's own, since the row is what focus lands on. */
function IssueRow({
  issue,
  unread,
  position,
  showStatus,
  selected,
  onToggle,
  onStartNext,
  moved,
}: { issue: IssueSummary; unread: boolean } & RowExtras): ReactNode {
  return (
    <li
      aria-label={`${issue.key} ${issue.title}`}
      className={`group flex items-start gap-3 border-t py-3 outline-none first:border-t-0 focus-visible:ring-2 ${borderDefault} ${focusVisibleRing} ${moved ? `${selectedCardBg} -mx-4 px-4` : ""}`}
      data-issue-row={issue.key}
      tabIndex={-1}
    >
      {onToggle === undefined ? null : (
        <input
          aria-label={`Select ${issue.key}`}
          checked={selected ?? false}
          className={`mt-1 shrink-0 ${checkboxAccent}`}
          onChange={onToggle}
          type="checkbox"
        />
      )}
      {position === undefined ? null : (
        <span
          className={`w-9 shrink-0 text-right text-sm font-semibold tabular-nums ${textSecondaryOnCanvas}`}
          title="Place in the queue: Legion starts issues in this order"
        >
          #{position}
        </span>
      )}
      {/* One grid, two arrangements. From `sm` the reference and the timestamp share the first
          line and the metadata runs below. On a phone the reference takes the whole first line
          and the timestamp moves down beside the metadata: beside the title it would leave the
          title a column about 130 px wide. */}
      <div className="grid min-w-0 flex-1 grid-cols-[minmax(0,1fr)_auto] items-start gap-2">
        <Link
          className={`col-span-2 row-start-1 min-w-0 text-sm sm:col-span-1 ${linkText} ${linkHoverText}`}
          data-issue-link
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
        >
          {/* Below 1280 px a link is an inline-flex box (`styles.css`), so the key and the title
              would be two flex columns. One wrapper keeps them one run of text that wraps across
              the row, and the key never breaks at its hyphen. */}
          <span className="min-w-0 break-words">
            <span className="font-semibold whitespace-nowrap">{issue.key}</span>
            <span className={`ml-2 ${textPrimaryOnCanvas}`}>{issue.title}</span>
          </span>
        </Link>
        <div
          className={`col-start-2 row-start-2 flex items-center gap-2 self-center text-xs sm:row-start-1 sm:self-start ${textMutedOnCanvas}`}
        >
          {onStartNext === undefined ? null : (
            <button
              className={`rounded-md border px-2 py-0.5 opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 ${borderDefault} ${surfaceMutedHoverBg} ${textSecondaryOnCanvas}`}
              onClick={onStartNext}
              type="button"
            >
              Start next
            </button>
          )}
          {unread ? <UnreadDot /> : null}
          {issue.open_asks === 0 ? null : <AttentionBadge count={issue.open_asks} />}
          <Timestamp at={issue.updated_at} />
        </div>
        <div className="col-start-1 row-start-2 flex min-w-0 flex-wrap items-center gap-2 sm:col-span-2">
          {showStatus ? <StatusPill>{statusLabel(issue.status as IssueStatus)}</StatusPill> : null}
          <PriorityControl
            disabled={issue.status === "done"}
            issueKey={issue.key}
            priority={issue.priority}
          />
          <ClaimChip claim={issue.claim} />
          <ProgressChips progress={issue.progress} />
          <UnreachableRouteMarker issue={issue} />
          {(issue.labels ?? []).map((label) => (
            <LabelPill key={label}>{label}</LabelPill>
          ))}
          {issue.parent === null ? null : (
            <Link
              className={`${pillClassName("label")} border ${borderDefault} ${cardHoverBorder}`}
              to={buildIssuePath({ key: issue.parent, kind: "issue" })}
              {...referenceTriggerProps({ key: issue.parent, kind: "issue" })}
            >
              {issue.parent}
            </Link>
          )}
        </div>
      </div>
    </li>
  );
}

interface Group {
  readonly id: string;
  readonly label: string;
  readonly issues: IssueSummary[];
  /** The epic this group is, when grouped by epic. */
  readonly epic?: IssueSummary;
  readonly open?: boolean;
  readonly note?: string;
}

/** Splits the rows into the bands the chosen grouping shows, every band in board order. */
function groupRows(
  grouping: ListGrouping,
  visible: readonly IssueSummary[],
  all: readonly IssueSummary[],
  position: ReadonlyMap<string, number>
): Group[] {
  if (grouping === "status") {
    return issueStatuses.map((status) => ({
      id: status,
      label: statusLabel(status),
      issues: visible.filter((issue) => issue.status === status),
      open: status !== "done",
    }));
  }
  if (grouping === "queue") {
    const queued = visible
      .filter((issue) => position.has(issue.key))
      .sort((a, b) => (position.get(a.key) ?? 0) - (position.get(b.key) ?? 0));
    return [
      {
        id: "queued",
        label: "Waiting to start, in the order Legion starts them",
        issues: queued,
        open: true,
      },
      {
        id: "running",
        label: "Running",
        issues: visible.filter((issue) => RUNNING.has(issue.status)),
        open: false,
      },
      {
        id: "rest",
        label: "Not in the queue",
        issues: visible.filter(
          (issue) => !position.has(issue.key) && !RUNNING.has(issue.status) && !isEpic(issue)
        ),
        open: false,
        note: "Triage, backlog, icebox and done, and todo issues not handed to Legion.",
      },
    ];
  }
  // Inside an epic or a person's band: queued issues first in the order they start, then running,
  // then the rest in board order.
  const startOrder = (issues: IssueSummary[]) =>
    issues.sort((a, b) => {
      const rankOf = (issue: IssueSummary) =>
        position.get(issue.key) ?? (RUNNING.has(issue.status) ? 1e6 : 2e6);
      return rankOf(a) - rankOf(b);
    });
  if (grouping === "assignee") {
    const byPerson = new Map<string, IssueSummary[]>();
    for (const issue of visible) {
      const who = issue.assignee ?? "Unassigned";
      byPerson.set(who, [...(byPerson.get(who) ?? []), issue]);
    }
    return [...byPerson.entries()]
      .sort((a, b) => b[1].length - a[1].length)
      .map(([who, issues]) => ({ id: who, label: who, issues: startOrder(issues), open: true }));
  }
  // By epic: each issue under the epic at the root of its parent chain.
  const byKey = new Map(all.map((issue) => [issue.key, issue]));
  const epicOf = (issue: IssueSummary): IssueSummary | undefined => {
    let current: IssueSummary | undefined = issue;
    for (let depth = 0; current?.parent && depth < 30; depth++) current = byKey.get(current.parent);
    return current !== undefined && current !== issue && isEpic(current) ? current : undefined;
  };
  const epics = all.filter(isEpic);
  const members = new Map<string, IssueSummary[]>(epics.map((epic) => [epic.key, []]));
  const loose: IssueSummary[] = [];
  for (const issue of visible) {
    if (isEpic(issue)) continue;
    const epic = epicOf(issue);
    if (epic === undefined) loose.push(issue);
    else members.get(epic.key)?.push(issue);
  }
  // Epics run side by side, so none is "first": the one whose next issue starts soonest leads.
  const nextStart = (epic: IssueSummary) =>
    Math.min(...(members.get(epic.key) ?? []).map((issue) => position.get(issue.key) ?? 1e9));
  const groups: Group[] = epics
    .map((epic) => ({
      id: epic.key,
      label: epic.title,
      issues: startOrder(members.get(epic.key) ?? []),
      epic,
      open: false,
    }))
    .sort((a, b) => nextStart(a.epic) - nextStart(b.epic));
  groups.push({
    id: "no-epic",
    label: "In no epic",
    issues: loose,
    open: loose.length > 0,
    note: loose.length === 0 ? "Every open issue is under an epic." : undefined,
  });
  return groups;
}

/** One epic's issues counted by where they stand, drawn as one bar with its numbers beside it. */
function StatusBar({ issues }: { issues: readonly IssueSummary[] }): ReactNode {
  const running = issues.filter((issue) => RUNNING.has(issue.status)).length;
  const waiting = issues.filter(isQueued).length;
  const other = issues.length - running - waiting;
  return (
    <span className={`flex items-center gap-3 text-xs ${textMutedOnCanvas}`}>
      <span
        aria-hidden
        className={`flex h-1.5 w-28 overflow-hidden rounded-full ${surfaceMutedBg}`}
      >
        <span className="bg-sky-600 dark:bg-sky-400" style={{ flex: running }} />
        <span className="bg-sky-300 dark:bg-sky-700" style={{ flex: waiting }} />
        <span style={{ flex: other }} />
      </span>
      <span className="tabular-nums whitespace-nowrap">
        {running} running · {waiting} waiting · {other} other
      </span>
    </span>
  );
}

export function IssueList({
  project,
  grouping = "status",
}: {
  project: string;
  grouping?: ListGrouping;
}): ReactNode {
  const { labels, matches, statuses } = useIssueFilters();
  const queryClient = useQueryClient();
  const login = useQuery(whoAmIQuery()).data?.login;
  const allIssues = useQuery(projectIssuesQuery(project, []));
  const filteredIssues = useQuery({
    ...projectIssuesQuery(project, labels),
    enabled: labels.length > 0,
  });
  const issues = labels.length === 0 ? allIssues : filteredIssues;
  const state = useQuery(userStateQuery());
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);
  const visibleIssues = useMemo(
    () =>
      (issues.data ?? []).filter(
        (issue) =>
          (statuses.length === 0 || statuses.includes(issue.status)) &&
          matches(issue, state.data?.[issue.key]?.last_read_seq ?? 0)
      ),
    [issues.data, matches, state.data, statuses]
  );
  // The queue is the whole project's, whatever the filters hide: a place means the same thing
  // filtered or not.
  const queue = useMemo(
    () =>
      (allIssues.data ?? [])
        .filter(isQueued)
        .sort((a, b) => (a.rank < b.rank ? -1 : a.rank > b.rank ? 1 : 0)),
    [allIssues.data]
  );
  const position = useMemo(
    () => new Map(queue.map((issue, index) => [issue.key, index + 1])),
    [queue]
  );
  const groups = useMemo(
    () => groupRows(grouping, visibleIssues, allIssues.data ?? [], position),
    [grouping, visibleIssues, allIssues.data, position]
  );
  const epics = useMemo(() => (allIssues.data ?? []).filter(isEpic), [allIssues.data]);
  const titleOf = (key: string) =>
    (allIssues.data ?? []).find((issue) => issue.key === key)?.title ?? key;

  // The keys that rove these rows register with the page's `project` scope, and they live
  // here because this component mounts only while the List is the view showing: on the Board,
  // whose own `j`/`k`/`o`/`Enter` act on its cards, `?` would otherwise list each key twice.
  // `roveFocus` and these `when`s read one rule for which rows count, so a key is offered
  // exactly while it moves: the rows of a collapsed status band are in the DOM, take no focus,
  // and are no step.
  const rows = () => [...document.querySelectorAll<HTMLElement>(ROW_SELECTOR)];
  const focusedRow = () => closestMatching(document.activeElement, ROW_SELECTOR);
  // The title link, by its mark: the row holds other links (a claim's agent, the parent), and
  // `o`/`Enter` must open the issue whichever comes first.
  const openFocusedRow = () =>
    focusedRow()?.querySelector<HTMLElement>("[data-issue-link]")?.click();
  useKeymap("project", [
    // Movement: a palette row that moves the cursor helps nobody.
    {
      id: "list-next",
      keys: "j",
      label: "Next issue",
      palette: false,
      run: () => roveFocus(rows(), focusedRow(), 1),
      when: () => reachableRows(rows()).length > 0,
    },
    {
      id: "list-previous",
      keys: "k",
      label: "Previous issue",
      palette: false,
      run: () => roveFocus(rows(), focusedRow(), -1),
      when: () => reachableRows(rows()).length > 0,
    },
    {
      id: "list-open",
      keys: "o",
      label: "Open issue",
      run: openFocusedRow,
      when: () => focusedRow() !== null,
    },
    {
      // Only from the row itself: Enter on its title link is the browser's own navigation. Out of
      // the palette, where `list-open` above is already the row for the same action.
      id: "list-open-enter",
      keys: "Enter",
      label: "Open the focused issue",
      palette: false,
      run: openFocusedRow,
      when: () => document.activeElement?.matches(ROW_SELECTOR) === true,
    },
  ]);

  /** Each action is one or more ordinary issue writes - the same an agent or the command bar
   *  makes - then one reload of the list. */
  const run = async (
    said: string,
    writes: (() => Promise<unknown>)[],
    highlight: readonly string[] = []
  ) => {
    setBusy(true);
    try {
      for (const write of writes) await write();
      setNotice(said);
      setMoved(new Set(highlight));
      setSelected(new Set());
    } catch (cause) {
      setNotice(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
      await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
    }
  };
  const patch = (key: string, input: UpdateIssueInput) => () =>
    api.patchIssue(key, input, { key: crypto.randomUUID(), login });
  const named = (keys: readonly string[]) =>
    keys.length === 1 ? `${keys[0]} “${titleOf(keys[0] ?? "")}”` : `${keys.length} issues`;
  /** Puts the issues at the head of the queue, in the order given. */
  const startNext = (keys: readonly string[]) => {
    const queued = keys.filter((key) => position.has(key));
    const head = queue.find((issue) => !queued.includes(issue.key));
    if (queued.length === 0 || head === undefined) return;
    void run(
      `${named(queued)} now ${queued.length === 1 ? "starts" : "start"} next.`,
      [...queued].reverse().map((key, index, reversed) =>
        patch(key, {
          rank: { before: index === 0 ? head.key : (reversed[index - 1] ?? head.key) },
        })
      ),
      queued
    );
  };
  const moveTo = (keys: readonly string[], epic: string | null) =>
    run(
      epic === null
        ? `Took ${named(keys)} out of ${keys.length === 1 ? "its epic" : "their epics"}.`
        : `Moved ${named(keys)} to the epic “${titleOf(epic)}”.`,
      keys.map((key) => patch(key, { parent: epic })),
      keys
    );
  const newEpic = async (keys: readonly string[]) => {
    const title = window.prompt(`Name the new epic for ${named(keys)}`);
    if (!title) return;
    let created = "";
    await run(
      `Made the epic “${title}” from ${named(keys)}.`,
      [
        async () => {
          created = (
            await api.createIssue(
              { project, title, labels: ["epic"], force: true },
              { key: crypto.randomUUID(), login }
            )
          ).key;
        },
        ...keys.map((key) => async () => patch(key, { parent: created })()),
      ],
      keys
    );
  };
  const renameEpic = (epic: IssueSummary) => {
    const title = window.prompt("Rename the epic", epic.title);
    if (title && title !== epic.title) {
      void run(`Renamed the epic to “${title}”.`, [patch(epic.key, { title })]);
    }
  };
  const mergeEpic = (from: IssueSummary, into: string, members: readonly IssueSummary[]) =>
    run(
      `Merged “${from.title}” into “${titleOf(into)}”.`,
      [
        ...members
          .filter((issue) => issue.parent === from.key)
          .map((issue) => patch(issue.key, { parent: into })),
        // A close needs its reason posted first, as `dispatch issue-update --status done` does.
        () =>
          api.createMessage(
            from.key,
            { body: `Merged into ${into} (“${titleOf(into)}”) from the issue list.` },
            { key: crypto.randomUUID(), login }
          ),
        patch(from.key, { status: "done" }),
      ],
      members.map((issue) => issue.key)
    );
  const toggle = (key: string) => {
    const next = new Set(selected);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    setSelected(next);
  };

  if (issues.isPending || state.isPending) {
    return <LoadingSkeleton label="Loading issues" />;
  }
  if (issues.isError || state.isError) {
    return <p className={dangerText}>Could not load issues.</p>;
  }
  const shaping = grouping !== "status";
  const selectedKeys = [...selected];

  return (
    <section aria-label="Project issues" className={shaping ? "pb-24" : undefined}>
      {notice === undefined ? null : (
        <p aria-live="polite" className={`mb-3 text-sm ${textSecondaryOnCanvas}`}>
          {notice}
        </p>
      )}
      {groups.map((group) => {
        if (group.issues.length === 0 && group.note === undefined) {
          return null;
        }
        const epic = group.epic;
        const next = group.issues
          .map((issue) => position.get(issue.key))
          .filter((place) => place !== undefined)
          .sort((a, b) => a - b)[0];
        return (
          // A band is a divider, not a card: a bordered box with 16 px of padding around one
          // or two rows spent more height on the grouping than on the issues. The label is the
          // list's section-label role, quieter than the rows it introduces.
          <details
            aria-label={`${group.label} (${group.issues.length})`}
            className="mb-4"
            key={`${grouping}-${group.id}`}
            open={group.open || group.issues.some((issue) => moved.has(issue.key))}
          >
            {/* No `flex`: it would set `display: flex` on the summary and take the native
                disclosure marker with it, leaving a collapsed band looking like a plain
                heading with nothing to say it opens. */}
            {epic === undefined ? (
              <summary
                className={`min-h-11 cursor-pointer py-3.5 text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}
              >
                {group.label} ({group.issues.length})
              </summary>
            ) : (
              <summary className={`min-h-11 cursor-pointer py-2.5 ${textPrimaryOnCanvas}`}>
                <span className="inline-flex w-[calc(100%-1.5rem)] flex-wrap items-center gap-x-4 gap-y-1 align-middle">
                  <span className="min-w-0 flex-1 text-sm font-semibold">
                    {group.label}{" "}
                    <span className={`font-normal ${textMutedOnCanvas}`}>
                      {epic.key} · {group.issues.length} open
                      {next === undefined ? "" : ` · next starts #${next}`}
                    </span>
                  </span>
                  <StatusBar issues={group.issues} />
                  <EpicMenu
                    epic={epic}
                    epics={epics}
                    onMerge={(into) => void mergeEpic(epic, into, group.issues)}
                    onRename={() => renameEpic(epic)}
                    onStartAll={() => startNext(group.issues.map((issue) => issue.key))}
                  />
                </span>
              </summary>
            )}
            {group.issues.length === 0 ? (
              <p
                className={`rounded-xl border px-4 py-3 text-sm ${borderDefault} ${textMutedOnCanvas}`}
              >
                {group.note}
              </p>
            ) : (
              <ul
                aria-label={`${group.label} issues`}
                className={`rounded-xl border px-4 ${borderDefault}`}
              >
                {group.note === undefined ? null : (
                  <li className={`py-2 text-xs ${textMutedOnCanvas}`}>{group.note}</li>
                )}
                {group.issues.map((issue) => (
                  <IssueRow
                    issue={issue}
                    key={issue.key}
                    moved={moved.has(issue.key)}
                    onStartNext={
                      shaping && position.has(issue.key) && position.get(issue.key) !== 1
                        ? () => startNext([issue.key])
                        : undefined
                    }
                    onToggle={shaping ? () => toggle(issue.key) : undefined}
                    position={shaping ? position.get(issue.key) : undefined}
                    selected={selected.has(issue.key)}
                    showStatus={shaping}
                    unread={issueIsUnread(issue, state.data?.[issue.key]?.last_read_seq ?? 0)}
                  />
                ))}
              </ul>
            )}
          </details>
        );
      })}
      {visibleIssues.length === 0 ? (
        <EmptyState label="No matching project issues" message="No issues match these filters." />
      ) : null}
      {selectedKeys.length === 0 ? null : (
        <div
          className={`fixed bottom-4 left-1/2 z-20 flex -translate-x-1/2 flex-wrap items-center gap-2 rounded-2xl border px-4 py-2 text-sm shadow-lg ${card} ${textPrimaryOnCanvas}`}
          role="toolbar"
        >
          <span className="mr-1 font-semibold">{selectedKeys.length} selected</span>
          <BarButton
            disabled={busy || !selectedKeys.some((key) => position.has(key))}
            onClick={() => startNext(selectedKeys)}
          >
            Start next
          </BarButton>
          <select
            aria-label="Move to epic"
            className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceMutedBg}`}
            disabled={busy}
            onChange={(event) => {
              if (event.target.value) void moveTo(selectedKeys, event.target.value);
            }}
            value=""
          >
            <option value="">Move to epic…</option>
            {epics.map((candidate) => (
              <option key={candidate.key} value={candidate.key}>
                {candidate.title}
              </option>
            ))}
          </select>
          <BarButton disabled={busy} onClick={() => void moveTo(selectedKeys, null)}>
            Take out of epic
          </BarButton>
          <BarButton disabled={busy} onClick={() => void newEpic(selectedKeys)}>
            New epic from these
          </BarButton>
          <BarButton disabled={busy} onClick={() => setSelected(new Set())}>
            Clear
          </BarButton>
        </div>
      )}
    </section>
  );
}

function EpicMenu({
  epic,
  epics,
  onRename,
  onMerge,
  onStartAll,
}: {
  epic: IssueSummary;
  epics: readonly IssueSummary[];
  onRename: () => void;
  onMerge: (into: string) => void;
  onStartAll: () => void;
}): ReactNode {
  return (
    // A menu inside the band's summary: its clicks must not open or close the band.
    // biome-ignore lint/a11y/noStaticElementInteractions: stops the summary's own toggle only
    // biome-ignore lint/a11y/useKeyWithClickEvents: keyboard reaches the buttons inside
    <span className="relative" onClick={(event) => event.preventDefault()}>
      <details className="inline-block">
        <summary
          aria-label={`Actions for ${epic.title}`}
          className={`cursor-pointer list-none rounded-md border px-2 py-0.5 text-xs ${borderDefault} ${surfaceMutedHoverBg} ${textSecondaryOnCanvas}`}
        >
          Epic ▾
        </summary>
        <span
          className={`absolute right-0 z-10 mt-1 flex w-64 flex-col rounded-lg border p-1 text-sm shadow-lg ${card}`}
        >
          <MenuButton onClick={onRename}>Rename</MenuButton>
          <MenuButton onClick={onStartAll}>Start all its waiting issues next</MenuButton>
          <label className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
            Merge into
            <select
              className={`mt-1 block w-full rounded-md border px-1 py-1 text-sm ${borderDefault} ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
              onChange={(event) => {
                if (event.target.value) onMerge(event.target.value);
              }}
              value=""
            >
              <option value="">Choose an epic…</option>
              {epics
                .filter((candidate) => candidate.key !== epic.key)
                .map((candidate) => (
                  <option key={candidate.key} value={candidate.key}>
                    {candidate.title}
                  </option>
                ))}
            </select>
          </label>
          <span className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
            To split it, tick some of its issues and choose “New epic from these”.
          </span>
        </span>
      </details>
    </span>
  );
}

function MenuButton({
  children,
  onClick,
}: {
  children: ReactNode;
  onClick: () => void;
}): ReactNode {
  return (
    <button
      className={`rounded-md px-2 py-1 text-left ${surfaceMutedHoverBg}`}
      onClick={(event) => {
        event.currentTarget.closest("details")?.removeAttribute("open");
        onClick();
      }}
      type="button"
    >
      {children}
    </button>
  );
}

function BarButton({
  children,
  disabled,
  onClick,
}: {
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
}): ReactNode {
  return (
    <button
      className={`rounded-lg border px-3 py-1 disabled:opacity-50 ${borderDefault} ${surfaceMutedHoverBg}`}
      disabled={disabled}
      onClick={onClick}
      type="button"
    >
      {children}
    </button>
  );
}
