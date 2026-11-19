/**
 * What the search prints: each line it finds, with what writes the value there, which settings it
 * comes from and where it is printed, and the verdict of the run.
 */
import type { Judged } from "../setting-prints.judged";
import type { Finding } from "./model";

export function printFinding(finding: Finding, judged: Judged | undefined, quiet: boolean) {
  console.log(`${finding.file}:${finding.line}: ${finding.text}`);
  console.log(`  written by ${finding.hows.join("; ")}: ${finding.expressions.join(" | ")}`);
  console.log(`  from ${finding.sources.join("; ")}`);
  console.log(`  printed by ${finding.ends.join("; ")}`);
  console.log(
    quiet
      ? "  quiet: no setting it prints reaches a URL"
      : judged === undefined
        ? "  NOT JUDGED"
        : `  judged: ${judged.why}`
  );
}

/** A judgement that names no line the search found. */
export function printStale(judged: Judged) {
  console.log(`STALE ${judged.file}: no line found holds ${JSON.stringify(judged.text)}`);
}

/** A judgement whose basis no longer holds, with why (`staleBases`). */
export function printShaken(judged: Judged, why: string) {
  console.log(
    `STALE ${judged.file}: the judgement of ${JSON.stringify(judged.text)} ${why}; read it again and write the digest`
  );
}

/** The run's summary line and its verdict, printed, and its exit status: 2 when the search cannot
 *  see a shape it must see, 1 when a line it found is not judged, a judgement names no line or a
 *  basis of one no longer holds, and 0 when every line it found is judged. */
export function verdict(run: {
  readonly scope: number;
  readonly outside: number;
  readonly files: number;
  readonly printing: number;
  readonly rounds: number;
  readonly quiet: number;
  readonly go: string;
  readonly blind: readonly string[];
  readonly found: number;
  readonly open: number;
  readonly stale: number;
  readonly shaken: number;
}): number {
  console.log(
    `Scope: ${run.scope} files this pull request changes, and the files whose written text a line of those prints; ${run.outside} lines outside it are not listed. TypeScript: ${run.files - 1} files read (and the planted one), ${run.printing} calls that print, ${run.rounds} rounds; ${run.quiet} more lines in the scope print a setting that reaches no URL. Go: ${run.go}`
  );
  if (run.blind.length > 0 || run.files <= 1) {
    for (const line of run.blind) console.log(`BLIND ${line}`);
    console.log(
      `UNKNOWN: the search cannot see a shape it must see; ${run.found} lines found, ${run.open} not judged`
    );
    return 2;
  }
  if (run.open > 0 || run.stale > 0 || run.shaken > 0) {
    console.log(
      `FAIL ${run.found} lines print a setting that can be a URL; ${run.open} not judged; ${run.stale} judgements name no line; ${run.shaken} rest on a basis that no longer holds`
    );
    return 1;
  }
  console.log(
    `PASS ${run.found} lines print a setting that can be a URL, each judged; the planted shapes were found`
  );
  return 0;
}
