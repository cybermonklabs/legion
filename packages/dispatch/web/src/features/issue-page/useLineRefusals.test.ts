import { expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { type LineRead, useLineRefusals } from "./useLineRefusals";

/**
 * The count that keys a failed-read line's alert (`useLineRefusals`), read draw by draw: each
 * case draws the hook over one line and draws it again with the line's reads as they then stand,
 * as a view draws it again as its reads change.
 */

/** A read under the key whose hash is `hash`, refused `errorUpdateCount` times, and failing or
 *  not: a read that holds an answer is not failing, whatever its count. */
function lineRead(hash: string, errorUpdateCount: number, failing: boolean): LineRead {
  return { errorUpdateCount, failing, hash };
}

/** The hook over one line of `reads`, drawn again with each new set of reads a case gives. */
function drawLine(reads: readonly LineRead[]) {
  const hook = renderHook(({ line }) => useLineRefusals([line])[0], {
    initialProps: { line: reads },
  });
  return {
    count: () => hook.result.current,
    draw: (line: readonly LineRead[]) => hook.rerender({ line }),
    unmount: () => hook.unmount(),
  };
}

// The statements line stands on two reads, each refused once. In one draw the first answers and
// the second is refused again: the refusals of the failing reads come to two before and two
// after, so a count that summed them would not change, where the line's count rises by the new
// refusal.
test("a new refusal of one read while another answers in the same draw raises the count", () => {
  const line = drawLine([lineRead("coverage", 1, true), lineRead("blocks", 1, true)]);
  try {
    expect(line.count()).toBe(2);
    line.draw([lineRead("coverage", 1, false), lineRead("blocks", 2, true)]);
    expect(line.count(), "the new refusal of the blocks was not counted").toBe(3);
  } finally {
    line.unmount();
  }
});

// The count is kept during render, so a view drawn again with nothing changed - a render of its
// own, or one its parent asked for - counts no refusal twice.
test("the count stays the same on a second draw of the same state", () => {
  const reads = [lineRead("decisions", 2, true)];
  const line = drawLine(reads);
  try {
    expect(line.count()).toBe(2);
    line.draw(reads);
    expect(line.count(), "a draw of the same reads counted again").toBe(2);
    line.draw([lineRead("decisions", 2, true)]);
    expect(line.count(), "a draw of the same state counted again").toBe(2);
  } finally {
    line.unmount();
  }
});

// A read whose key changes is a query of its own: the spec's blocks once the issue names a spec of
// another id, refused once there too. Its refusal is new to the line, though its count is no
// higher than the one the read had under the key before, so the whole of it is added.
test("a failing read whose key changes adds its new query's whole count", () => {
  const line = drawLine([lineRead("coverage", 0, false), lineRead("spec-a", 1, true)]);
  try {
    expect(line.count()).toBe(1);
    line.draw([lineRead("coverage", 0, false), lineRead("spec-b", 1, true)]);
    expect(line.count(), "the refusal under the new key was not counted").toBe(2);
    line.draw([lineRead("coverage", 0, false), lineRead("spec-b", 1, true)]);
    expect(line.count(), "a draw under the same key counted its refusal again").toBe(2);
  } finally {
    line.unmount();
  }
});

// The statements line stands on the coverage, refused twice, while the spec's blocks are out. The
// coverage answers and no read is failing: the line is drawn no more, and its count is 0. When
// the blocks are then refused, the line stands again and counts that one refusal, not the
// coverage's from before.
test("the count goes back to 0 once no read is failing", () => {
  const line = drawLine([lineRead("coverage", 2, true), lineRead("blocks", 0, false)]);
  try {
    expect(line.count()).toBe(2);
    line.draw([lineRead("coverage", 2, false), lineRead("blocks", 0, false)]);
    expect(line.count(), "the count stayed with no read failing").toBe(0);
    line.draw([lineRead("coverage", 2, false), lineRead("blocks", 1, true)]);
    expect(line.count(), "the line counted refusals from before it stood again").toBe(1);
  } finally {
    line.unmount();
  }
});
