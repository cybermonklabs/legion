/** The program of each row's own limit, for the rows of `harness-postgres-removal.test.ts`, the
 *  files split from it by concern, `harness-postgres-holders.test.ts`, `harness-postgres-leftover.test.ts` and the other files
 *  whose rows count their waits (`rowOf`): a row states what it waits for (`Waits`), its limit is
 *  the sum of those waits (`rowMs`), a wait that waits for several things itself is a part whose
 *  bound is the sum of its own (`Part`), and each fixture that starts a child or a holder read, or
 *  waits for a part, charges the row (`Row`), which fails where it started more than its limit
 *  counts. It holds the row's deadline too, and each wait's share of it where the CI step that runs
 *  the row ends first (`boundOf`), the life of a stand-in the row needs alive (`lifeOf`), and the
 *  record of each wait a row names (`during`), by which a fixture call that the row's deadline
 *  finds already passed names the wait that used that deadline up. */

import { machine } from "../harness-postgres-container-removal";
import { statMs, statStartMs } from "../harness-postgres-processes";
import { stopMs } from "../harness-postgres-removal";
import { stepEnds, stepEndsVariable } from "./step-end";

/** How long a row waits for one event of another process: a `bun` child's start of its sweep
 *  under load, which took more than 180 s once on a shared Linux machine under a load of 1,217, on
 *  2026-11-17, is the longest of them. */
export const claimEventMs = 240_000;
/** How long a row gives one call of its own fixtures that makes a container or an image with
 *  Docker (`docker` of `harness-postgres-leftover-fixtures.ts`), and one call of the image
 *  provider's that makes or starts a container, which the provider bounds by nothing
 *  (`providerStart` of `harness-postgres-provider-children.ts`): three times the 133 s that one
 *  `docker create` of a row took on a daemon a shared Linux machine shared, on 2026-11-19, while
 *  that daemon answered every other call, as the fixtures' other bounds are three times their
 *  largest reading. */
export const makeMs = 400_000;

/** The holder reads a start makes after it stopped a postmaster (`removePostmaster` of
 *  `e2e/harness-postgres-removal.ts`): one, and a second where a process still held the directory
 *  then, which a moment of the kernel's decides, so a row counts both. */
const readsAfterStop = 2;

/** What a row waits for, one wait after another, which its own limit is the sum of (`rowMs`):
 *  each `bun` child it starts (`children`), each of which may take `claimEventMs` to start under
 *  load; each holder read that the sweeps of those children make before a claim (`reads`), each of
 *  which starts a reader that may take `statStartMs` to start and then reads within `statMs`
 *  (`e2e/harness-postgres-processes.ts`); each stop of a postmaster those sweeps make (`stops`),
 *  each of which may take `stopMs` and is followed by `readsAfterStop` holder reads; each call of
 *  its fixtures that makes a container or an image (`makes`), each of which may take `makeMs`;
 *  each wait that waits for several things itself (`parts`, `Part`), such as a start of the pool
 *  in a child, at the bound the program sums for it, the longest of those it waits for at once
 *  (`AtOnce`); every other bound the row waits out (`bounds`, in ms): the bound of a removal, of
 *  an `until`, of the row's own writes; and the bound of each wait of its `finally` before it
 *  removes what it made, such as the stop of a stack it started (`ends`, in ms, or a `Part`).
 *  Those come after the row's deadline (`Row.deadline`), which ends every fixture call of the
 *  row's own waits: a call that runs to the deadline then leaves the `finally` the time its stops
 *  take, and the removals after them the room above it (`Row.cleanup`), where a deadline that
 *  counted the stops left a removal none, and the container of a stack the row killed stayed. The
 *  room above all of them is `rowRoomMs`, or the row's own (`roomMs`), a few seconds, for a row
 *  that sees that room run out in real time, so it takes seconds and not the room's minute. */
export interface Waits {
  readonly children: number;
  readonly reads: number;
  readonly stops?: number;
  readonly makes?: number;
  readonly bounds: readonly number[];
  readonly parts?: readonly (Part | AtOnce)[];
  readonly ends?: readonly (number | Part)[];
  readonly roomMs?: number;
}

/** What only `partOf` puts in a `Part`, so that no row and no fixture writes a part, or its bound,
 *  by hand: a part written as an object of its own is a compiler error. */
const summed: unique symbol = Symbol("partOf");

/** One wait of a row that waits for several things itself, and whose bound the program sums from
 *  them: a start of the pool, a stack's start or its stop, a shell's start, the end of a child the
 *  row killed, each a child that `harness-postgres-provider-children.ts` ends at that bound, or a
 *  start of the provider that a row runs in its own process. `what` is its kind, which the row's
 *  charge reads (`Row.part`), and `ms` its bound (`partOf`): what it waits for, one wait after
 *  another (`Waits`), the waits of its own end, and `rowRoomMs` above them, the same program as a
 *  row's own limit (`rowMs`). So a part's bound is above the bound of every wait inside it, and
 *  moves with each of them. A start of the pool that a row gave a fixed 600 s, while the start's
 *  removal of one container may wait 600 s itself, was killed inside its own bounds on a shared
 *  Linux machine on 2026-11-19. */
export interface Part {
  readonly what: string;
  readonly ms: number;
  readonly [summed]: true;
}

/** The `Part` of the kind `what` that waits for `waits`. */
export function partOf(what: string, waits: Waits): Part {
  return { what, ms: rowMs(waits), [summed]: true };
}

/** Parts a row starts at once and waits for together, of which the longest counts (`Waits`). */
export interface AtOnce {
  readonly atOnce: readonly Part[];
}

/** The waits of `all`, one after another, as one `Waits`: their counts added, and their bounds,
 *  parts and ends one list each, in turn. */
export function inTurn(...all: readonly Waits[]): Waits {
  const count = (of: (waits: Waits) => number | undefined) =>
    all.reduce((sum, waits) => sum + (of(waits) ?? 0), 0);
  return {
    children: count((waits) => waits.children),
    reads: count((waits) => waits.reads),
    stops: count((waits) => waits.stops),
    makes: count((waits) => waits.makes),
    bounds: all.flatMap((waits) => waits.bounds),
    parts: all.flatMap((waits) => waits.parts ?? []),
    ends: all.flatMap((waits) => waits.ends ?? []),
  };
}

/** The holder reads a row's sweeps may make in all (`Waits`): those before a claim, and those after
 *  each stop. */
function readsOf({ reads, stops = 0 }: Waits): number {
  return reads + stops * readsAfterStop;
}

/** Everything a row waits for, one wait after another (`Waits`), in ms: the limit of a child the
 *  row ends itself, and of an `until` of an event that comes after those waits. */
export function waitsMs(waits: Waits): number {
  const { children, stops = 0, makes = 0, bounds, parts = [] } = waits;
  return (
    children * claimEventMs +
    readsOf(waits) * (statStartMs + statMs) +
    stops * stopMs +
    makes * makeMs +
    bounds.reduce((sum, ms) => sum + ms, 0) +
    // Of parts the row waits for at once, the longest.
    parts.reduce(
      (sum, part) =>
        sum + ("atOnce" in part ? Math.max(0, ...part.atOnce.map((each) => each.ms)) : part.ms),
      0
    )
  );
}

/** The room a row's limit leaves above what it waits for (`waitsMs`): the row's own reads of files
 *  and of `/proc` between its waits, its asks of Docker and Postgres, each of which a daemon that
 *  took minutes over a removal answered within a second on 2026-11-18, and the moment between a
 *  bound firing in a child and the row reading what it ended, so that the error of the child or of
 *  the `until`, which names what did not come, ends the row, and not the row's own clock; and the
 *  removal of what a fixture call made that ran out the row's deadline (`Row.deadline`). */
export const rowRoomMs = 60_000;

/** A row's own limit: everything it waits for (`waitsMs`), the waits of its `finally`
 *  (`Waits.ends`), and its room above them, `rowRoomMs` or its own (`Waits.roomMs`). Every row of
 *  the files these fixtures serve that counts its waits (`rowOf`) states its limit so, and no
 *  other way, which `harness-postgres-source-rules.test.ts` reads from their source
 *  (`limitsNotSummed` of `@legion/test-fixtures/row-sums`). */
export function rowMs(waits: Waits): number {
  const ends = (waits.ends ?? []).reduce<number>(
    (sum, end) => sum + (typeof end === "number" ? end : end.ms),
    0
  );
  return waitsMs(waits) + ends + (waits.roomMs ?? rowRoomMs);
}

/** What a row started so far, held to what its limit counts (`Waits`): each fixture that starts a
 *  `bun` child charges the row for it (`child`), each sweep charges the holder reads it made, as
 *  the reader it ran counted them (`reads`), each fixture call that makes a container or an image
 *  charges it for that call (`made`), and each fixture that waits for a part charges it for that
 *  part before the wait (`part`). A row that starts more than its limit counts fails there, naming
 *  what the limit left out, whatever the load: its limit is below what it waits for. A part counts
 *  where the row's waits hold one of its kind whose bound reaches the part's, before the deadline
 *  (`Waits.parts`) or in the `finally` (`Waits.ends`), and each counted part serves one wait; the
 *  charge answers when that wait must end by where the row's step cut its limit (`cut`): the
 *  deadline, or `stops` for a wait of the `finally`.
 *  `ms` is how long the row's waits may take, `waitsMs` of them, and `deadline` is
 *  when those waits end, from the row's start, on the machine's clock (`machine` of
 *  `e2e/harness-postgres-container-removal.ts`): a fixture call of Docker or Postgres waits until
 *  then and no longer, so it ends within the row's own limit with a reason that names it.
 *  `stops` is when the waits of the row's `finally` (`Waits.ends`), such as the stop of a stack
 *  it started, end, and `cleanup` when a fixture call that removes what the row made waits until:
 *  after them, and before half the room above them (`rowRoomMs`, or `Waits.roomMs`), so a
 *  removal's line, and not the row's own clock, ends the row. `end` is when the row's own limit
 *  ends it (`rowMs` from its start), until which each stand-in the row needs alive lives
 *  (`lifeOf`).
 *  Where the CI step that runs the row ends before that (`stepEnds`), the step's end is the row's
 *  end, and `cut` says so: the deadline is that end less the row's own time for its `finally`
 *  (its stops, and the room above them in which it removes what it made), each scaled by what the
 *  step leaves of the row's own limit, so the row's waits and its `finally` share that time in the
 *  proportion of their bounds. Every wait of the row then ends by its share (`boundOf`), naming
 *  what it waited for and the step's end, so that a row whose parts hang ends inside its step
 *  with its own reason and still removes what it made, and not by the step's own clock. A run on
 *  a machine of one's own sets no such end, and its rows keep their own limits. */
export interface Row {
  readonly ms: number;
  readonly deadline: number;
  readonly stops: number;
  readonly cleanup: number;
  readonly end: number;
  readonly cut: string | undefined;
  child(what: string): void;
  reads(made: number, what: string): void;
  made(what: string): void;
  part(part: Part): number;
}

/** How long a stand-in process that the row `row` needs alive lives, in whole seconds, rounded
 *  up: until the row ends (`Row.end`), so that no wait of the row, inside its bounds, outlives it,
 *  and it ends by itself where the row did not end it. A stand-in a row gave a fixed 600 s died
 *  inside the row's waits on a shared Linux machine on 2026-11-19, when a start of the pool the
 *  row waited for took 620 s inside its own bounds. A stand-in whose parent outlives the row waits
 *  for its parent's end instead (`untilThisEnds`, `untilParentEnds` of `planted-marks.ts`): this is
 *  for one that must outlive the process that started it, as a `docker` call that hangs in its
 *  daemon outlives the start that ran it (`neverEnds`). */
export function lifeOf(row: Row): number {
  return Math.max(1, Math.ceil((row.end - machine.now()) / 1000));
}

/** The line of a stand-in `docker`'s body (`standInDocker` of
 *  `harness-postgres-removal-fixtures.ts`) for a call that Docker never ends: it becomes `sleep`,
 *  with the stand-in's path for its name (`exec -a "$0"`), so `running` finds it, until the row
 *  `row` ends (`lifeOf`), and so outlives the start that ran it, as a `docker` that hangs in its
 *  daemon does, and a row reads whether that start stopped it. */
export function neverEnds(row: Row): string {
  return `exec -a "$0" sleep ${lifeOf(row)}`;
}

/** How long a wait of the row `row` whose own bound is `ms` may run from now, and how a wait that
 *  ran that time out says so: its own bound, or, where the end of the row's CI step cut the row's
 *  limit (`Row.cut`), the time left until `by`, the row's deadline, or `Row.stops` for a wait of
 *  the row's `finally`, where that is shorter. A bound of `Infinity` is a wait that sets none of
 *  its own. */
export function boundOf(row: Row, ms: number, by = row.deadline): { ms: number; named: string } {
  const left = by - machine.now();
  if (row.cut === undefined || left >= ms) return { ms, named: `within ${ms / 1000} s` };
  const own = Number.isFinite(ms) ? `, before its own ${ms / 1000} s` : "";
  return {
    ms: Math.max(0, left),
    named: `by the row's deadline, ${Math.max(0, left) / 1000} s after this wait began, which ${row.cut} set${own}`,
  };
}

/** `ms` scaled by `limit / own`, the share of a row's own limit `own` that its CI step leaves it,
 *  in whole ms, rounded down exactly: the product first and then one division. A share taken first
 *  (`ms * (limit / own)`) rounds twice, so where the exact share is a whole number of ms it can fall
 *  a hair below it and floor one ms short: a run of the step-cut row of
 *  `harness-postgres-removal.test.ts` read 437 for an exact 438 on 2026-11-19. Each operand is a
 *  whole number of ms, and their product lies far below 2^53, so it is exact; the one division then
 *  errs by less than one part in 2^52 of its quotient, while a quotient that is not a whole number
 *  lies at least 1/`own` from every whole number, so it floors as the exact quotient does. */
function shareOf(ms: number, limit: number, own: number): number {
  return Math.floor((ms * limit) / own);
}

/** The `Row` of a row whose limit is `rowMs(waits)`, or the time its CI step leaves
 *  (`stepEnds`) where that is shorter, which starts now. */
export function rowOf(waits: Waits): Row {
  let children = 0;
  let reads = 0;
  let makes = 0;
  // Each part the limit counts, before the deadline or in the `finally`, until a wait uses it.
  const counted = [
    ...(waits.parts ?? [])
      .flatMap((part) => ("atOnce" in part ? part.atOnce : [part]))
      .map((part) => ({ part, inFinally: false })),
    ...(waits.ends ?? [])
      .filter((end): end is Part => typeof end !== "number")
      .map((part) => ({ part, inFinally: true })),
  ];
  const start = machine.now();
  const own = rowMs(waits);
  const room = waits.roomMs ?? rowRoomMs;
  const step = stepEnds();
  const limit = step === undefined ? own : Math.max(0, Math.min(own, step - start));
  // What the step leaves of the row's own limit: all of it where the step leaves it all, as off CI.
  const share = limit / own;
  const deadline = start + shareOf(waitsMs(waits), limit, own);
  // The waits of the `finally` after the deadline: its stops, then the room for its removals.
  const stops = deadline + shareOf(own - waitsMs(waits) - room, limit, own);
  const cut =
    step !== undefined && limit < own
      ? `the end of the CI step that runs it, at ${new Date(step).toISOString()} (${stepEndsVariable}), ${limit / 1000} s after the row began and before its own ${own / 1000} s`
      : undefined;
  return {
    ms: deadline - start,
    deadline,
    stops,
    // Half the room after the stops: the row's limit less half its share of the room.
    cleanup: start + limit - (room * share) / 2,
    end: start + limit,
    cut,
    child: (what) => {
      children += 1;
      if (children > waits.children) {
        throw new Error(
          `the row's limit counts ${waits.children} bun children, and it started ${children}, the last ${what}: its limit is below what it waits for`
        );
      }
    },
    reads: (made, what) => {
      reads += made;
      if (reads > readsOf(waits)) {
        throw new Error(
          `the row's limit counts ${readsOf(waits)} holder reads, and its sweeps made ${reads}, ${made} of them in ${what}: its limit is below what it waits for`
        );
      }
    },
    made: (what) => {
      makes += 1;
      if (makes > (waits.makes ?? 0)) {
        throw new Error(
          `the row's limit counts ${waits.makes ?? 0} calls that make a container or an image, and it made ${makes}, the last ${what}: its limit is below what it waits for`
        );
      }
    },
    part: (part) => {
      // The least counted bound that holds the part, so that a longer one stays for a longer wait.
      const fits = counted
        .filter((each) => each.part.what === part.what && each.part.ms >= part.ms)
        .sort((a, b) => a.part.ms - b.part.ms)[0];
      if (fits === undefined) {
        const left = counted
          .filter((each) => each.part.what === part.what)
          .map((each) => each.part.ms / 1000);
        throw new Error(
          `the row's limit counts ${left.length === 0 ? "nothing" : `${left.join(" s, ")} s`} for ${part.what} it has not waited for yet, and it waits for ${part.what} for up to ${part.ms / 1000} s: its limit is below what it waits for`
        );
      }
      counted.splice(counted.indexOf(fits), 1);
      return fits.inFinally ? stops : deadline;
    },
  };
}

/** One wait of this process's rows (`during`): what it waits for, and when it began and when it
 *  ended on the machine's clock (`machine`), `ended` unset while it runs. */
export interface Wait {
  readonly what: string;
  readonly began: number;
  ended?: number;
}

/** Every wait of this process's rows so far, in the order they began (`during`). The rows of one
 *  file run one after another, so the waits that ran at a row's deadline are that row's own. */
export const waits: Wait[] = [];

/** Waits for `wait()`, which its row names `what`, and records when it began and when it ended
 *  (`waits`), so that the line of a fixture call that the row's deadline finds already passed
 *  names the wait that used that deadline up (`ranOut` of
 *  `harness-postgres-leftover-fixtures.ts`). Each wait of a child in
 *  `harness-postgres-provider-children.ts`, and of Docker or of Postgres in
 *  `harness-postgres-leftover-fixtures.ts`, is one; a row names its other waits so, such as a
 *  provider's start of a stack that the row awaits itself. */
export async function during<T>(what: string, wait: () => Promise<T>): Promise<T> {
  const record: Wait = { what, began: machine.now() };
  waits.push(record);
  try {
    return await wait();
  } finally {
    record.ended = machine.now();
  }
}
