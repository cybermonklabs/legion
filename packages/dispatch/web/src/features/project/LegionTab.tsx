// The project's Legion efficiency dashboard, /projects/<KEY>/legion (dispatch://INFRA-979): one
// page per project in place of the deployment-wide Delivery and Pipeline pages, one column. From
// the top: where the finished issues' time went and each issue's own bar, work time to merge,
// rework, splitting, CI and deploys (LegionSections); then the live slots and the waiting line
// from the board route over /ws/live. Any issue opens in its timeline (LegionIssueTimeline) at
// /projects/<KEY>/legion/<ISSUE>, above the rest.
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

import { legionBoardQuery, legionSettingsQuery } from "../../api/queries";
import type { LegionBoardCard, LegionBoardResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildProjectPath } from "../refs/routes";
import { LegionPhaseCell } from "./issues-table-columns";
import { formatPhaseTime, legionPhases } from "./issues-table-model";
import { LegionIssueTimeline } from "./LegionIssueTimeline";
import { LegionSections } from "./LegionSections";

const CARD = `flex min-w-0 flex-col gap-2 rounded border px-4 py-3 ${surfaceBg} ${borderDefault}`;
const KEY_BUTTON = "font-mono text-xs underline decoration-dotted underline-offset-2";

/** The phases that hold a slot for its tree's children alone, and the work under a root that
 *  keeps such a slot busy (the idle-slot rule the dashboard states). */
const HELD_FOR_CHILDREN: Record<string, true> = {
  accepting: true,
  awaiting_merge: true,
  awaiting_person: true,
};
/** A CI verdict in the Slots table's narrow column; the full word is its title. */
const CI_SHORT: Record<string, string> = { green: "✓", failing: "✗", pending: "…" };
const CHILD_WORK: Record<string, true> = { implementing: true, testing: true, reviewing: true };
/** How many rows a live table shows before its "show all". */
const FIRST_ROWS = 10;

/** The button under a live table that shows all its rows, or the first ones again. */
function ShowAll({
  all,
  count,
  noun,
  onToggle,
}: {
  all: boolean;
  count: number;
  noun: string;
  onToggle: () => void;
}): ReactNode {
  if (count <= FIRST_ROWS) return null;
  return (
    <button
      className={`self-start text-xs underline ${textSecondaryOnCanvas}`}
      data-legion-show-all={noun}
      onClick={onToggle}
      type="button"
    >
      {all ? `Show the first ${FIRST_ROWS}` : `Show all ${count} ${noun}`}
    </button>
  );
}

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
        {project} names no Legion daemon, so it has no Legion dashboard. The Settings page names
        one.
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
          message="Couldn't load the slots and the waiting line."
          onRetry={() => void board.refetch()}
          retrying={board.isFetching}
        />
      ) : board.data === undefined ? (
        <p className={textMutedOnCanvas}>Loading the slots and the waiting line…</p>
      ) : (
        <>
          <Slots board={board.data} onOpenIssue={open} />
          <WaitingLine board={board.data} onOpenIssue={open} />
        </>
      )}
    </div>
  );
}

/** Each slot's holder, live: its phase as the table writes it, its roles in a turn, its children
 *  not yet started, its CI verdict, and whether the slot is held for children only or idle. */
function Slots({
  board,
  onOpenIssue,
}: {
  board: LegionBoardResponse;
  onOpenIssue: (key: string) => void;
}): ReactNode {
  const phases = useMemo(() => legionPhases(board), [board]);
  const [all, setAll] = useState(false);
  const cards = useMemo(() => board.columns.flatMap((column) => column.cards), [board]);
  const holders = cards
    .filter((card) => card.slot !== null)
    .sort((a, b) => (a.slot ?? 0) - (b.slot ?? 0));
  // A worker in a turn, an architect's included, makes the slot working whatever the phase.
  const state = (card: LegionBoardCard): string => {
    if (card.roles.length > 0 || !HELD_FOR_CHILDREN[card.phase]) return "working";
    const busy = cards.some(
      (other) =>
        other.tree === card.key && other.key !== card.key && CHILD_WORK[other.phase] === true
    );
    return busy ? "held for children only" : "held, children idle";
  };
  const shown = all ? holders : holders.slice(0, FIRST_ROWS);
  const held = holders.filter((card) => state(card) !== "working");
  const idle = holders.filter((card) => state(card) === "held, children idle");
  const { header } = board;
  return (
    <section aria-label="Slots" className={CARD} data-legion-slots>
      <h2 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>Slots, now</h2>
      <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-slot-summary>
        {header.slots_in_use} of {header.slot_cap ?? "?"} slots in use · {held.length} held with no
        worker of its own in a turn · {idle.length} of those with children idle, nothing under their
        root implementing, testing or reviewing
      </p>
      <p className={`text-xs ${textMutedOnSurface}`}>
        A slot is working while a worker of its issue is in a turn, the architect's included, or
        while its root is in a phase of its own work. It is held for children only while its root is
        accepting, awaiting merge or awaiting a person with no worker in a turn; held, children idle
        when, besides, nothing under the root is implementing, testing or reviewing. CI: ✓ green, ✗
        failing, … pending, — no verdict.
      </p>
      <div className="hidden md:block">
        <table className="w-full text-left text-xs">
          <thead className={textMutedOnSurface}>
            <tr>
              <th className="px-1 py-1 font-normal">Slot</th>
              <th className="px-1 py-1 font-normal">Issue</th>
              <th className="px-1 py-1 font-normal">Phase</th>
              <th className="px-1 py-1 font-normal">Children not started</th>
              <th className="px-1 py-1 font-normal">CI</th>
              <th className="px-1 py-1 font-normal whitespace-nowrap">Slot use</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((card) => {
              const phase = phases.get(card.key);
              return (
                <tr
                  className={`border-t align-top ${borderDefault}`}
                  data-legion-slot={card.slot ?? ""}
                  key={card.key}
                >
                  <td className="px-1 py-1">{card.slot}</td>
                  <td className="px-1 py-1">
                    <button
                      className={KEY_BUTTON}
                      onClick={() => onOpenIssue(card.key)}
                      type="button"
                    >
                      {card.key}
                    </button>
                    <div
                      className={`max-w-[13rem] truncate md:max-w-md ${textSecondaryOnCanvas}`}
                      title={card.title}
                    >
                      {card.title}
                    </div>
                  </td>
                  <td className="px-1 py-1">
                    {phase === undefined ? null : <SlotPhase card={card} phase={phase} />}
                  </td>
                  <td className="px-1 py-1">{card.children}</td>
                  <td
                    className="px-1 py-1 whitespace-nowrap"
                    data-legion-slot-ci={card.checks_verdict ?? ""}
                    title={card.checks_verdict ?? "No CI verdict"}
                  >
                    {CI_SHORT[card.checks_verdict ?? ""] ?? card.checks_verdict ?? "—"}
                  </td>
                  <td className="px-1 py-1 whitespace-nowrap" data-legion-slot-state={state(card)}>
                    {state(card)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {/* A narrow page stacks each slot into a card with every field the table has. */}
      <ol className="flex flex-col md:hidden" data-legion-slot-cards>
        {shown.map((card) => {
          const phase = phases.get(card.key);
          const facts: [string, ReactNode][] = [
            [
              "Phase",
              phase === undefined ? "—" : <SlotPhase card={card} key="phase" phase={phase} />,
            ],
            ["Children not started", card.children],
            [
              "CI",
              <span key="ci" title={card.checks_verdict ?? "No CI verdict"}>
                {CI_SHORT[card.checks_verdict ?? ""] ?? card.checks_verdict ?? "—"}{" "}
                {card.checks_verdict ?? "no verdict"}
              </span>,
            ],
            ["Slot use", state(card)],
          ];
          return (
            <li
              className={`flex flex-col gap-1 border-t py-2 text-xs ${borderDefault}`}
              data-legion-slot-card={card.slot ?? ""}
              key={card.key}
            >
              <div className="flex items-baseline gap-2">
                <span className={textMutedOnSurface}>Slot {card.slot}</span>
                <button className={KEY_BUTTON} onClick={() => onOpenIssue(card.key)} type="button">
                  {card.key}
                </button>
              </div>
              <div className={textSecondaryOnCanvas}>{card.title}</div>
              <dl className="grid grid-cols-[8.5rem_minmax(0,1fr)] gap-x-2 gap-y-1">
                {facts.map(([label, value]) => (
                  <div className="contents" key={label}>
                    <dt className={textMutedOnSurface}>{label}</dt>
                    <dd>{value}</dd>
                  </div>
                ))}
              </dl>
            </li>
          );
        })}
      </ol>
      <ShowAll all={all} count={holders.length} noun="slots" onToggle={() => setAll(!all)} />
    </section>
  );
}

/** A slot's phase: the issues table's own cell, and "· between turns" under it when no worker of
 *  the issue is in a turn (the roles themselves are on the board cards and in the table). */
function SlotPhase({
  card,
  phase,
}: {
  card: LegionBoardCard;
  phase: Parameters<typeof LegionPhaseCell>[0]["phase"];
}): ReactNode {
  return (
    <span className="flex flex-col gap-1">
      <LegionPhaseCell phase={phase} />
      {card.roles.length === 0 ? (
        <span className={`text-xs ${textMutedOnSurface}`} data-legion-slot-between-turns>
          · between turns
        </span>
      ) : null}
    </span>
  );
}

/** The daemon's waiting line in its own order, each root ready, held by a dependency or on a
 *  person, and the issues awaiting a person. */
function WaitingLine({
  board,
  onOpenIssue,
}: {
  board: LegionBoardResponse;
  onOpenIssue: (key: string) => void;
}): ReactNode {
  const queue = board.columns.find((column) => column.id === "queue")?.cards ?? [];
  const awaiting = board.columns.find((column) => column.id === "awaiting_person")?.cards ?? [];
  const [all, setAll] = useState(false);
  const now = Date.now();
  const why = (card: LegionBoardCard): string =>
    card.blocked_by.length > 0
      ? `held by ${card.blocked_by.join(", ")}`
      : card.waiting_on !== null && card.waiting_on.answered_at === null
        ? `on a person (${card.waiting_on.kind})`
        : "ready";
  const ready = queue.filter((card) => why(card) === "ready").length;
  const blocked = queue.filter((card) => card.blocked_by.length > 0).length;
  return (
    <section aria-label="Waiting line" className={CARD} data-legion-waiting-line>
      <h2 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>Waiting line, now</h2>
      <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-waiting-summary>
        {queue.length} roots waiting for a slot · {ready} ready · {blocked} held by a dependency ·{" "}
        {awaiting.length} awaiting a person
      </p>
      <p className={`text-xs ${textMutedOnSurface}`}>
        In the order the daemon starts them: the issues awaiting a person first, then the roots by
        their place in line. Waited is the time since the issue entered its phase.
      </p>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead className={textMutedOnSurface}>
            <tr>
              <th className="px-1 py-1 font-normal">#</th>
              <th className="px-1 py-1 font-normal">Issue</th>
              <th className="px-1 py-1 font-normal whitespace-nowrap">Waited</th>
              <th className="px-1 py-1 font-normal">Waiting</th>
            </tr>
          </thead>
          <tbody>
            {(() => {
              const rows = [
                ...awaiting.map((card) => ({ card, place: "person" })),
                ...queue.map((card, index) => ({ card, place: String(index + 1) })),
              ];
              return all ? rows : rows.slice(0, FIRST_ROWS);
            })().map(({ card, place }) => (
              <tr className={`border-t align-top ${borderDefault}`} key={`${place}-${card.key}`}>
                <td className="px-1 py-1">{place === "person" ? "—" : place}</td>
                <td className="px-1 py-1">
                  <button
                    className={KEY_BUTTON}
                    onClick={() => onOpenIssue(card.key)}
                    type="button"
                  >
                    {card.key}
                  </button>
                  <div
                    className={`max-w-[15rem] truncate md:max-w-xl ${textSecondaryOnCanvas}`}
                    title={card.title}
                  >
                    {card.title}
                  </div>
                </td>
                <td
                  className="px-1 py-1 whitespace-nowrap tabular-nums"
                  data-legion-waited={card.key}
                  title={card.phase_since ?? "Entry time unknown"}
                >
                  {formatPhaseTime(card.phase_since, now)}
                </td>
                <td className="px-1 py-1">
                  {place === "person" ? "awaiting a person" : why(card)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ShowAll
        all={all}
        count={awaiting.length + queue.length}
        noun="issues"
        onToggle={() => setAll(!all)}
      />
    </section>
  );
}
