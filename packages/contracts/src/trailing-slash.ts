// The one rule every half of this codebase reads a configured address by. An operator types a
// server or broker URL by hand into a unit file, a pod's environment or a shell, so an address
// ending in more than one slash is a typo rather than a shape to refuse; two clients that trimmed
// differently would post `//v1/...` and `/v1/...` for the same configuration. Go's half is
// `brokerclient.WithoutTrailingSlashes`.

/**
 * `url` without the slashes it ends in, counted from the right.
 *
 * A `/\/+$/` replace reads the same string once per position its unanchored search starts at,
 * which costs a URL of n slashes O(n²) (CodeQL js/polynomial-redos); this reads it once.
 */
export function withoutTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end -= 1;
  return url.slice(0, end);
}
