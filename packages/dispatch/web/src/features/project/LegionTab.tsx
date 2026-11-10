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
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildProjectPath } from "../refs/routes";
import { IssueGrid } from "./IssueGrid";
import { formatPhaseTime, legionPhaseLabel, legionPhases } from "./issues-table-model";
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

/** One slot's row: its holder's card and what the slot is used for, short and in detail. */
interface SlotRow {
  readonly key: string;
  readonly card: LegionBoardCard;
  readonly use: string;
  readonly detail: string;
}

/** A slot holder's phase: one label ("Retro" for a merging issue in retro) and, below it from
 *  640 px up, the time alone: in the phase, or since admission where the phase start is not
 *  recorded. The hover says which, and what a waiting root waits on; a phone shows the label. */
function SlotPhase({ card, label }: { card: LegionBoardCard; label: string }): ReactNode {
  const now = Date.now();
  const time =
    card.phase_since !== null
      ? { text: formatPhaseTime(card.phase_since, now), about: "in this phase" }
      : card.slot_since !== null
        ? {
            text: formatPhaseTime(card.slot_since, now),
            about: "since admission; phase start not recorded",
          }
        : { text: "—", about: "phase start and admission not recorded" };
  const waiting =
    card.waiting_on === null
      ? ""
      : `; waits on a ${card.waiting_on.kind}${card.waiting_on.answered_at ? ", answered" : ""}`;
  const title = `${time.text === "—" ? "" : `${time.text} `}${time.about}${waiting}`;
  return (
    <span className="flex flex-col gap-1" data-legion-phase={card.phase} title={title}>
      <span className="whitespace-normal break-words">{legionPhaseLabel(label, card.phase)}</span>
      <span
        className={`hidden text-xs sm:block ${textMutedOnCanvas}`}
        data-legion-fact="phase-time"
        title={time.about}
      >
        {time.text}
      </span>
    </span>
  );
}

/** Each slot's holder, live, in the Issues tab's table: slot, phase, what the slot is used for
 *  (the detail, with a held root's children not started, in the cell's hover) and CI. */
function Slots({ board, project }: { board: LegionBoardResponse; project: string }): ReactNode {
  const phases = useMemo(() => legionPhases(board), [board]);
  const rows = useMemo((): SlotRow[] => {
    const cards = board.columns.flatMap((column) => column.cards);
    // A worker in a turn (an architect's included) makes the slot working; a root in its own
    // phase with no worker in a turn is between turns; else the root holds it for its children.
    const use = (card: LegionBoardCard): Pick<SlotRow, "use" | "detail"> => {
      if (card.roles.length > 0)
        return { use: "working", detail: `${card.roles.join(", ")} in a turn` };
      if (card.phase === "awaiting_person")
        return {
          use: "held: person",
          detail: `Waits on a person${card.waiting_on ? ` (${card.waiting_on.kind})` : ""}`,
        };
      if (!HELD_FOR_CHILDREN[card.phase])
        return { use: "between turns", detail: "Own phase, no worker in a turn" };
      const busy = cards.some(
        (other) =>
          other.tree === card.key && other.key !== card.key && CHILD_WORK[other.phase] === true
      );
      // The children not started go in the hover: the title cell's pill already counts children.
      const waiting = `; ${card.children} not started`;
      return busy
        ? { use: "held: children", detail: `Held for its children, which work${waiting}` }
        : { use: "held: idle", detail: `Held for its children; no child works${waiting}` };
    };
    return cards
      .filter((card) => card.slot !== null)
      .sort((a, b) => (a.slot ?? 0) - (b.slot ?? 0))
      .map((card) => ({ key: card.key, card, ...use(card) }));
  }, [board]);
  // Each slot in use is in exactly one of these, so they add up to the slots in use.
  const tally = (use: string) => rows.filter((row) => row.use === use).length;
  const cap = board.header.slot_cap;
  return (
    <section aria-label="Slots" className={CARD} data-legion-slots>
      <SectionTitle
        info="Working: a worker is in a turn. Between turns: no worker of the root is in a turn. Held: the root waits on its children, who work. On a person: the root waits on a person. Idle: the root waits on its children, and none of them works."
        title="Slots"
      />
      <p className={`text-sm tabular-nums ${textSecondaryOnCanvas}`} data-legion-slot-summary>
        {cap === rows.length
          ? `${rows.length} slots, all in use`
          : `${rows.length} of ${cap ?? "?"} slots in use`}{" "}
        · {tally("working")} working · {tally("between turns")} between turns ·{" "}
        {tally("held: children")} held · {tally("held: person")} on a person · {tally("held: idle")}{" "}
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
            size: 56,
          },
          "key",
          "title",
          {
            id: "slot_phase",
            header: "Phase",
            value: (row) => phases.get(row.key)?.order ?? Number.MAX_SAFE_INTEGER,
            cell: (row) => {
              const phase = phases.get(row.key);
              return phase === undefined ? null : <SlotPhase card={row.card} label={phase.label} />;
            },
            filter: "picks",
            size: 160,
            primary: true,
          },
          {
            id: "slot_use",
            header: "Slot use",
            value: (row) => row.use,
            cell: (row) => (
              <span
                className="whitespace-nowrap"
                data-legion-slot-state={row.use}
                title={row.detail}
              >
                {row.use}
              </span>
            ),
            filter: "picks",
            size: 130,
          },
          {
            id: "slot_ci",
            header: "CI",
            value: (row) => row.card.checks_verdict ?? "No verdict",
            cell: (row) => row.card.checks_verdict ?? "—",
            filter: "picks",
            size: 80,
          },
        ]}
        empty="No slot in use."
        fit="rows"
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

/** One row of the waiting line: its place (none for an issue awaiting a person, which is out of
 *  the queue), its card, why it waits, and since when. */
interface WaitingRow {
  readonly key: string;
  readonly place: number | null;
  readonly card: LegionBoardCard;
  readonly why: string;
  readonly since: string | null;
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
    // A wait on a person counts from when the person was asked, else from the phase entry.
    const row = (card: LegionBoardCard, place: number | null): WaitingRow => {
      const onPerson =
        place === null || (card.waiting_on !== null && card.waiting_on.answered_at === null);
      return {
        key: card.key,
        place,
        card,
        why:
          place !== null && card.blocked_by.length > 0
            ? `held by ${card.blocked_by.join(", ")}`
            : onPerson
              ? "awaiting a person"
              : "ready",
        since: (onPerson ? card.waiting_on?.since : null) ?? card.phase_since,
      };
    };
    return [
      ...awaiting.map((card) => row(card, null)),
      ...queue.map((card, index) => row(card, index + 1)),
    ];
  }, [board]);
  const ready = rows.filter((row) => row.why === "ready").length;
  const blocked = rows.filter((row) => row.why.startsWith("held by")).length;
  const awaiting = rows.filter((row) => row.why === "awaiting a person").length;
  const now = Date.now();
  return (
    <section aria-label="Waiting line" className={CARD} data-legion-waiting-line>
      <SectionTitle
        info="The daemon starts the roots in this order. Waited: the time since the issue entered its phase, or since it started to wait for a person."
        title="Waiting line"
      />
      <p className={`text-sm tabular-nums ${textSecondaryOnCanvas}`} data-legion-waiting-summary>
        {rows.length} issues · {ready} ready · {blocked} held by a dependency · {awaiting} awaiting
        a person
      </p>
      <IssueGrid<WaitingRow>
        columns={[
          {
            id: "place",
            header: "#",
            // The queue first, by place; then the rows with no place, the longest wait first
            // and an unknown wait last.
            value: (row) =>
              row.place ??
              (row.since === null ? 2e12 : 1e12 - (now - Date.parse(row.since)) / 1000),
            cell: (row) =>
              row.place === null ? (
                <span
                  className="block h-4 w-full"
                  data-legion-place-none
                  title="Not in the queue: waits for a person"
                />
              ) : (
                <span className="tabular-nums">{row.place}</span>
              ),
            filter: false,
            size: 64,
            primary: true,
          },
          "key",
          "title",
          {
            id: "waited",
            header: "Waited",
            value: (row) => (row.since === null ? 0 : now - Date.parse(row.since)),
            cell: (row) => (
              <span
                className="tabular-nums whitespace-nowrap"
                data-legion-waited={row.key}
                title={row.since ?? "start of the wait not recorded"}
              >
                {formatPhaseTime(row.since, now)}
              </span>
            ),
            filter: false,
            size: 110,
            primary: true,
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
        fit="rows"
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
