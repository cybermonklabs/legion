// Holding a text to the names it carries rather than to its words. A test of a tool's description
// or a command's help holds the fields, flags, values, codes and commands that text names, each as
// a whole token, so the sentences around them may be reworded and a text that stops naming one
// fails. It lives in this package because the descriptions are held from packages/contracts,
// packages/envoy-client, packages/pi-shared and packages/pi-legion alike.

/**
 * Whether `text` holds `name` as a whole token: not inside a longer name such as `merged_by` for
 * `merged` or `issue-update` for `issue`, on each side where `name` begins or ends with a letter,
 * a digit or `_`. A name that begins or ends with other text, such as `--issue` or `` `Text:` ``,
 * is bounded by that text on that side.
 */
export function namesWhole(text: string, name: string): boolean {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
  const start = /^\w/u.test(name) ? "(?<![\\w-])" : "";
  const end = /\w$/u.test(name) ? "(?![\\w-])" : "";
  return new RegExp(`${start}${escaped}${end}`, "u").test(text);
}

/** The names of `names` that `text` does not hold whole, in their order: empty when it holds all. */
export function namesMissing(text: string, names: readonly string[]): string[] {
  return names.filter((name) => !namesWhole(text, name));
}
