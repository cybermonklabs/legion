/**
 * `url` without the slashes it ends in, counted from the right. A `/\/+$/` replace reads the same
 * string once per position its unanchored search starts at, which costs a URL with a run of n
 * slashes in it O(n²) (CodeQL js/polynomial-redos); this reads it once.
 *
 * A module of its own, importing nothing, because of what the alternative costs a reader of it.
 * This lived in `dispatch-config.ts`, which imports the broker credential's DPoP and token code;
 * `dispatch-http.ts` importing it from there put those seven files into the program `tsc` compiles
 * for `packages/dispatch`, through the fake Dispatch its browser-suite tests import, for the sake
 * of a slash-trimming helper. A file that every URL-holding module may import is a file that
 * imports nothing (dispatch://INFRA-341).
 */
export function withoutTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end -= 1;
  return url.slice(0, end);
}
