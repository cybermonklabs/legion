import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectIssuesQuery, whoAmIQuery } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { type IssueStatus, statusLabel } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

/** PROTOTYPE (dispatch://INFRA-1508): a project's work by epic. An epic is a root issue with the
 *  `epic` label, ranked by board order. The waiting line runs: issues marked start-first, then
 *  every waiting issue of epic 1, then epic 2, each in board order. Every action here is one or
 *  more ordinary issue writes (parent, labels, title, rank), the same an agent or the command bar
 *  makes. */

const EPIC_LABEL = "epic";
const FIRST_LABEL = "start-first";
const RUNNING = new Set(["in_progress", "testing", "needs_review", "retro"]);
const COLORS = [
  "#2f5bd3",
  "#c2410c",
  "#15803d",
  "#7c3aed",
  "#b45309",
  "#0e7490",
  "#be185d",
  "#4d7c0f",
  "#9333ea",
  "#0369a1",
  "#a16207",
  "#dc2626",
];

const hasLabel = (issue: IssueSummary, label: string) => (issue.labels ?? []).includes(label);
const isWaiting = (issue: IssueSummary) => issue.status === "todo" && hasLabel(issue, "legion");

interface Epic {
  readonly root: IssueSummary;
  readonly color: string;
  readonly issues: IssueSummary[];
}

function groupByEpic(issues: readonly IssueSummary[]): { epics: Epic[]; loose: IssueSummary[] } {
  const byKey = new Map(issues.map((issue) => [issue.key, issue]));
  const epics = issues
    .filter(
      (issue) => issue.parent === null && hasLabel(issue, EPIC_LABEL) && issue.status !== "done"
    )
    .map((root, index) => ({
      root,
      color: COLORS[index % COLORS.length] ?? "#666",
      issues: [] as IssueSummary[],
    }));
  const byRoot = new Map(epics.map((epic) => [epic.root.key, epic]));
  const loose: IssueSummary[] = [];
  for (const issue of issues) {
    if (byRoot.has(issue.key) || issue.status === "done") continue;
    let current: IssueSummary | undefined = issue;
    for (let depth = 0; current?.parent && depth < 30; depth++) current = byKey.get(current.parent);
    const epic = current === undefined ? undefined : byRoot.get(current.key);
    if (epic === undefined) loose.push(issue);
    else epic.issues.push(issue);
  }
  // Inside an epic: waiting issues in board order (the line's order), then running, then the rest.
  const order = (issue: IssueSummary) => (isWaiting(issue) ? 0 : RUNNING.has(issue.status) ? 1 : 2);
  for (const epic of epics) epic.issues.sort((a, b) => order(a) - order(b));
  return { epics, loose };
}

type Pending = { kind: "merge"; from: string } | { kind: "rename"; key: string; title: string };

export function EpicsView({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const login = useQuery(whoAmIQuery()).data?.login;
  const query = useQuery(projectIssuesQuery(project, []));
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [dragEpic, setDragEpic] = useState<string>();
  const [dragIssues, setDragIssues] = useState<readonly string[]>();
  const [over, setOver] = useState<string>();
  const [pending, setPending] = useState<Pending>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [log, setLog] = useState<readonly string[]>([]);

  const issues = query.data ?? [];
  const byKey = useMemo(() => new Map(issues.map((issue) => [issue.key, issue])), [issues]);
  const grouped = useMemo(() => groupByEpic(issues), [issues]);
  const epicOfKey = useMemo(() => {
    const map = new Map<string, Epic>();
    for (const epic of grouped.epics) for (const issue of epic.issues) map.set(issue.key, epic);
    return map;
  }, [grouped]);
  const first = issues.filter((issue) => hasLabel(issue, FIRST_LABEL) && issue.status !== "done");
  const line = useMemo(() => {
    const out = issues.filter((issue) => hasLabel(issue, FIRST_LABEL) && isWaiting(issue));
    for (const epic of grouped.epics) {
      out.push(...epic.issues.filter((issue) => isWaiting(issue) && !hasLabel(issue, FIRST_LABEL)));
    }
    return out;
  }, [issues, grouped]);
  const place = new Map(line.map((issue, index) => [issue.key, index + 1]));
  const running = issues.filter((issue) => RUNNING.has(issue.status)).length;

  /** Runs a batch of ordinary issue writes, then reloads the list once. */
  const run = async (label: string, writes: (() => Promise<unknown>)[]) => {
    setBusy(label);
    setError(undefined);
    try {
      for (const write of writes) await write();
      setLog((previous) => [label, ...previous].slice(0, 8));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(undefined);
      await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
    }
  };
  const patch = (key: string, input: UpdateIssueInput) => () =>
    api.patchIssue(key, input, { key: crypto.randomUUID(), login });
  const title = (key: string) => byKey.get(key)?.title ?? key;

  const moveIssues = (keys: readonly string[], epicKey: string) =>
    run(
      `Moved ${keys.length === 1 ? `“${title(keys[0] ?? "")}”` : `${keys.length} issues`} to “${title(epicKey)}”`,
      keys.filter((key) => key !== epicKey).map((key) => patch(key, { parent: epicKey }))
    );
  const startFirst = (keys: readonly string[], on: boolean) =>
    run(
      `${on ? "Start first" : "No longer first"}: ${keys.length === 1 ? `“${title(keys[0] ?? "")}”` : `${keys.length} issues`}`,
      keys.map((key) => {
        const labels = (byKey.get(key)?.labels ?? []).filter((label) => label !== FIRST_LABEL);
        return patch(key, { labels: on ? [...labels, FIRST_LABEL] : labels });
      })
    );
  const newEpic = async (keys: readonly string[]) => {
    const name = window.prompt(`Name the new epic for these ${keys.length} issues`);
    if (!name) return;
    const sourceEpic = epicOfKey.get(keys[0] ?? "")?.root.key;
    let created = "";
    await run(`New epic “${name}” with ${keys.length} issues`, [
      async () => {
        created = (
          await api.createIssue(
            { project, title: name, labels: [EPIC_LABEL], force: true },
            { key: crypto.randomUUID(), login }
          )
        ).key;
      },
      async () => sourceEpic && patch(created, { rank: { after: sourceEpic } })(),
      ...keys.map((key) => async () => patch(key, { parent: created })()),
    ]);
    setSelected(new Set());
  };
  const mergeEpic = (from: string, into: string) => {
    const epic = grouped.epics.find((candidate) => candidate.root.key === from);
    if (epic === undefined) return;
    const direct = epic.issues.filter((issue) => issue.parent === from);
    setPending(undefined);
    void run(`Merged “${epic.root.title}” into “${title(into)}”`, [
      ...direct.map((issue) => patch(issue.key, { parent: into })),
      // A close needs its reason posted first, as `dispatch issue-update --status done` does.
      () =>
        api.createMessage(
          from,
          { body: `Merged into ${into} (“${title(into)}”) from the Epics view.` },
          { key: crypto.randomUUID(), login }
        ),
      patch(from, { status: "done" }),
    ]);
  };
  const reorderEpic = (from: string, before: string) =>
    run(`Moved “${title(from)}” above “${title(before)}”`, [patch(from, { rank: { before } })]);

  const toggle = (set: ReadonlySet<string>, key: string) => {
    const next = new Set(set);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  };

  if (query.isPending) return <LoadingSkeleton label="Loading epics" />;
  if (query.isError) return <p className={dangerText}>Could not load this project's issues.</p>;

  const issueRow = (issue: IssueSummary, color?: string, showPlace?: boolean) => (
    <li
      className={`group flex items-center gap-2 border-t py-1.5 text-sm ${borderDefault}`}
      draggable
      key={issue.key}
      onDragStart={(event) => {
        event.stopPropagation();
        setDragIssues(selected.has(issue.key) ? [...selected] : [issue.key]);
      }}
      onDragEnd={() => setDragIssues(undefined)}
    >
      <input
        aria-label={`Select ${issue.title}`}
        checked={selected.has(issue.key)}
        className="shrink-0"
        onChange={() => setSelected(toggle(selected, issue.key))}
        type="checkbox"
      />
      {color === undefined ? null : (
        <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: color }} />
      )}
      {showPlace ? (
        <span className={`w-7 shrink-0 text-right tabular-nums ${textMutedOnCanvas}`}>
          {place.get(issue.key) ?? ""}
        </span>
      ) : null}
      <Link
        className={`min-w-0 flex-1 ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
      >
        <span className={textPrimaryOnCanvas}>{issue.title}</span>
      </Link>
      {hasLabel(issue, FIRST_LABEL) ? (
        <span className="shrink-0 rounded-full bg-orange-100 px-2 text-xs text-orange-800">
          first
        </span>
      ) : null}
      <span className={`w-20 shrink-0 text-right text-xs ${textMutedOnCanvas}`}>
        {isWaiting(issue)
          ? `#${place.get(issue.key) ?? "?"} in line`
          : statusLabel(issue.status as IssueStatus)}
      </span>
    </li>
  );

  return (
    <div className="mt-3 pb-24">
      {/* The line at a glance: one segment per waiting issue, coloured by its epic. */}
      <section aria-label="The waiting line" className={`mb-4 rounded-xl p-3 ${surfaceMutedBg}`}>
        <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1">
          <Stat
            label="open"
            value={
              grouped.epics.reduce((sum, epic) => sum + epic.issues.length, 0) +
              grouped.loose.length
            }
          />
          <Stat label="running" value={running} />
          <Stat label="waiting" value={line.length} />
          <Stat label="epics" value={grouped.epics.length} />
          <Stat label="start first" value={first.length} />
          {grouped.loose.length > 0 ? (
            <Stat label="in no epic" value={grouped.loose.length} />
          ) : null}
          <span className={`ml-auto text-xs ${textMutedOnCanvas}`}>
            {busy ?? (error === undefined ? (log[0] ?? "") : "")}
          </span>
        </div>
        <div
          className="mt-2 flex h-3 overflow-hidden rounded-full"
          title="The waiting line, by epic"
        >
          {line.map((issue) => (
            <span
              className="h-full flex-1 border-r border-white/60 last:border-r-0"
              key={issue.key}
              style={{
                background: hasLabel(issue, FIRST_LABEL)
                  ? "#ea580c"
                  : (epicOfKey.get(issue.key)?.color ?? "#999"),
              }}
              title={`#${place.get(issue.key)} ${issue.title}`}
            />
          ))}
        </div>
        {error === undefined ? null : <p className={`mt-2 text-sm ${dangerText}`}>{error}</p>}
      </section>

      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_360px]">
        <section aria-label="Epics">
          {first.length === 0 ? null : (
            <div className="mb-3 rounded-xl border-2 border-orange-400 p-3">
              <h2 className={`mb-1 text-sm font-semibold ${textPrimaryOnCanvas}`}>
                Start first{" "}
                <span className={`font-normal ${textMutedOnCanvas}`}>ahead of every epic</span>
              </h2>
              <ul>{first.map((issue) => issueRow(issue, epicOfKey.get(issue.key)?.color))}</ul>
            </div>
          )}
          <ol className="space-y-2">
            {grouped.epics.map((epic, index) => {
              const epicRunning = epic.issues.filter((issue) => RUNNING.has(issue.status));
              const waiting = epic.issues.filter(isWaiting);
              const rest = epic.issues.length - epicRunning.length - waiting.length;
              const isOpen = open.has(epic.root.key);
              const target = over === epic.root.key;
              const merging = pending?.kind === "merge" && pending.from !== epic.root.key;
              return (
                <li
                  className={`overflow-hidden rounded-xl border bg-white ${borderDefault} ${target ? "ring-2 ring-sky-500" : ""} ${dragEpic === epic.root.key ? "opacity-40" : ""} ${merging ? "cursor-pointer ring-2 ring-dashed ring-violet-400" : ""}`}
                  draggable={!isOpen}
                  key={epic.root.key}
                  onClick={merging ? () => mergeEpic(pending.from, epic.root.key) : undefined}
                  onDragEnd={() => setDragEpic(undefined)}
                  onDragLeave={() => setOver(undefined)}
                  onDragOver={(event) => {
                    event.preventDefault();
                    setOver(epic.root.key);
                  }}
                  onDragStart={() => setDragEpic(epic.root.key)}
                  onDrop={(event) => {
                    event.preventDefault();
                    setOver(undefined);
                    if (dragIssues !== undefined) void moveIssues(dragIssues, epic.root.key);
                    else if (dragEpic !== undefined && dragEpic !== epic.root.key)
                      void reorderEpic(dragEpic, epic.root.key);
                    setDragEpic(undefined);
                    setDragIssues(undefined);
                  }}
                  style={{ borderLeft: `6px solid ${epic.color}` }}
                >
                  <div className="flex items-center gap-3 px-3 py-2">
                    <span className={`w-6 text-right text-lg tabular-nums ${textMutedOnCanvas}`}>
                      {index + 1}
                    </span>
                    <div className="min-w-0 flex-1">
                      {pending?.kind === "rename" && pending.key === epic.root.key ? (
                        <input
                          autoFocus
                          className={`w-full rounded border px-1 font-semibold ${borderDefault}`}
                          onBlur={() => setPending(undefined)}
                          onChange={(event) =>
                            setPending({ ...pending, title: event.target.value })
                          }
                          onKeyDown={(event) => {
                            if (event.key === "Escape") setPending(undefined);
                            if (event.key === "Enter" && pending.title.trim()) {
                              setPending(undefined);
                              void run(`Renamed an epic to “${pending.title}”`, [
                                patch(epic.root.key, { title: pending.title.trim() }),
                              ]);
                            }
                          }}
                          value={pending.title}
                        />
                      ) : (
                        <button
                          className={`block w-full truncate text-left font-semibold ${textPrimaryOnCanvas}`}
                          onClick={(event) => {
                            event.stopPropagation();
                            if (!merging) setOpen(toggle(open, epic.root.key));
                          }}
                          type="button"
                        >
                          {epic.root.title}
                        </button>
                      )}
                      <div className="mt-1 flex h-1.5 overflow-hidden rounded-full bg-slate-100">
                        <span style={{ background: epic.color, flex: epicRunning.length }} />
                        <span style={{ background: `${epic.color}66`, flex: waiting.length }} />
                        <span style={{ flex: rest }} />
                      </div>
                      <div className={`mt-0.5 truncate text-xs ${textMutedOnCanvas}`}>
                        {waiting[0] === undefined
                          ? epicRunning.length > 0
                            ? "Nothing waiting"
                            : "Nothing open"
                          : `Next: ${waiting[0].title}`}
                      </div>
                    </div>
                    <div className={`shrink-0 text-right text-xs ${textSecondaryOnCanvas}`}>
                      <div>
                        <b>{epicRunning.length}</b> running · <b>{waiting.length}</b> waiting
                      </div>
                      <div className={textMutedOnCanvas}>
                        {epic.issues.length} open
                        {waiting[0] === undefined ? "" : ` · next #${place.get(waiting[0].key)}`}
                      </div>
                    </div>
                    <details
                      className="relative shrink-0"
                      onClick={(event) => event.stopPropagation()}
                    >
                      <summary className={`cursor-pointer list-none px-1 ${textMutedOnCanvas}`}>
                        ⋯
                      </summary>
                      <div
                        className={`absolute right-0 z-10 mt-1 w-48 rounded-lg border bg-white p-1 text-sm shadow ${borderDefault}`}
                      >
                        <MenuItem
                          onClick={() =>
                            setPending({
                              kind: "rename",
                              key: epic.root.key,
                              title: epic.root.title,
                            })
                          }
                        >
                          Rename
                        </MenuItem>
                        <MenuItem
                          onClick={() => setPending({ kind: "merge", from: epic.root.key })}
                        >
                          Merge into another epic…
                        </MenuItem>
                        <MenuItem
                          onClick={() => {
                            setOpen(new Set(open).add(epic.root.key));
                          }}
                        >
                          Split: open it, tick issues, “New epic”
                        </MenuItem>
                        <MenuItem
                          onClick={() =>
                            void startFirst(
                              waiting.map((issue) => issue.key),
                              true
                            )
                          }
                        >
                          Start all its waiting issues first
                        </MenuItem>
                      </div>
                    </details>
                  </div>
                  {isOpen ? (
                    <ul className="px-3 pb-2">{epic.issues.map((issue) => issueRow(issue))}</ul>
                  ) : null}
                </li>
              );
            })}
          </ol>
          {grouped.loose.length === 0 ? null : (
            <div className={`mt-4 rounded-xl border border-dashed p-3 ${borderDefault}`}>
              <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>In no epic</h2>
              <ul>{grouped.loose.map((issue) => issueRow(issue))}</ul>
            </div>
          )}
        </section>
        <aside aria-label="Next to start" className={`self-start rounded-xl p-3 ${surfaceMutedBg}`}>
          <h2 className={`mb-1 text-sm font-semibold ${textPrimaryOnCanvas}`}>Next to start</h2>
          <ol>
            {line
              .slice(0, 25)
              .map((issue) =>
                issueRow(
                  issue,
                  hasLabel(issue, FIRST_LABEL) ? "#ea580c" : epicOfKey.get(issue.key)?.color,
                  true
                )
              )}
          </ol>
          {line.length > 25 ? (
            <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>…and {line.length - 25} more</p>
          ) : null}
          {log.length === 0 ? null : (
            <>
              <h2 className={`mt-4 mb-1 text-sm font-semibold ${textPrimaryOnCanvas}`}>
                Your changes
              </h2>
              <ul className={`text-xs ${textSecondaryOnCanvas}`}>
                {log.map((entry, index) => (
                  <li className="py-0.5" key={`${index}-${entry}`}>
                    {entry}
                  </li>
                ))}
              </ul>
            </>
          )}
        </aside>
      </div>

      {pending?.kind === "merge" ? (
        <Bar>
          <span>
            Merge <b>“{title(pending.from)}”</b> into which epic? Click it.
          </span>
          <BarButton onClick={() => setPending(undefined)}>Cancel</BarButton>
        </Bar>
      ) : selected.size > 0 ? (
        <Bar>
          <span>
            <b>{selected.size}</b> selected
          </span>
          <BarButton
            onClick={() => void startFirst([...selected], true).then(() => setSelected(new Set()))}
          >
            Start first
          </BarButton>
          <select
            className="rounded-lg bg-white/15 px-2 py-1"
            onChange={(event) => {
              if (event.target.value) {
                void moveIssues([...selected], event.target.value).then(() =>
                  setSelected(new Set())
                );
              }
            }}
            value=""
          >
            <option value="">Move to epic…</option>
            {grouped.epics.map((epic, index) => (
              <option key={epic.root.key} value={epic.root.key}>
                {index + 1}. {epic.root.title}
              </option>
            ))}
          </select>
          <BarButton onClick={() => void newEpic([...selected])}>New epic from these</BarButton>
          {[...selected].some((key) =>
            hasLabel(byKey.get(key) ?? ({} as IssueSummary), FIRST_LABEL)
          ) ? (
            <BarButton
              onClick={() =>
                void startFirst([...selected], false).then(() => setSelected(new Set()))
              }
            >
              Not first
            </BarButton>
          ) : null}
          <BarButton onClick={() => setSelected(new Set())}>Clear</BarButton>
        </Bar>
      ) : null}
    </div>
  );
}

function Stat({ label, value }: { label: string; value: number }): ReactNode {
  return (
    <span className={`text-sm ${textMutedOnCanvas}`}>
      <b className={`text-2xl font-semibold tabular-nums ${textPrimaryOnCanvas}`}>{value}</b>{" "}
      {label}
    </span>
  );
}

function MenuItem({ children, onClick }: { children: ReactNode; onClick: () => void }): ReactNode {
  return (
    <button
      className="block w-full rounded px-2 py-1 text-left hover:bg-slate-100"
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

function Bar({ children }: { children: ReactNode }): ReactNode {
  return (
    <div className="fixed bottom-4 left-1/2 z-20 flex -translate-x-1/2 items-center gap-3 rounded-2xl bg-slate-900 px-4 py-2 text-sm text-white shadow-xl">
      {children}
    </div>
  );
}

function BarButton({ children, onClick }: { children: ReactNode; onClick: () => void }): ReactNode {
  return (
    <button
      className="rounded-lg bg-white/15 px-3 py-1 hover:bg-white/25"
      onClick={onClick}
      type="button"
    >
      {children}
    </button>
  );
}
