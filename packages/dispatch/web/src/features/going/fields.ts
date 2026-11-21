/** How the bar reads a JSON value the server sent, a frame's or a plan row's, as one kind. */

/** `value` where it is a string, else "". */
export function textOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** `value`'s fields where it is an object and no array, else none. */
export function objectOf(value: unknown): Readonly<Record<string, unknown>> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}
