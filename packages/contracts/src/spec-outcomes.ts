/**
 * The line form of an outcome of a change (R27; D6 of the work-units plan), the one form every
 * surface writes: `(requires <statement-id>) <text>` for an outcome an approved statement
 * requires, `(restored) <text>` for an outcome the change restores for the conclusion's
 * statement, and `<text>` for an outcome no approved statement covers. Dispatch reads a line by
 * the same rules (`specchange.ParseOutcome` in packages/envoy/internal/dispatch/specchange), so
 * `dispatch outcomes` refuses before it sends what Dispatch would refuse.
 */

/** One outcome in its JSON form, as `POST /api/v1/issues/{key}/outcomes` takes it. */
export interface OutcomeOfLine {
  readonly text: string;
  readonly statement?: string;
  readonly restored?: boolean;
}

/** The form of an id of The Spec a `(requires <id>)` marker names. */
export const STATEMENT_ID_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;

const REQUIRES_OPENING = "(requires";
const RESTORED_MARKER = "(restored)";

/** One outcome line read, or the reason it is refused. */
export type OutcomeLineReading =
  | { readonly outcome: OutcomeOfLine; readonly problem?: undefined }
  | { readonly outcome?: undefined; readonly problem: string };

/**
 * Reads one outcome line. A line that opens with a marker must hold one in full and a text after
 * it; any other nonblank line is an outcome no approved statement covers, as written. The space
 * around the line and between the marker and its text belongs to neither.
 */
export function parseOutcomeLine(raw: string): OutcomeLineReading {
  const line = raw.trim();
  if (line === "") return { problem: "the line is blank: an outcome is one line of text" };
  if (line.startsWith(RESTORED_MARKER)) {
    const text = line.slice(RESTORED_MARKER.length).trim();
    return text === ""
      ? { problem: `${JSON.stringify(line)} names no outcome after (restored)` }
      : { outcome: { text, restored: true } };
  }
  if (line.startsWith(`${REQUIRES_OPENING} `) || line.startsWith(`${REQUIRES_OPENING})`)) {
    const rest = line.slice(REQUIRES_OPENING.length);
    const close = rest.indexOf(")");
    const id = close < 0 ? "" : rest.slice(0, close).trim();
    if (close < 0 || !STATEMENT_ID_PATTERN.test(id)) {
      return {
        problem: `${JSON.stringify(line)} names no statement id in (requires <id>): an id is lower-case letters, digits, '.', '_' and '-'`,
      };
    }
    const text = rest.slice(close + 1).trim();
    return text === ""
      ? { problem: `${JSON.stringify(line)} names no outcome after (requires ${id})` }
      : { outcome: { text, statement: id } };
  }
  return { outcome: { text: line } };
}

/** An outcome in the line form, which `parseOutcomeLine` reads back. */
export function outcomeLine(outcome: OutcomeOfLine): string {
  if (outcome.statement !== undefined && outcome.statement !== "") {
    return `${REQUIRES_OPENING} ${outcome.statement}) ${outcome.text}`;
  }
  return outcome.restored === true ? `${RESTORED_MARKER} ${outcome.text}` : outcome.text;
}

/**
 * The outcomes of a list body, one per nonblank line in order, or the first refused line with its
 * number. An empty body, or one of blank lines alone, is an empty list.
 */
export function parseOutcomeLines(
  body: string
):
  | { readonly outcomes: readonly OutcomeOfLine[]; readonly problem?: undefined }
  | { readonly outcomes?: undefined; readonly problem: string } {
  const outcomes: OutcomeOfLine[] = [];
  const lines = body.split(/\r?\n/);
  for (const [index, line] of lines.entries()) {
    if (line.trim() === "") continue;
    const reading = parseOutcomeLine(line);
    if (reading.problem !== undefined) return { problem: `line ${index + 1}: ${reading.problem}` };
    outcomes.push(reading.outcome);
  }
  return { outcomes };
}
