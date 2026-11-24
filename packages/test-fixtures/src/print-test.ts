// The TypeScript box print test: a holder of the box (the contracts module's RemoteText), holding
// the probe, printed by every printer at every placement, and beside each such row its
// plain-string twin, which must print a marker so the row is known able to see one. A twin a
// printer cannot reach at the row's placement uses the placement TWIN_PLACEMENTS records for the
// holder's kind, that printer and that placement. Go's half is
// packages/envoy/internal/redact/redacttest/printtest.go.

import { format, inspect } from "node:util";
import { TWIN_PLACEMENTS } from "./print-table";

/** The probe's made-up credentials, and the parameter an access token rides under, in two parts
 *  so no line of this file reads as a credential to a scanner. */
export const REMOTE_USER = "not-a-secret-user";
export const REMOTE_PASSWORD = "not-a-secret-password";
export const REMOTE_TOKEN = "not-a-secret-token";
const PARAM = ["access", "token"].join("_");
/** Words that quote a URL carrying all three, as a refusal may. */
export const REMOTE_WORDS = `the issuer at https://${REMOTE_USER}:${REMOTE_PASSWORD}@issuer.example/path?${PARAM}=${REMOTE_TOKEN}`;

/** One holder: `boxed` holds the probe in each box field; `same` is its twin, the same shape with
 *  the probe as a plain string where each box was. `kind` names the shape, one of {@link KINDS},
 *  whose records in TWIN_PLACEMENTS the rows read, and whose exported twin, the probe in a public
 *  property, stands in for a printer that cannot read a `#` field: it shows that printer's output
 *  captured at that placement, and such a row is counted reshaped or relocated, never same. */
export interface Holder {
  readonly name: string;
  readonly kind: string;
  readonly boxed: unknown;
  readonly same: unknown;
}

/** An error with a public status, whose box fields are `#` fields and whose message quotes them
 *  as they are. An own property changes how Node's and Bun's inspection print an error. */
class ErrorTwin extends Error {
  readonly #v: string;
  constructor(
    readonly status: number,
    v: string
  ) {
    super(`refused: ${v}`);
    this.name = "ErrorTwin";
    this.#v = v;
  }
  get size(): number {
    return this.#v.length;
  }
}

/** An object whose box fields are `#` fields. */
class ClassTwin {
  readonly #v: string;
  constructor(v: string) {
    this.#v = v;
  }
  get size(): number {
    return this.#v.length;
  }
}

/**
 * The shapes a holder has, each with the representative twins TWIN_PLACEMENTS was measured on
 * (gen-print-table.ts): "text", the box itself, whose twin is the probe as a string; "error", an
 * `Error` whose box fields are `#` fields and whose message quotes them; "class", an object whose
 * box fields are `#` fields; "object", a plain object whose properties hold such an object. A
 * holder's own twins must print as its kind's do; one that prints less makes its rows blind.
 */
export const KINDS: Record<string, () => { readonly same: unknown; readonly exported: unknown }> = {
  text: () => ({ same: REMOTE_WORDS, exported: REMOTE_WORDS }),
  error: () => ({
    same: new ErrorTwin(403, REMOTE_WORDS),
    exported: { status: 403, v: REMOTE_WORDS },
  }),
  class: () => ({ same: new ClassTwin(REMOTE_WORDS), exported: { v: REMOTE_WORDS } }),
  object: () => ({
    same: { held: new ClassTwin(REMOTE_WORDS) },
    exported: { held: { v: REMOTE_WORDS } },
  }),
};

const SECRETS = [REMOTE_USER, REMOTE_PASSWORD, REMOTE_TOKEN];

/** Each secret `text` holds. */
export function secretsIn(text: string): string[] {
  return SECRETS.filter((s) => text.includes(s));
}

/** Runs `print` with `console.log` and `console.error` captured, and answers what they wrote. */
function captured(print: (v: unknown) => void, v: unknown): string {
  const lines: string[] = [];
  const log = console.log;
  const error = console.error;
  console.log = (...args: unknown[]) => lines.push(format(...args));
  console.error = (...args: unknown[]) => lines.push(format(...args));
  try {
    print(v);
  } finally {
    console.log = log;
    console.error = error;
  }
  return lines.join("\n");
}

/** The printers, each answering the text it printed. */
export const PRINTERS: Record<string, (v: unknown) => string> = {
  "JSON.stringify": (v) => String(JSON.stringify(v)),
  "util.inspect": (v) => inspect(v, { depth: 10 }),
  "util.inspect-showHidden": (v) => inspect(v, { depth: 10, showHidden: true }),
  "util.format-%s": (v) => format("%s", v),
  "util.format-%o": (v) => format("%o", v),
  "util.format-%O": (v) => format("%O", v),
  "Bun.inspect": (v) => Bun.inspect(v),
  template: (v) => `${v}`,
  "String()": (v) => String(v),
  "+": (v) => "" + v,
  spread: (v) =>
    typeof v === "object" && v !== null
      ? inspect({ ...v }, { depth: 10 }) + JSON.stringify({ ...v })
      : String(v),
  structuredClone: (v) => {
    try {
      return inspect(structuredClone(v), { depth: 10 });
    } catch (error) {
      return `structuredClone refused: ${(error as Error).message}`;
    }
  },
  "console.log": (v) => captured((x) => console.log(x), v),
  "console.error": (v) => captured((x) => console.error(x), v),
};

class PrivateHolder {
  readonly #v: unknown;
  constructor(v: unknown) {
    this.#v = v;
  }
  get held(): unknown {
    return this.#v;
  }
}

/** The placements. */
export const PLACEMENTS: Record<string, (v: unknown) => unknown> = {
  value: (v) => v,
  property: (v) => ({ v }),
  private: (v) => new PrivateHolder(v),
  "nested-1": (v) => ({ a: { v } }),
  "nested-2": (v) => ({ a: { b: { v } } }),
  "nested-3": (v) => ({ a: { b: { c: { v } } } }),
  array: (v) => [v],
  map: (v) => new Map([["k", v]]),
  cause: (v) => new Error("wrapped", { cause: v }),
};

/** The twin for a row, as a TWIN_PLACEMENTS record names it: "same", or "<variant>@<placement>"
 *  with variant "same", "exported" or "string" (the probe as a string); undefined, with the reason,
 *  where the record is missing, "unreachable", or names a variant or placement that is none. */
export function twinOf(
  holder: Holder,
  at: string,
  record: string | undefined
): [unknown, string] | [undefined, string] {
  if (record === undefined) return [undefined, "nowhere: TWIN_PLACEMENTS records no twin for it"];
  if (record === "unreachable")
    return [undefined, "nowhere: no twin was measured to reach this printer here"];
  const [variant, where] =
    record === "same" ? ["same", at] : (record.split("@") as [string, string]);
  const place = PLACEMENTS[where];
  if (place === undefined)
    return [undefined, `nowhere: TWIN_PLACEMENTS records the placement ${where}, which is none`];
  const exported = KINDS[holder.kind]?.().exported;
  const values: Record<string, unknown> = { same: holder.same, exported, string: REMOTE_WORDS };
  if (!(variant in values))
    return [
      undefined,
      `nowhere: TWIN_PLACEMENTS records the twin variant ${variant}, which is none`,
    ];
  return [place(values[variant]), record];
}

/** A row that leaked or was blind. */
export interface PrintFailure {
  readonly row: string;
  readonly why: string;
}

/**
 * Runs every printer at every placement on `holder`, prints each row's output as a
 * `REDACT-CAPTURE print/<holder>/<printer>/<placement> <base64>` line and one
 * `REDACT-PRINT <holder> rows=<n> same=<n> reshaped=<n> relocated=<n> blind=<n>` line, and answers
 * the rows that leaked (`REDACT-ASSERT print/<holder>/...`) or whose twin printed no marker.
 */
export function printTest(holder: Holder): PrintFailure[] {
  if (!(holder.kind in KINDS)) {
    return [
      {
        row: `print/${holder.name}`,
        why: `holder ${holder.name} has kind ${holder.kind}, which is none of KINDS`,
      },
    ];
  }
  const failures: PrintFailure[] = [];
  let rows = 0;
  let same = 0;
  let reshaped = 0;
  let relocated = 0;
  let blind = 0;
  for (const [printer, print] of Object.entries(PRINTERS)) {
    for (const [at, place] of Object.entries(PLACEMENTS)) {
      rows++;
      const capture = `print/${holder.name}/${printer}/${at}`;
      const out = print(place(holder.boxed));
      console.info(`REDACT-CAPTURE ${capture} ${Buffer.from(out, "utf8").toString("base64")}`);
      for (const secret of secretsIn(out))
        failures.push({
          row: capture,
          why: `REDACT-ASSERT ${capture}: the boxed value prints the secret ${secret}`,
        });
      const record = TWIN_PLACEMENTS[`${holder.kind}|${printer}|${at}`];
      if (record === "same") same++;
      else if (record?.split("@")[1] === at) reshaped++;
      else relocated++;
      const [placed, where] = twinOf(holder, at, record);
      const twinOut = placed === undefined ? "" : print(placed);
      if (secretsIn(twinOut).length === 0) {
        blind++;
        failures.push({
          row: capture,
          why: `blind: the twin, placed ${where}, prints no marker under ${printer}`,
        });
      }
    }
  }
  console.info(
    `REDACT-PRINT ${holder.name} rows=${rows} same=${same} reshaped=${reshaped} relocated=${relocated} blind=${blind}`
  );
  return failures;
}
