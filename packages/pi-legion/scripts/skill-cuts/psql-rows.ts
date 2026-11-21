/**
 * Reads what the session store's read command prints: psql's default aligned table, under the
 * command's own flags (`-X -q -w -v ON_ERROR_STOP=1 --single-transaction -c 'SET TRANSACTION READ
 * ONLY' -f -`), for a query of this runner. Every query of the runner returns one column whose
 * every value is one JSON value (`json_build_object(…)::text`), so each row is one line: JSON
 * escapes every newline, and a `|` stays inside a string. The table is a header line, a rule of
 * dashes, one line per row padded with spaces, and the footer `(N rows)`.
 *
 * psql prints a value as it is, except for control characters. JSON escapes each one below U+0020,
 * but not DEL (U+007F) or U+0080 to U+009F. psql prints DEL as the four characters `\x7F`, which
 * is no JSON escape, and U+0080 to U+009F as `\u0080` to `\u009F`, which JSON reads as the same
 * character. So each `\xHH` that stands where a JSON escape starts is read as the escape `\u00HH`;
 * JSON's own text holds none there, since `\x` is no escape of JSON's.
 *
 * `PsqlTable` reads the table a line at a time as the command prints it, so no more than one row's
 * text is held, and gives each row to its caller as its line is read. It refuses output that is
 * not one such table: no rule, a rule of more than one column, no footer, a row count that differs
 * from the footer's, text after the footer, or a row line that is not one JSON value, naming that
 * line's number. `psqlRows` returns the rows of a whole output, read the same way.
 *
 * `PullCheck` checks the rows of a transcript pull (pull.sql), every call's rows in order, one at
 * a time, and gives each row's bytes, which the caller appends to the file's text: the file row's
 * slices (offset -1) first, then each part's slices, each piece's slices in order. It refuses a
 * row of another shape, a row out of order or given twice, a slice left out, and a part that does
 * not start where the text before it ends (start_offset is a byte offset in the file, and the file
 * row holds the file's first bytes), so a page left out cannot pass as a whole file.
 */
import { z } from "zod";

const RULE = /^-+$/;
const FOOTER = /^\((\d+) rows?\)$/;
const NO_RULE = "psql output: no header rule (a line of dashes under a header)";
/** psql's `\xHH` where a JSON escape starts: after no backslash, or after an even run of them. */
const PSQL_HEX = /(?<!\\)((?:\\\\)*)\\x([0-9A-F]{2})/g;

/**
 * One psql aligned table holding one JSON value per row, read a line at a time. Each row goes to
 * `visit` as its line is read. Faults do not throw where they are found: the first fault of the
 * output is kept, no row is given after it, and `end` throws it, so a caller can first report how
 * the command ended. A throw of `visit` is kept the same way, no row is given after it, and `end`
 * throws it after the output's own faults. Lines count from 1.
 */
export class PsqlTable {
  readonly #visit: (row: unknown) => void;
  #line = 0;
  #rule = 0;
  #footer = 0;
  #blank = 0;
  #rows = 0;
  #fault: string | undefined;
  #visitFault: { error: unknown } | undefined;
  #carry: Buffer[] = [];

  constructor(visit: (row: unknown) => void) {
    this.#visit = visit;
  }

  /** Takes the next bytes of the output: each line they end is read, the rest is kept for the
   *  next bytes. */
  write(chunk: Uint8Array): void {
    const bytes = Buffer.from(chunk.buffer, chunk.byteOffset, chunk.byteLength);
    let from = 0;
    for (;;) {
      const newline = bytes.indexOf(0x0a, from);
      if (newline < 0) break;
      const piece = bytes.subarray(from, newline);
      const whole = this.#carry.length === 0 ? piece : Buffer.concat([...this.#carry, piece]);
      this.#carry = [];
      this.line(whole.toString("utf8"));
      from = newline + 1;
    }
    if (from < bytes.length) this.#carry.push(bytes.subarray(from));
  }

  /** Takes the next line of the output, without its newline. */
  line(text: string): void {
    this.#line += 1;
    if (this.#fault !== undefined) return;
    if (this.#rule === 0) {
      if (!/^-[-+]*$/.test(text)) return;
      if (this.#line === 1) this.#fault = NO_RULE;
      else if (!RULE.test(text)) {
        this.#fault = `psql output line ${this.#line}: the table has more than one column`;
      } else this.#rule = this.#line;
      return;
    }
    const trimmed = text.trim();
    if (this.#footer > 0) {
      if (trimmed !== "")
        this.#fault = `psql output line ${this.#line}: text after the table's footer`;
      return;
    }
    if (this.#blank > 0 && trimmed !== "") {
      this.#fault = `psql output line ${this.#blank}: not one JSON value: ""`;
      return;
    }
    const footer = FOOTER.exec(text);
    if (footer) {
      const count = Number(footer[1]);
      if (count !== this.#rows) {
        this.#fault = `psql output line ${this.#line}: the footer gives ${count} rows, the table holds ${this.#rows}`;
      } else this.#footer = this.#line;
      return;
    }
    if (trimmed === "") {
      this.#blank = this.#line;
      return;
    }
    let row: unknown;
    try {
      row = JSON.parse(trimmed.includes("\\x") ? trimmed.replace(PSQL_HEX, "$1\\u00$2") : trimmed);
    } catch {
      this.#fault = `psql output line ${this.#line}: not one JSON value: ${JSON.stringify(trimmed.slice(0, 120))}`;
      return;
    }
    this.#rows += 1;
    if (this.#visitFault !== undefined) return;
    try {
      this.#visit(row);
    } catch (error) {
      this.#visitFault = { error };
    }
  }

  /** Ends the output: the rows the table holds. Throws the output's first fault, else the first
   *  throw of `visit`. */
  end(): number {
    if (this.#carry.length > 0) {
      const last = Buffer.concat(this.#carry).toString("utf8");
      this.#carry = [];
      this.line(last);
    }
    if (this.#fault !== undefined) throw new Error(this.#fault);
    if (this.#rule === 0) throw new Error(NO_RULE);
    if (this.#footer === 0) throw new Error("psql output: no footer (N rows) after the table");
    if (this.#visitFault !== undefined) throw this.#visitFault.error;
    return this.#rows;
  }
}

/** The rows of one psql aligned table holding one JSON value per row; throws when `output` is not
 *  one, naming the line at fault (lines count from 1). */
export function psqlRows(output: string): unknown[] {
  const rows: unknown[] = [];
  const table = new PsqlTable((row) => {
    rows.push(row);
  });
  for (const line of output.split("\n")) table.line(line);
  table.end();
  return rows;
}

/** One row of a transcript pull: a piece's start offset (-1 for the file row), the slice of the
 *  piece (from 0), and the slice's text. */
export const PullRow = z.strictObject({
  o: z.number().int().min(-1),
  k: z.number().int().min(0),
  c: z.string(),
});
export type PullRow = z.infer<typeof PullRow>;

/** A pull row's place: its piece's offset and its slice. A call returns the rows after one. */
export interface PullKey {
  readonly o: number;
  readonly k: number;
}

/** The place before every row, where the first call starts. */
export const PULL_START: PullKey = { o: -2, k: 0 };

/** Checks the rows of one file's transcript pull, every call's rows in order, one at a time. */
export class PullCheck {
  #rows = 0;
  #last: PullKey = PULL_START;
  #bytes = 0;

  /** The place of the last row taken (PULL_START before any): where the next call starts. */
  get last(): PullKey {
    return this.#last;
  }

  /** The bytes of the rows taken: the length of the file's text so far. */
  get bytes(): number {
    return this.#bytes;
  }

  /** The bytes of `row`, the next row of the pull: what the file's text holds next. Throws when
   *  `row` is not one or does not follow the row before it. */
  take(row: unknown): Buffer {
    this.#rows += 1;
    const parsed = PullRow.safeParse(row);
    if (!parsed.success) {
      throw new Error(
        `pull row ${this.#rows} is not {"o": <offset>, "k": <slice>, "c": <text>}: ${parsed.error.message}`
      );
    }
    const { o, k, c } = parsed.data;
    const last = this.#last;
    const at = `the piece at offset ${o}, slice ${k},`;
    if (o === last.o && k === last.k) throw new Error(`pull rows give ${at} twice`);
    if (o < last.o || (o === last.o && k < last.k)) {
      throw new Error(`pull rows: ${at} comes after the one at offset ${last.o}, slice ${last.k}`);
    }
    if (o === last.o && k !== last.k + 1) {
      throw new Error(`pull rows: ${at} does not follow slice ${last.k}`);
    }
    if (o !== last.o && k !== 0) throw new Error(`pull rows: ${at} is not the piece's first`);
    if (o !== last.o && o >= 0 && o !== this.#bytes) {
      throw new Error(
        `pull rows: the part at offset ${o} does not start where the text before it ends (${this.#bytes})`
      );
    }
    this.#last = { o, k };
    const bytes = Buffer.from(c, "utf8");
    this.#bytes += bytes.length;
    return bytes;
  }
}
