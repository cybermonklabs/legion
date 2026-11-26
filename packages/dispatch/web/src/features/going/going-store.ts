import { useEffect, useSyncExternalStore } from "react";

import { api, apiErrorMessage, REQUEST_DEADLINE_MS, type WriteBounds } from "../../api/client";
import type {
  GoingContextInput,
  GoingConversation,
  GoingLine,
  GoingRun,
  GoingUndoLine,
} from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { type PanelEvent, type PanelKeys, type PanelPlace, panelNext } from "./panel-states";
import { type RunFrames, STARTED_RUN, stateOfRecord, withRunFrame } from "./run-frames";
import {
  endWords,
  type RunState,
  runEnded,
  runNext,
  type Withdrawal,
  withdrawalOf,
} from "./run-states";

/**
 * The command bar's conversation, which outlives every open of the bar: where it is on the screen
 * (`panel-states.ts`), its runs and how each stands as its frames have said (`run-frames.ts`),
 * the level the next run takes, the item a click on the page made the next request's context,
 * the rows of the open plan the page draws, the person's ticks and edits of that plan, the words
 * of a run whose start is out, and the words the person sent while a run worked. It is one store
 * of the page, held outside React so the bar over the page, the panel at the side, the chip and
 * the five pages that draw a plan all read the one conversation. Each run it holds is followed on
 * its stream until it ends, whether or not the bar is open: a run goes on in the server when the
 * bar closes, and the chip says how it stands.
 */

/** One run as the page knows it: its record (its words, its conversation, and how it stood when
 *  it was started, listed in History or stopped), what its frames say, whether it was started on
 *  this page, whether the person has seen how it ended, why its stream could not be read, whether
 *  its stream dropped and is being opened again, when the person asked for it, when its work
 *  first came to rest, when it ended, and when each of its blocks came. */
export interface RunEntry {
  readonly id: string;
  readonly record: GoingRun;
  readonly frames: RunFrames;
  /** How many frames of the run's stream have been read. */
  readonly read: number;
  readonly startedHere: boolean;
  readonly seen: boolean;
  readonly unreadable: string | undefined;
  /** Whether the run's stream dropped, such as when the server restarts (R11), and is being
   *  opened again: true from the drop until a connection is answered again, a frame comes, or
   *  the stream ends or is given up. */
  readonly reconnecting: boolean;
  /** When the person asked for the run, in this page's clock: the moment of Enter for a run
   *  started here, else its record's start. Its "Thinking…" timer counts from it. */
  readonly since: number;
  /** When this page first read a frame that left the run no longer working - its end, or its
   *  plan waiting - in this page's clock; undefined until then. "Worked for" counts from `since`
   *  to it for a run whose record was read while it worked. */
  readonly settledAt: number | undefined;
  /** When this page read the run's end, in this page's clock; undefined until then. */
  readonly endedAt: number | undefined;
  /** When this page first read each of the run's block frames, in this page's clock, in the order
   *  of `frames.blocks`: the time a block's read was made, for a run followed while it works. A
   *  stream read again from its first frame keeps these: the block it reads again holds what was
   *  read then. */
  readonly blocksAt: readonly number[];
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

/** A run whose start is out: the person's words, which the bar shows as the question at once
 *  (design v2, rule 4), when they pressed Enter, in this page's clock, and the run it follows up,
 *  whose conversation then stays on the screen above it; undefined for a run that starts a
 *  conversation of its own. It is the start of the conversation on the screen at the press, so
 *  opening another conversation lets it go (`openConversation`, `openRun`, `newConversation`). */
export interface StartingRun {
  readonly words: string;
  readonly at: number;
  readonly follows: string | undefined;
}

/** The words the person sent while `run`, the open conversation's newest run, worked. They wait
 *  until it no longer works, and then go as the answer to its plan or as the next turn
 *  (`useSendQueued`). */
export interface QueuedWords {
  readonly run: string;
  readonly words: string;
}

/** How a run starts besides its words: the run it follows up, its level, and the line on top of
 *  the bar it is started from, the one on the screen at the moment of the person's Enter, which
 *  the run records as its `line`; none for a run started anywhere else (the box under a
 *  conversation, the side panel's box, a Continue, a Retry). */
export interface RunStart {
  readonly continues?: string;
  readonly level?: GoingRun["level"];
  readonly line?: GoingLine;
}

/** Everything the store holds. */
export interface GoingState {
  readonly place: PanelPlace;
  /** Whether the panel at the side is widened to the app's content area (A6), which only the
   *  side has: every other place holds it false. Widening and docking move nothing of the run,
   *  its draft or its unsent words. */
  readonly wide: boolean;
  /** Whether the panel at the side is to take the focus in its box once it is drawn: set by the
   *  handoff from the bar (`handOffToPanel`), and let go as the panel takes the focus. */
  readonly panelFocus: boolean;
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
  /** The run whose start is out; undefined while none is. */
  readonly starting: StartingRun | undefined;
  /** The words sent while the open conversation's newest run worked; undefined while none wait. */
  readonly queued: QueuedWords | undefined;
  /** Why the last start, stop or Continue was refused, in Dispatch's words. */
  readonly refusal: string | undefined;
  /** What each run's Undo answered, by run, once it was pressed on this page: one line for each
   *  change it reversed or kept. Dispatch answers a second press from the same landings, so this
   *  only keeps the answer on the screen while the page lives. Whether an undo landed is not read
   *  from it: the run's record, read again once it answers (`setUndone`), says so. */
  readonly undone: ReadonlyMap<string, readonly GoingUndoLine[]>;
}

const INITIAL: GoingState = {
  clicked: undefined,
  conversation: undefined,
  draft: undefined,
  level: "standard",
  onPage: new Map(),
  panelFocus: false,
  place: "closed",
  queued: undefined,
  refusal: undefined,
  runs: new Map(),
  starting: undefined,
  undone: new Map(),
  wide: false,
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

/** A run's state as the page shows it: stopped once it is withdrawn (`runWithdrawal`), else what
 *  its frames say once any came, else what its record said. */
export function shownState(run: RunEntry): RunState {
  if (runWithdrawal(run) !== undefined) {
    return "stopped_person";
  }
  return run.read > 0 ? run.frames.state : stateOfRecord(run.record.state, run.record.stop_reason);
}

/** The reason `run` stopped or failed with: the withdrawal its record says where it says one, else
 *  its end frame's, else its record's; "" for none. */
export function endReason(run: RunEntry): string {
  const recorded = run.record.stop_reason ?? "";
  return withdrawalOf(recorded) !== undefined ? recorded : (run.frames.end?.reason ?? recorded);
}

/** How `run` is withdrawn, as its record says, else its end frame (`withdrawalOf`); undefined for a
 *  run that is not. The record is read again on each delete (`rereadEveryRun`), so its word is the
 *  newer. A run another writer's delete withdrew (`read_deleted`) shows nothing it wrote: a frame
 *  read before the delete stays in `frames`, and no surface draws it. A run its own plan's delete
 *  withdrew (`run_deleted`) shows its plan's card, whose rows show no word of the post, and nothing
 *  else it wrote. */
export function runWithdrawal(run: RunEntry): Withdrawal | undefined {
  return withdrawalOf(run.record.stop_reason ?? "") ?? withdrawalOf(run.frames.end?.reason ?? "");
}

/** Whether how `run` ended offers Continue (`endWords`): a bound, a refusal of part of what it
 *  found that its end offers Continue on, or its person's own Stop; never a stop its grant ended
 *  or a refusal of the person's words alone. */
export function offersContinue(run: RunEntry): boolean {
  return endWords(shownState(run), endReason(run), run.record.level, "")?.action === "continue";
}

/** Whether how `run` ended offers Try again (`endWords`): a failure whose words the person can
 *  say again; never one that asks for a new conversation, which Enter does not start. */
export function offersRetry(run: RunEntry): boolean {
  return endWords(shownState(run), endReason(run), run.record.level, "")?.action === "retry";
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

/** A conversation's title made from the words of its first question: those words with their
 *  first letter a capital, as a title starts (design v3, states 2 and 11), however the person
 *  typed them. The bar's header, History, the chip, the notice and Continue all name a
 *  conversation by it; the question's own bubble keeps the words as typed. */
export function titleWords(words: string): string {
  const [first = ""] = words;
  return first.toLocaleUpperCase() + words.slice(first.length);
}

/** The open conversation's title: its first question, or the words of the run whose start is out
 *  where that run starts a conversation of its own, as a title (`titleWords`); empty where there
 *  is neither. */
export function conversationTitle(of: GoingState = state): string {
  if (of.starting !== undefined && of.starting.follows === undefined) {
    return titleWords(of.starting.words);
  }
  const first = of.conversation?.[0];
  return titleWords((first === undefined ? undefined : of.runs.get(first))?.record.words ?? "");
}

/** Whether the open conversation's newest run ended done on an answer that asks the person a
 *  question and waits for their answer (R8: the agent asks which changes to make, and shows no
 *  plan), as the frame that ended its answer says (`RunFrames.waitsForPerson`). */
export function asksPerson(of: GoingState = state): boolean {
  const run = newestRun(of);
  return run !== undefined && shownState(run) === "done" && run.frames.waitsForPerson;
}

/** Reads the run's stream until it ends, folding each frame into the run. The stream starts at
 *  the run's first frame, so its first frame starts the run's frames again: a run read before
 *  is not drawn twice. While its stream is dropped and opened again, the run is `reconnecting`;
 *  a frame, an answered connection, the stream's end and giving it up each end that. */
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
        withRun(id, (run) => {
          const frames = withRunFrame(first ? STARTED_RUN : run.frames, name, data);
          const at = Date.now();
          // A frame adds at most one block; the stream read again from its first frame reads
          // each block again, which keeps the time it was first read, as `settledAt` and
          // `endedAt` keep theirs.
          const blocksAt = run.blocksAt;
          return {
            ...run,
            blocksAt: frames.blocks.length > blocksAt.length ? [...blocksAt, at] : blocksAt,
            endedAt: run.endedAt ?? (runEnded(frames.state) ? at : undefined),
            frames,
            read,
            reconnecting: false,
            settledAt: run.settledAt ?? (frames.state === "working" ? undefined : at),
          };
        });
      },
      controller.signal,
      (dropped) => {
        withRun(id, (run) => ({ ...run, reconnecting: dropped }));
      }
    )
    .catch((error: unknown) => {
      if (controller.signal.aborted) {
        return;
      }
      withRun(id, (run) => ({
        ...run,
        reconnecting: false,
        unreadable: apiErrorMessage(error, "The run's steps could not be read."),
      }));
    })
    .finally(() => {
      if (following.get(id) === controller) {
        following.delete(id);
      }
      // A stream that ended, or was given up, is no longer being opened again.
      if (state.runs.get(id)?.reconnecting === true) {
        withRun(id, (run) => ({ ...run, reconnecting: false }));
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

/** Whether the bar opens on `run`'s conversation: the run works, or its plan waits for the
 *  person. */
function holdsTheBar(run: RunEntry): boolean {
  const shown = shownState(run);
  return shown === "working" || shown === "waiting";
}

/** The runs the page holds of `run`'s conversation, oldest first. */
function conversationOf(run: RunEntry): string[] {
  return [...state.runs.values()]
    .filter((entry) => entry.record.conversation === run.record.conversation)
    .map((entry) => entry.id);
}

/**
 * Puts the conversation where `place` says: the shell's keys, the bar's Close and the panel's
 * Close and move. The bar opened over the page from closed (Ctrl-K, `/`, the Search control)
 * opens on the open conversation while its newest run works or its plan waits, its next run's
 * start is out or words wait for it; else on the conversation of this page's newest run that
 * works or waits; else on the search, with no conversation open (design v2, Keys).
 */
export function setPlace(place: PanelPlace): void {
  if (place === state.place) {
    return;
  }
  if (state.place !== "closed" || place !== "over") {
    // Only the side is ever widened, and only it takes the focus the handoff asked for: another
    // place, or a close, docks it and lets that request go.
    set({
      ...state,
      panelFocus: place === "side" && state.panelFocus,
      place,
      wide: place === "side" && state.wide,
    });
    return;
  }
  const open = newestRun();
  if (
    state.starting !== undefined ||
    state.queued !== undefined ||
    (open !== undefined && holdsTheBar(open))
  ) {
    set({ ...state, place });
    return;
  }
  const other = [...state.runs.values()]
    .filter((run) => run.startedHere && holdsTheBar(run))
    .at(-1);
  set({
    ...state,
    conversation: other === undefined ? undefined : conversationOf(other),
    draft: undefined,
    place,
    refusal: undefined,
  });
}

/** Widens the panel at the side to the app's content area (A6), or docks it back to its side
 *  width. Nothing but its width changes: its conversation, its runs, its plan's draft and the
 *  words the person has not sent stay where they are. It does nothing while the conversation is
 *  not at the side. */
export function setWide(wide: boolean): void {
  if (state.place === "side" && wide !== state.wide) {
    set({ ...state, wide });
  }
}

/** Hands the conversation over to the panel at the side from the bar over the page, and has the
 *  panel take the focus in its box once it is drawn and the bar's dialog has given the focus
 *  back (`clearPanelFocus`). It is the one step the bar's Open in panel and its Enter on the line
 *  Ask take once their run has started (`useAsk`): it starts nothing and changes no run. */
export function handOffToPanel(): void {
  set({ ...state, panelFocus: true, place: "side", wide: false });
}

/** Lets the handoff's request for the focus go, as the panel has taken it. */
export function clearPanelFocus(): void {
  if (state.panelFocus) {
    set({ ...state, panelFocus: false });
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
 *  followed, and History lists it; the words that waited for its run, and the question of a start
 *  that is out, are let go: that start's run is followed once Dispatch answers it, and the chip
 *  names it, but it opens nothing over the search. */
export function newConversation(): void {
  set({
    ...state,
    conversation: undefined,
    draft: undefined,
    queued: undefined,
    refusal: undefined,
    starting: undefined,
  });
}

function entryOf(record: GoingRun, startedHere: boolean, since?: number): RunEntry {
  return {
    blocksAt: [],
    endedAt: undefined,
    frames: STARTED_RUN,
    id: record.id,
    read: 0,
    reconnecting: false,
    record,
    seen: !startedHere,
    settledAt: undefined,
    since: since ?? Date.parse(record.created_at),
    startedHere,
    unreadable: undefined,
  };
}

/**
 * Starts a run of `words` with the page's `context`, and answers whether it started. It follows
 * up the open conversation's newest run where that run has ended (`POST /api/v1/going/runs` with
 * `continues`), and starts a conversation of its own otherwise. While the newest run works, the
 * words start no run beside it: they wait for it (`queued`, after any words that wait already),
 * and `useSendQueued` sends them once it no longer works. The run takes `start.level`, else the
 * level the bar is set to, and records `start.line`, the line on top it was started from. The
 * words show as the question from the moment of the call (`starting`). The start belongs to the
 * conversation on the screen at the press: where another was opened while it was out, which lets
 * the start go, the run Dispatch answers is followed, and the chip names it, but it joins no
 * conversation on the screen, and a refusal of it is let go with it.
 */
export async function startRun(
  words: string,
  context: GoingContextInput,
  login: string | undefined,
  { continues, level, line }: RunStart = {}
): Promise<boolean> {
  const newest = newestRun();
  if (continues === undefined && newest !== undefined && shownState(newest) === "working") {
    const waiting = state.queued?.run === newest.id ? `${state.queued.words}\n${words}` : words;
    set({ ...state, queued: { run: newest.id, words: waiting } });
    return false;
  }
  const follows = continues ?? (newest !== undefined && ended(newest) ? newest.id : undefined);
  const starting: StartingRun = { at: Date.now(), follows, words };
  set({ ...state, refusal: undefined, starting });
  try {
    const run = await api.createGoingRun(
      {
        context,
        level: level ?? state.level,
        words,
        ...(follows === undefined ? {} : { continues: follows }),
        ...(line === undefined ? {} : { line }),
      },
      goingBounds(login)
    );
    const runs = new Map(state.runs).set(run.id, entryOf(run, true, starting.at));
    if (state.starting !== starting) {
      set({ ...state, runs });
    } else {
      // Still on the screen, the conversation open is the one the start follows up.
      const conversation = follows === undefined ? [] : (state.conversation ?? []);
      set({
        ...state,
        conversation: [...conversation, run.id],
        draft: undefined,
        runs,
        starting: undefined,
      });
    }
    follow(run.id);
    return true;
  } catch (error) {
    if (state.starting === starting) {
      set({
        ...state,
        refusal: apiErrorMessage(error, "The run did not start."),
        starting: undefined,
      });
    }
    return false;
  }
}

/** The words that wait for the open conversation's newest run, once that run no longer works;
 *  undefined while it works or none wait. */
export function queuedReady(of: GoingState = state): QueuedWords | undefined {
  const held = of.queued;
  const run = newestRun(of);
  return held !== undefined && run?.id === held.run && shownState(run) !== "working"
    ? held
    : undefined;
}

/** Lets the words that wait go: the × beside them, or once they were sent. Words the store no
 *  longer holds as `held` were let go already. */
export function dropQueued(held: QueuedWords | undefined = state.queued): void {
  if (held !== undefined && state.queued === held) {
    set({ ...state, queued: undefined });
  }
}

/**
 * Sends the words that waited for a run through `enter`, the box's Enter (`useAsk`), once the run
 * no longer works: they answer the plan it ended waiting on (a revision in words), or they are the
 * next turn of its conversation. While its plan is still being read, `enter` takes nothing, and
 * the words wait on. The bar over the page and the panel at the side each call it; only one of the
 * two is on the screen at a time.
 */
export function useSendQueued(enter: (words: string) => boolean): void {
  // Read so the surface draws again, and this runs again, when the words can go.
  useGoingValue(queuedReady);
  useEffect(() => {
    const held = queuedReady();
    if (held !== undefined && enter(held.words)) {
      dropQueued(held);
    }
  });
}

/** Stops the run at once (`POST /api/v1/going/runs/{id}/stop`); its end frame says it stopped. A
 *  refusal says why while the run's conversation is open, and is let go where another was opened
 *  since. */
export function stopRun(id: string, login: string | undefined): void {
  const run = state.runs.get(id);
  if (run === undefined || runNext(shownState(run), "stop").send !== "stop") {
    return;
  }
  api.stopGoingRun(id, goingBounds(login)).then(
    (stopped) => withRun(id, (entry) => ({ ...entry, record: stopped })),
    (error: unknown) => {
      if (state.conversation?.includes(id) === true) {
        set({ ...state, refusal: apiErrorMessage(error, "The run did not stop.") });
      }
    }
  );
}

/** Continue on a run whose end offers it (`offersContinue`: a bound of its level, or its person's
 *  Stop): a follow-up of its conversation with a new budget of `level`, else of the stopped run's
 *  own level ("Continue in Deep" names Deep). It answers whether the follow-up started. It starts
 *  none while a start is out (`starting`): a second follow-up would take that start's place on the
 *  screen, and the run the first one starts would join no conversation shown. */
export function continueRun(
  id: string,
  context: GoingContextInput,
  login: string | undefined,
  level?: GoingRun["level"]
): Promise<boolean> {
  const run = state.runs.get(id);
  if (
    state.starting !== undefined ||
    run === undefined ||
    !offersContinue(run) ||
    runNext(shownState(run), "continue").send !== "continue"
  ) {
    return Promise.resolve(false);
  }
  return startRun("Continue", context, login, { continues: id, level: level ?? run.record.level });
}

/** The newest read of each run's record that `rereadRun` sent, by run. */
const recordReads = new Map<string, number>();

/**
 * Reads `run`'s record again (`GET /api/v1/going/runs/{id}`) where the page holds the run: what
 * Undo did and can still do of it - when an undo landed (`undone_at`), and how many of its changes
 * Undo can take back or leaves standing (`undoable_count`, `standing_count`) - is Dispatch's to
 * say, from what committed, and it moves as the rows of the run's plan run and as an Undo lands.
 * The record held is the one the newest read answered: an older read answered later is dropped.
 * A record that cannot be read leaves the one held, as a reload that could not read it would.
 */
export function rereadRun(run: string): void {
  if (!state.runs.has(run)) {
    return;
  }
  const read = (recordReads.get(run) ?? 0) + 1;
  recordReads.set(run, read);
  api.goingRun(run).then(
    (record) => {
      if (recordReads.get(run) === read) {
        withRun(run, (entry) => ({ ...entry, record }));
      }
    },
    () => undefined
  );
}

/** Holds what the Undo of `run` answered (`GoingState.undone`), and reads the run's record again
 *  (`rereadRun`): the answer's lines do not say which of their requests committed. */
export function setUndone(run: string, lines: readonly GoingUndoLine[]): void {
  set({ ...state, undone: new Map(state.undone).set(run, lines) });
  rereadRun(run);
}

/** Reads the record of every run the page holds again (`rereadRun`), on the delete of a comment or
 *  an issue message (`comment.deleted`, `message.deleted`): the delete withdraws each run that read
 *  the post, whose record then reads stopped with `ANSWER_WITHDRAWN` or `ANSWER_WITHDRAWN_BY_RUN`
 *  (`runWithdrawal`), and the page cannot tell which of its runs read it. */
export function rereadEveryRun(): void {
  for (const run of state.runs.keys()) {
    rereadRun(run);
  }
}

/** Opens a conversation History lists: its runs, oldest first, each read from its stream's first
 *  frame unless its frames were read to its end already, and the conversation shown over the page
 *  where nothing shows it yet. What the conversation open before held - its plan's draft, the
 *  words that waited for its run, why its last start, stop or Continue was refused, and the start
 *  of its next run that is out - is let go. */
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
  const opened = records.map((record) => record.id);
  // A start that is out stays only where it follows up a run of the conversation opened.
  const follows = state.starting?.follows;
  set({
    ...state,
    conversation: opened,
    draft: undefined,
    place: state.place === "closed" ? "over" : state.place,
    queued: undefined,
    refusal: undefined,
    runs,
    starting: follows !== undefined && opened.includes(follows) ? state.starting : undefined,
  });
  for (const id of unread) {
    follow(id);
  }
}

/** Opens the conversation of a run the page already follows (the chip's Open, a notice's Open or
 *  Review). The words that wait, and a start that is out, stay where they are of that
 *  conversation, and are let go otherwise; why the last start, stop or Continue was refused is
 *  let go. */
export function openRun(id: string): void {
  const run = state.runs.get(id);
  if (run === undefined) {
    return;
  }
  const conversation = conversationOf(run);
  const follows = state.starting?.follows;
  set({
    ...state,
    conversation,
    place: state.place === "closed" ? "over" : state.place,
    queued:
      state.queued !== undefined && conversation.includes(state.queued.run)
        ? state.queued
        : undefined,
    refusal: undefined,
    starting: follows !== undefined && conversation.includes(follows) ? state.starting : undefined,
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
  recordReads.clear();
  set(INITIAL);
}
