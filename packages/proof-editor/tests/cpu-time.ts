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
 * slower machine. The nearest break on the other side is
 * `patches/micromark-core-commonmark@2.0.3.patch` reverted, which cost 18,230 ms of CPU time in
 * one round of control measurements and 24,750 ms in another: 1.5 to 2 times this budget. That
 * margin is the room the budget's size rests on. Every reading was taken with `cpuTimeOf` below,
 * the port of `packages/envoy/internal/cputest`.
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
 * The wall-clock ceiling every row of tests/heavy-bodies.test.ts carries, and the two measured
 * rows of tests/nested-directive.test.ts, in place of bun's own default of 5,000 ms.
 *
 * On bun 1.4.4 a per-test limit does not interrupt a synchronous body: it is reported once that
 * body returns, so a row whose body ran past this fails on it afterwards. It is a ceiling set far
 * above any healthy row, not a hang guard. Under the calibrated load of 12 busy loops the slowest
 * of these rows, "the default mode parses 49 items nested by indentation then blank lines", took
 * 64.4 s of wall time, and the next three took 61.3 s, 54.8 s and 52.4 s, so this ceiling is about
 * 4.7 times the slowest (bun's junit reporter, one loaded run of the two files, 2026-11-09). What
 * ends a real hang is the `dispatch-ui` job's `timeout-minutes: 15` in .github/workflows/ci.yaml.
 */
export const measuredRowCeilingMs = 300_000;

/**
 * The wall-clock ceiling the four rows of tests/nested-directive.test.ts that read no cost carry,
 * in place of bun's own default of 5,000 ms.
 *
 * It is a ceiling of the same kind as `measuredRowCeilingMs` above: reported once the row's body
 * returns, rather than interrupting it. Under the same load of 12 busy loops those four rows cost
 * 0.13 s to 0.73 s of wall time, so this ceiling is about 82 times the dearest of them. What ends
 * a real hang is the `dispatch-ui` job's `timeout-minutes: 15` in .github/workflows/ci.yaml.
 */
export const plainRowCeilingMs = 60_000;

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
 * One side of a growth step, read: what one parse of that side cost, and what it threw where it
 * threw.
 *
 * The cost and the throw come back together, as `Measure` in `packages/envoy/internal/cputest`
 * returns its duration beside its error, so a row can assert the cost of a parse that refuses its
 * input and then assert the refusal itself. A break can make a parse both dearer and throwing, and
 * what the row is for is the cost (INFRA-1629).
 */
type CostReading = {
  /** The CPU time one run cost: the window the runs filled, divided by those runs. */
  readonly cpuMs: number;
  /** Whether a run threw. A run that throws `undefined` has thrown too, which is why this stands
   *  beside `thrown` rather than being read off it. */
  readonly threw: boolean;
  /** The last throw a run raised, `undefined` where no run threw. */
  readonly thrown: unknown;
};

/**
 * The CPU time one run of `work` costs, in milliseconds, with the throw where `work` threw.
 *
 * It runs `work` once in a timed window, and runs it more times in the next window until a window
 * lasts `windowSteps` clock steps, then divides that window by its runs. A shorter window is
 * discarded, never averaged in. This is `Measure` in `packages/envoy/internal/cputest`: the run
 * count doubles, or jumps straight to a quarter past the window when the window so far says that
 * is further, so a run a little slower than this window's average still lands the next window past
 * the mark.
 *
 * A run that throws does not end the window: the throw is kept, the rest of the window runs, and
 * the reading carries the window's per-run cost beside the last throw. The parses these rows
 * measure refuse their input every time, so every run of such a window throws and the window
 * measures exactly the work the refusal costs. The window is what makes the reading steady: one
 * unrepeated run of a 20 ms refusal reads as 10, 20 or 30 ms on this pod's 10 ms clock, an error
 * of up to 50 per cent on the baseline of a growth check. A throw from a window too short to keep
 * is kept as well, so a work that throws once and returns on every later run is read as a work
 * that threw, never as a plain cost.
 */
function cpuTimeOf(work: () => unknown): CostReading {
  const want = windowSteps * cpuClockStepMs();
  let threw = false;
  let thrown: unknown;
  let runs = 1;
  for (;;) {
    const started = processCpuMs();
    for (let run = 0; run < runs; run++) {
      try {
        work();
      } catch (cause) {
        threw = true;
        thrown = cause;
      }
    }
    const took = processCpuMs() - started;
    if (took >= want) return { cpuMs: took / runs, threw, thrown };
    if (took <= 0) {
      runs *= 10;
      continue;
    }
    runs = Math.max(runs * 2, Math.floor((runs * want * 5) / (took * 4)) + 1);
  }
}

/**
 * The least cost of `defaultSamples` readings of `cpuTimeOf`, with the last throw of them.
 *
 * Both sides of a growth check take the same number of samples. Sampling the small side more often
 * than the large one biases the baseline low under bursty load and inflates the ratio, which is the
 * defect `linear_test.go`'s own `sampleCount` comment records from its third round. A work that
 * throws is sampled as many times as any other.
 *
 * A reading already past `parseCpuBudgetMs` is not read again: a second reading cannot bring it
 * under the budget, and taking one doubles the cost of the failure these rows exist to catch. With
 * the nesting bound of src/block-nesting.ts raised past the shape, `a chain of 15,999 list markers
 * on one line` costs 235,560 ms of CPU time read-only, so a second reading of it would carry that
 * row past `measuredRowCeilingMs` and report that ceiling where the budget should report a clean
 * failure. Every reading under the budget, which is every healthy row, is read as many times as
 * before, so the symmetry above is untouched.
 */
function fastestCpuTimeOf(work: () => unknown): CostReading {
  let best = Number.POSITIVE_INFINITY;
  let threw = false;
  let thrown: unknown;
  for (let sample = 0; sample < defaultSamples; sample++) {
    const reading = cpuTimeOf(work);
    if (reading.cpuMs < best) best = reading.cpuMs;
    if (reading.threw) {
      threw = true;
      thrown = reading.thrown;
    }
    if (best > parseCpuBudgetMs) break;
  }
  return { cpuMs: best, threw, thrown };
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
 * shape's growing dimension did at `growthStep`. It reports what `costGrewTooFast` decided, as
 * `growsLinearly` in `linear_test.go` reports `growthVerdict`'s. Both costs are in milliseconds.
 */
export function expectCostGrowsLinearly(name: string, small: number, full: number): void {
  const bound = linearSlack * growthStep;
  const ratio = full / small;
  const reading = `${Math.round(full)} ms at the full size against ${Math.round(small)} ms`;
  // The small side's share of the growing dimension, read from `growthStep` so that the sentence
  // cannot contradict the step it reports.
  const fraction = `1/${growthStep}`;
  const grew = `at ${fraction} of its growing dimension, ${ratio.toFixed(1)} times`;
  const larger = `for a dimension ${growthStep} times larger (at most ${bound} times)`;
  expect(costGrewTooFast(small, full), `${name} cost ${reading} ${grew} ${larger}`).toBe(false);
}

/**
 * A shape a row measures, at the two sizes of a growth step.
 *
 * `grow` builds the shape at a size of the dimension whose growth costs. The helpers below build
 * the shape at `full` and at an eighth of that size themselves, so the growth check's bound of
 * `linearSlack * growthStep` is the bound of the step the two sizes actually make, which a caller
 * doing its own arithmetic could not promise. `growsLinearly` in `linear_test.go` takes its sizes
 * the same way. tests/heavy-bodies.test.ts's `HeavyShape` and tests/nested-directive.test.ts's
 * `DirectiveBody` both extend this.
 */
export type CostShape = {
  /** What the shape is, as a row's name says it. */
  name: string;
  /** The shape at `size` of its growing dimension. */
  grow: (size: number) => string;
  /** The size at which the body is the one the row stands for. */
  full: number;
};

/**
 * Reads `shape` at both sizes and holds the full size to `parseCpuBudgetMs` and to the growth
 * check: the two assertions both helpers below share.
 *
 * `parse` is given the body to read, so the two sizes go through one closure and differ by their
 * input and by nothing else; `what` is what the growth check calls the parse in its message. Both
 * bodies are built before either is measured, as `cpuTimeOf` in `linear_test.go` builds a run's
 * input outside the window it times: building a body is not the work under test.
 */
function costsWithinBounds(
  what: string,
  shape: CostShape,
  parse: (body: string) => unknown
): { small: CostReading; full: CostReading } {
  const smallBody = shape.grow(Math.floor(shape.full / growthStep));
  const fullBody = shape.grow(shape.full);
  const small = fastestCpuTimeOf(() => parse(smallBody));
  const full = fastestCpuTimeOf(() => parse(fullBody));
  expect(full.cpuMs, `the full size cost ${Math.round(full.cpuMs)} ms of CPU time`).toBeLessThan(
    parseCpuBudgetMs
  );
  expectCostGrowsLinearly(what, small.cpuMs, full.cpuMs);
  return { small, full };
}

/**
 * What a parse produced at the full size, once `shape` has been read at both sizes and found to
 * cost less than `parseCpuBudgetMs` and to grow no faster than its growing dimension did at
 * `growthStep`.
 *
 * The row asserts what it reads on the document this returns, which is the one the full size's
 * measured parse produced.
 *
 * A parse that threw where it was to return a document is reported only once both assertions have
 * passed, because a break can make a parse both dearer and throwing and the cost is what the row
 * is for. The throw reaches the runner as itself, with its own stack.
 */
export function parsedWithinCostBounds<T>(
  what: string,
  shape: CostShape,
  parse: (body: string) => T
): T {
  let parsed: T | undefined;
  const { small, full } = costsWithinBounds(what, shape, (body) => {
    parsed = parse(body);
  });
  // The full size's throw is reported first: it is the side the row reads its document from. A
  // small side that threw is reported after it, rather than left to pass on the full size alone.
  if (full.threw) throw full.thrown;
  if (small.threw) throw small.thrown;
  if (parsed === undefined) throw new Error("the measured parse produced no document");
  return parsed;
}

/**
 * Fails unless the parse of one side was refused with an `Error` whose message matches `refused`.
 *
 * A throw that is not an `Error` is thrown on as it is, so it reaches the runner with its own
 * stack rather than as a message that matched nothing.
 */
function expectRefusal(side: string, reading: CostReading, refused: RegExp): void {
  if (!reading.threw) {
    throw new Error(`the parse at the ${side} size was expected to be refused and was not`);
  }
  if (!(reading.thrown instanceof Error)) throw reading.thrown;
  const what = `the parse at the ${side} size was refused by something else`;
  expect(reading.thrown.message, what).toMatch(refused);
}

/**
 * Holds a parse that refuses `shape` to the two cost assertions above, and then to the refusal
 * itself at both sizes: each must throw an `Error` whose message matches `refused`.
 *
 * The costs are asserted first, for the reason above: a break that makes a refused parse dearer is
 * read as the cost failure the row is for, not as a missing refusal. Both sides are then checked
 * against the pattern, because a side that failed with something else — a `RangeError` from a
 * stack it overflowed, say — is not the refusal this row measures, and reading it as one puts the
 * cost of another failure into the growth check (INFRA-1629).
 */
export function refusalWithinCostBounds(
  what: string,
  shape: CostShape,
  parse: (body: string) => unknown,
  refused: RegExp
): void {
  const { small, full } = costsWithinBounds(what, shape, parse);
  expectRefusal("full", full, refused);
  expectRefusal("small", small, refused);
}
