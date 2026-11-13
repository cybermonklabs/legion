import { useSyncExternalStore } from "react";

import { ApiError, api, goingUndoRequest, goingWriteRequest } from "../../api/client";
import type { GoingUndoLine, GoingWriteInput, GoingWriteLine } from "../../api/types";
import { WriteDiscardedError } from "../writes/keyed-write";
import { plainWrite, type Write, type Writes, writeUnsettled } from "../writes/write-store";
import { refusalText } from "./offer";

/**
 * The bar's turns: what each run of the bar wrote and what its undo did, kept outside the bar so
 * that the undo key (`$mod+z`, `app.tsx`) works from any page and the bar draws the lines of a
 * run it is showing whoever sent them.
 *
 * Every write of a turn is one write of the page's store (`Writes.run`), one request per
 * invocation to `POST /api/v1/going/runs/{id}/writes`, all on the run's own line
 * (`goingRunTarget`): they go out one after another, so a write still waiting its turn has never
 * reached Dispatch and an undo stops it there (`Writes.discard`) rather than sending it to undo
 * it. A turn's undo waits for the writes that are out, since each may have reached Dispatch, and
 * then asks Dispatch to undo every write of the run that went through
 * (`POST /api/v1/going/runs/{id}/undo`), each by the undo its catalogue row states.
 */

/** How a write of the bar came to run: at once on submit, as the decision chose it at or above
 *  the action threshold (`auto`); picked by the person among the offers (`person`); or confirmed
 *  by a person whose setting asks before every write (`confirmed`). */
export type GoingApproval = GoingWriteInput["approved"];

/** The two writes the bar never runs at once, whatever the decision's confidence: a request
 *  handed to a live agent spends that agent a turn, and a wrong new issue is a husk. Each goes
 *  through the bar's own offer, which always asks (`SendToAgentOffer`, `NewIssueOffer`); Dispatch
 *  refuses `auto` for both as well. */
export const ALWAYS_ASK: Readonly<Record<string, true>> = {
  dispatch_issue: true,
  dispatch_send_to_agent: true,
};

/** Where one write of a turn stands: out or waiting its turn, landed, refused by Dispatch, or
 *  stopped by an undo before it was sent. */
export type TurnLineState = "sending" | "done" | "refused" | "stopped";

/** One write of a turn, as its line shows it. Before Dispatch answers, the line names what the
 *  invocation's arguments name; once it has, the item, the field, the value before and the value
 *  now that Dispatch read (`written`), or Dispatch's refusal. */
export interface TurnLine {
  readonly key: string;
  readonly action: string;
  /** The item the arguments name, `""` where they name none. */
  readonly target: string;
  /** What the arguments change, besides the item. */
  readonly change: string;
  readonly state: TurnLineState;
  readonly written: GoingWriteLine | undefined;
  readonly refusal: { readonly code: string | undefined; readonly message: string } | undefined;
  /** How the write's undo went, once a turn's undo answered for it. */
  readonly undo: GoingUndoLine | undefined;
}

/** Where a turn's undo stands: out, answered with one line per write it undid, or refused as a
 *  whole. `stopped` counts the writes it stopped before they were sent. */
export interface TurnUndo {
  readonly state: "undoing" | "undone" | "refused";
  readonly lines: readonly GoingUndoLine[];
  readonly stopped: number;
  readonly message: string | undefined;
}

/** One run of the bar, as this page wrote and undid it. */
export interface Turn {
  readonly runId: string;
  readonly words: string;
  readonly lines: readonly TurnLine[];
  readonly undo: TurnUndo | undefined;
}

/** The undo the undo key made last, which the shell says, since the key works from any page. */
export interface LastUndo extends TurnUndo {
  readonly runId: string;
  readonly words: string;
}

const TARGET_PREFIX = "going-run:";

/** The line every write of one run goes out on, in press order. */
function goingRunTarget(runId: string): string {
  return `${TARGET_PREFIX}${runId}`;
}

function runOfTarget(target: string | undefined): string | undefined {
  return target?.startsWith(TARGET_PREFIX) === true
    ? target.slice(TARGET_PREFIX.length)
    : undefined;
}

let turns: ReadonlyMap<string, Turn> = new Map();
let lastUndo: LastUndo | undefined;
/** The runs whose undo, out now, the shell says as it goes: an undo the key made, or one already
 *  out from a turn's Undo that the key was pressed during. */
const followed = new Set<string>();
/** Each write of a turn this page has made that is queued, out or landed, by run and line key
 *  (`run\naction:invocation`): a second submit of the turn answers it rather than writing it
 *  again. A write refused, or stopped before it was sent, leaves it, so making the write again is
 *  the person's. */
const made = new Map<string, Promise<GoingWriteLine | null>>();
const listeners = new Set<() => void>();

function publish(): void {
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function changeTurn(runId: string, words: string, change: (turn: Turn) => Turn): void {
  const current = turns.get(runId) ?? { lines: [], runId, undo: undefined, words };
  const next = new Map(turns);
  next.set(runId, change(current));
  turns = next;
  publish();
}

function changeLine(runId: string, key: string, change: (line: TurnLine) => TurnLine): void {
  changeTurn(runId, "", (turn) => ({
    ...turn,
    lines: turn.lines.map((line) => (line.key === key ? change(line) : line)),
  }));
}

/** The turn of `runId` as this page holds it. */
export function turnOf(runId: string): Turn | undefined {
  return turns.get(runId);
}

/** The turn of `runId`, drawn again whenever a line of it changes. */
export function useTurn(runId: string | undefined): Turn | undefined {
  return useSyncExternalStore(subscribe, () =>
    runId === undefined ? undefined : turns.get(runId)
  );
}

/** The undo the undo key made last, until the person dismisses what the shell says of it. */
export function useLastUndo(): LastUndo | undefined {
  return useSyncExternalStore(subscribe, () => lastUndo);
}

export function dismissLastUndo(): void {
  lastUndo = undefined;
  publish();
}

/** Forgets every turn: a test's reset. */
export function forgetTurns(): void {
  turns = new Map();
  lastUndo = undefined;
  followed.clear();
  made.clear();
  publish();
}

/** An action as the bar names it: the catalogue's name without its prefix. */
export function actionName(action: string): string {
  return action.replace(/^dispatch_/, "").replaceAll("_", " ");
}

/** An action and the item it acts on, as a line names them: `issue update CORE-1`. */
export function actionOn(action: string, target: string): string {
  return `${actionName(action)}${target === "" ? "" : ` ${target}`}`;
}

/** The arguments that name the item a write acts on, in the order the bar reads them. */
const TARGET_ARGUMENTS = [
  "issue",
  "ask",
  "comment",
  "artifact",
  "in_reply_to",
  "retract",
  "session",
  "project",
  "key",
] as const;

/** The item an invocation's arguments name, `""` where they name none. */
export function targetOf(args: Readonly<Record<string, unknown>>): string {
  for (const name of TARGET_ARGUMENTS) {
    const value = args[name];
    if (typeof value === "string" && value !== "") return value;
  }
  return "";
}

const SHOWN_MAX = 80;

function cut(text: string): string {
  return text.length > SHOWN_MAX ? `${text.slice(0, SHOWN_MAX - 1)}…` : text;
}

/** A value as one line shows it: text as it is, nothing as `none`, anything else as its JSON. */
function shown(value: unknown): string {
  if (value === undefined || value === null) return "none";
  if (typeof value === "string") return cut(value);
  return cut(JSON.stringify(value));
}

/** The value a write read before or after it, for the field it names: a read of an issue answers
 *  an object keyed by the fields the write changed, and the line shows the fields' own values. */
export function shownValue(value: unknown, field: string): string {
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    const record = value as Readonly<Record<string, unknown>>;
    const fields = field.split(", ").filter((name) => name !== "");
    if (fields.length > 0 && fields.every((name) => name in record)) {
      return fields.map((name) => shown(record[name])).join(", ");
    }
  }
  return shown(value);
}

/** What an invocation's arguments change, besides the item they name. */
function changeOf(args: Readonly<Record<string, unknown>>, target: string): string {
  return cut(
    Object.entries(args)
      .filter(([, value]) => value !== target)
      .map(([name, value]) => `${name} ${shown(value)}`)
      .join(", ")
  );
}

/** One write of a turn to make: the invocation of an offer of the run's proposal, the fill of
 *  the argument it was missing, the arguments the write sends over the invocation's own, and how
 *  it came to run. */
export interface GoingWriteSpec {
  readonly runId: string;
  readonly words: string;
  readonly action: string;
  readonly invocation: number;
  /** The invocation's own arguments, which its line is named from. */
  readonly arguments: Readonly<Record<string, unknown>>;
  readonly approved: GoingApproval;
  /** The argument the invocation was missing, and the value the person gave it. */
  readonly fill?: { readonly field: string; readonly value: unknown };
  readonly replace?: Readonly<Record<string, unknown>>;
}

/**
 * Makes one write of a turn and keeps its line: one write of the store on the run's line, inline
 * since the line says its refusal itself. Answers Dispatch's line for it (`null` for a landing it
 * had no answer to give for) or rejects with its refusal, or with `WriteDiscardedError` where an
 * undo stopped it before it was sent. An invocation of the run this page already has queued, out
 * or landed is not written again: the call answers that write's own answer, and sends nothing.
 */
export function runGoingWrite(
  writes: Writes,
  spec: GoingWriteSpec
): Promise<GoingWriteLine | null> {
  const key = `${spec.action}:${spec.invocation}`;
  const invocation = `${spec.runId}\n${key}`;
  const already = made.get(invocation);
  if (already !== undefined) return already;
  const named: Record<string, unknown> = { ...spec.arguments, ...spec.replace };
  if (spec.fill !== undefined) named[spec.fill.field] = spec.fill.value;
  const target = targetOf(named);
  const line: TurnLine = {
    action: spec.action,
    change: changeOf(named, target),
    key,
    refusal: undefined,
    state: "sending",
    target,
    undo: undefined,
    written: undefined,
  };
  changeTurn(spec.runId, spec.words, (turn) => ({
    ...turn,
    lines: [...turn.lines.filter((kept) => kept.key !== key), line],
  }));
  const input: GoingWriteInput = {
    approved: spec.approved,
    invocation: spec.invocation,
    offer: spec.action,
    ...(spec.replace === undefined ? {} : { arguments: { ...spec.replace } }),
    ...(spec.fill === undefined ? {} : { fill: spec.fill.value }),
  };
  const write: Promise<GoingWriteLine | null> = writes
    .run({
      detail: null,
      inline: true,
      kind: plainWrite,
      label: actionOn(spec.action, target),
      presentation: "indicator",
      request: goingWriteRequest(spec.runId, input),
      target: goingRunTarget(spec.runId),
    })
    .then(
      (written) => {
        changeLine(spec.runId, key, (kept) => ({
          ...kept,
          state: "done",
          written: written ?? undefined,
        }));
        return written;
      },
      (error: unknown) => {
        changeLine(spec.runId, key, (kept) =>
          error instanceof WriteDiscardedError
            ? { ...kept, state: "stopped" }
            : {
                ...kept,
                refusal: {
                  code: error instanceof ApiError ? error.code : undefined,
                  message: refusalText(error, "the write did not reach Dispatch."),
                },
                state: "refused",
              }
        );
        if (made.get(invocation) === write) made.delete(invocation);
        throw error;
      }
    );
  made.set(invocation, write);
  return write;
}

/** Makes every write of `specs`, in order on the run's line, each line saying how it went. */
export async function runGoingWrites(
  writes: Writes,
  specs: readonly GoingWriteSpec[]
): Promise<void> {
  await Promise.allSettled(specs.map((spec) => runGoingWrite(writes, spec)));
}

/** Stops every write of the runs `inRun` names that waits its turn and was never sent: it has not
 *  reached Dispatch, so undoing the turn is not sending it. Answers how many it stopped. */
function stopWaiting(writes: Writes, inRun: (runId: string) => boolean): number {
  let stopped = 0;
  for (const write of writes.rows()) {
    const runId = runOfTarget(write.target);
    if (runId === undefined || !inRun(runId) || write.status !== "queued" || write.sent) continue;
    if (writes.discard(write.id)) stopped += 1;
  }
  return stopped;
}

/** Settles once no write of the runs `inRun` names is out: each may have reached Dispatch, so the
 *  undo waits for what Dispatch made of it. */
function writesSettled(writes: Writes, inRun: (runId: string) => boolean): Promise<void> {
  const out = () =>
    writes.rows().some((write) => {
      const runId = runOfTarget(write.target);
      return runId !== undefined && inRun(runId) && writeUnsettled(write);
    });
  if (!out()) return Promise.resolve();
  const { promise, resolve } = Promise.withResolvers<void>();
  const stop = writes.subscribe(() => {
    if (out()) return;
    stop();
    resolve();
  });
  return promise;
}

/** The reason an issue the turn created is closed with, and the note an ask's retraction and a
 *  document's restoring version carry. */
function undoReason(words: string): string | undefined {
  return words === "" ? undefined : `Undone from the command bar: “${words}”`;
}

function setUndo(runId: string, words: string, undo: TurnUndo, notice: boolean): void {
  if (notice) followed.add(runId);
  if (followed.has(runId)) {
    lastUndo = { ...undo, runId, words };
    if (undo.state !== "undoing") followed.delete(runId);
  }
  changeTurn(runId, words, (turn) => ({
    ...turn,
    lines: turn.lines.map((line) => {
      const answered = undo.lines.find((undone) => undone.write_id === line.written?.write_id);
      return answered === undefined ? line : { ...line, undo: answered };
    }),
    undo,
  }));
}

/**
 * Undoes the turn of `runId`: every write of it still waiting its turn is stopped before it is
 * sent, every write that is out is waited for, and Dispatch undoes every write of it that went
 * through, one line each, a field another writer changed since refused in its own line and the
 * rest undone. `notice` says the undo in the shell as well (the undo key's, from any page).
 */
export async function undoGoingTurn(
  writes: Writes,
  runId: string,
  words = "",
  notice = false
): Promise<void> {
  // The words the turn's undo names: the caller's, else the turn's own.
  const said = words === "" ? (turns.get(runId)?.words ?? "") : words;
  const out = turns.get(runId)?.undo;
  if (out?.state === "undoing") {
    // An undo of this turn is out already, so this is that undo and not a second: the shell,
    // where this one would have said itself, follows that one and settles when it does.
    if (notice) {
      followed.add(runId);
      lastUndo = { ...out, runId, words: said };
      publish();
    }
    return;
  }
  const inRun = (id: string) => id === runId;
  const stopped = stopWaiting(writes, inRun);
  setUndo(runId, said, { lines: [], message: undefined, state: "undoing", stopped }, notice);
  await writesSettled(writes, inRun);
  const reason = undoReason(said);
  try {
    const answer = await writes.run({
      detail: null,
      inline: true,
      kind: plainWrite,
      label: "Undo the command bar's turn",
      presentation: "indicator",
      request: goingUndoRequest(runId, reason === undefined ? {} : { reason }),
      target: goingRunTarget(runId),
    });
    setUndo(
      runId,
      said,
      {
        lines: answer?.lines ?? [],
        message:
          answer === null
            ? "Dispatch undid the turn and kept no answer naming each write."
            : undefined,
        state: "undone",
        stopped,
      },
      notice
    );
  } catch (error) {
    setUndo(
      runId,
      said,
      {
        lines: [],
        message: refusalText(error, "the undo did not reach Dispatch."),
        state: "refused",
        stopped,
      },
      notice
    );
  }
}

/**
 * The undo key: undoes the person's newest turn of the bar from whatever page they are on. A turn
 * this page is still writing is that turn; with none, it is the newest undoable run Dispatch
 * holds for them (`GET /api/v1/me/going-runs?undoable=true&limit=1`), so the key works after a
 * reload and from a page the bar was never opened on.
 */
export async function undoNewestGoingTurn(writes: Writes): Promise<void> {
  // A press while an undo is out is that undo's: a second would undo the turn again.
  if (lastUndo?.state === "undoing") return;
  const writing = writes
    .rows()
    .filter((write) => runOfTarget(write.target) !== undefined && writeUnsettled(write))
    .reduce<Write | undefined>(
      (newest, write) =>
        newest === undefined || write.pressedAt > newest.pressedAt ? write : newest,
      undefined
    );
  const local = runOfTarget(writing?.target);
  if (local !== undefined) {
    await undoGoingTurn(writes, local, "", true);
    return;
  }
  // What the key says before it knows which turn is the person's.
  const say = (state: TurnUndo["state"], message: string | undefined) => {
    lastUndo = { lines: [], message, runId: "", state, stopped: 0, words: "" };
    publish();
  };
  say("undoing", undefined);
  try {
    const page = await api.myGoingRuns({ limit: 1, undoable: true });
    const run = page.rows?.[0];
    if (run === undefined) {
      say("undone", "Nothing to undo: no turn of the command bar has a write left to undo.");
      return;
    }
    await undoGoingTurn(writes, run.id, run.words, true);
  } catch (error) {
    say("refused", refusalText(error, "Dispatch did not say which turn is yours to undo."));
  }
}

/** How one write's undo went, in the words a line says it in. */
export function undoneText(line: GoingUndoLine): string {
  if (!line.outcome.ok) {
    const fields = (line.fields ?? []).filter((field) => !field.ok);
    return fields.length === 0
      ? `not undone: ${line.outcome.message}`
      : `not undone: ${fields.map((field) => `${field.field}: ${field.message}`).join("; ")}`;
  }
  switch (line.undo) {
    case "write_back":
      return "written back";
    case "reverse":
      return "reversed";
    case "close_created":
      return "closed";
    case "trace":
      return "kept, and marked undone";
  }
}
