import { useSyncExternalStore } from "react";

import { api, apiErrorMessage, REQUEST_DEADLINE_MS, type WriteBounds } from "../../api/client";
import type {
  GoingContextInput,
  GoingConversation,
  GoingRun,
  GoingRunInput,
} from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { type PanelEvent, type PanelKeys, type PanelPlace, panelNext } from "./panel-states";
import { type RunFrames, STARTED_RUN, stateOfRecord, withRunFrame } from "./run-frames";
import { type RunState, runEnded, runNext } from "./run-states";

/**
 * The command bar's conversation, which outlives every open of the bar: where it is on the screen
 * (`panel-states.ts`), its runs and how each stands as its frames have said (`run-frames.ts`),
 * the level the next run takes, the item a click on the page made the next request's context,
 * the rows of the open plan the page draws, and the person's ticks and edits of that plan. It is
 * one store of the page, held outside React so the bar over the page, the panel at the side, the
 * chip and the five pages that draw a plan all read the one conversation. Each run it holds is
 * followed on its stream until it ends, whether or not the bar is open: a run goes on in the
 * server when the bar closes, and the chip says how it stands.
 */

/** One run as the page knows it: its record (its words, its conversation, and how it stood when
 *  it was started, listed in History or stopped), what its frames say, whether it was started on
 *  this page, whether the person has seen how it ended, and why its stream could not be read. */
export interface RunEntry {
  readonly id: string;
  readonly record: GoingRun;
  readonly frames: RunFrames;
  /** How many frames of the run's stream have been read. */
  readonly read: number;
  readonly startedHere: boolean;
  readonly seen: boolean;
  readonly unreadable: string | undefined;
}

/** The person's answer to the open plan so far: the rows they unticked, and the values they
 *  edited, by row, for the plan numbered `plan` of `run`. */
export interface PlanDraft {
  readonly run: string;
  readonly plan: number;
  readonly unticked: ReadonlySet<number>;
  readonly edits: ReadonlyMap<number, Readonly<Record<string, unknown>>>;
}

/** The item a click on the page named, and the page it was clicked on. */
export interface ClickedItem {
  readonly context: GoingContextInput;
  readonly path: string;
}

/** Everything the store holds. */
export interface GoingState {
  readonly place: PanelPlace;
  /** The open conversation's runs, oldest first; undefined while none is open. */
  readonly conversation: readonly string[] | undefined;
  readonly runs: ReadonlyMap<string, RunEntry>;
  /** How much work the next run may do (R10): Fast, Standard or Deep. */
  readonly level: GoingRun["level"];
  /** The item a click on the page named. */
  readonly clicked: ClickedItem | undefined;
  /** The plan rows each page surface draws, by the surface's id. */
  readonly onPage: ReadonlyMap<string, readonly number[]>;
  readonly draft: PlanDraft | undefined;
  /** Whether a run's start is out. */
  readonly starting: boolean;
  /** Why the last start, stop or Continue was refused, in Dispatch's words. */
  readonly refusal: string | undefined;
}

const INITIAL: GoingState = {
  clicked: undefined,
  conversation: undefined,
  draft: undefined,
  level: "standard",
  onPage: new Map(),
  place: "closed",
  refusal: undefined,
  runs: new Map(),
  starting: false,
};

let state: GoingState = INITIAL;
const listeners = new Set<() => void>();
/** The streams being read, by run. */
const following = new Map<string, AbortController>();
/** The reading frame of each run (`RunFrames.planReads`) at which its plan was last read again
 *  (`claimPlanRead`). */
const planReadsAt = new Map<string, number>();

/** Whether `run` is shown as it ended and nothing more of it comes. */
function ended(run: RunEntry): boolean {
  return runEnded(shownState(run));
}

/** `of` with each ended run of its open conversation marked seen, so the chip no longer names
 *  it. A run that works or waits is not marked: the chip names it again once the conversation
 *  leaves the screen. */
function withSeen(of: GoingState): GoingState {
  let runs: Map<string, RunEntry> | undefined;
  for (const id of of.conversation ?? []) {
    const run = of.runs.get(id);
    if (run === undefined || run.seen || !ended(run)) continue;
    runs ??= new Map(of.runs);
    runs.set(id, { ...run, seen: true });
  }
  return runs === undefined ? of : { ...of, runs };
}

/** `of` without the runs of each conversation that is not open once every run of it has ended,
 *  been seen and is no longer followed: the chip names none of them, and History's Open reads
 *  them again from their streams. */
function withoutDone(of: GoingState): GoingState {
  const open = new Set(of.conversation ?? []);
  const kept = new Set<string>();
  for (const run of of.runs.values()) {
    if (open.has(run.id) || !run.seen || following.has(run.id) || !ended(run)) {
      kept.add(run.record.conversation);
    }
  }
  const runs = new Map([...of.runs].filter(([, run]) => kept.has(run.record.conversation)));
  return runs.size === of.runs.size ? of : { ...of, runs };
}

/** Holds `next`. While the conversation is on the screen the person sees how each of its runs
 *  ended; once another conversation is open, the ones done with are let go. */
function set(next: GoingState): void {
  const shown = next.place === "closed" ? next : withSeen(next);
  state = shown.conversation === state.conversation ? shown : withoutDone(shown);
  for (const listener of listeners) {
    listener();
  }
}

function withRun(id: string, change: (run: RunEntry) => RunEntry): void {
  const run = state.runs.get(id);
  if (run === undefined) {
    return;
  }
  set({ ...state, runs: new Map(state.runs).set(id, change(run)) });
}

/** The bounds of one request of the bar: a key of its own, the asker's login, and the page's
 *  silence deadline, as the one keyed path takes every write of the page. */
export function goingBounds(login: string | undefined): WriteBounds {
  return {
    deadlineMs: REQUEST_DEADLINE_MS,
    key: newSendKey(),
    ...(login === undefined ? {} : { login }),
  };
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The store as it stands now. */
export function goingState(): GoingState {
  return state;
}

/** The store, read by a component and drawn again on each change. */
export function useGoing(): GoingState {
  return useSyncExternalStore(subscribe, goingState, goingState);
}

/** One part of the store, read by a component and drawn again only when that part changes:
 *  `select` answers a value, or a part the store holds, never a new object. */
export function useGoingValue<T>(select: (of: GoingState) => T): T {
  const read = () => select(state);
  return useSyncExternalStore(subscribe, read, read);
}

/** A run's state as the page shows it: what its frames say once any came, else what its record
 *  said. */
export function shownState(run: RunEntry): RunState {
  return run.read > 0 ? run.frames.state : stateOfRecord(run.record.state, run.record.stop_reason);
}

/** The open conversation's newest run. */
export function newestRun(of: GoingState = state): RunEntry | undefined {
  const id = of.conversation?.at(-1);
  return id === undefined ? undefined : of.runs.get(id);
}

/** Whether the open conversation's newest run works now: what Stop, and Esc in the empty box,
 *  stop. */
export function conversationWorking(of: GoingState = state): boolean {
  const run = newestRun(of);
  return run !== undefined && shownState(run) === "working";
}

/** Reads the run's stream until it ends, folding each frame into the run. The stream starts at
 *  the run's first frame, so its first frame starts the run's frames again: a run read before
 *  is not drawn twice. */
function follow(id: string): void {
  if (following.has(id)) {
    return;
  }
  const controller = new AbortController();
  following.set(id, controller);
  let read = 0;
  api
    .streamGoingRunEvents(
      id,
      (name, data) => {
        const first = read === 0;
        read += 1;
        withRun(id, (run) => ({
          ...run,
          frames: withRunFrame(first ? STARTED_RUN : run.frames, name, data),
          read,
        }));
      },
      controller.signal
    )
    .catch((error: unknown) => {
      if (controller.signal.aborted) {
        return;
      }
      withRun(id, (run) => ({
        ...run,
        unreadable: apiErrorMessage(error, "The run's steps could not be read."),
      }));
    })
    .finally(() => {
      if (following.get(id) === controller) {
        following.delete(id);
      }
    });
}

/** Moves the conversation as the person's key, press or move asks (`panelNext`), and stops the
 *  run where that says to. */
export function movePanel(event: PanelEvent, login: string | undefined, keys?: PanelKeys): void {
  const transition = panelNext(state.place, event, keys);
  setPlace(transition.place);
  const run = newestRun();
  if (transition.stopRun === true && run !== undefined) {
    stopRun(run.id, login);
  }
}

/** Puts the conversation where `place` says: the shell's keys, the bar's Close and the panel's
 *  Close and move. */
export function setPlace(place: PanelPlace): void {
  if (place !== state.place) {
    set({ ...state, place });
  }
}

/** Sets the level the next run takes. */
export function setLevel(level: GoingRun["level"]): void {
  if (level !== state.level) {
    set({ ...state, level });
  }
}

/** Whether two contexts name the same fields with the same values. */
function sameContext(a: GoingContextInput, b: GoingContextInput): boolean {
  const fields = Object.keys(a) as (keyof GoingContextInput)[];
  return fields.length === Object.keys(b).length && fields.every((field) => a[field] === b[field]);
}

/** Names the item a click on the page was on, which the next request at the side is about. */
export function setClicked(context: GoingContextInput, path: string): void {
  const now = state.clicked;
  if (now !== undefined && now.path === path && sameContext(now.context, context)) {
    return;
  }
  set({ ...state, clicked: { context, path } });
}

/** Closes the open conversation, so the next request starts another. Its runs are still
 *  followed, and History lists it. */
export function newConversation(): void {
  set({ ...state, conversation: undefined, draft: undefined, refusal: undefined });
}

function entryOf(record: GoingRun, startedHere: boolean): RunEntry {
  return {
    frames: STARTED_RUN,
    id: record.id,
    read: 0,
    record,
    seen: !startedHere,
    startedHere,
    unreadable: undefined,
  };
}

/**
 * Starts a run of `words` with the page's `context`, and answers whether it started. It follows
 * up the open conversation's newest run where that run has ended (`POST /api/v1/going/runs` with
 * `continues`), and starts a conversation of its own otherwise: a run that still works or waits
 * goes on beside it. A run of the `kind` `new_issue` is the line File as a new issue: a Fast run
 * that files the words as one new issue at once, so it takes no level of its own.
 */
export async function startRun(
  words: string,
  context: GoingContextInput,
  login: string | undefined,
  continues?: string,
  kind?: GoingRunInput["kind"]
): Promise<boolean> {
  const newest = newestRun();
  const follows = continues ?? (newest !== undefined && ended(newest) ? newest.id : undefined);
  set({ ...state, refusal: undefined, starting: true });
  try {
    const run = await api.createGoingRun(
      {
        context,
        words,
        ...(kind === undefined ? { level: state.level } : { kind }),
        ...(follows === undefined ? {} : { continues: follows }),
      },
      goingBounds(login)
    );
    const entry = entryOf(run, true);
    const conversation =
      follows !== undefined && state.conversation !== undefined ? state.conversation : [];
    set({
      ...state,
      conversation: [...conversation, run.id],
      draft: undefined,
      runs: new Map(state.runs).set(run.id, entry),
      starting: false,
    });
    follow(run.id);
    return true;
  } catch (error) {
    set({
      ...state,
      refusal: apiErrorMessage(error, "The run did not start."),
      starting: false,
    });
    return false;
  }
}

/** Stops the run at once (`POST /api/v1/going/runs/{id}/stop`); its end frame says it stopped. */
export function stopRun(id: string, login: string | undefined): void {
  const run = state.runs.get(id);
  if (run === undefined || runNext(shownState(run), "stop").send !== "stop") {
    return;
  }
  api.stopGoingRun(id, goingBounds(login)).then(
    (stopped) => withRun(id, (entry) => ({ ...entry, record: stopped })),
    (error: unknown) => set({ ...state, refusal: apiErrorMessage(error, "The run did not stop.") })
  );
}

/** Continue on a run a bound of its level stopped: a follow-up of its conversation with a new
 *  budget of the same level. It answers whether the follow-up started. */
export function continueRun(
  id: string,
  context: GoingContextInput,
  login: string | undefined
): Promise<boolean> {
  const run = state.runs.get(id);
  if (run === undefined || runNext(shownState(run), "continue").send !== "continue") {
    return Promise.resolve(false);
  }
  return startRun("Continue", context, login, id);
}

/** Opens a conversation History lists: its runs, oldest first, each read from its stream's first
 *  frame unless its frames were read to its end already, and the conversation shown over the page
 *  where nothing shows it yet. */
export function openConversation(conversation: GoingConversation): void {
  const records = [...(conversation.runs ?? [])].reverse();
  const runs = new Map(state.runs);
  const unread: string[] = [];
  for (const record of records) {
    const known = runs.get(record.id);
    runs.set(record.id, known === undefined ? entryOf(record, false) : { ...known, record });
    if (known === undefined || known.read === 0 || !ended(known)) {
      unread.push(record.id);
    }
  }
  set({
    ...state,
    conversation: records.map((record) => record.id),
    draft: undefined,
    place: state.place === "closed" ? "over" : state.place,
    refusal: undefined,
    runs,
  });
  for (const id of unread) {
    follow(id);
  }
}

/** Opens the conversation of a run the page already follows (the chip's Open). */
export function openRun(id: string): void {
  const run = state.runs.get(id);
  if (run === undefined) {
    return;
  }
  const conversation = [...state.runs.values()]
    .filter((entry) => entry.record.conversation === run.record.conversation)
    .map((entry) => entry.id);
  set({
    ...state,
    conversation,
    place: state.place === "closed" ? "over" : state.place,
  });
}

/** The plan rows the page surface `surface` draws, or none once it no longer draws any. */
export function drawOnPage(surface: string, rows: readonly number[]): void {
  const now = state.onPage.get(surface);
  if (rows.length === 0 && now === undefined) {
    return;
  }
  if (now !== undefined && now.length === rows.length && now.every((row, at) => row === rows[at])) {
    return;
  }
  const onPage = new Map(state.onPage);
  if (rows.length === 0) onPage.delete(surface);
  else onPage.set(surface, rows);
  set({ ...state, onPage });
}

/** The draft of plan `plan` of `run`: the one `held`, or an empty one where it is of another
 *  plan. */
export function draftOf(
  run: string,
  plan: number,
  held: PlanDraft | undefined = state.draft
): PlanDraft {
  return held !== undefined && held.run === run && held.plan === plan
    ? held
    : { edits: new Map(), plan, run, unticked: new Set() };
}

/** Holds the person's answer to the plan so far. */
export function setDraft(draft: PlanDraft): void {
  set({ ...state, draft });
}

/** Whether the plan of `run` is to be read again at its `planReads`th reading frame: true once
 *  for each new one, however many surfaces draw the plan. */
export function claimPlanRead(run: string, planReads: number): boolean {
  if ((planReadsAt.get(run) ?? 0) >= planReads) {
    return false;
  }
  planReadsAt.set(run, planReads);
  return true;
}

/** Clears the store and stops every stream it reads: a test's reset, and a sign-out's. */
export function resetGoingStore(): void {
  for (const controller of following.values()) {
    controller.abort();
  }
  following.clear();
  planReadsAt.clear();
  set(INITIAL);
}
