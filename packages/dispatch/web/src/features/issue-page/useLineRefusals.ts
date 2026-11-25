import { useRef } from "react";

/** One read of a failed-read line, as `useLineRefusals` counts it in one render. */
export interface LineRead {
  /** How many times its query has been refused (TanStack Query's `errorUpdateCount`). */
  readonly errorUpdateCount: number;
  /** Whether it counts against the line now: for the Spec view, that it stands failed
   *  (`standsFailed`); for the names, that it holds no answer under its set of addresses. */
  readonly failing: boolean;
  /** Its key, hashed (`hashKey`): a read under another key is a query of its own. */
  readonly hash: string;
}

/** What `useLineRefusals` kept of one line at the last draw. */
interface LineCount {
  /** How many times the line's reads had been refused since it last stood. */
  readonly refusals: number;
  /** Each of the line's reads at that draw, in the line's order. */
  readonly seen: readonly LineRead[];
}

/**
 * How many times the failing reads of each failed-read line have been refused since the line last
 * stood, which keys the line's alert (`FailedReadLine`'s `failure`), so a screen reader says the
 * failure again exactly for a refusal new to the line. Its two callers are the Spec view's lines
 * (`SpecFilterBody`) and the names line (`useIssueTree`'s `namesFailure`).
 *
 * The rule: while a read of a line is failing, the line's count is its count at the last draw
 * plus, for each failing read, what its `errorUpdateCount` rose by since the last draw at the same
 * key, or the whole of it where the read was not seen at that place or its key's hash differs;
 * while none is failing, it is 0. A read under another key - a names read moved to another set of
 * addresses - is a query of its
 * own, so the refusals it holds are new to the line. An answer leaves `errorUpdateCount` as it is:
 * TanStack sets it to 0 only for a query made afresh or reset. Every read's count and hash are
 * kept at every draw, failing or not, so a read that answers takes nothing out of the count, and a
 * new refusal of another read of the line in the same draw is still counted.
 *
 * `lines` are the same lines, each with the same reads in the same order, on every render. The
 * ref is written during render with that render's state, so a second render of the same state
 * counts nothing more.
 */
export function useLineRefusals(lines: readonly (readonly LineRead[])[]): number[] {
  const last = useRef<readonly LineCount[] | undefined>(undefined);
  const before = last.current;
  const counts = lines.map((reads, at): LineCount => {
    const was = before?.[at];
    let refusals = 0;
    if (reads.some((read) => read.failing)) {
      refusals = was?.refusals ?? 0;
      for (const [index, read] of reads.entries()) {
        if (!read.failing) continue;
        const seen = was?.seen[index];
        refusals +=
          seen === undefined || seen.hash !== read.hash
            ? read.errorUpdateCount
            : Math.max(0, read.errorUpdateCount - seen.errorUpdateCount);
      }
    }
    return { refusals, seen: reads };
  });
  last.current = counts;
  return counts.map(({ refusals }) => refusals);
}
