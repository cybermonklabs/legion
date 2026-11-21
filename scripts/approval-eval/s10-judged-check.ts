import { judgedOutcomeOf, OUTCOME_FILE } from "./s10-judged-io";

/**
 * The read-back `s10_judge.py` runs before it commits each request's outcome: the outcome file's
 * text on standard input, read with the analysis's own parser (`judgedOutcomeOf`), so a stage the
 * claim run writes is one `analyze.ts --claim-s10` reads, whatever S10's frozen code returned. It
 * prints, on one line of JSON, why the parser refuses the text, or null where it reads it back:
 * `{"refused": null}` or `{"refused": "<the parser's error>"}`. Any error the parser raises is a
 * refusal, since the analysis would stop on it too. Exit 0 whenever it has read its input; any
 * other exit is a failure of the read-back itself, which stops the judging run.
 */

const USAGE = `usage: bun scripts/approval-eval/s10-judged-check.ts [<path the outcome is written at>]

Reads one outcome file of the judged stage on standard input and prints, on one line of JSON,
whether the analysis's parser refuses it: {"refused": null} or {"refused": "<reason>"}.`;

/** Why `judgedOutcomeOf` refuses `text` as the outcome at `path`, or null where it reads it. */
function refusalOf(text: string, path: string): string | null {
  try {
    judgedOutcomeOf(text, path);
    return null;
  } catch (error) {
    return error instanceof Error ? `${error.name}: ${error.message}` : String(error);
  }
}

/** The command: the refusal on one line of JSON, and the exit status it takes. */
export async function main(args: readonly string[]): Promise<number> {
  if (args.length > 1) {
    console.error(USAGE);
    return 2;
  }
  const text = await Bun.stdin.text();
  console.log(JSON.stringify({ refused: refusalOf(text, args[0] ?? OUTCOME_FILE) }));
  return 0;
}

if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
