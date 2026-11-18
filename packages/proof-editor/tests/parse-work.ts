/**
 * The work a parse does in the two scans this package's dependency patches make linear, for the
 * rows that hold a heavy body's parse to a cost in proportion to its length.
 *
 * Each patch counts what its scan reads and exports the count (packages/proof-editor/AGENTS.md,
 * "The four dependency patches"): `lazyLineScanSteps` of `micromark`
 * (`patches/micromark@4.0.2.patch`), one step per event the lazy-line test reads, and
 * `labelEndScanSteps` of `micromark-core-commonmark`
 * (`patches/micromark-core-commonmark@2.0.3.patch`), one per event a label end's search for its
 * opening `[` reads and one per character its definition lookup reads. A row reads both around one
 * parse: the counts are the dependency's own record of the work, so a busy machine does not move
 * them, and a scan put back to reading every event again on each line or each `]` reads a number
 * of steps that grows with the square of the body.
 *
 * The counters run over the whole process. A parse is synchronous and the rows of a file run one at
 * a time, so the steps between the two readings are that parse's alone.
 */

import { expect } from "bun:test";
import { lazyLineScanSteps } from "micromark";
import { labelEndScanSteps } from "micromark-core-commonmark";

/**
 * The most scan steps a parse may take per character of its body.
 *
 * The highest healthy reading is 3.0 a character: a run of 15,000 brackets and as many closers,
 * where each `]` reads the events added since the label end before it stopped looking and the
 * opening below them (89,995 steps on 30,000 characters, in both modes, 2026-11-14). The lazy
 * shapes read 0.5 a character (one event for each lazy line), and a body that holds no lazy line
 * and no `]` reads none. This bound is twice the highest. A scan that reads every event again
 * reads a number of steps that grows with the square of the body instead: thousands a character
 * at these lengths.
 */
export const scanStepsPerCharacter = 6;

/** A body a row parses, as a reader is sent it. */
export type WorkShape = {
  /** What the body is, as a row's name says it. */
  name: string;
  /** The body, at the length the row stands for. */
  body: string;
};

/** What one parse of a body read in the two scans, and what it threw where it threw. */
type WorkReading<T> = {
  /** Events the lazy-line test read. */
  readonly lazyLine: number;
  /** Events and label characters the label ends read. */
  readonly labelEnd: number;
  /** Whether the parse threw. A parse that throws `undefined` has thrown too, which is why this
   *  stands beside `thrown` rather than being read off it. */
  readonly threw: boolean;
  /** What the parse threw, `undefined` where it threw nothing. */
  readonly thrown: unknown;
  /** What the parse returned, `undefined` where it threw. */
  readonly value: T | undefined;
};

/** Parses once, reading the two scans' counts around the parse, and keeps what it threw. */
function readWork<T>(parse: () => T): WorkReading<T> {
  const lazyLineBefore = lazyLineScanSteps();
  const labelEndBefore = labelEndScanSteps();
  let value: T | undefined;
  let threw = false;
  let thrown: unknown;
  try {
    value = parse();
  } catch (cause) {
    threw = true;
    thrown = cause;
  }
  return {
    lazyLine: lazyLineScanSteps() - lazyLineBefore,
    labelEnd: labelEndScanSteps() - labelEndBefore,
    threw,
    thrown,
    value,
  };
}

/**
 * Fails when the parse took more than `scanStepsPerCharacter` scan steps per character of its body,
 * naming the steps it took in each scan and the steps the body allows.
 */
function expectWorkInProportion(what: string, body: string, reading: WorkReading<unknown>): void {
  const steps = reading.lazyLine + reading.labelEnd;
  const allowed = scanStepsPerCharacter * body.length;
  expect(
    steps,
    `${what} took ${steps} scan steps (${reading.lazyLine} in the lazy-line test, ` +
      `${reading.labelEnd} in the label ends) on ${body.length} characters, where at most ` +
      `${allowed} (${scanStepsPerCharacter} a character) are allowed`
  ).toBeLessThanOrEqual(allowed);
}

/**
 * What one parse of `shape`'s body produced, once its scan steps have been found to be in
 * proportion to the body's length.
 *
 * A parse that threw where it was to return a document is reported only after the count has
 * passed, because a break can make a parse both dearer and throwing and the cost is what the row
 * is for. The throw reaches the runner as itself, with its own stack.
 */
export function parsedWithinWorkBounds<T>(
  what: string,
  shape: WorkShape,
  parse: (body: string) => T
): T {
  const reading = readWork(() => parse(shape.body));
  expectWorkInProportion(what, shape.body, reading);
  if (reading.threw) throw reading.thrown;
  if (reading.value === undefined) throw new Error("the parse produced no document");
  return reading.value;
}

/**
 * Holds a parse that refuses `shape`'s body to the count above, and then to the refusal itself: it
 * must throw an `Error` whose message matches `refused`.
 *
 * The count is asserted first, for the reason above: a break that makes a refused parse dearer is
 * read as the cost failure the row is for, not as a missing refusal. A throw that is not an `Error`
 * is thrown on as it is, so it reaches the runner with its own stack rather than as a message that
 * matched nothing.
 */
export function refusalWithinWorkBounds(
  what: string,
  shape: WorkShape,
  parse: (body: string) => unknown,
  refused: RegExp
): void {
  const reading = readWork(() => parse(shape.body));
  expectWorkInProportion(what, shape.body, reading);
  if (!reading.threw) throw new Error(`${what} was expected to be refused and was not`);
  if (!(reading.thrown instanceof Error)) throw reading.thrown;
  expect(reading.thrown.message, `${what} was refused by something else`).toMatch(refused);
}
