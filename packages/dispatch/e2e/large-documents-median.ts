// How a measuring row of `large-documents-measured.e2e.ts` takes its readings and reads them:
// how many it takes (`measuredReadings`), the line each reading is printed as
// (`printMeasurement`), and the median the row holds to R11's targets (`printTheMedian`). No
// other file prints a measurement. What one reading is, and how `measureDocument` takes it, is
// `large-documents-page.ts`.
import { test } from "@playwright/test";

import {
  type DocumentMeasurement,
  type DocumentOpening,
  longTaskTargetMs,
} from "./large-documents-page";

/** How many readings a measuring row takes of one document, and what its targets are then read
 *  against: the median of them. The protocol is the spec's own - "Each number is the median of
 *  3 runs" (dispatch://INFRA-341/spec, the Design paragraph R11 binds its targets to) - and
 *  what asks for it is the measured rows' own spread: the same two rows at one head read
 *  4,824 ms and 8,311 ms on a first attempt and 3,838 ms and 5,105 ms on the retry
 *  (`e2e/playwright.config.ts`), so one reading held to a target is a target thrown dice for.
 *  An odd count, so the median is a reading the row really took rather than a mean of two that
 *  happened to neighbour it.
 *
 *  Each reading is a page of its own. `measureDocument` times one navigation, and a second
 *  `goto` in the same page would be timed against a browser that already holds the document's
 *  text, its chunks and its blocks - a faster number that is not the reader's first visit. */
export const measuredReadings = 3;

/** What the line below says about itself, for a reader who has not read this file: it is quoted
 *  in a pull request body and in a Dispatch check result, where the numbers have to tell
 *  themselves apart and say which of them a run is held to. */
const measurementLegend =
  "The first editable block is the editor's own first visible block, the first block a reader " +
  'can type in (its root carries `data-write-path="live"`); the median of that number over a ' +
  "row's readings, and the median of the longest main-thread task, are the two read against " +
  "R11's targets. No reading is held to a target on its own - the median line below the " +
  "readings is the one the row asserts - and the only thing the rows assert of a reading is " +
  "that the page recorded a main-thread task at all, since a longest task read off nothing " +
  "meets any target. " +
  "First painted is the first block of the document on the screen from any draw, " +
  "which is the server's own where it drew the document's opening blocks into the page it " +
  "sent. The page's own draw is the first block the app itself put on the screen, in the " +
  "read-only preview inside the document surface it renders: that one is the number this rig " +
  "read as the editor's before INFRA-341 corrected it. Neither draw takes an edit. The " +
  "opening is the document's own chain on the same clock, step by step: the text read the " +
  "schema admission check is, the socket, its SyncStep1, the state frame back, and the " +
  "editor's root in the page.";

/** The opening timeline as one phrase of the line above: each step and when it happened, in
 *  the order they happen, with the steps that never happened named as such. */
function openingLine(opening: DocumentOpening): string {
  const steps: [string, number | undefined][] = [
    ["text asked", opening.textAskedMs],
    ["text answered", opening.textAnsweredMs],
    ["socket created", opening.socketCreatedMs],
    ["socket opened", opening.socketOpenedMs],
    ["SyncStep1 out", opening.syncStep1Ms],
    ["state frame in", opening.stateFrameMs],
    ["editor created", opening.editorRootMs],
  ];
  return steps
    .map(([step, at]) => `${step} ${at === undefined ? "never" : `${Math.round(at)} ms`}`)
    .join(" / ");
}

/**
 * Puts one reading where the run's reader finds it: on the row's annotations, which the report
 * keeps with the result, and on stdout, which `--reporter line` prints as the row runs. Nothing
 * asserts the numbers on this line; `firstBlockTargetMs` is printed beside the first of them so
 * a reader sees what the row's median of these readings will be held to, and `reading` says
 * which of the row's readings this is.
 */
export function printMeasurement(
  what: string,
  measured: DocumentMeasurement,
  { firstBlockTargetMs, reading }: { firstBlockTargetMs: number; reading: number }
): void {
  const over = measured.longTasks.filter(({ ms }) => ms > longTaskTargetMs);
  const line =
    `${what}, reading ${reading} of ${measuredReadings}: ` +
    `first editable block (this reading not asserted, the median's target ` +
    `${firstBlockTargetMs} ms) ${Math.round(measured.firstBlockMs)} ms, ` +
    `longest main-thread task (this reading not asserted, the median's target ` +
    `${longTaskTargetMs} ms) ${Math.round(measured.longestTaskMs)} ms, ` +
    `first painted (not asserted) ${measured.firstPaintedMs === undefined ? "never" : `${Math.round(measured.firstPaintedMs)} ms`}, ` +
    `page's own draw (not asserted) ${measured.pageDrawnMs === undefined ? "never" : `${Math.round(measured.pageDrawnMs)} ms`}, ` +
    `editor open (not asserted) ${measured.editorOpenedMs === undefined ? "never" : `${Math.round(measured.editorOpenedMs)} ms`}, ` +
    `opening (not asserted) ${openingLine(measured.opening)}, ` +
    `${measured.longTasks.length} main-thread task(s) over 50 ms and ${over.length} over ` +
    `${longTaskTargetMs} ms` +
    (over.length === 0
      ? ""
      : ` (${over.map(({ at, ms }) => `${Math.round(ms)} ms at ${Math.round(at)} ms`).join("; ")})`) +
    `, over ${measured.scrollSteps} scroll steps in a ${Math.round(measured.windowClosedMs)} ms window. ` +
    measurementLegend;
  test.info().annotations.push({ description: line, type: "R11 measurement" });
  console.log(`R11 measurement · ${line}`);
}

/** The two numbers a measuring row holds to its targets, each the median of its readings. */
export interface MeasuredMedian {
  /** The median first editable block, in milliseconds from the start of navigation. */
  readonly firstBlockMs: number;
  /** The median longest main-thread task of a reading's window, in milliseconds. */
  readonly longestTaskMs: number;
}

/** The middle of an odd number of readings. */
function middleOf(numbers: readonly number[]): number {
  const sorted = [...numbers].sort((first, second) => first - second);
  return sorted[(sorted.length - 1) / 2] ?? Number.NaN;
}

/**
 * The median of the readings a row took, printed beside every one of them and answered to the
 * row, which holds that median to the targets and the readings themselves to nothing. The
 * readings are printed by `printMeasurement` as each is taken, so this line is read under them
 * and says what the spread beneath it came to.
 */
export function printTheMedian(
  what: string,
  readings: readonly DocumentMeasurement[],
  firstBlockTargetMs: number
): MeasuredMedian {
  const firstBlocks = readings.map(({ firstBlockMs }) => firstBlockMs);
  const longestTasks = readings.map(({ longestTaskMs }) => longestTaskMs);
  const median = {
    firstBlockMs: middleOf(firstBlocks),
    longestTaskMs: middleOf(longestTasks),
  };
  const spread = (numbers: readonly number[]) =>
    numbers.map((ms) => `${Math.round(ms)}`).join(", ");
  const line =
    `${what}: first editable block (asserted, target ${firstBlockTargetMs} ms) ` +
    `${Math.round(median.firstBlockMs)} ms, the median of ${spread(firstBlocks)} ms; ` +
    `longest main-thread task (asserted, target ${longTaskTargetMs} ms) ` +
    `${Math.round(median.longestTaskMs)} ms, the median of ${spread(longestTasks)} ms. ` +
    "Each reading is one navigation of a page of its own, printed in full above and held to " +
    "nothing: what a reader of this line is owed is the spread the median came out of, which " +
    "a single number hides.";
  test.info().annotations.push({ description: line, type: "R11 median" });
  console.log(`R11 median of ${readings.length} readings · ${line}`);
  return median;
}
