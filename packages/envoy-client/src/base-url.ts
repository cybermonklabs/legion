/**
 * The reading of a Dispatch base URL's trailing slashes that `dispatch-config.ts` and
 * `dispatch-http.ts` share.
 *
 * It is a module of its own, importing nothing, because both of its readers need it and neither
 * may reach the other's imports: `dispatch-config.ts` resolves a credential and so reaches
 * `@legion/broker-client`, while `dispatch-http.ts` is compiled by the dashboard's own tsc
 * program, which must not compile broker-client — a filter of `ci.yaml`'s `changes` job selects
 * the files each typecheck compiles, and a program that reaches a package its filter does not
 * select skips the job that would have caught a change to it
 * (`.github/scripts/check-ts-filters.sh`).
 */

/**
 * `url` without the slashes it ends in, counted from the right. A `/\/+$/` replace reads the same
 * string once per position its unanchored search starts at, which costs a URL of n slashes O(n²)
 * (CodeQL js/polynomial-redos); this reads it once. A URL of nothing but slashes is left empty, as
 * that pattern left it.
 */
export function withoutTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end -= 1;
  return url.slice(0, end);
}
