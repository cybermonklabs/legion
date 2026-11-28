/**
 * recheck's reading of a regular expression (https://makenowjust-labs.github.io/recheck/): whether
 * some text costs a backtracking engine more than linear work in its length. A row cannot count
 * that work, because JavaScriptCore counts no step a regular expression takes, and a limit on time
 * holds no two machines alike; recheck's automaton checker decides it from the pattern's automaton
 * instead, for every text and not only the one a row builds, and with no clock (`timeout: null`),
 * so its verdict is the same on every machine.
 *
 * A pattern recheck reads in a moment is analyzed by its row on every run (`analyzerRows`). One it
 * takes longer over is analyzed once, by the `analyze:patterns` script of the package that runs it
 * (`rewritePatternRecord`), and its verdict is committed in a record beside the test, which its
 * row holds to the pattern's source and flags: a pattern changed since is a failing row until the
 * script runs again.
 */
import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { check } from "recheck";
import { z } from "zod";

/** The recheck version installed, which every record names: the pin of each package's
 * package.json that runs an analyzer row. */
export const RECHECK_VERSION = z
  .object({ version: z.string() })
  .parse(createRequire(import.meta.url)("recheck/package.json")).version;

/** The longest analysis a row runs on every run, in milliseconds; a pattern recheck takes longer
 * over is analyzed by `rewritePatternRecord` and recorded. */
export const MOMENT_MS = 1_000;

/** The time `rewritePatternRecord` gives one pattern's analysis, in milliseconds, before it records
 * the pattern as having no verdict. */
export const ANALYSIS_BUDGET_MS = 300_000;

/** What recheck read in one pattern: its source and flags; recheck's status, or `no verdict` where
 * its analysis had not ended in the time given; what recheck said beside the status (the
 * complexity of a safe pattern, the complexity and an attack string of a vulnerable one, the
 * error of an unknown one); and how long the analysis took, or for no verdict the time it was
 * given, in milliseconds. */
const PatternAnalysis = z.object({
  source: z.string(),
  flags: z.string(),
  status: z.enum(["safe", "vulnerable", "unknown", "no verdict"]),
  verdict: z.string(),
  ms: z.number(),
});
type PatternAnalysis = z.infer<typeof PatternAnalysis>;

/** A package's record of the patterns whose analysis takes more than a moment, by name, with a
 * note of what writes it and the recheck version that read them. */
const PatternRecord = z.object({
  "//": z.string(),
  recheck: z.string(),
  patterns: z.record(z.string(), PatternAnalysis),
});
type PatternRecord = z.infer<typeof PatternRecord>;

/** recheck's automaton checker's reading of `pattern`, with no time limit of its own; with
 * `budgetMs`, the analysis is cancelled at that time and reads as no verdict. */
export async function analyzePattern(pattern: RegExp, budgetMs?: number): Promise<PatternAnalysis> {
  const { source, flags } = pattern;
  const started = performance.now();
  const diagnostics = await check(source, flags, {
    checker: "automaton",
    timeout: null,
    ...(budgetMs === undefined ? {} : { signal: AbortSignal.timeout(budgetMs) }),
  });
  const ms = Math.round(performance.now() - started);
  switch (diagnostics.status) {
    case "safe":
      return { source, flags, status: "safe", verdict: diagnostics.complexity.summary, ms };
    case "vulnerable":
      return {
        source,
        flags,
        status: "vulnerable",
        verdict: `${diagnostics.complexity.summary}, attack ${diagnostics.attack.pattern}`,
        ms,
      };
    case "unknown":
      return diagnostics.error.kind === "cancel" && budgetMs !== undefined
        ? { source, flags, status: "no verdict", verdict: "cancelled", ms: budgetMs }
        : { source, flags, status: "unknown", verdict: JSON.stringify(diagnostics.error), ms };
  }
}

/** What a failing row says of `name`'s analysis: the pattern, the status and what recheck said. */
function reading(name: string, analysis: PatternAnalysis): string {
  return `${name} /${analysis.source}/${analysis.flags} is ${analysis.status}: ${analysis.verdict}`;
}

/**
 * One row per pattern of `patterns`: each the record at `recordPath` names is held to its source
 * and flags and to the recorded verdict, and every other is analyzed by its row. Every verdict must
 * be `safe`, but a pattern of `withoutVerdict` may be recorded as having none, which is a risk its
 * package accepts and names. A further row holds the record to the recheck version installed and
 * to the patterns there are, so a pattern renamed or removed leaves no stale analysis behind.
 */
export function analyzerRows(
  patterns: Readonly<Record<string, RegExp>>,
  recordPath: URL | string,
  withoutVerdict: readonly string[] = []
): void {
  const record = PatternRecord.parse(JSON.parse(readFileSync(recordPath, "utf8")));
  const rerun = `run the package's analyze:patterns script to analyze it again`;
  describe("each pattern, as recheck's automaton checker reads it", () => {
    test(`the record of slow analyses is of recheck ${RECHECK_VERSION} and of these patterns`, () => {
      expect(record.recheck, `the record is of recheck ${record.recheck}: ${rerun}`).toBe(
        RECHECK_VERSION
      );
      const unknown = Object.keys(record.patterns).filter((name) => !(name in patterns));
      expect(unknown, `the record names patterns the module does not run: ${rerun}`).toEqual([]);
      const unexplained = withoutVerdict.filter((name) => !(name in record.patterns));
      expect(unexplained, "a pattern let off a verdict is not in the record").toEqual([]);
    });
    for (const [name, pattern] of Object.entries(patterns)) {
      const recorded = record.patterns[name];
      if (recorded === undefined) {
        test(`${name} is linear in the text`, async () => {
          const analysis = await analyzePattern(pattern);
          expect(analysis.status, reading(name, analysis)).toBe("safe");
        });
        continue;
      }
      test(`${name} is linear in the text, as the record of its slow analysis says`, () => {
        expect(
          { source: pattern.source, flags: pattern.flags },
          `${name} changed since the record was written: ${rerun}`
        ).toEqual({ source: recorded.source, flags: recorded.flags });
        const allowed = withoutVerdict.includes(name) ? ["safe", "no verdict"] : ["safe"];
        expect(allowed, reading(name, recorded)).toContain(recorded.status);
      });
    }
  });
}

/**
 * Analyzes each of `patterns` and writes the record at `recordPath` of those whose analysis took
 * more than `MOMENT_MS` or had no verdict in `ANALYSIS_BUDGET_MS`; `explanation` is the record's
 * own note of what writes it. What it read of each pattern is printed, so a run that ends in a
 * failing verdict says so before a row does.
 *
 * Each analysis runs in a process of its own: the script that calls this runs itself again with
 * the pattern's name as its one argument, and that run prints the analysis as JSON. recheck's
 * agent goes on working at an analysis it was told to cancel until its process ends, which
 * slowed the analysis after a cancelled one from 13 ms to 1,663 ms in a 2-CPU pod; a process
 * that ends with its analysis takes that work with it.
 */
export async function rewritePatternRecord(
  recordPath: URL | string,
  patterns: Readonly<Record<string, RegExp>>,
  explanation: string
): Promise<void> {
  const only = process.argv[2];
  if (only !== undefined) {
    const pattern = patterns[only];
    if (pattern === undefined) throw new Error(`no pattern is named ${only}`);
    process.stdout.write(JSON.stringify(await analyzePattern(pattern, ANALYSIS_BUDGET_MS)));
    return;
  }
  const slow: Record<string, PatternAnalysis> = {};
  for (const name of Object.keys(patterns)) {
    const run = Bun.spawnSync([process.execPath, Bun.main, name], { stderr: "inherit" });
    if (!run.success) throw new Error(`the analysis of ${name} exited with ${run.exitCode}`);
    const analysis = PatternAnalysis.parse(JSON.parse(run.stdout.toString()));
    process.stdout.write(`${reading(name, analysis)} (${analysis.ms} ms)\n`);
    if (analysis.ms > MOMENT_MS || analysis.status === "no verdict") slow[name] = analysis;
  }
  const record: PatternRecord = { "//": explanation, recheck: RECHECK_VERSION, patterns: slow };
  writeFileSync(recordPath, `${JSON.stringify(record, null, 2)}\n`);
}
