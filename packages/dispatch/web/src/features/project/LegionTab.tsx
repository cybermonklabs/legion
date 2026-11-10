// The project's Legion dashboard, /projects/<KEY>/legion (dispatch://INFRA-979), one column:
// metrics first (LegionSections: time per phase, work time, rework, splitting, CI and deploys),
// then the issue tables, each the Issues tab's table (IssueGrid): finished issues, slots and the
// waiting line. An issue key opens its timeline (LegionIssueTimeline) at
// /projects/<KEY>/legion/<ISSUE>, above the rest.
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useNavigate } from "react-router-dom";

import { legionBoardQuery, legionSettingsQuery } from "../../api/queries";
import type { LegionBoardCard, LegionBoardResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildProjectPath } from "../refs/routes";
import { IssueGrid } from "./IssueGrid";
import { LegionPhaseCell } from "./issues-table-columns";
import { formatPhaseTime, legionPhases } from "./issues-table-model";
import { LegionIssueTimeline } from "./LegionIssueTimeline";
import { LegionSections, SectionTitle } from "./LegionSections";

const CARD = `flex min-w-0 flex-col gap-2 rounded border px-4 py-3 ${surfaceBg} ${borderDefault}`;

/** The phases in which a root holds its slot for its children alone. */
const HELD_FOR_CHILDREN: Record<string, true> = {
  accepting: true,
  awaiting_merge: true,
  awaiting_person: true,
};
/** The phases of a child that keep its root's held slot busy. */
const CHILD_WORK: Record<string, true> = { implementing: true, testing: true, reviewing: true };

export function LegionTab({
  issue,
  project,
}: {
  readonly project: string;
  readonly issue?: string;
}): ReactNode {
  const navigate = useNavigate();
  const settings = useQuery(legionSettingsQuery());
  const hasLegion = (settings.data ?? []).some((setting) => setting.project === project);
  const open = (key: string) => navigate(buildProjectPath({ kind: "legion", project, issue: key }));
  const close = () => navigate(buildProjectPath({ kind: "legion", project }));
  const board = useQuery({ ...legionBoardQuery({ kind: "legion", project }), enabled: hasLegion });

  if (settings.isPending) {
    return (
      <p className={textMutedOnCanvas} data-legion-dashboard>
        Loading…
      </p>
    );
  }
  if (!hasLegion) {
    return (
      <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-dashboard>
        No Legion daemon for {project}. Set one on the Settings page.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-3" data-legion-dashboard>
      {issue === undefined ? null : (
        <LegionIssueTimeline issueKey={issue} onClose={close} onOpenIssue={open} />
      )}
      <LegionSections onOpenIssue={open} project={project} />
      {board.isError ? (
        <QueryError
          message="Could not load the slots and the waiting line."
          onRetry={() => void board.refetch()}
          retrying={board.isFetching}
        />
      ) : board.data === undefined ? (
        <p className={textMutedOnCanvas}>Loading…</p>
      ) : (
        <>
          <Slots board={board.data} project={project} />
          <WaitingLine board={board.data} project={project} />
        </>
      )}
    </div>
  );
}

/** The timeline address a key on the Legion tab links to. */
const timelineHref =
  (project: string) =>
  (row: { key: string }): string =>
    buildProjectPath({ kind: "legion", project, issue: row.key });

/** One slot's row: its holder's card and what the slot is used for. */
interface SlotRow {
  readonly key: string;
  readonly card: LegionBoardCard;
  readonly use: string;
}

/** Each slot's holder, live, in the Issues tab's table: slot, phase (with "between turns" when no
 *  worker of it is in a turn), children not started, CI and what the slot is used for. */
function Slots({ board, project }: { board: LegionBoardResponse; project: string }): ReactNode {
  const phases = useMemo(() => legionPhases(board), [board]);
  const rows = useMemo((): SlotRow[] => {
    const cards = board.columns.flatMap((column) => column.cards);
    // A worker in a turn (an architect's included) or the root's own phase makes the slot
    // working; else the root holds it for its children.
    const use = (card: LegionBoardCard): string => {
      if (card.roles.length > 0) return `working: ${card.roles.join(", ")} in a turn`;
      if (!HELD_FOR_CHILDREN[card.phase]) return "working: own phase";
      const busy = cards.some(
        (other) =>
          other.tree === card.key && other.key !== card.key && CHILD_WORK[other.phase] === true
      );
      return busy ? "held: for its children" : "held: children idle";
    };
    return cards
      .filter((card) => card.slot !== null)
      .sort((a, b) => (a.slot ?? 0) - (b.slot ?? 0))
      .map((card) => ({ key: card.key, card, use: use(card) }));
  }, [board]);
  const held = rows.filter((row) => row.use.startsWith("held")).length;
  const idle = rows.filter((row) => row.use === "held: children idle").length;
  const { header } = board;
  return (
    <section aria-label="Slots" className={CARD} data-legion-slots>
      <SectionTitle
        info="Held: the root waits on its children, and no worker of the root is in a turn. Children idle: no child of the root implements, tests or reviews."
        title="Slots"
      />
      <p className={`text-sm tabular-nums ${textSecondaryOnCanvas}`} data-legion-slot-summary>
        {header.slots_in_use} of {header.slot_cap ?? "?"} in use · {held} held · {idle} children
        idle
      </p>
      <IssueGrid<SlotRow>
        columns={[
          {
            id: "slot_index",
            header: "Slot",
            value: (row) => row.card.slot ?? -1,
            cell: (row) => <span className="tabular-nums">{row.card.slot}</span>,
            filter: false,
            size: 72,
          },
          "key",
          "title",
          {
            id: "slot_phase",
            header: "Phase",
            value: (row) => phases.get(row.key)?.order ?? Number.MAX_SAFE_INTEGER,
            cell: (row) => {
              const phase = phases.get(row.key);
              return phase === undefined ? null : (
                <span className="flex flex-col gap-1">
                  <LegionPhaseCell phase={phase} />
                  {row.card.roles.length === 0 ? (
                    <span
                      className={`text-xs ${textMutedOnSurface}`}
                      data-legion-slot-between-turns
                    >
                      between turns
                    </span>
                  ) : null}
                </span>
              );
            },
            filter: "picks",
            size: 200,
          },
          {
            id: "children_not_started",
            header: "Children not started",
            value: (row) => row.card.children,
            cell: (row) => <span className="tabular-nums">{row.card.children}</span>,
            filter: false,
            size: 120,
          },
          "ci",
          {
            id: "slot_use",
            header: "Slot use",
            value: (row) => row.use,
            cell: (row) => <span data-legion-slot-state={row.use}>{row.use}</span>,
            filter: "picks",
            size: 220,
          },
        ]}
        empty="No slot in use."
        initialSort={[{ id: "slot_index", desc: false }]}
        keyHref={timelineHref(project)}
        label="Slots"
        layoutKey="legion-slots"
        pageSize={25}
        project={project}
        rows={rows}
      />
    </section>
  );
}

/** One row of the waiting line: its place (none for an issue awaiting a person), its card and
 *  why it waits. */
interface WaitingRow {
  readonly key: string;
  readonly place: number | null;
  readonly card: LegionBoardCard;
  readonly why: string;
}

/** The daemon's waiting line, in the order it starts them, in the Issues tab's table: place, the
 *  time each issue waited in its phase, and why it waits. */
function WaitingLine({
  board,
  project,
}: {
  board: LegionBoardResponse;
  project: string;
}): ReactNode {
  const rows = useMemo((): WaitingRow[] => {
    const queue = board.columns.find((column) => column.id === "queue")?.cards ?? [];
    const awaiting = board.columns.find((column) => column.id === "awaiting_person")?.cards ?? [];
    const why = (card: LegionBoardCard): string =>
      card.blocked_by.length > 0
        ? `held by ${card.blocked_by.join(", ")}`
        : card.waiting_on !== null && card.waiting_on.answered_at === null
          ? `on a person (${card.waiting_on.kind})`
          : "ready";
    return [
      ...awaiting.map((card) => ({ key: card.key, place: null, card, why: "awaiting a person" })),
      ...queue.map((card, index) => ({ key: card.key, place: index + 1, card, why: why(card) })),
    ];
  }, [board]);
  const queued = rows.filter((row) => row.place !== null);
  const ready = queued.filter((row) => row.why === "ready").length;
  const blocked = queued.filter((row) => row.card.blocked_by.length > 0).length;
  const awaiting = rows.length - queued.length;
  const now = Date.now();
  return (
    <section aria-label="Waiting line" className={CARD} data-legion-waiting-line>
      <SectionTitle
        info="The daemon starts the roots in this order. Waited is the time since the issue entered its phase."
        title="Waiting line"
      />
      <p className={`text-sm tabular-nums ${textSecondaryOnCanvas}`} data-legion-waiting-summary>
        {queued.length} waiting · {ready} ready · {blocked} held by a dependency · {awaiting}{" "}
        awaiting a person
      </p>
      <IssueGrid<WaitingRow>
        columns={[
          {
            id: "place",
            header: "#",
            value: (row) => row.place ?? 0,
            cell: (row) => <span className="tabular-nums">{row.place ?? "—"}</span>,
            filter: false,
            size: 64,
          },
          "key",
          "title",
          {
            id: "waited",
            header: "Waited",
            value: (row) =>
              row.card.phase_since === null ? 0 : now - Date.parse(row.card.phase_since),
            cell: (row) => (
              <span
                className="tabular-nums whitespace-nowrap"
                data-legion-waited={row.key}
                title={row.card.phase_since ?? "Entry time unknown"}
              >
                {formatPhaseTime(row.card.phase_since, now)}
              </span>
            ),
            filter: false,
            size: 110,
          },
          {
            id: "waiting_why",
            header: "Waiting",
            value: (row) => row.why,
            filter: "picks",
            size: 220,
          },
        ]}
        empty="Nothing waits."
        initialSort={[{ id: "place", desc: false }]}
        keyHref={timelineHref(project)}
        label="Waiting line"
        layoutKey="legion-waiting"
        pageSize={25}
        project={project}
        rows={rows}
      />
    </section>
  );
}
