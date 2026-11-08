/**
 * CPU time, for the rows that bound what a parse costs.
 *
 * A parse's wall time counts every moment the scheduler leaves this process off a core, so a busy
 * machine stretches it without the parse doing any more work. That is what made the rows reading
 * `performance.now()` fail on a loaded runner. CPU time leaves that wait out. It is not immune to
 * load: a busy machine still slows this process's own CPU time through shared caches and clock
 * frequency, so a growth check samples each side more than once.
 *
 * This module is a port of the Go package `packages/envoy/internal/cputest` (`Process`,
 * `resolution` and `Measure`) and of `cpuTimeOf`, `fastestOf`, `growthVerdict` and `growsLinearly`
 * in `packages/envoy/internal/dispatch/pmdoc/linear_test.go`. Read those for the longer account.
 *
 * `process.cpuUsage()` counts the whole process, so a row that times a call by it runs no other
 * measured work beside that call: the rows using this module parse one document at a time.
 */

import { expect } from "bun:test";
import process from "node:process";

/**
 * The step a growth check compares across: the full shape against an eighth of it.
 *
 * With `linearSlack` the check's bound is 32. The highest healthy ratio measured in this pod is
 * 14.46, and a cost growing with the square of its input reads about 64 at this step, so a healthy
 * parse and a quadratic one fall on opposite sides of the bound. `growthStep` in
 * `packages/envoy/internal/dispatch/pmdoc/linear_test.go`, which this one mirrors, is 8 as well.
 */
export const growthStep = 8;

/**
 * How many times past the size step a cost may grow and still read as linear.
 *
 * The slack pays for the measurement's own error and the machine's noise; the block below the
 * three constants holds those figures. The slack must stay below `growthStep`, or the arithmetic
 * stops discriminating: at a slack of 8 the bound would be 64, which a quadratic's 64 only equals
 * rather than passes. `linearSlack` in `packages/envoy/internal/dispatch/pmdoc/linear_test.go` is
 * 3 at the same step; this pod's own throughput moves by tens of per cent between windows, which
 * is what the fourth covers.
 */
export const linearSlack = 4;

/**
 * The most CPU time one parse of a shape at its full size may cost, on any runner.
 *
 * The highest healthy reading measured in this pod is 3,570 ms ("49 items nested by indentation
 * then blank lines", default mode, under 12 busy loops), so this carries 3.4 times of slack for a
 * slower machine. The one broken parser measured costs 205,220 ms, which clears the budget 17
 * times. It is a bound on work, not a hang guard: a parse that hangs is ended by the CI job's
 * `timeout-minutes`, never by this number. Both readings were taken with `cpuTimeOf` below, the
 * port of `packages/envoy/internal/cputest`.
 */
const parseCpuBudgetMs = 12_000;

// What these three constants do not catch (INFRA-1629):
//
// - A parse 3 times slower than today and still linear passes both the budget and the growth
//   check: 3 times the worst healthy reading is 10,710 ms, inside the budget, and a constant
//   factor leaves a ratio where it was.
// - A quadratic parse of a shape no row holds is caught by nothing here. The shapes the rows do
//   hold are the ones in tests/heavy-bodies.test.ts and tests/nested-directive.test.ts.
// - The slack is not a tight bound. The growth check reads two measurements, each wrong by up to
//   4 per cent from the clock (see `windowSteps`) before the machine's own noise, and the slack
//   of 4 is the room the healthy ratio of 14.46 above needs, against the bound of 32.

/**
 * How many changes of the CPU clock `cpuClockStepMs` waits for. A sample that also counts another
 * thread's CPU time reads a larger step, never a smaller one, so the least of a few is the clock's
 * own step (`resolutionSamples` in `packages/envoy/internal/cputest`).
 */
const clockStepSamples = 5;

/**
 * How many clock steps a timed window must last before `cpuTimeOf` divides it by its runs
 * (`windowSteps` in `packages/envoy/internal/cputest`).
 *
 * A reading is wrong by up to one step at each end, so 50 steps keeps that error near 4 per cent.
 * Without it a 20 ms parse reads 10, 20 or 30 ms on this pod's 10 ms clock, and a ratio built from
 * two such readings is noise.
 */
const windowSteps = 50;

/**
 * How many readings `fastestCpuTimeOf` keeps the least of. Two, where the `sampleCount` it
 * mirrors in `linear_test.go` is 3: a reading here is already a whole window of runs, and a third
 * would add that window to each of the two sides every row measures.
 */
const defaultSamples = 2;

/** This process's total CPU time since it started, user and system, in milliseconds. */
function processCpuMs(): number {
  const usage = process.cpuUsage();
  return (usage.user + usage.system) / 1000;
}

/** What `cpuClockStepMs` measured, kept so the measurement runs once. */
let clockStepMs = 0;

/**
 * The smallest step `process.cpuUsage()` moves in on this machine, in milliseconds: the least
 * difference between two readings that differ, over `clockStepSamples` spins. It is measured once
 * and kept. A Linux kernel moves the clock in steps far under a millisecond; gVisor moves it in
 * 10 ms steps, so there a call shorter than 10 ms reads as 0 or as 10 ms.
 */
function cpuClockStepMs(): number {
  if (clockStepMs === 0) clockStepMs = measureCpuClockStepMs();
  return clockStepMs;
}

function measureCpuClockStepMs(): number {
  let step = 0;
  for (let sample = 0; sample < clockStepSamples; sample++) {
    const started = processCpuMs();
    let next = processCpuMs();
    while (next === started) next = processCpuMs();
    if (step === 0 || next - started < step) step = next - started;
  }
  return step;
}

/**
 * What a throw inside a measured window costs. `cpuTimeOf` gives the throw back to its caller
 * rather than swallowing it, so a row can assert the cost of a parse that refuses its input and
 * then assert the refusal itself.
 */
class CpuTimedError extends Error {
  /** The CPU time one run cost: the window the runs filled, divided by those runs. */
  readonly cpuMs: number;

  constructor(cpuMs: number, cause: unknown) {
    super(`the work threw after ${Math.round(cpuMs)} ms of CPU time`, { cause });
    this.name = "CpuTimedError";
    this.cpuMs = cpuMs;
  }
}

/**
 * The CPU time one run of `work` costs, in milliseconds.
 *
 * It runs `work` once in a timed window, and runs it more times in the next window until a window
 * lasts `windowSteps` clock steps, then divides that window by its runs. A shorter window is
 * discarded, never averaged in. This is `Measure` in `packages/envoy/internal/cputest`: the run
 * count doubles, or jumps straight to a quarter past the window when the window so far says that
 * is further, so a run a little slower than this window's average still lands the next window past
 * the mark.
 *
 * A run that throws does not end the window. The measurement keeps the throw aside, runs the rest
 * of the window, and raises a `CpuTimedError` carrying the window's per-run cost as its `cpuMs`
 * and the throw it kept as its `cause`. The parses these rows measure refuse their input every
 * time, so every run of such a window throws and the window measures exactly the work the refusal
 * costs. The window is what makes the reading steady: one unrepeated run of a 20 ms refusal reads
 * as 10, 20 or 30 ms on this pod's 10 ms clock, an error of up to 50 per cent on the baseline of
 * a growth check. A throw from a window too short to keep is kept as well, so a work that throws
 * once and returns on every later run reaches its caller as a throw, never as a plain reading.
 */
function cpuTimeOf(work: () => unknown): number {
  const want = windowSteps * cpuClockStepMs();
  let threw = false;
  let thrown: unknown;
  let runs = 1;
  for (;;) {
    const started = processCpuMs();
    // The runs sit inside one `try`, as they do for a work that never throws; a throw re-enters
    // it at the next run, so the window goes on and a work that never throws costs no more.
    for (let run = 0; run < runs; ) {
      try {
        for (; run < runs; run++) work();
      } catch (cause) {
        threw = true;
        thrown = cause;
        run++;
      }
    }
    const took = processCpuMs() - started;
    if (took >= want) {
      if (threw) throw new CpuTimedError(took / runs, thrown);
      return took / runs;
    }
    if (took <= 0) {
      runs *= 10;
      continue;
    }
    runs = Math.max(runs * 2, Math.floor((runs * want * 5) / (took * 4)) + 1);
  }
}

/**
 * The least of `defaultSamples` readings of `cpuTimeOf`.
 *
 * Both sides of a growth check take the same number of samples. Sampling the small side more often
 * than the large one biases the baseline low under bursty load and inflates the ratio, which is the
 * defect `linear_test.go`'s own `sampleCount` comment records from its third round. A work that
 * throws is sampled as many times as any other, and raises a `CpuTimedError` carrying the least of
 * those readings and the throw of the last sample.
 *
 * A reading already past `parseCpuBudgetMs` is not read again: a second reading cannot bring it
 * under the budget, and taking one doubles the cost of the failure these rows exist to catch. With
 * the nesting bound of src/block-nesting.ts neutralised, `a chain of 15,999 list markers on one
 * line` costs 205,220 ms of CPU time read-only, so a second reading of it would carry that row
 * past its own 300,000 ms hang guard and report a timeout where the budget should report a clean
 * failure. Every reading under the budget, which is every healthy row, is read as many times as
 * before, so the symmetry above is untouched.
 */
function fastestCpuTimeOf(work: () => unknown): number {
  let best = Number.POSITIVE_INFINITY;
  let thrown: CpuTimedError | undefined;
  for (let sample = 0; sample < defaultSamples; sample++) {
    try {
      const took = cpuTimeOf(work);
      if (took < best) best = took;
    } catch (error) {
      if (!(error instanceof CpuTimedError)) throw error;
      if (error.cpuMs < best) best = error.cpuMs;
      thrown = error;
    }
    if (best > parseCpuBudgetMs) break;
  }
  if (thrown !== undefined) throw new CpuTimedError(best, thrown.cause);
  return best;
}

/**
 * One side of a growth step, read: what one parse of that side cost, and what it threw where it
 * threw. `costReadingOf` is the only way this module reads a side, so both helpers below assert
 * the cost of a side before they report its throw.
 */
type CostReading = {
  /** The CPU time one run of the parse cost, in milliseconds. */
  readonly cpuMs: number;
  /** The throw the measurement kept, with the parse's own throw as its `cause`; `undefined`
   *  where every run of the parse returned. */
  readonly thrown: CpuTimedError | undefined;
};

/**
 * Reads one side of a growth step: the cost always, and the throw where the parse threw.
 *
 * `fastestCpuTimeOf` raises a `CpuTimedError` carrying both the least reading and the throw, so a
 * row that called it directly would lose the reading the moment the parse threw. A break can make
 * a parse both dearer and throwing, and what the row is for is the cost, so the cost comes back
 * here as a value and the throw is left for the row to report after its assertions (INFRA-1629).
 * A throw that is not a `CpuTimedError` comes from the measurement itself, not from the parse,
 * and is left on its way out.
 */
function costReadingOf(parse: () => unknown): CostReading {
  try {
    return { cpuMs: fastestCpuTimeOf(parse), thrown: undefined };
  } catch (error) {
    if (!(error instanceof CpuTimedError)) throw error;
    return { cpuMs: error.cpuMs, thrown: error };
  }
}

/**
 * Whether `full`, the cost at the full size, grew more than `slack` times faster than the input
 * did at `step` (`growthVerdict` in `linear_test.go`). Both costs are in milliseconds.
 *
 * The step and the slack are arguments, defaulting to this module's own, so that
 * tests/cpu-time.test.ts can run this arithmetic at another step and read what it decides there.
 */
export function costGrewTooFast(
  small: number,
  full: number,
  step = growthStep,
  slack = linearSlack
): boolean {
  return full > slack * step * small;
}

/**
 * Fails when `full`, the cost at the full size, grew more than `linearSlack` times faster than the
 * input did at `growthStep`. It reports what `costGrewTooFast` decided, as `growsLinearly` in
 * `linear_test.go` reports `growthVerdict`'s. Both costs are in milliseconds.
 */
export function expectCostGrowsLinearly(name: string, small: number, full: number): void {
  const bound = linearSlack * growthStep;
  const ratio = full / small;
  const reading = `${Math.round(full)} ms at the full size against ${Math.round(small)} ms`;
  // The small side's share of the full size, read from `growthStep` so that the sentence cannot
  // contradict the step it reports.
  const fraction = `1/${growthStep}`;
  const grew = `at ${fraction} of it, ${ratio.toFixed(1)} times for an input ${growthStep} times`;
  const message = `${name} cost ${reading} ${grew} larger (at most ${bound} times)`;
  expect(costGrewTooFast(small, full), message).toBe(false);
}

/**
 * What a parse produced at the full size, once that parse has been read at both sizes and found
 * to cost less than `parseCpuBudgetMs` and to grow no faster than its input did at `growthStep`.
 *
 * `parse` is given the body to read, so the two sizes go through one closure and differ by their
 * input and by nothing else; `name` is what the growth check calls the parse in its message. The
 * row asserts what it reads on the document this returns, which is the one the full size's
 * measured parse produced.
 *
 * A parse that threw where it was to return a document is reported only once both assertions
 * have passed, because a break can make a parse both dearer and throwing and the cost is what
 * the row is for. The throw reaches the runner as itself, with its own stack.
 */
export function parsedWithinCostBounds<T>(
  name: string,
  parse: (body: string) => T,
  smallBody: string,
  fullBody: string
): T {
  const small = costReadingOf(() => parse(smallBody));
  let parsed: T | undefined;
  const full = costReadingOf(() => {
    parsed = parse(fullBody);
  });
  expect(full.cpuMs, `the full size cost ${Math.round(full.cpuMs)} ms of CPU time`).toBeLessThan(
    parseCpuBudgetMs
  );
  expectCostGrowsLinearly(name, small.cpuMs, full.cpuMs);
  // The full size's throw is reported first: it is the side the row reads its document from. A
  // small side that threw is reported after it, rather than left to pass on the full size alone.
  if (full.thrown !== undefined) throw full.thrown.cause;
  if (small.thrown !== undefined) throw small.thrown.cause;
  if (parsed === undefined) throw new Error("the measured parse produced no document");
  return parsed;
}

/**
 * The refusal a parse raised at the full size, once that refusal has been read at both sizes and
 * found to cost less than `parseCpuBudgetMs` and to grow no faster than its input did at
 * `growthStep`.
 *
 * The two costs are asserted before the refusal is required, for the reason above: a break that
 * makes a refused parse dearer is read as the cost failure the row is for, not as a missing
 * refusal. A side that returned rather than being refused then fails the row, so a row that
 * means to measure a refusal cannot read a plain reading as one. A cause that is not an `Error`
 * is thrown on as it is, so such a throw reaches the runner with its own stack rather than as a
 * message that matched nothing.
 */
export function refusalWithinCostBounds(
  name: string,
  parse: (body: string) => unknown,
  smallBody: string,
  fullBody: string
): Error {
  const small = costReadingOf(() => parse(smallBody));
  const full = costReadingOf(() => parse(fullBody));
  expect(full.cpuMs, `the full size cost ${Math.round(full.cpuMs)} ms of CPU time`).toBeLessThan(
    parseCpuBudgetMs
  );
  expectCostGrowsLinearly(name, small.cpuMs, full.cpuMs);
  if (full.thrown === undefined) {
    throw new Error("the parse at the full size was expected to be refused and was not");
  }
  if (small.thrown === undefined) {
    throw new Error("the parse at the small size was expected to be refused and was not");
  }
  const refusal = full.thrown.cause;
  if (!(refusal instanceof Error)) throw refusal;
  return refusal;
}
