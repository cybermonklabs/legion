import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  MouseSensor,
  pointerWithin,
  TouchSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import { useQueryClient } from "@tanstack/react-query";
import { type CSSProperties, type ReactNode, useCallback, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { ApiError, issuePatchRequest } from "../../api/client";
import { boardMoveMutationKey } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, Pill } from "../../components/Pill";
import {
  borderDefault,
  borderStrong,
  card,
  cardHoverBorder,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { formatPhaseTime, formatWorkTime } from "../legion/lib/format";
import { closeRefPreview, referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { laneLabel, type PriorityLane, priorityLanes } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";
import { CARD_STAGES, type StageId, stageIds, stageLabel, stageStatus } from "./issues-stages";
import { StageMark } from "./issues-table-columns";
import type { Item } from "./issues-table-model";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

const boardCollisionDetection: CollisionDetection = (args) => {
  const pointerCollisions = pointerWithin(args);
  return pointerCollisions.length > 0 ? pointerCollisions : closestCorners(args);
};

const boardFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

/** One fact of a card with Legion work: its name, then its value. */
function Fact({ hook, label, value }: { hook: string; label: string; value: string }): ReactNode {
  return (
    <span className={`inline-flex gap-1 ${textSecondaryOnSurface}`} data-legion-fact={hook}>
      <span className={textMutedOnSurface}>{label}</span>
      <span>{value}</span>
    </span>
  );
}

/** What a card with a card on the Legion board shows beside today's card: every field the
 *  Legion view's card showed, from the board route. */
function LegionFacts({ item, nowMs }: { item: Item; nowMs: number }): ReactNode {
  const legion = item.card?.card;
  if (legion === undefined) return null;
  const roles = legion.roles ?? [];
  const blockedBy = legion.blocked_by ?? legion.waiting?.blocked_by ?? [];
  return (
    <>
      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <Fact
          hook="phase-time"
          label="In phase"
          value={formatPhaseTime(legion.phase_since, nowMs)}
        />
        {item.facts === undefined ? null : (
          <Fact hook="work-time" label="Work time" value={formatWorkTime(item.facts.work_hours)} />
        )}
        <Fact
          hook="slot"
          label="Slot"
          value={legion.slot === null ? "none" : String(legion.slot)}
        />
        <Fact
          hook="roles"
          label="In a turn"
          value={roles.length === 0 ? "nobody" : roles.join(", ")}
        />
        <Fact hook="checks" label="CI" value={legion.checks_verdict ?? "no verdict"} />
      </div>
      {blockedBy.length === 0 ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="blocked-by"
        >
          Blocked by {blockedBy.join(", ")}
        </p>
      )}
      {legion.waiting_on === null ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="waiting-on"
        >
          Waiting on {legion.waiting_on.kind}
          {legion.waiting_on.since === null
            ? ""
            : ` since ${new Date(legion.waiting_on.since).toLocaleString()}`}
          {legion.waiting_on.answered_at === null ? "" : ", answered"}
        </p>
      )}
      {legion.children === 0 ? null : (
        <p className={`mt-2 text-xs ${textMutedOnSurface}`} data-legion-fact="children">
          {legion.children} {legion.children === 1 ? "issue" : "issues"} of this tree not started
        </p>
      )}
    </>
  );
}

/** One card: today's card (key, title, priority, claim, progress, labels, open asks, unread),
 *  and for an issue with a card on the Legion board its Legion fields. The whole card is the
 *  drag handle: an 8 px mouse move or a 150 ms touch hold lifts it. */
function BoardCard({
  item,
  nowMs,
  unread,
}: {
  item: Item;
  nowMs: number;
  unread: boolean;
}): ReactNode {
  const { issue } = item;
  const { attributes, isDragging, listeners, setNodeRef, transform } = useDraggable({
    attributes: { role: "article", roleDescription: "card", tabIndex: -1 },
    id: issue.key,
  });
  const drop = useDroppable({ id: `card:${issue.key}` });
  const setRefs = useCallback(
    (node: HTMLElement | null) => {
      setNodeRef(node);
      drop.setNodeRef(node);
    },
    [setNodeRef, drop.setNodeRef]
  );
  const style: CSSProperties = {
    transform:
      transform === null ? undefined : `translate3d(${transform.x}px, ${transform.y}px, 0)`,
  };
  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`cursor-grab rounded-xl border p-3 ${card} ${cardHoverBorder} ${boardFocusRing} ${
        isDragging ? "relative z-50 opacity-70" : ""
      } ${drop.isOver && !isDragging ? selectedCardBorder : ""}`}
      data-board-card={issue.key}
      ref={setRefs}
      style={style}
      {...attributes}
      {...listeners}
    >
      <Link
        className={`flex flex-col text-sm ${linkText} ${linkHoverText}`}
        data-issue-link
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
        {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
      >
        {/* The unread dot sits beside the key, as it does in the table's Key cell, so a card
            with no chips below shows no lone dot. */}
        <span className="inline-flex items-center gap-1.5 self-start font-semibold">
          {issue.key}
          {unread ? <UnreadDot /> : null}
          {item.place === undefined ? null : (
            <span className={`font-normal ${textMutedOnSurface}`}>#{item.place}</span>
          )}
        </span>
        <span className={`mt-1 self-start ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Link>
      <div className="mt-3 flex flex-wrap items-center gap-2 empty:hidden">
        {/* A card with no priority shows none: the badge would read as a word with no value. */}
        {issue.priority === null ? null : (
          <PriorityControl
            disabled={issue.status === "done"}
            issueKey={issue.key}
            priority={issuePriority(issue.priority)}
          />
        )}
        <ClaimChip claim={issue.claim} />
        <ProgressChips progress={issue.progress} />
        <UnreachableRouteMarker issue={issue} />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks === 0 ? null : (
          <span title={`${issue.open_asks} open asks`}>
            <AttentionBadge count={issue.open_asks} />
          </span>
        )}
      </div>
      <LegionFacts item={item} nowMs={nowMs} />
    </article>
  );
}

/** One stage's column: its title is the stage's label, and the whole column is its drop
 *  target. */
function StageColumn({
  dropId,
  items,
  nowMs,
  stage,
  userState,
}: {
  dropId: string;
  items: readonly Item[];
  nowMs: number;
  stage: StageId;
  userState: UserState | undefined;
}): ReactNode {
  const { isOver, setNodeRef } = useDroppable({ id: dropId });
  return (
    <section
      aria-label={stageLabel(stage)}
      className={`w-72 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={stage}
      ref={setNodeRef}
      tabIndex={-1}
    >
      <header
        className={`min-h-[44px] rounded-xl border px-3 py-2 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-header"
      >
        <div
          className={`flex items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
        >
          <StageMark stage={stage} />
          <Pill>{items.length}</Pill>
        </div>
      </header>
      {items.length === 0 ? (
        <div
          className={`mt-3 min-h-32 rounded-xl border border-dashed px-3 py-6 text-center text-xs ${textMutedOnCanvas} ${
            isOver ? selectedCardBorder : borderStrong
          }`}
        >
          Nothing here
        </div>
      ) : (
        <div className="mt-3 space-y-3">
          {items.map((item) => (
            <BoardCard
              item={item}
              key={item.issue.key}
              nowMs={nowMs}
              unread={issueIsUnread(item.issue, userState?.[item.issue.key]?.last_read_seq ?? 0)}
            />
          ))}
        </div>
      )}
    </section>
  );
}

/** A collapsed rail: Icebox while the reader has not turned it on, and Done while the view
 *  lists no done issue. It is a drop target like a column: a card dropped on the Done rail
 *  closes the issue. */
function Rail({
  count,
  dropId,
  stage,
}: {
  count: number | undefined;
  dropId: string;
  stage: StageId;
}): ReactNode {
  const { isOver, setNodeRef } = useDroppable({ id: dropId });
  return (
    <section
      aria-label={`${stageLabel(stage)} (collapsed)`}
      className={`w-10 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={stage}
      ref={setNodeRef}
      tabIndex={-1}
    >
      <div
        className={`flex h-full min-h-[52px] flex-col items-center gap-3 rounded-xl border px-1 py-3 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-rail"
      >
        {count === undefined ? null : <Pill>{count}</Pill>}
        <span
          className={`text-sm font-semibold tracking-wide ${textPrimaryOnSurface}`}
          style={{ writingMode: "vertical-rl" }}
        >
          {stageLabel(stage)}
        </span>
      </div>
    </section>
  );
}

/**
 * The Board's columns, in stage order: each stage the view's Stage filter names; where it names
 * none, the stage of each status its Status filter picks (Done is the rail); and every other
 * stage one of its rows is in. Icebox is a column only when the reader turns it on, and a rail
 * otherwise. An empty Legion stage is drawn only in a project with a Legion daemon.
 */
export function boardStages({
  hasLegion,
  rows,
  showIcebox,
  stagePicks,
  statusPicks,
}: {
  hasLegion: boolean;
  rows: readonly Item[];
  showIcebox: boolean;
  stagePicks: readonly string[] | undefined;
  statusPicks: readonly string[] | undefined;
}): StageId[] {
  const named = new Set<StageId>();
  const picks = stagePicks ?? statusPicks ?? [];
  for (const pick of picks) {
    const stage = stageIds.find((id) => id === pick);
    if (stage === undefined || stage === "done") continue;
    if (!hasLegion && CARD_STAGES.includes(stage)) continue;
    named.add(stage);
  }
  for (const row of rows) if (row.stage !== "done") named.add(row.stage);
  return stageIds.filter(
    (stage) => named.has(stage) && (stage !== "icebox" || showIcebox || stagePicks !== undefined)
  );
}

/** A column's drop id: its stage, inside a lane where the board has lanes. */
const dropIdOf = (stage: StageId, lane: string | undefined) =>
  lane === undefined ? `stage:${stage}` : `lane:${lane}|stage:${stage}`;

/** Where a drop lands: a column by its stage (and lane), a card by its key, or nothing the board
 *  knows. */
function readDrop(
  overId: string,
  rows: readonly Item[]
): { stage: StageId; key?: string; lane?: PriorityLane } | undefined {
  if (overId.startsWith("card:")) {
    const key = overId.slice("card:".length);
    const row = rows.find((item) => item.issue.key === key);
    return row === undefined
      ? undefined
      : { stage: row.stage, key, lane: issuePriority(row.issue.priority) };
  }
  const match = /^(?:lane:([^|]+)\|)?stage:(.+)$/.exec(overId);
  const stage = stageIds.find((id) => id === match?.[2]);
  if (match === null || stage === undefined) return undefined;
  const lane = priorityLanes.find((candidate) => String(candidate) === match[1]);
  return { stage, ...(match[1] === undefined ? {} : { lane: lane ?? null }) };
}

/**
 * The Board: a layout of the current view. It draws every row the view lists, in columns by
 * stage, the cards in each column in the view's sort. A drop on a status column, or on the Done
 * rail, sets that status from any column; Testing, a status as well as a card stage, takes a
 * drop as a status column. A card dropped from a Legion stage column stays there until the
 * daemon's next read moves it. A drop within the Queue column writes the rank, and the card
 * takes its new place when the daemon's next read moves its Place. Any other drop on a Legion
 * stage column is refused, because the daemon owns those stages.
 */
export function IssueBoard({
  hasLegion,
  lanes,
  listsDone,
  project,
  rows,
  showIcebox,
  stagePicks,
  statusPicks,
  userState,
}: {
  hasLegion: boolean;
  lanes: boolean;
  /** The view lists done issues: the Done rail opens into a column with their cards. */
  listsDone: boolean;
  project: string;
  /** The view's rows, in the view's sort. */
  rows: readonly Item[];
  showIcebox: boolean;
  stagePicks: readonly string[] | undefined;
  statusPicks: readonly string[] | undefined;
  userState: UserState | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<string>();
  const nowMs = Date.now();
  const stages = useMemo(
    () => boardStages({ hasLegion, rows, showIcebox, stagePicks, statusPicks }),
    [hasLegion, rows, showIcebox, stagePicks, statusPicks]
  );
  const patch = useKeyedWrite({
    inline: true,
    label: ({ key }: { input: UpdateIssueInput; key: string }) => `${key} moved on the board`,
    target: ({ key }) => `issue:${key}`,
    request: ({ input, key }) => issuePatchRequest(key, input),
    mutationKey: boardMoveMutationKey(project),
    scope: { id: `board:${project}` },
  });
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 150, tolerance: 5 } })
  );
  const openKey = [...projectIssuesQueryKey(project, []), "open"];

  const move = async (key: string, input: UpdateIssueInput) => {
    const previous = queryClient.getQueryData<IssueSummary[]>(openKey);
    if (previous !== undefined && input.status !== undefined) {
      void queryClient.cancelQueries({ queryKey: openKey, exact: true });
      queryClient.setQueryData(
        openKey,
        previous.map((issue) =>
          issue.key === key ? { ...issue, status: input.status ?? issue.status } : issue
        )
      );
    }
    try {
      await patch.mutateAsync({ input, key });
      setNotice(undefined);
    } catch (cause) {
      if (previous !== undefined) queryClient.setQueryData(openKey, previous);
      setNotice(cause instanceof ApiError ? cause.message : "Could not move the card.");
    } finally {
      await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
    }
  };

  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (over === null) return;
    const key = String(active.id);
    const row = rows.find((item) => item.issue.key === key);
    const target = readDrop(String(over.id), rows);
    if (row === undefined || target === undefined || target.key === key) return;
    // A drop into another lane takes that lane's priority, as today's lanes do.
    const lane = lanes ? target.lane : undefined;
    const priority =
      lane === undefined || lane === issuePriority(row.issue.priority) ? {} : { priority: lane };
    const status = stageStatus(target.stage);
    if (target.stage === "queue") {
      // Within the Queue column a drop writes the rank; from anywhere else it is refused.
      if (row.stage !== "queue") {
        setNotice("Legion puts issues in its queue: a card cannot be dropped there");
        return;
      }
      const column = rows.filter((item) => item.stage === "queue");
      const neighbour = target.key ?? column.at(-1)?.issue.key;
      if (neighbour === undefined || neighbour === key) return;
      const movingUp =
        target.key !== undefined &&
        column.findIndex((item) => item.issue.key === key) >
          column.findIndex((item) => item.issue.key === target.key);
      void move(key, {
        ...priority,
        rank: movingUp ? { before: neighbour } : { after: neighbour },
      });
      return;
    }
    if (status === undefined) {
      setNotice(
        `Legion owns the ${stageLabel(target.stage)} stage: a card cannot be dropped there`
      );
      return;
    }
    const input: UpdateIssueInput = {
      ...priority,
      ...(row.issue.status === status ? {} : { status }),
      ...(target.key === undefined ? {} : { rank: { before: target.key } }),
    };
    if (Object.keys(input).length === 0) return;
    void move(key, input);
  };

  const done = rows.filter((item) => item.stage === "done");
  // Icebox, while the reader has not turned it on, is a rail in its own place in the stage order
  // (after Triage, before Backlog), as the Board on main draws it: for a view that lists Icebox
  // issues or picks the Icebox status. One rule for the whole board, so every lane draws it.
  const iceboxRail =
    !stages.includes("icebox") &&
    (rows.some((item) => item.stage === "icebox") || (statusPicks ?? []).includes("icebox"));
  const order: readonly StageId[] = iceboxRail
    ? [...stages, "icebox" as const].sort((a, b) => stageIds.indexOf(a) - stageIds.indexOf(b))
    : stages;
  const drawRow = (from: readonly Item[], lane: string | undefined) => {
    const columns = new Map<StageId, Item[]>();
    for (const item of from) {
      const list = columns.get(item.stage) ?? [];
      list.push(item);
      columns.set(item.stage, list);
    }
    return (
      <div className="flex w-max items-stretch gap-4">
        {order.map((stage) =>
          stage === "icebox" && iceboxRail ? (
            <Rail
              count={(columns.get("icebox") ?? []).length}
              dropId={dropIdOf("icebox", lane)}
              key={stage}
              stage="icebox"
            />
          ) : (
            <StageColumn
              dropId={dropIdOf(stage, lane)}
              items={columns.get(stage) ?? []}
              key={stage}
              nowMs={nowMs}
              stage={stage}
              userState={userState}
            />
          )
        )}
        {listsDone ? (
          <StageColumn
            dropId={dropIdOf("done", lane)}
            items={columns.get("done") ?? []}
            nowMs={nowMs}
            stage="done"
            userState={userState}
          />
        ) : (
          <Rail count={undefined} dropId={dropIdOf("done", lane)} stage="done" />
        )}
      </div>
    );
  };

  return (
    <section aria-label="Project board" className="min-w-0" data-board-stages={stages.join(" ")}>
      {notice === undefined ? null : (
        <div
          aria-live="assertive"
          className={`mb-3 rounded-xl border px-4 py-2 text-sm ${card} ${dangerText}`}
          role="alert"
        >
          {notice}
        </div>
      )}
      <DndContext
        collisionDetection={boardCollisionDetection}
        onDragEnd={onDragEnd}
        onDragStart={closeRefPreview}
        sensors={sensors}
      >
        {lanes ? (
          <div className="flex flex-col gap-6" data-testid="board-lanes-container">
            {priorityLanes.map((lane) => {
              const inLane = rows.filter((item) => issuePriority(item.issue.priority) === lane);
              return (
                <section aria-label={`${laneLabel(lane)} lane`} key={String(lane)}>
                  <h2
                    className={`mb-2 flex items-center gap-2 text-xs font-semibold tracking-wide uppercase ${textSecondaryOnSurface}`}
                  >
                    {laneLabel(lane)}
                    <Pill>{inLane.length}</Pill>
                  </h2>
                  <div className="relative overflow-x-auto pb-3">
                    {drawRow(inLane, String(lane))}
                  </div>
                </section>
              );
            })}
          </div>
        ) : (
          // `relative` is load-bearing: a card's `sr-only` text is absolutely placed, and with no
          // placed ancestor inside the scroll box it widens the whole page past the window.
          <div className="relative overflow-x-auto pb-3" data-testid="board-scroll-container">
            {drawRow(rows, undefined)}
          </div>
        )}
      </DndContext>
      <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>
        {rows.length} {rows.length === 1 ? "issue" : "issues"}
        {done.length > 0 ? `, ${done.length} done` : ""}
      </p>
    </section>
  );
}
