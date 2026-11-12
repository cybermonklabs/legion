import type { GoingBlockData } from "../../../api/types";

/** One kind's block frame, as `GoingBlockData` carries it. */
export type BlockOf<K extends GoingBlockData["kind"]> = Extract<GoingBlockData, { kind: K }>;

/**
 * A row's value as a block draws it: a number, a string or a boolean as itself, and nothing for a
 * value the read left empty. A block shows exactly what the server's read returned, so a number
 * is written in full as the read wrote it, never rounded or grouped into another number.
 */
export function cellText(value: unknown): string {
  if (value === null || value === undefined) {
    return "";
  }
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return JSON.stringify(value);
}

/** A row's value as the number a bar or a chart plots, or undefined where it holds none. */
export function cellNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}
