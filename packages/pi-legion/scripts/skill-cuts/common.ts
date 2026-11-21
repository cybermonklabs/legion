/**
 * What the skill-cuts commands share: a whole-file write, JSON text, a zod error's faults on one
 * line, strict option parsing, the configuration as a refusal, and the two errors a command's
 * `main` turns into its exit status (`Refused` 2, `Stopped` 3). It imports no other command's
 * file, so every one of them can import it.
 */
import { mkdirSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { messageFor } from "@legion/envoy-client/errors";
import type { z } from "zod";
import { loadConfig, type SkillCutsConfig } from "./config";

/** A command's refusal of its arguments or inputs, before any store read or model call: its
 *  `main` exits 2. */
export class Refused extends Error {}

/** A run that stops on its own rule (the budget is spent, or the memory passed its limit): its
 *  `main` exits 3. */
export class Stopped extends Error {}

/** Writes `text` to `file` whole: into a fresh file beside it that is then renamed over it. */
export function writeWhole(file: string, text: string): void {
  mkdirSync(path.dirname(file), { recursive: true });
  const fresh = `${file}.${process.pid}.tmp`;
  writeFileSync(fresh, text);
  renameSync(fresh, file);
}

/** `value` as JSON, two spaces deep, with a final newline. */
export function json(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

/** Each issue of a zod error as `<path>: <message>`, joined by `; `; an issue of the whole value
 *  is named `root`. */
export function schemaFaults(error: z.ZodError, root = "the value"): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || root}: ${issue.message}`)
    .join("; ");
}

/** Parses `argv` with `options`, strictly, with no positional argument. Throws a refusal that
 *  ends with `usage`. */
export function parse<O extends NonNullable<Parameters<typeof parseArgs>[0]>["options"]>(
  argv: readonly string[],
  options: O,
  usage: string
) {
  try {
    return parseArgs({ args: [...argv], options, strict: true, allowPositionals: false }).values;
  } catch (error) {
    throw new Refused(`${messageFor(error)}. ${usage}`);
  }
}

/** The value of each option `names` names, or a refusal naming the ones missing. */
export function required<K extends string>(
  values: Partial<Record<K, unknown>>,
  names: readonly K[],
  usage: string
): Record<K, string> {
  const missing = names.filter((name) => typeof values[name] !== "string");
  if (missing.length > 0) {
    throw new Refused(`${missing.map((name) => `--${name}`).join(", ")} required. ${usage}`);
  }
  return values as Record<K, string>;
}

/** The configuration in `file`. Throws a refusal naming why it is none. */
export function configFrom(file: string): SkillCutsConfig {
  try {
    return loadConfig(file);
  } catch (error) {
    throw new Refused(messageFor(error));
  }
}
