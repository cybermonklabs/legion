// The records a worker run leaves and the line each is opened with, in the one place that line
// is written and read: the gh stand-in opens <run>/calls.jsonl the first time it is invoked, the
// daemon stand-in opens its log when it starts listening, and score.ts passes that line over
// wherever it reads a record. Writer and reader are two programs, so the marker written here and
// matched there is one fact and belongs in one module: a stand-in that wrote another key would
// have its opening line read as a call of the run, and the run that called nothing would be
// scored as one that did.
import { z } from "zod";

/** The gh and bun stand-ins' record of every call they answered, under the run directory. */
export const CALLS = "calls.jsonl";

/** The line the writer of a record opens it with, `{at, opened}`, with `record` the record's own
 * name (`CALLS`, and the daemon stand-in's log file). It says its writer was there and records
 * no act of the run, so no score reads it: a record holding it alone is the run that called
 * nothing or moved nothing, where a record nothing wrote at all is a rig error (score.ts's
 * `unscored`). It carries its own newline, as every line of a JSONL record does. */
export function openingLine(record: string): string {
  return `${JSON.stringify({ at: new Date().toISOString(), opened: record })}\n`;
}

/** That same line as its reader reads it (`openingLine`), which is how score.ts tells it from a
 * call, a push or a move. */
export const Opened = z.looseObject({ at: z.string(), opened: z.string() });

/** A reader's refusal to judge a run: an input the run did not record, one it recorded that
 * cannot be rebuilt from what the caller named (the architect's ruling of 2026-11-18: a reader
 * judges a run from its archive and the provenance it recorded alone, never from what the pod
 * holds now), or an entry of a runs directory that is neither a run nor its record. No verdict:
 * `rig.sh health`, `rig.sh score` and judge.ts each name the cause and exit 2, and none counts or
 * replaces the run. */
export class Refused extends Error {}

/** `read`, run at its first call alone, and its answer kept for every later one: a read of a
 * run's record or remote that several answers need, read once. A leaf of this module, so every
 * program of the run reads it from one place: completion-answers.ts (the daemon's reads), the gh
 * stand-in (facts of the pull request), and score.ts (its lazy judge readings). It lives here and
 * not in completion-answers.ts since that module imports run-names.ts, whose words the bundle a
 * run's PATH resolves `gh` to may not hold (gh-standin.test.ts's row "the gh a run's PATH resolves
 * to names none of the rig's words"), and this module is in that bundle already. */
export function once<T>(read: () => T): () => T {
  let kept: { readonly value: T } | undefined;
  return () => {
    kept ??= { value: read() };
    return kept.value;
  };
}
