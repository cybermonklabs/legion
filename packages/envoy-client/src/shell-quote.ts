/** `value` as one shell word, safe to paste whatever it holds: bare when every character is in
 *  [A-Za-z0-9_@%+=:,./-], otherwise POSIX single-quoted, each ' written as '\''. The repository's
 *  one rule, held to packages/envoy/scripts/fixtures/shell-quote.json, which the Go copy
 *  (packages/envoy/internal/shellquote) and packages/envoy/scripts/dev-postgres.sh's `quote` are
 *  held to as well. */
export function shellQuote(value: string): string {
  return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value) ? value : `'${value.replaceAll("'", "'\\''")}'`;
}
