// What the views demo's stub writes as the real server writes it
// (packages/dispatch/e2e/views-demo/README.md): the status order, a time, and a rank.

/** The lifecycle's statuses in the listing's order (model.IssueStatuses). */
export const ISSUE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
];

const RANK_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

/** A time as the server writes it (RFC 3339 with trailing fractional zeros dropped). */
export function goTime(date: Date): string {
  return date.toISOString().replace(/\.?0+Z$/, "Z");
}

/** rank.Between (internal/dispatch/rank): a base62 key strictly between two others. */
export function rankBetween(previous: string, next: string): string {
  const digit = (char: string) => RANK_ALPHABET.indexOf(char);
  const after = (prev: string): string => {
    for (let i = 0; i < prev.length; i++) {
      const d = digit(prev[i]);
      if (d < RANK_ALPHABET.length - 1) return prev.slice(0, i) + RANK_ALPHABET[d + 1];
    }
    return `${prev}U`;
  };
  const before = (upper: string): string => {
    if (upper === "") return "U";
    const d = digit(upper[0]);
    if (d > 1) return RANK_ALPHABET[Math.floor(d / 2)];
    return RANK_ALPHABET[0] + before(upper.slice(1));
  };
  if (previous === "") return before(next);
  if (next === "") return after(previous);
  let common = 0;
  while (common < previous.length && common < next.length && previous[common] === next[common]) {
    common++;
  }
  if (common === previous.length) return previous + before(next.slice(common));
  const low = digit(previous[common]);
  const high = digit(next[common]);
  if (high - low > 1)
    return previous.slice(0, common) + RANK_ALPHABET[Math.floor((low + high) / 2)];
  return previous.slice(0, common + 1) + after(previous.slice(common + 1));
}

/** The place `{before, after}` puts an item at among `ranks` (api/issue_rank.go): `after` the
 *  rank it lands below, `before` the one it lands above, each "" where it names none. */
export function rankAt(ranks: readonly string[], after: string, before: string): string {
  const sorted = [...ranks].sort();
  let previous = after;
  let next = before;
  if (previous !== "") {
    next = sorted.find((rank) => rank > previous && (next === "" || rank < next)) ?? next;
  } else if (next !== "") {
    previous = sorted.filter((rank) => rank < next).at(-1) ?? "";
  } else {
    previous = sorted.at(-1) ?? "";
  }
  return rankBetween(previous, next);
}
