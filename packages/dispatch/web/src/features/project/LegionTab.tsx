// The project's Legion efficiency dashboard, /projects/<KEY>/legion (dispatch://INFRA-979): one
// page per project in place of the deployment-wide Delivery and Pipeline pages. From the top:
// the headline tiles and every card of LegionHealthPanel (the four Legion metrics, the project's
// PR CI and its deploys, each with graph or table and the range compare); the live slots and the
// waiting line from the board route over /ws/live; and any issue opened in its timeline
// (LegionIssueTimeline) at /projects/<KEY>/legion/<ISSUE>. A number's issues open in a list
// here, each key opening that issue's timeline.
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
import { legionPhases, legionRolesText } from "./issues-table-model";
import { LegionHealthPanel } from "./LegionHealthPanel";
import { LegionIssueTimeline } from "./LegionIssueTimeline";

const CARD = `flex min-w-0 flex-col gap-2 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;
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
  const [selection, setSelection] = useState<{ keys: string[]; label: string } | null>(null);
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
      <LegionHealthPanel
        layout="dashboard"
        onOpenIssue={open}
        onSelectIssues={(keys, label) => setSelection({ keys, label })}
        project={project}
      />
      {selection === null ? null : (
        <SelectedIssues
          board={board.data}
          onClear={() => setSelection(null)}
          onOpenIssue={open}
          selection={selection}
        />
      )}
      {board.isError ? (
        <QueryError
          message="Couldn't load the slots and the waiting line."
          onRetry={() => void board.refetch()}
          retrying={board.isFetching}
        />
      ) : board.data === undefined ? (
        <p className={textMutedOnCanvas}>Loading the slots and the waiting line…</p>
      ) : (
        <div className="grid grid-cols-1 gap-3 xl:grid-cols-2">
          <Slots board={board.data} onOpenIssue={open} />
          <WaitingLine board={board.data} onOpenIssue={open} />
        </div>
      )}
    </div>
  );
}

/** The issues behind a number the reader clicked, each opening its timeline. */
function SelectedIssues({
  board,
  onClear,
  onOpenIssue,
  selection,
}: {
  board: LegionBoardResponse | undefined;
  onClear: () => void;
  onOpenIssue: (key: string) => void;
  selection: { keys: string[]; label: string };
}): ReactNode {
  const titles = useMemo(() => {
    const byKey = new Map<string, string>();
    for (const column of board?.columns ?? []) {
      for (const card of column.cards) byKey.set(card.key, card.title);
    }
    return byKey;
  }, [board]);
  return (
    <section aria-label="Selected issues" className={CARD} data-legion-selection>
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
          {selection.label}: {selection.keys.length} issues
        </h2>
        <button className={`text-xs ${textSecondaryOnCanvas}`} onClick={onClear} type="button">
          Clear
        </button>
      </header>
      <ul className="flex max-h-48 flex-col gap-0.5 overflow-auto">
        {selection.keys.map((key) => (
          <li className="flex items-baseline gap-2 text-sm" key={key}>
            <button className={KEY_BUTTON} onClick={() => onOpenIssue(key)} type="button">
              {key}
            </button>
            <span className={`truncate ${textSecondaryOnCanvas}`}>{titles.get(key) ?? ""}</span>
          </li>
        ))}
      </ul>
    </section>
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
  const cards = useMemo(() => board.columns.flatMap((column) => column.cards), [board]);
  const holders = cards
    .filter((card) => card.slot !== null)
    .sort((a, b) => (a.slot ?? 0) - (b.slot ?? 0));
  const state = (card: LegionBoardCard): string => {
    if (!HELD_FOR_CHILDREN[card.phase]) return "working";
    const busy = cards.some(
      (other) =>
        other.tree === card.key && other.key !== card.key && CHILD_WORK[other.phase] === true
    );
    return busy ? "held for children only" : "idle";
  };
  const held = holders.filter((card) => state(card) !== "working");
  const idle = holders.filter((card) => state(card) === "idle");
  const { header } = board;
  return (
    <section aria-label="Slots" className={CARD} data-legion-slots>
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>Slots</h2>
        <span className={`text-xs ${textSecondaryOnCanvas}`} data-legion-slot-summary>
          {header.slots_in_use} of {header.slot_cap ?? "?"} in use · {held.length} held for children
          only · {idle.length} idle
        </span>
      </header>
      <p className={`text-xs ${textMutedOnSurface}`}>
        A slot is held for children only while its root is accepting, awaiting merge or awaiting a
        person; it is idle when, besides, nothing under the root is implementing, testing or
        reviewing.
      </p>
      <div className="max-h-96 overflow-auto">
        <table className="w-full text-left text-xs">
          <thead className={textMutedOnSurface}>
            <tr>
              <th className="px-1 py-1 font-normal">Slot</th>
              <th className="px-1 py-1 font-normal">Issue</th>
              <th className="px-1 py-1 font-normal">Phase</th>
              <th className="px-1 py-1 font-normal">Roles in a turn</th>
              <th className="px-1 py-1 font-normal">Children not started</th>
              <th className="px-1 py-1 font-normal">CI</th>
              <th className="px-1 py-1 font-normal">Slot use</th>
            </tr>
          </thead>
          <tbody>
            {holders.map((card) => {
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
                      className={`max-w-44 truncate ${textSecondaryOnCanvas}`}
                      title={card.title}
                    >
                      {card.title}
                    </div>
                  </td>
                  <td className="px-1 py-1">
                    {phase === undefined ? null : <LegionPhaseCell phase={phase} />}
                  </td>
                  <td className="px-1 py-1">{legionRolesText(card)}</td>
                  <td className="px-1 py-1">{card.children}</td>
                  <td
                    className="px-1 py-1 whitespace-nowrap"
                    data-legion-slot-ci={card.checks_verdict ?? ""}
                    title={card.checks_verdict ?? "No CI verdict"}
                  >
                    {CI_SHORT[card.checks_verdict ?? ""] ?? card.checks_verdict ?? "—"}
                  </td>
                  <td className="px-1 py-1" data-legion-slot-state={state(card)}>
                    {state(card)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
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
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>Waiting line</h2>
        <span className={`text-xs ${textSecondaryOnCanvas}`} data-legion-waiting-summary>
          {queue.length} waiting · {ready} ready · {blocked} held by a dependency ·{" "}
          {awaiting.length} awaiting a person
        </span>
      </header>
      <div className="max-h-96 overflow-auto">
        <table className="w-full text-left text-xs">
          <thead className={textMutedOnSurface}>
            <tr>
              <th className="px-1 py-1 font-normal">#</th>
              <th className="px-1 py-1 font-normal">Issue</th>
              <th className="px-1 py-1 font-normal">Waiting</th>
            </tr>
          </thead>
          <tbody>
            {[
              ...awaiting.map((card) => ({ card, place: "person" })),
              ...queue.map((card, index) => ({ card, place: String(index + 1) })),
            ].map(({ card, place }) => (
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
                  <div className={`max-w-72 truncate ${textSecondaryOnCanvas}`} title={card.title}>
                    {card.title}
                  </div>
                </td>
                <td className="px-1 py-1">
                  {place === "person" ? "awaiting a person" : why(card)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
